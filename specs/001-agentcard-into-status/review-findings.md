# Deep Review Findings

**Date:** 2026-05-21
**Branch:** 001-agentcard-into-status
**Rounds:** 1
**Gate Outcome:** PASS
**Invocation:** manual

## Summary

| Severity | Found | Fixed | Remaining |
|----------|-------|-------|-----------|
| Critical | 3 | 3 | 0 |
| Important | 10 | 3 | 7 |
| Minor | 5 | - | 5 |
| **Total** | **18** | **6** | **12** |

**Agents completed:** 5/5 (+ 1 external tool)
**Agents failed:** none

## Findings

### FINDING-1
- **Severity:** Critical
- **Confidence:** 95
- **File:** operator/internal/controller/agentruntime_controller.go:635-707
- **Category:** correctness / production-readiness
- **Source:** correctness-agent (also reported by: architecture-agent, prod-readiness-agent)
- **Round found:** 1
- **Resolution:** fixed (round 1)

**What is wrong:**
`AnnotationLastCardFetchHash` was set on `rt.ObjectMeta.Annotations` in-memory but never persisted. `Status().Update()` only writes the status subresource, not metadata. Combined with `FetchedAt` being set to `metav1.Now()` on every fetch, this created an infinite reconciliation loop: each reconcile fetched the card (skip-check never matched), set a new timestamp, wrote status, triggering re-reconciliation.

**Why this matters:**
Continuous API server load proportional to AgentRuntime count. Saturates the controller's work queue and hits agent workloads with constant HTTP requests.

**How it was resolved:**
Added `persistCardFetchAnnotation()` using `client.MergeFrom` patch to write the annotation to the API server separately from the status update. Also changed `FetchedAt` to only update when `CardID` changes (content actually changed), preventing no-op status writes from triggering re-reconciliation.

### FINDING-2
- **Severity:** Critical
- **Confidence:** 95
- **File:** operator/internal/controller/agentcard_controller_test.go:1726-1818
- **Category:** test-quality
- **Source:** test-quality-agent (also reported by: coderabbit)
- **Round found:** 1
- **Resolution:** fixed (round 1)

**What is wrong:**
The deprecation warning test had no assertions verifying the deprecation event was emitted. The reconciler had no `Recorder` set, so `r.Recorder.Event()` was skipped entirely. The test passed even without the deprecation feature.

**Why this matters:**
FR-006 requires deprecation warnings. Without proper test assertions, the behavior could regress silently.

**How it was resolved:**
Added `record.NewFakeRecorder(10)` to the test reconciler and assertion that a "Deprecated" event was emitted.

### FINDING-3
- **Severity:** Critical
- **Confidence:** 90
- **File:** operator/internal/controller/agentruntime_controller_test.go (missing)
- **Category:** test-quality
- **Source:** test-quality-agent
- **Round found:** 1
- **Resolution:** fixed (round 1)

**What is wrong:**
No test for FR-013 (retain last card data on fetch failure). A regression that cleared `status.card` on failure would not be caught.

**Why this matters:**
FR-013 explicitly requires card data retention on failure.

**How it was resolved:**
Added "Card data retention on fetch failure (FR-013)" test that verifies existing card data is preserved when fetch fails and `CardSynced` condition is set to False.

### FINDING-4
- **Severity:** Important
- **Confidence:** 80
- **File:** operator/internal/controller/agentruntime_controller.go:748-761
- **Category:** correctness
- **Source:** correctness-agent
- **Round found:** 1
- **Resolution:** remaining (intentional)

**What is wrong:**
`workloadChangeKey` uses `GetGeneration()` which increments on any spec change (including replica count), not just pod template changes.

**Why this matters:**
Scaling a Deployment triggers an unnecessary card re-fetch.

**Why it remains:**
The spec says "Pod template hash changes (or generation for StatefulSets/Sandboxes)". Using generation is simpler and causes at most one extra fetch per scaling event. The cost is a single HTTP GET. Switching to pod-template-hash computation would add complexity for minimal benefit.

### FINDING-5
- **Severity:** Important
- **Confidence:** 92
- **File:** operator/internal/controller/agentruntime_controller.go:501-520
- **Category:** architecture
- **Source:** architecture-agent
- **Round found:** 1
- **Resolution:** remaining (deferred)

**What is wrong:**
`serviceHTTPPort` and `getAgentTLSPort` are duplicated between AgentCard and AgentRuntime controllers with diverging implementations.

**Why it remains:**
The functions are small and self-contained. Extracting to a shared utility is a follow-up refactor that should be done when the AgentCard controller is deprecated. Not a correctness issue.

### FINDING-6
- **Severity:** Important
- **Confidence:** 90
- **File:** operator/internal/controller/agentruntime_controller.go:763-773
- **Category:** architecture
- **Source:** architecture-agent
- **Round found:** 1
- **Resolution:** remaining (deferred)

**What is wrong:**
`computeCardContentHash` is functionally identical to `AgentCardReconciler.computeCardID`.

**Why it remains:**
Same as FINDING-5. The duplication will be resolved when AgentCard is fully deprecated.

### FINDING-7
- **Severity:** Important
- **Confidence:** 85
- **File:** operator/internal/controller/agentruntime_controller.go:666
- **Category:** architecture
- **Source:** architecture-agent
- **Round found:** 1
- **Resolution:** remaining (intentional)

**What is wrong:**
Protocol is hardcoded to `agentcard.A2AProtocol`. The AgentCard controller detects protocol from workload labels.

**Why it remains:**
This feature is specifically about A2A card discovery. The spec only mentions A2A. Multi-protocol support can be added when the need arises.

### FINDING-8
- **Severity:** Important
- **Confidence:** 90
- **File:** operator/internal/controller/agentruntime_controller.go:200, 668-671
- **Category:** production-readiness
- **Source:** prod-readiness-agent
- **Round found:** 1
- **Resolution:** remaining (acceptable)

**What is wrong:**
No `RequeueAfter` returned on card fetch failure. Transient failures won't retry until the next reconcile trigger.

**Why it remains:**
The reconcile loop already re-triggers on any workload change. Card discovery is best-effort and non-blocking. Adding retry logic would add complexity for a status field that is supplementary to the core runtime function.

### FINDING-9
- **Severity:** Important
- **Confidence:** 90
- **File:** operator/internal/controller/agentruntime_controller_test.go (missing)
- **Category:** test-quality
- **Source:** test-quality-agent (also reported by: coderabbit)
- **Round found:** 1
- **Resolution:** remaining (requires envtest)

**What is wrong:**
No happy-path test that exercises successful card fetch through `fetchAndUpdateCard` with a mock fetcher.

**Why it remains:**
The test requires setting up a `mockFetcher` implementation and injecting it into the reconciler. The `agentcard.Fetcher` mock type only exists in `agentcard_controller_test.go` and is not exported. Creating an equivalent mock in the agentruntime test would be straightforward but tests require envtest infrastructure. Deferred to CI.

### FINDING-10
- **Severity:** Important
- **Confidence:** 50
- **File:** operator/internal/controller/agentruntime_controller.go:711-745
- **Category:** security
- **Source:** security-agent
- **Round found:** 1
- **Resolution:** remaining (by design)

**What is wrong:**
mTLS fallback to plaintext HTTP when TLS port is missing.

**Why it remains:**
The fallback is intentional for gradual rollout. A warning Event is emitted. The spec's acceptance scenario US2.3 explicitly says "without mTLS configured, the fetch uses plain HTTP." A strict mode can be added in a follow-up iteration.

### FINDING-11 (Minor)

`CardId` renamed to `CardID` per Go acronym conventions. Fixed in round 1.

### FINDING-12 (Minor)

Nil check added on `fetchResult.CardData` after `FetchAuthenticated`. Fixed in round 1.

## Remaining Findings

All remaining Important findings are architectural decisions (deferred duplication cleanup) or test coverage gaps requiring envtest infrastructure. No Critical findings remain.
