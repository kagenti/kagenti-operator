# Review Guide: Card Discovery Refinement Alignment

**Generated**: 2026-05-27 | **Spec**: [spec.md](spec.md)

## Why This Change

PR #372 merged the card discovery feature (fetching A2A agent cards into AgentRuntime status). Live cluster testing exposed three API quality issues: (1) platform engineers cannot tell whether a card was fetched securely (mTLS) or over plain HTTP without reading operator logs, (2) the condition model has a naming stutter (`CardSynced True CardSynced` in kubectl output) and conflates unrelated failure modes, and (3) field names are misleading (`cardId` holds a content hash, not an identifier). Since the PR just merged and no external consumers exist yet, we have a narrow window to fix these before the API surface becomes locked.

## What Changes

Six breaking changes to the AgentRuntime CRD status fields:

1. New `transportSecurity` field on `CardStatus` records whether the card was fetched over mTLS or plain HTTP
2. Condition type renamed from `CardSynced` to `CardFetched` with transport-aware reasons (`Fetched` vs `FetchedInsecure`)
3. `cardId` field renamed to `cardHash` (it holds a SHA-256 content hash, not an identifier)
4. `fetchedAt` field renamed to `lastCardFetchTime` (clarifies this is the controller's fetch timestamp, not the card's creation time)
5. Port resolution now checks `rossoctl.io/port` annotation, then port named `a2a`, before falling back to `http`/first port
6. New `WorkloadNotReady` condition reason split from `ServiceNotFound`

All changes are backward-incompatible CRD field/condition renames. No migration needed since there are no consumers yet.

## How It Works

The changes modify four layers:

**Types** (`api/v1alpha1/agentruntime_types.go`): Rename `CardID` to `CardHash`, `FetchedAt` to `LastCardFetchTime`, add `TransportSecurity string` field. Update printer column marker from `CardSynced` to `CardFetched`.

**Controller** (`internal/controller/agentruntime_controller.go`): Rename `ConditionTypeCardSynced` constant to `ConditionTypeCardFetched`. In `fetchCard()`, determine transport security from the fetch path (authenticated fetcher returns `"mTLS"`, default fetcher returns `"plainHTTP"`). In `fetchAndUpdateCard()`, set transport-aware condition reasons and the new `TransportSecurity` field. Add `checkWorkloadReady()` helper before service resolution. Replace `serviceHTTPPort()` body with the annotation-first resolution chain.

**CRD** (auto-generated from types via `make manifests`): Schema reflects new field names and printer column.

**Tests** (`internal/controller/agentruntime_controller_test.go`): Migrate all existing card test assertions to new names. Add new tests for transport security, workload readiness, and port resolution annotation.

## When It Applies

**Applies when**:
- Card discovery is enabled (`--enable-card-discovery` flag)
- An AgentRuntime targets a workload (Deployment, StatefulSet, or Sandbox) that serves `/.well-known/agent-card.json`
- The platform engineer reads `status.card` or the `CardFetched` condition via kubectl, automation, or UI

**Does not apply when**:
- Card discovery is disabled (existing behavior unchanged)
- Identity binding enforcement (deferred to the identity binding migration)
- AgentCard CRD deprecation or removal (separate epic)
- Cross-cluster or multi-cluster card discovery (out of scope)

## Key Decisions

1. **`transportSecurity` as a field, not just a condition reason.** Condition reasons are transient (lost on the next status transition). A field persists with the card data, letting consumers (UI, policy engines) query it directly. The condition reason also reflects transport for `kubectl describe` visibility.

2. **Condition model where every reason maps to one diagnostic action.** `CardFetched` as the type (past participle, like Kubernetes' `Scheduled` or `Initialized`) instead of `CardSynced` (implies polling, which we don't do). Transport-aware reasons (`Fetched` vs `FetchedInsecure`) for security visibility. `WorkloadNotReady` split from `ServiceNotFound` because they require different operator responses (wait vs check config). `FetchSkipped` and `DiscoveryDisabled` retained from PR #372 for feature flag and no-op cases.

3. **Port resolution with annotation escape hatch.** The `rossoctl.io/port` annotation on Services handles multi-port edge cases where auto-detection picks the wrong port. The `a2a` port name takes priority over generic `http` because we're specifically fetching A2A protocol cards.

4. **`cardHash` over `cardId`.** The field holds a SHA-256 content hash for change detection, not a unique identifier. `cardId` suggests a stable ID (like a UUID); `cardHash` accurately conveys "content fingerprint that changes when the card changes."

5. **Workload readiness check before service resolution.** Distinguishes "pods aren't ready yet" (transient, wait) from "no matching Service" (configuration issue, fix it). Different diagnostic actions for the operator.

## Areas Needing Attention

- **Breaking changes**: All field renames and the condition type rename are CRD-breaking. This is acceptable because PR #372 just merged with no external consumers yet. If any downstream tooling has already started parsing `cardId` or `CardSynced`, this will break it.

- **ConfigMap fetch path**: The `transportSecurity` value for the ConfigMap path (signed card from init-container) is `"configMap"`. This path is being deprecated but still exists during coexistence. Reviewers should assess whether this value is appropriate or whether `"signed"` better describes the semantics.

- **Constitution update**: The project constitution (`.specify/memory/constitution.md`) references `CardSynced` from the original bug fix. It needs to be updated to `CardFetched` as part of this change (FR-009).

## Open Questions

1. Should `transportSecurity` values be documented as a formal enum in the CRD description, or left as free-form strings for extensibility (e.g., future `"ztunnel"` value)?
2. Are there any downstream consumers already parsing `cardId` or `CardSynced` that we're not aware of? If so, the breaking window may be shorter than assumed.

## Review Checklist

- [ ] Key decisions are justified
- [ ] Breaking changes are documented with migration guidance
- [ ] Scope matches the stated boundaries
- [ ] Success criteria are achievable
- [ ] No unstated assumptions
- [ ] Condition reasons are exhaustive (every code path sets a reason)
- [ ] `transportSecurity` field is set on every successful fetch path
- [ ] Port resolution chain matches FR-006 (annotation > a2a > http > first > default)
- [ ] Old field names (`cardId`, `fetchedAt`, `CardSynced`) are fully removed from CRD schema
- [ ] Constitution updated to reflect new condition type
