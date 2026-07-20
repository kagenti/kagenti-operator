# E2E Tests

End-to-end tests for the rossoctl-operator. The suite runs 25 specs:

- **Manager tests** (2 specs) — controller pod readiness and Prometheus metrics
- **AuthBridge Injection tests** (4 specs) — sidecar injection, idempotency, opt-out, and HTTP validation
- **AgentCard tests** (6 specs) — webhook validation, auto-discovery, duplicate prevention, audit mode, and SPIRE signature verification
- **AgentRuntime tests** (8 specs) — label application, status lifecycle, idempotency, error handling, tool type, StatefulSet support, identity/trace overrides, and deletion cleanup
- **Combined tests** (5 specs) — AgentRuntime + AgentCard + Auth Bridge integration: labels, auto-created card, sidecar injection, identity binding, and deletion cleanup

## Prerequisites

- [Kind](https://kind.sigs.k8s.io/) — `go install sigs.k8s.io/kind@latest`
- [Helm](https://helm.sh/) — `brew install helm`
- [kubectl](https://kubernetes.io/docs/tasks/tools/) — `brew install kubectl`
- Container runtime: **Docker** or **Podman**

The test suite auto-detects Docker vs Podman. No env vars needed. AuthBridge sidecar images (`authbridge-envoy`, `proxy-init`, `spiffe-helper`) are pulled from `ghcr.io/rossoctl/cortex` and loaded into Kind during setup.

## Run

```bash
# Create a fresh Kind cluster
kind delete cluster 2>/dev/null; kind create cluster

# Run all 25 specs (~15 min)
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
# AuthBridge injection tests only (~3 min)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="AuthBridge"

# Webhook + auto-discovery tests only (~4 min)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="should reject AgentCard|should not create|should auto-create|should reject duplicate"

# Signature verification tests only (~5 min)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="SignatureInvalidAudit|should verify signed"

# Manager tests only
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="Manager"

# AgentRuntime tests only (~3 min)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="AgentRuntime E2E"

```

## Cleanup

```bash
kind delete cluster
```

## Test scenarios

| Scenario | Context | What it tests |
|----------|---------|---------------|
| Inject sidecars | AuthBridge injection | Webhook injects `envoy-proxy`, `spiffe-helper` containers, `proxy-init` init container, and expected volumes |
| Idempotency | AuthBridge injection | Pod recreation produces exactly 1 of each sidecar (no duplicates) |
| Injection opt-out | AuthBridge injection | Pod with `rossoctl.io/inject=disabled` label gets zero sidecars |
| HTTP validation | AuthBridge injection | Traffic flows through injected envoy proxy (curl → service → iptables → envoy → app) |
| Reject missing targetRef | Without signature | Webhook rejects AgentCard with no `spec.targetRef` |
| No protocol label | Without signature | Workload with `rossoctl.io/type=agent` but no `protocol.rossoctl.io/*` label gets no auto-created card |
| Auto-discovery | Without signature | Properly labeled workload gets an auto-created AgentCard with correct targetRef, protocol, and Synced=True |
| Duplicate prevention | Without signature | Webhook rejects a second AgentCard targeting the same workload |
| Audit mode | With signature | Unsigned card syncs (Synced=True) but reports SignatureVerified=False with reason SignatureInvalidAudit |
| Signed agent | With signature | SPIRE-signed card gets SignatureVerified=True, correct SPIFFE ID, Synced=True, and Bound=True |
| Apply labels and config-hash | Agent lifecycle | AgentRuntime controller adds `rossoctl.io/type=agent`, `managed-by`, config-hash, and triggers AgentCard auto-creation |
| Ready=True | Agent lifecycle | AgentRuntime CR reaches Ready=True condition |
| Idempotent re-reconcile | Agent lifecycle | Deployment generation stays stable over 30s (no spurious updates) |
| Clean up on deletion | Agent lifecycle | Deletion removes `rossoctl.io/type`, `managed-by`, and config-hash from the workload |
| Missing target error | Error cases | AgentRuntime targeting non-existent Deployment sets TargetResolved=False |
| Tool type label | Tool type | AgentRuntime with type=tool applies `rossoctl.io/type=tool` label and no AgentCard is created |
| StatefulSet target | StatefulSet target | AgentRuntime applies labels, config-hash, and reaches Active for a StatefulSet workload |
| CR overrides | CR overrides | AgentRuntime with CR overrides produces the same config-hash as a minimal CR (CR fields excluded from hash) |
## Architecture

### What gets installed

The test suite sets up the following infrastructure in a Kind cluster:

```
BeforeSuite (once per suite)
├── Build & load operator image into Kind
├── Install Prometheus Operator v0.77.1 (metrics/ServiceMonitor CRDs)
├── Install CertManager v1.16.3 (webhook TLS certificates)
├── Build & load agentcard-signer image into Kind
├── Pull & load AuthBridge sidecar images (authbridge-envoy, proxy-init, spiffe-helper)
└── Install SPIRE via Helm (spire-crds v0.5.0 + spire v0.28.3)

BeforeAll (per Describe block)
├── make install → applies AgentCard + AgentRuntime CRDs via kustomize
├── make deploy → creates namespace, RBAC, Deployment, webhook, ServiceMonitor
├── Wait for controller pod Running + webhook endpoint ready
└── Create test namespace:
    ├── e2e-authbridge-test (rossoctl-enabled=true, PSA privileged — for proxy-init NET_ADMIN)
    └── e2e-agentcard-test (agentcard=true, PSA restricted)
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
                                                     rossoctl-operator-system:
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
┌─ rossoctl-operator-system ────────────────────────────────────────┐
│  Controller Manager Pod                                           │
│  ├── Mutating webhook (injects AuthBridge sidecars into pods)     │
│  ├── Validating webhook (validates AgentCard/AgentRuntime CRs)    │
│  ├── Metrics server (HTTPS, scraped by Prometheus)                │
│  ├── AgentCardSync controller                                     │
│  │   watches Deployments → auto-creates AgentCards                │
│  └── AgentCard controller                                         │
│      fetches card metadata, verifies signatures, evaluates binding│
└──────────┬────────────────────────────────┬──────────────────────┘
           │ injects sidecars               │ fetches agent-card.json
           ▼                                ▼
┌─ e2e-authbridge-test ────────────┐ ┌─ e2e-agentcard-test ────────┐
│  authbridge-agent Deployment      │ │  Agent Deployments           │
│  ├── echo container (app:8080)    │ │  (echo, audit, signed)       │
│  ├── envoy-proxy (injected)       │ │  Services + AgentCard CRs    │
│  ├── spiffe-helper (injected)     │ └──────────▲─────────────────┘
│  └── proxy-init (injected init)   │            │ SPIRE CSI volumes
│  disabled-agent (no injection)    │            │
│  ConfigMaps + AgentRuntime CR     │            │
└──────────────────────────────────┘            │
┌─ spire-system ───────────┴──────────────────────────────────────┐
│  SPIRE Server → issues SVIDs via ClusterSPIFFEID policies         │
│  SPIRE Agent (DaemonSet) → distributes SVIDs via CSI driver       │
│  spire-bundle ConfigMap → CA certs for signature verification     │
└──────────────────────────────────────────────────────────────────┘
```

### AgentRuntime test infrastructure

The AgentRuntime E2E tests use a separate namespace (`e2e-agentruntime-test`) and lightweight
`pause:3.9` containers (no HTTP serving needed). The test creates ConfigMap fixtures to exercise
the 2-layer config merge:

```
BeforeAll (AgentRuntime E2E)
├── Deploy controller (make install + make deploy)
├── Wait for controller pod Running + webhook endpoint ready
├── Create namespace e2e-agentruntime-test (PSA restricted)
├── Ensure rossoctl-system namespace exists
├── Create rossoctl-platform-config ConfigMap (cluster defaults, layer 1)
└── Create runtime-ns-defaults ConfigMap (namespace defaults, layer 2)
```

```
AgentRuntime controller flow:
                                                    ┌─ rossoctl-system ──────────────────┐
                                                    │  rossoctl-platform-config ConfigMap │
                                                    │  (cluster defaults, layer 1)       │
                                                    └────────────┬──────────────────────┘
                                                                 │
┌─ AgentRuntime CR ─────────┐     ┌─ Controller ────────────┐    │    ┌─ e2e-agentruntime-test ────────┐
│  spec.type: agent         │────▶│  Resolve target          │◀───┘    │  runtime-ns-defaults ConfigMap  │
│  spec.targetRef:          │     │  Resolve config (2-layer)│◀────────│  (namespace defaults, layer 2)  │
│    name: runtime-agent-   │     │  Apply labels + hash     │         │                                  │
│          target           │     └──────────┬───────────────┘         │  runtime-agent-target Deployment │
└───────────────────────────┘                │                         │  runtime-tool-target Deployment  │
                                             │                         │  runtime-sts-target StatefulSet  │
                                             │                         │  runtime-minimal-target Deploy.  │
                                             │                         │  runtime-overrides-target Deploy.│
                                             │                         └──────────────────────────────────┘
                                             ▼
                                  Target Deployment updated:
                                  ├── rossoctl.io/type = agent|tool
                                  ├── app.kubernetes.io/managed-by = rossoctl-operator
                                  └── rossoctl.io/config-hash = SHA256 (pod template annotation)
```

**Cross-controller note:** The agent target fixture includes `protocol.rossoctl.io/a2a`. When
AgentRuntime adds `rossoctl.io/type=agent`, AgentCardSync auto-creates an AgentCard that fails
to sync (pause container serves no HTTP). This is expected and harmless.

### Test scenario details

#### Inject sidecars

Deploys `authbridge-agent` with `rossoctl.io/type=agent` label and a matching AgentRuntime CR.
The mutating webhook (`inject.rossoctl.io`) intercepts the pod CREATE, calls `InjectAuthBridge`,
and adds: `envoy-proxy` sidecar (envoy with passthrough config), `spiffe-helper` sidecar
(manages SVID rotation), and `proxy-init` init container (sets up iptables rules for traffic
interception as root with NET_ADMIN/NET_RAW). The test verifies container names, init container
names, and injected volumes (`shared-data`, `spire-agent-socket`, `spiffe-helper-config`,
`svid-output`, `envoy-config`, `authproxy-routes`).

#### Idempotency

Deletes the running `authbridge-agent` pod and waits for the Deployment to recreate it.
Verifies the new pod has exactly 1 `envoy-proxy`, 1 `spiffe-helper`, and 1 `proxy-init` —
no duplicate injection occurs on pod recreation.

#### Injection opt-out

Deploys `disabled-agent` with `rossoctl.io/inject=disabled` label on the pod template.
The webhook's `objectSelector` (matching `rossoctl.io/inject NOT IN [disabled]`) filters
this pod at the API server level, so the webhook handler is never called. The test verifies
zero sidecar containers and zero init containers.

#### HTTP validation

Waits for `envoy-proxy` to be ready, then:
1. Execs into the echo container and curls envoy admin at `127.0.0.1:9901/server_info` (asserts 200)
2. Runs a curl pod targeting the `authbridge-agent` Service on port 8080
3. Asserts HTTP 200, proving traffic flows: curl → Service → iptables → envoy:15124 → app:8080

#### Reject missing targetRef

Applies an AgentCard with no `spec.targetRef`. The validating webhook checks
`agentcard.Spec.TargetRef != nil` and rejects with `"spec.targetRef is required"`.

#### No protocol label

Deploys `noproto-agent` with `rossoctl.io/type=agent` but no `protocol.rossoctl.io/*` label.
The sync controller's `shouldSyncWorkload()` requires both the agent type AND a protocol
label, so it skips this workload. The test uses `Consistently` for 30s to prove no card appears.

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

#### Apply labels and config-hash

Deploys `runtime-agent-target` (pause container with `protocol.rossoctl.io/a2a` label) and
creates an AgentRuntime CR with `type: agent` targeting it. The controller resolves the target,
merges 2-layer config (cluster ConfigMap `rossoctl-platform-config` in `rossoctl-system` +
namespace ConfigMap with `rossoctl.io/defaults=true` + CR-level overrides), and applies labels
to the Deployment. Test verifies `rossoctl.io/type=agent` on both workload metadata and pod
template, `app.kubernetes.io/managed-by=rossoctl-operator` on workload metadata, and
`rossoctl.io/config-hash` annotation (non-empty, 64 hex chars) on the pod template. Also
verifies the cross-controller interaction: once `rossoctl.io/type=agent` is applied alongside
the existing `protocol.rossoctl.io/a2a` label, AgentCardSync auto-creates an AgentCard
(`runtime-agent-target-deployment-card`) with the correct `managed-by` label and targetRef.

#### Ready=True

Uses the AgentRuntime CR from the previous test (ordered context). Once the controller has
resolved the target and applied configuration, it sets the `Ready` condition to `True`.
Test verifies via jsonpath.

#### Idempotent re-reconcile

With the AgentRuntime CR and target Deployment already reconciled, records the Deployment's
`metadata.generation` and asserts it stays constant over 30 seconds using `Consistently`.
A generation change would indicate the controller is making spurious updates to the Deployment
spec on each reconcile loop, which would trigger unnecessary rolling restarts.

#### Clean up on deletion

Deletes the AgentRuntime CR and verifies the finalizer (`rossoctl.io/cleanup`) runs correctly:

1. **Target Deployment still exists** — the controller cleans up labels, not the workload
2. **`rossoctl.io/type` removed** — the label was applied by the operator and is removed on cleanup
3. **`app.kubernetes.io/managed-by` removed** — workload is no longer operator-managed
4. **`rossoctl.io/config-hash` removed** — annotation is cleared since the workload is no longer managed
5. **AgentRuntime CR returns 404** — finalizer completed and CR was fully deleted

#### Missing target error

Creates an AgentRuntime CR targeting `nonexistent-deployment`. The controller's target
resolution fails because no Deployment with that name exists. The controller sets
`TargetResolved=False`. Test verifies via jsonpath, then cleans up the CR.

#### Tool type label

Deploys `runtime-tool-target` (pause container without protocol labels) and creates an
AgentRuntime CR with `type: tool`. The controller applies `rossoctl.io/type=tool` to the
workload metadata. Unlike the agent fixture, the tool target has no `protocol.rossoctl.io/*`
label, so AgentCardSync does not auto-create an AgentCard. The test verifies this with a
15-second `Consistently` check confirming no AgentCard referencing `runtime-tool-target` exists.

#### StatefulSet target

Deploys `runtime-sts-target` (StatefulSet with headless Service and pause container) and
creates an AgentRuntime CR with `kind: StatefulSet` in the targetRef. The controller resolves
the StatefulSet target identically to Deployments via `runtimePodTemplateAccessor`. Test
verifies `rossoctl.io/type=agent` and `app.kubernetes.io/managed-by=rossoctl-operator` on
StatefulSet metadata, Phase=Active, and a valid 64-char config-hash on the pod template.

#### CR overrides

Deploys two target Deployments and creates two AgentRuntime CRs: one minimal (no overrides)
and one with CR-level fields. Both CRs reach Phase=Active. The test records each Deployment's
`rossoctl.io/config-hash` annotation and asserts they are the same, confirming that CR-level
overrides are excluded from the config hash (2-layer merge). The webhook reads CR overrides
at pod CREATE time instead.

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
