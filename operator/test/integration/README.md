# Integration Tests

Integration tests for the rossoctl-operator that run against a live Kubernetes cluster. They exercise the reconciler's binding evaluation and trust bundle rotation logic using real CRDs and API server interactions, with mocked fetcher and signature providers.

## Prerequisites

- [Kind](https://kind.sigs.k8s.io/) installed
- `kubectl` configured (Kind sets this up automatically)
- A running Kind cluster with rossoctl CRDs installed (`make install` handles CRD installation)
- The rossoctl operator must **not** be running in the cluster — these tests call `Reconcile` manually, and a running operator would cause concurrent reconciliations that interfere with assertions

## Running

```sh
# From the rossoctl-operator directory:
make test-integration
```

This will:
1. Verify `kind` is installed and a cluster is running
2. Install CRDs via `make install`
3. Run all integration tests with `-tags=integration`

### Filtering by tag

The `INTEGRATION_TAGS` variable controls which build tags are used (default: `integration`):

```sh
make test-integration INTEGRATION_TAGS=authbridge
```

### Running individual tests

```sh
go test -v -tags=integration ./test/integration/... -timeout 5m -run TestIdentityBindingIntegration
go test -v -tags=integration ./test/integration/... -timeout 5m -run TestTrustBundleRotation
```

## Build Tag Convention

All integration tests use the `//go:build integration` build tag so they are excluded from `go test ./...` and `make test` (unit tests). Future test files that require a live cluster should use the same tag. Additional tags (e.g., `authbridge`) can be introduced for tests with extra dependencies.

## Existing Tests

### Identity Binding (`identity_binding_integration_test.go`)

Verifies the AgentCard reconciler's SPIFFE identity binding evaluation against a real cluster:

- **Matching binding**: Creates a Deployment, Service, and AgentCard, then runs the reconciler with a mock signature provider returning the expected SPIFFE ID. Asserts the AgentCard status shows `Bound=true`.
- **Non-matching binding**: Same setup but the mock provider returns a SPIFFE ID that does not match the allowlist. Asserts `Bound=false`.

### Trust Bundle Rotation (`trust_bundle_rotation_integration_test.go`)

Verifies that the operator detects trust bundle changes (simulating CA rotation) and triggers a workload rollout restart:

1. Initial reconciliation records `bundle-hash=hash-v1` on the Deployment
2. The mock provider's bundle hash is changed to `hash-v2`
3. After re-reconciliation, asserts the Deployment's `bundle-hash` annotation is updated and a `resign-trigger` annotation is set (triggering pod rollout)

## What Is Mocked vs. Real

| Component | Real or Mocked |
|-----------|---------------|
| Kubernetes API server | Real (Kind cluster) |
| CRDs (AgentCard, etc.) | Real (installed via `make install`) |
| AgentCard reconciler | Real |
| Agent card fetcher | Mocked (`mockFetcher`) |
| Signature provider | Mocked (`mockSignatureProvider`, `rotationMockProvider`) |
