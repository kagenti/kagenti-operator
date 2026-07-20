# Feature Specification: mTLS Transport Security for Agent Communication

**Feature Branch**: `003-mtls-transport-security`
**Created**: 2026-06-03
**Status**: Draft
**Input**: Brainstorm document `specs/003-mtls-transport-security/brainstorm.md`
**ADR**: ODH-ADR-AgentOps-0002 — Agent Network Policy and mTLS Identity
**Jira**: RHAIENG-4944 — Agent Discovery via mTLS
**Parent Feature**: RHAISTRAT-1599 — Productize & Downstream the Agent Operator
**Target Release**: rhoai-3.5.EA2

## Scope

Enable mutual TLS (mTLS) as the default transport security layer for two communication paths in the Rossoctl platform:

1. **Controller-to-agent** — the operator controller fetching agent cards and verifying agent identity over mTLS
2. **Agent-to-agent** — inter-agent calls where both sides prove identity via SPIRE-issued X.509 certificates

All status updates (transport security, attested SPIFFE ID, mTLS readiness conditions) are written to **AgentRuntime status**, not AgentCard.

### Out of Scope

- Istio integration — no Istio dependency; SPIRE is the sole mTLS provider
- User-supplied certificates and cert-manager — deferred to a future iteration
- `spec.policy` enforcement (NetworkPolicy, AuthorizationPolicy) — separate spec
- AgentCard CRD removal — separate migration spec
- Cross-cluster agent federation — future work
- Bearer token / OAuth2 authorization — handled by authbridge, orthogonal to mTLS
- Downstreaming logistics for cortex — separate spike
- Webhook injector changes — existing spiffe-helper injection is sufficient

## Clarifications

### Session 2026-06-03

- Q: What sidecar mode should mTLS target? → A: All sidecar modes (envoy, proxy-sidecar, lite, waypoint). mTLS must be sidecar-agnostic.
- Q: What certificate sources should be supported? → A: SPIRE only. No Istio dependency.
- Q: How should the controller obtain its SPIFFE identity? → A: go-spiffe SDK directly in the controller binary (already implemented via SpiffeFetcher).
- Q: How should mTLS be enforced? → A: Enabled by default (permissive). Disabled is opt-in per-AgentRuntime.
- Q: How should certificates reach the data-plane sidecar? → A: spiffe-helper sidecar (file-based). Keeps proxy certificate-source-agnostic.
- Q: What happens when mTLS is enabled but SPIRE is not deployed? → A: Fail clearly. Error condition on AgentRuntime; operator must deploy SPIRE or set mTLSMode: disabled.

## Current State — What Already Exists

### Authbridge (cortex) — DONE

mTLS is fully implemented in authbridge across all three proxy modes on main:

- `authlib/tls/server.go` — `ServerConfig()` for mTLS reverse-proxy listener (inbound)
- `authlib/tls/client.go` — `ClientConfig()` for mTLS forward-proxy dialer (outbound)
- `authlib/spiffe/source.go` — `X509Source` interface abstracting SPIFFE credentials
- `authlib/spiffe/provider.go` — File-based provider reading from spiffe-helper PEM files
- `authbridge-proxy/main.go` — Full mTLS wiring: `cfg.MTLS` → `MTLSOptions` → listeners
- `authbridge-lite/main.go` — Same mTLS wiring as proxy (size-optimized variant)
- Permissive mode: inbound byte-peek TLS-sniffing, outbound plaintext
- Strict mode: inbound rejects non-TLS, outbound TLS-or-fail
- Per-handshake certificate rotation from spiffe-helper files
- `authtls.Metrics` for TLS handshake observability

### Operator (rossoctl-operator) — Partially Done

- `MTLSMode` field on `AgentRuntimeSpec`: enum with values `disabled`, `permissive`, `strict` (defaults to `disabled`)
- `CardStatus` struct on `AgentRuntimeStatus`: includes `TransportSecurity`, `AttestedAgentSpiffeID`, `ValidSignature`
- `SpiffeFetcher` in `internal/agentcard/fetcher.go`: mTLS-authenticated fetch using go-spiffe X509Source
- `--enable-verified-fetch` flag gates mTLS fetch (default: `false`)
- `--enable-card-discovery` flag gates card discovery into `status.card`
- Webhook injector auto-injects spiffe-helper when `mTLSMode` is non-disabled

## User Scenarios & Testing

### User Story 1 — Agent-to-Agent mTLS by Default (Priority: P1)

A platform operator deploys two agents. Without any mTLS-specific configuration, both agents communicate over mTLS automatically because SPIRE is deployed and mTLS defaults to permissive.

**Why this priority**: This is the core value — mTLS should just work without manual configuration when the infrastructure (SPIRE) is present.

**Independent Test**: Deploy two agent workloads with SPIRE, create AgentRuntimes (no explicit `mTLSMode`), and verify inter-agent calls use mTLS by checking authbridge logs for TLS handshake entries.

**Acceptance Scenarios**:

1. **Given** two AgentRuntimes with no explicit `mTLSMode` set and SPIRE deployed in the cluster, **When** agent A calls agent B, **Then** the authbridge sidecar establishes an mTLS connection using SPIRE-issued SVIDs and both agents' SPIFFE IDs are verified against the trust bundle.
2. **Given** an AgentRuntime with `mTLSMode: disabled`, **When** agent A calls this agent, **Then** the authbridge sidecar accepts plaintext HTTP connections (no TLS required).
3. **Given** an AgentRuntime with `mTLSMode: strict`, **When** a plaintext HTTP request arrives, **Then** the authbridge sidecar rejects the connection.

---

### User Story 2 — Controller-to-Agent Communication over mTLS (Priority: P1)

The operator controller communicates with agent workloads over mTLS by default when SPIRE is available. When fetching the A2A agent card from the live agent endpoint (`/.well-known/agent-card.json`), the controller uses mTLS to verify the agent's identity. The verified SPIFFE identity and transport security metadata are recorded in `AgentRuntime.status.card`. The AgentCard CRD is not involved — the controller talks directly to the agent workload and writes results to AgentRuntime status.

**Why this priority**: Controller-to-agent identity verification is a core security requirement. The SpiffeFetcher already exists; this wires it as the default.

**Independent Test**: Deploy an agent with SPIRE identity, create an AgentRuntime, and verify `status.card.transportSecurity` is `mtls` and `status.card.attestedAgentSpiffeID` contains the agent's SPIFFE ID.

**Acceptance Scenarios**:

1. **Given** an AgentRuntime targeting a workload with SPIRE identity and the controller has access to the SPIRE Workload API, **When** the controller reconciles the AgentRuntime, **Then** the A2A agent card is fetched from the live endpoint over mTLS and `AgentRuntime.status.card.transportSecurity` is `mtls`.
2. **Given** an AgentRuntime targeting a workload with SPIRE identity, **When** the controller fetches the A2A agent card over mTLS, **Then** `AgentRuntime.status.card.attestedAgentSpiffeID` contains the SPIFFE ID extracted from the peer certificate.
3. **Given** a controller without SPIRE configured (no Workload API socket), **When** the controller reconciles an AgentRuntime, **Then** the A2A agent card is fetched over plain HTTP and `AgentRuntime.status.card.transportSecurity` is `http`.

---

### User Story 3 — Clear Error When SPIRE Is Unavailable (Priority: P1)

An operator deploying agents without SPIRE but with mTLS defaulting to enabled gets a clear error condition explaining what's wrong and how to fix it.

**Why this priority**: Fail-clearly prevents silent security gaps. Operators must know when mTLS isn't active.

**Independent Test**: Create an AgentRuntime in a cluster without SPIRE and verify the `MTLSReady` condition is `False` with reason `SPIREUnavailable`.

**Acceptance Scenarios**:

1. **Given** an AgentRuntime with `mTLSMode: permissive` (the default) and no SPIRE deployed, **When** the controller reconciles, **Then** `status.conditions` includes `MTLSReady=False` with reason `SPIREUnavailable` and message `"mTLS requires SPIRE; either deploy SPIRE or set mTLSMode: disabled"`.
2. **Given** an AgentRuntime with `mTLSMode: disabled`, **When** the controller reconciles in a cluster without SPIRE, **Then** `MTLSReady=True` with reason `MTLSDisabled` (mTLS is explicitly opted out — no error).
3. **Given** an AgentRuntime with `MTLSReady=False` and SPIRE is subsequently deployed, **When** the controller reconciles, **Then** `MTLSReady` transitions to `True`.

---

### User Story 4 — JWS Signing Pipeline Deprecation Warning (Priority: P2)

Operators using the legacy JWS signing pipeline receive deprecation warnings directing them to mTLS as the replacement.

**Why this priority**: Signals the migration path without breaking existing setups.

**Independent Test**: Start the operator with `--require-a2a-signature=true` and verify deprecation warnings in the logs.

**Acceptance Scenarios**:

1. **Given** the operator is started with `--require-a2a-signature=true`, **When** the operator boots, **Then** a deprecation warning is logged: `"--require-a2a-signature is deprecated; mTLS replaces JWS card signing. This flag will be removed in a future release."`.
2. **Given** the operator is started with `--signature-audit-mode=true`, **When** the operator boots, **Then** a similar deprecation warning is logged.
3. **Given** the operator is started with none of the legacy signing flags, **When** the operator boots, **Then** no deprecation warnings are logged.

---

### Edge Cases

- What happens when the spiffe-helper is slow to write initial SVIDs? The authbridge proxy blocks at startup until certificates are available (`WaitForCredentialFile` in `authlib/config/resolve.go`). The kubelet restart policy handles prolonged failures.
- What happens when an agent with `mTLSMode: strict` tries to call an agent with `mTLSMode: disabled`? The outbound mTLS handshake fails because the target doesn't present a TLS listener. The authbridge logs the handshake failure. This is expected — strict callers cannot reach non-TLS targets.
- What happens when `mTLSMode` changes on a running AgentRuntime? The ConfigMap hash changes, triggering a rolling restart of the workload. The new sidecar boots with the updated mTLS config.
- What happens during SVID rotation? The authbridge's per-handshake callbacks re-read certificates from disk on every TLS handshake. Rotation is transparent — no restart, no connection drop.
- What happens in a mixed-mode deployment (some agents permissive, some strict)? Permissive agents accept both TLS and plaintext inbound. Strict agents reject plaintext. Permissive outbound is plaintext — a permissive caller cannot reach a strict target. For full mesh mTLS, all agents should be strict.

### Coexistence with Istio mTLS

SPIRE-based mTLS (this spec) and Istio-based mTLS (PR #383 SharedTrust controller, Issue #399 Istio auto-labeling) operate at different layers and are complementary, not competing:

- **SPIRE mTLS** (this spec): Application-layer identity via authbridge sidecars. SPIRE issues X.509 SVIDs that prove agent identity. The authbridge proxy terminates/originates TLS using these certificates.
- **Istio mTLS** (#383, #399): Infrastructure-layer encryption via ztunnel (ambient mode) or Envoy sidecar (sidecar mode). Provides pod-to-pod encryption transparently at L4.

When both are active on the same workload, traffic is double-encrypted (Istio at L4, then SPIRE at L7 inside the authbridge). This is functionally correct but wastes resources. For this iteration, both can coexist without conflict because they operate on different ports/layers. A future optimization could detect Istio enrollment and skip authbridge TLS when Istio ambient mode covers the same path.

The operator does not need to detect or interact with Istio in this spec. If Istio is present, it adds a transparent encryption layer underneath; if absent, authbridge handles mTLS on its own.

### MTLSReady Condition and Ready Condition Interaction

`MTLSReady=False` does NOT block `Ready=True`. The `Ready` condition reflects whether the workload is configured and running — not whether mTLS is active. Blocking Ready would break existing deployments without SPIRE during the transition period.

When `MTLSReady=False`, the operator:
- Sets `Ready=True` (workload is functional)
- Sets `MTLSReady=False/SPIREUnavailable` with actionable message
- Emits a Kubernetes Event (type Warning) so `kubectl describe agentruntime` shows the issue

Operators monitor `MTLSReady` to track mTLS rollout progress across the fleet.

### SPIRE Detection Heuristic

The controller detects SPIRE availability by checking for the spiffe-helper init container or the SPIRE agent socket volume mount in the workload's pod template. This covers the standard deployment pattern where the webhook injects spiffe-helper.

**Known limitation**: SPIRE CSI driver deployments use a `csi` volume type instead of the spiffe-helper init container. The detection heuristic should also check for CSI volumes with driver `csi.spiffe.io`. This is a follow-up enhancement — for the initial implementation, spiffe-helper is the supported pattern.

## Requirements

### Functional Requirements

- **FR-001**: The system MUST default `mTLSMode` to `permissive` when not explicitly set on an AgentRuntime CR.
- **FR-002**: The controller MUST set a `rossoctl.io/mtls-mode` annotation on the workload's pod template with the resolved `mTLSMode` value (`permissive`, `strict`, or `disabled`). The webhook reads this annotation (and/or the AgentRuntime CR directly) at pod CREATE time to configure the authbridge sidecar.
- **FR-003**: The webhook MUST set the `MTLS_MODE` environment variable on the authbridge sidecar container based on the AgentRuntime's `mTLSMode` value. Authbridge reads this env var at startup to configure its TLS listeners.
- **FR-004**: The system MUST set an `MTLSReady` condition on AgentRuntime status indicating whether mTLS infrastructure (SPIRE) is available.
- **FR-005**: The system MUST use `SpiffeFetcher` (mTLS) as the default card fetcher when the controller has access to the SPIRE Workload API socket.
- **FR-006**: The system MUST fall back to `DefaultFetcher` (plain HTTP) when SPIRE is not configured on the controller pod.
- **FR-007**: The system MUST record `status.card.transportSecurity` as `mtls` or `http` on AgentRuntime to indicate which transport was used for the card fetch.
- **FR-008**: The system MUST record `status.card.attestedAgentSpiffeID` on AgentRuntime with the SPIFFE ID extracted from the peer certificate when the card is fetched over mTLS.
- **FR-009**: The system MUST trigger a rolling restart of the workload when `mTLSMode` changes. The controller sets a `rossoctl.io/mtls-mode` annotation on the pod template; when this annotation value changes, Kubernetes triggers a rolling restart. This is independent of the platform config hash.
- **FR-010**: The system MUST log deprecation warnings at operator startup when legacy JWS signing flags (`--require-a2a-signature`, `--signature-audit-mode`, `--enforce-network-policies`) are set to `true`.
- **FR-011**: The system MUST default legacy JWS signing flags to `false`.
- **FR-012**: The system MUST change the `--enable-verified-fetch` flag default to `true` (kill switch retained for one release cycle).

### Key Entities

- **AgentRuntime**: Existing CRD extended with mTLS defaults. `spec.mTLSMode` controls transport security. `status.card` holds transport security metadata. `status.conditions` includes `MTLSReady`. All mTLS-related status goes here, NOT on AgentCard.
- **Authbridge Sidecar**: Per-pod sidecar container configured by the webhook at pod CREATE time. Reads `MTLS_MODE` env var to configure TLS listeners.
- **SPIRE**: External dependency providing X.509 SVIDs via the Workload API. Must be deployed for mTLS to function.
- **spiffe-helper**: Sidecar container that fetches SVIDs from SPIRE and writes PEM files to a shared volume. The authbridge proxy reads these files.
- **SpiffeFetcher**: Existing component in `internal/agentcard/fetcher.go` that performs mTLS-authenticated card fetches using go-spiffe X509Source directly.

## Success Criteria

### Measurable Outcomes

- **SC-001**: Agents deployed with SPIRE communicate over mTLS by default without explicit `mTLSMode` configuration.
- **SC-002**: The controller fetches agent cards over mTLS by default when SPIRE is available, with the transport security and attested SPIFFE ID visible in `AgentRuntime.status.card`.
- **SC-003**: Operators without SPIRE see a clear `MTLSReady=False` condition with actionable guidance.
- **SC-004**: Existing deployments using JWS signing see deprecation warnings and continue functioning during the transition period.
- **SC-005**: mTLS works across all authbridge sidecar modes (envoy, proxy-sidecar, lite).

## Assumptions

- SPIRE is deployed in the cluster and the SPIRE agent socket is accessible to both the controller pod and agent workload pods.
- The authbridge mTLS implementation in cortex (on main) is stable and tested.
- Each agent workload has exactly one Pod (one-agent-per-pod model), so pod identity equals agent identity (SPIFFE ID).
- The spiffe-helper sidecar is already injected by the webhook when `mTLSMode` is non-disabled — no changes to injection logic are needed.
- The authbridge sidecar supports an `MTLS_MODE` environment variable to configure TLS listener mode at startup.

## Repositories Affected

### rossoctl-operator (primary)

| File | Change |
|------|--------|
| `api/v1alpha1/agentruntime_types.go` | Change `mTLSMode` default to `permissive`; add `MTLSReady` condition type |
| `internal/controller/agentruntime_controller.go` | Set `rossoctl.io/mtls-mode` annotation on pod template; set `MTLSReady` condition; use SpiffeFetcher by default |
| `internal/controller/agentruntime_controller_test.go` | Tests for mTLS annotation, default mTLS, MTLSReady condition |
| `internal/webhook/injector/pod_mutator.go` | Read `mTLSMode` from AgentRuntime CR; set `MTLS_MODE` env var on authbridge container |
| `cmd/main.go` | Default `--enable-verified-fetch` to `true`; default signing flags to `false`; add deprecation log warnings |
| `config/crd/bases/` | Regenerate CRD manifests if type changes |

### cortex (verification + testing)

| File | Change |
|------|--------|
| `authbridge/cmd/authbridge-envoy/main.go` | Verify mTLS wiring exists (same as proxy/lite) |
| `authbridge/authlib/listener/` | Integration tests for mTLS handshake across modes |
| `authbridge/authlib/tls/` | Verify permissive/strict behavior matches operator expectations |
