# Bifrost reconciler runbook

_Status: operational • Scope: `nais/bifrost` • Companion: [`RECONCILER_IMPROVEMENT_PLAN.md`](RECONCILER_IMPROVEMENT_PLAN.md)_

The reconcile loop (`pkg/reconciler`) re-renders every bifrost-managed `Unleash` CR from its
recorded intent and patches the live CR toward it, on CR events and on a periodic resync. It is
dark-launched: the chart ships `reconciler.enabled=false`, and enabling it turns on observe mode
first (`dryRun=true`, `charts/bifrost/values.yaml`).

This runbook is written to be read **before** `dryRun` is turned off. Everything in it is checked
against the code, the chart and `Feature.yaml` at the commit it was written on; line references are
given so a claim that has drifted can be spotted. Where something could not be verified it says so
rather than guessing.

---

## 1. How do I stop it?

There is **no runtime toggle**. The whole configuration is read once from the environment at process
start (`cmd/run.go` → `config.New`, `pkg/config/config.go:230`), there is no config reload, no SIGHUP
handler (`pkg/server/server.go:353` notifies only `SIGTERM`/`SIGINT`) and no admin endpoint (the only
non-API routes are `/healthz`, `/metrics` and `/openapi.json`). **Every stop below is a pod restart.**

In increasing order of blast radius:

### 1.1 `dryRun` — stop the writes, keep the measurement

| | |
| --- | --- |
| Env var | `BIFROST_RECONCILER_DRY_RUN=true` (`pkg/config/config.go:172`) |
| Chart value | `backend.reconciler.dryRun` (`charts/bifrost/values.yaml`, default `true`) |
| Fasit toggle | `backend.reconciler.dryRun` (bool, `charts/bifrost/Feature.yaml`) |

```bash
kubectl -n <bifrost-ns> set env deploy/bifrost-backend BIFROST_RECONCILER_DRY_RUN=true
```

**Stops:** the patch, and only the patch. `Reconcile` returns before `client.Patch` and records
`action="would_change"` instead (`pkg/reconciler/unleash.go:105-109`, patch at `:123`).

**Does not stop:** the watch, the resync, the render, the instance census, or any of §2.

> **Trap:** the *code* default for `BIFROST_RECONCILER_DRY_RUN` is `false` (`pkg/config/config.go:172`)
> — observe mode is a *chart* default, not a code default. A bifrost started with
> `BIFROST_RECONCILER_ENABLED=true` and nothing else writes immediately.

### 1.2 `reconciler.enabled` — stop the loop

| | |
| --- | --- |
| Env var | `BIFROST_RECONCILER_ENABLED=false` (`pkg/config/config.go:167`) |
| Chart value | `backend.reconciler.enabled` (`charts/bifrost/values.yaml`, default `false`) |
| Fasit toggle | `backend.reconciler.enabled` (bool, `charts/bifrost/Feature.yaml`) |

```bash
kubectl -n <bifrost-ns> set env deploy/bifrost-backend BIFROST_RECONCILER_ENABLED=false
```

**Stops:** everything in `pkg/reconciler`. The controller-runtime manager is only constructed when
the flag is true (`pkg/server/server.go:323`), so with it false there is no watch, no resync, no
patch and no census — the gauges stay at 0 and the census timestamp stays 0 (see §4).

**Does not stop:** everything in §2. Bifrost keeps provisioning and writing `Unleash` CRs.

**Latency:** one rolling restart, ~30–60s in practice for a single replica; not measured, so treat it
as the order of magnitude rather than an SLO. `kubectl set env` patches the Deployment directly and
is **reverted by the next helm sync** — the template always renders the variable from the chart value
(`charts/bifrost/templates/backend-deployment.yaml`), so a redeploy overwrites the manual override.
For anything that must survive, change the fasit toggle (minutes to hours, via CI).

### 1.3 Scale to zero — stop the process

```bash
kubectl -n <bifrost-ns> scale deploy/bifrost-backend --replicas=0
```

**Stops:** the reconciler and everything else in the process, in seconds.

**Also stops (this is the cost):** the provisioning API. `nais-api` is bifrost's only consumer, so
instance create/update/delete from console stops working, and so does the release-channel read path.

**Does not stop:** unleasherator. Running Unleash instances keep running and keep being reconciled by
unleasherator from their CRs — stopping bifrost freezes *intent*, not the instances.

> The deployment name is `{{ .Release.Name }}-backend` via `bifrost.fullname`
> (`charts/bifrost/templates/_helpers.tpl`); confirm with
> `kubectl -n <bifrost-ns> get deploy -l app=bifrost`.

---

## 2. What does **not** stop it

Turning the reconcile loop off stops the *converging* loop. It does not stop bifrost from writing
`Unleash` CRs. Everything below writes through the same repository (`UnleashRepository.Update`,
which renders the whole CRD and PUTs it) and is unaffected by every toggle in §1:

| Writer | Gate | Where it writes |
| --- | --- | --- |
| The request/response API (`POST`/`PUT`/`DELETE /v1/unleash`) | none — always registered | `pkg/server/server.go:211` → `pkg/api/http/v1/handlers/unleash.go:191` (create), `:348` (update), `:401` (delete) |
| Ingress-class re-apply on startup | none — runs on every process start | `pkg/server/server.go:280-288` → `UnleashRepository.ReconcileIngressClasses`, `pkg/infrastructure/kubernetes/unleash_repository.go:267` |
| Version→channel migration batch | `BIFROST_UNLEASH_MIGRATION_ENABLED` | `pkg/server/server.go:290-303` → `pkg/application/migration/reconciler.go:186` (migrate), `:240` (rollback) |
| Channel→channel migration batch | `BIFROST_UNLEASH_CHANNEL_MIGRATION_ENABLED` | `pkg/server/server.go:305-317` → `pkg/application/migration/channel_reconciler.go:198` (migrate), `:252` (rollback) |

Consequences worth knowing before you need them:

- **The API keeps stamping bifrost's metadata.** Every create and update renders through
  `BuildUnleashCRD`, which stamps the `app.kubernetes.io/managed-by=bifrost` label and the
  `bifrost.nais.io/desired-state` annotation (`unleash_repository.go:829` → `managed.go:126`). This
  happens with the reconciler disabled, and has been happening since #550 shipped. Disabling the
  reconciler does not stop intent from being recorded.
- **Both migration batches are one-shot at startup, not loops.** They list, filter and walk the
  candidates once and return (`migration/reconciler.go:48-131`). So *restarting the pod to disable
  the reconciler re-runs them from the top* if their own flags are on. Check those flags before you
  restart.
- **The ingress-class re-apply is unconditional and fleet-wide.** It patches every `Unleash` CR in
  the namespace whose web/api ingress class differs from the configured one — including CRs bifrost
  does not manage, since it does not filter on the managed-by label
  (`unleash_repository.go:267-302`).
- **An in-flight reconcile can still land.** On shutdown the manager context is cancelled
  (`pkg/server/server.go:382`) and controller-runtime then gives in-flight runnables
  `GracefulShutdownTimeout` — default 30s (controller-runtime v0.22.5,
  `pkg/manager/internal.go:55`; bifrost does not override it, `pkg/reconciler/manager.go`) — to
  finish. A patch already issued is not recalled. During a rolling restart the old pod keeps
  reconciling until it exits.
- **Stopping is not undoing.** Patches already applied stay applied; see §6.

---

## 3. Per-instance opt-out, and the move that does not work

### Removing the managed-by label opts out — of the loop

```bash
kubectl -n <unleash-ns> label unleash <name> app.kubernetes.io/managed-by-
```

Both the watch predicate (`pkg/reconciler/unleash.go:240-242`) and the in-loop check
(`:71-74`) require the label, and the reconciler can never re-add it: the only code that sets it,
`applyManagedMetadata` (`:223-235`), runs after the check that already returned. The in-loop check
also counts the case, as `action="skipped", reason="missing_managed_label"`.

**But it is only durable against the reconciler.** The next `PUT` (or `POST`) through the API
re-stamps the label, because every render goes through `stampManagedMetadata`
(`unleash_repository.go:829`). Issue #588 describes the label opt-out as durable; that holds for the
loop, not for the API path.

### Deleting the desired-state annotation does **not** opt out

`resolveIntent` falls back to reverse-engineering the live spec with `LoadConfigFromCRD`
(`pkg/reconciler/unleash.go:161-162`; #588 cites `:149`, which is now the validation call — the line
moved, the behaviour did not). The instance stays managed, and the config it converges to is derived
from the spec rather than from the recorded intent — which is *lossier*, not safer.

An operator who deletes "bifrost's annotation" to make a manual hotfix stick gets the worst of both:
still managed, and now converging on a reverse-engineered config.

### Repairing an instance durably

The annotation is authoritative — the live spec is not. In order of preference:

1. **Go through the API** (`PUT /v1/unleash/<name>`). It records the new intent and writes the spec
   in one step, conditionally on the resourceVersion it read.
2. **Fix the annotation by hand.** It is the `unleash.Config` JSON plus a `schemaVersion` key
   (`managed.go:40-88`). It must carry `schemaVersion: 1` — any other value is refused and the
   instance is skipped (`managed.go:84`) — must pass `Config.Validate()`, and its `name` must match
   the instance (`pkg/reconciler/unleash.go:149-158`).
3. **Editing only the spec does not survive** once `dryRun=false`: the next resync re-renders from
   the annotation and patches your edit away.

---

## 4. Is it running at all?

Metrics, all on bifrost's own `/metrics` (unauthenticated; controller-runtime's own listener is
disabled and its registry is merged into this endpoint, `pkg/server/metrics.go:43-58`), scraped every
minute by the ServiceMonitor (`charts/bifrost/templates/backend-servicemonitor.yaml`).

| Metric | Meaning |
| --- | --- |
| `bifrost_reconciler_managed_instances` | instances carrying the managed-by label |
| `bifrost_reconciler_unmanaged_instances` | instances in the namespace without it |
| `bifrost_reconciler_instances_updated_timestamp_seconds` | unix time of the last **successful** census |
| `bifrost_reconciler_actions_total{action,reason}` | one increment per instance per reconcile |

### The trap: 0 does not mean "empty fleet"

All four are registered in `init()` (`pkg/reconciler/metrics.go:90`), and `package server` imports
`package reconciler` unconditionally (`pkg/server/server.go:22`). **Every bifrost process publishes
these gauges as 0 from start-up, including one with the reconciler switched off.** `managed=0,
unmanaged=0` is therefore not evidence of anything.

The timestamp is what separates "never measured" from "measured, and the fleet is empty":

```promql
# Has a census ever completed in this process? Anything else means the loop is not running.
bifrost_reconciler_instances_updated_timestamp_seconds > 0
```

The census runs immediately when the manager starts and then every resync interval
(`pkg/reconciler/unleash.go:263-275`). A failed `List` logs a warning, keeps the previous gauge
values and deliberately does **not** advance the timestamp (`:287-292`), so a persistently failing
census shows up as a stale stamp rather than as a plausible steady state:

```promql
# Stalled census: censused at least once, then nothing for 3 resync intervals (default 10m).
bifrost_reconciler_instances_updated_timestamp_seconds > 0
  and (time() - bifrost_reconciler_instances_updated_timestamp_seconds) > 1800
```

### Is it doing any work?

The action counter has **no pre-initialised label combinations** (`pkg/reconciler/metrics.go:29-37`
registers the vector only), so the series simply does not exist until the first reconcile. Absence is
the signal, not zero:

```promql
absent(bifrost_reconciler_actions_total)                       # nothing has ever reconciled
sum by (action, reason) (increase(bifrost_reconciler_actions_total[15m]))
```

Every managed instance is requeued after one resync interval (`RequeueAfter: r.resync`,
`pkg/reconciler/unleash.go:99/108/131`), so over a 10m window expect **at least one action per
managed instance**. Materially fewer means instances are dropping out of the loop:

```promql
sum(increase(bifrost_reconciler_actions_total[10m])) < sum(bifrost_reconciler_managed_instances)
```

Logs, if you have the pod rather than the dashboard: `Starting Unleash reconciler manager`
(`pkg/server/server.go:331`), then per instance either `Instance differs from desired configuration
(dry-run: no changes applied)` (`pkg/reconciler/unleash.go:107`) or `Reconciled instance to desired
configuration` (`:130`).

### Not verifiable today

There is **no metric that reports whether the reconciler is enabled** — no build-info or config gauge
exists. "Enabled but never ran a census" is therefore indistinguishable in Prometheus from "not
enabled", and cannot be alerted on; it has to be checked against the deployment's env or the fasit
toggle. Exporting an info gauge for `enabled`/`dryRun` would close this and is the obvious follow-up.

Leader election is off by default (`pkg/config/config.go:178`) and the chart pins `replicas: 1`
(`charts/bifrost/templates/backend-deployment.yaml`), so no per-pod aggregation is needed today. If
replicas ever grow, the census runnable only runs on the leader (`pkg/reconciler/metrics.go:77-84`)
and non-leaders would publish a permanent 0 next to the leader's real numbers.

---

## 5. Reading `would_change`

`action="would_change"` is dry-run's output: one increment per instance per reconcile where a change
*would* have been applied. The `reason` label is what makes it actionable.

```promql
sum by (reason) (bifrost_reconciler_actions_total{action="would_change"})
sum by (reason) (increase(bifrost_reconciler_actions_total{action="would_change"}[10m]))
```

| `reason` | What it means | What to do |
| --- | --- | --- |
| `spec_mismatch` | The live spec differs from the render — **real drift**, and what would actually be patched (`pkg/reconciler/unleash.go:190-191`) | Investigate before enforcing. The log line carries `spec_sections` naming the differing top-level spec fields (`:177-183`, `:207-218`) |
| `desired_state_mismatch` | Spec already matches; only the `bifrost.nais.io/desired-state` annotation differs (`:196-197`) | Adoption bookkeeping. Expected to be non-zero on first enable and to fall to zero once every instance has been written through the loop or the API |
| `missing_managed_label` | Not reachable under `would_change` | The loop returns at `:71` before rendering, so a missing label appears as `action="skipped"`, never as `would_change` |

The checks are ordered (`inSync`, `:189-200`): an instance with both spec drift and a stale annotation
reports `spec_mismatch` only. So `desired_state_mismatch` really does mean "spec is already correct".

Two counters that are *not* `would_change` but belong on the same dashboard:

```promql
sum(increase(bifrost_reconciler_actions_total{action="intent_error"}[15m]))  # annotation unusable
sum(increase(bifrost_reconciler_actions_total{action="error"}[15m]))         # the patch failed
```

`intent_error` (`pkg/reconciler/unleash.go:76-85`) means the instance is *silently out of the
reconciled set*: unreadable schema version, a config that fails validation, or an annotation naming a
different instance. It stops reporting `in_sync` and nothing else notices.

### Go/no-go for turning `dryRun` off

Not a threshold anyone has committed to; the rule the plan states (§6a) is *understood*, not *zero*:
every `spec_mismatch` instance should be explainable from its `spec_sections` before enforcing, and
`intent_error` should be zero.

---

## 6. When it is doing the wrong thing

**Signature of a non-converging loop:** `action="changed"` does not fall off after the first pass.
A healthy enforce looks like one burst as the fleet is converged, then near-zero. If instead

```promql
sum(increase(bifrost_reconciler_actions_total{action="changed"}[30m])) >= 3 * sum(bifrost_reconciler_managed_instances)
```

holds steadily (roughly: every managed instance changes on every resync), the loop is fighting
someone. Known candidates: another writer from §2 (a migration batch running in the same process), or
a render that can never equal the stored object — the `omitempty`-on-bool case documented at
`unleash_repository.go:687-692` is exactly that failure and cost a permanent `spec_mismatch`.

**Immediate response, cheapest first:**

1. `dryRun=true` (§1.1). The loop keeps measuring, so you can still see the reason breakdown, but it
   stops writing.
2. If that is not enough, `enabled=false` (§1.2).
3. `replicas=0` (§1.3) only if the API itself must stop too.
4. Follow with the fasit toggle so the next helm sync does not undo the manual override.

**What rolling back does not undo:**

- **Patches already applied.** There is no revert path and no snapshot of the previous spec. An
  instance patched to a wrong spec stays wrong until it is written again, through the API.
- **Pod restarts caused by those patches.** Unleasherator re-rolls the Unleash deployment on spec
  changes; the loop cannot un-restart anything.
- **The label and the annotation.** They are on every CR the API has touched since #550, they are not
  removed when the reconciler is disabled, and the reconciler itself never removes either
  (`applyManagedMetadata` only sets, `pkg/reconciler/unleash.go:223-235`).
- **A bad recorded intent.** This is the sharp one: if a wrong intent was captured, then disabling the
  loop, fixing the spec by hand, and re-enabling **re-applies the wrong intent**. The annotation is
  authoritative, the spec is not. Repair per §3.

---

## 7. Alerts

`charts/bifrost/templates/prometheus-alerts.yaml` has **no reconciler rule** as of this commit; the
`PrometheusRule` covers Unleash database latency, ingress errors and unleasherator connectivity only.
#588 makes shipping alerts a precondition for `dryRun=false`. The two rules that are expressible with
the metrics that exist:

```yaml
- alert: BifrostReconcilerErrors
  expr: sum(rate(bifrost_reconciler_actions_total{action=~"error|intent_error"}[10m])) > 0
  for: 15m

- alert: BifrostReconcilerCensusStalled
  expr: bifrost_reconciler_instances_updated_timestamp_seconds > 0
        and (time() - bifrost_reconciler_instances_updated_timestamp_seconds) > 1800
  for: 10m
```

The third rule #588 asks for — *absence of activity while enabled* — **cannot be written today**, for
the reason in §4: nothing exports the enabled flag, and every disabled bifrost publishes the same
zeros and the same absent counter as a broken enabled one. Either export an info gauge first, or
accept that this case is caught by the census-stalled rule only after the first successful census.

Thresholds above are proposals, not tuned against observed data.
