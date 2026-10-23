# Bifröst

> Fleet management API for Unleash feature toggle instances

Bifröst provides centralized management of Unleash instances in Kubernetes, with support for automated version management through release channels. Named after the Norse bridge connecting realms, Bifröst connects development teams with their feature toggle infrastructure.

## Overview

Bifröst is a REST API that orchestrates Unleash deployments on Kubernetes, handling:

- **Instance lifecycle management** - Create, update, and delete Unleash instances
- **Release channel automation** - Automatically upgrade instances via scheduled release channels
- **Database provisioning** - Automated PostgreSQL database creation per instance
- **Network policy management** - Secure instance isolation with FQDN-based policies
- **Multi-tenancy** - Team-based access control and resource isolation

## Quick Start

```bash
# Create an Unleash instance with a specific version
curl -X POST http://bifrost/v1/unleash \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-unleash",
    "custom_version": "5.11.0",
    "allowed_teams": "team-a,team-b"
  }'

# Or use a release channel for automatic updates
curl -X POST http://bifrost/v1/unleash \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-unleash",
    "release_channel_name": "stable",
    "allowed_teams": "team-a,team-b"
  }'
```

## API Reference

Interactive API documentation is available at `/swagger/index.html` when the server is running.

### Endpoints

```http
GET    /healthz                       - Health check

GET    /v1/unleash                    - List all instances
GET    /v1/unleash/:name              - Get instance details
POST   /v1/unleash                    - Create new instance
PUT    /v1/unleash/:name              - Update instance
DELETE /v1/unleash/:name              - Delete instance

GET    /v1/releasechannels            - List all release channels
GET    /v1/releasechannels/:name      - Get channel details
```

### Create/Update Instance

```json
{
  "name": "my-unleash",
  "custom_version": "5.10.2",          // OR
  "release_channel_name": "stable",    // (mutually exclusive)
  "enable_federation": true,
  "allowed_teams": "team-a,team-b",
  "allowed_clusters": "dev-gcp,prod-gcp",
  "log_level": "info",
  "database_pool_max": 5,
  "database_pool_idle_timeout_ms": 2000
}
```

### Resource profiles (activation gated)

`resource_profile` accepts `small`, `medium`, or `large` on create/update.
Omission preserves selection; empty/null and unknown values are rejected.
Responses retain the existing CRD shape and add
`resource_profile`, `resource_profile_explicit`, `resource_profile_matches`, and
`resolved_resources` (actual live container resources).

| Profile | CPU request | Memory request | Memory limit |
| --- | --- | --- | --- |
| small | 100m | 128Mi | 256Mi |
| medium | 100m | 256Mi | 512Mi |
| large | 100m | 512Mi | 1Gi |

The table is provisional, **not capacity approval**. Selection and enforcement
default off (`BIFROST_UNLEASH_RESOURCE_PROFILES_ENABLED=false`).
Set `backend.unleash.resourceProfilesEnabled` in Helm/Fasit for the selected
environment (default `false`; set back to `false` to disable safely).
Merging ships through ci-nais and tenant fan-out, with no manual hold before dev-nais.
nais/api and Console selection are separate integration work (see #641/#640).

Absent selection displays inherited small without acquiring resource ownership.
Explicit selection on existing instances requires valid recorded desired-state
intent; it never adopts an unannotated instance. Legacy adoption remains separate.
Live tuning, CPU limits, extra resource keys, replicas and sidecar resources survive
unrelated writes. Other managed sidecar fields (image, args and security settings)
still reconcile; unknown live sidecars follow the rendered spec. Profile-only writes
preserve the entire live spec apart from the selected Unleash resource keys.
New instances retain the historical small defaults and no CPU
limit, persisting explicit small intent even when selection is disabled.
Explicit selection owns only CPU request and memory request/limit.
It may roll out the Deployment; downsizing is not safe merely because an
instantaneous memory reading is low.

Rollout is blocked pending the evidence in [#647](https://github.com/nais/bifrost/issues/647#issuecomment-6364771461):

1. **Before merging the compatibility build**, verify deployed revisions/settings
   and census raw intent schemas and field names, without printing intent payloads
   or nonces. Unknown fields fail closed even with the flag false. Establish
   compatibility evidence before the automatic canary/fan-out, not at a later dev-nais hold.
2. Before enabling, inventory **all** persisted explicit profiles and actual
   resources, including default small on new creates. Enabling can enforce every
   mismatch and roll out instances, not just those selected through a UI. Approve
   aggregate request reservations against capacity and measured demand.
3. Quiesce in-flight version/channel transactions and competing autoscalers.
   Disabling new migration admission alone does not stop existing transactions.
4. Confirm the connected tenant interactively, then validate one tenant on dev-nais
   first, not nav. Do not automate tenant switching or JITA. Real-kind tests prove
   Bifrost HTTP/CRD/reconciler behavior, not Unleasherator Deployment Pod templates;
   controller evidence is still required. Disabled `in_sync` is not profile compliance.
5. To disable, set the Helm/Fasit flag false and retain this compatible build.
   Existing intent/resources remain; new selection and enforcement stop. This
   neither undoes a rollout nor permits a general binary rollback. The **first
   create writes schema 2 even with the flag false**, so pre-schema-2 code is not
   a safe revert. Do not rewrite fleet intents or resize back to small as rollback.

Deployed configurations, census, capacity and upstream Pod-template behavior remain
unverified; this plan is not rollout approval.

### Instance Response

```json
{
  "name": "my-unleash",
  "namespace": "unleash",
  "version": "5.10.2",
  "version_source": "custom",
  "custom_version": "5.10.2",
  "release_channel_name": "",
  "status": "Ready",
  "status_label": "green",
  "api_url": "https://my-unleash-api.example.com/api/",
  "web_url": "https://my-unleash.example.com/",
  "created_at": "2024-01-01T00:00:00Z",
  "age": "2 weeks"
}
```

### Release Channel Response

```json
{
  "name": "stable",
  "version": "5.11.0",
  "type": "sequential",
  "schedule": "0 2 * * 1",
  "description": "Stable release channel",
  "current_version": "5.11.0",
  "last_updated": "2024-03-15T10:30:00Z",
  "created_at": "2024-01-01T00:00:00Z"
}
```

### Error Response

```json
{
  "error": "validation_failed",
  "message": "Configuration validation failed",
  "details": {
    "validation": "cannot specify both custom_version and release_channel_name"
  },
  "status_code": 400
}
```

## Architecture

Bifröst operates as a Kubernetes-native control plane:

```text
┌─────────────┐
│   Bifröst   │ (REST API)
│   API       │
└──────┬──────┘
       │
       ├──► Kubernetes ──► Unleasherator CRDs ──► Unleash Pods
       │
       └──► Google Cloud SQL ──► PostgreSQL Databases
```

### Dependencies

**Kubernetes Operators:**

- [Unleasherator](https://github.com/nais/unleasherator) - Unleash instance controller
- [FQDN Network Policy](https://github.com/GoogleCloudPlatform/gke-fqdnnetworkpolicies-golang) - Network isolation

**Google Cloud:**

- Cloud SQL (PostgreSQL) for Unleash databases
- Service account with **Cloud SQL Admin** role

## Configuration

Bifröst is configured via environment variables.

### Google Configuration

| Variable                    | Description                 |
| --------------------------- | --------------------------- |
| `BIFROST_GOOGLE_PROJECT_ID` | The Google Cloud project ID |

### OAuth JWT Configuration

| Variable                                          | Description                                |
| ------------------------------------------------- | ------------------------------------------ |
| `BIFROST_UNLEASH_INSTANCE_WEB_OAUTH_JWT_AUDIENCE` | Expected audience for OAuth JWT validation |

### Unleash Configuration

| Variable                                     | Description                                                   |
| -------------------------------------------- | ------------------------------------------------------------- |
| `BIFROST_UNLEASH_INSTANCE_NAMESPACE`         | The Kubernetes namespace where Unleash instances are deployed |
| `BIFROST_UNLEASH_INSTANCE_SERVICE_ACCOUNT`   | The Kubernetes service account used by Unleash instances      |
| `BIFROST_UNLEASH_SQL_INSTANCE_ID`            | The SQL instance ID for Unleash databases                     |
| `BIFROST_UNLEASH_SQL_INSTANCE_REGION`        | The SQL instance region for Unleash databases                 |
| `BIFROST_UNLEASH_SQL_INSTANCE_ADDRESS`       | The SQL instance address for Unleash databases                |
| `BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_HOST`  | The ingress host for Unleash instances Web UI                 |
| `BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_CLASS` | The ingress class for Unleash instances Web UI                |
| `BIFROST_UNLEASH_INSTANCE_API_INGRESS_HOST`  | The ingress host for Unleash instances API                    |
| `BIFROST_UNLEASH_INSTANCE_API_INGRESS_CLASS` | The ingress class for Unleash instances API                   |

### Channel migration

Channel migrations use the authoritative `bifrost.nais.io/desired-state`
annotation and persist progress in `bifrost.nais.io/channel-migration`.
`BIFROST_UNLEASH_CHANNEL_MIGRATION_MAX_CANDIDATES` limits new transactions per
process start; existing transactions are always recovered. Automatic rollback
is disabled unless `BIFROST_UNLEASH_CHANNEL_MIGRATION_ROLLBACK_SAFE=true`
explicitly declares the database downgrade safe. Target write failures and
timeouts otherwise persist the `manual-recovery-required` phase for operator
action.

### Legacy adoption

`BIFROST_RECONCILER_AUTO_ADOPT=true` is disabled by default. It only considers
Bifrost-managed legacy CRs that lack valid `bifrost.nais.io/desired-state`.
It canonicalizes one CR at a time, writes
`bifrost.nais.io/adoption=pending`, then waits for current-generation
reconciliation and connectivity before admitting another. Unknown or multiple
markers stop adoption. Dry-run renders the deterministic next candidate
without writes. Pending-marker cleanup continues when auto-admission is
disabled.

## Development

### Prerequisites

- Go 1.21+
- Local Kubernetes cluster (kind, k3d, or minikube)
- Google Cloud service account with Cloud SQL Admin role

### Setup

Install required CRDs in your local cluster:

```bash
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/gke-fqdnnetworkpolicies-golang/main/config/crd/bases/networking.gke.io_fqdnnetworkpolicies.yaml
kubectl apply -f https://raw.githubusercontent.com/nais/unleasherator/main/config/crd/bases/unleash.nais.io_unleashes.yaml
```

### Local Environment

Set these variables for local development:

| Variable                         | Example          | Description              |
| -------------------------------- | ---------------- | ------------------------ |
| `BIFROST_SERVER_HOST`            | `127.0.0.1`      | API server bind address  |
| `GOOGLE_APPLICATION_CREDENTIALS` | `~/gcp/key.json` | Service account key file |
| `KUBECONFIG`                     | `~/.kube/config` | Kubernetes config file   |

### Running

```bash
# Start the API server
mise run start

# Run tests
mise run test

# Build binary
mise run build

# Run all checks
mise run all
```

## Contributing

Contributions are welcome! Please ensure:

- Tests pass (`mise run test`)
- Code is formatted (`mise run fmt`)
- Linting passes (`mise run lint`)

## Support

For issues and feature requests, please use the [GitHub issue tracker](https://github.com/nais/bifrost/issues).

## License

[MIT](LICENSE)
