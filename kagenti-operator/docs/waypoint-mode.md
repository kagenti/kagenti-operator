# Waypoint Mode - Design and User Guide

**Version**: 1.0
**Status**: Production-Ready
**Last Updated**: 2026-04-03

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Design Principles](#design-principles)
- [Key Components](#key-components)
- [User Guide](#user-guide)
- [Configuration](#configuration)
- [Security Model](#security-model)
- [Performance Characteristics](#performance-characteristics)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)

---

## Overview

**Waypoint Mode** is a deployment pattern for Kagenti agents that eliminates per-pod sidecars by centralizing L7 proxy and authentication logic in shared Istio waypoint gateways. This mode is the **default** for new agent deployments.

### What is Waypoint Mode?

In waypoint mode:
- **Agents deploy as single containers** (no sidecars)
- **L7 proxy shared per namespace** via Istio waypoint gateways
- **L4 mTLS handled by ztunnel** (Istio ambient mesh component)
- **Client credentials managed centrally** by the operator
- **Automatic infrastructure provisioning** (gateways, Istio config)

### Benefits

| Benefit | Description |
|---------|-------------|
| **Resource Efficiency** | 66% reduction in containers per pod vs sidecar mode |
| **Simplified Pod Topology** | Single container per agent pod |
| **Centralized Auth** | OAuth client credentials managed by operator |
| **Automatic Provisioning** | Zero manual configuration for waypoint gateways |
| **Security Isolation** | Admin credentials never exposed to agent namespaces |

### Comparison: Waypoint vs Sidecar Mode

| Aspect | Waypoint Mode | Sidecar Mode (Legacy) |
|--------|---------------|------------------------|
| Containers per pod | **1** (agent only) | 3+ (agent + envoy + spiffe-helper + client-registration) |
| L7 Proxy | Shared waypoint gateway (1 per namespace) | Per-pod envoy sidecar |
| L4 mTLS | ztunnel DaemonSet (Istio ambient) | envoy sidecar |
| Client Registration | Operator-managed (centralized) | In-pod sidecar or operator-managed |
| Istio Integration | Ambient mesh | Sidecar injection |
| Resource Overhead | Low (shared gateway) | High (per-pod sidecars) |
| Pod Startup Time | Fast (single container) | Slower (init containers + sidecars) |

---

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│ Kagenti Platform Operator (kagenti-system namespace)            │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │ NamespaceWaypointReconciler                               │   │
│  │  - Watches Namespaces with kagenti.io/type=agent         │   │
│  │  - Provisions Istio Gateway resources                     │   │
│  │  - Applies Istio ambient mesh labels                      │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │ ClientRegistrationReconciler                              │   │
│  │  - Watches agent Deployments/StatefulSets                │   │
│  │  - Registers OIDC clients in Keycloak                     │   │
│  │  - Creates client credential secrets                      │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
                              │
                              │ Provisions & Manages
                              │
        ┌─────────────────────┴─────────────────────┐
        │                                           │
        ▼                                           ▼
┌───────────────────────────────┐   ┌───────────────────────────────┐
│ Agent Namespace (e.g., team1) │   │ Agent Namespace (e.g., team2) │
│                               │   │                               │
│ ┌───────────────────────────┐ │   │ ┌───────────────────────────┐ │
│ │ Istio Waypoint Gateway    │ │   │ │ Istio Waypoint Gateway    │ │
│ │  - L7 Envoy proxy         │ │   │ │  - L7 Envoy proxy         │ │
│ │  - JWT validation         │ │   │ │  - JWT validation         │ │
│ │  - Token exchange         │ │   │ │  - Token exchange         │ │
│ │  - mTLS workload cert     │ │   │ │  - mTLS workload cert     │ │
│ └───────────────────────────┘ │   │ └───────────────────────────┘ │
│              │                │   │              │                │
│              ▼                │   │              ▼                │
│ ┌───────────────────────────┐ │   │ ┌───────────────────────────┐ │
│ │ Agent Pod (1 container)   │ │   │ │ Agent Pod (1 container)   │ │
│ │  - Agent application      │ │   │ │  - Agent application      │ │
│ │  - OAuth client creds     │ │   │ │  - OAuth client creds     │ │
│ │    (mounted from Secret)  │ │   │ │    (mounted from Secret)  │ │
│ └───────────────────────────┘ │   │ └───────────────────────────┘ │
│                               │   │                               │
│ ┌───────────────────────────┐ │   │ ┌───────────────────────────┐ │
│ │ Client Credentials Secret │ │   │ │ Client Credentials Secret │ │
│ │  - client-id.txt          │ │   │ │  - client-id.txt          │ │
│ │  - client-secret.txt      │ │   │ │  - client-secret.txt      │ │
│ └───────────────────────────┘ │   │ └───────────────────────────┘ │
│                               │   │                               │
│ Namespace Labels:             │   │ Namespace Labels:             │
│  istio-discovery: enabled     │   │  istio-discovery: enabled     │
│  istio.io/dataplane-mode:     │   │  istio.io/dataplane-mode:     │
│    ambient                    │   │    ambient                    │
│  istio.io/use-waypoint:       │   │  istio.io/use-waypoint:       │
│    team1-waypoint             │   │    team2-waypoint             │
└───────────────────────────────┘   └───────────────────────────────┘
```

### Data Flow: Agent-to-Agent Communication

```
┌─────────────────────────────────────────────────────────────────┐
│ Step 1: Agent Obtains Access Token                             │
└─────────────────────────────────────────────────────────────────┘

Agent Pod (team1/agent-a)
    │
    │ 1. Read client credentials from mounted Secret
    │    - client-id: team1/agent-a
    │    - client-secret: <random-32-char-secret>
    │
    ▼
    │ 2. Request access token from Keycloak
    │    POST /realms/kagenti/protocol/openid-connect/token
    │    grant_type=client_credentials
    │
Keycloak
    │
    │ 3. Return JWT access token
    │    - aud: [team2/agent-b, team3/agent-c, ...]
    │    - azp: team1/agent-a
    │    - exp: 300 (5 minutes)
    │
    ▼
Agent Pod (has access token)

┌─────────────────────────────────────────────────────────────────┐
│ Step 2: Cross-Namespace Call (team1/agent-a → team2/agent-b)   │
└─────────────────────────────────────────────────────────────────┘

Agent Pod (team1/agent-a)
    │
    │ 4. HTTP Request with JWT
    │    GET http://agent-b.team2.svc.cluster.local:8080/api/task
    │    Authorization: Bearer <jwt-token>
    │
    ▼
ztunnel (L4 mTLS - node-local DaemonSet)
    │
    │ 5. L4 mTLS tunnel
    │    Source: team1/agent-a
    │    Dest: team1-waypoint
    │
    ▼
Waypoint Gateway (team1-waypoint)
    │
    │ 6. L7 Processing
    │    - Extract JWT from Authorization header
    │    - Validate JWT signature (Keycloak JWKS)
    │    - Check audience claim (must include team2/agent-b)
    │    - Check expiry, issuer, etc.
    │    - Optional: Exchange token for team2/agent-b audience
    │
    ▼ L4 mTLS (cross-namespace)
    │
Waypoint Gateway (team2-waypoint)
    │
    │ 7. Final L7 validation
    │    - Re-validate JWT
    │    - Check audience matches team2/agent-b
    │    - Apply AuthorizationPolicy (if configured)
    │
    ▼ L4 mTLS (in-namespace)
    │
ztunnel (team2 namespace)
    │
    ▼
Agent Pod (team2/agent-b)
    │
    │ 8. Receive authenticated request
    │    Headers include validated identity information
```

---

## Design Principles

### 1. Zero-Configuration Deployment

**Principle**: Agents should deploy with minimal configuration. Infrastructure provisioning should be automatic.

**Implementation**:
- NamespaceWaypointReconciler watches namespaces with `kagenti.io/type: agent` label
- Automatically creates Istio Gateway resources
- Automatically applies Istio ambient mesh labels
- No manual `istioctl` commands required

**Example**:
```yaml
# All you need is this label on the namespace
apiVersion: v1
kind: Namespace
metadata:
  name: team1
  labels:
    kagenti.io/type: agent  # Triggers automatic waypoint provisioning
```

### 2. Centralized Secret Management

**Principle**: Admin credentials should never be exposed to agent namespaces. Client credentials should be managed by the operator.

**Implementation**:
- `keycloak-admin-secret` exists ONLY in operator namespace (kagenti-system)
- ClientRegistrationReconciler reads admin secret from `r.OperatorNamespace`
- Agent namespaces receive only client credentials (client-id + client-secret)
- Secrets have owner references for automatic cleanup

**Security Benefits**:
- Reduced attack surface (admin credentials in single namespace)
- Principle of least privilege (agents never see admin credentials)
- Simplified secret rotation (one secret to rotate instead of N)

### 3. Resource Efficiency

**Principle**: Minimize per-pod overhead. Share infrastructure where possible.

**Implementation**:
- One waypoint gateway per namespace (shared by all agents)
- No per-pod envoy sidecars
- No per-pod spiffe-helper sidecars
- No per-pod client-registration sidecars

**Resource Savings**:
- 66% reduction in containers per pod
- Reduced CPU/memory footprint
- Faster pod startup times

### 4. Istio Ambient Mesh Integration

**Principle**: Leverage Istio ambient mesh for L4 mTLS and waypoint gateway support.

**Implementation**:
- `istio.io/dataplane-mode: ambient` enables Istio ambient mesh
- ztunnel DaemonSet handles L4 mTLS transparently
- Waypoint gateways handle L7 processing
- No sidecar injection required

**Benefits**:
- Simpler pod topology
- Transparent L4 mTLS
- Centralized L7 policy enforcement

---

## Key Components

### NamespaceWaypointReconciler

**Purpose**: Automatically provision waypoint gateways for namespaces with Kagenti agents.

**Triggers**:
- Namespace with `kagenti.io/type: agent` label
- Pod created with `kagenti.io/type: agent` label in the namespace

**Actions**:
1. Check if namespace has Kagenti workload pods
2. If yes, create Istio Gateway resource (if not exists)
3. Apply Istio ambient mesh labels to namespace:
   - `istio-discovery: enabled`
   - `istio.io/dataplane-mode: ambient`
   - `istio.io/use-waypoint: <namespace>-waypoint`

**Gateway Specification**:
```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: team1-waypoint
  namespace: team1
spec:
  gatewayClassName: istio-waypoint
  listeners:
  - name: mesh
    port: 15008
    protocol: HBONE
```

**Controller Configuration**:
```go
// cmd/main.go
flag.BoolVar(&enableWaypointProvisioning, "enable-waypoint-provisioning", false,
    "Enable automatic waypoint gateway provisioning for namespaces with Kagenti agents")
```

**Typical Reconciliation Time**: ~20 seconds from agent pod creation to waypoint ready.

### ClientRegistrationReconciler

**Purpose**: Register agent workloads as OAuth clients in Keycloak and create credential secrets.

**Triggers**:
- Deployment or StatefulSet with `kagenti.io/type: agent` label
- Label `kagenti.io/client-registration-inject` is NOT set to "true" (opt-out of operator management)

**Actions**:
1. Read Keycloak configuration from `kagenti-operator-config` ConfigMap (or fallback to namespace `authbridge-config`)
2. Read `keycloak-admin-secret` from operator namespace (kagenti-system)
3. Register or fetch OIDC client in Keycloak:
   - Client ID: `namespace/workload-name` (or SPIFFE ID if SPIRE enabled)
   - Client auth type: `client-secret`
   - Token exchange: enabled
   - Audience scope: platform clients + configured audiences
4. Create/update Secret in agent namespace:
   - Name: `kagenti-keycloak-client-credentials-<hash>`
   - Keys: `client-id.txt`, `client-secret.txt`
   - Owner reference: Deployment/StatefulSet (auto-deleted with workload)
5. Annotate pod template with secret name for webhook mounting

**Secret Naming**:
```go
// Deterministic: SHA256 hash of namespace + workload name
func keycloakClientCredentialsSecretName(namespace, workload string) string {
    sum := sha256.Sum256([]byte(namespace + "\x00" + workload + "\x00kagenti-keycloak-client-credentials"))
    return "kagenti-keycloak-client-credentials-" + hex.EncodeToString(sum[:8])
}
```

**Controller Configuration**:
```go
// cmd/main.go
flag.BoolVar(&enableOperatorClientRegistration, "enable-operator-client-registration", false,
    "Enable operator-managed Keycloak client registration (default path)")
```

**Typical Reconciliation Time**: ~30 seconds from deployment creation to secret available.

### Istio Waypoint Gateway

**Purpose**: Shared L7 proxy for all agents in a namespace.

**Responsibilities**:
- JWT validation (signature, expiry, audience, issuer)
- Token exchange (OAuth 2.0 RFC 8693)
- AuthorizationPolicy enforcement
- mTLS termination (workload certificates from Istio CA)
- Request routing to upstream services

**Pod Specification** (managed by Istio):
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: team1-waypoint-f6d4d946-xxxxx
  namespace: team1
  labels:
    gateway.networking.k8s.io/gateway-name: team1-waypoint
    istio.io/gateway-name: team1-waypoint
spec:
  containers:
  - name: istio-proxy
    image: gcr.io/istio-release/proxyv2:1.24.0
    args:
    - proxy
    - waypoint
    - --domain
    - $(POD_NAMESPACE).svc.cluster.local
    env:
    - name: ISTIO_META_WAYPOINT_NAME
      value: team1-waypoint
```

**Resource Requirements** (default):
- CPU: 100m request, 2000m limit
- Memory: 128Mi request, 1Gi limit

**Scaling**: Horizontal Pod Autoscaler can be configured for high-traffic namespaces.

### ztunnel (Istio Ambient Mesh)

**Purpose**: L4 mTLS data plane for Istio ambient mesh.

**Deployment**: DaemonSet (one pod per node)

**Responsibilities**:
- Transparent L4 mTLS tunneling
- Traffic capture via iptables or eBPF
- Workload identity verification (SPIFFE SVIDs)
- Traffic routing to waypoint gateways

**Configuration**: Managed by Istio control plane (istiod).

---

## User Guide

### Prerequisites

1. **Istio Ambient Mesh Installed**:
   ```bash
   istioctl install --set profile=ambient --set values.pilot.env.PILOT_ENABLE_AMBIENT=true
   ```

2. **Keycloak Deployed and Configured**:
   - Realm created (e.g., `kagenti`)
   - Admin credentials available

3. **Kagenti Operator Deployed**:
   ```bash
   kubectl apply -f config/crd/bases/
   kubectl apply -f config/rbac/
   kubectl apply -f config/manager/
   ```

4. **Operator Configuration**:
   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: kagenti-operator-config
     namespace: kagenti-system
   data:
     KEYCLOAK_URL: https://keycloak.example.com
     KEYCLOAK_REALM: kagenti
     CLIENT_AUTH_TYPE: client-secret
     KEYCLOAK_TOKEN_EXCHANGE_ENABLED: "true"
     KEYCLOAK_AUDIENCE_SCOPE_ENABLED: "true"
     PLATFORM_CLIENT_IDS: kagenti
     SPIRE_ENABLED: "false"
   ```

5. **Keycloak Admin Secret** (operator namespace only):
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: keycloak-admin-secret
     namespace: kagenti-system
   type: Opaque
   stringData:
     KEYCLOAK_ADMIN_USERNAME: admin
     KEYCLOAK_ADMIN_PASSWORD: <secure-password>
   ```

### Deploying an Agent in Waypoint Mode

**Step 1**: Create namespace with agent label

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: my-agents
  labels:
    kagenti.io/type: agent  # Triggers waypoint provisioning
```

**Step 2**: Deploy your agent

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-agent
  namespace: my-agents
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-agent
  template:
    metadata:
      labels:
        app: my-agent
        kagenti.io/type: agent  # Required for operator discovery
        kagenti.io/auth-mode: waypoint  # Optional: documents intent
    spec:
      containers:
      - name: agent
        image: my-org/my-agent:latest
        env:
        - name: KEYCLOAK_URL
          value: https://keycloak.example.com
        - name: KEYCLOAK_REALM
          value: kagenti
        # Client credentials will be mounted by webhook at /shared/client-*.txt
        volumeMounts:
        - name: shared-data
          mountPath: /shared
      volumes:
      - name: shared-data
        emptyDir: {}
```

**Step 3**: Verify deployment

```bash
# Check waypoint gateway created
kubectl get gateway -n my-agents

# Check Istio labels applied
kubectl get namespace my-agents -o jsonpath='{.metadata.labels}' | jq '.'

# Check client secret created
kubectl get secrets -n my-agents | grep kagenti-keycloak-client-credentials

# Check pod has single container
kubectl get pod -n my-agents -l app=my-agent -o jsonpath='{.items[0].spec.containers[*].name}'
```

**Step 4**: Access client credentials in your agent

```python
# Python example
import os

def get_keycloak_credentials():
    """Read client credentials from mounted secret."""
    client_id = open('/shared/client-id.txt').read().strip()
    client_secret = open('/shared/client-secret.txt').read().strip()
    return client_id, client_secret

def get_access_token():
    """Obtain JWT access token from Keycloak."""
    import requests

    client_id, client_secret = get_keycloak_credentials()
    keycloak_url = os.getenv('KEYCLOAK_URL')
    realm = os.getenv('KEYCLOAK_REALM')

    response = requests.post(
        f"{keycloak_url}/realms/{realm}/protocol/openid-connect/token",
        data={
            'grant_type': 'client_credentials',
            'client_id': client_id,
            'client_secret': client_secret,
        },
        headers={'Content-Type': 'application/x-www-form-urlencoded'}
    )

    return response.json()['access_token']

# Use the token
token = get_access_token()
headers = {'Authorization': f'Bearer {token}'}
response = requests.get('http://other-agent.other-ns.svc.cluster.local:8080/api/task', headers=headers)
```

### Token Exchange for Cross-Namespace Calls

When calling an agent in a different namespace, exchange your token for the target audience:

```python
def exchange_token(access_token, target_audience):
    """Exchange token for specific audience using OAuth 2.0 Token Exchange (RFC 8693)."""
    import requests

    client_id, client_secret = get_keycloak_credentials()
    keycloak_url = os.getenv('KEYCLOAK_URL')
    realm = os.getenv('KEYCLOAK_REALM')

    response = requests.post(
        f"{keycloak_url}/realms/{realm}/protocol/openid-connect/token",
        data={
            'grant_type': 'urn:ietf:params:oauth:grant-type:token-exchange',
            'client_id': client_id,
            'client_secret': client_secret,
            'subject_token': access_token,
            'subject_token_type': 'urn:ietf:params:oauth:token-type:access_token',
            'audience': target_audience,
        },
        headers={'Content-Type': 'application/x-www-form-urlencoded'}
    )

    return response.json()['access_token']

# Example: Call agent in different namespace
access_token = get_access_token()
target_audience = 'other-namespace/other-agent'
exchanged_token = exchange_token(access_token, target_audience)

headers = {'Authorization': f'Bearer {exchanged_token}'}
response = requests.get('http://other-agent.other-namespace.svc.cluster.local:8080/api/task', headers=headers)
```

---

## Configuration

### Operator Flags

```bash
# cmd/main.go
--enable-waypoint-provisioning=true         # Enable automatic waypoint provisioning (default: false)
--enable-operator-client-registration=true  # Enable operator-managed client registration (default: false)
--operator-namespace=kagenti-system         # Operator namespace for reading admin secrets
--spire-trust-domain=cluster.local          # SPIRE trust domain (if SPIRE enabled)
```

### Opt-Out of Waypoint Mode

To use legacy sidecar mode for specific workloads:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: legacy-agent
  namespace: my-agents
spec:
  template:
    metadata:
      labels:
        kagenti.io/type: agent
        kagenti.io/client-registration-inject: "true"  # Opt into sidecar mode
```

This will:
- Disable operator-managed client registration
- Enable webhook injection of client-registration sidecar
- Use per-pod envoy sidecar instead of waypoint gateway

### Namespace-Specific Configuration

Override Keycloak config per namespace (fallback from operator config):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config
  namespace: my-agents
data:
  KEYCLOAK_URL: https://keycloak.example.com
  KEYCLOAK_REALM: my-custom-realm
  CLIENT_AUTH_TYPE: client-secret
  KEYCLOAK_TOKEN_EXCHANGE_ENABLED: "true"
```

Operator will prefer `kagenti-operator-config` from operator namespace, but fall back to namespace-local `authbridge-config` if operator config is incomplete.

---

## Security Model

### Admin Credential Isolation

**Threat Model**: Compromised agent namespace should NOT expose Keycloak admin credentials.

**Implementation**:
- `keycloak-admin-secret` exists ONLY in operator namespace (kagenti-system)
- Agent namespaces NEVER contain admin credentials
- ClientRegistrationReconciler reads from `r.OperatorNamespace`

**Verification**:
```bash
# Admin secret should exist only here
kubectl get secret -n kagenti-system keycloak-admin-secret

# Should fail (NotFound)
kubectl get secret -n my-agents keycloak-admin-secret
```

### Client Credential Lifecycle

**Creation**:
- Operator creates secret with owner reference to Deployment/StatefulSet
- Secret automatically deleted when workload is deleted

**Rotation**:
- Manual: Delete secret, operator will recreate with new credentials
- Automatic: Future enhancement (rotate on schedule)

**Access Control**:
- Secret mounted read-only into agent pods
- RBAC: Only pods in the namespace can read the secret
- No cluster-wide secret access required

### JWT Validation at Waypoint

Waypoint gateways validate JWTs before routing:

1. **Signature Verification**: RSA signature verified against Keycloak JWKS
2. **Expiry Check**: `exp` claim must be in the future
3. **Issuer Validation**: `iss` claim must match trusted Keycloak issuer
4. **Audience Validation**: `aud` claim must include target service
5. **Not-Before Check**: `nbf` claim (if present) must be in the past

**Istio RequestAuthentication**:
```yaml
apiVersion: security.istio.io/v1
kind: RequestAuthentication
metadata:
  name: jwt-validation
  namespace: my-agents
spec:
  jwtRules:
  - issuer: https://keycloak.example.com/realms/kagenti
    jwksUri: https://keycloak.example.com/realms/kagenti/protocol/openid-connect/certs
    audiences:
    - my-agents/my-agent
```

**Istio AuthorizationPolicy**:
```yaml
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: require-jwt
  namespace: my-agents
spec:
  action: ALLOW
  rules:
  - from:
    - source:
        requestPrincipals: ["*"]
    when:
    - key: request.auth.claims[aud]
      values: ["my-agents/my-agent"]
```

---

## Performance Characteristics

### Resource Overhead

**Waypoint Mode** (per namespace with 10 agents):
- Waypoint gateway pod: 1
- Agent pods: 10 (1 container each)
- Total containers: 11

**Sidecar Mode** (per namespace with 10 agents):
- Agent pods: 10 (3 containers each: agent + envoy + spiffe-helper)
- Total containers: 30

**Savings**: 63% reduction in container count.

### Latency Impact

**L4 mTLS (ztunnel)**:
- Overhead: ~0.5ms (transparent TCP tunnel)
- CPU: Minimal (eBPF-based traffic capture)

**L7 Proxy (waypoint)**:
- JWT validation: ~1-2ms (cached JWKS)
- Token exchange: ~100ms (roundtrip to Keycloak)
- Total L7 overhead: ~2-5ms (without token exchange)

**Recommendation**: Cache access tokens and reuse until expiry (5 minutes default).

### Scalability

**Waypoint Gateway Scaling**:
- Default: 1 replica per namespace
- High-traffic namespaces: Use HorizontalPodAutoscaler
  ```yaml
  apiVersion: autoscaling/v2
  kind: HorizontalPodAutoscaler
  metadata:
    name: team1-waypoint-hpa
    namespace: team1
  spec:
    scaleTargetRef:
      apiVersion: apps/v1
      kind: Deployment
      name: team1-waypoint
    minReplicas: 2
    maxReplicas: 10
    metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
  ```

**ztunnel Scaling**:
- DaemonSet: Scales with node count automatically
- No manual intervention required

---

## Troubleshooting

### Waypoint Gateway Not Created

**Symptom**: Namespace has agents but no waypoint gateway.

**Diagnosis**:
```bash
# Check namespace labels
kubectl get namespace my-agents -o yaml | grep labels -A 10

# Check operator logs
kubectl logs -n kagenti-system deployment/kagenti-controller-manager | grep waypoint

# Check if waypoint provisioning enabled
kubectl get deployment -n kagenti-system kagenti-controller-manager -o yaml | grep enable-waypoint
```

**Solution**:
1. Ensure namespace has `kagenti.io/type: agent` label
2. Ensure operator has `--enable-waypoint-provisioning=true` flag
3. Check operator RBAC permissions for Gateway resources

### Client Secret Not Created

**Symptom**: Agent deployed but no `kagenti-keycloak-client-credentials-*` secret.

**Diagnosis**:
```bash
# Check operator logs
kubectl logs -n kagenti-system deployment/kagenti-controller-manager | grep clientregistration

# Check if keycloak-admin-secret exists
kubectl get secret -n kagenti-system keycloak-admin-secret

# Check Keycloak config
kubectl get configmap -n kagenti-system kagenti-operator-config -o yaml
```

**Common Causes**:
1. Missing `keycloak-admin-secret` in kagenti-system
2. Incorrect `KEYCLOAK_URL` or `KEYCLOAK_REALM` in config
3. Keycloak admin credentials invalid (401 errors in logs)
4. Workload has `kagenti.io/client-registration-inject: "true"` (opt-out of operator management)

**Solution**:
```bash
# Create admin secret if missing
kubectl create secret generic keycloak-admin-secret \
  -n kagenti-system \
  --from-literal=KEYCLOAK_ADMIN_USERNAME=admin \
  --from-literal=KEYCLOAK_ADMIN_PASSWORD=<password>

# Verify Keycloak connectivity
curl -X POST "https://keycloak.example.com/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=<password>"
```

### Token Exchange Fails

**Symptom**: `400 Bad Request: Requested audience not available: target-namespace/target-agent`

**Diagnosis**:
```bash
# Decode JWT to see available audiences
TOKEN=$(cat /tmp/my_token.txt)
PAYLOAD=$(echo "$TOKEN" | cut -d'.' -f2)
echo "$PAYLOAD==" | base64 -d | jq '.aud'
```

**Root Cause**: Target audience not configured in Keycloak client scopes.

**Solution**:
1. **Option A**: Use existing audience (check token's `aud` claim)
2. **Option B**: Configure Keycloak audience scopes:
   - Navigate to Keycloak Admin Console
   - Realms → kagenti → Client scopes
   - Create audience scope for target agent
   - Assign to source agent client

**Future Enhancement**: Operator will automatically configure bidirectional audience scopes.

### 503 Errors from Waypoint

**Symptom**: `upstream connect error or disconnect/reset before headers`

**Diagnosis**:
```bash
# Check waypoint logs
kubectl logs -n my-agents -l gateway.networking.k8s.io/gateway-name=my-agents-waypoint

# Check target pod is running
kubectl get pods -n target-namespace -l app=target-agent

# Check if target pod has listener on expected port
kubectl exec -n target-namespace deployment/target-agent -- netstat -tuln
```

**Common Causes**:
1. Target pod not running
2. Target pod has no HTTP server on expected port
3. Service port mismatch (Service port ≠ container port)

**Solution**:
1. Ensure target pod has HTTP server listening
2. Verify Service port matches container port
3. Check Istio VirtualService / DestinationRule configuration (if custom routing)

---

## FAQ

### Q: Is waypoint mode the default?

**A**: Yes, for new deployments. Waypoint mode is the default when:
- Operator has `--enable-waypoint-provisioning=true`
- Operator has `--enable-operator-client-registration=true`
- Workload does NOT have `kagenti.io/client-registration-inject: "true"` label

Legacy sidecar mode is opt-in via the `kagenti.io/client-registration-inject: "true"` label.

### Q: Can I mix waypoint and sidecar mode in the same cluster?

**A**: Yes. Waypoint mode and sidecar mode can coexist:
- Waypoint mode: Namespaces with Istio ambient mesh + waypoint gateways
- Sidecar mode: Namespaces with Istio sidecar injection

Just ensure the appropriate Istio configuration is applied per namespace.

### Q: How do I migrate from sidecar to waypoint mode?

**A**: See [Migration Guide](./migration-sidecar-to-waypoint.md) for step-by-step instructions.

### Q: Does waypoint mode support SPIFFE/SPIRE?

**A**: Yes. When `SPIRE_ENABLED=true` in configuration:
- Client IDs use SPIFFE format: `spiffe://<trust-domain>/ns/<namespace>/sa/<serviceAccount>`
- Requires `--spire-trust-domain` flag on operator
- Requires dedicated ServiceAccount (not `default`)

### Q: What happens if waypoint gateway pod crashes?

**A**: Kubernetes will automatically restart the pod. During downtime:
- L4 mTLS still works (ztunnel)
- L7 requests will fail until waypoint recovers
- Consider deploying multiple waypoint replicas for HA

### Q: How do I monitor waypoint gateways?

**A**: Waypoint gateways expose Prometheus metrics:
```bash
kubectl port-forward -n my-agents deployment/my-agents-waypoint 15020:15020
curl http://localhost:15020/stats/prometheus
```

Key metrics:
- `istio_requests_total`: Request count
- `istio_request_duration_milliseconds`: Latency
- `istio_request_bytes`: Request size
- `envoy_cluster_upstream_cx_connect_fail`: Connection failures

### Q: Can I customize waypoint gateway resources?

**A**: Currently, waypoint gateways use Istio defaults. Custom resource limits can be set via Istio configuration. Future enhancement: Allow per-namespace customization.

---

## Additional Resources

- [Architecture Documentation](./architecture.md)
- [Operator-Managed Client Registration](./operator-managed-client-registration.md)
- [Migration Guide: Sidecar to Waypoint](./migration-sidecar-to-waypoint.md)
- [Istio Ambient Mesh Documentation](https://istio.io/latest/docs/ambient/)
- [OAuth 2.0 Token Exchange (RFC 8693)](https://datatracker.ietf.org/doc/html/rfc8693)
- [Keycloak Documentation](https://www.keycloak.org/documentation)
