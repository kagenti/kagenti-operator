# Data Model: mTLS Transport Security for Agent Communication

## Entity Changes

### Modified: AgentRuntimeSpec

| Field | Change | Before | After |
|-------|--------|--------|-------|
| `mTLSMode` | Default changed | `disabled` | `permissive` |

No new fields added to spec. The existing `mTLSMode` field (enum: `disabled`, `permissive`, `strict`) is reused with a changed default.

### Modified: AgentRuntimeStatus (via conditions)

New condition type added:

| Condition | Description |
|-----------|-------------|
| `MTLSReady` | Whether mTLS infrastructure (SPIRE) is available for this workload |

### Existing: CardStatus (AgentRuntime status.card)

No changes to the struct. The following fields are already defined and will be populated by the mTLS-enabled card fetch:

| Field | Type | Description | Populated When |
|-------|------|-------------|---------------|
| `transportSecurity` | TransportSecurity | `mtls` or `http` | Every card fetch |
| `attestedAgentSpiffeID` | string | SPIFFE ID from peer certificate | mTLS card fetch |
| `validSignature` | *bool | JWS signature validation result | Signature present (deprecated path) |

### New Condition: MTLSReady

Added to AgentRuntime `status.conditions[]`.

| Reason | Status | When |
|--------|--------|------|
| `SPIREAvailable` | True | SPIRE is deployed and certificates are available to the workload |
| `SPIREUnavailable` | False | mTLSMode is permissive or strict but SPIRE infrastructure is not detected |
| `MTLSDisabled` | True | mTLSMode is explicitly set to `disabled` |

### New: Pod Template Annotation

The controller sets a `rossoctl.io/mtls-mode` annotation on the workload's pod template. This serves two purposes: (1) triggers a rolling restart when the value changes, and (2) makes the mTLS mode visible on the pod.

| Annotation | Values | Set By |
|------------|--------|--------|
| `rossoctl.io/mtls-mode` | `permissive`, `strict`, `disabled` | Controller (on pod template) |

### New: Authbridge Sidecar Env Var

The webhook sets an `MTLS_MODE` environment variable on the authbridge sidecar container at pod CREATE time.

| Env Var | Values | Set By | Read By |
|---------|--------|--------|---------|
| `MTLS_MODE` | `permissive`, `strict`, `disabled` | Webhook (at pod CREATE) | Authbridge (at startup) |

### Modified: Feature Flags (cmd/main.go)

| Flag | Before Default | After Default | Notes |
|------|---------------|---------------|-------|
| `--enable-verified-fetch` | `false` | `true` | Kill switch retained for one release |
| `--require-a2a-signature` | `true` | `false` | Deprecated |
| `--signature-audit-mode` | `true` | `false` | Deprecated |
| `--enforce-network-policies` | `true` | `false` | Deprecated |

## Relationships

```
AgentRuntime.spec.mTLSMode
    │
    ├── Controller sets rossoctl.io/mtls-mode annotation on pod template
    │       │
    │       └── Annotation change triggers rolling restart
    │
    ├── Webhook reads mTLSMode from AgentRuntime CR at pod CREATE
    │       │
    │       └── Sets MTLS_MODE env var on authbridge sidecar container
    │               │
    │               ├── Inbound (reverse proxy): mTLS termination
    │               └── Outbound (forward proxy): mTLS origination
    │
    ├── Operator sets MTLSReady condition on AgentRuntime.status
    │       │  (informational — does NOT block Ready condition)
    │       │
    │       ├── SPIREAvailable (True) — SPIRE detected
    │       ├── SPIREUnavailable (False) — SPIRE not detected + Warning Event
    │       └── MTLSDisabled (True) — mTLSMode: disabled
    │
    └── Controller uses SpiffeFetcher (when SPIRE available)
            │
            └── Fetches A2A card from live agent over mTLS
                    │
                    ├── status.card.transportSecurity = "mtls"
                    └── status.card.attestedAgentSpiffeID = <SPIFFE ID>
```

## State Transitions for mTLSMode

```
[Default: permissive] -- operator sets mTLSMode: strict --> [Strict]
[Default: permissive] -- operator sets mTLSMode: disabled --> [Disabled]
[Strict] -- operator removes mTLSMode --> [Default: permissive]
[Disabled] -- operator removes mTLSMode --> [Default: permissive]

ConfigMap hash changes on every mTLSMode transition → rolling restart
```

## State Transitions for MTLSReady condition

```
[Not Set] -- reconcile with mTLSMode non-disabled + SPIRE available --> [True/SPIREAvailable]
[Not Set] -- reconcile with mTLSMode non-disabled + SPIRE absent --> [False/SPIREUnavailable]
[Not Set] -- reconcile with mTLSMode: disabled --> [True/MTLSDisabled]
[False/SPIREUnavailable] -- SPIRE deployed --> [True/SPIREAvailable]
[True/SPIREAvailable] -- mTLSMode changed to disabled --> [True/MTLSDisabled]
```
