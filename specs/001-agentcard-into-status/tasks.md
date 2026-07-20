# Tasks: Consolidate AgentCard Data Into AgentRuntime Status

**Input**: Design documents from `specs/001-agentcard-into-status/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Test tasks are included since this is a controller change requiring unit and integration test coverage.

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Phase 1: Setup

**Purpose**: CRD type changes and code generation that all stories depend on

- [X] T001 Add `CardStatus` struct to `AgentRuntimeStatus` in `operator/api/v1alpha1/agentruntime_types.go`. The `CardStatus` struct embeds `AgentCardData` (reuse existing struct) and adds: `FetchedAt *metav1.Time`, `CardId string`, `Protocol string`, `ValidSignature *bool`, `SignatureKeyID string`, `SignatureVerificationDetails string`, `AttestedAgentSpiffeID string`. The change-detection hash is stored as an annotation (`agent.rossoctl.dev/last-card-fetch-hash`), not in the struct. Add the `Card *CardStatus` field to `AgentRuntimeStatus`. Add `CardSynced` as a new condition type constant.
- [X] T002 Run `make generate` and `make manifests` in `operator/` to regenerate deepcopy functions and CRD manifests. Verify `zz_generated.deepcopy.go` has the new `CardStatus` deepcopy method and `config/crd/bases/` has the updated AgentRuntime CRD.
- [X] T003 Add `--enable-card-discovery` boolean flag (default: false) to `operator/cmd/main.go`. When enabled, create `agentcard.NewConfigMapFetcher()` and `agentcard.NewSpiffeFetcher()` (conditional on SPIRE config), and inject them into the `AgentRuntimeReconciler` as new fields: `AgentFetcher agentcard.Fetcher`, `AuthenticatedFetcher agentcard.AuthenticatedFetcher`, `SignatureProvider signature.Provider`, `EnableCardDiscovery bool`, `SpireTrustDomain string`. Follow the existing pattern of `--enable-verified-fetch` flag wiring for the AgentCard controller.

**Checkpoint**: CRD types updated, code generated, flag wired. Ready for controller changes.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Service resolution helper and RBAC that all card discovery stories need

**CRITICAL**: No user story work can begin until this phase is complete

- [X] T004 Add a `resolveServiceForWorkload` method to `AgentRuntimeReconciler` in `operator/internal/controller/agentruntime_controller.go`. Given a namespace and Deployment name, first try to get a Service with the same name (matching existing AgentCard convention). If not found, list Services in the namespace, match by Pod selector labels from the Deployment, and return the first match. Return the Service object and the selected port (first HTTP port, or default 8000). Also add `getAgentTLSPort` (reuse logic from `agentcard_controller.go` line 684).
- [X] T005 [P] Add Service `get;list;watch` RBAC for the agentruntime controller in `operator/internal/controller/agentruntime_controller.go` via kubebuilder RBAC markers: `// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch`. Run `make manifests` to update `config/rbac/`.

**Checkpoint**: Service resolution and RBAC ready. User story implementation can begin.

---

## Phase 3: User Story 1 - Discover Agent Capabilities from AgentRuntime (Priority: P1) MVP

**Goal**: AgentRuntime `status.card` is populated with A2A card data fetched from the agent's Service endpoint on rollout events. Operators can read card data from a single resource.

**Independent Test**: Deploy an agent with a valid `/.well-known/agent-card.json`, create an AgentRuntime, and confirm `status.card` is populated via `kubectl get agentruntime -o yaml`.

### Tests for User Story 1

- [X] T006 [P] [US1] Add unit tests for `resolveServiceForWorkload` in `operator/internal/controller/agentruntime_controller_test.go`. Test cases: Service found by name, Service found by selector match, no matching Service, multiple ports (uses first HTTP port), Deployment with no ready pods.
- [X] T007 [P] [US1] Add unit tests for the card fetch phase in `operator/internal/controller/agentruntime_controller_test.go`. Test cases: successful fetch populates `status.card` with all fields, fetch failure retains stale data and sets `CardSynced=False`, invalid JSON sets `CardParseFailed` condition, feature flag disabled skips fetch, pod template hash unchanged skips fetch, feature flag toggled off clears `status.card`.

### Implementation for User Story 1

- [X] T008 [US1] Add a `fetchAndUpdateCard` method to `AgentRuntimeReconciler` in `operator/internal/controller/agentruntime_controller.go`. This is the main card fetch phase, called from `Reconcile()` after step 5 (config hash). Logic: (1) If `EnableCardDiscovery` is false, clear `status.card` if populated, set `CardSynced` condition to `CardDiscoveryDisabled`, return. (2) Read the change-detection hash from annotation `agent.rossoctl.dev/last-card-fetch-hash` on the AgentRuntime; compare against current workload pod template hash (or generation for StatefulSet/Sandbox); if unchanged and `status.card` is populated, set `CardSynced` to `FetchSkipped`, return. (3) Call `resolveServiceForWorkload`. (4) Build service URL via `agentcard.GetServiceURL`. (5) Call `AgentFetcher.Fetch()`. (6) On success: build `CardStatus` from fetched `AgentCardData`, set `fetchedAt`, compute `cardId` via SHA-256, set `protocol`, store current hash in annotation. (7) On failure: retain existing `status.card`, set `CardSynced=False` with error reason. (8) Update status via `r.Status().Update()` with retry. Note: FR-009 (max response body size) is implicitly satisfied because the reused `doHTTPFetch()` in `internal/agentcard/fetcher.go` already enforces `maxCardSize = 1 MiB`.
- [X] T009 [US1] Wire the `fetchAndUpdateCard` call into the `Reconcile()` method in `operator/internal/controller/agentruntime_controller.go`. Insert after the config hash computation (step 5, around line 165) and before the label propagation phase. Pass the resolved target workload info.
- [X] T010 [US1] Extract the change-detection key from the target workload in the `fetchAndUpdateCard` method. For Deployments, read the pod-template-hash label. For StatefulSets and Sandboxes, use the resource generation. Store the key in the `agent.rossoctl.dev/last-card-fetch-hash` annotation on the AgentRuntime object (not in status).

**Checkpoint**: US1 complete. `status.card` is populated on reconcile when the flag is enabled.

---

## Phase 4: User Story 2 - Verified Card Discovery via mTLS (Priority: P1)

**Goal**: When mTLS is configured, the card fetch uses the SPIFFE/SPIRE infrastructure from PR #284 to establish a verified connection. Verification results (attested SPIFFE ID, signature validation) are surfaced in `status.card`.

**Independent Test**: Deploy an agent with SPIRE identity, enable card discovery and mTLS on the AgentRuntime, and verify `status.card` includes `attestedAgentSpiffeID` and `validSignature` fields.

### Tests for User Story 2

- [X] T011 [P] [US2] Add unit tests for mTLS card fetch in `operator/internal/controller/agentruntime_controller_test.go`. Test cases: mTLS fetch populates `attestedAgentSpiffeID` and `validSignature`, mTLS handshake failure retains stale data and sets condition, fallback to HTTP when no TLS port, JWS signature verification populates `signatureKeyID`.

### Implementation for User Story 2

- [X] T012 [US2] Extend `fetchAndUpdateCard` in `operator/internal/controller/agentruntime_controller.go` to support mTLS. After resolving the Service, check for the `agent-tls` named port (reuse `getAgentTLSPort`). If present and `AuthenticatedFetcher` is not nil, call `AuthenticatedFetcher.FetchAuthenticated()` instead of `AgentFetcher.Fetch()`. On success, populate `attestedAgentSpiffeID` from `FetchResult.AgentSpiffeID`.
- [X] T013 [US2] Add JWS signature verification to `fetchAndUpdateCard` in `operator/internal/controller/agentruntime_controller.go`. After fetching the card data (mTLS or HTTP), if `SignatureProvider` is not nil and the card has signatures, call `SignatureProvider.VerifySignature()`. Populate `status.card.validSignature`, `status.card.signatureKeyID`, and `status.card.signatureVerificationDetails` from the `VerificationResult`.
- [X] T014 [US2] Handle mTLS fallback to HTTP in `fetchAndUpdateCard`. If `AuthenticatedFetcher` is set but no `agent-tls` port exists on the Service, fall back to `AgentFetcher.Fetch()` and leave verification fields empty. Log a warning and emit a Kubernetes Event (reuse pattern from agentcard_controller.go line 351-356).

**Checkpoint**: US2 complete. mTLS verified card fetch works with SPIFFE identity extraction and JWS signature validation.

---

## Phase 5: User Story 3 - Deprecation Warning on AgentCard Creation (Priority: P2)

**Goal**: When a new AgentCard CR is created, the controller emits a deprecation log warning. Existing AgentCards continue to function normally.

**Independent Test**: Create an AgentCard CR and check controller logs for the deprecation message.

### Tests for User Story 3

- [X] T015 [P] [US3] Add unit test for deprecation warning in `operator/internal/controller/agentcard_controller_test.go`. Verify that reconciling a recently created AgentCard emits a deprecation log message and Kubernetes Event.

### Implementation for User Story 3

- [X] T016 [US3] Add deprecation warning to `AgentCardReconciler.Reconcile()` in `operator/internal/controller/agentcard_controller.go`. After the deletion/finalizer check (around line 166), check if `agentCard.CreationTimestamp` is within the last 5 minutes. If so, log a warning: "AgentCard is deprecated; card data is now available via AgentRuntime status.card. Migrate to AgentRuntime-based discovery." Also emit a Kubernetes Event with reason "Deprecated" and type Warning.

**Checkpoint**: US3 complete. Deprecation warnings emitted for new AgentCard CRs.

---

## Phase 6: User Story 4 - Feature-Gated Card Discovery (Priority: P2)

**Goal**: The card discovery behavior is fully controlled by the `--enable-card-discovery` flag. Disabling the flag clears stale card data.

**Independent Test**: Start the operator with the flag disabled, verify no card fetch. Enable the flag, verify card fetch works. Disable again, verify `status.card` is cleared.

### Tests and Verification for User Story 4

- [X] T017 [US4] Add unit tests for feature flag toggle behavior in `operator/internal/controller/agentruntime_controller_test.go`. Test cases: (1) flag disabled means no card fetch attempted and `status.card` remains nil, (2) flag disabled clears existing populated `status.card` data and sets `CardSynced` condition to `CardDiscoveryDisabled`, (3) flag enabled triggers fetch and populates `status.card`, (4) toggling flag off after previous population clears data on next reconcile. These tests validate the flag-off cleanup logic built in T008 step 1.

**Checkpoint**: US4 complete. Feature flag fully controls card discovery lifecycle.

---

## Phase 7: Polish and Cross-Cutting Concerns

**Purpose**: End-to-end validation, documentation, and CRD manifest finalization

- [X] T018 [P] Run `make generate && make manifests` in `operator/` to ensure all generated code and CRD manifests are up to date after all changes.
- [X] T019 [P] Add a `CardSynced` print column to the AgentRuntime CRD in `operator/api/v1alpha1/agentruntime_types.go` via kubebuilder marker: `// +kubebuilder:printcolumn:name="CardSynced",type="string",JSONPath=".status.conditions[?(@.type=='CardSynced')].status",description="Card Sync Status"`. Regenerate manifests.
- [ ] T020 [P] Add e2e test scenario to `operator/test/e2e/e2e_test.go` that deploys a test agent Deployment with a mock `/.well-known/agent-card.json` endpoint, creates an AgentRuntime targeting it with card discovery enabled, and verifies `status.card` is populated within 30 seconds.
- [ ] T021 Run full test suite: `make test` in `operator/`. Fix any regressions. Ensure existing AgentCard controller tests still pass.
- [ ] T022 Verify CRD backward compatibility: confirm existing AgentRuntime CRs without `status.card` continue to work without errors when the operator runs with card discovery disabled.

---

## Dependencies and Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: No dependencies, start immediately
- **Phase 2 (Foundational)**: Depends on Phase 1 (T001-T003 must complete)
- **Phase 3 (US1)**: Depends on Phase 2 (T004-T005 must complete)
- **Phase 4 (US2)**: Depends on Phase 3 (T008-T010 must complete, extends `fetchAndUpdateCard`)
- **Phase 5 (US3)**: Can start after Phase 1 (independent of US1/US2, different controller file)
- **Phase 6 (US4)**: Depends on Phase 3 (validates flag behavior of `fetchAndUpdateCard`)
- **Phase 7 (Polish)**: Depends on all user stories completing

### User Story Dependencies

- **US1 (P1)**: After Foundational. No dependencies on other stories.
- **US2 (P1)**: After US1. Extends `fetchAndUpdateCard` with mTLS path.
- **US3 (P2)**: After Setup only. Independent (modifies `agentcard_controller.go`, not `agentruntime_controller.go`).
- **US4 (P2)**: After US1. Validates flag toggle behavior already built in US1.

### Within Each User Story

- Tests written first, then implementation
- Service resolution before card fetch
- Plain HTTP fetch before mTLS extension
- Commit after each task or logical group

### Parallel Opportunities

- T006 and T007 can run in parallel (different test scenarios, same file)
- T011 and T015 can run in parallel (different controller test files)
- T005 can run in parallel with T004 (RBAC markers vs service resolution logic)
- T019, T020, T021 can run in parallel (different files)
- US3 (Phase 5) can run in parallel with US1 (Phase 3) since they modify different controllers

---

## Parallel Example: User Story 1

```bash
# Launch US1 tests in parallel:
Task: "Unit tests for resolveServiceForWorkload in agentruntime_controller_test.go"
Task: "Unit tests for card fetch phase in agentruntime_controller_test.go"

# US3 can run in parallel with US1 (different controller files):
Task: "Deprecation warning in agentcard_controller.go"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup (CRD types + flag)
2. Complete Phase 2: Foundational (service resolution + RBAC)
3. Complete Phase 3: User Story 1 (plain HTTP card fetch)
4. **STOP and VALIDATE**: Test card discovery with a real agent deployment
5. Deploy/demo if ready

### Incremental Delivery

1. Setup + Foundational: CRD and infrastructure ready
2. Add US1 (plain HTTP card fetch): Test independently, deploy (MVP!)
3. Add US2 (mTLS verification): Test with SPIRE, deploy
4. Add US3 (deprecation warning): Independent, can ship alongside US1
5. Add US4 (flag toggle validation): Confirms flag lifecycle
6. Polish: e2e tests, print columns, backward compatibility check

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- Reuses existing `agentcard.Fetcher`, `agentcard.SpiffeFetcher`, `signature.Provider` interfaces (no new packages)
- All new code goes in existing files (no new Go source files)
- CRD change is additive (status field only), no API version bump needed
