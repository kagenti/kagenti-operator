# Review Guide: mTLS Transport Security for Agent Communication

**Generated**: 2026-06-08 | **Spec**: [spec.md](spec.md)

## Why This Change

Agent-to-agent and controller-to-agent communication in rossoctl currently runs over plaintext HTTP by default. While authbridge already has full mTLS support implemented (permissive/strict modes, SPIRE-based SVIDs, per-handshake cert rotation), the operator doesn't activate it by default. Operators must manually set flags and configure mTLS mode. This spec makes mTLS the default transport security, with clear error conditions when SPIRE is unavailable.

## What Changes

1. **mTLS enabled by default**: `mTLSMode` defaults to `permissive` (was implicitly `disabled`). Agents communicate over mTLS automatically when SPIRE is deployed.
2. **MTLSReady condition**: New status condition on AgentRuntime showing whether mTLS infrastructure (SPIRE) is available, with actionable error messages when it's not.
3. **Controller uses mTLS by default**: `--enable-verified-fetch` and `--enable-card-discovery` flags flip to `true`. SpiffeFetcher becomes the default card fetcher.
4. **JWS signing deprecation**: Legacy signing flags (`--require-a2a-signature`, `--signature-audit-mode`, `--enforce-network-policies`) emit deprecation warnings.
5. **Annotation-based mTLS delivery**: Controller sets `rossoctl.io/mtls-mode` annotation on pod template (triggers restart on change). Webhook reads `mTLSMode` from AgentRuntime CR at pod CREATE and sets `MTLS_MODE` env var on authbridge container.

No breaking changes. Existing deployments without SPIRE get a clear `MTLSReady=False` condition and can opt out with `mTLSMode: disabled`.

## How It Works

The implementation leverages heavily what's already built:

- **Authbridge** (rossocortex): mTLS is fully implemented across all proxy modes. No changes needed, only verification.
- **Operator**: The main work is (a) changing the `mTLSMode` kubebuilder default to `permissive`, (b) setting `rossoctl.io/mtls-mode` annotation on the pod template, (c) adding `MTLSReady` condition logic, (d) flipping flag defaults, and (e) webhook sets `MTLS_MODE` env var on authbridge container.
- **SPIRE detection**: The controller checks whether spiffe-helper volume mounts exist in the workload's pod template. If absent while mTLS is enabled, `MTLSReady=False/SPIREUnavailable`.
- **Rolling restart**: When `mTLSMode` changes, the `rossoctl.io/mtls-mode` annotation on the pod template changes, triggering a Kubernetes rolling restart. This is independent of the platform config hash (per PR #405).

Of 24 tasks, 2 are already done (`[DONE]`), 1 is partial (`[PARTIAL]`), and the remaining 21 are new work, mostly focused on the operator side.

## When It Applies

**Applies when**:
- Deploying agents in clusters with SPIRE
- Controller fetching agent cards from live workloads
- Setting or changing `mTLSMode` on AgentRuntime CRs
- Migrating from JWS signing to mTLS-based identity

**Does not apply when**:
- Using Istio service mesh for mTLS (explicitly out of scope, separate effort in [#399](https://github.com/rossoctl/operator/issues/399))
- Working with user-supplied certificates or cert-manager (future iteration)
- Authbridge plugin changes (orthogonal)
- Cross-cluster agent federation (future work)

## Key Decisions

1. **SPIRE only, no Istio dependency**: The spec explicitly scopes to SPIRE-based mTLS. Istio service mesh mTLS (L4, pod-to-pod) is a separate effort tracked in [#399](https://github.com/rossoctl/operator/issues/399) and [PR #383](https://github.com/rossoctl/operator/pull/383). These are complementary (SPIRE = application-layer identity, Istio = infrastructure-layer encryption), not competing.

2. **Permissive as default, not strict**: Accepts both TLS and plaintext inbound. This allows gradual rollout without breaking existing agents that haven't enabled SPIRE yet.

3. **Controller uses go-spiffe SDK directly**: SpiffeFetcher uses `go-spiffe/v2` X509Source in-process, not file-based certificates. This is different from the data-plane approach (spiffe-helper sidecar writing PEM files).

4. **Feature-gated via existing CRD field**: No new CLI flag for mTLS enablement. The `mTLSMode` field on AgentRuntimeSpec is the control surface. `--enable-verified-fetch` is retained as a kill switch only.

5. **JWS signing soft-deprecated**: Flags default to false (already on main), warnings added. No code removal yet, just signaling the migration path.

## Areas Needing Attention

- **Overlap with Istio mTLS work**: [PR #383](https://github.com/rossoctl/operator/pull/383) (SharedTrust controller, already merged) and [Issue #399](https://github.com/rossoctl/operator/issues/399) (Istio auto-labeling) introduce Istio-based mTLS at the infrastructure layer. This spec operates at the application layer (SPIRE). Reviewers should verify these don't conflict at the configuration level (e.g., what happens when both SPIRE mTLS and Istio mTLS are active on the same workload).

- **Annotation + env var contract**: T005 (controller sets `rossoctl.io/mtls-mode` annotation, webhook sets `MTLS_MODE` env var) and T020 (verifying authbridge reads this env var) are the critical integration point. If authbridge doesn't read `MTLS_MODE`, mTLS mode changes have no effect.

- **MTLSReady condition gating Ready**: T016 proposes that `MTLSReady=False` should affect the overall `Ready` condition. The exact behavior (block Ready entirely vs. add a warning) needs careful design, since it changes the controller's availability semantics.

- **SPIRE detection heuristic**: Using spiffe-helper volume mount presence as a proxy for "SPIRE is available" may not cover all deployment patterns (e.g., SPIRE with CSI driver instead of init container).

## Open Questions

- How do SPIRE-based mTLS (this spec) and Istio-based mTLS ([PR #383](https://github.com/rossoctl/operator/pull/383), [Issue #399](https://github.com/rossoctl/operator/issues/399)) coexist? Is double encryption acceptable, or should one disable when the other is active?
- Should `MTLSReady=False` block the overall `Ready=True` condition, or just add a warning?
- Does the SPIRE detection heuristic (spiffe-helper volume check) cover SPIRE CSI driver deployments?

## Review Checklist

- [ ] Key decisions are justified (especially SPIRE-only vs Istio interaction)
- [ ] Scope matches the stated boundaries (no Istio, no user certs, no cross-cluster)
- [ ] Constitution compliance verified (all 5 principles addressed in plan.md)
- [ ] Annotation + env var contract matches authbridge expectations
- [ ] No conflict with existing SharedTrust controller ([PR #383](https://github.com/rossoctl/operator/pull/383))
- [ ] Task reconciliation against `main` is accurate ([DONE]/[PARTIAL] markers)
- [ ] Success criteria are achievable and testable
- [ ] Deprecation warnings are clear and actionable

---

<!-- Code phase sections are appended below this line by the phase-manager command -->
