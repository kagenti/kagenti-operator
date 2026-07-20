# Deep Review Findings

**Date:** 2026-05-28
**Branch:** 002-card-discovery-alignment
**Rounds:** 1
**Gate Outcome:** PASS
**Invocation:** manual

## Summary

| Severity | Found | Fixed | Remaining |
|----------|-------|-------|-----------|
| Critical | 0 | 0 | 0 |
| Important | 7 | 4 | 3 (pre-existing) |
| Minor | 14 | 0 | 14 |
| **Total** | **21** | **4** | **17** |

**Agents completed:** 5/5 (+ 1 external tool)
**Agents failed:** none

Note: 3 remaining Important findings are pre-existing architectural patterns (redundant API calls per reconcile, persistCardFetchAnnotation race, unconditional fetchAndUpdateCard call) not introduced by this feature. They are documented for future improvement but do not block this gate.

## Findings

### FINDING-1
- **Severity:** Important
- **Confidence:** 85
- **File:** operator/internal/controller/agentruntime_controller.go:905-910
- **Category:** correctness
- **Source:** correctness-agent
- **Round found:** 1
- **Resolution:** fixed (round 1)

**What is wrong:**
Missing nil guard on `cardData` returned from `AgentFetcher.Fetch` in the plain HTTP fetch path. If `Fetch()` returns `(nil, nil)`, the code would dereference `cardData` causing a nil pointer panic. The authenticated fetch path already had this guard.

**Why this matters:**
Any alternate `Fetcher` implementation that returns `(nil, nil)` would cause a runtime panic. The asymmetry with the authenticated path (which checks for nil) was a correctness gap.

**How it was resolved:**
Added nil check after the plain HTTP fetch, matching the pattern already used in the authenticated fetch path.

### FINDING-2
- **Severity:** Important
- **Confidence:** 90
- **File:** operator/internal/controller/agentruntime_controller.go:594-595
- **Category:** production-readiness
- **Source:** production-agent (also reported by: correctness-agent, architecture-agent)
- **Round found:** 1
- **Resolution:** fixed (round 1)

**What is wrong:**
`serviceHTTPPort` created a logger from `context.Background()` instead of using the caller's context, losing all contextual fields (namespace, name, reconcile ID).

**Why this matters:**
Invalid annotation warning logs would lack resource context, making production debugging in multi-tenant clusters difficult.

**How it was resolved:**
Changed function signature to accept `ctx context.Context` and updated all call sites.

### FINDING-3
- **Severity:** Important
- **Confidence:** 95
- **File:** operator/internal/controller/agentruntime_controller_test.go (missing test)
- **Category:** test-quality
- **Source:** test-agent
- **Round found:** 1
- **Resolution:** fixed (round 1)

**What is wrong:**
No test exercised the `FetchSkipped` condition reason (US2 acceptance scenario 7, FR-003).

**Why this matters:**
`FetchSkipped` is the only "success" condition reason with no test coverage. A regression in change-detection-key logic would go unnoticed.

**How it was resolved:**
Added test that performs a successful fetch, then calls `fetchAndUpdateCard` again without modifying the workload, asserting `CardFetched=True` with reason `FetchSkipped`.

### FINDING-4
- **Severity:** Important
- **Confidence:** 95
- **File:** operator/internal/controller/agentruntime_controller_test.go (missing test)
- **Category:** test-quality
- **Source:** test-agent
- **Round found:** 1
- **Resolution:** fixed (round 1)

**What is wrong:**
No test exercised the `FetchFailed` condition reason (US2 acceptance scenario 5, FR-003). The "card data retention" test used a nonexistent deployment, which hit `WorkloadNotReady` instead of `FetchFailed`.

**Why this matters:**
If the `FetchFailed` path had a bug (e.g., clearing card data), no test would catch it.

**How it was resolved:**
Added test with a ready Deployment + Service and a stub fetcher that returns an error, asserting `CardFetched=False` with reason `FetchFailed` and error message content.

### FINDING-5 (pre-existing, not fixed)
- **Severity:** Important
- **Confidence:** 85
- **File:** operator/internal/controller/agentruntime_controller.go:753-840
- **Category:** production-readiness
- **Source:** production-agent
- **Round found:** 1
- **Resolution:** deferred (pre-existing pattern)

**What is wrong:**
Multiple redundant API GET calls for the same workload object per reconcile (5 calls for resolveTargetRef, workloadChangeKey, checkWorkloadReady, resolveServiceForWorkload, applyWorkloadConfig).

**Why this matters:**
Unnecessary API server load in steady state.

**Deferred because:** This is a pre-existing architectural pattern not introduced by this feature. Fixing it requires refactoring the reconciler to pass a workload object through the call chain.

### FINDING-6 (pre-existing, not fixed)
- **Severity:** Important
- **Confidence:** 80
- **File:** operator/internal/controller/agentruntime_controller.go:849-866
- **Category:** production-readiness
- **Source:** production-agent
- **Round found:** 1
- **Resolution:** deferred (pre-existing pattern)

**What is wrong:**
`persistCardFetchAnnotation` save/restore pattern for status is fragile under concurrent reconciles.

**Deferred because:** Pre-existing pattern, not introduced by this feature.

### FINDING-7 (pre-existing, not fixed)
- **Severity:** Important
- **Confidence:** 85
- **File:** operator/internal/controller/agentruntime_controller.go:213
- **Category:** production-readiness
- **Source:** production-agent
- **Round found:** 1
- **Resolution:** deferred (pre-existing pattern)

**What is wrong:**
`fetchAndUpdateCard` called unconditionally on every reconcile even when discovery is disabled, potentially causing unnecessary status updates.

**Deferred because:** Pre-existing pattern, not introduced by this feature.

## Minor Findings (14)

| ID | Category | File | Description |
|----|----------|------|-------------|
| M-1 | architecture | agentruntime_controller.go:622 | `getAgentTLSPort` duplicated between controllers |
| M-2 | architecture | agentruntime_controller.go:1062 | Excessive 10-line goconst nolint comment |
| M-3 | architecture | agentruntime_controller.go:494 | `countConfiguredPods` lists broader than needed |
| M-4 | architecture | agentruntime_types.go:92,128 | AuthBridgeMode/MTLSMode as raw strings |
| M-5 | security | agentruntime_types.go:163 | TraceSpec.Endpoint has no CRD validation |
| M-6 | security | agentruntime_types.go:148 | AllowedAudiences has no MaxItems |
| M-7 | security | agentruntime_controller.go:599 | Port annotation allows > 65535 |
| M-8 | security | agentruntime_types.go:244 | SkillImageRef.Image has no pattern validation |
| M-9 | correctness | agentruntime_controller.go:561 | checkWorkloadReady has no default case |
| M-10 | test-quality | agentruntime_controller_test.go:955 | FR-013 test doesn't exercise FetchFailed path |
| M-11 | test-quality | agentruntime_controller_test.go | No first-port fallback test |
| M-12 | test-quality | agentruntime_controller_test.go | No configMap transportSecurity test |
| M-13 | test-quality | agentruntime_controller_test.go:204,258 | Discarded reconcile errors |
| M-14 | production | agentruntime_controller.go:952 | ensureNamespaceConfigMaps runs uncached on every reconcile |

## Post-Fix Spec Coverage

| Requirement | Implementation | Status |
|-------------|---------------|--------|
| FR-001: transportSecurity field with enum | api/v1alpha1/agentruntime_types.go:203-206 | verified |
| FR-002: CardSynced -> CardFetched | internal/controller/agentruntime_controller.go:74 | verified |
| FR-003: Condition reasons | internal/controller/agentruntime_controller.go:716-800 | verified |
| FR-004: cardId -> cardHash | api/v1alpha1/agentruntime_types.go:197 | verified |
| FR-005: fetchedAt -> lastCardFetchTime | api/v1alpha1/agentruntime_types.go:193 | verified |
| FR-006: Port resolution chain | internal/controller/agentruntime_controller.go:594-620 | verified |
| FR-007: Workload readiness check | internal/controller/agentruntime_controller.go:558-583 | verified |
| FR-008: Printer column rename | api/v1alpha1/agentruntime_types.go:280 | verified |
| FR-009: Constitution update | No references to update | verified |

All spec requirements verified after fix loop.
