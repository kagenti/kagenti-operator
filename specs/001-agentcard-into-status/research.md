# Research: Consolidate AgentCard Data Into AgentRuntime Status

## R1: Service Endpoint Resolution Strategy

**Decision**: Selector matching. Resolve the workload's Pod selector labels, list Services in the same namespace, find the first Service whose selector matches. Applies to all supported workload types (Deployment, StatefulSet, Sandbox).

**Rationale**: Standard Kubernetes pattern. Works automatically without user annotations. The existing AgentCard controller uses a simpler convention (Service name = workload name via `workload.ServiceName`), but selector matching is more robust for cases where Service and workload names diverge.

**Alternatives considered**:
- Naming convention (Service name = workload name): simpler but brittle when names differ
- Annotation-driven: more flexible but adds configuration burden
- Hybrid (selector match + annotation override): future enhancement if needed

**Implementation note**: The existing `AgentCardReconciler.getWorkload()` sets `ServiceName: targetRef.Name` (line 526 of agentcard_controller.go). For the AgentRuntime controller, we should match this convention initially (use the workload name as the Service name) since it aligns with how Services are typically created for agent workloads. If no Service matches by name, fall back to selector matching.

## R2: Card Fetch Trigger Mechanism

**Decision**: Pod template hash change detection. The AgentRuntime controller already watches workloads and reconciles on changes. The card fetch phase checks whether the workload's pod-template-hash (or generation for StatefulSets/Sandboxes) has changed since the last successful fetch by comparing against a hash stored in an annotation (`agent.rossoctl.dev/last-card-fetch-hash`), not in the CRD status API surface.

**Rationale**: Avoids unnecessary HTTP calls on every reconcile. Pod template hash changes correlate with actual code/config changes that could affect the agent card. The AgentRuntime controller already reconciles on Deployment changes, so no new watches needed.

**Alternatives considered**:
- Periodic polling (SyncPeriod): wastes resources when nothing changed
- Generation-based: doesn't capture all relevant changes
- Always fetch: simpler but wasteful at scale

## R3: Reusable Code from AgentCard Controller and PR #284

**Decision**: Reuse the following components directly:

| Component | Package | Reuse strategy |
|-----------|---------|----------------|
| `agentcard.Fetcher` interface | `internal/agentcard` | Direct reuse, inject into AgentRuntime reconciler |
| `agentcard.AuthenticatedFetcher` interface | `internal/agentcard` | Direct reuse for mTLS path |
| `agentcard.SpiffeFetcher` | `internal/agentcard` | Direct reuse, same X509Source |
| `agentcard.ConfigMapFetcher` | `internal/agentcard` | Direct reuse for signed card ConfigMap path |
| `AgentCardData` struct | `api/v1alpha1` | Embed in new `CardStatus` struct |
| `signature.Provider` interface | `internal/signature` | Direct reuse for JWS verification |
| `signature.VerificationResult` | `internal/signature` | Direct reuse for verification fields |
| `doHTTPFetch()` | `internal/agentcard` | Already shared between fetchers |
| `extractSpiffeIDFromTLS()` | `internal/agentcard` | Already used by SpiffeFetcher |

**Rationale**: The entire fetch, parse, and verify pipeline already exists. The AgentRuntime controller just needs to call the same interfaces and write results to a different status struct.

## R4: Feature Flag Design

**Decision**: Add `--enable-card-discovery` flag to the operator binary (cmd/main.go). When disabled, the card fetch phase in the reconcile loop is a no-op. When toggled off, the reconciler clears `status.card` on the next reconcile.

**Rationale**: Follows the existing pattern of `--enable-verified-fetch` flag on the AgentCard controller. Simple boolean flag, no runtime reconfiguration needed.

**Implementation note**: The flag controls whether the `Fetcher` and `AuthenticatedFetcher` are injected into the AgentRuntime reconciler at startup. When disabled, the fields are nil and the card fetch phase short-circuits.

## R5: CardStatus Struct Design

**Decision**: New `CardStatus` struct wrapping `AgentCardData` with fetch metadata and verification fields. Placed in `api/v1alpha1/agentruntime_types.go`.

**Rationale**: Keeps card payload separate from fetch/verification metadata. The `AgentCardData` struct is reused as-is (no duplication). Fetch metadata and verification fields are added alongside, not mixed into the card payload.

**Fields**:
- Card payload: embedded `AgentCardData` (name, description, version, url, skills, capabilities, etc.)
- Fetch metadata: `fetchedAt` (timestamp), `cardId` (SHA-256 content hash), `protocol` (detected agent protocol)
- Verification: `validSignature` (bool), `signatureKeyID`, `attestedAgentSpiffeID`, `signatureVerificationDetails`

**Note (from review feedback)**: `lastPodTemplateHash` was originally in this struct but moved to an annotation (`agent.rossoctl.dev/last-card-fetch-hash`) to avoid coupling the change-detection mechanism to the public API surface.

## R6: Deprecation Warning Implementation

**Decision**: Add a log warning at Info level in `AgentCardReconciler.Reconcile()` when processing a newly created AgentCard (check if `agentCard.CreationTimestamp` is within the last reconcile window). Also emit a Kubernetes Event.

**Rationale**: Non-intrusive. Operators see it in logs and events without any behavior change. The AgentCard controller continues to function normally.

**Implementation note**: Check `agentCard.CreationTimestamp.After(time.Now().Add(-5 * time.Minute))` as a heuristic for "recently created". Log once per new card, not on every reconcile.
