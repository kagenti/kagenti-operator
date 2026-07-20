# Contract: AgentRuntime status.card

## Overview

The `status.card` field on the AgentRuntime CRD exposes discovered A2A agent card data, fetch metadata, and identity verification results. This contract defines the shape and semantics of this new status field.

## CRD Status Extension

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
status:
  phase: Active
  configuredPods: 1
  conditions:
    - type: CardSynced
      status: "True"
      reason: CardSynced
      message: "Successfully fetched agent card for my-agent"
  card:
    # A2A card payload (from AgentCardData)
    name: "my-agent"
    description: "An example A2A agent"
    version: "1.0.0"
    url: "http://my-agent.default.svc.cluster.local:8000"
    skills:
      - id: "summarize"
        name: "Summarize"
        description: "Summarizes text input"
        tags: ["nlp", "text"]
    capabilities:
      streaming: true
      pushNotifications: false
    defaultInputModes: ["text/plain"]
    defaultOutputModes: ["text/plain"]
    
    # Fetch metadata
    fetchedAt: "2026-05-21T10:30:00Z"
    cardId: "a1b2c3d4e5f6..."
    protocol: "a2a"
    lastPodTemplateHash: "6b7c8d9e0f"
    
    # Verification fields (populated when mTLS is active)
    validSignature: true
    signatureKeyID: "key-001"
    signatureVerificationDetails: "JWS signature valid (x5c chain verified)"
    attestedAgentSpiffeID: "spiffe://example.org/ns/default/sa/my-agent"
```

## Condition Contract: CardSynced

Added to `status.conditions[]` alongside existing conditions (Ready, TargetResolved, ConfigResolved).

| Reason | Status | Trigger |
|--------|--------|---------|
| `CardSynced` | True | Successful fetch and parse |
| `FetchSkipped` | True | Pod template hash unchanged, existing data valid |
| `CardFetchFailed` | False | HTTP or mTLS connection error |
| `CardParseFailed` | False | Response is not valid A2A JSON |
| `ServiceNotFound` | False | No Service matches the Deployment selector |
| `WorkloadNotReady` | False | Target Deployment has zero ready Pods |
| `CardDiscoveryDisabled` | False | Feature flag is disabled |

## Feature Flag

```
--enable-card-discovery=false   # default: disabled
```

When disabled: `status.card` is nil, no `CardSynced` condition is set. If previously enabled and data exists, `status.card` is cleared on the next reconcile.

## Backward Compatibility

- No changes to `spec` fields on AgentRuntime
- No changes to existing `status` fields (phase, configuredPods, existing conditions)
- AgentCard CRD continues to function independently
- No API version bump required (additive status change only)
