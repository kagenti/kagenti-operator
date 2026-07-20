# Migration Guide: Agent CRD to Workload-Based Agents

This guide describes how to migrate from the legacy `Agent` CRD to workload-based agents (Deployments and StatefulSets).

## Overview

The Rossoctl operator is transitioning from the custom `Agent` CRD to standard Kubernetes workloads (Deployments and StatefulSets) for deploying agents. This provides better alignment with Kubernetes best practices and enables more flexible deployment options.

## Migration Timeline

| Phase | Description | Action Required |
|-------|-------------|-----------------|
| **Current** | Hybrid mode - both Agent CRD and workloads supported | Optional migration |
| **Future** | Agent CRD deprecated | Migrate remaining agents |
| **Final** | Agent CRD removed | Complete migration required |

## Prerequisites

- Rossoctl operator v2.x or later
- Access to modify workload manifests
- Understanding of your current Agent CRD configurations

## Step-by-Step Migration

### Step 1: Identify Agents to Migrate

List all Agent CRDs in your cluster:

```bash
kubectl get agents -A
```

For each agent, note:
- Name and namespace
- Labels (especially `rossoctl.io/type` and protocol labels)
- PodTemplateSpec configuration
- Service configuration

### Step 2: Create Replacement Workload

For each Agent CRD, create a corresponding Deployment or StatefulSet.

#### Example: Migrating an Agent CRD to a Deployment

**Before (Agent CRD):**

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: Agent
metadata:
  name: my-agent
  namespace: default
  labels:
    rossoctl.io/type: agent
    rossoctl.io/agent-protocol: a2a  # Old label
spec:
  podTemplateSpec:
    spec:
      containers:
        - name: agent
          image: my-agent:latest
          ports:
            - containerPort: 8000
```

**After (Deployment):**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-agent
  namespace: default
  labels:
    app.kubernetes.io/name: my-agent
    rossoctl.io/type: agent
    protocol.rossoctl.io/a2a: ""  # New multi-protocol label
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: my-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: my-agent
        rossoctl.io/type: agent
    spec:
      containers:
        - name: agent
          image: my-agent:latest
          ports:
            - containerPort: 8000
---
apiVersion: v1
kind: Service
metadata:
  name: my-agent
  namespace: default
spec:
  selector:
    app.kubernetes.io/name: my-agent
  ports:
    - port: 8000
      targetPort: 8000
```

### Step 3: Update AgentCard (if manually created)

If you have manually created AgentCards using the `selector` field, migrate them to use `targetRef`:

**Before (selector-based):**

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: my-agent-card
spec:
  syncPeriod: "30s"
  selector:
    matchLabels:
      app.kubernetes.io/name: my-agent
      rossoctl.io/type: agent
```

**After (targetRef-based):**

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: my-agent-card
spec:
  syncPeriod: "30s"
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-agent
```

### Step 4: Verify the Migration

1. Check that the new Deployment/StatefulSet is running:

   ```bash
   kubectl get deployment my-agent -n default
   ```

2. Verify the AgentCard was automatically created (or updated):

   ```bash
   kubectl get agentcard -n default
   ```

3. Check the AgentCard status:

   ```bash
   kubectl describe agentcard my-agent-deployment-card -n default
   ```

   Look for:
   - `Synced` condition is `True`
   - `status.targetRef` shows the correct workload reference
   - `status.card` contains the fetched agent card data

### Step 5: Delete the Old Agent CRD

Once verified, delete the old Agent CRD:

```bash
kubectl delete agent my-agent -n default
```

## Label Migration

Update your labels to use the new multi-protocol format:

| Old Label | New Label |
|-----------|-----------|
| `rossoctl.io/agent-protocol: a2a` | `protocol.rossoctl.io/a2a: ""` |
| `rossoctl.io/protocol: a2a` | `protocol.rossoctl.io/a2a: ""` |

Both old label formats are still supported for backward compatibility, but they will be removed in a future release. The new prefix-based format allows a single workload to declare support for multiple protocols simultaneously.

### Required Labels for Workloads

| Label | Value | Required |
|-------|-------|----------|
| `rossoctl.io/type` | `agent` | Yes |
| `protocol.rossoctl.io/<name>` | `""` (existence implies support) | Yes (at least one) |
| `app.kubernetes.io/name` | `<agent-name>` | Recommended |

## AgentCard Naming Convention

When migrating to workloads, AgentCards are automatically created with the following naming pattern:

```
<workload-name>-<workload-kind-lowercase>-card
```

Examples:
- Deployment `my-agent` → AgentCard `my-agent-deployment-card`
- StatefulSet `my-agent` → AgentCard `my-agent-statefulset-card`

This prevents naming collisions when the same agent name exists as different workload types.

## Disabling Legacy Agent CRD Support

After migrating all agents, you can disable legacy Agent CRD support by setting the operator flag:

```bash
--enable-legacy-agent-crd=false
```

This will:
- Stop watching Agent CRD resources
- Disable the AgentCardSync controller for Agent CRDs
- Reduce resource usage and simplify the operator

### Helm Configuration

If using Helm, update your values:

```yaml
operator:
  args:
    - --enable-legacy-agent-crd=false
```

## Troubleshooting

### AgentCard not created for my Deployment

Ensure your Deployment has the required labels:

```yaml
metadata:
  labels:
    rossoctl.io/type: agent
    rossoctl.io/protocol: a2a  # or mcp, etc.
```

### AgentCard shows "WorkloadNotFound"

Check that:
1. The `targetRef` in the AgentCard points to the correct workload
2. The workload exists in the same namespace as the AgentCard
3. The workload has the `rossoctl.io/type: agent` label

### Deprecation warnings in logs

If you see deprecation warnings, update your configuration:

1. **"AgentCard uses deprecated 'selector' field"**
   - Migrate to `targetRef` as shown in Step 3

2. **"workload uses deprecated label 'rossoctl.io/agent-protocol'"**
   - Update to `rossoctl.io/protocol`

## Rollback

If you encounter issues during migration:

1. Re-enable legacy Agent CRD support:
   ```bash
   --enable-legacy-agent-crd=true
   ```

2. Recreate the Agent CRD from your backup

3. Delete the new Deployment/StatefulSet if needed

The operator supports running in hybrid mode indefinitely, so you can migrate gradually.

## Support

For questions or issues:
- Open an issue at https://github.com/rossoctl/operator/issues
- Refer to the full migration plan in the project documentation.
