# Dynamic Agent Discovery

This document describes the Rossoctl Operator's dynamic agent discovery feature, which enables Kubernetes-native discovery and introspection of deployed AI agents.

## Overview

Dynamic agent discovery solves a fundamental challenge in multi-agent systems: **how do agents find and understand each other at runtime?** The AgentCard Custom Resource provides a Kubernetes-native solution that automatically indexes agent metadata from running agents, making discovery as simple as querying Kubernetes resources.

## Problem Statement

Agent discovery followed by invocation is a complex problem in agentic systems. Rossoctl provides a straightforward, durable, and Kubernetes-native solution that:

- Works with existing Kubernetes tooling (`kubectl`, `oc`)
- Requires no external registry or service
- Integrates seamlessly with agent lifecycle management
- Provides near real-time discovery without polling agents directly

## Architecture

### AgentCard CRD

The `AgentCard` Custom Resource serves as a lightweight catalog that stores agent metadata fetched from deployed agents. It uses `spec.targetRef` to reference the backing workload (Deployment or StatefulSet) and maintains owner references for automatic cleanup.

```
Deployment/StatefulSet → AgentCard (Discovery Index)
     ↓                         ↓
  Runs A2A Agent        Stores Agent Card Data
     ↓                         ↓
Exposes /.well-known/    Enables kubectl get agentcards
  agent.json
```

### Key Design Decisions

1. **Deployment-First**: Users create standard Kubernetes workloads; AgentCard handles discovery via `targetRef`
2. **Owner References**: AgentCards use owner references pointing to their workload, enabling automatic cleanup
3. **Protocol-Based**: Currently supports A2A protocol, extensible to other protocols
4. **Status-Based Storage**: Agent metadata is stored in status fields (observed state) rather than spec (desired state)
5. **Namespace-Scoped**: AgentCards are namespace-scoped for better security and access control

## How It Works

### 1. Workload Enrollment via AgentRuntime

Agent workloads (Deployments or StatefulSets) are enrolled by creating an `AgentRuntime` CR. The operator's controller applies the `rossoctl.io/type` label automatically — a `ValidatingAdmissionPolicy` prevents setting it directly on workloads.

The workload itself carries a protocol label to declare which protocol it speaks:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: weather-agent
  labels:
    protocol.rossoctl.io/a2a: ""        # Speaks A2A protocol
    app.kubernetes.io/name: weather-agent
spec:
  # ... standard Deployment spec
---
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-agent
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
```

Multiple protocols can be declared simultaneously on the Deployment:

```yaml
  labels:
    protocol.rossoctl.io/a2a: ""        # Speaks A2A
    protocol.rossoctl.io/mcp: ""        # Also speaks MCP
```

### 2. AgentCard Creation

When a workload with appropriate labels is deployed, an AgentCard is automatically created by the AgentCardSync controller, or users can create one explicitly with `targetRef`:

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: weather-agent-deployment-card
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
```

### 3. Periodic Synchronization

The AgentCard controller:

1. Resolves the backing workload via `spec.targetRef`
2. Constructs the service URL for the agent
3. Fetches the agent card from `/.well-known/agent-card.json`
4. Optionally verifies the card signature
5. Parses and stores the card data in the AgentCard status
6. Updates conditions to reflect sync status
7. Repeats periodically based on `syncPeriod`

### 4. Discovery via kubectl

Once synchronized, agent cards are discoverable via standard Kubernetes commands:

```bash
# List all agent cards
kubectl get agentcards

# Output:
# NAME                 PROTOCOL   CARD_NAME           SYNCED   LASTSYNC   AGE
# weather-agent-card   a2a        Weather Assistant   True     5m         10m
# (CARD_NAME column displays AgentCard status.card.name, not the Agent resource name)

# Get detailed information
kubectl describe agentcard weather-agent-card
```

## Benefits

### 1. Zero-Cost Discovery

Agent discovery leverages Kubernetes labels—no external registry or service required. Discovery is essentially "free" from an infrastructure perspective.

### 2. Lifecycle Awareness

Agent cards exist throughout the agent's lifecycle, even during restarts. This is valuable in adaptive systems where knowing an agent exists (even if temporarily unavailable) is preferable to discovering it only when ready.

### 3. Change Detection

Periodic synchronization creates opportunities for:
- Detecting signature changes
- Tracking capability modifications
- Monitoring endpoint updates
- Flagging unexpected agent behavior

### 4. Developer-Friendly

Discovery integrates naturally with existing workflows:
- `kubectl get agentcards` to list agents
- `kubectl describe agentcard <name>` for details
- Standard RBAC for access control
- Familiar Kubernetes tooling

### 5. Multi-Agent Systems

Dynamic discovery enables sophisticated multi-agent patterns:
- Agents can discover peers at runtime
- Systems can adapt to agent availability
- No hardcoded dependencies between agents
- Support for dynamic agent topologies

## A2A Protocol Support

The current implementation focuses on the [A2A (Agent-to-Agent)](https://a2a-protocol.org/) protocol, which provides:

- Standardized agent card format
- Well-defined discovery endpoint (`/.well-known/agent-card.json`)
- Rich metadata about agent capabilities
- Skill and parameter descriptions

### Agent Card Structure

A typical A2A agent card includes:

```json
{
  "name": "Weather Assistant",
  "description": "Provides weather information",
  "version": "1.0.0",
  "url": "http://weather-agent.default.svc.cluster.local:8000",
  "capabilities": {
    "streaming": true
  },
  "defaultInputModes": ["text"],
  "defaultOutputModes": ["text"],
  "skills": [
    {
      "name": "get-weather",
      "description": "Get current weather for a city",
      "parameters": [
        {
          "name": "city",
          "type": "string",
          "required": true,
          "description": "City name"
        }
      ]
    }
  ]
}
```

This data is automatically fetched and stored in the AgentCard status.

## Usage Examples

### Deploy an Agent with Discovery

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: assistant-agent
  labels:
    protocol.rossoctl.io/a2a: ""
    app.kubernetes.io/name: assistant-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: assistant-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: assistant-agent
    spec:
      containers:
      - name: agent
        image: "ghcr.io/myorg/assistant:v1.0.0"
        ports:
        - containerPort: 8000
---
apiVersion: v1
kind: Service
metadata:
  name: assistant-agent
spec:
  selector:
    app.kubernetes.io/name: assistant-agent
  ports:
    - name: http
      port: 8000
      targetPort: 8000
---
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: assistant-agent
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: assistant-agent
```

The operator applies the `rossoctl.io/type: agent` label to the Deployment, and the AgentCard is created automatically by the AgentCardSync controller when both `rossoctl.io/type` and a protocol label are present.

### Query Agent Cards

```bash
# List all agent cards
kubectl get agentcards

# Get agent card in YAML
kubectl get agentcard assistant-agent-card -o yaml

# Watch for changes
kubectl get agentcards -w

# Filter by protocol
kubectl get agentcards -l protocol.rossoctl.io/a2a
```

### Access Agent Metadata

```bash
# Get agent name
kubectl get agentcard assistant-agent-card -o jsonpath='{.status.card.name}'

# List agent skills
kubectl get agentcard assistant-agent-card -o jsonpath='{.status.card.skills[*].name}'

# Check sync status
kubectl get agentcard assistant-agent-card -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}'
```

## Integration with MCP Bridge

The AgentCard system is designed to work with an MCP (Model Context Protocol) Bridge that:

1. Queries AgentCards to discover available agents
2. Exposes agent capabilities through MCP tools
3. Routes invocations to the appropriate agents
4. Provides a unified interface for agent interaction

This creates a simple entry point for:
- Developers debugging multi-agent systems
- Agents discovering and invoking peers
- Dynamic multi-agent orchestration

See [feature: Add an A2A MCP Bridge](https://github.com/rossoctl/examples/issues/84) for details.

## Status Conditions

AgentCards maintain two primary conditions:

### Synced Condition

Indicates whether the agent card was successfully fetched:

```yaml
conditions:
- type: Synced
  status: "True"
  reason: SyncSucceeded
  message: "Successfully fetched agent card for Weather Assistant"
  lastTransitionTime: "2024-01-15T10:30:00Z"
```

Reasons:
- `SyncSucceeded`: Card fetched successfully
- `SyncFailed`: Unable to fetch card
- `AgentNotReady`: Agent not ready yet
- `ProtocolNotSupported`: Unsupported protocol
- `InvalidCard`: Card format invalid

### Ready Condition

Indicates whether the agent card is ready for queries:

```yaml
conditions:
- type: Ready
  status: "True"
  reason: ReadyToServe
  message: "Agent index is ready for queries"
  lastTransitionTime: "YYYY-MM-DDTHH:MM:SSZ"
```

## Security Considerations

### CardSignature Verification

The operator supports A2A agent card signature verification. When enabled (`--require-a2a-signature`):

1. Public keys are stored in Kubernetes Secrets or fetched via JWKS endpoints
2. Card signatures are verified during each sync cycle
3. Unsigned or invalid cards are flagged in status
4. Identity binding can be configured via `spec.identityBinding` with SPIFFE ID allowlists

### RBAC

AgentCards respect Kubernetes RBAC:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-discoverer
rules:
- apiGroups: ["agent.rossoctl.dev"]
  resources: ["agentcards"]
  verbs: ["get", "list", "watch"]
```

This allows fine-grained control over who can discover agents.

### Namespace Isolation

AgentCards are namespace-scoped, preventing agents from discovering agents in other namespaces unless explicitly granted cluster-wide permissions.

## Troubleshooting

### AgentCard Not Syncing

Check the workload:
```bash
kubectl get deployment weather-agent -o yaml
kubectl logs -l app.kubernetes.io/name=weather-agent
```

Check the AgentCard conditions:
```bash
kubectl describe agentcard weather-agent-deployment-card
```

Common issues:
- Workload not ready (pods not running)
- Service not accessible
- Agent card endpoint not implemented
- Protocol label missing or incorrect
- `targetRef` pointing to wrong workload

### Stale AgentCard Data

AgentCards sync periodically. To force a sync, delete and recreate:
```bash
kubectl delete agentcard weather-agent-card
# Controller will recreate automatically
```

Or update the syncPeriod to sync more frequently.

### Orphaned AgentCards

If a workload is deleted, its AgentCard should be automatically cleaned up via owner references. If orphaned cards persist:

```bash
# Find orphaned cards
kubectl get agentcards --all-namespaces

# Manually clean up
kubectl delete agentcard <orphaned-card-name>
```

## Future Enhancements

1. **CardSignature Verification**: Verify agent identity through signed cards
2. **Additional Protocols**: Support for protocols beyond A2A
3. **Cross-Namespace Discovery**: Controlled discovery across namespaces
4. **OpenShift Console Plugin**: UI for visualizing discovered agents
5. **Aggregated Views**: Group and query agents by capability or team
6. **Health Monitoring**: Track agent availability and response times

## Related Resources

- [API Reference - AgentCard](./api-reference.md#agentcard)
- [Architecture Documentation](./architecture.md)
- [A2A Protocol Specification](https://a2a-protocol.org/dev/specification/)
- [GitHub Issue #303](https://github.com/rossoctl/rossoctl/issues/303)
- [Pull Request #114](https://github.com/rossoctl/operator/pull/114)
