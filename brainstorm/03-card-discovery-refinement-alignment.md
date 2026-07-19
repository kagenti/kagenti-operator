# Brainstorm: Card Discovery Refinement Alignment

**Date:** 2026-05-26
**Status:** active
**Origin:** Divergence analysis between upstream implementation (PR #372) and RHAIENG-4948 refinement document
**Depends on:** Response from @iamiller on RHAIENG-4948 comment

## Problem

PR #372 merged the card discovery feature upstream. The RHAIENG-4948 refinement document describes the same feature but with different naming, condition semantics, and a transport security model that the implementation doesn't capture. Since the PR just merged, we can still make breaking changes without migration concerns. This is a window to align the implementation with the refinement doc where it makes the design better, before consumers depend on the current field names and condition reasons.

## What to Adopt

### 1. Transport Security Field on CardStatus

**What the refinement doc says:** Condition reasons should distinguish `FetchSucceeded` (mTLS) from `FetchSucceededPlainHTTP` (plain HTTP).

**What we propose instead:** A dedicated `transportSecurity` field on `CardStatus`, plus transport-aware condition reasons.

```go
// TransportSecurity indicates the transport layer used for the card fetch.
// +optional
TransportSecurity string `json:"transportSecurity,omitempty"`
```

Values: `"mTLS"`, `"plainHTTP"`.

**Why a field is better than condition reasons alone:**
- Condition reasons are transient. When the condition flips to `False` on the next failure, the transport info from the last successful fetch is lost. A field persists with the card data.
- Consumers (UI, policy engines, compliance dashboards) can query `.status.card.transportSecurity` directly without parsing condition reasons.
- We already have `attestedAgentSpiffeID` which implies mTLS when non-empty, but that's implicit. An explicit field is clearer.

**In addition to the field**, the condition reason should reflect transport:
- `CardSynced` (mTLS verified)
- `CardSyncedInsecure` (plain HTTP, no transport identity)
- Or keep a single `CardSynced` reason and let consumers check the field

**Implementation:**
- `fetchCard()` already branches on `AuthenticatedFetcher` vs `AgentFetcher`. Set `transportSecurity` based on which path executed.
- The mTLS path (SPIFFE fetcher) sets `"mTLS"`.
- The HTTP fallback path (DefaultFetcher) sets `"plainHTTP"`.
- The ConfigMap path (signed card from init-container) could set `"configMap"` for completeness.

**Why this matters:**
A card fetched over plain HTTP within the cluster network has no transport-level identity guarantee. Any pod in the same namespace could serve a fake card to the controller. Knowing the transport mode lets platform engineers assess their security posture: "all my agents have `transportSecurity: mTLS`" vs "agent X is still on plainHTTP because SPIRE isn't configured in that namespace."

### 2. Split ServiceNotFound into ServiceNotFound + WorkloadNotReady

**What the refinement doc says:** Use `WorkloadNotReady` when the workload has no Ready pods, separate from service resolution failure.

**Current behavior:** Our `ServiceNotFound` reason conflates two distinct cases:
1. The workload exists but has no matching Service (configuration issue)
2. The workload isn't ready yet (transient, will resolve on its own)

These need different operator responses:
- `ServiceNotFound`: check your Service configuration, selector labels, port names
- `WorkloadNotReady`: wait, the pods are starting up

**Implementation:**
- In `fetchAndUpdateCard`, check workload readiness (Ready pods > 0) before attempting service resolution.
- If workload has zero Ready pods, set `CardSynced=False, reason=WorkloadNotReady`.
- If workload is ready but no Service matches, keep `CardSynced=False, reason=ServiceNotFound`.

### 3. Unified Condition Model (Rename + Restructure)

The refinement doc and our implementation each have gaps. Neither model is complete. The merged model below takes the best from both.

**Problems with our current model:**
- `CardSynced` as both condition type AND reason reads as a stutter in `kubectl describe`: `CardSynced True CardSynced`
- No transport awareness. Can't tell from `kubectl describe` whether the fetch was secure.
- `ServiceNotFound` conflates "no Service exists" with "workload not ready"

**Problems with the refinement doc model:**
- Missing `FetchSkipped` (no-op when pod template unchanged)
- Missing `DiscoveryDisabled` (feature flag off)
- `FetchPending` adds an `Unknown` state without actionable signal

**Proposed merged model:**

| Type | Status | Reason | When |
|------|--------|--------|------|
| `CardFetched` | True | `Fetched` | mTLS fetch succeeded |
| `CardFetched` | True | `FetchedInsecure` | plain HTTP fetch (no transport identity) |
| `CardFetched` | True | `FetchSkipped` | pod template unchanged, existing data valid |
| `CardFetched` | False | `FetchFailed` | timeout, connection error, invalid JSON, 404 |
| `CardFetched` | False | `ServiceNotFound` | workload exists but no matching Service |
| `CardFetched` | False | `WorkloadNotReady` | workload has no ready pods |
| `CardFetched` | False | `DiscoveryDisabled` | feature flag off, stale data cleared |

**Why this is better than either model alone:**

1. **Condition type `CardFetched`**: past participle as adjective (like `Scheduled`, `Initialized` in core Kubernetes). Accurately describes event-driven fetch, no "sync" implication. No stutter with reason names.

2. **Transport in the reason (`Fetched` vs `FetchedInsecure`)**: visible in `kubectl describe` without needing `-o yaml`. A platform engineer sees `CardFetched True FetchedInsecure` and immediately knows there's a security gap. The `transportSecurity` field is the machine-readable source of truth; the reason is the human-readable signal.

3. **`FetchSkipped` preserved** (from our model): important for operators watching rollouts. Tells them "nothing changed, existing data is still valid." Without it, after a non-template spec change (like scaling), the condition stays at the previous reason, which is confusing.

4. **`WorkloadNotReady` + `ServiceNotFound` split**: different operator response. `WorkloadNotReady` = wait, pods are starting. `ServiceNotFound` = check your Service labels and selectors.

5. **`DiscoveryDisabled`** (shortened from `CardDiscoveryDisabled`): since the condition type already says `Card`, the prefix is redundant.

**Why we can do this now:**
The PR just merged. No external consumers depend on the `CardSynced` condition type yet. No downstream tooling parses it. The printer column can be renamed in the same change. If we wait, this becomes a breaking API change requiring migration.

**Implementation:**
- Rename condition type constant and printer column
- Update `fetchAndUpdateCard` to set reason based on which fetch path executed (authenticated vs default fetcher)
- Update all test assertions
- Update constitution reference

### 4. Rename cardId to cardHash

**Current field:** `cardId` (type `string`, holds a SHA-256 content hash).

**Problem:** `cardId` suggests a unique identifier assigned to the card (like a UUID or database ID). The field actually holds a SHA-256 content hash used for change detection: "has the card content changed since last fetch?" Calling it `cardId` misleads consumers into treating it as a stable identifier.

**Proposed:** Rename to `cardHash`. Matches the refinement doc's `CardHash` naming and accurately describes the field's purpose: a content hash for diffing.

**Implementation:** Rename the Go field, JSON tag, and CRD schema. Update `computeCardContentHash` callers and tests. Straightforward find-and-replace.

### 5. Port Resolution Chain (Refinement Doc's Approach)

**Current implementation:** `serviceHTTPPort()` looks for generic HTTP port names (`http`, `https`, `grpc`), falls back to first port.

**Refinement doc:** `rossoctl.io/port annotation` -> port named `a2a` -> first port -> default 8000.

**Why the doc's approach is better:**

1. **Protocol-specific port name (`a2a`)**: We're fetching A2A agent cards, not generic HTTP content. If a Service has an admin port named `http` on port 80 and an A2A port named `a2a` on port 8000, our current code picks the wrong one.

2. **Annotation escape hatch (`rossoctl.io/port`)**: Standard Kubernetes pattern for when auto-detection fails. Near-zero implementation cost (check one annotation before the existing port resolution). Useful for multi-port Services, non-standard configurations, or when the A2A endpoint runs on an unusual port.

3. **Default 8000**: The A2A Python SDK defaults to port 8000. Our current code has no default and relies entirely on what's in the Service spec.

**Proposed resolution chain:**
1. `rossoctl.io/port` annotation on the Service (explicit override, highest priority)
2. Service port named `a2a` (protocol-specific match)
3. Service port named `http` (generic fallback, backward compatible)
4. First port in Service spec (last resort)

This is backward compatible: existing Services with port named `http` still work. Services that add a port named `a2a` get more precise resolution. The annotation is there for edge cases.

**Implementation:** Modify `serviceHTTPPort()` to check the annotation first, then look for `a2a` port name before falling back to `http`/first port. Small change, no new dependencies.

### 6. Rename fetchedAt to lastCardFetchTime

**Current field:** `fetchedAt` (type `*metav1.Time`).

**Problem:** On a `CardStatus` struct that holds agent card data, `fetchedAt` is ambiguous. It could mean "when was this card originally published by the agent." The field actually records when the controller last fetched the card from the agent's endpoint.

**Proposed:** Rename to `lastCardFetchTime`. Self-documenting: it's the timestamp of the last controller-initiated fetch. The `last` prefix conveys that multiple fetches happen over time (one per rollout), which matches the event-driven design.

**Implementation:** Rename Go field, JSON tag, and CRD schema. Same find-and-replace as `cardHash`.

## What NOT to Adopt

### FetchPending Intermediate State

The doc suggests `CardFetched=Unknown, reason=FetchPending` when a rollout is detected but pods aren't ready. In practice, the reconcile either succeeds or fails. There's no meaningful window where the controller holds a "pending" state between reconciles. controller-runtime requeueing handles this naturally. Adding an intermediate condition state adds complexity without actionable signal.

## Implementation Scope

| Change | Breaking? | Effort | Files |
|--------|-----------|--------|-------|
| Add `transportSecurity` field to `CardStatus` | Additive | Small | `agentruntime_types.go`, `zz_generated.deepcopy.go`, CRD YAMLs |
| Populate `transportSecurity` from fetch path | No | Small | `agentruntime_controller.go` (in `fetchCard`) |
| Unified condition model (type rename + reason restructure) | Condition type + reasons | Medium | Controller, tests, CRD printer column, constitution |
| Rename `cardId` to `cardHash` | Field rename | Small | `agentruntime_types.go`, controller, tests, CRD YAMLs |
| Rename `fetchedAt` to `lastCardFetchTime` | Field rename | Small | `agentruntime_types.go`, controller, tests, CRD YAMLs |
| Port resolution: annotation + `a2a` port name | Behavior change | Small | `agentruntime_controller.go` (`serviceHTTPPort`) |
| Tests for all of the above | No | Medium | `agentruntime_controller_test.go` |

Total estimate: 1 day of work, all in one PR.

## Open Questions

1. Should `transportSecurity` be an enum (string with known values) or a free-form string? Enum is safer for consumers but less extensible.
2. The ConfigMap fetch path (signed card from init-container): should `transportSecurity` be `"configMap"`, `"signed"`, or something else? This path is being deprecated but still exists during coexistence.
3. Wait for Ian's response on RHAIENG-4948 before implementing, or proceed since the changes improve on both models?

## Decision Needed

Waiting for Ian's response on RHAIENG-4948 before proceeding. If he agrees the current approach is fine, we adopt only the substantive improvements (transport security field, workload readiness split) without the naming changes. If he prefers alignment with the doc, we do the full set including the `CardFetched` rename.

## References

- PR #372: upstream implementation (merged 2026-05-25)
- RHAIENG-4948: refinement document with sequence diagrams
- RHAIENG-4944: parent epic (Agent Discovery via mTLS)
- Brainstorm #01: original card-into-agentruntime brainstorm
- Constitution v1.0.0: controller-runtime safety principles
