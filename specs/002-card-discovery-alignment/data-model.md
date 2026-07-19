# Data Model: Card Discovery Refinement Alignment

## CardStatus (modified)

```go
type CardStatus struct {
    AgentCardData `json:",inline"`

    // LastCardFetchTime is the timestamp of the last successful card fetch.
    // Renamed from FetchedAt for clarity: this records when the controller
    // fetched the card, not when the card was created by the agent.
    // +optional
    LastCardFetchTime *metav1.Time `json:"lastCardFetchTime,omitempty"`

    // CardHash is a SHA-256 content hash of the fetched card data.
    // Used for change detection across rollouts.
    // Renamed from CardID to accurately describe its purpose.
    // +optional
    CardHash string `json:"cardHash,omitempty"`

    // Protocol is the detected agent protocol (e.g., "a2a").
    // +optional
    Protocol string `json:"protocol,omitempty"`

    // TransportSecurity indicates the transport layer used for the card fetch.
    // Values: "mTLS" (SPIFFE-verified), "plainHTTP" (no transport identity).
    // +optional
    TransportSecurity string `json:"transportSecurity,omitempty"`

    // ValidSignature is the result of JWS signature verification.
    // +optional
    ValidSignature *bool `json:"validSignature,omitempty"`

    // SignatureKeyID is the key ID from the verified JWS header.
    // +optional
    SignatureKeyID string `json:"signatureKeyID,omitempty"`

    // SignatureVerificationDetails contains details or errors from signature verification.
    // +optional
    SignatureVerificationDetails string `json:"signatureVerificationDetails,omitempty"`

    // AttestedAgentSpiffeID is the SPIFFE ID extracted from the mTLS peer certificate.
    // +optional
    AttestedAgentSpiffeID string `json:"attestedAgentSpiffeID,omitempty"`
}
```

## Field Changes Summary

| Old Name | New Name | JSON Tag | Reason |
|----------|----------|----------|--------|
| `FetchedAt` | `LastCardFetchTime` | `lastCardFetchTime` | Clarifies this is controller fetch time, not card creation time |
| `CardID` | `CardHash` | `cardHash` | Clarifies this is a content hash, not a unique identifier |
| (new) | `TransportSecurity` | `transportSecurity` | Records transport layer used for fetch |

## Condition Model

| Constant | Old Value | New Value |
|----------|-----------|-----------|
| `ConditionTypeCardSynced` | `"CardSynced"` | Renamed to `ConditionTypeCardFetched` = `"CardFetched"` |

### Condition Reasons

| Old Reason | New Reason | Status | When |
|------------|------------|--------|------|
| `"CardSynced"` | `"Fetched"` | True | mTLS fetch succeeded |
| (new) | `"FetchedInsecure"` | True | Plain HTTP fetch succeeded |
| `"FetchSkipped"` | `"FetchSkipped"` | True | Pod template unchanged |
| `"CardFetchFailed"` | `"FetchFailed"` | False | Fetch error |
| `"ServiceNotFound"` | `"ServiceNotFound"` | False | No matching Service |
| (new) | `"WorkloadNotReady"` | False | No Ready pods |
| `"CardDiscoveryDisabled"` | `"DiscoveryDisabled"` | False | Feature flag off |

## Port Resolution Annotation

| Annotation | Scope | Type | Example |
|------------|-------|------|---------|
| `rossoctl.io/port` | Service | string (numeric) | `"9090"` |

Resolution chain: annotation > port named `a2a` > port named `http` > first port > default 8000
