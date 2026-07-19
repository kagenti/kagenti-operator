# API Reference

This document provides a comprehensive reference for the Rossoctl Operator Custom Resource Definitions (CRDs).

## Custom Resources

- [AgentCard](#agentcard) — Fetches and stores agent metadata for dynamic discovery
- [AgentRuntime](#agentruntime) — Configures identity and observability for agent/tool workloads

---

## AgentCard

The `AgentCard` Custom Resource stores agent metadata for dynamic discovery and introspection. It synchronizes agent card data from deployed agents that implement supported protocols (currently A2A).

### API Group and Version

- **API Group:** `agent.rossoctl.dev`
- **API Version:** `v1alpha1`
- **Kind:** `AgentCard`
- **Short Names:** `agentcards`, `cards`

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `syncPeriod` | string | No | How often to re-fetch the agent card (default: "30s", format: "30s", "5m", etc.) |
| `targetRef` | [TargetRef](#targetref) | Yes | Identifies the workload backing this agent |
| `identityBinding` | [IdentityBinding](#identitybinding) | No | SPIFFE identity binding configuration |

#### TargetRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | Yes | API version of the target resource (e.g., "apps/v1") |
| `kind` | string | Yes | Kind of the target resource (e.g., "Deployment", "StatefulSet") |
| `name` | string | Yes | Name of the target resource |

#### IdentityBinding

Configures workload identity binding for an AgentCard. The SPIFFE ID is extracted from the leaf certificate's SAN URI in the `x5c` chain during signature verification.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `trustDomain` | string | No | Overrides the operator-level `--spire-trust-domain` for this AgentCard. If empty, the operator flag value is used. |
| `strict` | boolean | No | Enables enforcement mode: binding failures trigger network isolation. When false (default), results are recorded in status only (audit mode). |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `card` | [AgentCardData](#agentcarddata) | Cached agent card data from the agent |
| `conditions` | [][Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) | Current state of indexing process |
| `lastSyncTime` | timestamp | When the agent card was last successfully fetched |
| `protocol` | string | Detected agent protocol (e.g., "a2a") |
| `targetRef` | [TargetRef](#targetref) | Resolved reference to the backing workload |
| `validSignature` | boolean | Whether the agent card JWS signature is valid |
| `signatureVerificationDetails` | string | Human-readable details about the last signature verification |
| `signatureKeyId` | string | Key ID (`kid`) from the JWS protected header |
| `signatureSpiffeId` | string | SPIFFE ID from the JWS protected header (set only when signature is valid) |
| `signatureIdentityMatch` | boolean | `true` when both signature verification AND identity binding pass |
| `cardId` | string | SHA256 hash of card content for drift detection |
| `expectedSpiffeID` | string | SPIFFE ID used for binding evaluation |
| `bindingStatus` | [BindingStatus](#bindingstatus) | Result of identity binding evaluation |

#### BindingStatus

| Field | Type | Description |
|-------|------|-------------|
| `bound` | boolean | Whether the verified SPIFFE ID is in the allowlist |
| `reason` | string | Machine-readable reason (`Bound`, `NotBound`, `AgentNotFound`) |
| `message` | string | Human-readable description |
| `lastEvaluationTime` | timestamp | When the binding was last evaluated |

#### AgentCardData

Represents the A2A agent card structure based on the [A2A specification](https://a2a-protocol.org/).

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Human-readable name of the agent |
| `description` | string | What the agent does |
| `version` | string | Agent version |
| `url` | string | Endpoint where the agent can be reached |
| `documentationUrl` | string | Link to the agent's documentation |
| `iconUrl` | string | Link to the agent's icon image |
| `provider` | [AgentProvider](#agentprovider) | Organization providing the agent |
| `capabilities` | [AgentCapabilities](#agentcapabilities) | Supported A2A features |
| `defaultInputModes` | []string | Default media types the agent accepts |
| `defaultOutputModes` | []string | Default media types the agent produces |
| `skills` | [][AgentSkill](#agentskill) | Skills/capabilities offered by the agent |
| `supportsAuthenticatedExtendedCard` | boolean | Whether agent has an extended card |
| `signatures` | [][AgentCardSignature](#agentcardsignature) | JWS signatures per A2A spec section 8.4.2 |

#### AgentProvider

| Field | Type | Description |
|-------|------|-------------|
| `organization` | string | Name of the provider organization |
| `url` | string | Provider's website |

#### AgentCapabilities

| Field | Type | Description |
|-------|------|-------------|
| `streaming` | boolean | Whether the agent supports streaming responses |
| `pushNotifications` | boolean | Whether the agent supports push notifications |
| `extensions` | [][AgentExtension](#agentextension) | A2A protocol extensions supported by the agent |

#### AgentExtension

| Field | Type | Description |
|-------|------|-------------|
| `uri` | string | Unique identifier for this extension |
| `description` | string | What this extension does |
| `required` | boolean | Whether this extension must be supported by the client |
| `params` | map[string]JSON | Extension-specific configuration parameters |

#### AgentSkill

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier for this skill |
| `name` | string | Skill name |
| `description` | string | What this skill does |
| `tags` | []string | Keywords describing classes of capabilities for this skill |
| `examples` | []string | Sample scenarios demonstrating how the skill can be used |
| `inputModes` | []string | Media types this skill accepts |
| `outputModes` | []string | Media types this skill produces |
| `parameters` | [][SkillParameter](#skillparameter) | Parameters this skill accepts |

#### SkillParameter

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Parameter name |
| `type` | string | Parameter type (e.g., "string", "number", "boolean") |
| `description` | string | What this parameter is for |
| `required` | boolean | Whether this parameter must be provided |
| `default` | string | Default value for this parameter |

#### AgentCardSignature

| Field | Type | Description |
|-------|------|-------------|
| `protected` | string | Base64url-encoded JWS protected header (contains `alg`, `kid`, `typ`, `x5c`) |
| `signature` | string | Base64url-encoded JWS signature value |
| `header` | object | Optional unprotected JWS header parameters (e.g., `timestamp`) |

### Examples

#### Deploy Agent with AgentRuntime (Recommended)

Create a Deployment with a protocol label and an `AgentRuntime` CR to enroll it. The operator applies `rossoctl.io/type: agent` to the Deployment, and the AgentCardSync controller auto-creates an AgentCard named `weather-agent-deployment-card` (pattern: `{name}-{kind}-card`):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: weather-agent
  namespace: default
  labels:
    app.kubernetes.io/name: weather-agent
    protocol.rossoctl.io/a2a: ""
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: weather-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: weather-agent
    spec:
      containers:
      - name: agent
        image: "ghcr.io/rossoctl/examples/weather_service:v0.0.1-alpha.3"
        ports:
        - containerPort: 8000
        env:
        - name: PORT
          value: "8000"
---
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-agent
  namespace: default
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
```

#### Manual AgentCard with Identity Binding

If you need `identityBinding.strict: true` (enforcement mode), create the AgentCard before the workload is labeled. The sync controller will skip auto-creation when it finds an existing card targeting the same workload:

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: weather-agent-deployment-card
  namespace: default
spec:
  syncPeriod: "30s"
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
  identityBinding:
    strict: true
```

#### View Discovered Agents

```bash
# List all agent cards
kubectl get agentcards

# Example output:
# NAME                        PROTOCOL   KIND         TARGET          AGENT             SYNCED   LASTSYNC   AGE
# weather-agent-deployment-card   a2a    Deployment   weather-agent   Weather Assistant  True     5m         10m

# Get detailed information
kubectl describe agentcard weather-agent-deployment-card
```

#### AgentCard Status Example

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: weather-agent-deployment-card
  namespace: default
  ownerReferences:
    - apiVersion: apps/v1
      kind: Deployment
      name: weather-agent
      controller: true
spec:
  syncPeriod: 30s
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
status:
  protocol: a2a
  lastSyncTime: "2025-12-19T10:30:00Z"
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent

  card:
    name: "Weather Assistant"
    description: "Provides weather information using MCP tools"
    version: "1.0.0"
    url: "http://weather-agent.default.svc.cluster.local:8000"

    capabilities:
      streaming: true
      pushNotifications: false

    defaultInputModes:
      - text
    defaultOutputModes:
      - text

    skills:
      - id: "get-weather-001"
        name: "get-weather"
        description: "Get current weather for a city"
        tags:
          - weather
          - forecast
        examples:
          - "What is the weather in New York?"
          - "Get the forecast for London"
        inputModes:
          - text
        outputModes:
          - text
        parameters:
          - name: "city"
            type: "string"
            description: "City name to get weather for"
            required: true

  conditions:
    - type: Synced
      status: "True"
      reason: SyncSucceeded
      message: "Successfully fetched agent card for Weather Assistant"
      lastTransitionTime: "2025-12-19T10:30:00Z"
    - type: Ready
      status: "True"
      reason: ReadyToServe
      message: "Agent index is ready for queries"
      lastTransitionTime: "2025-12-19T10:30:00Z"
```

#### Query Agent Metadata

```bash
# Get agent name from card
kubectl get agentcard weather-agent-deployment-card \
  -o jsonpath='{.status.card.name}'

# List all skills
kubectl get agentcard weather-agent-deployment-card \
  -o jsonpath='{.status.card.skills[*].name}'

# Get agent endpoint
kubectl get agentcard weather-agent-deployment-card \
  -o jsonpath='{.status.card.url}'

# Check signature verification
kubectl get agentcard weather-agent-deployment-card \
  -o jsonpath='{.status.validSignature}'

# Check identity binding
kubectl get agentcard weather-agent-deployment-card \
  -o jsonpath='{.status.bindingStatus.bound}'
```

---

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: custom-agent-card
  namespace: default
spec:
  syncPeriod: "5m"  # Sync every 5 minutes instead of default 30s
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: custom-agent
```

### Common Status Conditions

#### AgentCard Conditions

| Type | Status | Reason | Description |
|------|--------|--------|-------------|
| `Synced` | `True` | `SyncSucceeded` | Agent card fetched successfully |
| `Synced` | `False` | `WorkloadNotFound` | Referenced workload does not exist |
| `Synced` | `False` | `WorkloadNotReady` | Workload is not ready to serve |
| `Synced` | `False` | `NoProtocol` | Workload missing `protocol.rossoctl.io/<name>` label |
| `Synced` | `False` | `FetchFailed` | Failed to fetch agent card from endpoint |
| `Synced` | `False` | `SignatureInvalid` | Signature verification failed (enforce mode) |
| `Ready` | `True` | `ReadyToServe` | Agent index ready for queries |
| `SignatureVerified` | `True` | `SignatureValid` | JWS signature verified successfully |
| `SignatureVerified` | `False` | `SignatureInvalid` | JWS signature verification failed |
| `Bound` | `True` | `Bound` | SPIFFE ID is in the allowlist |
| `Bound` | `False` | `NotBound` | SPIFFE ID is not in the allowlist |

---

## Required Labels for Workload-Based Agents

For Deployments and StatefulSets to be automatically discovered by the operator, the following labels are needed on the workload. `rossoctl.io/type` is applied by the operator via an AgentRuntime CR (a `ValidatingAdmissionPolicy` prevents setting it directly). Protocol labels are set by the user:

| Label | Value | Set By | Description |
|-------|-------|--------|-------------|
| `rossoctl.io/type` | `agent` or `tool` | Operator (via AgentRuntime) | Classifies the workload. Applied automatically when an AgentRuntime CR targets it. |
| `protocol.rossoctl.io/<name>` | `""` (existence implies support) | User | Protocol(s) the agent speaks (e.g., `protocol.rossoctl.io/a2a`, `protocol.rossoctl.io/mcp`). Required for AgentCard auto-creation. |
| `app.kubernetes.io/name` | `<agent-name>` | User | Standard Kubernetes app name label (recommended) |

---

## AgentRuntime

The `AgentRuntime` Custom Resource configures identity (SPIFFE) for agent and tool workloads. Unlike AgentCard, which handles discovery and metadata fetching, AgentRuntime provides runtime configuration for workload identity.

### API Group and Version

- **API Group:** `agent.rossoctl.dev`
- **API Version:** `v1alpha1`
- **Kind:** `AgentRuntime`
- **Short Names:** `art`, `agentrt`

### Relationship to AgentCard

AgentRuntime and AgentCard serve complementary purposes:

- **AgentCard**: Fetches and stores agent metadata (capabilities, skills, endpoints) for dynamic discovery. Handles signature verification and identity binding validation.
- **AgentRuntime**: Configures identity (SPIFFE trust domain) for running workloads.

Both resources use the shared `TargetRef` type to reference the backing workload (Deployment, StatefulSet, etc.).

### Configuration Precedence

The controller computes the config hash from two platform layers (highest priority wins):

1. **Namespace defaults** — ConfigMap with `rossoctl.io/defaults=true` label in the workload's namespace
2. **Cluster defaults** — `rossoctl-platform-config` ConfigMap in `rossoctl-system`

> **Note:** Per-CR overrides (authBridgeMode, mtlsMode) are **not** included in the controller's config hash. The webhook reads these fields at pod CREATE time.

> **Note:** Feature gates (`rossoctl-feature-gates`) are platform-wide policy and are **not** overrideable by namespace defaults or AgentRuntime CRs. They control which AuthBridge components (envoy proxy, SPIFFE helper, client registration) are enabled globally, and whether skill discovery (`skillDiscovery`) is active.

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | Yes | Classifies the workload as `agent` or `tool` |
| `targetRef` | [TargetRef](#targetref) | Yes | Identifies the workload backing this runtime (uses the same TargetRef type as AgentCard) |
### Labels and Annotations Applied to Target Workloads

The AgentRuntime controller applies the following labels and annotations to the target workload:

**Workload metadata labels:**

| Label | Value | Description |
|-------|-------|-------------|
| `rossoctl.io/type` | `agent` or `tool` | Classifies the workload (from `spec.type`) |
| `app.kubernetes.io/managed-by` | `rossoctl-operator` | Indicates the workload is managed by the operator. Removed on AgentRuntime deletion. |

**Workload metadata annotations:**

| Annotation | Value | Description |
|------------|-------|-------------|
| `rossoctl.io/skills` | JSON array of skill names | Read by the operator (not set by it) to discover linked skills. Set by the rossoctl backend or the user. Value is a JSON array (e.g., `["weather-forecast","resume-reviewer"]`). Populates `status.linkedSkills` when the `skillDiscovery` feature gate is enabled. |

**PodTemplateSpec labels:**

| Label | Value | Description |
|-------|-------|-------------|
| `rossoctl.io/type` | `agent` or `tool` | Classifies pods spawned by this workload |

**PodTemplateSpec annotations:**

| Annotation | Value | Description |
|------------|-------|-------------|
| `rossoctl.io/config-hash` | SHA-256 hex string | Deterministic hash of the resolved configuration. Changes trigger rolling updates. |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | int64 | Most recent generation observed by the controller |
| `configuredPods` | int32 | Count of pods with expected labels/configuration |
| `conditions` | [][Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) | Current state of the AgentRuntime |

#### Conditions

| Condition | Status | Reason | Description |
|-----------|--------|--------|-------------|
| `TargetResolved` | True | `TargetFound` | Target Deployment/StatefulSet exists |
| `TargetResolved` | False | `TargetNotFound` | Target workload not found; controller requeues after 30s |
| `ConfigResolved` | True | `ConfigResolved` | Configuration merged successfully from all layers |
| `ConfigResolved` | True | `ConfigWarning` | Configuration merged but ambiguity detected (e.g., multiple namespace defaults ConfigMaps with `rossoctl.io/defaults=true`). The warning is surfaced in the condition message and as a Kubernetes event. |
| `Ready` | True | `Configured` | Labels and config-hash applied to the target workload |
| `Ready` | False | `ConfigHashError` | Failed to compute the config hash |
| `Ready` | False | `ConfigApplyError` | Failed to apply labels/annotations to the workload |
| `IstioMeshEnrolled` | True | `NamespaceLabeled` | Namespace labeled with `istio-discovery=enabled` and `istio.io/dataplane-mode=ambient` for Istio ambient mesh enrollment |
| `IstioMeshEnrolled` | False | `OptedOut` | Namespace has `rossoctl.io/istio-mesh=disabled` annotation; Istio mesh labels not applied |
| `IstioMeshEnrolled` | False | `PatchFailed` | Failed to patch namespace labels (e.g., RBAC misconfiguration). Non-fatal; reconcile continues. |
| `SkillsMounted` | True | `SkillsApplied` | OCI skill ImageVolumes applied to the target workload |
| `SkillsMounted` | False | `FeatureGateDisabled` | Skills defined but `skillImageVolumes` feature gate is disabled |
| `SkillsMounted` | False | `UnsupportedWorkloadKind` | Skills defined but the target workload kind (e.g., Sandbox) does not support skill ImageVolumes |

### Admission Validation

A validating webhook prevents ownership conflicts:

- **Duplicate targetRef rejection**: If an AgentRuntime CR already targets a given workload (same `apiVersion` + `kind` + `name`) in the same namespace, creating or updating another AgentRuntime to target the same workload is rejected at admission time.
- **Fail-open on API errors**: If the webhook's internal list call fails (e.g., transient API server error), the request is allowed through to avoid blocking deployments. Note: the Kubernetes-level `failurePolicy` is set to `Fail`, so if the webhook pod itself is unreachable, the API server will reject AgentRuntime creates/updates. This is consistent with the AgentCard webhook.

This prevents conflicting label updates where two AgentRuntime CRs with different `type` values (e.g., `agent` vs `tool`) would fight over the same workload's `rossoctl.io/type` label.

- **Skill validation**: Duplicate skill names and duplicate mount paths within the same AgentRuntime are rejected.

### Examples

#### Basic Agent Runtime

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-agent-runtime
  namespace: default
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
```

#### Agent Runtime with Identity Overrides

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-agent-runtime
  namespace: default
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
  identity:
    spiffe:
      trustDomain: custom.example.com
```

#### Tool Runtime

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: calculator-tool-runtime
  namespace: default
spec:
  type: tool
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: calculator-tool
```

#### Agent Runtime with Skill Discovery

When the `skillDiscovery` feature gate is enabled, the operator reads the `rossoctl.io/skills` annotation from the target workload and populates `status.linkedSkills`.

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: resume-agent-runtime
  namespace: default
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: resume-agent
```

To enable skill discovery:

```yaml
# In the rossoctl-feature-gates ConfigMap (rossoctl-system namespace)
# or via Helm values:
featureGates:
  skillDiscovery: true
```

### kubectl Usage Examples

```bash
# List all agent runtimes (using short name)
kubectl get art

# List agent runtimes with full name
kubectl get agentruntimes

# Example output:
# NAME                      TYPE    TARGET          READY   AGE
# weather-agent-runtime     agent   weather-agent   True    5m
# calculator-tool-runtime   tool    calculator-tool True    3m

# Get detailed information
kubectl describe agentruntime weather-agent-runtime

# View configured pods count
kubectl get art weather-agent-runtime -o jsonpath='{.status.configuredPods}'
```

---

## Additional Resources

- [Dynamic Agent Discovery](./dynamic-agent-discovery.md) — How AgentCard enables agent discovery
- [Signature Verification](./agentcard-signature-verification.md) — JWS signature verification setup
- [Identity Binding](./agentcard-identity-binding.md) — SPIFFE identity binding guide
- [Architecture Documentation](./architecture.md) — Operator design and components
- [Developer Guide](./dev.md) — Contributing and development
- [Getting Started Tutorial](../GETTING_STARTED.md) — Detailed tutorials and examples
