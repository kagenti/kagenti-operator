# Brainstorm: AgentCard Data Into AgentRuntime Status

**Date:** 2026-05-20
**Status:** active

## Problem Framing

AgentRuntime and AgentCard are two CRDs with identical cardinality (one per workload), the same namespace, the same lifecycle, and the same owner. AgentCard is a pure read-only CR whose content is entirely controller-managed. Its data (card metadata, verification status, binding state) fits naturally into AgentRuntime's `status` section.

The AgentCard CRD has three specific problems documented in ADR ODH-ADR-AgentOps-0002:

1. It conflates observation with policy. The CR is controller-created and controller-written, leaving no room for admin-authored policy fields.
2. Its JWS signing pipeline signs a skeleton card with empty skills/capabilities (#292). The signed content and live content are disconnected.
3. Maintaining two CRs for the same Deployment doubles the RBAC surface and splits "how this agent participates in the platform" across two APIs.

This brainstorm covers the first step: moving card data into AgentRuntime status. mTLS, policy fields, and AgentCard removal are separate follow-up items.

## Context

- ADR: ODH-ADR-AgentOps-0002 (Agent Network Policy and mTLS Identity)
- Upstream issue: rossoctl-operator#371 (Consolidate AgentCard into AgentRuntime status)
- Related: rossoctl-operator#292 (skeleton-card problem)
- Related: rossoctl-operator#284 (mTLS verified fetch, merged 2026-05-20, infrastructure reusable later)
- Upstream sync: IBM maintainers agreed to AgentCard deprecation path (2026-05-15)
- RHAISTRAT-1599 AC review: acceptance criteria updated to reflect AgentRuntime-based discovery

## Approaches Considered

### A: Extend the existing AgentRuntime controller (chosen)

Add a card fetch phase to the existing `agentruntime_controller.go` reconciliation loop. After resolving the target workload and applying labels, the controller fetches `/.well-known/agent-card.json` from the agent's Service endpoint over plain HTTP, parses it into an A2A-compliant struct, and writes it to `status.card`. Triggered by Pod template hash change on the target workload.

- Pros: Minimal new code. Reuses existing workload watches and reconciliation infrastructure. Single controller, single reconcile loop.
- Cons: Makes agentruntime_controller.go larger (already 29K). Card fetch adds network I/O to a controller that currently only does Kubernetes API calls.

### B: New dedicated card discovery controller

Create a separate `agentruntime_card_controller.go` that watches AgentRuntime CRs and handles only card fetching. The main controller continues handling labels, sidecar injection, and config.

- Pros: Clean separation. Card fetch failures don't block main reconciliation. Independent rate limiter for network I/O.
- Cons: Two controllers watching the same CRD. Need to coordinate status updates. More moving parts for a simple HTTP GET.

### C: Card fetch as a Kubernetes Job

Create short-lived Jobs to fetch cards on rollout events.

- Pros: Completely decouples fetching from the controller. Reusable binary.
- Cons: Heavy for a simple HTTP GET. Adds Job RBAC, cleanup, failure handling. Overkill.

## Decision

**Approach A: Extend the existing AgentRuntime controller.** The card fetch is a single HTTP GET that takes milliseconds. If performance becomes an issue at scale (hundreds of agents), extracting to a separate controller (Approach B) is a clean refactor.

## Key Requirements

### What gets built

- New `AgentCardStatus` struct on AgentRuntime CRD, modeled on the A2A protocol agent-card.json spec (name, description, skills, protocols, endpoint). Not mirrored from the existing AgentCard CRD fields.
- Card fetch phase added to the existing AgentRuntime controller reconcile loop.
- Fetch triggers on Pod template hash change (rollout events only, no polling, no periodic fallback).
- Fetch is plain HTTP GET to `/.well-known/agent-card.json` via the agent's Service endpoint.
- Card data written to `status.card` on AgentRuntime.

### What gets deprecated

- AgentCard CRD gets a deprecation log warning on creation.
- AgentCard remains functional (both CRDs coexist during transition).

### Out of scope (future iterations)

- mTLS for the card fetch (port from #284).
- `spec.policy` fields (allowedIngressNamespaces, dependencies, externalEgress).
- AgentCard CRD removal and controller cleanup.
- ValidatingAdmissionPolicy for label restriction.
- Migration tooling.

## Open Questions

- Exact A2A AgentCard JSON schema fields to model in the Go struct
- How to discover the agent's Service endpoint from the AgentRuntime targetRef (resolve Deployment -> Service via selector matching or naming convention)
- Feature flag name and default (e.g. `--enable-card-discovery`, off by default)
- Whether `status.card.fetchedAt` timestamp should be included for diagnostics
