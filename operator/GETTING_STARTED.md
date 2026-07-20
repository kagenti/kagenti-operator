# Getting Started with Rossoctl Operator

> **Note**: This guide assumes you have already installed the Rossoctl platform on your cluster. For OpenShift, use [`scripts/ocp/setup-rossoctl.sh`](https://github.com/rossoctl/rossoctl/blob/main/scripts/ocp/setup-rossoctl.sh) — the recommended installer for OCP deployments. For plain Kubernetes, install the operator directly via Helm (see the [Quick Start](../README.md#quick-start) in the root README).

## Prerequisites

The examples in this guide assume you are running Ollama locally with a specific model loaded. Before proceeding, ensure you have:

- **Ollama installed and running** on your local machine
- **Model loaded**: `llama3.2:3b-instruct-fp16`
- **Ollama accessible** at `http://host.docker.internal:11434/v1` from within the Kubernetes cluster

The agent deployments in this guide are configured with the following environment variables to connect to your local Ollama instance:
```yaml
- name: LLM_API_BASE
  value: http://host.docker.internal:11434/v1
- name: LLM_API_KEY
  value: dummy
- name: LLM_MODEL
  value: llama3.2:3b-instruct-fp16
```

---

## Intent

This guide provides step-by-step instructions for deploying and testing AI agents with the Rossoctl platform. By following this guide, you will learn how to deploy agents and integrate MCP (Model Context Protocol) servers to extend agent capabilities.

## Scenario

In this guide, you will:

1. Deploy a weather agent that can answer weather-related queries
2. Deploy an MCP server that provides tools and resources to the agent
3. Test the end-to-end flow by sending a weather query ("What is the weather in New York?") to the agent, which will communicate with the MCP server and return a response

This scenario demonstrates the complete lifecycle of an AI agent deployment on the Rossoctl platform, from initial deployment to integration with external tools via MCP servers.

---
## Overview

### Rossoctl Operator
The Rossoctl Operator discovers, indexes, and secures AI agents deployed in Kubernetes. Enroll workloads by creating an `AgentRuntime` CR:

1. Create a Deployment with a `protocol.rossoctl.io/a2a` label
2. Create an `AgentRuntime` CR pointing to it
3. The controller applies `rossoctl.io/type` labels and triggers sidecar injection automatically
4. The protocol label enables automatic AgentCard creation for agent discovery

A `ValidatingAdmissionPolicy` prevents the `rossoctl.io/type` label from being set directly on Deployments or StatefulSets — it can only be applied by the operator via an AgentRuntime CR.

> **Note:** The `Agent` Custom Resource is deprecated and will be removed in a future release.

---

## Deploy an Agent with AgentRuntime (Recommended)

The AgentRuntime approach requires only a `protocol.rossoctl.io/a2a` label on your Deployment — the controller applies `rossoctl.io/type`, computes a config hash, and triggers the AuthBridge webhook to inject sidecars. The protocol label tells the AgentCard sync controller which protocol the agent speaks, enabling automatic discovery.

### Step 1: Deploy a Deployment with Protocol Label

```yaml
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: weather-agent
  namespace: team1
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
        imagePullPolicy: Always
        env:
        - name: PORT
          value: "8000"
        - name: UV_CACHE_DIR
          value: /app/.cache/uv
        - name: MCP_URL
          value: http://weather-tool-mcp.team1.svc.cluster.local:8000/mcp
        - name: LLM_API_BASE
          value: http://host.docker.internal:11434/v1
        - name: LLM_API_KEY
          value: dummy
        - name: LLM_MODEL
          value: llama3.2:3b-instruct-fp16
---
apiVersion: v1
kind: Service
metadata:
  name: weather-agent
  namespace: team1
spec:
  selector:
    app.kubernetes.io/name: weather-agent
  ports:
  - name: http
    port: 8000
    targetPort: 8000
EOF
```

### Step 2: Create an AgentRuntime CR

```yaml
kubectl apply -f - <<EOF
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-agent-runtime
  namespace: team1
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
EOF
```

The controller will:
1. Resolve `targetRef` and verify the Deployment exists
2. Apply `rossoctl.io/type: agent` and `app.kubernetes.io/managed-by: rossoctl-operator` labels
3. Compute a config hash from cluster/namespace platform configuration and set it as a `rossoctl.io/config-hash` annotation on the PodTemplateSpec
4. Trigger a rolling update — new Pods are created with the `rossoctl.io/type` label, which the AuthBridge webhook matches to inject sidecars

### Step 3: Check Status

```bash
# Check AgentRuntime status
kubectl get agentruntime -n team1

# Example output:
# NAME                      TYPE    TARGET          READY   AGE
# weather-agent-runtime     agent   weather-agent   True    2m

# View detailed conditions
kubectl describe agentruntime weather-agent-runtime -n team1

# Verify labels were applied to the Deployment
kubectl get deployment weather-agent -n team1 --show-labels

# Check that pods received sidecar injection
kubectl get pods -n team1 -l rossoctl.io/type=agent -o jsonpath='{.items[0].spec.containers[*].name}'
```

### Updating Configuration

When you update the AgentRuntime CR (e.g., changing the trust domain), the webhook picks up the new values at pod CREATE time. CR spec changes do **not** trigger a rolling update — only platform config changes (cluster or namespace ConfigMaps) do:

```bash
kubectl patch agentruntime weather-agent-runtime -n team1 --type merge -p '
spec:
  identity:
    spiffe:
      trustDomain: custom.example.com
'
```

### Deleting an AgentRuntime

When you delete the AgentRuntime CR, the controller performs a graceful cleanup:
- Removes the `rossoctl.io/type` label from the workload metadata and PodTemplateSpec
- Removes the `rossoctl.io/config-hash` annotation from the PodTemplateSpec (triggers a rolling update so existing injected pods are replaced)
- Removes the `app.kubernetes.io/managed-by` label

```bash
kubectl delete agentruntime weather-agent-runtime -n team1
```

---

## Deploy an MCP Server

MCP (Model Context Protocol) servers provide tools and resources that your agents can use. Deploy an MCP server after your agent is running.

### Basic MCP Server
```yaml
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: weather-tool
  labels:
    app.kubernetes.io/name: weather-tool
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: weather-tool
  template:
    metadata:
      labels:
        app.kubernetes.io/name: weather-tool
    spec:
      containers:
      - name: mcp
        image: ghcr.io/rossoctl/examples/weather_tool:latest
        imagePullPolicy: Always
        env:
        - name: PORT
          value: "8000"
        - name: HOST
          value: 0.0.0.0
        - name: UV_CACHE_DIR
          value: /app/.cache/uv
        ports:
        - containerPort: 8000
        volumeMounts:
        - mountPath: /app/.cache
          name: cache
      volumes:
      - name: cache
        emptyDir: {}
---
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-tool
  namespace: team1
spec:
  type: tool
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-tool
---
apiVersion: v1
kind: Service
metadata:
  labels:
    app.kubernetes.io/component: agent
    app.kubernetes.io/managed-by: rossoctl-ui
    app.kubernetes.io/name: weather-service
    rossoctl.io/framework: LangGraph
    rossoctl.io/inject: enabled
    rossoctl.io/workload-type: deployment
    protocol.rossoctl.io/a2a: ""
  name: weather-service
  namespace: team1
spec:
  ports:
  - name: http
    port: 8080
    protocol: TCP
    targetPort: 8000
  selector:
    app.kubernetes.io/name: weather-service
    rossoctl.io/type: agent
  type: ClusterIP
---
apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    rossoctl.io/managed-by: webhook
  name: weather-service
  namespace: team1
EOF
```
---
**Check Status**:
```bash

# List deployments (mcp servers)
kubectl get deployments -n team1 -l rossoctl.io/type=tool

# List pods (mcp servers)
kubectl get pods -n team1 -l rossoctl.io/type=tool

# Check detailed status
kubectl describe deployments -l rossoctl.io/type=tool -n team1
kubectl describe pods -l rossoctl.io/type=tool -n team1

# View logs
kubectl logs -l rossoctl.io/type=tool -n team1 --prefix
```


---
## End-to-End Testing: Send a Prompt to Agent

Once your Agent and MCP Server are deployed and running, you can test the end-to-end flow by sending a prompt to the agent using JSON-RPC.

> **Note**: The examples below use a temporary curl pod running inside the cluster. This approach allows you to directly access the agent service using its internal Kubernetes service DNS name (e.g., `http://weather-tool-mcp.team1.svc.cluster.local:8000`) without the need to set up port-forwarding from your local machine.

The reason this DNS name works is that a Kubernetes Service is created for each MCP Server using a specific naming convention:

[server-name]-mcp

For example, if your MCP Server resource is named weather-tool, the operator will generate a service named:

weather-tool-mcp

Kubernetes then exposes this service internally via its built-in DNS system, producing a fully qualified service hostname in the form:

[server-name]-mcp.[namespace].svc.cluster.local
### Test Weather Query

This example sends a weather query for New York to the weather-agent, which will communicate with the MCP Server to retrieve the weather information.
```bash
kubectl run curl-pod -i --tty --rm \
  --image=curlimages/curl:8.1.2 -n team1 -- \
  curl -sS -X POST http://weather-agent.team1.svc.cluster.local:8000/ \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":"76CD6BA3-16AA-4CED-8E0E-19156B8C5886","method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"What is the weather in NY?"}],"messageId":"DF05857B-98B7-4414-BD63-19E16E684E39"}}}'
```

> **Tip**: If you don't see a command prompt after running the command, try pressing enter. The pod will automatically be deleted after the curl command completes.

**Expected Response**

The agent should return a JSON-RPC response similar to:
```json
{
  "id": "76CD6BA3-16AA-4CED-8E0E-19156B8C5886",
  "jsonrpc": "2.0",
  "result": {
    "artifacts": [
      {
        "artifactId": "5e6ab98e-95a8-47d4-bfaf-a35ce9bd9235",
        "parts": [
          {
            "kind": "text",
            "text": "The current weather in NY is mostly cloudy with a temperature of 46.9°F (8.3°C). There is a moderate breeze coming from the northwest at 9.7 mph (15.6 km/h). It's currently nighttime."
          }
        ]
      }
    ],
    "contextId": "4991dce0-a893-4a65-8599-0621b46984dd",
    "history": [
      {
        "contextId": "4991dce0-a893-4a65-8599-0621b46984dd",
        "kind": "message",
        "messageId": "DF05857B-98B7-4414-BD63-19E16E684E39",
        "parts": [
          {
            "kind": "text",
            "text": "What is the weather in NY?"
          }
        ],
        "role": "user",
        "taskId": "b08ce1b8-59b6-4e96-b375-b3a0a4889b65"
      }
    ],
    "id": "b08ce1b8-59b6-4e96-b375-b3a0a4889b65",
    "kind": "task",
    "status": {
      "state": "completed",
      "timestamp": "2025-12-12T15:57:31.382161+00:00"
    }
  }
}
```

The response shows:
- **artifacts**: Contains the weather information returned by the agent
- **history**: Shows the conversation history, including the agent's interaction with the MCP server tools
- **status**: Indicates the task completed successfully
---

**Monitor the Interaction**

You can watch the logs to see the agent communicating with the MCP server:
```bash
# Watch agent logs
kubectl logs -f -l app.kubernetes.io/name=weather-agent -n team1

# Watch MCP server logs (in another terminal)
kubectl logs -f -l rossoctl.io/type=tool -n team1
```

---

## Checking Status

### Deployment Status
```bash
# Check deployment
kubectl get deployment weather-agent -n team1

# Detailed status
kubectl describe deployment weather-agent -n team1

# Check if pods are ready
kubectl get pods -n team1 -l app.kubernetes.io/name=weather-agent
```

### AgentCard Status
```bash
# List discovered agent cards
kubectl get agentcards -n team1

# Get detailed agent card info
kubectl describe agentcard -n team1

# Check signature verification status
kubectl get agentcard -n team1 -o jsonpath='{.items[0].status.validSignature}'
```

### MCP Server Status
```bash
# List deployments (mcp servers)
kubectl get deployments -n team1 -l rossoctl.io/type=tool

# Detailed status
kubectl describe deployment weather-tool -n team1

# Check if pods are running
kubectl get pods -n team1 -l rossoctl.io/type=tool

# View logs
kubectl logs -l rossoctl.io/type=tool -n team1 -f
```

---

## Troubleshooting

### Agent Not Starting
```bash
# Check deployment status
kubectl describe deployment weather-agent -n team1

# Check events
kubectl get events -n team1 --field-selector involvedObject.name=weather-agent

# Check pod status
kubectl get pods -n team1 -l app.kubernetes.io/name=weather-agent
```

### MCP Server Not Starting
```bash
# Check MCP server status
kubectl get deployment weather-tool -n team1 -o yaml

# Check pod status
kubectl get pods -n team1 -l rossoctl.io/type=tool

# Check events
kubectl get events -n team1 --field-selector involvedObject.kind=Pod,involvedObject.name=$(kubectl get pods -n team1 -l rossoctl.io/type=tool -o jsonpath='{.items[0].metadata.name}') --sort-by='.lastTimestamp'

# Verify service account exists
kubectl get serviceaccount weather-service -n team1

# Check for image pull issues
kubectl describe pod -n team1 -l rossoctl.io/type=tool
```

### Agent Not Responding to Requests
```bash
# Verify agent service is accessible
kubectl get svc weather-agent -n team1

# Test connectivity from within the cluster using a temporary curl pod
kubectl run test-curl --image=curlimages/curl:8.1.2 --rm -i --restart=Never -n team1 -- \
  curl -v http://weather-agent.team1.svc.cluster.local:8000/health

# Check if agent can reach MCP server
# Note: The agent container may not have curl installed, so we'll use a debug pod in the same namespace
kubectl run curl-debug --image=curlimages/curl:8.1.2 --rm -i --tty -n team1 -- \
  curl -v http://weather-tool-mcp.team1.svc.cluster.local:8000/health

```

### Ollama Connection Issues
```bash
# Verify Ollama is running and accessible from your local machine
curl http://localhost:11434/v1/models

# Check agent logs for LLM-related errors
kubectl logs -l app.kubernetes.io/name=weather-agent -n team1 | grep -i "llm\|model\|ollama"

# Check for connection errors in agent logs
kubectl logs -l app.kubernetes.io/name=weather-agent -n team1 --tail=100

# Test if the agent pod can reach Ollama
kubectl run curl-test --image=curlimages/curl:8.1.2 --rm -i --tty -n team1 -- \
  curl -v http://host.docker.internal:11434/v1/models
```

---

## Next Steps

- [Controller-Webhook Interaction](docs/controller-webhook-interaction.md) — How the AgentRuntime controller and AuthBridge webhook coordinate
- [Dynamic Agent Discovery](docs/dynamic-agent-discovery.md) — How AgentCard enables agent discovery
- [Signature Verification](docs/agentcard-signature-verification.md) — Set up JWS signature verification
- [Identity Binding](docs/agentcard-identity-binding.md) — Configure SPIFFE identity binding
- [Migration Guide](../docs/migration/migrate-agent-crd-to-workloads.md) — Migrating from Agent CRD to workloads
- [API Reference](docs/api-reference.md) — Full CRD specifications

---
