# Research: mTLS Transport Security for Agent Communication

## R1: Authbridge mTLS Config Delivery (UPDATED)

**Decision**: ~~The operator injects `mtls:` block into the authbridge ConfigMap.~~ **Superseded per PR #405 review.** The controller sets a `rossoctl.io/mtls-mode` annotation on the pod template. The webhook reads `mTLSMode` from the AgentRuntime CR at pod CREATE time and sets `MTLS_MODE` env var on the authbridge container.

**Annotation**: `rossoctl.io/mtls-mode: permissive` (or `strict` or `disabled`) on pod template.

**Env var**: `MTLS_MODE=permissive` (or `strict` or `disabled`) on authbridge container.

**Resolved mode behavior** (unchanged):
- `permissive`: Inbound uses byte-peek TLS-sniffing (accepts both TLS and plaintext). Outbound uses plaintext.
- `strict`: Inbound rejects non-TLS connections. Outbound requires TLS.

**Rationale**: ConfigMap injection was the original approach, but PR #405 removes CR-level fields from the config hash. Using an annotation on the pod template ensures mTLSMode changes trigger rolling restarts independently of the config hash. The webhook already reads AgentRuntime CRs at pod CREATE — adding the env var is a natural extension.

**Alternatives considered**:
- ConfigMap injection (original approach) — rejected: ConfigMap is namespace-level, doesn't support per-AgentRuntime mTLSMode.
- Authbridge watches AgentRuntime CR directly — rejected: bad sidecar design (API server watches, RBAC blast radius, tight coupling).

## R2: Authbridge SPIFFE Config Schema

**Decision**: The authbridge sidecar reads SPIFFE configuration from a `spiffe:` block. This is already injected by the operator when spiffe-helper is present.

**Schema**:
```yaml
spiffe:
  socket: "unix:///spiffe-workload-api/spire-agent.sock"
  mirrorFiles: true
  mirrorDir: "/spiffe-certs"
```

**Rationale**: The SPIFFE block is a prerequisite for `mtls:`. The operator already handles this — no changes needed.

## R3: Rolling Restart Mechanism (UPDATED)

**Decision**: ~~mTLSMode flows through the config hash.~~ **Superseded per PR #405.** The controller sets a `rossoctl.io/mtls-mode` annotation on the workload's pod template. When the annotation value changes (e.g., `permissive` → `strict`), Kubernetes detects a pod template change and triggers a rolling restart. This is independent of the platform config hash (`rossoctl.io/config-hash`), which now only reflects cluster + namespace defaults.

**Rationale**: PR #405 removes all CR-level fields from the config hash (2-layer merge: cluster + namespace only). A separate annotation preserves per-AgentRuntime mTLSMode restart semantics without conflating it with platform config.

## R4: SpiffeFetcher Default Behavior

**Decision**: When the controller pod has the SPIRE Workload API socket available (`--verified-fetch-spiffe-socket`), use `SpiffeFetcher` as the primary fetcher. Fall back to `DefaultFetcher` only when SPIRE is not configured.

**Current behavior**: `SpiffeFetcher` is only used when `--enable-verified-fetch=true` (default: false). Changing the default to `true` and keeping the flag as a kill switch satisfies the "enabled by default, disabled is opt-in" requirement.

**Rationale**: Minimal code change — just a default value flip. The flag remains for emergency rollback per Constitution V.

## R5: MTLSReady Condition Design

**Decision**: Add a new condition type `MTLSReady` to AgentRuntime status conditions.

**Condition states**:

| Reason | Status | When |
|--------|--------|------|
| `SPIREAvailable` | True | SPIRE is deployed and certificates are available |
| `SPIREUnavailable` | False | mTLSMode is non-disabled but SPIRE is not deployed or unreachable |
| `MTLSDisabled` | True | mTLSMode is explicitly set to `disabled` (no mTLS expected) |

**Detection**: The controller checks whether the spiffe-helper init container is present in the workload's pod template and whether the SPIRE agent socket volume mount exists. If mTLSMode is `permissive` or `strict` but these are absent, `MTLSReady=False`.

**Rationale**: Follows the existing condition pattern (TargetResolved, ConfigResolved, Ready). Uses the same `metav1.Condition` type.

## R6: Deprecation Warning Implementation

**Decision**: At operator startup in `cmd/main.go`, after flag parsing, check if any legacy signing flags are set to `true` and log structured deprecation warnings.

**Flags to deprecate**:
- `--require-a2a-signature` (default: false)
- `--signature-audit-mode` (default: false)
- `--enforce-network-policies` (default: false)

**Warning format**: `slog.Warn("flag deprecated", "flag", name, "replacement", "mTLS transport security", "removal", "next release")`

**Rationale**: Structured logging matches existing operator patterns. No runtime behavior change — just warnings.

## R7: Authbridge-Envoy mTLS Status

**Decision**: Verify whether `authbridge-envoy/main.go` has the same mTLS wiring as proxy and lite modes.

**Finding**: Need to verify — the envoy mode uses `ext_proc`/`ext_authz` listeners instead of HTTP reverse/forward proxy. mTLS in envoy mode may be handled via Envoy's native `DownstreamTlsContext`/`UpstreamTlsContext` rather than the Go-level `authtls` package. The operator generates envoy bootstrap config, so TLS contexts need to be included there.

**Action**: Verify during implementation. If envoy mode uses native Envoy TLS, the operator must inject TLS context config into the envoy bootstrap ConfigMap.
