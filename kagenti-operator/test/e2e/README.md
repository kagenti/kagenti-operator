# E2E Tests

End-to-end tests for the kagenti-operator. The suite runs 11 specs:

- **Manager tests** (2 specs) — controller pod readiness and Prometheus metrics
- **AgentCard tests** (6 specs) — webhook validation, auto-discovery, duplicate prevention, audit mode, and SPIRE signature verification
- **ClientRegistration tests** (3 specs) — Keycloak client registration, SPIFFE ID templates, and Secret creation

## Prerequisites

- [Kind](https://kind.sigs.k8s.io/) — `go install sigs.k8s.io/kind@latest`
- [Helm](https://helm.sh/) — `brew install helm`
- [kubectl](https://kubernetes.io/docs/tasks/tools/) — `brew install kubectl`
- Container runtime: **Docker** or **Podman**

The test suite auto-detects Docker vs Podman. No env vars needed.

## Run

```bash
# Create a fresh Kind cluster
kind delete cluster 2>/dev/null; kind create cluster

# Run all 11 specs (~9 min)
make test-e2e
```

The suite automatically builds/loads images, installs Prometheus, CertManager, SPIRE, deploys the controller, runs tests, and tears everything down.

## Skip pre-installed components

If Prometheus, CertManager, or SPIRE are already on your cluster:

```bash
PROMETHEUS_INSTALL_SKIP=true \
CERT_MANAGER_INSTALL_SKIP=true \
SPIRE_INSTALL_SKIP=true \
make test-e2e
```

## Run specific scenarios

```bash
# Webhook + auto-discovery tests only (~4 min)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="should reject AgentCard|should not create|should auto-create|should reject duplicate"

# Signature verification tests only (~5 min)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="SignatureInvalidAudit|should verify signed"

# ClientRegistration tests only (~3 min)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="ClientRegistration"

# Manager tests only
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="Manager"
```

## Cleanup

```bash
kind delete cluster
```

## Test scenarios

| Scenario | Context | What it tests |
|----------|---------|---------------|
| Reject missing targetRef | Without signature | Webhook rejects AgentCard with no `spec.targetRef` |
| No protocol label | Without signature | Workload with `kagenti.io/type=agent` but no `protocol.kagenti.io/*` label gets no auto-created card |
| Auto-discovery | Without signature | Properly labeled workload gets an auto-created AgentCard with correct targetRef, protocol, and Synced=True |
| Duplicate prevention | Without signature | Webhook rejects a second AgentCard targeting the same workload |
| Audit mode | With signature | Unsigned card syncs (Synced=True) but reports SignatureVerified=False with reason SignatureInvalidAudit |
| Signed agent | With signature | SPIRE-signed card gets SignatureVerified=True, correct SPIFFE ID, Synced=True, and Bound=True |
| Client registration (non-SPIRE) | ClientRegistration | Controller creates client credentials Secret with `namespace/workloadName` format and patches pod template annotation |
| Client registration (SPIRE default) | ClientRegistration | Controller creates SPIFFE ID using default template `spiffe://{domain}/ns/{namespace}/sa/{serviceAccount}` |
| Client registration (SPIRE custom) | ClientRegistration | Controller respects `SPIRE_ID_TEMPLATE` env var for custom SPIFFE ID formats |

## Architecture

### What gets installed

The test suite sets up the following infrastructure in a Kind cluster:

```
BeforeSuite (once per suite)
├── Build & load operator image into Kind
├── Install Prometheus Operator v0.77.1 (metrics/ServiceMonitor CRDs)
├── Install CertManager v1.16.3 (webhook TLS certificates)
├── Build & load agentcard-signer image into Kind
└── Install SPIRE via Helm (spire-crds v0.5.0 + spire v0.28.3)

BeforeAll (per Describe block)
├── make install → applies AgentCard CRD via kustomize
├── make deploy → creates namespace, RBAC, Deployment, webhook, ServiceMonitor
├── Wait for controller pod Running + webhook endpoint ready
└── Create test namespace e2e-agentcard-test (labeled agentcard=true + PSA restricted)
```

### How the operator is installed

```
make docker-build           make install               make deploy
      │                          │                          │
      ▼                          ▼                          ▼
Build image from          kustomize build config/crd   kustomize edit set image
Dockerfile                       │                          │
      │                   kubectl apply --server-side  kustomize build config/default
      ▼                          │                          │
kind load docker-image           ▼                   kubectl apply --server-side
(podman fallback)         AgentCard CRD created              │
                                                             ▼
                                                     kagenti-operator-system:
                                                     ├── ServiceAccount
                                                     ├── ClusterRole + Binding
                                                     ├── Certificate + Issuer (cert-manager)
                                                     ├── Webhook Service (port 443)
                                                     ├── Metrics Service (port 8443)
                                                     ├── Deployment (controller pod)
                                                     ├── ValidatingWebhookConfiguration
                                                     └── ServiceMonitor (Prometheus)
```

### Component interactions

```
┌─ cert-manager ───────────────────────────────────────────────────┐
│  Issues TLS cert for operator webhook                             │
│  Injects CA into ValidatingWebhookConfiguration                  │
└───────────────────────────┬──────────────────────────────────────┘
                            │ TLS cert
                            ▼
┌─ kagenti-operator-system ────────────────────────────────────────┐
│  Controller Manager Pod                                           │
│  ├── Webhook server (validates AgentCard create/update)           │
│  ├── Metrics server (HTTPS, scraped by Prometheus)                │
│  ├── AgentCardSync controller                                     │
│  │   watches Deployments → auto-creates AgentCards                │
│  └── AgentCard controller                                         │
│      fetches card metadata, verifies signatures, evaluates binding│
└───────────────────────────┬──────────────────────────────────────┘
                            │ fetches /.well-known/agent-card.json
                            ▼
┌─ e2e-agentcard-test ─────────────────────────────────────────────┐
│  Agent Deployments (echo-agent, audit-agent, signed-agent)        │
│  Services (expose agents for card fetching)                       │
│  AgentCard CRs (auto-created or manually applied)                 │
└───────────────────────────▲──────────────────────────────────────┘
                            │ SPIRE CSI volume provides SVIDs
┌─ spire-system ───────────┴──────────────────────────────────────┐
│  SPIRE Server → issues SVIDs via ClusterSPIFFEID policies         │
│  SPIRE Agent (DaemonSet) → distributes SVIDs via CSI driver       │
│  spire-bundle ConfigMap → CA certs for signature verification     │
└──────────────────────────────────────────────────────────────────┘
```

### Test scenario details

#### Reject missing targetRef

Applies an AgentCard with no `spec.targetRef`. The validating webhook checks
`agentcard.Spec.TargetRef != nil` and rejects with `"spec.targetRef is required"`.

#### No protocol label

Deploys `noproto-agent` with `kagenti.io/type=agent` but no `protocol.kagenti.io/*` label.
The sync controller's `shouldSyncWorkload()` requires both the agent type AND a protocol
label, so it skips this workload. The test uses `Consistently` for 15s to prove no card appears.

#### Auto-discovery

Deploys `echo-agent` with both labels plus an inline Python HTTP server serving
`/.well-known/agent-card.json`. The sync controller auto-creates `echo-agent-deployment-card`.
The main controller reconciles it: fetches the card JSON from the Service endpoint, extracts
protocol from labels, and sets `Synced=True`. Test verifies managed-by label, targetRef fields,
protocol, and sync status.

#### Duplicate prevention

With `echo-agent-deployment-card` still present from the previous test (ordered container),
attempts to create `echo-agent-manual-card` targeting the same Deployment. The webhook's
`checkDuplicateTargetRef()` lists all AgentCards in the namespace, finds the existing card
with matching targetRef, and rejects with `"an AgentCard already targets"`.

#### Audit mode

Controller is patched with `--require-a2a-signature=true --signature-audit-mode=true`.
Deploys unsigned `audit-agent`. The controller verifies the signature (fails — no signature),
but audit mode allows sync to proceed. Status shows `Synced=True` and
`SignatureVerified=False` with reason `SignatureInvalidAudit`.

#### Signed agent

The most complex scenario. Controller runs with `--require-a2a-signature=true` (no audit mode).

1. **ClusterSPIFFEID** tells SPIRE to issue SVIDs to agent pods
2. **signed-agent** Deployment uses an `agentcard-signer` init-container that:
   - Connects to SPIRE agent via CSI-mounted socket
   - Signs the unsigned card JSON with the pod's SVID
   - Writes signed card to a shared emptyDir volume
3. Main container serves the signed card via HTTP
4. Controller fetches the card, verifies the x5c signature chain against the SPIRE trust
   bundle, extracts the SPIFFE ID from the leaf cert SAN
5. Identity binding checks that the SPIFFE ID belongs to the configured trust domain

Test verifies: `SignatureVerified=True` (reason `SignatureValid`),
`signatureSpiffeId = spiffe://example.org/ns/e2e-agentcard-test/sa/signed-agent-sa`,
`Synced=True`, `Bound=True`.

#### Client registration (non-SPIRE mode)

Controller is patched with `--enable-operator-client-registration=true --spire-trust-domain=example.org`.
Deploys a mock Keycloak server, `authbridge-config` ConfigMap with `SPIRE_ENABLED=false`, and
`keycloak-admin-secret`. Deploys `clientreg-agent` workload.

1. Controller watches the Deployment (has `kagenti.io/type=agent` label)
2. Reads `authbridge-config` and `keycloak-admin-secret` from workload namespace
3. Computes client ID as `namespace/workloadName` (non-SPIRE format)
4. Registers client with mock Keycloak via admin API
5. Creates Secret with `client-id.txt` and `client-secret.txt` (owner: Deployment)
6. Patches Deployment pod template with `kagenti.io/keycloak-client-credentials-secret-name` annotation

Test verifies: Secret created, contains both keys, client ID format is `e2e-clientreg-test/clientreg-agent`,
and pod template annotation references the Secret name.

#### Client registration (SPIRE mode with default template)

Updates `authbridge-config` to `SPIRE_ENABLED=true`. Deploys `clientreg-spire-agent` with dedicated
ServiceAccount `clientreg-spire-sa`.

1. Controller detects SPIRE is enabled
2. Validates ServiceAccount is not `default`
3. Renders default SPIFFE ID template: `spiffe://{{.TrustDomain}}/ns/{{.Namespace}}/sa/{{.ServiceAccount}}`
4. Computes client ID as `spiffe://example.org/ns/e2e-clientreg-test/sa/clientreg-spire-sa`
5. Registers client and creates Secret with SPIFFE ID as client ID

Test verifies: Client ID matches expected SPIFFE format with default template.

#### Client registration (SPIRE mode with custom template)

Patches operator Deployment to set env var `SPIRE_ID_TEMPLATE=spiffe://{{.TrustDomain}}/workload/{{.Namespace}}/{{.ServiceAccount}}`.
Waits for operator restart. Deploys `clientreg-custom-agent` with ServiceAccount `clientreg-custom-sa`.

1. Controller reads custom template from environment variable
2. Renders custom template with workload metadata
3. Computes client ID as `spiffe://example.org/workload/e2e-clientreg-test/clientreg-custom-sa`
4. Registers client and creates Secret

Test verifies: Client ID matches expected SPIFFE format with custom template. Restores operator
to default configuration after test.

## Troubleshooting

**Stale cluster state** — if you see errors about namespaces being terminated or cert-manager TLS failures, delete and recreate the cluster:

```bash
kind delete cluster && kind create cluster
```

**Podman socket errors** — ensure your Podman machine is running:

```bash
podman machine start
```

**Override container tool** — if auto-detection picks the wrong runtime:

```bash
CONTAINER_TOOL=podman make test-e2e
```
