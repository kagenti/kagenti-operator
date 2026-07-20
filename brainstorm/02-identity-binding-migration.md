# Brainstorm: Identity Binding Migration from AgentCard to AgentRuntime

**Date:** 2026-05-21
**Status:** active
**Triggered by:** PR #372 review feedback (pdettori)

## Problem Framing

AgentCard's `spec.identityBinding` is the only admin-authored policy field on the AgentCard CRD. It controls two things:

1. **Trust domain scoping** (`trustDomain`): overrides the operator-level `--spire-trust-domain` for a specific agent workload. The controller checks whether the agent's SPIFFE ID (from JWS signature or mTLS peer cert) belongs to this trust domain.

2. **Strict enforcement** (`strict`): when true, a binding failure removes the `signature-verified` label from the workload, triggering the NetworkPolicy controller to apply a restrictive policy that isolates the agent.

Identity binding is orthogonal to card discovery. The card fetch is the mechanism that surfaces the SPIFFE ID, but the binding evaluation and enforcement are about the workload's identity posture, not the card content. Moving card data into AgentRuntime status does not affect binding behavior.

## Current State

```
AgentCard.spec.identityBinding
├── trustDomain: string (per-agent override of --spire-trust-domain)
└── strict: bool (default: false)
    ├── false → binding results recorded in status only, no enforcement
    └── true → binding failure removes signature-verified label
                → NetworkPolicy controller applies restrictive policy
```

AgentRuntime already has a related field:

```
AgentRuntime.spec.identity.spiffe.trustDomain
```

This field serves a different purpose today (configuring the workload's own SVID trust domain for injection), but the naming and placement overlap with identity binding's trust domain scoping.

## Migration Path

### Phase 1: Card data into AgentRuntime status (this PR, #372)

- `status.card` surfaces card data, fetch metadata, and verification results
- Identity binding stays on AgentCard
- AgentCard controller continues all enforcement (label propagation, NetworkPolicy)
- No behavior changes for existing identity binding users

### Phase 2: Identity binding into AgentRuntime spec (future PR)

Move `identityBinding` to AgentRuntime.spec, alongside the existing identity fields:

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
spec:
  identity:
    spiffe:
      trustDomain: example.org    # existing: workload SVID trust domain
    binding:                       # new: migrated from AgentCard
      trustDomain: example.org     # override for binding evaluation
      strict: false                # enforcement toggle
```

Open question: should `identity.spiffe.trustDomain` and `identity.binding.trustDomain` be unified? They serve related but different purposes (SVID injection vs binding evaluation). If unified, a single `trustDomain` at the `identity` level could serve both.

### Phase 3: Enforcement migration (future PR)

Move label propagation logic (`signature-verified` label) from AgentCard controller to AgentRuntime controller. The AgentRuntime controller already manages workload labels and annotations, so this is a natural fit.

### Phase 4: AgentCard CRD removal

Once identity binding and enforcement have migrated:
- Remove AgentCard CRD
- Remove AgentCardSyncReconciler (auto-creates AgentCards for labelled workloads)
- Remove AgentCard controller
- Clean up RBAC, webhooks, and test fixtures

## Design Considerations

### Enforcement during coexistence

During the transition (Phases 1-2), enforcement continues via the AgentCard controller. Both CRDs coexist. Operators who use identity binding today see no behavior change.

After Phase 2, the AgentRuntime controller evaluates binding from its own spec fields. The AgentCard controller's binding logic becomes dead code and is removed in Phase 4.

### AgentCardSyncReconciler

The sync controller auto-creates AgentCards for workloads with `rossoctl.io/type=agent` labels. During coexistence, it continues to function. After Phase 2, the sync controller is no longer needed because:
- Card discovery is handled by AgentRuntime controller (Phase 1)
- Identity binding is on AgentRuntime spec (Phase 2)
- There is no remaining reason to auto-create AgentCards

### Trust domain field unification

Three options:

| Option | Structure | Pros | Cons |
|--------|-----------|------|------|
| A: Separate fields | `identity.spiffe.trustDomain` + `identity.binding.trustDomain` | Clear separation of concerns | Confusing duplication |
| B: Unified field | `identity.trustDomain` (serves both) | Simple, one source of truth | Loses granularity if they diverge |
| C: Binding inherits | `identity.binding.trustDomain` defaults to `identity.spiffe.trustDomain` if unset | Best of both, explicit override | Slightly more complex defaulting |

Option C seems best: the common case is a single trust domain, but the override is available when needed.

## Open Questions

- Should binding evaluation in the AgentRuntime controller use the verification data already in `status.card` (from Phase 1), or should it re-evaluate independently?
- Is there a need for namespace-level binding policy (via ConfigMap), analogous to `authbridge-runtime-config`?
- Should the `AgentCardSyncReconciler` get its own deprecation warning before removal?
