# Architecture

This document provides a detailed overview of the Kagenti Operator architecture, including its components, workflows, and design principles.

## Table of Contents

- [Overview](#overview)
- [Core Components](#core-components)
- [Architecture Diagram](#architecture-diagram)
- [Controller Architecture](#controller-architecture)
- [Security Architecture](#security-architecture)
- [Reconciliation Loops](#reconciliation-loops)
- [Deployment Modes](#deployment-modes)

---

## Overview

The Kagenti Operator is a Kubernetes controller that implements the [Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/) to automate the discovery and lifecycle management of AI agents. It follows a **Deployment-first model**: users create standard Kubernetes Deployments or StatefulSets, and the operator discovers them via `AgentCard` CRs with `targetRef`.

### Design Goals

- **Deployment-First**: Users create standard Kubernetes workloads; the operator handles discovery
- **Declarative Configuration**: Infrastructure as Code using Kubernetes CRDs
- **Security**: Signature verification, identity binding, RBAC, and least-privilege principles
- **Scalability**: Supports multiple agents concurrently
- **Cloud-Native**: Leverages native Kubernetes primitives and patterns

---

## Core Components

### Custom Resource Definitions (CRDs)

#### AgentCard CRD
- Provides dynamic discovery of AI agents via `targetRef`-based workload binding
- Fetches and caches agent metadata (A2A agent cards) from running workloads
- Supports signature verification and identity binding
- Stores agent capabilities, skills, and endpoint information

#### AgentRuntime CRD
- The declarative way to enroll a workload into the Kagenti platform
- Developer creates an AgentRuntime CR with `targetRef` — the controller applies labels and triggers injection
- Configures identity (SPIFFE) and observability (OTEL traces) per workload via 3-layer defaults (cluster → namespace → CR)
- Uses `targetRef` to reference backing workloads (Deployment, StatefulSet)
- The `kagenti.io/type` label applied by the controller triggers the webhook's `objectSelector`
- Developer workloads stay completely clean — no kagenti labels required in manifests

### Controllers

#### AgentCard Controller
- Watches AgentCard resources
- Resolves workloads via `targetRef` (Deployments, StatefulSets)
- Fetches agent cards from running workloads
- Verifies signatures and evaluates identity bindings
- Updates status with cached card data and conditions

#### AgentCardSync Controller
- Watches Deployments and StatefulSets with agent labels
- Automatically creates AgentCard resources for discovered workloads
- Sets owner references for garbage collection

#### AgentCard NetworkPolicy Controller
- Watches AgentCard resources when `--enforce-network-policies` is enabled
- Creates **permissive** NetworkPolicies for agents with verified signatures (and binding, if configured)
- Creates **restrictive** NetworkPolicies for agents that fail verification
- Resolves pod selectors from the backing workload's pod template labels

#### AgentRuntime Controller
- Watches AgentRuntime CRs, Deployments, StatefulSets, and ConfigMaps
- Applies `kagenti.io/type` label and `kagenti.io/config-hash` annotation to target workloads
- Computes config hash from 3-layer merged configuration (cluster defaults → namespace defaults → CR overrides)
- Triggers rolling updates when configuration changes (any layer)
- On CR deletion: removes type label, managed-by label and config-hash annotation (causing the workload to lose sidecars)
- Coordinates with the AuthBridge mutating webhook (in-process) which injects sidecars at Pod CREATE time

### Supporting Components

#### Webhooks
- **AuthBridge Mutating Webhook**: Intercepts Pod CREATE requests and injects sidecar containers (envoy-proxy, proxy-init, spiffe-helper, client-registration) based on feature gates, workload labels, and AgentRuntime CR configuration. See [AuthBridge Webhook Design](authbridge-webhook.md) for the full precedence chain and configuration merge.
- **AgentCard Validator**: Ensures `targetRef` is set on AgentCards. Rejects duplicate `targetRef` entries (prevents multiple AgentCards targeting the same workload in a namespace).
- **AgentRuntime Validator**: Rejects duplicate `targetRef` entries (prevents multiple AgentRuntime CRs targeting the same workload in a namespace). Uses authoritative API server reads to eliminate informer cache-lag races.

#### Signature Providers
- **X5CProvider**: Validates `x5c` certificate chains against the SPIRE X.509 trust bundle and verifies JWS signatures using the leaf public key

---

## Architecture Diagram

```mermaid
graph TB
    subgraph "User Interaction"
        User[User/Developer]
        User -->|Creates| Deployment[Deployment/StatefulSet]
        User -->|Creates| CardCR[AgentCard CR]
        User -->|Creates| RuntimeCR[AgentRuntime CR]
    end

    subgraph "Kagenti Operator"
        ValidationWebhook[Validating Webhooks]
        InjectionWebhook[AuthBridge Mutating Webhook]
        CardController[AgentCard Controller]
        SyncController[AgentCardSync Controller]
        RuntimeController[AgentRuntime Controller]
        CardCR -->|Validates| ValidationWebhook
        RuntimeCR -->|Validates| ValidationWebhook

        ValidationWebhook -->|Valid CR| CardController
    end

    subgraph "Config Sources"
        ClusterCM[Cluster Defaults ConfigMaps]
        NsCM[Namespace Defaults ConfigMap]
        TrustBundle[SPIRE Trust Bundle ConfigMap]
    end

    subgraph "Runtime"
        Pod[Agent Pods]

        Deployment -->|Creates| Pod
        CardController -->|Fetches agent card from| Pod
        InjectionWebhook -->|Injects sidecars at CREATE| Pod
    end

    SigProvider -->|Validates x5c chain| TrustBundle

    RuntimeController -->|Applies labels + config-hash| Deployment
    RuntimeController -->|Reads defaults| ClusterCM
    RuntimeController -->|Reads defaults| NsCM
    RuntimeController -->|Watches| RuntimeCR
    InjectionWebhook -->|Reads config| ClusterCM

    SyncController -->|Watches| Deployment
    SyncController -->|Auto-creates| CardCR
    CardCR -->|targetRef| Deployment
    RuntimeCR -->|targetRef| Deployment

    style User fill:#ffecb3
    style CardCR fill:#e1f5fe
    style RuntimeCR fill:#e1f5fe
    style ValidationWebhook fill:#fff3e0
    style InjectionWebhook fill:#fff3e0
    style CardController fill:#ffe0b2
    style SyncController fill:#ffe0b2
    style Deployment fill:#d1c4e9
    style Pod fill:#c8e6c9
```

---

## Controller Architecture

### AgentCard Controller

The AgentCard Controller reconciles AgentCard CRs by resolving the backing workload via `targetRef`, fetching the agent card from the running agent, and storing the result in status.

#### Reconciliation Flow

```
1. Watch for AgentCard CR changes
2. Resolve workload via spec.targetRef (Deployment or StatefulSet)
3. Construct service URL for the agent
4. Fetch agent card from /.well-known/agent-card.json
5. Optionally verify signature (if --require-a2a-signature)
6. Evaluate identity binding (if spec.identityBinding configured)
7. Update AgentCard status:
   a. Store cached card data
   b. Set sync conditions
   c. Record signature verification result
8. Requeue after syncPeriod for next fetch
```

#### AgentCard Naming Convention

The controller maintains several conditions:

| Condition | Meaning |
|-----------|---------|
| `Synced` | Agent card fetched successfully from the workload |
| `Ready` | Agent card is available for discovery queries |

Examples:
- Deployment `weather-agent` -> AgentCard `weather-agent-deployment-card`
- StatefulSet `weather-agent` -> AgentCard `weather-agent-statefulset-card`

### AgentRuntime Controller

The AgentRuntime Controller reconciles AgentRuntime CRs by resolving the target workload, computing a config hash from the 3-layer merged configuration, and applying labels and annotations to trigger rolling updates and webhook injection.

#### Reconciliation Flow

```
1. Fetch AgentRuntime CR
2. Handle deletion (if marked for deletion):
   a. Remove kagenti.io/type label from workload metadata and PodTemplateSpec
   b. Remove kagenti.io/config-hash annotation from PodTemplateSpec (triggers rolling update)
   c. Remove managed-by label
   d. Remove finalizer
3. Ensure kagenti.io/cleanup finalizer is present
4. Resolve targetRef (verify Deployment/StatefulSet exists)
5. Compute config hash from merged configuration:
   a. Read cluster defaults (kagenti-platform-config)
   b. Read cluster feature gates (kagenti-feature-gates)
      Note: feature gates are platform-wide policy — they are NOT
      overrideable by namespace defaults or AgentRuntime CRs.
   c. Read namespace defaults (ConfigMap with kagenti.io/defaults=true)
   d. Merge defaults: cluster → namespace → CR spec (CR wins)
   e. Hash the merged result (deterministic SHA256)
   f. Surface warnings (e.g., multiple namespace defaults ConfigMaps)
      as a ConfigResolved condition on the AgentRuntime status
6. Apply to target workload:
   a. kagenti.io/type label on workload metadata + PodTemplateSpec
   b. app.kubernetes.io/managed-by: kagenti-operator on workload metadata
   c. kagenti.io/config-hash annotation on PodTemplateSpec
7. Count configured pods and update status
```

#### Controller ↔ Webhook Interaction

The controller and the AuthBridge mutating webhook (both in the same operator binary) work together:

```
AgentRuntime CR created/updated
  → Controller applies kagenti.io/type label + config-hash annotation
    → PodTemplateSpec change triggers Kubernetes rolling update
      → New Pods created with kagenti.io/type label
        → Webhook's objectSelector matches → injects AuthBridge sidecars
```

| Concern | Controller | Webhook |
|---------|-----------|---------|
| Detect config change | Yes (3-layer merge + hash) | No |
| Trigger pod restart | Yes (annotation on PodTemplateSpec) | No |
| Read ConfigMap data | Yes (for hash computation) | Yes (for sidecar configuration) |
| Merge config values | Yes (same 3-layer merge) | Yes (independently) |
| Mutate pod spec | No | Yes (sidecar injection) |

#### Watches

| Resource | Scope | Purpose |
|----------|-------|---------|
| AgentRuntime | All namespaces | Primary resource |
| Deployment | All namespaces | Re-reconcile if target workload modified externally |
| StatefulSet | All namespaces | Re-reconcile if target workload modified externally |
| ConfigMap (cluster) | `kagenti-system` | Recompute hash when cluster defaults change |
| ConfigMap (namespace) | `kagenti.io/defaults=true` | Recompute hash when namespace defaults change |

#### Conditions

| Condition | Meaning |
|-----------|---------|
| `TargetResolved` | Target workload (Deployment/StatefulSet) exists |
| `ConfigResolved` | Configuration merged successfully. Reason is `ConfigResolved` when clean, `ConfigWarning` when ambiguity detected (e.g., multiple namespace defaults ConfigMaps). Warnings are surfaced in the condition message and as Kubernetes events. |
| `Ready` | Labels and config-hash applied successfully |

### NetworkPolicy Controller

The NetworkPolicy Controller enforces network isolation based on signature verification.

#### Reconciliation Flow

```
1. Watch AgentCard resources (when --enforce-network-policies is enabled)
2. Resolve the workload and pod selector labels
3. Determine verification status:
   a. If identity binding configured: both signature AND binding must pass
   b. Otherwise: signature verification alone
4. Create permissive or restrictive NetworkPolicy
5. Clean up NetworkPolicy on AgentCard deletion
```

#### Policy Types

| Status | Policy | Effect |
|--------|--------|--------|
| Verified | Permissive | Allows traffic from/to other verified agents |
| Unverified | Restrictive | Blocks all traffic except DNS and operator |

---

## Security Architecture

### Signature Verification

The operator verifies JWS signatures embedded in agent cards per A2A spec section 8.4:

1. Extract `x5c` certificate chain from JWS protected header
2. Validate the chain against the SPIRE X.509 trust bundle
3. Extract the SPIFFE ID from the leaf certificate's SAN URI
4. Extract the leaf public key and verify the JWS signature (reject `none`, verify key type matches `alg`)
5. Create canonical JSON payload (sorted keys, no whitespace, `signatures` field excluded)
6. Reconstruct signing input: `BASE64URL(protected) || '.' || BASE64URL(canonical_payload)`
7. Verify the cryptographic signature against the leaf public key

Supported algorithms: RS256, RS384, RS512, ES256, ES384, ES512.

### Identity Binding

When `spec.identityBinding` is configured on an AgentCard:

1. The SPIFFE ID is extracted from the leaf certificate's SAN URI (proven by the x5c chain, not self-asserted)
2. The SPIFFE ID's trust domain is validated against the configured trust domain (`spec.identityBinding.trustDomain` or `--spire-trust-domain`)
3. Both signature AND binding must pass for the `signature-verified=true` label
4. NetworkPolicy enforcement uses this label for traffic control

---

## Security Model

### RBAC

The operator implements least-privilege access control. All permissions are declared as `+kubebuilder:rbac` markers on the controller source files and compiled into the `manager-role` ClusterRole at `config/rbac/role.yaml`.

#### AgentRuntime Controller Permissions

Source: `internal/controller/agentruntime_controller.go`

| API Group | Resources | Verbs | Purpose |
|-----------|-----------|-------|---------|
| `agent.kagenti.dev` | `agentruntimes` | get, list, watch, create, update, patch, delete | Full lifecycle management of the primary resource |
| `agent.kagenti.dev` | `agentruntimes/status` | get, update, patch | Set phase (Pending/Active/Error), conditions, and configuredPods count |
| `agent.kagenti.dev` | `agentruntimes/finalizers` | update | Add/remove `kagenti.io/cleanup` finalizer for graceful deletion |
| `apps` | `deployments`, `statefulsets` | get, list, watch, update, patch | Resolve targetRef, apply labels (`kagenti.io/type`, `managed-by`) and config-hash annotation |
| `""` (core) | `configmaps` | get, list, watch | Read cluster defaults (`kagenti-platform-config`), feature gates (`kagenti-feature-gates`), and namespace defaults |
| `""` (core) | `pods` | get, list, watch | Count configured pods and verify ownership chains |
| `""` (core) | `events` | create, patch | Record reconciliation events (TargetNotFound, ConfigWarning, Configured) |

#### AgentCard Controller Permissions

Source: `internal/controller/agentcard_controller.go`

| API Group | Resources | Verbs | Purpose |
|-----------|-----------|-------|---------|
| `agent.kagenti.dev` | `agentcards` | get, list, watch, create, update, patch, delete | Full lifecycle management |
| `agent.kagenti.dev` | `agentcards/status` | get, update, patch | Store cached card data, sync conditions, signature results |
| `agent.kagenti.dev` | `agentcards/finalizers` | update | Finalizer management |
| `apps` | `deployments`, `statefulsets` | get, list, watch, update, patch | Resolve targetRef and propagate signature labels |
| `""` (core) | `services` | get, list, watch | Construct service URLs for agent card fetching |
| `""` (core) | `configmaps` | get, list, watch | Read SPIRE trust bundle for signature verification |

#### AgentCard NetworkPolicy Controller Permissions

Source: `internal/controller/agentcard_networkpolicy_controller.go`

| API Group | Resources | Verbs | Purpose |
|-----------|-----------|-------|---------|
| `networking.k8s.io` | `networkpolicies` | get, list, watch, create, update, patch, delete | Create permissive/restrictive NetworkPolicies based on signature verification |
| `""` (core) | `pods` | get, list, watch, update, patch | Resolve pod selectors from workload pod template labels |

#### Cross-Namespace Considerations

- The operator uses a **ClusterRole** and **ClusterRoleBinding** in cluster-wide mode, allowing it to reconcile resources across all namespaces
- In **namespaced mode** (`NAMESPACES2WATCH` env var), the same permissions are scoped to specific namespaces via **Role** and **RoleBinding**
- The AgentRuntime controller reads ConfigMaps from `kagenti-system` (cluster defaults) regardless of mode — this requires cross-namespace read access
- Namespace defaults ConfigMaps are read from the workload's own namespace

### Secret Management

## Reconciliation Loops

### AgentCard Reconciliation

```go
func (r *AgentCardReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
    // 1. Fetch AgentCard
    agentCard := &AgentCard{}
    if err := r.Get(ctx, req.NamespacedName, agentCard); err != nil {
        return Result{}, client.IgnoreNotFound(err)
    }

    // 2. Resolve workload via targetRef (duck typing)
    workload, err := r.getWorkload(ctx, agentCard)

    // 3. Check workload readiness
    if !workload.Ready { ... }

    // 4. Get protocol and fetch agent card
    card, err := r.AgentFetcher.Fetch(ctx, protocol, serviceURL)

    // 5. Verify signature (if enabled)
    if r.RequireSignature {
        result, err := r.verifySignature(ctx, cardData)
    }

    // 6. Evaluate identity binding (if configured)
    if agentCard.Spec.IdentityBinding != nil {
        binding := r.computeBinding(agentCard, verifiedSpiffeID)
    }

    // 7. Update status and propagate labels
    r.updateAgentCardStatus(ctx, agentCard, ...)
    r.propagateSignatureLabel(ctx, workload, isVerified)

    return Result{RequeueAfter: syncPeriod}, nil
}
```

---

## Deployment Modes

### Cluster-Wide Mode

- Operator watches all namespaces
- Uses ClusterRole and ClusterRoleBinding
- Suitable for platform teams
- Single operator instance manages entire cluster

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kagenti-operator-manager-role
rules:
  - apiGroups: ["agent.kagenti.dev"]
    resources: ["agentcards"]
    verbs: ["*"]
```

### Namespaced Mode

- Operator watches specific namespaces (via `NAMESPACES2WATCH` env var)
- Uses Role and RoleBinding per namespace
- Suitable for multi-tenant environments
- Multiple operator instances possible

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kagenti-operator-manager-role
  namespace: team1
rules:
  - apiGroups: ["agent.kagenti.dev"]
    resources: ["agentcards"]
    verbs: ["*"]
```

---

## Performance and Scalability

### Resource Management

- Controllers use efficient caching (controller-runtime informers)
- Field indexers for fast AgentCard lookups by targetRef name
- Reconciliation includes backoff for transient errors
- Status updates use optimistic locking with retry

### Scaling Considerations

| Component | Scaling Strategy |
|-----------|------------------|
| Operator | Single replica (leader election optional) |
| Agents | Horizontal scaling via replicas field |
| AgentRuntimes | One per agent/tool workload |
| AgentCards | One per agent workload |
| NetworkPolicies | One per AgentCard (when enforcement enabled) |

### Monitoring

The operator exposes metrics via Prometheus:

- Reconciliation duration and error rates
- Signature verification counters, duration, and errors (`a2a_signature_verification_*`)

---

## Additional Resources

- [API Reference](./api-reference.md) — CRD specifications
- [Controller-Webhook Interaction](./controller-webhook-interaction.md) — AgentRuntime controller and AuthBridge webhook coordination
- [Dynamic Agent Discovery](./dynamic-agent-discovery.md) — AgentCard discovery system
- [Signature Verification](./agentcard-signature-verification.md) — JWS signature setup guide
- [Identity Binding](./agentcard-identity-binding.md) — SPIFFE identity binding guide
- [AuthBridge Webhook Design](./authbridge-webhook.md) — Sidecar injection precedence and configuration
- [Operator-Managed Client Registration](./operator-managed-client-registration.md) — Keycloak credential lifecycle
- [Developer Guide](./dev.md) — Contributing to the operator
- [Getting Started](../GETTING_STARTED.md) — Tutorials and examples
