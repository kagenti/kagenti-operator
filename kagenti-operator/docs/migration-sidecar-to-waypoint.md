# Migration Guide: Sidecar Mode to Waypoint Mode

**Version**: 1.0
**Last Updated**: 2026-04-03
**Audience**: Platform teams, DevOps engineers

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Migration Strategy](#migration-strategy)
- [Step-by-Step Migration](#step-by-step-migration)
- [Rollback Procedure](#rollback-procedure)
- [Validation](#validation)
- [Troubleshooting](#troubleshooting)

---

## Overview

This guide provides instructions for migrating existing Kagenti agent deployments from **sidecar mode** to **waypoint mode**.

### What Changes

| Aspect | Sidecar Mode (Before) | Waypoint Mode (After) |
|--------|----------------------|------------------------|
| **Pod Topology** | 3+ containers (agent + envoy + spiffe-helper + client-registration) | 1 container (agent only) |
| **L7 Proxy** | Per-pod envoy sidecar | Shared waypoint gateway (1 per namespace) |
| **Client Registration** | In-pod sidecar OR operator-managed | Operator-managed (default) |
| **Istio Integration** | Sidecar injection | Ambient mesh |
| **Client Credentials** | Mounted from sidecar-created secret OR operator secret | Operator-managed secret |
| **Namespace Config** | Istio sidecar injection label | Istio ambient mesh labels |

### Benefits of Migration

- **Resource Efficiency**: 66% reduction in containers per pod
- **Simplified Operations**: No sidecar lifecycle management
- **Centralized Auth**: Operator manages client credentials
- **Faster Deployments**: Single-container pods start faster
- **Security**: Admin credentials isolated to operator namespace

---

## Prerequisites

### Cluster Requirements

1. **Istio Ambient Mesh Installed**:
   ```bash
   # Verify Istio ambient components
   kubectl get deployment -n istio-system istiod
   kubectl get daemonset -n istio-system ztunnel
   ```

2. **Kagenti Operator Updated**:
   - Minimum version: Includes NamespaceWaypointReconciler and operator-managed client registration
   - Verify operator image includes waypoint support:
     ```bash
     kubectl get deployment -n kagenti-system kagenti-controller-manager \
       -o jsonpath='{.spec.template.spec.containers[0].image}'
     ```

3. **Operator Configuration**:
   ```yaml
   # ConfigMap: kagenti-operator-config in kagenti-system
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
   ```

4. **Keycloak Admin Secret** (in operator namespace):
   ```bash
   kubectl get secret -n kagenti-system keycloak-admin-secret
   ```

### Agent Requirements

1. **Agent Code Compatibility**: Agents must support reading client credentials from mounted files:
   - `/shared/client-id.txt`
   - `/shared/client-secret.txt`

2. **Service Mesh Compatibility**: Agents must work with L4 mTLS (ztunnel) and L7 proxy (waypoint)

---

## Migration Strategy

### Recommended Approach: Blue-Green Migration

Migrate one namespace at a time to minimize risk:

1. **Phase 1**: Test in staging namespace
2. **Phase 2**: Migrate non-critical production namespaces
3. **Phase 3**: Migrate critical production namespaces
4. **Phase 4**: Decommission sidecar infrastructure

### Timeline

| Phase | Duration | Rollback Risk |
|-------|----------|---------------|
| Preparation | 1 hour | N/A |
| Staging Test | 1-2 days | Low |
| Non-Critical Production | 1 week | Low |
| Critical Production | 2 weeks | Medium |
| Cleanup | 1 week | Low |

---

## Step-by-Step Migration

### Phase 1: Preparation

**1.1 Enable Operator Features**

Update operator deployment to enable waypoint provisioning and client registration:

```bash
kubectl patch deployment -n kagenti-system kagenti-controller-manager --type=json -p='[
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--enable-waypoint-provisioning=true"
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--enable-operator-client-registration=true"
  }
]'
```

**1.2 Verify Operator Configuration**

```bash
# Check operator flags
kubectl get deployment -n kagenti-system kagenti-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | jq -r '.[]' | grep enable

# Expected output:
# --enable-waypoint-provisioning=true
# --enable-operator-client-registration=true
```

**1.3 Create Staging Namespace**

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: staging-agents
  labels:
    kagenti.io/type: agent  # Triggers waypoint provisioning
EOF
```

### Phase 2: Deploy Test Agent in Waypoint Mode

**2.1 Deploy Test Agent**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-agent-waypoint
  namespace: staging-agents
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test-agent-waypoint
  template:
    metadata:
      labels:
        app: test-agent-waypoint
        kagenti.io/type: agent
        kagenti.io/auth-mode: waypoint
    spec:
      containers:
      - name: agent
        image: my-org/my-agent:latest
        env:
        - name: KEYCLOAK_URL
          valueFrom:
            configMapKeyRef:
              name: kagenti-operator-config
              key: KEYCLOAK_URL
        - name: KEYCLOAK_REALM
          valueFrom:
            configMapKeyRef:
              name: kagenti-operator-config
              key: KEYCLOAK_REALM
        volumeMounts:
        - name: shared-data
          mountPath: /shared
      volumes:
      - name: shared-data
        emptyDir: {}
```

**2.2 Verify Waypoint Infrastructure**

```bash
# Wait for waypoint gateway
kubectl wait --for=condition=Programmed=true \
  gateway/staging-agents-waypoint \
  -n staging-agents \
  --timeout=60s

# Verify Istio labels
kubectl get namespace staging-agents -o jsonpath='{.metadata.labels}' | jq '.'

# Expected labels:
# {
#   "istio-discovery": "enabled",
#   "istio.io/dataplane-mode": "ambient",
#   "istio.io/use-waypoint": "staging-agents-waypoint"
# }
```

**2.3 Verify Client Registration**

```bash
# Wait for client secret
kubectl wait --for=jsonpath='{.data.client-id\.txt}' \
  secret/kagenti-keycloak-client-credentials-* \
  -n staging-agents \
  --timeout=60s

# Verify secret contents
SECRET_NAME=$(kubectl get secrets -n staging-agents -o name | grep kagenti-keycloak-client-credentials)
kubectl get -n staging-agents $SECRET_NAME -o jsonpath='{.data.client-id\.txt}' | base64 -d
```

**2.4 Test Agent Functionality**

```bash
# Check pod has single container
POD_NAME=$(kubectl get pod -n staging-agents -l app=test-agent-waypoint -o jsonpath='{.items[0].metadata.name}')
kubectl get pod -n staging-agents $POD_NAME -o jsonpath='{.spec.containers[*].name}'
# Expected: single "agent" container

# Check agent can obtain access token
kubectl exec -n staging-agents $POD_NAME -- \
  curl -s -X POST "${KEYCLOAK_URL}/realms/kagenti/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=$(cat /shared/client-id.txt)" \
  -d "client_secret=$(cat /shared/client-secret.txt)" | jq -r '.access_token'
```

### Phase 3: Migrate Production Namespace

**3.1 Identify Production Namespace**

```bash
# List namespaces with sidecar-mode agents
kubectl get namespaces -l istio-injection=enabled

# Select target namespace for migration
export TARGET_NAMESPACE=production-agents
```

**3.2 Backup Current Configuration**

```bash
# Backup namespace
kubectl get namespace $TARGET_NAMESPACE -o yaml > backup-${TARGET_NAMESPACE}-namespace.yaml

# Backup deployments
kubectl get deployments -n $TARGET_NAMESPACE -o yaml > backup-${TARGET_NAMESPACE}-deployments.yaml

# Backup secrets (if manually managed)
kubectl get secrets -n $TARGET_NAMESPACE -o yaml > backup-${TARGET_NAMESPACE}-secrets.yaml
```

**3.3 Remove Sidecar Injection Label**

```bash
# Remove Istio sidecar injection label
kubectl label namespace $TARGET_NAMESPACE istio-injection-

# Add Kagenti agent type label (triggers waypoint provisioning)
kubectl label namespace $TARGET_NAMESPACE kagenti.io/type=agent
```

**3.4 Update Agent Deployments**

For each deployment in the namespace:

```bash
# Remove sidecar-specific labels/annotations
kubectl patch deployment my-agent -n $TARGET_NAMESPACE --type=json -p='[
  {
    "op": "remove",
    "path": "/spec/template/metadata/labels/kagenti.io~1client-registration-inject"
  }
]'

# Add waypoint mode label (optional, for documentation)
kubectl patch deployment my-agent -n $TARGET_NAMESPACE --type=json -p='[
  {
    "op": "add",
    "path": "/spec/template/metadata/labels/kagenti.io~1auth-mode",
    "value": "waypoint"
  }
]'
```

**3.5 Trigger Rolling Update**

```bash
# Force pod restart to remove sidecars
kubectl rollout restart deployment -n $TARGET_NAMESPACE

# Wait for rollout to complete
kubectl rollout status deployment -n $TARGET_NAMESPACE --timeout=5m
```

**3.6 Verify Migration**

```bash
# Verify waypoint gateway created
kubectl get gateway -n $TARGET_NAMESPACE

# Verify Istio ambient labels
kubectl get namespace $TARGET_NAMESPACE -o jsonpath='{.metadata.labels}' | jq '. | with_entries(select(.key | startswith("istio")))'

# Verify pods have single container
for deployment in $(kubectl get deployments -n $TARGET_NAMESPACE -o name); do
  echo "Checking $deployment..."
  kubectl get $deployment -n $TARGET_NAMESPACE \
    -o jsonpath='{.spec.template.spec.containers[*].name}' && echo ""
done

# Verify client secrets created
kubectl get secrets -n $TARGET_NAMESPACE | grep kagenti-keycloak-client-credentials
```

**3.7 Validate Agent Communication**

```bash
# Test intra-namespace communication
POD_A=$(kubectl get pod -n $TARGET_NAMESPACE -l app=agent-a -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n $TARGET_NAMESPACE $POD_A -- \
  curl -s http://agent-b.${TARGET_NAMESPACE}.svc.cluster.local:8080/health

# Test cross-namespace communication
kubectl exec -n $TARGET_NAMESPACE $POD_A -- \
  curl -s http://agent-c.other-namespace.svc.cluster.local:8080/health
```

### Phase 4: Cleanup

**4.1 Remove Legacy Secrets** (if applicable)

```bash
# List legacy client-registration secrets
kubectl get secrets -n $TARGET_NAMESPACE | grep -E '(client-registration|sidecar)'

# Delete if no longer needed
kubectl delete secret legacy-client-registration-secret -n $TARGET_NAMESPACE
```

**4.2 Remove Per-Namespace Keycloak Admin Secrets** (IMPORTANT)

```bash
# SECURITY: Remove admin secrets from agent namespaces
# (Only operator namespace should have keycloak-admin-secret)

kubectl delete secret keycloak-admin-secret -n $TARGET_NAMESPACE --ignore-not-found

# Verify only operator namespace has admin secret
kubectl get secret keycloak-admin-secret -A
# Should only show: kagenti-system/keycloak-admin-secret
```

**4.3 Update Monitoring/Alerts**

- Update Prometheus queries to use waypoint gateway metrics
- Update service mesh dashboards to show ambient mesh metrics
- Remove sidecar-specific alerts (e.g., envoy_proxy_down)

---

## Rollback Procedure

If issues occur during migration, follow these steps to rollback:

### Quick Rollback (Restore Sidecar Mode)

**1. Re-enable Sidecar Injection**

```bash
# Remove waypoint labels
kubectl label namespace $TARGET_NAMESPACE kagenti.io/type-

# Re-enable Istio sidecar injection
kubectl label namespace $TARGET_NAMESPACE istio-injection=enabled
```

**2. Update Deployments**

```bash
# Add sidecar mode label
kubectl patch deployment my-agent -n $TARGET_NAMESPACE --type=json -p='[
  {
    "op": "add",
    "path": "/spec/template/metadata/labels/kagenti.io~1client-registration-inject",
    "value": "true"
  }
]'

# Trigger restart
kubectl rollout restart deployment -n $TARGET_NAMESPACE
```

**3. Restore Secrets** (if needed)

```bash
# Restore from backup
kubectl apply -f backup-${TARGET_NAMESPACE}-secrets.yaml
```

**4. Verify Rollback**

```bash
# Verify sidecars injected
kubectl get pods -n $TARGET_NAMESPACE -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].name}{"\n"}{end}'
# Should show: agent, istio-proxy, spiffe-helper, etc.
```

---

## Validation

### Post-Migration Checklist

- [ ] Waypoint gateway running and PROGRAMMED
- [ ] Namespace has Istio ambient labels (`istio.io/dataplane-mode: ambient`)
- [ ] Agent pods have single container (no sidecars)
- [ ] Client credential secrets exist and have correct data
- [ ] Agents can obtain access tokens from Keycloak
- [ ] Intra-namespace communication working
- [ ] Cross-namespace communication working
- [ ] Token exchange working (if configured)
- [ ] No Keycloak admin secrets in agent namespaces
- [ ] Monitoring/alerts updated

### Automated Validation Script

```bash
#!/bin/bash
set -e

NAMESPACE=$1

if [ -z "$NAMESPACE" ]; then
  echo "Usage: $0 <namespace>"
  exit 1
fi

echo "=== Validating waypoint mode migration for $NAMESPACE ==="

# Check waypoint gateway
echo "Checking waypoint gateway..."
kubectl get gateway -n $NAMESPACE ${NAMESPACE}-waypoint \
  -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' | grep -q "True" && \
  echo "✅ Waypoint gateway programmed" || \
  echo "❌ Waypoint gateway not ready"

# Check Istio labels
echo "Checking Istio ambient labels..."
kubectl get namespace $NAMESPACE -o jsonpath='{.metadata.labels.istio\.io/dataplane-mode}' | grep -q "ambient" && \
  echo "✅ Ambient mode enabled" || \
  echo "❌ Ambient mode not enabled"

# Check pod container count
echo "Checking pod topology..."
CONTAINER_COUNT=$(kubectl get pods -n $NAMESPACE -l kagenti.io/type=agent \
  -o jsonpath='{.items[0].spec.containers[*].name}' | wc -w | tr -d ' ')
if [ "$CONTAINER_COUNT" -eq 1 ]; then
  echo "✅ Single-container pods (waypoint mode)"
else
  echo "❌ Multi-container pods (sidecar mode?)"
fi

# Check client secret
echo "Checking client credentials..."
kubectl get secrets -n $NAMESPACE | grep -q kagenti-keycloak-client-credentials && \
  echo "✅ Client credential secret exists" || \
  echo "❌ Client credential secret missing"

# Check admin secret NOT in namespace
echo "Checking security (no admin secret in agent namespace)..."
kubectl get secret keycloak-admin-secret -n $NAMESPACE 2>&1 | grep -q "NotFound" && \
  echo "✅ Admin secret NOT in agent namespace (secure)" || \
  echo "❌ Admin secret found in agent namespace (SECURITY ISSUE)"

echo "=== Validation complete ==="
```

---

## Troubleshooting

### Issue: Waypoint Gateway Not Created

**Symptoms**:
- Namespace labeled but no gateway resource
- Operator logs show no reconciliation events

**Diagnosis**:
```bash
# Check operator is running
kubectl get pods -n kagenti-system

# Check operator logs
kubectl logs -n kagenti-system deployment/kagenti-controller-manager | grep waypoint

# Verify operator flags
kubectl get deployment -n kagenti-system kagenti-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | jq -r '.[]'
```

**Solution**:
1. Verify `--enable-waypoint-provisioning=true` flag set
2. Check operator RBAC has Gateway resource permissions
3. Restart operator if configuration changed

### Issue: Pods Still Have Sidecars After Migration

**Symptoms**:
- Pods show multiple containers after rolling update

**Diagnosis**:
```bash
# Check pod spec
kubectl get pod -n $NAMESPACE <pod-name> -o yaml | grep -A 20 "containers:"

# Check namespace labels
kubectl get namespace $NAMESPACE -o yaml | grep labels -A 10
```

**Possible Causes**:
1. Istio sidecar injection still enabled (`istio-injection=enabled` label)
2. Pod template still has `kagenti.io/client-registration-inject: "true"` label
3. Webhook still configured for sidecar injection

**Solution**:
```bash
# Remove sidecar injection labels
kubectl label namespace $NAMESPACE istio-injection-
kubectl patch deployment <deployment> -n $NAMESPACE --type=json -p='[
  {"op": "remove", "path": "/spec/template/metadata/labels/kagenti.io~1client-registration-inject"}
]'

# Force restart
kubectl rollout restart deployment <deployment> -n $NAMESPACE
```

### Issue: Agent Can't Obtain Access Token

**Symptoms**:
- Agent logs show "client credentials not found"
- HTTP 401 from Keycloak

**Diagnosis**:
```bash
# Check if secret exists
kubectl get secrets -n $NAMESPACE | grep kagenti-keycloak-client-credentials

# Check secret contents
SECRET_NAME=$(kubectl get secrets -n $NAMESPACE -o name | grep kagenti-keycloak-client-credentials | head -1)
kubectl get -n $NAMESPACE $SECRET_NAME -o jsonpath='{.data}' | jq '.'

# Check if mounted in pod
kubectl get pod -n $NAMESPACE <pod-name> -o yaml | grep -A 10 volumeMounts
```

**Solution**:
1. Verify `keycloak-admin-secret` exists in kagenti-system
2. Check operator logs for client registration errors
3. Verify pod has `/shared` volume mount
4. Manually trigger secret recreation:
   ```bash
   kubectl delete secret -n $NAMESPACE $SECRET_NAME
   kubectl rollout restart deployment -n $NAMESPACE
   ```

### Issue: Cross-Namespace Communication Fails

**Symptoms**:
- 503 errors when calling agents in other namespaces
- "upstream connect error" in waypoint logs

**Diagnosis**:
```bash
# Check target namespace has waypoint
kubectl get gateway -n <target-namespace>

# Check target namespace Istio labels
kubectl get namespace <target-namespace> -o jsonpath='{.metadata.labels}' | jq '.'

# Check ztunnel logs
kubectl logs -n istio-system daemonset/ztunnel | grep <pod-ip>
```

**Solution**:
1. Ensure target namespace also migrated to waypoint mode
2. Verify Istio ambient mesh enabled in both namespaces
3. Check network policies not blocking cross-namespace traffic
4. Test with token exchange for proper authorization

---

## Best Practices

1. **Migrate During Low-Traffic Windows**: Schedule migrations during maintenance windows to minimize user impact.

2. **Monitor Closely**: Watch operator logs, waypoint gateway metrics, and application logs during migration.

3. **Test Thoroughly in Staging**: Validate entire workflow (token acquisition, cross-namespace calls, token exchange) in staging before production.

4. **Document Namespace State**: Keep records of which namespaces are sidecar vs waypoint mode during transition period.

5. **Coordinate with Security Team**: Verify admin secret removal from agent namespaces aligns with security policies.

6. **Update Runbooks**: Update incident response procedures to reflect waypoint mode architecture.

---

## Additional Resources

- [Waypoint Mode User Guide](./waypoint-mode.md)
- [Operator-Managed Client Registration](./operator-managed-client-registration.md)
- [Architecture Documentation](./architecture.md)
- [Istio Ambient Mesh Migration](https://istio.io/latest/docs/ambient/migrate-from-sidecar/)
