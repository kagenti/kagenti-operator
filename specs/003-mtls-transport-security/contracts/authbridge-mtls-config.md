# Contract: Authbridge mTLS Configuration

> **SUPERSEDED**: The ConfigMap-based `mtls:` block injection approach has been replaced by an annotation + env var approach per PR #405 team review. See below for the current contract.

## Owner

rossoctl-operator controller + webhook (producer) → authbridge sidecar (consumer)

## Current Contract: Annotation + Env Var

### Controller → Workload Pod Template

The controller sets an annotation on the workload's pod template:

```yaml
metadata:
  annotations:
    rossoctl.io/mtls-mode: "permissive"  # or "strict" or "disabled"
```

When `mTLSMode` changes on the AgentRuntime CR, this annotation changes, triggering a rolling restart via Kubernetes pod template change detection.

### Webhook → Authbridge Sidecar

At pod CREATE time, the webhook reads `mTLSMode` from the AgentRuntime CR and sets an environment variable on the authbridge sidecar container:

```yaml
env:
  - name: MTLS_MODE
    value: "permissive"  # or "strict" or "disabled"
```

Authbridge reads `MTLS_MODE` at startup to configure its TLS listeners.

## Behavior Contract

| mTLSMode | Annotation value | Env var | Inbound behavior | Outbound behavior |
|----------|-----------------|---------|-----------------|------------------|
| `disabled` | `disabled` | `disabled` | Plaintext only | Plaintext only |
| `permissive` | `permissive` | `permissive` | TLS-sniff: accepts TLS and plaintext | Plaintext |
| `strict` | `strict` | `strict` | TLS required, rejects plaintext | TLS required |

## Separation of Concerns

| Component | Responsibility |
|-----------|---------------|
| **Controller** | Sets `rossoctl.io/mtls-mode` annotation on pod template. Triggers rolling restart on change. |
| **Webhook** | Reads `mTLSMode` from AgentRuntime CR at pod CREATE. Sets `MTLS_MODE` env var on authbridge container. |
| **Authbridge** | Reads `MTLS_MODE` env var at startup. Configures TLS listeners accordingly. No Kubernetes API dependency. |

## SPIFFE Config (existing, no change)

The SPIFFE block is already handled by the webhook when spiffe-helper is injected. No changes needed.
