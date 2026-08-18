# AgentRuntime Authentication Configuration

This guide explains how to configure SPIFFE-based authentication and token exchange for agents and tools using the AgentRuntime CRD's `spec.auth` field.

## Overview

The `spec.auth` field allows you to declaratively configure:
- **Authentication mode**: How the agent authenticates to Keycloak (federated-jwt, client-secret, or disabled)
- **Outbound routes**: Which destinations require token exchange and what audiences to request

When configured, the operator generates AuthBridge routes that automatically perform OAuth2 token exchange, requesting SPIFFE tokens with the appropriate audiences for each destination.

## Prerequisites

- SPIRE deployed and running (required for `mode: federated-jwt`)
- Keycloak 26.6+ with federated client authentication enabled
- SPIFFE Identity Provider configured in Keycloak

## Configuration

### Basic Structure

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: my-agent
  namespace: team1
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-agent
  auth:
    mode: federated-jwt  # or client-secret, disabled
    outbound:
      - destination:
          host: "tool.team1.svc.cluster.local"
        audiences:
          - "spiffe://trust-domain/ns/team1/sa/tool"
```

### Authentication Modes

#### `federated-jwt` (Recommended)

Uses SPIFFE JWT-SVID for client authentication. Provides the strongest security model with workload identity.

**Requirements:**
- SPIRE must be deployed
- Workload must have a dedicated ServiceAccount
- Keycloak must support federated client authentication (26.6+)

**Example:**
```yaml
spec:
  auth:
    mode: federated-jwt
    outbound:
      - destination:
          host: "weather-tool-mcp.team1.svc.cluster.local"
        audiences:
          - "spiffe://localtest.me/ns/team1/sa/weather-tool"
```

#### `client-secret` (Default)

Traditional OAuth2 client credentials flow. The operator provisions a client secret.

**Example:**
```yaml
spec:
  auth:
    mode: client-secret
    # No outbound routes needed - uses default policy
```

#### `disabled`

No authentication configured. Use for public endpoints or when authentication is handled elsewhere.

**Example:**
```yaml
spec:
  auth:
    mode: disabled
```

### Outbound Routes

Routes define which destinations require token exchange and what audiences to request.

#### Exact Host Matching

Match a specific hostname:

```yaml
spec:
  auth:
    mode: federated-jwt
    outbound:
      - destination:
          host: "weather-tool-mcp.team1.svc.cluster.local"
        audiences:
          - "spiffe://localtest.me/ns/team1/sa/weather-tool"
```

#### Regex Matching

Match multiple hosts with a pattern:

```yaml
spec:
  auth:
    mode: federated-jwt
    outbound:
      - destination:
          hostRegex: ".*\\.team1\\.svc\\.cluster\\.local"
        audiences:
          - "spiffe://localtest.me/ns/team1/sa/default"
```

#### Multiple Audiences

Request tokens with multiple audiences:

```yaml
spec:
  auth:
    mode: federated-jwt
    outbound:
      - destination:
          host: "multi-service.team1.svc.cluster.local"
        audiences:
          - "spiffe://localtest.me/ns/team1/sa/service-a"
          - "spiffe://localtest.me/ns/team1/sa/service-b"
```

#### Multiple Routes

Define separate routes for different destinations:

```yaml
spec:
  auth:
    mode: federated-jwt
    outbound:
      - destination:
          host: "tool-a.team1.svc.cluster.local"
        audiences:
          - "spiffe://localtest.me/ns/team1/sa/tool-a"
      - destination:
          host: "tool-b.team1.svc.cluster.local"
        audiences:
          - "spiffe://localtest.me/ns/team1/sa/tool-b"
      - destination:
          hostRegex: ".*\\.team2\\.svc\\.cluster\\.local"
        audiences:
          - "spiffe://localtest.me/ns/team2/sa/default"
```

## How It Works

When you create an AgentRuntime with `spec.auth` configured:

1. **Webhook Intercepts Pod Creation**: The operator's mutating webhook intercepts pod admission for the target workload

2. **Fetches AgentRuntime**: The webhook looks up the AgentRuntime CR matching the workload

3. **Generates Routes**: If `spec.auth.mode` is `federated-jwt` and `spec.auth.outbound` is defined, the webhook generates token-exchange routes

4. **Injects ConfigMap**: Routes are injected into the per-agent AuthBridge ConfigMap at `pipeline.outbound.plugins[token-exchange].config.routes`

5. **AuthBridge Enforces Routes**: When the agent makes outbound requests, AuthBridge:
   - Matches the destination against route patterns
   - Requests a SPIFFE token with the specified audiences via OAuth2 token exchange
   - Includes the token in the `Authorization` header

## Generated Configuration

For the weather agent example above, the operator generates:

```yaml
# In authbridge-config-weather-service ConfigMap
pipeline:
  outbound:
    plugins:
      - name: token-exchange
        config:
          keycloak_url: "http://keycloak-service.keycloak.svc:8080"
          keycloak_realm: "rossoctl"
          default_policy: "passthrough"
          identity:
            type: "spiffe"
            jwt_audience: "http://keycloak.localtest.me:8080/realms/rossoctl"
          routes:
            - destination:
                host: "weather-tool-mcp.team1.svc.cluster.local"
              audiences:
                - "spiffe://localtest.me/ns/team1/sa/weather-tool"
```

## SPIFFE ID Format

SPIFFE IDs follow the pattern: `spiffe://<trust-domain>/ns/<namespace>/sa/<serviceaccount>`

- **trust-domain**: Usually `cluster.local` or your custom domain (e.g., `localtest.me`)
- **namespace**: Kubernetes namespace where the target workload runs
- **serviceaccount**: ServiceAccount name used by the target workload

**Example:**
- Weather tool in `team1` namespace with `weather-tool` ServiceAccount:
  - SPIFFE ID: `spiffe://localtest.me/ns/team1/sa/weather-tool`

## Complete Example

Here's a complete working example with an agent calling a tool:

```yaml
---
# Weather Tool ServiceAccount
apiVersion: v1
kind: ServiceAccount
metadata:
  name: weather-tool
  namespace: team1
---
# Weather Tool Deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: weather-tool
  namespace: team1
  labels:
    rossoctl.io/type: tool
spec:
  replicas: 1
  selector:
    matchLabels:
      app: weather-tool
  template:
    metadata:
      labels:
        app: weather-tool
        rossoctl.io/type: tool
    spec:
      serviceAccountName: weather-tool
      containers:
      - name: tool
        image: ghcr.io/rossoctl/examples/weather-tool:latest
        ports:
        - containerPort: 8000
---
# Weather Tool Service
apiVersion: v1
kind: Service
metadata:
  name: weather-tool-mcp
  namespace: team1
spec:
  selector:
    app: weather-tool
  ports:
  - port: 8000
    targetPort: 8000
---
# Weather Tool AgentRuntime
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
# Weather Agent ServiceAccount
apiVersion: v1
kind: ServiceAccount
metadata:
  name: weather-agent
  namespace: team1
---
# Weather Agent Deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: weather-agent
  namespace: team1
  labels:
    rossoctl.io/type: agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app: weather-agent
  template:
    metadata:
      labels:
        app: weather-agent
        rossoctl.io/type: agent
    spec:
      serviceAccountName: weather-agent
      containers:
      - name: agent
        image: ghcr.io/rossoctl/examples/weather-agent:latest
        env:
        - name: MCP_URL
          value: "http://weather-tool-mcp.team1.svc.cluster.local:8000/mcp"
        ports:
        - containerPort: 8000
---
# Weather Agent AgentRuntime with auth configuration
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-agent
  namespace: team1
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
  # Configure SPIFFE authentication with token exchange
  auth:
    mode: federated-jwt
    outbound:
      # Request SPIFFE token with weather-tool audience when calling MCP
      - destination:
          host: "weather-tool-mcp.team1.svc.cluster.local"
        audiences:
          - "spiffe://localtest.me/ns/team1/sa/weather-tool"
```

## Troubleshooting

### Routes Not Generated

**Symptom**: AuthBridge ConfigMap doesn't contain routes

**Check:**
1. AgentRuntime exists and matches the workload:
   ```bash
   kubectl get agentruntime -n <namespace>
   kubectl describe agentruntime <name> -n <namespace>
   ```

2. Pod was created after AgentRuntime:
   ```bash
   kubectl delete pod -l app=<app-name> -n <namespace>
   # Wait for new pod to be created
   ```

3. Check operator logs:
   ```bash
   kubectl logs -n rossoctl-system -l control-plane=controller-manager --tail=100
   ```

### Token Exchange Failing

**Symptom**: Agent can't reach tool, gets 401/403 errors

**Check:**
1. SPIRE is running:
   ```bash
   kubectl get pods -n spire
   ```

2. ServiceAccount exists and matches SPIFFE ID:
   ```bash
   kubectl get sa <serviceaccount> -n <namespace>
   ```

3. Keycloak has SPIFFE Identity Provider configured

4. Check AuthBridge logs:
   ```bash
   kubectl logs -n <namespace> <pod-name> -c authbridge-proxy
   ```

### Wrong Audiences

**Symptom**: Token exchange succeeds but tool rejects token

**Check:**
1. Verify the tool's SPIFFE ID matches the requested audience:
   ```bash
   # Get tool's ServiceAccount
   kubectl get pod <tool-pod> -n <namespace> -o jsonpath='{.spec.serviceAccountName}'
   
   # Construct SPIFFE ID: spiffe://<trust-domain>/ns/<namespace>/sa/<serviceaccount>
   ```

2. Check tool's AuthBridge config expects this audience:
   ```bash
   kubectl get cm authbridge-config-<tool-name> -n <namespace> -o yaml
   ```

## Best Practices

1. **Use Dedicated ServiceAccounts**: Each agent/tool should have its own ServiceAccount for distinct SPIFFE identities

2. **Principle of Least Privilege**: Only request audiences for destinations the workload actually needs to call

3. **Specific Host Matching**: Prefer exact `host` matches over `hostRegex` when possible for better performance and clarity

4. **Document Trust Domains**: Make sure your SPIFFE trust domain is documented and consistent across the cluster

5. **Test Token Exchange**: Use the weather agent example as a template for testing your configuration

## See Also

- [SPIFFE Specification](https://github.com/spiffe/spiffe)
- [OAuth2 Token Exchange (RFC 8693)](https://datatracker.ietf.org/doc/html/rfc8693)
- [Keycloak Federated Client Authentication](https://www.keycloak.org/docs/latest/securing_apps/#_client-authentication)
- [AuthBridge Documentation](../../docs/authbridge/)
