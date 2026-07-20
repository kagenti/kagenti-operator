# Bundle Service

This package contains the Rossoctl bundle-service binary and SRE-facing operational guidance for running the service in Kubernetes.

## Purpose

The bundle service serves OPA authorization bundles to AuthBridge clients over HTTP. It runs as a cluster service and provides policy bundles assembled from global, namespace, and client-specific `AuthorizationPolicy` CRs.

## Deployment

### Quick start (kind)

```bash
./hack/bundle-service-kind.sh [cluster-name] [namespace]
# Defaults: cluster=rossoctl, namespace=rossoctl-system
```

This script builds the image, loads it into kind, installs the CRD, applies the default global policy CR, and deploys the service.

### Production manifests

Deploy using the manifests in `operator/config/bundleservice/`:

- `deployment.yaml`
- `service.yaml`
- `serviceaccount.yaml`
- `rbac.yaml`
- `networkpolicy.yaml`
- `default-policy.yaml` — the default global `AuthorizationPolicy` CR

Default deployment settings:

- Namespace: `rossoctl-system`
- Deployment name: `bundle-service`
- Service name: `bundle-service`
- Port: `8080`

### Prerequisites

The `AuthorizationPolicy` CRD must be installed before deploying:

```bash
kubectl apply -f config/crd/bases/agent.rossoctl.dev_authorizationpolicies.yaml
```

The default global policy CR must be applied for the service to produce valid bundles:

```bash
kubectl apply -f config/bundleservice/default-policy.yaml
```

### Runtime configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `POD_NAMESPACE` | (from downward API) | Namespace where the service runs; used to identify global policies |
| `LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `KUBECONFIG` | (in-cluster) | Path to kubeconfig when running outside the cluster |

## Health and readiness

| Endpoint | Purpose | Success | Failure |
|----------|---------|---------|---------|
| `GET /healthz` | Liveness probe | `200 OK` | — |
| `GET /readyz` | Readiness probe | `200 OK` | `503 Service Unavailable` |

The service reports ready once the Kubernetes informer has synced. Until then, all `/bundles` requests return `503`.

## Operational behavior

### Request flow

1. Client sends `GET /bundles?spiffe={trust-domain}/ns/{namespace}/sa/{name}`
2. Service checks readiness, parses identity, verifies authorization
3. Fast path: ETagCache hit + `If-None-Match` match → `304 Not Modified`
4. Medium path: BundleCache hit → return cached bundle
5. Slow path: build bundle from CRs (deduplicated by singleflight, bounded by semaphore)

### Concurrency limits

At most 10 bundle builds run concurrently. Additional requests queue until a slot is available. This protects the Kubernetes API server and etcd from thundering herd during cluster restarts.

Requests for the same client identity are deduplicated — only one build runs while others wait for its result.

### Expected response codes

| Code | Cause |
|------|-------|
| `200` | Bundle served successfully |
| `304` | Bundle unchanged (ETag match) |
| `400` | Missing or unparseable SPIFFE ID |
| `403` | Identity verification failed |
| `413` | Bundle exceeds 5 MB limit |
| `500` | Internal error during bundle build |
| `503` | Service not ready (informer not synced) |

### Logs

Structured logs via `log/slog`. Key log events:

- `bundle request received` — every incoming request (URL, method)
- `bundle response` — every response (URL, status, size)
- `bundle built` — new bundle generated (namespace, name, hash)
- `bundle exceeds size limit` — bundle too large (namespace, name, size)
- Policy change events from watcher (scope, namespace, key)

## Global policy CR

The default global `AuthorizationPolicy` CR (`config/bundleservice/default-policy.yaml`) defines the decision logic for all four OPA query paths. It determines how namespace and client tiers are combined.

Platform engineers can customize this CR to:

- Remove namespace tier support entirely
- Add or remove namespace override capability
- Change combination logic (AND → OR, add additional checks)
- Set default allow/deny behavior

If the global CR is deleted, OPA has no rules at the query paths and all decisions default to deny (fail-closed).

## Audit and monitoring

Monitor:

- Deployment availability and pod restarts
- Readiness/liveness probe status
- `413` and `500` response rates
- Bundle build latency (via log timestamps)
- Cache effectiveness (frequency of `304` vs `200` responses)

## Related resources

- `operator/config/bundleservice/` — deployment manifests and default policy
- `operator/config/crd/bases/agent.rossoctl.dev_authorizationpolicies.yaml` — CRD definition
- `operator/internal/bundleservice/` — service implementation

## Architecture and API details

For architecture and API contract details, see [ARCHITECTURE.md](ARCHITECTURE.md).
