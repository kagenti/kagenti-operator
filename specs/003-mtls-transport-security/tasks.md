# Tasks: mTLS Transport Security for Agent Communication

**Input**: Design documents from `specs/003-mtls-transport-security/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Test tasks are included since this is a controller change requiring unit and integration test coverage per the constitution (Principle II: Spec-Anchored Testing).

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

**Codebase Audit (2026-06-05)**: Reconciled tasks against current main. Several items are partially or fully implemented. Tasks marked `[DONE]` reflect existing code; tasks marked `[PARTIAL]` need only the remaining delta.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## File Paths

All paths are relative to `operator/` (the Go module root inside the repo):

- `api/v1alpha1/agentruntime_types.go` — CRD types
- `internal/controller/agentruntime_controller.go` — main reconciler (has `fetchCard()`, conditions, etc.)
- `internal/controller/agentruntime_config.go` — config merge and hash (has `resolvedConfig` with `MTLSMode`)
- `internal/controller/agentruntime_controller_test.go` — controller tests
- `internal/webhook/injector/` — sidecar injection (envoy template, resolved config)
- `cmd/main.go` — flag definitions and wiring

## What Already Exists on Main

The following are already implemented and do NOT need new code:

- `MTLSMode` field on `AgentRuntimeSpec` with values `disabled`, `permissive`, `strict` (defaults to empty/disabled)
- `TransportSecurity` enum (`mtls`, `http`) and `CardStatus` struct on status
- `SpiffeFetcher` / `AuthenticatedFetcher` wired into `fetchCard()` — already chooses mTLS vs HTTP
- Webhook resolves `MTLSMode` from CR > namespace > default chain and configures authbridge sidecar
- `--require-a2a-signature`, `--signature-audit-mode`, `--enforce-network-policies` already default to `false`
- Envoy template has `MTLSEnabled`, `MTLSPermissive`, `MTLSStrict` wiring for TLS contexts
- Webhook resolves `MTLSMode` from CR > namespace > default chain
- `authbridge-runtime-config` ConfigMap content is captured in config hash

---

## Phase 1: Setup

**Purpose**: CRD type changes, flag defaults, and code generation

- [ ] T001 Change `mTLSMode` default to `permissive` in `api/v1alpha1/agentruntime_types.go`. Currently the field has no kubebuilder default marker and empty string is treated as disabled. Add `// +kubebuilder:default=permissive` marker on the `MTLSMode` field (line 124). Add `ConditionTypeMTLSReady = "MTLSReady"` constant alongside the existing condition types (line 71-75).
- [ ] T002 Run `make generate && make manifests` to regenerate deepcopy and CRD manifests. Verify the CRD schema shows `default: permissive` for mtlsMode.
- [ ] T003 [P] Change `--enable-verified-fetch` flag default from `false` to `true` in `cmd/main.go` (line 164). Change `--enable-card-discovery` flag default from `false` to `true` in `cmd/main.go` (line 162). Both are needed — verified fetch without card discovery does nothing.
- [ ] T004 [P] Add deprecation warning logs in `cmd/main.go` after `flag.Parse()`. When `--require-a2a-signature`, `--signature-audit-mode`, or `--enforce-network-policies` is explicitly `true`, log: `setupLog.Info("DEPRECATED: flag will be removed in a future release; mTLS transport security replaces JWS signing", "flag", "<name>")`. The defaults are already `false` — no change needed there.

**Checkpoint**: CRD default updated, flags flipped, code regenerated.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: MTLSReady condition logic and mTLS annotation on pod template

**CRITICAL**: No user story work can begin until this phase is complete

- [ ] T005 [SUPERSEDED → REPLACED] Add `rossoctl.io/mtls-mode` annotation to the workload's pod template in `internal/controller/agentruntime_controller.go`. In `applyWorkloadConfig()`, set the annotation value to the resolved `mTLSMode` (`permissive`, `strict`, or `disabled`). When `mTLSMode` changes on the AgentRuntime CR, the annotation value changes, causing Kubernetes to trigger a rolling restart. This replaces the previous ConfigMap-based approach — the webhook reads `mTLSMode` from the AgentRuntime CR at pod CREATE time and sets `MTLS_MODE` env var on the authbridge container. Update the webhook in `internal/webhook/injector/pod_mutator.go` to set this env var.
- [ ] T006 Add `MTLSReady` condition logic to the reconcile loop in `internal/controller/agentruntime_controller.go`. Insert after target resolution (around line 170) and before the Ready condition (line 251). Logic: if `mTLSMode` resolves to `disabled` or empty-before-default → `MTLSReady=True/MTLSDisabled`; if `permissive` or `strict` → check whether the workload's pod template has spiffe-helper volume mounts or the SPIRE agent socket mount → if present `MTLSReady=True/SPIREAvailable`, if absent `MTLSReady=False/SPIREUnavailable` with message `"mTLS requires SPIRE; either deploy SPIRE or set mTLSMode: disabled"`. Use `r.setCondition()` (existing helper). Follow save/restore pattern around Patch calls (Constitution I).

**Checkpoint**: MTLSReady condition and mTLS annotation ready.

---

## Phase 3: User Story 1 — Agent-to-Agent mTLS by Default (Priority: P1) MVP

**Goal**: Agents deployed with SPIRE communicate over mTLS automatically without explicit mTLSMode configuration because mTLSMode defaults to permissive.

**Independent Test**: Deploy two agent workloads with SPIRE, create AgentRuntimes with no explicit mTLSMode, verify the pod template has `rossoctl.io/mtls-mode: permissive` annotation, authbridge sidecar has `MTLS_MODE=permissive` env var, and `MTLSReady=True`.

### Tests for User Story 1

- [ ] T007 [P] [US1] Add unit tests for `rossoctl.io/mtls-mode` annotation in `internal/controller/agentruntime_controller_test.go`. Test cases: (a) mTLSMode unset (defaults to permissive via kubebuilder marker) → pod template annotation is `rossoctl.io/mtls-mode: permissive`; (b) mTLSMode `strict` → annotation is `strict`; (c) mTLSMode `disabled` → annotation is `disabled`. Also test webhook: verify `MTLS_MODE` env var is set on authbridge container. Create objects in envtest and read back from API server (Constitution II).
- [ ] T008 [P] [US1] Add unit tests for `MTLSReady` condition in `internal/controller/agentruntime_controller_test.go`. Test cases: (a) mTLSMode permissive + spiffe-helper present → `MTLSReady=True/SPIREAvailable`; (b) mTLSMode permissive + no spiffe-helper → `MTLSReady=False/SPIREUnavailable`; (c) mTLSMode disabled → `MTLSReady=True/MTLSDisabled`. Read back AgentRuntime from envtest API server (Constitution II).
- [ ] T009 [P] [US1] Add unit test for annotation change on mTLSMode transition in `internal/controller/agentruntime_controller_test.go`. Verify that changing mTLSMode from `permissive` to `strict` or `disabled` results in a different `rossoctl.io/mtls-mode` annotation on the workload pod template, triggering a rolling restart. Note: mTLSMode is NOT in the config hash (per PR #405) — the annotation is the restart mechanism.

### Implementation for User Story 1

- [ ] T010 [US1] Wire T005 and T006 into the reconcile flow end-to-end. Create an AgentRuntime with no explicit mTLSMode, reconcile, verify: (a) the resolved mTLSMode is `permissive`; (b) the pod template has `rossoctl.io/mtls-mode: permissive` annotation; (c) `MTLSReady` condition is set; (d) webhook sets `MTLS_MODE` env var on authbridge container.
- [ ] T011 [US1] Verify that changing `mTLSMode` on an existing AgentRuntime triggers a workload rolling restart via the `rossoctl.io/mtls-mode` annotation change on the pod template (NOT via config hash — mTLSMode is excluded from the hash per PR #405).

**Checkpoint**: Agent-to-agent mTLS defaults to permissive. Annotation and conditions correct.

---

## Phase 4: User Story 2 — Controller-to-Agent Communication over mTLS (Priority: P1)

**Goal**: The operator controller uses SpiffeFetcher by default when SPIRE is available. Transport security metadata is recorded in AgentRuntime.status.card.

**Independent Test**: Deploy an agent with SPIRE, create an AgentRuntime, verify `status.card.transportSecurity` is `mtls`.

**Note**: The `fetchCard()` method (line 863) already implements the mTLS-first, HTTP-fallback logic. The `AuthenticatedFetcher` is already wired. The main change is making it the default via T003 (`--enable-verified-fetch=true`).

### Tests for User Story 2

- [ ] T012 [P] [US2] Add unit tests in `internal/controller/agentruntime_controller_test.go` verifying: (a) when `AuthenticatedFetcher` is set and TLS port exists → `status.card.transportSecurity` is `mtls` and `attestedAgentSpiffeID` is populated; (b) when `AuthenticatedFetcher` is nil → `status.card.transportSecurity` is `http`; (c) when TLS port is missing, falls back to HTTP with a `FallbackToHTTP` event. Use stub fetchers. Read back from envtest (Constitution II).
- [ ] T013 [P] [US2] Add unit test verifying `--enable-verified-fetch=false` (kill switch) results in `AuthenticatedFetcher` being nil even when SPIRE is configured. Test at the `cmd/main.go` wiring level or at the reconciler level with `EnableVerifiedFetch=false`.

### Implementation for User Story 2

- [x] T014 [DONE] [US2] The `fetchCard()` method already wires SpiffeFetcher as the preferred fetcher, falls back to HTTP, and populates `transportSecurity` and `attestedAgentSpiffeID`. No new code needed — T003 (flag default change) activates this path.

**Checkpoint**: Controller fetches over mTLS by default. Status metadata populated.

---

## Phase 5: User Story 3 — Clear Error When SPIRE Is Unavailable (Priority: P1)

**Goal**: Operators without SPIRE see a clear MTLSReady=False condition with actionable guidance.

**Independent Test**: Create an AgentRuntime in a cluster without SPIRE, verify MTLSReady=False/SPIREUnavailable.

### Tests for User Story 3

- [ ] T015 [P] [US3] Add unit test in `internal/controller/agentruntime_controller_test.go` for the SPIRE-unavailable case: (a) workload with no spiffe-helper volume → `MTLSReady=False/SPIREUnavailable` with message containing `"mTLS requires SPIRE"`; (b) verify `Ready` condition reflects the MTLSReady failure. Read back from envtest (Constitution II).

### Implementation for User Story 3

- [ ] T016 [US3] Verify T006's MTLSReady condition works end-to-end. `MTLSReady=False` does NOT block `Ready=True` — the workload is still functional without mTLS. Instead, emit a Kubernetes Warning Event on the AgentRuntime so `kubectl describe` surfaces the issue. Also expand SPIRE detection to check for CSI driver volumes (`csi.spiffe.io`) alongside spiffe-helper.

**Checkpoint**: SPIRE-unavailable produces clear, actionable conditions.

---

## Phase 6: User Story 4 — JWS Signing Pipeline Deprecation Warning (Priority: P2)

**Goal**: Operators using legacy signing flags see deprecation warnings.

**Independent Test**: Start operator with `--require-a2a-signature=true`, verify deprecation log.

### Tests for User Story 4

- [ ] T017 [P] [US4] Add test verifying deprecation warning is logged when legacy flags are set to `true`. This can be a simple test that parses log output or validates the warning logic function.

### Implementation for User Story 4

- [ ] T018 [US4] Verify T004's deprecation warnings work. Ensure all three flags emit warnings. Note: the flags already default to `false` on main — only the warning message is new.

**Checkpoint**: Deprecation warnings active.

---

## Phase 7: Authbridge Verification (cortex)

**Purpose**: Verify authbridge mTLS is complete and matches operator expectations

- [x] T019 [DONE] Envoy mTLS wiring confirmed — webhook injector has `MTLSEnabled`, `MTLSPermissive`, `MTLSStrict` template fields driving envoy TLS contexts.
- [ ] T020 [P] Verify the `cfg.MTLS` config schema in `authbridge/authlib/config/config.go` matches what the operator generates. Specifically: does the authbridge expect `mtls:\n  mode: permissive` or a different shape? Clone `cortex` and check.
- [ ] T021 Add mTLS integration tests (if not already present in authbridge). Test: (a) permissive accepts TLS + plaintext; (b) strict rejects plaintext; (c) cert rotation.

**Checkpoint**: Authbridge config contract verified.

---

## Phase 8: Polish & Cross-Cutting Concerns

- [ ] T022 [P] Add e2e test in `test/e2e/` — deploy agents with SPIRE, create AgentRuntimes with default mTLSMode, verify ConfigMap, conditions, and card fetch transport.
- [ ] T023 [P] Update documentation (`GETTING_STARTED.md`) for mTLS-by-default behavior and opt-out via `mTLSMode: disabled`.
- [ ] T024 Run `make generate && make manifests && make test` — verify no regressions.

---

## Summary of Actual Work Needed

| Task | Status | Work Required |
|------|--------|---------------|
| T001 | NEW | Add kubebuilder default marker + MTLSReady condition constant |
| T002 | NEW | make generate && make manifests |
| T003 | NEW | Flip two flag defaults to true |
| T004 | NEW | Add deprecation warning logs (flag defaults already false) |
| T005 | REPLACED | Set `rossoctl.io/mtls-mode` annotation on pod template + webhook sets `MTLS_MODE` env var |
| T006 | NEW | MTLSReady condition logic in reconcile loop |
| T007-T009 | NEW | Unit tests for annotation, condition, annotation change |
| T010-T011 | NEW | End-to-end wiring verification |
| T012-T013 | NEW | Unit tests for SpiffeFetcher default |
| T014 | DONE | fetchCard() already implements mTLS-first logic |
| T015 | NEW | Unit test for SPIRE unavailable |
| T016 | NEW | MTLSReady condition + Warning Event (does not block Ready) |
| T017-T018 | NEW | Deprecation warning test + verification |
| T019 | DONE | Envoy mTLS confirmed in webhook injector |
| T020 | NEW | Verify authbridge config schema match |
| T021-T024 | NEW | Integration tests, e2e, docs, regression check |

**Net new implementation tasks**: 6 (T001, T003, T004, T005, T006, T016)
**Net new test tasks**: 8 (T007-T009, T012-T013, T015, T017, T022)
**Already done**: 2 (T014, T019)
**Verification/polish**: 6 (T002, T010-T011, T018, T020-T021, T023-T024)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — start immediately
- **Foundational (Phase 2)**: Depends on T001, T002
- **User Stories (Phase 3-6)**: All depend on Phase 2
  - US4 can start after T004 (independent of Phase 2)
- **Authbridge Verification (Phase 7)**: Independent — run in parallel
- **Polish (Phase 8)**: After all user stories

### Recommended Execution Order

1. T001 → T002 (CRD changes must come first)
2. T003 + T004 in parallel (flag changes)
3. T005 + T006 (foundational — ConfigMap + condition)
4. T007-T011 (US1 tests + wiring)
5. T012-T014 (US2 tests — T014 is already done, just verify)
6. T015-T016 (US3 tests + Ready condition gate)
7. T017-T018 (US4 deprecation)
8. T020-T024 (verification, e2e, docs)
