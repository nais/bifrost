# Bifrost: Adversarial Review & Reconciler Improvement Plan

_Status: proposal • Scope: `nais/bifrost` • Companion: `nais/unleasherator`_

## 1. Problem statement

Bifrost is described as "responsible for reconciling the Unleash instances according to the config
they are supposed to have." **It does not reconcile.** It is a synchronous Gin CRUD API: desired
config is rendered into an `Unleash` CR (and a Cloud SQL database + user + secret + FQDN
NetworkPolicy) **only** on `POST`/`PUT`, and torn down **only** on `DELETE`. There is no control
loop, so:

- **Global config drift is permanent.** Ingress host, SQL instance CIDR, cloud-sql-proxy sidecar
  image, OAuth audience, FQDN egress list, and resource limits are baked into each CR at write time
  (`unleash_repository.go:591-758`). When any of these change, existing instances keep the stale
  value forever. Only the ingress **class** is ever re-applied (`ReconcileIngressClasses`).
- **Manual/out-of-band drift is never corrected.** If an admin edits an `Unleash` CR or deletes the
  credentials secret, nothing repairs it.
- **Partial failures orphan real resources.** Provisioning spans Postgres + Kubernetes with no
  transaction, rollback, or idempotency, so failures leak Cloud SQL databases/users/secrets.

The one component named "reconciler" (`pkg/application/migration/reconciler.go`) is a **one-shot,
in-memory batch migration** run once at startup — not a converging control loop.

The fix is to turn bifrost into a proper **controller-runtime reconciler**: the API records desired
state; a reconcile loop continuously converges actual → desired (CR spec + child resources +
database), with ownership-based GC, idempotent "ensure" operations, finalizers, status, and leader
election.

---

## 1a. Dependencies & trust boundary (nais-api + PSK)

**Upstream (who calls bifrost):** `nais-api` is the **sole consumer** of the bifrost REST API. It
uses the generated `pkg/bifrostclient` configured with `BifrostAPIURL`
(`nais/api:internal/unleash/bifrost.go`, `internal/cmd/api/api.go`), and consumes:
`GET /v1/releasechannels[/:name]`, `GET/POST /v1/unleash`, `GET/PUT/DELETE /v1/unleash/:name`
(instance CRUD + an "issue checker" that reads instance state). **nais-api authenticates end users
and enforces team ownership/authorization**; bifrost is an internal backend behind a NetworkPolicy.

**Downstream (what bifrost drives):**
- **Kubernetes API (management cluster):** creates/updates `Unleash` CRs (unleasherator `api/v1`
  types), FQDN NetworkPolicies, credential Secrets; reads ReleaseChannels.
- **unleasherator operator:** actually realizes `Unleash` CRs and owns ReleaseChannel reconciliation.
  Bifrost must not fight it — use a distinct field manager and only own bifrost-rendered fields.
- **Google Cloud SQL Admin API:** databases/users on a shared SQL instance.
- **GitHub API:** image tags for version listing.

**Auth model — pre-shared key (decided):** bifrost trusts a single privileged caller (nais-api), so
auth is a **pre-shared key held in fasit**, injected as env into both sides — bifrost validates the
header; nais-api sends it via a `bifrostclient` `RequestEditorFn`. Terraform already manages a
`bifrost_teams_api_key` fasit value in `nais-terraform-modules/modules/management/bifrost.tf`; add a
sibling `bifrost_api_key` the same way and wire it to **both** bifrost and nais-api.

**Consequence for the review:** because nais-api is the trusted authority for team scoping, bifrost
does **not** need per-team authorization. The "tenant isolation" findings (S2/S3) are therefore
**not** bugs to fix in bifrost — they are a documented trust boundary: bifrost relies on nais-api +
PSK + NetworkPolicy. What bifrost still owes is: (1) the PSK gate (S1), (2) strict input validation
of everything nais-api passes (names, allowed_teams), and (3) not returning secrets even to a
trusted caller (S5).

**This is a coordinated, cross-repo change** (`nais/bifrost` + `nais/api` + `nais-terraform-modules`)
and must be sequenced (accept-then-enforce) — see Phase 0 and the "PSK auth" issue.

---

## 2. Adversarial review — findings

Verified by direct code reading. Severity is operational impact.

### 2.1 Security (fix before anything else)

| # | Severity | Finding | Location |
|---|----------|---------|----------|
| S1 | **critical** | **No authentication** on any endpoint. Only a NetworkPolicy stands between a caller and full instance CRUD (each provisions a DB + workload). **Fix = PSK** (§1a): validate a fasit-shared key that only nais-api holds. | `server.go:151-175` |
| S2 | ~~critical~~ **by-design** | **No per-team authz in bifrost.** Reframed given §1a: nais-api is the trusted authority for team scoping, so this is a documented trust boundary, not a bifrost bug. Once S1 (PSK) lands, "any caller" collapses to "nais-api only". Keep tenant enforcement in nais-api. | `handlers/unleash.go:57,81,187,303` |
| S3 | ~~high~~ **by-design + validate** | **Federation allowlist set by the caller.** Authorizing who may edit `allowed_teams`/`allowed_clusters` is nais-api's job. Bifrost should still **validate** the values (defense-in-depth), but not treat this as its authz surface. | `handlers/unleash.go:230-240` |
| S4 | high | **Weak randomness for the federation nonce** — `utils.RandomString` uses `math/rand` (~41 bits, predictable). DB passwords correctly use `crypto/rand`; the nonce is inconsistent. | `utils/strings.go:42`, consumed `unleash_repository.go:598` |
| S5 | medium | **Info leak** — `GET` returns the raw CRD including `Spec.Federation.SecretNonce`; bind/validation errors are echoed verbatim. | `handlers/unleash.go:97,121` |
| S6 | medium | **No quota / rate limit** → cost-amplification DoS (unbounded DB + workload creation), amplified by S1. | `handlers/unleash.go:112`, `service.go:72` |
| S7 | medium | **Weak name validation** — no length bound, dots allowed; a valid-API but invalid-k8s/DB name fails mid-sequence and orphans resources (see R1). | `domain/unleash/config.go:154` |

### 2.2 Resource lifecycle & correctness (the "not a reconciler" damage)

| # | Severity | Finding | Location |
|---|----------|---------|----------|
| R1 | **critical** | **`Create` has no rollback.** DB → user → secret → netpol → CR run sequentially; any later failure orphans the earlier Cloud SQL DB/user/secret/netpol. Non-idempotent `Create` calls mean retries hit `AlreadyExists` and wedge. | `service.go:72-92`, `manager.go:52,116`, `unleash_repository.go:420` |
| R2 | **critical** | **`Delete` aborts on first error, CR-first.** A transient secret-delete failure skips DB/user deletion; because the CR is already gone, the handler's `Get` guard then 404s, so the leaked DB/user can never be reaped via the API. | `service.go:145-163`, `handlers/unleash.go:307` |
| R3 | high | **Cloud SQL operations are async but never awaited.** Every `.Do()` discards the returned `*admin.Operation`, so create "succeeds" before the DB exists (pod connects too early) and user-delete races the still-running DB-drop (orphaned user). | `manager.go:57,83,131,147` |
| R4 | high | **`Update` rebuilds the whole object**, preserving only 4 metadata fields — clobbering finalizers, ownerReferences, labels, annotations, and resetting `Size` to 1 on every PUT. | `unleash_repository.go:187-193,617-622` |
| R5 | high | **PUT round-trip loss** — `LogLevel`, `DatabasePoolMax`, `DatabasePoolIdleTimeoutMs` are read only from the request body; omitted → reset to builder defaults. `LoadConfigFromCRD` (which would preserve them) is unused by the HTTP path. | `handlers/unleash.go:218`, `dto/unleash.go:58-64` |
| R6 | medium | **Update fails if the FQDN netpol is missing** (Get-then-Update, no create-if-absent), so a manually-deleted or pre-feature netpol makes the instance permanently un-updatable. | `unleash_repository.go:428-509` |
| R7 | medium | **Children not owner-referenced.** FQDN netpol and DB secret have no `OwnerReference` to the CR, so `kubectl delete unleash` orphans them and cascade GC is impossible. | `unleash_repository.go:354-365`, `manager.go:100` |
| R8 | low | **APIVersion mismatch** — list/get use `unleasherator.nais.io/v1`; the real group is `unleash.nais.io/v1`. Inert with the typed client, latent under unstructured/serialization. | `unleash_repository.go:45,85,118` vs `:615` |

### 2.3 The existing "migration reconciler" (batch, not a loop)

| # | Severity | Finding | Location |
|---|----------|---------|----------|
| M1 | high | **In-memory, one-shot, no resume.** State lives in a `sync.Map`; a restart mid-migration loses progress, and an instance left mid-flight is silently dropped (no longer matches the custom-version filter) with no health verification. | `migration/reconciler.go:24,73-131` |
| M2 | high | **Shutdown misclassified as health failure** → triggers rollback on the already-cancelled context, which then fails → `statusRollbackFailed` + "manual intervention" alarms for a normal SIGTERM. | `reconciler.go:191-198`, `common.go:151` |
| M3 | high | **Stale-Ready false positive.** `waitForHealthy` accepts the old generation's `Ready` conditions immediately after Update (no `observedGeneration` gate), marking a possibly-broken migration "completed". | `common.go:164`, unleasherator `IsReady()` |
| M4 | high | **Lossy rollback.** Rollback rebuilds the whole CR from a lossy `Config` round-trip (`CustomImage` split keeps only the tag; repo/name/size/resources reset to hardcoded defaults). | `reconciler.go:207-242`, `unleash_repository.go:548-579,752` |
| M5 | medium | **Transient errors are terminal** (any Get/Update error → `statusFailed`, no retry/backoff); `waitForHealthy` never checks at t=0 and self-times-out when `timeout ≤ pollInterval`. | `reconciler.go:149-187`, `common.go:146-155` |
| M6 | low | **No leader election** — correctness depends entirely on `replicas: 1`; a surge rollout runs two reconcilers over the same instances. | `server.go:247-270` |

### 2.4 Operational robustness

| # | Severity | Finding | Location |
|---|----------|---------|----------|
| O1 | medium | **HTTP server ignores configured timeouts** (`Read`/`Write`/`Idle` parsed but never applied) → Slowloris exposure. | `server.go:279` |
| O2 | medium | **Startup goroutines leak / not awaited**; ingress-reconcile goroutine uses `context.Background()` and isn't cancelled; serve goroutine `Fatal`s and bypasses cleanup. | `server.go:233,287,294-301` |
| O3 | medium | **GitHub tags client** is unauthenticated, no timeout/context/cache, no body limit → 60 req/hr rate limit breaks provisioning; picks `tags[0]` not the newest release (`ReleaseTime` never used for ordering). | `github/tags.go:21`, `domain/unleash/config.go:115` |
| O4 | low | **`GinMiddleware` variable shadow** — inner `c *gin.Context` shadows the `*Config` receiver, so `c.Set("config", c)` stores the gin context, not the config. | `config.go:152-157` |
| O5 | low | **`godotenv` fragile string match**; `New()` panics on missing required config; `DebugMode` is dead config while the logger is hardcoded to Debug in prod (secret-leak risk). | `config.go:136,160-172`, `server.go:82` |
| O6 | low | **Always-OK `/healthz`, no readiness** — a broken kube/GCP client still reports healthy. | `server.go:155` |

---

## 3. Target architecture

Split bifrost into two cooperating roles inside one binary:

```
                 writes desired intent                 converges actual → desired
   HTTP client ───────────────────────►  API layer ───────────────────────► Reconciler (control loop)
   (IAP/JWT)      (authn + authz +        (thin: validate,                    watch Unleash CRs +
                   quota + validation)     persist intent,                     periodic resync
                                           return 202)                         │
                                                                               ├─ render desired spec (global cfg + intent)
                                                                               ├─ server-side apply Unleash CR
                                                                               ├─ ensure FQDN netpol (owned)
                                                                               ├─ ensure Cloud SQL db/user/secret (owned, awaited)
                                                                               └─ write status/conditions
```

- **The API stops doing side effects.** It authenticates, authorizes, validates, records desired
  intent, and returns `202 Accepted`. It never touches Cloud SQL directly.
- **The reconciler owns all convergence.** It runs on CR events **and** a periodic resync (e.g. 10m),
  so global-config changes propagate to every instance automatically and drift self-heals.
- **Everything is idempotent "ensure".** Create/update collapse into one apply path; there is no
  separate imperative create vs update.

### 3.1 Where does desired state live?

Per-instance intent (version source, allowed teams/clusters, log level, DB pool, nonce) currently
lives tangled inside the rendered `Unleash` spec, which is why `Update` and rollback are lossy.
Two options:

- **Option A — intent annotation on the `Unleash` CR (recommended first step).** Store the canonical
  intent as a single JSON blob under a bifrost-owned annotation
  (`bifrost.nais.io/desired-state`) plus a `app.kubernetes.io/managed-by: bifrost` label. The
  reconciler reads the annotation (non-lossy source of truth), renders the full spec deterministically,
  and server-side-applies. **No new CRD, minimal migration, immediate drift correction.**
- **Option B — dedicated `BifrostUnleash` CRD (cleaner long-term).** The API writes a small
  desired-state CR; a reconciler renders the `unleasherator` `Unleash` CR + DB from it. Best
  separation of concerns (bifrost = tenant/fleet intent, unleasherator = k8s realization) but a
  heavier migration and a two-hop reconcile.

**Recommendation:** ship Option A now (it removes R4/R5/M4 lossiness immediately and needs no CRD
rollout), and keep Option B as a follow-up once the reconcile loop is proven.

---

## 4. Reconciler design (Option A)

Use `sigs.k8s.io/controller-runtime` (already a dependency) with a `Manager` embedded in the bifrost
process.

```go
// One reconciler, keyed by Unleash CR name; selector-filtered to bifrost-managed instances.
func (r *UnleashReconciler) Reconcile(ctx, req) (ctrl.Result, error) {
    cr := get(req)                                 // NotFound → nothing to do
    if !managedByBifrost(cr) { return noRequeue }  // label filter
    if beingDeleted(cr) { return r.finalize(ctx, cr) }
    ensureFinalizer(cr)

    intent := parseIntent(cr)                      // from bifrost.nais.io/desired-state annotation
    desired := render(r.globalConfig, intent)      // the SINGLE shared render fn (also used by API preview)

    // 1. Database (idempotent + awaited)
    ensureDatabase(ctx, intent.Name)               // get-or-create, poll Operation → DONE
    ensureDatabaseUser(ctx, intent.Name)           // get-or-create; rotate only on explicit request
    ensureCredentialsSecret(ctx, intent.Name, ...) // CreateOrUpdate, OwnerReference=cr

    // 2. Kubernetes objects (server-side apply, preserve foreign fields)
    applyUnleashSpec(ctx, cr, desired.Spec)        // patch only bifrost-owned fields
    ensureFQDNNetworkPolicy(ctx, cr, desired.Netpol) // CreateOrUpdate, OwnerReference=cr

    // 3. Status
    setConditions(cr, ...)                         // Ready/Degraded + observedGeneration
    return requeueAfter(resyncInterval)            // periodic re-render → global-config drift heals
}
```

Key design rules:

1. **Idempotent ensure everywhere.** Replace `Insert`/`Create` with get-or-create (treat
   `AlreadyExists`/409 as success) and `controllerutil.CreateOrUpdate` / server-side apply. Fixes
   R1, R6, and the M-series retry problems.
2. **Await Cloud SQL operations.** Capture the returned `*admin.Operation` and poll
   `operations.Get` until `DONE` (bounded by ctx timeout) before advancing to the dependent step.
   Fixes R3.
3. **Owner references on all children** (secret, FQDN netpol) → cascade GC; delete becomes mostly
   "let Kubernetes GC" plus a finalizer for the external Cloud SQL DB/user. Fixes R2, R7.
4. **Finalizer for external state.** A `bifrost.nais.io/finalizer` runs best-effort DB/user/secret
   teardown (accumulate errors, `IgnoreNotFound`, delete DB before user) and only removes the
   finalizer once external cleanup succeeds. Fixes R2.
5. **Server-side apply / field-scoped patch** so bifrost only owns the fields it renders and never
   clobbers finalizers, ownerReferences, labels, or `Size` set by unleasherator or humans. Fixes R4/R5.
6. **Generation-aware health.** Gate "ready after change" on `status.observedGeneration ≥` the
   generation returned by the apply, not stale conditions. Fixes M3.
7. **Leader election** (`manager.Options{LeaderElection: true}`) so HA/rollouts are safe. Fixes M6.
8. **Migrations become reconcile policy, not a batch job.** Version/channel migration is expressed
   as a transformation of the desired intent, applied idempotently by the same loop, checkpointed on
   the CR (annotation/condition), resumable across restarts, with rollback via re-applying the stored
   prior intent under a **fresh** bounded context. Fixes M1/M2/M4/M5.

---

## 5. Phased implementation plan

Each phase is independently shippable and leaves the system better than before.

### Phase 0 — Security & orphan stop-gap (urgent, no architecture change)
- **S1 (PSK auth — cross-repo, coordinated):**
  1. `nais-terraform-modules`: add a `bifrost_api_key` fasit value (mirror `bifrost_teams_api_key`),
     wired to **both** the bifrost and nais-api deployments.
  2. `bifrost`: add an auth middleware (in front of `/v1/*`, exempting `/healthz`, `/readyz`,
     `/openapi.json`) that compares an `Authorization`/`X-API-Key` header against the configured key
     using a **constant-time** compare; support a set of keys for rotation. Ship in **accept-then-
     enforce** mode: first release logs-but-allows missing keys (metric on unauthenticated calls),
     second release fails closed once nais-api is confirmed sending it.
  3. `nais-api`: add a `bifrostclient` `RequestEditorFn` that sets the header from its fasit-injected
     config. Deploy **before** bifrost flips to enforce.
- **S5:** return a response DTO that omits `SecretNonce` and other secret material; stop echoing raw
  bind/validation errors to the client (log server-side).
- **S7 / input validation:** enforce strict DNS-1123-label + max-length on instance name before any
  resource is created; validate `allowed_teams`/`allowed_clusters` values.
- (S2/S3 need **no bifrost code** beyond S1+validation — see §1a; note them in the nais-api trust doc.)
- **S4:** reimplement `utils.RandomString` on `crypto/rand`; lengthen the nonce.
- **R1/R2:** make `Create` roll back on failure (deferred best-effort teardown) and `Delete`
  best-effort + `IgnoreNotFound` + DB-before-user, decoupled from the CR `Get` guard.
- **R3:** await Cloud SQL operations.
- **O1/O2/O4/O5:** apply server timeouts; single root context + `WaitGroup` for graceful shutdown;
  fix the `GinMiddleware` shadow; `errors.Is(os.ErrNotExist)` for godotenv; drive log level from config.

### Phase 1 — Idempotent, ownership-based primitives (prepares for the loop)
- Convert `dbManager` and `unleash_repository` mutations to idempotent **ensure** operations
  (get-or-create, `CreateOrUpdate`, server-side apply). Fixes R1/R6 structurally.
- Set **OwnerReferences** on secret + FQDN netpol (R7). Add a finalizer for Cloud SQL teardown.
- Extract a single **`render(globalConfig, intent) → (Spec, Netpol)`** function used by both the API
  (for preview/validation) and the reconciler — one source of truth for rendering.
- Introduce the **intent annotation** (`bifrost.nais.io/desired-state`) + `managed-by` label; write it
  on create/update. Make `LoadConfigFromCRD` read the annotation (non-lossy). Fixes R4/R5/M4.
- Fix R8 (single canonical GroupVersion), O3 (hardened, cached, authenticated GitHub client + sort by
  `ReleaseTime`), O6 (`/readyz` with dependency checks).

### Phase 2 — Introduce the reconciler
- Embed a controller-runtime `Manager` with **leader election**; register the `UnleashReconciler`
  watching `Unleash` CRs filtered by the `managed-by` label, owning secret + netpol, with periodic
  resync (drift correction).
- Move all convergence (render → apply → ensure DB/netpol/secret → status) into `Reconcile`.
- **The API becomes thin:** validate + authz + persist intent (create/patch the CR annotation) +
  `202`. Remove side effects from the request path (fixes S6 blast radius and the create/delete
  race). Report status via the CR conditions the reconciler writes.

### Phase 3 — Fold migrations into the loop, retire the batch
- Express version/channel migration as an intent transformation applied idempotently by the
  reconciler, checkpointed on the CR, resumable, with generation-aware health gating and
  fresh-context rollback. Delete `pkg/application/migration/*` one-shot batch once parity is proven.

### Phase 4 (optional) — Dedicated `BifrostUnleash` CRD (Option B)
- If the annotation-as-intent proves limiting, promote intent to a first-class CRD for cleaner
  API/validation/RBAC and a proper `kubectl` surface.

---

## 6. Testing & rollout

- **Unit:** `render()` golden tests; ensure-idempotency (apply twice → no diff); finalizer teardown
  with injected failures; Cloud SQL operation-await with a fake operations service.
- **envtest:** reconcile against a real API server + fake Cloud SQL/GitHub; drift tests (mutate a CR
  field, assert it's corrected on resync); global-config-change test (bump proxy image → all
  instances re-rendered); orphan test (fail mid-create → assert no leaked children).
- **Migration parity:** run Phase 3 reconciler against a snapshot of prod-like instances; assert no
  unintended spec diffs vs the batch migrator.
- **Rollout:** ship Phase 0 immediately (security). Gate the reconciler behind a feature flag
  (`BIFROST_RECONCILER_ENABLED`, default off); enable in a non-prod tenant first; watch drift/apply
  metrics; then enable in prod with `replicas: 1` until leader election lands, then scale.

## 7. Risks & mitigations

- **Reconciler fighting unleasherator / release-channel controller.** Use server-side apply with a
  distinct field manager and only own bifrost-rendered fields; never write status or fields owned by
  unleasherator. Verify against issue set #753–#756 (unleasherator rollout controller).
- **Mass re-render on first enable** (global config differs from baked-in values) could restart many
  pods at once. Mitigate with a bounded work rate / maxConcurrentReconciles and a canary tenant.
- **Intent-annotation migration** for existing instances: backfill the annotation from the current
  spec once (best-effort `LoadConfigFromCRD`) before enabling strict re-render.
- **Auth rollout** (S1/S2) may break existing unauthenticated clients — coordinate with callers;
  ship behind the ingress/IAP that already fronts it, then enforce in code.

---

## 8. Issue structure — one PRD or several?

**Recommendation: multiple, as an epic + focused issues** (not one mega-issue). The work has
different urgencies, owners, and blast radii, and one item is cross-repo:

- **Epic / PRD** (this document): "Turn bifrost into a reconciler" — the umbrella, links the children.
- **Issue A — PSK authentication (cross-repo, urgent).** Own issue because it spans
  `nais/bifrost` + `nais/api` + `nais-terraform-modules` and needs the accept-then-enforce sequence.
  Blocks nothing but is the highest-priority security fix.
- **Issue B — provisioning correctness / stop orphaning (urgent).** R1–R3: rollback-on-failure,
  best-effort idempotent delete, await Cloud SQL operations. Pure bifrost, shippable now.
- **Issue C — introduce the reconciler (epic-sized).** Phases 1–2: idempotent ensure primitives,
  owner references, shared `render()`, intent annotation, the controller-runtime loop, leader
  election. The architectural core.
- **Issue D — fold migrations into the loop.** Phase 3; depends on C.
- **Issue E — operational hardening (low-risk bundle).** O1–O6 + S4 (crypto nonce) + O3 (GitHub
  client) — can be done independently and in parallel.

Rationale: A must be coordinated with nais-api and can't sit inside a bifrost-only epic; B is a
fast, high-value correctness fix that shouldn't wait for the reconciler; C/D are the large
architectural change; E is cleanup. Splitting keeps each independently reviewable, assignable, and
schedulable.

---

_Appendix: all findings above were verified by direct code reading of `nais/bifrost` at the current
checkout. Line numbers may shift as the code evolves._
