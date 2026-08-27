# Token Exchange Testing Guide

This document describes how to test JWT-SVID fetching and token exchange functionality.

## Test Levels

### 1. Unit Tests

**Location:** `operator/internal/controller/clientregistration_controller_fetchjwtsvid_test.go`

**What they test:**
- Error handling when SPIFFE socket is invalid
- Error handling when SPIFFE socket path is empty
- Method signature and basic validation

**Limitations:**
- Cannot test actual SPIRE Workload API interaction without a running SPIRE agent
- Cannot verify JWT-SVID content or token exchange flow

**Run them:**
```bash
cd operator
go test -v -run TestFetchJWTSVID ./internal/controller/
```

### 2. Integration Tests

**Not yet implemented** - Would require:
- Mock SPIFFE Workload API server implementation
- Test fixtures for JWT-SVID responses
- Mock Keycloak IDP for token exchange verification

**Future work:**
- Implement mock Workload API using gRPC server
- Test JWT-SVID marshaling/unmarshaling
- Test Keycloak JWT-SVID grant flow

### 3. E2E Tests

**Location:** Main repo `rossoctl/tests/e2e/`

**What they test:**
- Full operator deployment with SPIRE
- Client registration with SPIFFE auth enabled
- Token exchange in weather agent demo
- AuthBridge routes configuration and matching

**Prerequisites:**
- Kind cluster with SPIRE deployed
- Keycloak with SPIFFE IDP configured
- Operator deployed with `--use-spiffe-auth=true`

**Run them:**
```bash
# From rossoctl repo root
./.github/scripts/local-setup/kind-full-test.sh --skip-cluster-destroy

# Verify operator has SPIFFE auth enabled
kubectl get deployment rossoctl-controller-manager -n rossoctl-system \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | grep use-spiffe-auth

# Check client registration logs
kubectl logs -n rossoctl-system deployment/rossoctl-controller-manager \
  | grep "authenticated with JWT-SVID"

# Deploy weather agents and verify token exchange
./.github/scripts/operator/72-deploy-weather-tool.sh
./.github/scripts/operator/74-deploy-weather-agent.sh

# Check AuthBridge logs for token exchange activity
kubectl logs -n team1 -l app.kubernetes.io/name=weather-service -c authbridge-proxy \
  | grep "token-exchange"
```

## Testing Token Exchange Flow

### Manual Verification

1. **Verify operator fetches JWT-SVID:**
```bash
# Check operator logs for SPIFFE Workload API client creation
kubectl logs -n rossoctl-system deployment/rossoctl-controller-manager \
  | grep -i "spiffe\|jwt-svid"
```

2. **Verify Keycloak client registration:**
```bash
# Check that client secret was created with federated-jwt mode
kubectl get secrets -n team1 | grep keycloak-client-credentials

# Inspect secret contents (should have client-id.txt, may have client-secret.txt)
kubectl get secret <secret-name> -n team1 -o jsonpath='{.data}' | jq 'keys'
```

3. **Verify AuthBridge token exchange:**
```bash
# Get weather-service pod
POD=$(kubectl get pod -n team1 -l app.kubernetes.io/name=weather-service -o jsonpath='{.items[0].metadata.name}')

# Trigger outbound request
kubectl exec -n team1 $POD -c agent -- python3 -c "
import urllib.request
url = 'http://weather-tool-mcp.team1.svc.cluster.local:8000/mcp'
data = b'{\"method\": \"tools/list\"}'
req = urllib.request.Request(url, data=data, headers={'Content-Type': 'application/json'})
try:
    response = urllib.request.urlopen(req, timeout=10)
    print('Success:', response.status)
except Exception as e:
    print('Request sent:', str(e)[:100])
"

# Check AuthBridge logs for token-exchange activity
kubectl logs -n team1 $POD -c authbridge-proxy --tail=50 | grep token-exchange
```

### Automated E2E Test Script

See `/tmp/rossoctl/e2e/spiffe-sdk-test.sh` for a complete automated test that:
1. Deploys operator with updated image
2. Verifies single-container pod (no spiffe-helper sidecar)
3. Deploys weather agents with SPIFFE identity
4. Triggers token exchange
5. Verifies logs show successful token exchange

## Adding New Tests

### Unit Test Template

```go
func TestFetchJWTSVID_YourCase(t *testing.T) {
    r := &ClientRegistrationReconciler{
        SpiffeSocket: "unix:///your/socket/path",
    }

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    svid, err := r.fetchJWTSVID(ctx, "your-audience")
    
    // Assert expected behavior
    if err != nil {
        t.Fatalf("expected success, got error: %v", err)
    }
    
    if svid == "" {
        t.Fatal("expected non-empty JWT-SVID")
    }
}
```

### Integration Test (Future)

Would require mock Workload API implementation:

```go
// mockWorkloadAPI implements SPIFFE Workload API gRPC service
type mockWorkloadAPI struct {
    // Store test JWT-SVIDs
    svids map[string]string
}

func TestFetchJWTSVID_WithMockWorkloadAPI(t *testing.T) {
    // 1. Start mock gRPC server
    // 2. Create ClientRegistrationReconciler with mock socket
    // 3. Call fetchJWTSVID()
    // 4. Verify returned JWT-SVID matches mock response
}
```

## Common Issues

### "failed to create SPIFFE Workload API client"

- SPIRE agent is not running
- Socket path is incorrect
- Socket permissions are wrong
- Pod doesn't have SPIRE volume mount

### "failed to fetch JWT-SVID"

- Workload not attested by SPIRE agent
- Audience doesn't match any SPIRE registration entries
- SPIRE server trust domain mismatch

### Token exchange fails in AuthBridge

- Keycloak SPIFFE IDP not configured
- JWT-SVID audience doesn't match Keycloak realm
- Client not registered in Keycloak
- AuthBridge identity.type not set to "spiffe"

## Related Documentation

- [SPIFFE Workload API Spec](https://github.com/spiffe/spiffe/blob/main/standards/SPIFFE_Workload_API.md)
- [go-spiffe Documentation](https://pkg.go.dev/github.com/spiffe/go-spiffe/v2)
- [Rossoctl E2E Testing](../../../rossoctl/tests/e2e/README.md)
- [AuthBridge Token Exchange](../../../rossoctl/auth/authbridge-proxy/docs/token-exchange.md)
