# Implementation Plan: mTLS Transport Security for Agent Communication

**Branch**: `003-mtls-transport-security` | **Date**: 2026-06-03 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/003-mtls-transport-security/spec.md`

## Summary

Enable mTLS as the default transport security for controller-to-agent and agent-to-agent communication. Authbridge mTLS is already implemented in cortex across all proxy modes (envoy, proxy-sidecar, lite). The remaining work is operator-side: the controller sets a `rossoctl.io/mtls-mode` annotation on the pod template (triggering rolling restart on change), the webhook reads `mTLSMode` from the AgentRuntime CR and sets `MTLS_MODE` env var on the authbridge container, defaulting `mTLSMode` to `permissive`, wiring `SpiffeFetcher` as the default card fetcher, adding `MTLSReady` status conditions, and deprecating the JWS signing pipeline flags. SPIRE is the sole certificate provider; Istio is out of scope.

## Technical Context

**Language/Version**: Go 1.25, controller-runtime v0.23.3

**Primary Dependencies**: controller-runtime, go-spiffe/v2, k8s.io/apimachinery

**Storage**: Kubernetes CRD status subresource (no external storage)

**Testing**: Ginkgo/Gomega (unit + integration), envtest for controller tests, e2e in `test/e2e/`

**Target Platform**: Kubernetes 1.31+

**Project Type**: Kubernetes operator (kubebuilder-based)

**Performance Goals**: mTLS adds < 5ms to TLS handshake; no polling (event-driven only)

**Constraints**: No Istio dependency; feature-gated where appropriate (Constitution V); SPIRE must be deployed for mTLS; backward compatible with existing deployments

**Scale/Scope**: Hundreds of AgentRuntimes per cluster; one SPIRE-issued SVID per agent pod

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Notes |
|-----------|--------|-------|
| I. Reconciler Status Integrity | PASS | `MTLSReady` condition and `status.card` mutations must use save/restore pattern around any Patch calls. Plan ensures status updates happen after metadata patches. |
| II. Spec-Anchored Testing | PASS | All tests will create objects in envtest and read back from API server to verify conditions and status.card fields. |
| III. Controller-Runtime Safety | PASS | ConfigMap hash annotation (metadata patch) happens before Status().Update() for MTLSReady and card fields. No blocking HTTP calls without timeout. |
| IV. CRD-First Design | PASS | `MTLSReady` condition type added as a constant. `mTLSMode` default change is in the CRD schema. All status fields use concrete types with JSON tags. |
| V. Feature-Gated Rollout | PASS | `--enable-verified-fetch` retained as kill switch (default: true). Signing flags default to false with deprecation warnings. mTLS default-on is via `mTLSMode` field default, not a flag — operators can set `disabled` to opt out. |

No constitution violations. No complexity justification needed.

## Project Structure

### Documentation (this feature)

```text
specs/003-mtls-transport-security/
├── brainstorm.md        # Design exploration and clarifications
├── spec.md              # Feature specification
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── contracts/           # Phase 1 output
└── tasks.md             # Phase 2 output (/speckit-tasks)
```

### Source Code (rossoctl-operator)

```text
operator/
├── api/v1alpha1/
│   ├── agentruntime_types.go       # MODIFY: mTLSMode default, MTLSReady condition type
│   └── zz_generated.deepcopy.go    # REGENERATE: after type changes
├── internal/controller/
│   ├── agentruntime_controller.go      # MODIFY: set rossoctl.io/mtls-mode annotation, MTLSReady condition, SpiffeFetcher default
│   ├── agentruntime_controller_test.go # MODIFY: tests for mTLS annotation, conditions, defaults
├── internal/webhook/injector/
│   └── pod_mutator.go                  # MODIFY: read mTLSMode from AgentRuntime CR, set MTLS_MODE env var on authbridge
├── cmd/
│   └── main.go                     # MODIFY: flag defaults, deprecation warnings
├── config/
│   ├── crd/bases/                  # REGENERATE: CRD manifests
│   └── rbac/                       # VERIFY: no new RBAC needed
└── test/
    ├── e2e/                        # MODIFY: mTLS e2e scenario
    └── integration/                # MODIFY: mTLS integration test
```

### Source Code (cortex — verification only)

```text
cortex/authbridge/
├── cmd/
│   ├── authbridge-envoy/main.go    # VERIFY: mTLS wiring exists
│   ├── authbridge-proxy/main.go    # VERIFIED: mTLS wiring complete
│   └── authbridge-lite/main.go     # VERIFIED: mTLS wiring complete
├── authlib/
│   ├── tls/                        # VERIFIED: ServerConfig, ClientConfig
│   ├── spiffe/                     # VERIFIED: X509Source, Provider
│   ├── config/                     # VERIFY: MTLS config schema
│   └── listener/
│       ├── reverseproxy/           # VERIFIED: MTLSOptions integration
│       └── forwardproxy/           # VERIFIED: MTLSOptions integration
└── tests/                          # ADD: mTLS integration tests
```

**Structure Decision**: Existing kubebuilder project structure. All changes extend existing files. No new packages or directories needed in the operator. Authbridge changes are verification and testing only — implementation is already on main.

## Complexity Tracking

No constitution violations. No complexity justification needed.
