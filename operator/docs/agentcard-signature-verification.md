# AgentCard Signature Verification

**GitHub Issue:** [#116 - Feature: Strict CardSignature Checking](https://github.com/rossoctl/operator/issues/116)

This guide covers how AgentCard signature verification works in the rossoctl-operator. Agents sign their AgentCards at pod startup using a SPIRE init-container, and the operator verifies signatures using `x5c` certificate chain validation against the SPIRE trust bundle.

---

## How It Works

1. A **SPIRE init-container** runs before the agent container, fetches an X.509-SVID from the SPIRE Workload API, and signs the AgentCard with the SVID private key
2. The signed card includes an `x5c` header containing the full certificate chain
3. The **operator's X5CProvider** validates the certificate chain against the SPIRE trust bundle, extracts the leaf public key, and verifies the JWS signature
4. The **SPIFFE ID** is extracted from the leaf certificate's SAN URI (cryptographically proven, not self-asserted)
5. **Trust-domain binding** validates that the SPIFFE ID belongs to the configured trust domain

### Signature Format (JWS)

```json
{
  "name": "My Agent",
  "url": "http://my-agent:8000",
  "signatures": [
    {
      "protected": "<base64url({alg, kid, typ, x5c})>",
      "signature": "<base64url(JWS-signature)>"
    }
  ]
}
```

The `protected` header contains:
- `alg`: signature algorithm (`RS256`, `ES256`, `ES384`, `ES512`)
- `kid`: SHA-256 fingerprint of the leaf certificate (first 16 hex chars)
- `typ`: `JOSE`
- `x5c`: X.509 certificate chain (leaf-first, standard base64)

### Verification Steps

1. Extract `x5c` from the JWS protected header
2. Validate the certificate chain against the SPIRE X.509 trust bundle
3. Validate the leaf certificate has exactly one `spiffe://` SAN URI
4. Extract the leaf public key
5. Create canonical JSON payload (sorted keys, no whitespace, `signatures` excluded)
6. Verify the JWS signature against the leaf public key
7. Extract the SPIFFE ID from the leaf cert SAN for identity binding

---

## Prerequisites

| Requirement | Notes |
|-------------|-------|
| Kubernetes 1.25+ / OpenShift 4.12+ | Init-containers, emptyDir volumes |
| SPIRE Server + Agent | Deployed cluster-wide (DaemonSet) |
| `spire-controller-manager` | Automates workload registration via `ClusterSPIFFEID` |
| SPIFFE CSI driver | Exposes SPIRE socket to pods |
| Trust bundle in ConfigMap | SPIRE's `BundlePublisher` `k8s_configmap` plugin maintains the trust bundle automatically |

---

## Setup

### 1. Register Rossoctl agents with SPIRE

Create a `ClusterSPIFFEID` so SPIRE automatically issues SVIDs to agent pods:

```yaml
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: rossoctl-agents
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      rossoctl.io/type: agent
  namespaceSelector:
    matchLabels: {}
```

### 2. Trust bundle (automatic via SPIRE BundlePublisher)

The operator reads the SPIRE trust bundle from the ConfigMap that SPIRE's
`BundlePublisher` `k8s_configmap` plugin maintains automatically. No manual
Secret creation is needed — the SPIRE server keeps this ConfigMap up to date
on every CA rotation.

### 3. Configure the operator

```bash
--require-a2a-signature=true
--spire-trust-domain=<your-trust-domain>
--spire-trust-bundle-configmap=spire-bundle
--spire-trust-bundle-configmap-namespace=spire-system
```

### 4. Deploy an agent with the signing init-container

See `demos/agentcard-spire-signing/` for complete manifests and a runnable demo. The key elements:

```yaml
initContainers:
  - name: sign-agentcard
    image: rossoctl/agentcard-signer:latest
    env:
      - name: SPIFFE_ENDPOINT_SOCKET
        value: unix:///run/spire/agent-sockets/spire-agent.sock
      - name: UNSIGNED_CARD_PATH
        value: /etc/agentcard/agent.json
      - name: AGENT_CARD_PATH
        value: /app/.well-known/agent-card.json
      - name: SIGN_TIMEOUT
        value: "30s"
    volumeMounts:
      - name: spire-agent-socket
        mountPath: /run/spire/agent-sockets
        readOnly: true
      - name: unsigned-card
        mountPath: /etc/agentcard
        readOnly: true
      - name: signed-card
        mountPath: /app/.well-known
```

### 5. Create the AgentCard CR

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: weather-agent-card
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent
  identityBinding:
    strict: true
```

### 6. Verify

```bash
kubectl get agentcard weather-agent-card -o yaml
```

Expected status:
- `validSignature: true`
- `signatureSpiffeId: spiffe://<trust-domain>/ns/<ns>/sa/<sa>`
- `bindingStatus.bound: true`
- `conditions[type=SignatureVerified].status: True`

---

## Operator Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--require-a2a-signature` | `false` | Require AgentCards to have valid JWS signatures |
| `--spire-trust-domain` | (none) | SPIRE trust domain for binding validation (required) |
| `--spire-trust-bundle-configmap` | (none) | ConfigMap containing the SPIRE trust bundle (SPIFFE JSON) |
| `--spire-trust-bundle-configmap-namespace` | (none) | Namespace of the trust bundle ConfigMap |
| `--spire-trust-bundle-configmap-key` | `bundle.spiffe` | Key in the ConfigMap containing the SPIFFE JSON data |
| `--spire-trust-bundle-refresh-interval` | `5m` | How often to re-read the trust bundle |
| `--svid-expiry-grace-period` | `30m` | How far before SVID expiry to trigger proactive workload restart |
| `--signature-audit-mode` | `false` | Log failures without blocking |
| `--enforce-network-policies` | `false` | Create NetworkPolicies for signature enforcement |

## Init-Container Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SPIFFE_ENDPOINT_SOCKET` | `unix:///run/spire/agent-sockets/spire-agent.sock` | SPIRE Workload API socket |
| `UNSIGNED_CARD_PATH` | `/etc/agentcard/agent.json` | Path to read the unsigned card |
| `AGENT_CARD_PATH` | `/app/.well-known/agent-card.json` | Path to write the signed card |
| `SIGN_TIMEOUT` | `30s` | Timeout for SPIRE connection |

## Prometheus Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `rossoctl_x5c_chain_validation_total` | Counter | Chain validation attempts (labels: valid, reason) |
| `rossoctl_x5c_trust_bundle_age_seconds` | Gauge | Age of the cached trust bundle |
| `rossoctl_x5c_trust_bundle_load_errors_total` | Counter | Trust bundle load failures |
| `rossoctl_x5c_binding_trust_domain_mismatch_total` | Counter | Trust domain mismatches |

## Troubleshooting

| Issue | Cause | Fix |
|-------|-------|-----|
| Init-container `CrashLoopBackOff` | SPIRE socket not mounted or no `ClusterSPIFFEID` matching | Check CSI driver; check `ClusterSPIFFEID` selectors |
| `"No signature verified via x5c chain validation"` | Trust bundle ConfigMap missing or stale | Verify ConfigMap exists with valid SPIFFE JSON |
| `"x5c chain validation failed"` | Cert signed by untrusted CA | Ensure trust bundle matches the SPIRE CA |
| `bindingStatus.bound: false` | Trust domain mismatch | Check `--spire-trust-domain` matches the SVID's trust domain |
| `"x5c header missing"` | Card served without signatures | Verify agent reads from the signed-card emptyDir |

```bash
# Debug commands
kubectl logs -n <ns> <pod> -c sign-agentcard          # Init-container logs
kubectl get agentcard <name> -o yaml                    # Full status
kubectl get configmap spire-bundle -n spire-system       # Trust bundle exists
```
