# Data Model: Consolidate AgentCard Data Into AgentRuntime Status

## Entity Changes

### New: CardStatus (AgentRuntime status.card)

Added to `AgentRuntimeStatus` as an optional field.

**Fields**:

| Field | Type | Description | Source |
|-------|------|-------------|--------|
| `name` | string | Agent name from A2A card | AgentCardData (embedded) |
| `description` | string | Agent description | AgentCardData (embedded) |
| `version` | string | Agent version | AgentCardData (embedded) |
| `url` | string | Agent endpoint URL | AgentCardData (embedded) |
| `documentationUrl` | string | Documentation URL | AgentCardData (embedded) |
| `iconUrl` | string | Agent icon URL | AgentCardData (embedded) |
| `provider` | AgentProvider | Service provider info | AgentCardData (embedded) |
| `capabilities` | AgentCapabilities | A2A capability set | AgentCardData (embedded) |
| `defaultInputModes` | []string | Supported input media types | AgentCardData (embedded) |
| `defaultOutputModes` | []string | Supported output media types | AgentCardData (embedded) |
| `skills` | []AgentSkill | Agent skills list | AgentCardData (embedded) |
| `supportsAuthenticatedExtendedCard` | *bool | Extended card support | AgentCardData (embedded) |
| `signatures` | []AgentCardSignature | JWS signatures | AgentCardData (embedded) |
| `fetchedAt` | *metav1.Time | Last successful fetch timestamp | New field |
| `cardId` | string | SHA-256 hash of card content | New field |
| `protocol` | string | Detected agent protocol (e.g., "a2a") | New field |
| `validSignature` | *bool | JWS signature validation result | New field |
| `signatureKeyID` | string | Key ID from verified JWS header | New field |
| `signatureVerificationDetails` | string | Verification details/error message | New field |
| `attestedAgentSpiffeID` | string | SPIFFE ID from mTLS peer certificate | New field |

**Note**: Change-detection hash (`lastPodTemplateHash`) is stored as an annotation (`agent.rossoctl.dev/last-card-fetch-hash`) on the AgentRuntime, not in the CRD status. This keeps the implementation mechanism out of the public API surface.

### Modified: AgentRuntimeStatus

| Field | Change | Before | After |
|-------|--------|--------|-------|
| `card` | Added | (not present) | `*CardStatus` (optional) |

### Modified: AgentRuntimeReconciler

| Field | Change | Description |
|-------|--------|-------------|
| `AgentFetcher` | Added | `agentcard.Fetcher` interface (plain HTTP) |
| `AuthenticatedFetcher` | Added | `agentcard.AuthenticatedFetcher` interface (mTLS) |
| `SignatureProvider` | Added | `signature.Provider` interface (JWS verification) |
| `EnableCardDiscovery` | Added | Feature flag (bool) |
| `SpireTrustDomain` | Added | SPIFFE trust domain string |

### New Condition: CardSynced

Added to AgentRuntime `status.conditions[]`.

| Reason | Status | When |
|--------|--------|------|
| `CardSynced` | True | Card fetched and parsed successfully |
| `CardFetchFailed` | False | HTTP/mTLS fetch error |
| `CardParseFailed` | False | JSON parse error |
| `ServiceNotFound` | False | No Service matches the workload's selector |
| `WorkloadNotReady` | False | Workload has zero ready Pods |
| `FetchTimeout` | False | Card fetch exceeded 10-second timeout |
| `CardDiscoveryDisabled` | False | Feature flag is off (clears stale data) |
| `FetchSkipped` | True | Pod template hash unchanged, card data still valid |

## Relationships

```
AgentRuntime (1) ---> (0..1) CardStatus (status.card)
                              |
                              +--> AgentCardData (embedded, reused from api/v1alpha1)
                              +--> Fetch metadata (fetchedAt, cardId, protocol)
                              +--> Verification fields (validSignature, attestedAgentSpiffeID, ...)

AgentRuntime.spec.targetRef ---> Deployment/StatefulSet/Sandbox
                                    |
                                    +--> Pod selector labels ---> Service (selector match)
                                                                    |
                                                                    +--> /.well-known/agent-card.json
```

## State Transitions for status.card

```
[Empty] -- feature flag enabled + reconcile --> [Fetching]
[Fetching] -- success --> [Populated] (card data + metadata + verification)
[Fetching] -- failure --> [Empty or Stale] (retain last good data, CardSynced=False)
[Populated] -- pod template hash change --> [Fetching] (re-fetch)
[Populated] -- feature flag disabled --> [Empty] (cleared on next reconcile)
[Populated] -- workload deleted --> [Stale] (AgentRuntime still exists, card data stale)
```
