# Architecture

This document provides a detailed overview of the Rossoctl Operator architecture, including its components, workflows, and design principles.

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

The Rossoctl Operator is a Kubernetes controller that implements the [Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/) to automate the discovery and lifecycle management of AI agents. It follows a **Deployment-first model**: users create standard Kubernetes Deployments or StatefulSets, and the operator discovers them via `AgentCard` CRs with `targetRef`.

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
- The declarative way to enroll a workload into the Rossoctl platform
- Developer creates an AgentRuntime CR with `targetRef` — the controller applies labels and triggers injection
- Platform-level config (cluster → namespace) drives the config hash; per-CR overrides are read by the webhook at pod CREATE time
- Uses `targetRef` to reference backing workloads (Deployment, StatefulSet)
- The `rossoctl.io/type` label applied by the controller triggers the webhook's `objectSelector`
- Developer workloads only need a `protocol.rossoctl.io/a2a` label — the controller applies `rossoctl.io/type` and `managed-by` labels automatically

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
- Applies `rossoctl.io/type` label and `rossoctl.io/config-hash` annotation to target workloads
- Computes config hash from 2-layer merged configuration (cluster defaults → namespace defaults)
- Discovers linked skills by reading the `rossoctl.io/skills` annotation from target workloads when the `skillDiscovery` feature gate is enabled
- Triggers rolling updates when configuration changes
- On CR deletion: removes type label, managed-by label and config-hash annotation (causing the workload to lose sidecars)
- Coordinates with the AuthBridge mutating webhook (in-process) which injects sidecars at Pod CREATE time

#### SPIRE Operand Controller
- Creates and reconciles 5 SPIRE operand CRs (`operator.openshift.io/v1alpha1`) when ZTWIM CRDs are present on the cluster
- CRD-gated: no feature flag needed — automatically activates on OpenShift 4.19+ where ZTWIM operator is installed
- Manages: ZeroTrustWorkloadIdentityManager (parent), SpiffeCSIDriver, SpireServer, SpireAgent, SpireOIDCDiscoveryProvider (children)
- Drift correction: any manual change to CR spec or deletion is automatically corrected
- Replaces fragile Helm post-install hooks with proper reconciliation loop
- Uses `SpireBootstrapRunnable` to create initial ZTWIM CR at startup, triggering the controller's watch

### Supporting Components

#### Webhooks
- **AuthBridge Mutating Webhook**: Intercepts Pod CREATE requests and injects sidecar containers (envoy-proxy, proxy-init, spiffe-helper, client-registration) based on feature gates, workload labels, and AgentRuntime CR configuration. See [AuthBridge Webhook Design](authbridge-webhook.md) for the full precedence chain and configuration merge.
- **AgentCard Validator**: Ensures `targetRef` is set on AgentCards. Rejects duplicate `targetRef` entries (prevents multiple AgentCards targeting the same workload in a namespace).
- **AgentRuntime Validator**: Rejects duplicate `targetRef` entries (prevents multiple AgentRuntime CRs targeting the same workload in a namespace). Uses authoritative API server reads to eliminate informer cache-lag races.

#### Admission Policies
- **Agent Label Protection (ValidatingAdmissionPolicy)**: Prevents manual application of the `rossoctl.io/type` label on Deployments and StatefulSets. Only the operator's service account (via an AgentRuntime CR) is allowed to set this label. Users who attempt to add the label directly are rejected with a message directing them to create an AgentRuntime instead. The policy allows non-operator users to update workloads that already carry the label, as long as they don't change its value.

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

    subgraph "Rossoctl Operator"
        ValidationWebhook[Validating Webhooks]
        InjectionWebhook[AuthBridge Mutating Webhook]
        VAP[Agent Label Protection VAP]
        CardController[AgentCard Controller]
        SyncController[AgentCardSync Controller]
        RuntimeController[AgentRuntime Controller]
        SpireController[SPIRE Operand Controller]
        CardCR -->|Validates| ValidationWebhook
        RuntimeCR -->|Validates| ValidationWebhook
        Deployment -->|CREATE/UPDATE with rossoctl.io/type| VAP

        ValidationWebhook -->|Valid CR| CardController
    end

    subgraph "SPIRE Infrastructure"
        ZTWIMCR[ZTWIM CR]
        SpireCRs[SpireServer + SpireAgent + SpiffeCSIDriver + SpireOIDC]
        ZTWIMOperator[ZTWIM Operator]
        SpireController -->|Creates/Updates| ZTWIMCR
        SpireController -->|Creates/Updates| SpireCRs
        ZTWIMOperator -->|Reconciles| ZTWIMCR
        ZTWIMOperator -->|Reconciles| SpireCRs
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
    style VAP fill:#fff3e0
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
   a. Remove rossoctl.io/type label from workload metadata and PodTemplateSpec
   b. Remove rossoctl.io/config-hash annotation from PodTemplateSpec (triggers rolling update)
   c. Remove managed-by label
   d. Remove finalizer
3. Ensure rossoctl.io/cleanup finalizer is present
4. Resolve targetRef (verify Deployment/StatefulSet exists)
5. Compute config hash from merged configuration:
   a. Read cluster defaults (rossoctl-platform-config)
   b. Read cluster feature gates (rossoctl-feature-gates)
      Note: feature gates are platform-wide policy — they are NOT
      overrideable by namespace defaults or AgentRuntime CRs.
   c. Read namespace defaults (ConfigMap with rossoctl.io/defaults=true)
   d. Merge defaults: cluster → namespace (2-layer, no CR fields)
   e. Hash the merged result (deterministic SHA256)
   f. Surface warnings (e.g., multiple namespace defaults ConfigMaps)
      as a ConfigResolved condition on the AgentRuntime status
6. Apply to target workload:
   a. rossoctl.io/type label on workload metadata + PodTemplateSpec
   b. app.kubernetes.io/managed-by: rossoctl-operator on workload metadata
   c. rossoctl.io/config-hash annotation on PodTemplateSpec
7. Count configured pods and update status
```

#### Controller ↔ Webhook Interaction

The controller and the AuthBridge mutating webhook (both in the same operator binary) work together:

```
AgentRuntime CR created/updated
  → Controller applies rossoctl.io/type label + config-hash annotation
    → PodTemplateSpec change triggers Kubernetes rolling update
      → New Pods created with rossoctl.io/type label
        → Webhook's objectSelector matches → injects AuthBridge sidecars
```

| Concern | Controller | Webhook |
|---------|-----------|---------|
| Detect config change | Yes (2-layer merge + hash) | No |
| Trigger pod restart | Yes (annotation on PodTemplateSpec) | No |
| Read ConfigMap data | Yes (for hash computation) | Yes (for sidecar configuration) |
| Merge config values | Yes (2-layer platform config) | Yes (independently) |
| Mutate pod spec | No | Yes (sidecar injection) |

#### Watches

| Resource | Scope | Purpose |
|----------|-------|---------|
| AgentRuntime | All namespaces | Primary resource |
| Deployment | All namespaces | Re-reconcile if target workload modified externally |
| StatefulSet | All namespaces | Re-reconcile if target workload modified externally |
| ConfigMap (cluster) | `rossoctl-system` | Recompute hash when cluster defaults change |
| ConfigMap (namespace) | `rossoctl.io/defaults=true` | Recompute hash when namespace defaults change |

#### Conditions

| Condition | Meaning |
|-----------|---------|
| `TargetResolved` | Target workload (Deployment/StatefulSet) exists |
| `ConfigResolved` | Configuration merged successfully. Reason is `ConfigResolved` when clean, `ConfigWarning` when ambiguity detected (e.g., multiple namespace defaults ConfigMaps). Warnings are surfaced in the condition message and as Kubernetes events. |
| `Ready` | Labels and config-hash applied successfully |

### SPIRE Operand Controller

The SPIRE Operand Controller manages the lifecycle of 5 SPIRE operand CRs on OpenShift clusters where the ZTWIM (Zero Trust Workload Identity Manager) operator is installed. It replaces fragile Helm post-install hooks with a proper reconciliation loop that corrects drift.

#### CRD Gating

The controller uses `SpireOperandCRDExists()` to check for the `ZeroTrustWorkloadIdentityManager` CRD via the Kubernetes discovery API (3 retries). No feature flag is needed — the controller activates automatically when CRDs are present (OpenShift 4.19+).

#### Bootstrap Flow

`SpireBootstrapRunnable` is a one-shot `manager.Runnable` that creates the initial ZTWIM CR at startup if absent. This triggers the controller's watch, which then creates the 4 child CRs.

#### Reconciliation Flow

```
1. Get ZTWIM CR "cluster" — if NotFound, return (bootstrap pending)
2. ensureUnstructuredCR(ZTWIM) — CreateOrUpdate with desired spec:
   - trustDomain (auto-discovered), clusterName="agent-platform", bundleConfigMap="spire-bundle"
3. ensureChildren() — CreateOrUpdate for each of 4 children:
   a. SpiffeCSIDriver: agentSocketPath, pluginName
   b. SpireServer: caSubject, persistence, datastore, jwtIssuer
   c. SpireAgent: nodeAttestor, workloadAttestors
   d. SpireOIDCDiscoveryProvider: csiDriverName, jwtIssuer
4. Record events on create/update, return success
```

Children inherit `trustDomain` and `clusterName` from the parent ZTWIM CR — these fields are NOT set on child CRs (OCP 4.19 CRD constraint).

#### Managed CRs

All CRs are `operator.openshift.io/v1alpha1`, cluster-scoped, name `"cluster"`:

| CR | Key Spec Fields | Role |
|---|---|---|
| ZeroTrustWorkloadIdentityManager | trustDomain, clusterName, bundleConfigMap | Parent — created first |
| SpiffeCSIDriver | agentSocketPath, pluginName | CSI volume plugin for SVID mounting |
| SpireServer | caSubject, persistence, datastore, jwtIssuer | SPIRE server configuration |
| SpireAgent | nodeAttestor, workloadAttestors | Node-level SPIRE agent |
| SpireOIDCDiscoveryProvider | csiDriverName, jwtIssuer | OIDC endpoint for JWT-SVID |

#### Watches

| Resource | Scope | Purpose |
|----------|-------|---------|
| ZeroTrustWorkloadIdentityManager | Cluster | Primary resource — reconcile on create/update/delete |
| SpiffeCSIDriver | Cluster | Secondary — map to ZTWIM reconcile for drift correction |
| SpireServer | Cluster | Secondary — map to ZTWIM reconcile for drift correction |
| SpireAgent | Cluster | Secondary — map to ZTWIM reconcile for drift correction |
| SpireOIDCDiscoveryProvider | Cluster | Secondary — map to ZTWIM reconcile for drift correction |

#### Drift Correction

Uses `controllerutil.CreateOrUpdate` — if a CR exists but spec differs from desired state, it is updated. If a CR is deleted, it is recreated on next reconcile (triggered by the child watch). All CRs are labeled `app.kubernetes.io/managed-by: rossoctl-operator`.

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
| `agent.rossoctl.dev` | `agentruntimes` | get, list, watch, create, update, patch, delete | Full lifecycle management of the primary resource |
| `agent.rossoctl.dev` | `agentruntimes/status` | get, update, patch | Set phase (Pending/Active/Error), conditions, and configuredPods count |
| `agent.rossoctl.dev` | `agentruntimes/finalizers` | update | Add/remove `rossoctl.io/cleanup` finalizer for graceful deletion |
| `apps` | `deployments`, `statefulsets` | get, list, watch, update, patch | Resolve targetRef, apply labels (`rossoctl.io/type`, `managed-by`) and config-hash annotation |
| `""` (core) | `configmaps` | get, list, watch | Read cluster defaults (`rossoctl-platform-config`), feature gates (`rossoctl-feature-gates`), and namespace defaults |
| `""` (core) | `namespaces` | get, list, watch, patch | Read and label namespaces for Istio ambient mesh enrollment |
| `""` (core) | `pods` | get, list, watch | Count configured pods and verify ownership chains |
| `""` (core) | `events` | create, patch | Record reconciliation events (TargetNotFound, ConfigWarning, Configured) |

#### AgentCard Controller Permissions

Source: `internal/controller/agentcard_controller.go`

| API Group | Resources | Verbs | Purpose |
|-----------|-----------|-------|---------|
| `agent.rossoctl.dev` | `agentcards` | get, list, watch, create, update, patch, delete | Full lifecycle management |
| `agent.rossoctl.dev` | `agentcards/status` | get, update, patch | Store cached card data, sync conditions, signature results |
| `agent.rossoctl.dev` | `agentcards/finalizers` | update | Finalizer management |
| `apps` | `deployments`, `statefulsets` | get, list, watch, update, patch | Resolve targetRef and propagate signature labels |
| `""` (core) | `services` | get, list, watch | Construct service URLs for agent card fetching |
| `""` (core) | `configmaps` | get, list, watch | Read SPIRE trust bundle for signature verification |

#### AgentCard NetworkPolicy Controller Permissions

Source: `internal/controller/agentcard_networkpolicy_controller.go`

| API Group | Resources | Verbs | Purpose |
|-----------|-----------|-------|---------|
| `networking.k8s.io` | `networkpolicies` | get, list, watch, create, update, patch, delete | Create permissive/restrictive NetworkPolicies based on signature verification |
| `""` (core) | `pods` | get, list, watch, update, patch | Resolve pod selectors from workload pod template labels |

#### SPIRE Operand Controller Permissions

Source: `internal/controller/spire_operand_controller.go`

| API Group | Resources | Verbs | Purpose |
|-----------|-----------|-------|---------|
| `operator.openshift.io` | `zerotrustworkloadidentitymanagers` | create, get, list, update, watch | Create and reconcile ZTWIM parent CR |
| `operator.openshift.io` | `spiffecsidrivers` | create, get, list, update, watch | Create and reconcile SpiffeCSIDriver CR |
| `operator.openshift.io` | `spireservers` | create, get, list, update, watch | Create and reconcile SpireServer CR |
| `operator.openshift.io` | `spireagents` | create, get, list, update, watch | Create and reconcile SpireAgent CR |
| `operator.openshift.io` | `spireoidcdiscoveryproviders` | create, get, list, update, watch | Create and reconcile SpireOIDCDiscoveryProvider CR |

#### Cross-Namespace Considerations

- The operator uses a **ClusterRole** and **ClusterRoleBinding** in cluster-wide mode, allowing it to reconcile resources across all namespaces
- In **namespaced mode** (`NAMESPACES2WATCH` env var), the same permissions are scoped to specific namespaces via **Role** and **RoleBinding**
- The AgentRuntime controller reads ConfigMaps from `rossoctl-system` (cluster defaults) regardless of mode — this requires cross-namespace read access
- Namespace defaults ConfigMaps are read from the workload's own namespace

### Admission Control — Agent Label Protection

The operator deploys a `ValidatingAdmissionPolicy` (VAP) that prevents direct application of the `rossoctl.io/type` label on Deployments and StatefulSets. This label is the entry point for the entire rossoctl platform (webhook injection, agent discovery, client registration), so it must only be set through the official enrollment path — creating an AgentRuntime CR.

#### How It Works

| Layer | Purpose |
|-------|---------|
| **matchConstraints** | Targets CREATE and UPDATE of `apps/v1` Deployments and StatefulSets |
| **matchConditions** | Skips evaluation when the object doesn't have `rossoctl.io/type` or when the request comes from the operator's service account |
| **validation** | On UPDATE, allows the request only if `rossoctl.io/type` was already present with the same value (user is modifying other fields). On CREATE, always rejects since the label should not be set manually. |

#### Scenarios

| Action | Result |
|--------|--------|
| User creates Deployment with `rossoctl.io/type: agent` | **Rejected** — create an AgentRuntime instead |
| User adds `rossoctl.io/type` to existing Deployment | **Rejected** — create an AgentRuntime instead |
| User changes `rossoctl.io/type` from `agent` to `tool` | **Rejected** — update the AgentRuntime instead |
| User updates Deployment that already has the label (label unchanged) | **Allowed** |
| User removes `rossoctl.io/type` from Deployment | **Allowed** (matchCondition skips — new object has no label) |
| Operator controller applies label via AgentRuntime | **Allowed** (service account exemption) |

#### Resources

The VAP is deployed as part of the operator's kustomize manifests (`config/vap/`):

- `ValidatingAdmissionPolicy` — `agent-label-protection`
- `ValidatingAdmissionPolicyBinding` — binds with `validationActions: [Deny]`

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
  name: rossoctl-operator-manager-role
rules:
  - apiGroups: ["agent.rossoctl.dev"]
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
  name: rossoctl-operator-manager-role
  namespace: team1
rules:
  - apiGroups: ["agent.rossoctl.dev"]
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
