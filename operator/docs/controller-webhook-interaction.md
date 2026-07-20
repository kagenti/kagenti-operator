# Controller-Webhook Interaction

This document describes how the AgentRuntime controller and AuthBridge mutating webhook coordinate to configure and inject sidecars into agent and tool workloads. Both components run in the same operator binary.

## Two-Phase Model

The AgentRuntime system uses a two-phase model:

1. **Controller phase**: The AgentRuntime controller watches CRs and applies labels + a config-hash annotation to the target workload's PodTemplateSpec. This triggers a Kubernetes rolling update.
2. **Webhook phase**: The AuthBridge mutating webhook intercepts Pod CREATE requests. When a Pod has the `rossoctl.io/type` label (applied by the controller), the webhook injects sidecar containers.

The controller never mutates Pods directly. The webhook does not decide *which labels to apply* — that is encoded by the controller. However, the webhook does require a matching AgentRuntime CR to exist as a gate before injecting sidecars, and reads the CR for per-workload configuration overrides.

## Sequence Diagrams

### AgentRuntime CR Create

```mermaid
sequenceDiagram
    participant User
    participant API as API Server
    participant Val as Validating Webhook
    participant Ctrl as AgentRuntime Controller
    participant WH as AuthBridge Webhook
    participant K8s as Kubernetes (ReplicaSet)

    User->>API: kubectl apply AgentRuntime CR
    API->>Val: Validate (reject duplicate targetRef)
    Val-->>API: Allowed
    API-->>Ctrl: Reconcile event

    Ctrl->>API: Get target Deployment
    Ctrl->>API: Get cluster defaults ConfigMap (rossoctl-platform-config)
    Ctrl->>API: Get feature gates ConfigMap (rossoctl-feature-gates)
    Ctrl->>API: List namespace defaults ConfigMaps (rossoctl.io/defaults=true)
    Note over Ctrl: Merge config: cluster → namespace (2-layer)
    Note over Ctrl: Compute SHA256 config hash

    Ctrl->>API: Patch Deployment:<br/>+ rossoctl.io/type label<br/>+ managed-by label<br/>+ config-hash annotation on PodTemplateSpec
    API-->>K8s: PodTemplateSpec changed → rolling update
    K8s->>API: Create new Pod (with rossoctl.io/type label)
    API->>WH: Mutating admission (Pod CREATE)
    WH->>API: Read AgentRuntime CR overrides
    WH->>API: Read namespace ConfigMaps
    Note over WH: Merge config and build sidecar containers
    WH-->>API: Patch Pod with sidecar containers + volumes
    API-->>K8s: Pod created with sidecars

    Ctrl->>API: Update AgentRuntime status (phase=Active, conditions, configuredPods)
```

### AgentRuntime CR Update (Config Change)

```mermaid
sequenceDiagram
    participant User
    participant API as API Server
    participant Ctrl as AgentRuntime Controller
    participant WH as AuthBridge Webhook
    participant K8s as Kubernetes (ReplicaSet)

    User->>API: kubectl patch AgentRuntime (e.g., change trust domain)
    API-->>Ctrl: Reconcile event

    Ctrl->>API: Get target Deployment
    Note over Ctrl: Recompute config hash (2-layer, no CR fields)
    Note over Ctrl: Hash unchanged → no rolling update needed

    Note over Ctrl: CR spec changes do NOT trigger rolling updates.
    Note over Ctrl: The webhook reads CR overrides at pod CREATE time.

    Ctrl->>API: Update AgentRuntime status
```

### ConfigMap Change (Cluster or Namespace Defaults)

```mermaid
sequenceDiagram
    participant Admin
    participant API as API Server
    participant Ctrl as AgentRuntime Controller
    participant K8s as Kubernetes (ReplicaSet)

    Admin->>API: Update rossoctl-platform-config ConfigMap
    API-->>Ctrl: Watch fires → reconcile all affected AgentRuntimes

    loop For each AgentRuntime targeting a workload
        Ctrl->>API: Get target Deployment
        Note over Ctrl: Recompute config hash (2-layer)
        alt Hash changed
            Ctrl->>API: Patch Deployment config-hash annotation
            API-->>K8s: Rolling update → new Pods with updated sidecars
        else Hash unchanged
            Note over Ctrl: No-op (defaults unchanged)
        end
        Ctrl->>API: Update AgentRuntime status
    end
```

### AgentRuntime CR Delete

```mermaid
sequenceDiagram
    participant User
    participant API as API Server
    participant Ctrl as AgentRuntime Controller
    participant K8s as Kubernetes (ReplicaSet)

    User->>API: kubectl delete AgentRuntime CR
    API-->>Ctrl: Reconcile (deletionTimestamp set)

    Note over Ctrl: Finalizer rossoctl.io/cleanup is present

    Ctrl->>API: Patch Deployment:<br/>- Remove rossoctl.io/type label<br/>- Remove rossoctl.io/config-hash annotation<br/>- Remove managed-by label
    API-->>K8s: PodTemplateSpec changed → rolling update
    Note over K8s: New Pods lack the type label — webhook skips injection

    Ctrl->>API: Remove finalizer from AgentRuntime CR
    API->>API: CR garbage collected
```

## Responsibility Split

| Concern | Controller | Webhook |
|---------|-----------|---------|
| Detect config change | Yes (2-layer merge + hash) | No |
| Trigger pod restart | Yes (annotation on PodTemplateSpec) | No |
| Read ConfigMap data | Yes (for hash computation) | Yes (for sidecar configuration) |
| Merge config values | Yes (2-layer platform config) | Yes (independently, includes CR overrides at admission time) |
| Mutate pod spec | No | Yes (sidecar injection) |
| Read AgentRuntime CR | Yes (primary resource) | Yes (for per-workload overrides) |
| Apply workload labels | Yes | No |
| Decide injection eligibility | No (encodes in labels) | Yes (objectSelector + precedence chain) |

## 2-Layer Configuration Merge (Controller)

The controller computes the config hash from platform-level configuration only (no CR fields):

```
┌──────────────────────────────────────┐
│ Layer 2: Namespace defaults          │  ← higher precedence
│   (ConfigMap with                    │
│    rossoctl.io/defaults=true label)   │
├──────────────────────────────────────┤
│ Layer 1: Cluster defaults            │  ← lower precedence
│   (rossoctl-platform-config in        │
│    rossoctl-system namespace)         │
└──────────────────────────────────────┘
```

**CR-level overrides** (type, authBridgeMode, mtlsMode, skills) are **not** included in the controller's config hash. The webhook reads these fields at pod CREATE time.

**Feature gates** (`rossoctl-feature-gates` ConfigMap) are platform-wide policy and are **not** part of the merge hierarchy. They control which sidecar components are enabled globally and cannot be overridden by namespace defaults or AgentRuntime CRs.

The controller uses the merged config to compute a deterministic SHA256 hash. This hash is set as the `rossoctl.io/config-hash` annotation on the workload's PodTemplateSpec. When platform config changes (cluster or namespace ConfigMaps), the hash changes, which triggers a Kubernetes rolling update. CR spec changes do **not** change the hash.

The webhook performs its own merge at Pod CREATE time, including CR overrides, to resolve the actual configuration values used for sidecar container environment variables.

> **Note:** The controller and webhook use slightly different sources for layer 1. The controller reads the `rossoctl-platform-config` ConfigMap from `rossoctl-system` via the API server. The webhook uses compiled defaults overlaid with `/etc/rossoctl/config.yaml` (PlatformConfig), which is designed to carry equivalent values. Both produce the same effective defaults in a correctly deployed cluster.

## Global and Cluster Configuration

When workloads have the `rossoctl.io/type` label (applied by the operator via an AgentRuntime CR), the webhook uses two levels of global configuration:

### PlatformConfig (Global Defaults)

The webhook loads **PlatformConfig** at startup from compiled defaults overlaid with `/etc/rossoctl/config.yaml`. This provides:

- Sidecar container images and resource requests/limits
- Proxy ports and UID
- Token exchange defaults (token URL, audience, scopes)
- SPIFFE trust domain and socket path
- Observability settings (trace endpoint, protocol, sampling)

PlatformConfig is hot-reloaded via fsnotify when the config file changes. It forms **layer 1** (lowest precedence) of the configuration merge.

### Feature Gates (Global Policy)

Feature gates are loaded from the `rossoctl-feature-gates` ConfigMap (mounted at `/etc/rossoctl/feature-gates/feature-gates.yaml`) and hot-reloaded. They act as cluster-wide kill switches:

| Gate | Default | Effect |
|------|---------|--------|
| `globalEnabled` | `true` | Master kill switch — `false` disables all injection |
| `envoyProxy` | `true` | Enable/disable envoy-proxy + proxy-init |
| `spiffeHelper` | `true` | Enable/disable spiffe-helper |
| `clientRegistration` | `true` | Enable/disable client-registration |
| `injectTools` | `false` | Allow injection for `rossoctl.io/type=tool` workloads |
| `perWorkloadConfigResolution` | `false` | Switch from ValueFrom refs to literal env var injection |

Feature gates are **not** part of the config merge — they cannot be overridden by namespace defaults or AgentRuntime CRs.

### Config Resolution Modes

When `perWorkloadConfigResolution` is **false** (default), the webhook builds sidecar containers with `ValueFrom` ConfigMapKeyRef/SecretKeyRef references. Kubelet resolves these at container start time from namespace ConfigMaps. This means workloads pick up namespace ConfigMap changes on next pod restart without needing a config hash change.

When `perWorkloadConfigResolution` is **true**, the webhook resolves all config values at admission time by reading namespace ConfigMaps and AgentRuntime CR overrides, then injects literal environment variable values into the sidecar containers.

## AgentRuntime Required — Admission Policy

A `ValidatingAdmissionPolicy` prevents the `rossoctl.io/type` label from being set directly on Deployments or StatefulSets. Only the operator's service account (via the AgentRuntime controller) can apply this label. This ensures every enrolled workload has a corresponding AgentRuntime CR, which provides:
- Automatic rolling updates on config change (any layer)
- Status reporting (phase, conditions, configured pod count)
- Graceful cleanup via finalizer

## Related Documentation

- [API Reference](api-reference.md) — AgentRuntime and AgentCard CRD specifications
- [AuthBridge Webhook Design](authbridge-webhook.md) — Sidecar injection precedence chain and configuration merge
- [Architecture](architecture.md) — Overall operator architecture
