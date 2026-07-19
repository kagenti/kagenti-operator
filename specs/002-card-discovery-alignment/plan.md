# Implementation Plan: Card Discovery Refinement Alignment

**Branch**: `002-card-discovery-alignment` | **Date**: 2026-05-27 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `specs/002-card-discovery-alignment/spec.md`

## Summary

Improve the card discovery API surface (merged in PR #372) based on findings from live cluster testing and API review: add transport security visibility, fix misleading field names, align condition model with Kubernetes conventions, and add protocol-aware port resolution. Six changes: add `transportSecurity` field, rename condition type `CardSynced` to `CardFetched` with transport-aware reasons, rename `cardId` to `cardHash` and `fetchedAt` to `lastCardFetchTime`, implement protocol-aware port resolution with annotation override, and add workload readiness check. All changes are breaking but acceptable since the API has no external consumers yet.

## Technical Context

**Language/Version**: Go 1.26
**Primary Dependencies**: controller-runtime v0.23.3, k8s.io/api, k8s.io/apimachinery
**Storage**: Kubernetes etcd (via CRD status subresource)
**Testing**: Ginkgo/Gomega with controller-runtime envtest
**Target Platform**: Kubernetes 1.35+
**Project Type**: Kubernetes operator (kubebuilder scaffold)
**Performance Goals**: N/A (refactoring, no new reconcile overhead)
**Constraints**: Must not regress existing card discovery behavior; all 181 tests must pass
**Scale/Scope**: 6 files modified, ~200 lines changed

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Notes |
|-----------|--------|-------|
| I. Reconciler Status Integrity | PASS | `persistCardFetchAnnotation` save/restore pattern preserved |
| II. Spec-Anchored Testing | PASS | Tests will verify via envtest API server read-back |
| III. Controller-Runtime Safety | PASS | No new Patch/Update calls between status mutations and Status().Update |
| IV. CRD-First Design | PASS | All field renames reflected in both Go types and CRD YAML |
| V. Feature-Gated Rollout | PASS | No new feature flags; changes are within existing `--enable-card-discovery` gate |

## Project Structure

### Documentation (this feature)

```text
specs/002-card-discovery-alignment/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
└── tasks.md             # Phase 2 output (via /speckit-tasks)
```

### Source Code (files to modify)

```text
operator/
├── api/v1alpha1/
│   ├── agentruntime_types.go          # CardStatus struct (field renames + new field)
│   └── zz_generated.deepcopy.go       # Auto-generated
├── cmd/
│   └── main.go                        # No changes needed (fetcher wiring unchanged)
├── config/
│   ├── crd/bases/
│   │   └── agent.rossoctl.dev_agentruntimes.yaml  # CRD schema
│   └── rbac/
│       └── role.yaml                  # No changes needed
├── charts/operator/
│   └── crds/
│       └── agent.rossoctl.dev_agentruntimes.yaml  # Helm chart CRD
├── internal/controller/
│   ├── agentruntime_controller.go     # Condition constants, fetchAndUpdateCard, serviceHTTPPort
│   └── agentruntime_controller_test.go # All card-related tests
└── .specify/memory/
    └── constitution.md                # Update condition references
```

**Structure Decision**: Modifying existing files only. No new files or directories needed.

## Complexity Tracking

No constitution violations. No complexity justifications needed.
