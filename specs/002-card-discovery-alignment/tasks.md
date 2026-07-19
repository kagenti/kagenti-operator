# Tasks: Card Discovery Refinement Alignment

**Input**: Design documents from `specs/002-card-discovery-alignment/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md

**Tests**: Included. Existing tests must be migrated and new tests added per spec acceptance scenarios.

**Organization**: Tasks grouped by user story. US1 and US2 are P1 (implement first), US3 and US4 are P2.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Phase 1: Foundational (Field Renames + Condition Type)

**Purpose**: Rename fields and condition type across all files. These are blocking prerequisites for all user stories since US1-US4 all reference the new names.

- [X] T001 Rename `CardID` to `CardHash` and `FetchedAt` to `LastCardFetchTime` in `CardStatus` struct, update JSON tags from `cardId`/`fetchedAt` to `cardHash`/`lastCardFetchTime` in `operator/api/v1alpha1/agentruntime_types.go`
- [X] T002 Add `TransportSecurity string` field with JSON tag `transportSecurity` and godoc to `CardStatus` struct in `operator/api/v1alpha1/agentruntime_types.go`
- [X] T003 Run `make generate` to regenerate `operator/api/v1alpha1/zz_generated.deepcopy.go`
- [X] T004 Rename `ConditionTypeCardSynced` to `ConditionTypeCardFetched` with value `"CardFetched"` in `operator/internal/controller/agentruntime_controller.go` (line 74)
- [X] T005 Update all condition reason string constants: `"CardSynced"` to `"Fetched"`, `"CardFetchFailed"` to `"FetchFailed"`, `"CardDiscoveryDisabled"` to `"DiscoveryDisabled"` in `operator/internal/controller/agentruntime_controller.go`
- [X] T006 Update all references from `CardID` to `CardHash` and `FetchedAt` to `LastCardFetchTime` in `fetchAndUpdateCard` function in `operator/internal/controller/agentruntime_controller.go`
- [X] T007 Rename printer column from `CardSynced` to `CardFetched` and update JSONPath from `CardSynced` to `CardFetched` in kubebuilder marker on `AgentRuntime` struct in `operator/api/v1alpha1/agentruntime_types.go`
- [X] T008 Run `make manifests` to regenerate CRD YAMLs in `operator/config/crd/bases/` and sync to `charts/operator/crds/` via `make sync-chart-crds`
- [X] T009 Update all test assertions referencing `CardSynced`, `CardID`, `FetchedAt`, and old reason strings in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T010 Run `make test` to verify zero regressions after renames

## Phase 2: US1 - Transport Security Visibility (P1)

**Goal**: Platform engineers can determine transport security posture from `status.card.transportSecurity` and the `CardFetched` condition reason.

**Independent Test**: Deploy agents with and without mTLS, verify `transportSecurity` field and condition reason reflect the transport used.

- [X] T011 [US1] Set `TransportSecurity` field in `fetchCard` function: set `"mTLS"` when `AuthenticatedFetcher` path executes (fetchResult != nil), set `"plainHTTP"` when `AgentFetcher` HTTP fallback path executes, in `operator/internal/controller/agentruntime_controller.go`
- [X] T012 [US1] Update `fetchAndUpdateCard` to pass `transportSecurity` from `fetchCard` into `cardStatus.TransportSecurity` and use transport-aware condition reasons: `"Fetched"` for mTLS, `"FetchedInsecure"` for plainHTTP, in `operator/internal/controller/agentruntime_controller.go`
- [X] T013 [US1] Add test: card fetched with stub fetcher (simulating plain HTTP) sets `transportSecurity: "plainHTTP"` and reason `FetchedInsecure` in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T014 [US1] Add test: card fetched with authenticated fetcher mock sets `transportSecurity: "mTLS"` and reason `Fetched` in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T015 [US1] Add test: transport security field updates when transport changes (mTLS to plainHTTP on re-fetch) in `operator/internal/controller/agentruntime_controller_test.go`

## Phase 3: US2 - Unified Condition Model (P1)

**Goal**: Every condition reason maps to exactly one diagnostic action. `WorkloadNotReady` split from `ServiceNotFound`.

**Independent Test**: Create AgentRuntimes in all states and verify each condition type/status/reason combination.

- [X] T016 [US2] Add `checkWorkloadReady` helper that checks `readyReplicas > 0` for Deployments/StatefulSets and skips check for Sandboxes in `operator/internal/controller/agentruntime_controller.go`
- [X] T017 [US2] Call `checkWorkloadReady` before `resolveServiceForWorkload` in `fetchAndUpdateCard`; set `CardFetched=False, reason=WorkloadNotReady` if check fails, in `operator/internal/controller/agentruntime_controller.go`
- [X] T018 [US2] Add test: workload with zero readyReplicas sets `CardFetched False WorkloadNotReady` in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T019 [US2] Add test: workload ready but no Service sets `CardFetched False ServiceNotFound` (existing test, update assertions) in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T020 [US2] Add test: discovery disabled sets `CardFetched False DiscoveryDisabled` (existing test, update assertions) in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T021 [US2] Verify existing `FetchSkipped` test passes with new condition type name in `operator/internal/controller/agentruntime_controller_test.go`

## Phase 4: US3 - Accurate Field Names (P2)

**Goal**: `cardHash` and `lastCardFetchTime` are verified end-to-end via envtest.

**Independent Test**: Create AgentRuntime in envtest, run reconcile with stub fetcher, read back from API server, verify field names.

- [X] T022 [P] [US3] Add envtest integration test: create AgentRuntime, reconcile with stub fetcher, read back from API server, assert `status.card.cardHash` is a SHA-256 hex string and `status.card.lastCardFetchTime` is an RFC 3339 timestamp in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T023 [US3] Verify CRD schema contains `cardHash` and `lastCardFetchTime` fields and does NOT contain `cardId` or `fetchedAt` by inspecting generated `operator/config/crd/bases/agent.rossoctl.dev_agentruntimes.yaml`

## Phase 5: US4 - Protocol-Aware Port Resolution (P2)

**Goal**: Port resolution uses `rossoctl.io/port` annotation, then `a2a` port name, then `http`, then first port.

**Independent Test**: Create Services with various port configurations and verify correct port selection.

- [X] T024 [US4] Replace body of `serviceHTTPPort` in `operator/internal/controller/agentruntime_controller.go`: check `rossoctl.io/port` annotation first (parse as int32, validate > 0, log warning on invalid), then port named `a2a`, then port named `http`, then first port, default 8000
- [X] T025 [US4] Add test: Service with `rossoctl.io/port` annotation uses annotated port in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T026 [US4] Add test: Service with port named `a2a` uses that port (no annotation) in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T027 [US4] Add test: Service with invalid `rossoctl.io/port` annotation falls back to port name resolution in `operator/internal/controller/agentruntime_controller_test.go`
- [X] T028 [P] [US4] Add test: Service with both annotation and `a2a` port, annotation takes priority in `operator/internal/controller/agentruntime_controller_test.go`

## Phase 6: Polish & Cross-Cutting

**Purpose**: Constitution update, final validation, CRD verification.

- [X] T029 Update `ConditionTypeCardSynced` references to `ConditionTypeCardFetched` in constitution at `.specify/memory/constitution.md`
- [X] T030 Run full `make test` to verify all tests pass (zero regressions, target: 181+ tests passing)
- [X] T031 Verify no references to old names (`cardId`, `fetchedAt`, `CardSynced`) remain in Go source files via `grep -r` across `operator/`

## Dependencies

```text
Phase 1 (Foundational) ──► Phase 2 (US1) ──► Phase 3 (US2) ──► Phase 6 (Polish)
                       ──► Phase 4 (US3) ──────────────────────►
                       ──► Phase 5 (US4) ──────────────────────►
```

- Phase 1 blocks all user stories (field renames must complete first)
- US1 (transport security) blocks US2 (condition model uses transport-aware reasons)
- US3 and US4 are independent of US1/US2 and can run in parallel after Phase 1
- Phase 6 runs after all user stories complete

## Parallel Execution Opportunities

After Phase 1 completes:
- **Batch 1**: T011-T015 (US1) + T022-T023 (US3) + T024-T028 (US4) can run in parallel
- **Batch 2**: T016-T021 (US2) runs after US1 completes
- **Batch 3**: T029-T031 (Polish) runs after all stories complete

## Implementation Strategy

**MVP**: Phase 1 + Phase 2 (US1) delivers transport security visibility, the highest-value change.

**Full delivery**: All 6 phases, estimated at 31 tasks. No external dependencies. All changes are within the operator codebase.
