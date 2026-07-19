<!--
Sync Impact Report
- Version change: 0.0.0 → 1.0.0
- Added principles:
  - I. Reconciler Status Integrity
  - II. Spec-Anchored Testing
  - III. Controller-Runtime Safety
  - IV. CRD-First Design
  - V. Feature-Gated Rollout
- Added sections:
  - Controller-Runtime Gotchas
  - Quality Gates
  - Governance
- Templates requiring updates:
  - .specify/templates/plan-template.md: ✅ no changes needed (Constitution Check section exists)
  - .specify/templates/spec-template.md: ✅ no changes needed (acceptance scenario format compatible)
  - .specify/templates/tasks-template.md: ✅ no changes needed (task format compatible)
- Follow-up TODOs: none
-->

# Rossoctl Operator Constitution

## Core Principles

### I. Reconciler Status Integrity

In-memory status mutations MUST survive all API server interactions within
a single reconcile cycle. Any call that refreshes the reconciled object
from the API server (Patch, Update on the main resource) MUST save and
restore in-memory status to prevent silent data loss.

Rationale: `client.Patch()` and `client.Update()` replace the local object
with the API server response, wiping unpersisted in-memory status changes.
This caused a production bug where `status.card` and all conditions
disappeared despite successful card fetches. The bug passed code review
and 180 unit tests because the Patch call failed silently in test
environments where the object didn't exist in the API server.

### II. Spec-Anchored Testing

Tests MUST verify outcomes using the same method the spec's acceptance
scenario describes. If the spec says "confirm via `kubectl get`", the test
MUST read the object back from the API server (envtest), not inspect
in-memory state. Tests that only check in-memory state after a function
call may pass when API server interactions fail silently, hiding bugs that
only manifest in production.

Rationale: The card discovery bug was invisible to tests because
`r.Patch()` returned NotFound (object not in envtest), the error was
logged but not returned, and the in-memory state appeared correct. A test
that read back from the API server would have caught the discrepancy
immediately.

### III. Controller-Runtime Safety

All reconciler code MUST follow these controller-runtime rules:

- Never call `r.Patch()` or `r.Update()` on the main resource between
  in-memory status mutations and `Status().Update()`. If unavoidable,
  save and restore `rt.Status` across the call.
- `Status().Update()` only persists the status subresource. Metadata
  changes (labels, annotations) require a separate Patch on the main
  resource.
- When mixing metadata patches and status updates, be aware that the
  metadata patch refreshes the object and invalidates in-memory status.
- HTTP fetches to in-cluster Services during reconciliation MUST have
  timeouts (10s default) to prevent blocking the work queue.

Rationale: These are controller-runtime framework behaviors that are not
obvious from the API surface. They have caused production bugs in this
project and are documented here to prevent recurrence.

### IV. CRD-First Design

CRD schemas MUST be the source of truth for the operator's data model.
Status fields MUST use concrete types with explicit JSON tags, not
unstructured maps. All status fields MUST be validated against the
deployed CRD schema, not just against Go compilation.

Rationale: A Kubernetes operator's contract is its CRD. Schema mismatches
between code and CRD silently drop fields during API server round-trips.

### V. Feature-Gated Rollout

New controller behaviors that modify workload state or add API server
interactions MUST be gated behind a CLI flag (disabled by default). The
flag MUST be documented in the Helm chart values. Existing behavior MUST
NOT change when the flag is disabled.

Rationale: Operators run in shared clusters. Ungated behavior changes
risk disrupting workloads during upgrades. Feature flags allow gradual
rollout and easy rollback.

## Controller-Runtime Gotchas

This section documents framework-specific behaviors that are not obvious
from the API surface and have caused bugs in this project. Review agents
and developers MUST consult this section when reviewing reconciler code.

1. **Patch/Update refreshes the object**: `client.Patch(ctx, obj, patch)`
   and `client.Update(ctx, obj)` replace `obj` with the API server
   response. Any in-memory mutations not yet persisted are lost.

2. **Status is a separate subresource**: `Status().Update()` only writes
   status fields. Metadata (annotations, labels) requires a separate
   main-resource Patch.

3. **Single worker queue**: By default, controller-runtime uses one
   worker per controller. A blocking HTTP call (e.g., card fetch with no
   timeout) blocks all reconciliation for that controller.

4. **envtest vs production divergence**: Operations that fail silently in
   envtest (e.g., Patch on a non-existent object) may succeed in
   production with different side effects. Tests MUST create objects in
   envtest before exercising code paths that interact with the API server.

## Quality Gates

- All reconciler tests MUST create the reconciled object in envtest and
  read it back after the operation under test (Principle II).
- Deep review findings that trigger auto-fixes MUST be followed by a
  regression check: does the fix preserve all previously-passing behavior?
- Card discovery and other HTTP-dependent features MUST be tested with
  stub fetchers that return controlled data, AND with envtest objects
  that trigger the full Patch/Status().Update cycle.

## Governance

This constitution governs all development on the rossoctl-operator. It
supersedes informal conventions and ad-hoc patterns.

**Amendment process**: Propose changes via PR. Changes to principles
require a rationale with a concrete example (bug, incident, or design
decision) that motivated the change. Version increments follow semver:
MAJOR for principle removals or redefinitions, MINOR for new principles
or sections, PATCH for clarifications.

**Compliance**: All PRs and code reviews MUST verify compliance with
these principles. The deep review agents receive this constitution as
context and MUST flag violations.

**Version**: 1.0.0 | **Ratified**: 2026-05-22 | **Last Amended**: 2026-05-22
