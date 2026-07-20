# AuthBridge TLS Bridge — Phase 2 (Operator Coordination) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the AuthBridge TLS bridge work end-to-end on operator-deployed agents — the operator provisions a per-agent cert-manager CA, mounts the signing key into the sidecar and the trust cert + trust env into the agent, and renders the `tls_bridge:` config block — so a real agent's outbound HTTPS is decrypted into the pipeline, gated behind an off-by-default `tlsBridgeMode`.

**Architecture:** A new `tlsBridgeMode: disabled|enabled` field on the `AgentRuntime` CRD, resolved CR→namespace→default exactly like `mtlsMode`. When `enabled` (and feature-gated on, and cert-manager present, and `authBridgeMode ∈ {proxy-sidecar, lite}`), a new per-agent CA reconciler creates a SelfSigned `Issuer` + a CA `Certificate` (`isCA: true`, **no Name Constraints**) → a Secret. The mutating webhook hard-mounts `tls.crt`/`tls.key` into the sidecar (mode `0440`) and `ca.crt` + trust env into the agent, and renders `tls_bridge: {mode: enabled, ca_dir: /etc/authbridge/tls-bridge-ca}` into the per-agent config (consolidated schema, rossocortex#522 — no scope/ca_source/cert-paths). The `:9094` session-API localhost-bind (it now carries decrypted bodies) already shipped authbridge-side in #522, so the operator PR carries no extensions work; raw-body redaction is a deferred follow-up.

**Tech Stack:** Go 1.x (rossoctl-operator, kubebuilder/controller-runtime), cert-manager Go API `github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1` (v1.20.2, already vendored + scheme-registered), `k8s.io/utils/ptr`. Phase-1 AuthBridge code (rossocortex PR #522) is the **prerequisite** — the rendered `tls_bridge:` block is only understood by the post-#522 authbridge image.

**Locked decisions (from brainstorming):**
1. CRD `tlsBridgeMode: disabled|enabled` maps 1:1 to `tls_bridge.mode`; `envoy-sidecar` + `enabled` ⇒ validating-webhook reject. (The authbridge schema was consolidated in #522: no scope/external — `enabled` intercepts all eligible on the configured ports.)
2. **Unconstrained** per-agent CA (no X.509 Name Constraints). Containment = per-agent isolation + sidecar-only `0440` key + rotation. (Avoids the cert-manager `NameConstraints` feature-gate dependency entirely.)
3. Fully **decoupled** from SPIRE/mTLS. Hard deps only: `authBridgeMode ∈ {proxy-sidecar, lite}` + cert-manager installed.
   - **Cross-repo dependency for true SPIRE-free operation (added post-implementation, verified e2e):** the operator alone does not achieve this. The rossoctl chart renders an empty `spiffe: {}` into every agent's base config, and the pre-fix authbridge binary built the SPIFFE provider on mere presence of that block → `NewX509Source` blocked forever when no SPIRE socket was mounted. SPIRE-free boot therefore requires **rossocortex #523** (need-driven SPIFFE provider: build only when mTLS or a `identity.type=spiffe` plugin consumes it). On the operator side, `applyTLSBridgeMounts` now calls `ensureFSGroup` **unconditionally** (not only under `spireEnabled`) so the non-root sidecar can read the `0440` CA Secret without SPIRE — see the `0440`/`FSGroup=0` note below, which previously held only on the SPIRE path.

**Spec:** `rossocortex/authbridge/docs/superpowers/specs/2026-06-12-authbridge-tlsbridge-design.md` (Phase 2 section). **Phase 1 plan:** `rossocortex/authbridge/docs/superpowers/plans/2026-06-17-authbridge-tlsbridge-phase1.md`.

---

## Verified anchors in the operator (source of truth)

All operator paths under `operator/operator/` unless noted. Verified 2026-06-18.

- **CRD:** `api/v1alpha1/agentruntime_types.go` — `AgentRuntimeSpec` with `AuthBridgeMode string` (`:79`, enum `proxy-sidecar;envoy-sidecar;lite;waypoint`) and `MTLSMode string` (`:116`, `+kubebuilder:default=permissive`, enum `disabled;permissive;strict`). Both are bare strings validated by kubebuilder enum markers.
- **Mode constants:** `internal/webhook/injector/constants.go` — AuthBridge modes `:18-22` (`ModeProxySidecar`, `ModeLite`, `ModeEnvoySidecar`, `ModeWaypoint`), mTLS modes `:66-70` (`MTLSModeDisabled`/`Permissive`/`Strict`).
- **CR overrides:** `internal/webhook/injector/agentruntime_config.go` — `AgentRuntimeOverrides` struct (`:36`) with `MTLSMode *string` (`:61`); `extractOverrides` populates pointers when spec fields non-empty (`:102-126`).
- **Namespace extract:** `internal/webhook/injector/namespace_config.go` — `ExtractMTLSMode(yaml) string` (`:171-186`).
- **Resolution + env inject:** `internal/webhook/injector/pod_mutator.go` — mtls CR→ns→default chain (`:247-281`); `setOrAddEnv(c, "MTLS_MODE", mtlsMode)` proxy-sidecar (`:519-526`) + envoy-sidecar (`:632-638`); agent-container loop pattern `if c.Name == AuthBridgeProxyContainerName { continue }` (`:529-535`); `setOrAddEnv` helper (`:1103`); `injectHTTPProxyEnv` (`:1088`); `ApplyKeycloakClientCredentialsSecretVolumes(podSpec, annotations)` called (`:572`, `:654`); `ensureFSGroup` sets `FSGroup=0` (`:1072`).
- **Per-agent config render:** `pod_mutator.go` — `ensurePerAgentConfigMap(...)` (`:861`); `cfg["mode"]=mode` (`:911`); the `mtls:` block (`:930-938`); SSA-applies CM data key `config.yaml` (`:948-958`).
- **Resolved config struct:** `internal/webhook/injector/resolved_config.go` — `ResolvedConfig` (`:26`) fields `AuthBridgeMode`/`MTLSMode` (`:63-64`); `ResolveConfig` re-runs the chain (`:69`, `:119-128`).
- **Feature gates:** `internal/webhook/config/feature_gates.go` — `FeatureGates` struct (`:13`), `DefaultFeatureGates()` (`:34`, off-by-default precedent: `InjectTools`/`PerWorkloadConfigResolution`/`SkillDiscovery` all `false`); loader `feature_gate_loader.go` `Get()` (`:89`); gate check pattern `pod_mutator.go:312`, `precedence.go:92`.
- **Volumes:** `internal/webhook/injector/volume_builder.go` — `BuildResolvedVolumes(spireEnabled bool, envoyConfigMapName string)` (`:148`, the active path), `BuildRequiredVolumes()` (`:25`), `BuildRequiredVolumesNoSpire()` (`:99`). No Secret-backed volume exists today; volumes are appended `corev1.Volume{...}` literals; `Optional: ptr.To(true)` used (`:79` etc.); **no `DefaultMode` set anywhere today**.
- **Container mounts + securitycontext:** `internal/webhook/injector/container_builder.go` — `BuildProxySidecarContainerWithPorts` volumeMounts (`:237-274`); `SecurityContext{RunAsUser: ptr.To(b.cfg.Proxy.UID), RunAsGroup: ..., RunAsNonRoot: true, AllowPrivilegeEscalation: false}` (`:301-311`); envoy analog (`:85-143`, `:189-194`). `Proxy.UID` default 1337.
- **cert-manager:** `internal/controller/sharedtrust_controller.go` imports `cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"` + `cmmeta` (`:24-25`); `cmv1.AddToScheme(scheme)` in `cmd/main.go:75`; `CertManagerCRDExists(cfg)` (`:394-430`) reusable; registration gated on it in `cmd/main.go:709-718`. **No per-agent Certificate/Issuer is created anywhere today.** `CreateOrUpdate` + `SetControllerReference` template: `internal/controller/mlflow_controller.go:281-299`.
- **RBAC:** marker `sharedtrust_controller.go:95-96` (`certificates` + `clusterissuers`, verbs `get;list;watch`); static dupes `config/rbac/role.yaml:152-156` and `charts/operator/templates/rbac/role.yaml:65-74` (hand-maintained, NOT marker-synced).
- **Validating webhook:** `internal/webhook/v1alpha1/agentruntime_webhook.go` — already rejects `mtlsMode != disabled` with `envoy-sidecar`; the place to add the `tlsBridgeMode=enabled` + `envoy-sidecar` reject.
- **cert-manager version:** `operator/go.mod:8` → `v1.20.2`. `CertificateSpec.NameConstraints` exists but is NOT used (decision 2).

---

## File Structure

**rossoctl-operator (bulk):**
- `api/v1alpha1/agentruntime_types.go` — new `TLSBridgeMode` field.
- `internal/webhook/injector/constants.go` — `TLSBridgeMode*` consts + the CA mount path/volume names.
- `internal/webhook/config/feature_gates.go` (+ `feature_gate_loader.go` banner) — `TLSBridge bool` gate.
- `internal/webhook/injector/agentruntime_config.go` — `TLSBridgeMode *string` override + extraction.
- `internal/webhook/injector/namespace_config.go` — `ExtractTLSBridgeMode`.
- `internal/webhook/injector/resolved_config.go` — `TLSBridgeMode` field + resolution.
- `internal/webhook/injector/pod_mutator.go` — resolution chain; the `tls_bridge:` render block; the sidecar+agent mount/env injection (new helper `applyTLSBridgeMounts`).
- `internal/webhook/injector/volume_builder.go` — the Secret volume (helper `tlsBridgeCAVolume`).
- `internal/webhook/injector/container_builder.go` — sidecar mount.
- `internal/controller/tlsbridge_ca_controller.go` — **new** per-agent CA reconciler.
- `internal/webhook/v1alpha1/agentruntime_webhook.go` — envoy-sidecar reject.
- `cmd/main.go` — register the reconciler (gated).
- RBAC marker on the new reconciler + `config/rbac/role.yaml` + `charts/operator/templates/rbac/role.yaml`.

**rossocortex:** none required for this PR. The `:9094` localhost-bind already
shipped in #522 (`config.Load` rewrites `session_api_addr` to loopback when
`tls_bridge.mode=enabled`). Raw-body redaction is a deferred follow-up, not a Phase-2 blocker.

**Naming constants (used across tasks — define once in Task 2):**
- `TLSBridgeModeDisabled = "disabled"`, `TLSBridgeModeEnabled = "enabled"`
- `TLSBridgeCAVolumeName = "tls-bridge-ca"`, `TLSBridgeCAMountPath = "/etc/authbridge/tls-bridge-ca"`
- `TLSBridgeCASecretSuffix = "-tls-bridge-ca"` (Secret name = `<agentName>` + suffix)
- Trust env keys: `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `AWS_CA_BUNDLE`, `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH` → all set to `<TLSBridgeCAMountPath>/ca.crt`.

---

## Task 0: Branch

- [ ] **Step 1: Branch the operator repo off current main**

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/operator
git fetch origin main   # or upstream main, per your remote
git checkout -b feat/tlsbridge-phase2 origin/main
```

(All operator work happens here. No rossocortex changes are needed — the `:9094` localhost-bind already landed in #522.)

---

## Task 1: CRD field `TLSBridgeMode`

**Files:**
- Modify: `operator/api/v1alpha1/agentruntime_types.go` (after `MTLSMode`, ~`:116`)

- [ ] **Step 1: Add the field** to `AgentRuntimeSpec`, immediately after the `MTLSMode` field:

```go
	// TLSBridgeMode controls AuthBridge's outbound TLS bridge (decrypt agent
	// egress HTTPS into the pipeline). Only honored for authBridgeMode
	// proxy-sidecar or lite; rejected with envoy-sidecar. Requires cert-manager.
	// +optional
	// +kubebuilder:default=disabled
	// +kubebuilder:validation:Enum=disabled;enabled
	TLSBridgeMode string `json:"tlsBridgeMode,omitempty"`
```

- [ ] **Step 2: Regenerate CRD manifests + deepcopy**

```bash
cd rossoctl-operator && make generate manifests
```
Expected: `zz_generated.deepcopy.go` unchanged (string field needs no deepcopy), and `config/crd/bases/*agentruntime*.yaml` now lists `tlsBridgeMode` with enum `[disabled, enabled]`, default `disabled`. If the chart vendors the CRD, also run any chart-CRD sync target (e.g. `make sync-crds` if present) or copy the regenerated CRD into `charts/operator/`.

- [ ] **Step 3: Build + commit**

Run: `cd rossoctl-operator && go build ./...`
Expected: builds.

```bash
git add operator/api/v1alpha1/agentruntime_types.go operator/config/crd operator/charts 2>/dev/null
git commit -s -m "feat(tlsbridge): add AgentRuntime.spec.tlsBridgeMode (disabled|enabled)"
```

---

## Task 2: Constants

**Files:**
- Modify: `operator/internal/webhook/injector/constants.go` (near the mTLS consts `:66-70`)

- [ ] **Step 1: Add the constants** after the mTLS mode block:

```go
	// TLS bridge modes (AgentRuntime.spec.tlsBridgeMode).
	TLSBridgeModeDisabled = "disabled"
	TLSBridgeModeEnabled  = "enabled"

	// TLSBridgeCAVolumeName is the Secret-backed volume carrying the per-agent
	// cert-manager CA. tls.crt/tls.key go to the sidecar (signing); ca.crt goes
	// to the agent (trust).
	TLSBridgeCAVolumeName   = "tls-bridge-ca"
	TLSBridgeCAMountPath    = "/etc/authbridge/tls-bridge-ca"
	TLSBridgeCASecretSuffix = "-tls-bridge-ca"
```

- [ ] **Step 2: Build + commit**

Run: `go build ./...`

```bash
git add operator/internal/webhook/injector/constants.go
git commit -s -m "feat(tlsbridge): mode + CA-mount constants"
```

---

## Task 3: Off-by-default feature gate

**Files:**
- Modify: `operator/internal/webhook/config/feature_gates.go` (struct `:13`, `DefaultFeatureGates` `:34`, banner `:178`)
- Test: `operator/internal/webhook/config/feature_gates_test.go` (or the existing test file)

- [ ] **Step 1: Write the failing test** — default must be OFF:

```go
func TestDefaultFeatureGates_TLSBridgeOff(t *testing.T) {
	g := DefaultFeatureGates()
	if g.TLSBridge {
		t.Errorf("TLSBridge feature gate must default to false")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`g.TLSBridge` undefined)

Run: `cd rossoctl-operator && go test ./internal/webhook/config/ -run TestDefaultFeatureGates_TLSBridge -v`

- [ ] **Step 3: Add the field** to the `FeatureGates` struct (after `SkillDiscovery`):

```go
	// TLSBridge enables AuthBridge's outbound TLS bridge (decrypt agent egress).
	// Off by default; see docs/.../tlsbridge-phase2.
	TLSBridge bool `json:"tlsBridge" yaml:"tlsBridge"`
```

Leave it out of / set `false` in `DefaultFeatureGates()` (the zero value is `false`, matching the other off-by-default gates). Add it to the `logFeatureGates`/banner (`:178`) so its state is visible at startup.

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/webhook/config/ -run TestDefaultFeatureGates_TLSBridge -v`

- [ ] **Step 5: Commit**

```bash
git add operator/internal/webhook/config/
git commit -s -m "feat(tlsbridge): add off-by-default TLSBridge feature gate"
```

---

## Task 4: Mode resolution plumbing (CR → namespace → default)

**Files:**
- Modify: `agentruntime_config.go` (`AgentRuntimeOverrides` `:36`, `extractOverrides` `:102-126`)
- Modify: `namespace_config.go` (add `ExtractTLSBridgeMode`, mirror `ExtractMTLSMode` `:171-186`)
- Modify: `resolved_config.go` (`ResolvedConfig` `:63-64`, `ResolveConfig` `:119-128`)
- Test: `operator/internal/webhook/injector/resolved_config_test.go`

- [ ] **Step 1: Write the failing test** — resolution prefers CR over namespace over default:

```go
func TestResolveConfig_TLSBridgeMode_Precedence(t *testing.T) {
	// CR override wins.
	rc := ResolveConfig(&AgentRuntimeOverrides{TLSBridgeMode: ptr.To("enabled")}, NamespaceConfig{}, FeatureGates{})
	if rc.TLSBridgeMode != "enabled" {
		t.Errorf("CR override: got %q want enabled", rc.TLSBridgeMode)
	}
	// Default when nothing set.
	rc2 := ResolveConfig(&AgentRuntimeOverrides{}, NamespaceConfig{}, FeatureGates{})
	if rc2.TLSBridgeMode != "" && rc2.TLSBridgeMode != TLSBridgeModeDisabled {
		t.Errorf("default: got %q want disabled/empty", rc2.TLSBridgeMode)
	}
}
```

(Adjust the `ResolveConfig` call shape to the real signature you find at `resolved_config.go:69` — the test exists to lock precedence, not the arg list.)

- [ ] **Step 2: Run — expect FAIL** (`TLSBridgeMode` undefined on overrides/resolved)

Run: `cd rossoctl-operator && go test ./internal/webhook/injector/ -run TestResolveConfig_TLSBridgeMode -v`

- [ ] **Step 3: Add the override field + extraction** in `agentruntime_config.go`. In `AgentRuntimeOverrides` (after `MTLSMode *string`):

```go
	TLSBridgeMode *string
```

In `extractOverrides`, after the `MTLSMode` block (mirror it exactly):

```go
	if spec.TLSBridgeMode != "" {
		v := spec.TLSBridgeMode
		o.TLSBridgeMode = &v
	}
```

- [ ] **Step 4: Add `ExtractTLSBridgeMode`** in `namespace_config.go` — copy `ExtractMTLSMode` (`:171-186`) verbatim, changing the YAML key it reads to `tls_bridge.mode` (or whatever key the namespace `authbridge-runtime-config` would carry; if namespace-level bridge config isn't supported initially, this helper may just return `""` — but keep the function for symmetry so the resolution chain compiles):

```go
// ExtractTLSBridgeMode reads tls_bridge.mode from the namespace authbridge
// runtime YAML; "" when absent. Mirrors ExtractMTLSMode.
func ExtractTLSBridgeMode(runtimeYAML string) string {
	// ... copy ExtractMTLSMode body, read the "tls_bridge" map's "mode" key ...
}
```

- [ ] **Step 5: Add the resolved field + chain** in `resolved_config.go`. In `ResolvedConfig` (after `MTLSMode`):

```go
	TLSBridgeMode string
```

In `ResolveConfig` (mirror the `MTLSMode` resolution at `:124-128`):

```go
	rc.TLSBridgeMode = TLSBridgeModeDisabled
	if v := ExtractTLSBridgeMode(nsConfig.AuthBridgeRuntimeYAML); v != "" {
		rc.TLSBridgeMode = v
	}
	if overrides != nil && overrides.TLSBridgeMode != nil {
		rc.TLSBridgeMode = *overrides.TLSBridgeMode
	}
```

- [ ] **Step 6: Run — expect PASS**

Run: `go test ./internal/webhook/injector/ -run TestResolveConfig_TLSBridgeMode -v`

- [ ] **Step 7: Commit**

```bash
git add operator/internal/webhook/injector/agentruntime_config.go operator/internal/webhook/injector/namespace_config.go operator/internal/webhook/injector/resolved_config.go operator/internal/webhook/injector/resolved_config_test.go
git commit -s -m "feat(tlsbridge): resolve tlsBridgeMode (CR>namespace>default)"
```

---

## Task 5: Validating-webhook reject (enabled + envoy-sidecar)

**Files:**
- Modify: `operator/internal/webhook/v1alpha1/agentruntime_webhook.go` (beside the existing mtls+envoy reject)
- Test: `operator/internal/webhook/v1alpha1/agentruntime_webhook_test.go`

- [ ] **Step 1: Write the failing test**:

```go
func TestValidate_TLSBridgeEnabled_RejectsEnvoySidecar(t *testing.T) {
	ar := &AgentRuntime{Spec: AgentRuntimeSpec{AuthBridgeMode: "envoy-sidecar", TLSBridgeMode: "enabled"}}
	_, err := ar.ValidateCreate() // or the validator's Validate(ctx, ar) shape
	if err == nil {
		t.Fatalf("expected rejection: tlsBridgeMode=enabled is incompatible with envoy-sidecar")
	}
	// And the allowed combo must pass:
	ok := &AgentRuntime{Spec: AgentRuntimeSpec{AuthBridgeMode: "proxy-sidecar", TLSBridgeMode: "enabled"}}
	if _, err := ok.ValidateCreate(); err != nil {
		t.Fatalf("proxy-sidecar + enabled must be allowed, got %v", err)
	}
}
```

(Match the real validator entrypoint — find how the mtls+envoy check is invoked in this file and mirror it.)

- [ ] **Step 2: Run — expect FAIL** (no rejection yet)

Run: `cd rossoctl-operator && go test ./internal/webhook/v1alpha1/ -run TestValidate_TLSBridgeEnabled -v`

- [ ] **Step 3: Add the check** beside the mtls+envoy reject:

```go
	if spec.TLSBridgeMode == "enabled" && spec.AuthBridgeMode == "envoy-sidecar" {
		return nil, fmt.Errorf("tlsBridgeMode=enabled requires authBridgeMode proxy-sidecar or lite; it is not supported with envoy-sidecar (the TLS bridge lives in the Go forward proxy)")
	}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/webhook/v1alpha1/ -run TestValidate_TLSBridgeEnabled -v`

- [ ] **Step 5: Commit**

```bash
git add operator/internal/webhook/v1alpha1/
git commit -s -m "feat(tlsbridge): reject tlsBridgeMode=enabled with envoy-sidecar"
```

---

## Task 6: Per-agent CA reconciler (net-new)

**Files:**
- Create: `operator/internal/controller/tlsbridge_ca_controller.go`
- Test: `operator/internal/controller/tlsbridge_ca_controller_test.go`

This reconciler watches `AgentRuntime`. When the resolved mode is `enabled` AND the feature gate is on AND cert-manager is present, it ensures (per agent): a namespace SelfSigned `Issuer` (shared, name `authbridge-tls-bridge-selfsigned`) and a CA `Certificate` (`isCA: true`, `secretName: <agent>-tls-bridge-ca`, **no nameConstraints**) owned by the AgentRuntime. cert-manager then issues the Secret (`tls.crt`/`tls.key`/`ca.crt`). The hard pod-mount (Task 7) blocks pod start until that Secret exists — solving the ordering race.

> **Contract with authbridge `FileSource` (rossocortex#522).** The bridge's
> `NewFileSource` now *validates* the mounted Secret at boot and **fails loud** if the
> cert is not a CA (`IsCA=false` / missing `KeyUsageCertSign`) or if cert/key don't match.
> So the `Certificate` below MUST keep `IsCA: true` **and** `Usages` including
> `cmv1.UsageCertSign` — both are present in the spec below; do not drop them, or the
> sidecar will refuse to start (which is the intended fail-loud, not a silent tunnel). The
> ECDSA P-256 key cert-manager issues (SEC1-encoded) is parseable by `FileSource` (it tries
> PKCS#8 → PKCS#1 → SEC1); RSA would also work. cert-manager issues cert+key together, so
> the match check passes.

- [ ] **Step 1: Write the failing test** (envtest or fake client — mirror an existing controller test in `internal/controller/`):

```go
func TestTLSBridgeCAReconciler_CreatesIssuerAndCert(t *testing.T) {
	// Given an AgentRuntime with tlsBridgeMode=enabled and the gate on,
	// Reconcile creates a SelfSigned Issuer and a CA Certificate (isCA, no
	// nameConstraints) with secretName "<name>-tls-bridge-ca", owner-ref'd to
	// the AgentRuntime. Use a fake client seeded with the AgentRuntime + the
	// cmv1 scheme. Assert both objects exist after Reconcile.
}
```

(Use `sigs.k8s.io/controller-runtime/pkg/client/fake` with `cmv1.AddToScheme` + the operator scheme. Mirror the construction in an existing `*_controller_test.go`.)

- [ ] **Step 2: Run — expect FAIL** (`TLSBridgeCAReconciler` undefined)

Run: `cd rossoctl-operator && go test ./internal/controller/ -run TestTLSBridgeCAReconciler -v`

- [ ] **Step 3: Implement the reconciler**:

```go
package controller

import (
	"context"
	"fmt"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ctrl "sigs.k8s.io/controller-runtime"
	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/webhook/injector"
)

const tlsBridgeSelfSignedIssuer = "authbridge-tls-bridge-selfsigned"

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates;issuers,verbs=get;list;watch;create;update;patch;delete

type TLSBridgeCAReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	FeatureGates *config.FeatureGates // pointer so hot-reload is observed
}

func (r *TLSBridgeCAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.FeatureGates == nil || !r.FeatureGates.TLSBridge {
		return ctrl.Result{}, nil
	}
	ar := &agentv1alpha1.AgentRuntime{}
	if err := r.Get(ctx, req.NamespacedName, ar); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Resolve effective mode the same way the webhook does (CR field is enough
	// for the controller's purpose; namespace/default are handled at admission).
	if ar.Spec.TLSBridgeMode != injector.TLSBridgeModeEnabled {
		return ctrl.Result{}, nil // disabled — nothing to provision (no GC of the Secret here; ownerRef handles deletion when the AR is removed)
	}

	// 1) SelfSigned Issuer (namespace-shared, idempotent).
	issuer := &cmv1.Issuer{ObjectMeta: metav1.ObjectMeta{Name: tlsBridgeSelfSignedIssuer, Namespace: ar.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.Spec = cmv1.IssuerSpec{IssuerConfig: cmv1.IssuerConfig{SelfSigned: &cmv1.SelfSignedIssuer{}}}
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure selfsigned issuer: %w", err)
	}

	// 2) Per-agent CA Certificate (isCA, NO nameConstraints — decision 2).
	secretName := ar.Name + injector.TLSBridgeCASecretSuffix
	cert := &cmv1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ar.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.Spec = cmv1.CertificateSpec{
			IsCA:       true,
			CommonName: "authbridge-tls-bridge-ca-" + ar.Name,
			SecretName: secretName,
			Duration:   &metav1.Duration{Duration: 90 * 24 * time.Hour},
			RenewBefore: &metav1.Duration{Duration: 15 * 24 * time.Hour},
			PrivateKey: &cmv1.CertificatePrivateKey{Algorithm: cmv1.ECDSAKeyAlgorithm, Size: 256},
			Usages:     []cmv1.KeyUsage{cmv1.UsageCertSign, cmv1.UsageDigitalSignature},
			IssuerRef:  cmmeta.ObjectReference{Name: tlsBridgeSelfSignedIssuer, Kind: "Issuer", Group: "cert-manager.io"},
		}
		return controllerutil.SetControllerReference(ar, cert, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure CA certificate: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *TLSBridgeCAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentv1alpha1.AgentRuntime{}).
		Owns(&cmv1.Certificate{}).
		Named("tlsbridge-ca").
		Complete(r)
}
```

(Imports `runtime`, `config`, `time` elided above — add them. EC P-256 (`Size: 256`) matches Phase 1's `FileSource` PKCS#1/PKCS#8/SEC1 parsing. The leaf private key the proxy mints with is separate — this CA key signs leaves.)

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/controller/ -run TestTLSBridgeCAReconciler -v`

- [ ] **Step 5: Register in `cmd/main.go`** — gated on cert-manager presence (mirror the SharedTrust registration at `:709-718`):

```go
	if controller.CertManagerCRDExists(mgr.GetConfig()) {
		if err = (&controller.TLSBridgeCAReconciler{
			Client:       mgr.GetClient(),
			Scheme:       mgr.GetScheme(),
			FeatureGates: featureGates, // the *config.FeatureGates the manager already holds
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "TLSBridgeCA")
			os.Exit(1)
		}
	}
```

- [ ] **Step 6: Build + commit**

Run: `cd rossoctl-operator && go build ./... && go test ./internal/controller/ -run TLSBridge`

```bash
git add operator/internal/controller/tlsbridge_ca_controller.go operator/internal/controller/tlsbridge_ca_controller_test.go operator/cmd/main.go
git commit -s -m "feat(tlsbridge): per-agent cert-manager CA reconciler (unconstrained)"
```

---

## Task 7: RBAC delta (create/update issuers + certificates)

**Files:**
- Modify: `operator/config/rbac/role.yaml` (the cert-manager rule `:152-156`)
- Modify: `operator/charts/operator/templates/rbac/role.yaml` (`:65-74`)

(The kubebuilder marker was added in Task 6 Step 3; `make manifests` regenerates `config/rbac/role.yaml`. The chart role is hand-maintained — edit it directly.)

- [ ] **Step 1: Regenerate + verify the generated rule**

```bash
cd rossoctl-operator && make manifests
```
Expected: `config/rbac/role.yaml` now grants `create;update;patch;delete` (plus `get;list;watch`) on `cert-manager.io` `certificates` AND `issuers` (issuers newly added).

- [ ] **Step 2: Edit the chart role** to match — add to the `cert-manager.io` rule:

```yaml
- apiGroups:
  - cert-manager.io
  resources:
  - certificates
  - issuers
  - clusterissuers
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
```

- [ ] **Step 3: Commit**

```bash
git add operator/config/rbac/role.yaml operator/charts/operator/templates/rbac/role.yaml
git commit -s -m "feat(tlsbridge): RBAC to create per-agent cert-manager Issuer/Certificate"
```

---

## Task 8: Render the `tls_bridge:` config block

**Files:**
- Modify: `operator/internal/webhook/injector/pod_mutator.go` — `ensurePerAgentConfigMap` (`:861`, the `mtls:` block `:930-938`); thread a `tlsBridgeMode string` param + pass it from both call sites.
- Test: extend `pod_mutator_test.go`

- [ ] **Step 1: Write the failing test** — enabled renders the block; disabled scrubs it:

```go
func TestEnsurePerAgentConfigMap_TLSBridgeBlock(t *testing.T) {
	// Render with tlsBridgeMode=enabled -> config.yaml contains:
	//   tls_bridge: {mode: enabled, ca_dir: /etc/authbridge/tls-bridge-ca}
	// Render with disabled -> no tls_bridge key.
}
```

(Mirror the existing `mtls` ConfigMap-render test in `pod_mutator_test.go`.)

- [ ] **Step 2: Run — expect FAIL**

Run: `cd rossoctl-operator && go test ./internal/webhook/injector/ -run TestEnsurePerAgentConfigMap_TLSBridge -v`

- [ ] **Step 3: Add the render block** right after the `mtls:` block (`:938`), and thread `tlsBridgeMode` into the function signature + both call sites:

```go
	if tlsBridgeMode == TLSBridgeModeEnabled {
		// Consolidated schema (rossocortex#522): mode + ca_dir only.
		// ca_dir = the mounted cert-manager Secret (tls.crt/tls.key/ca.crt by
		// convention). No scope/ca_source/cert+key paths.
		cfg["tls_bridge"] = map[string]interface{}{
			"mode":   "enabled",
			"ca_dir": TLSBridgeCAMountPath,
		}
	} else {
		delete(cfg, "tls_bridge")
	}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/webhook/injector/ -run TestEnsurePerAgentConfigMap_TLSBridge -v`

- [ ] **Step 5: Commit**

```bash
git add operator/internal/webhook/injector/pod_mutator.go operator/internal/webhook/injector/pod_mutator_test.go
git commit -s -m "feat(tlsbridge): render tls_bridge config block (mode + ca_dir)"
```

---

## Task 9: Mount the CA — sidecar key (0440) + agent cert + trust env

**Files:**
- Modify: `operator/internal/webhook/injector/volume_builder.go` (helper `tlsBridgeCAVolume`; add to `BuildResolvedVolumes` `:148` + the two legacy builders)
- Modify: `operator/internal/webhook/injector/container_builder.go` (sidecar mount `:237`)
- Modify: `operator/internal/webhook/injector/pod_mutator.go` (new `applyTLSBridgeMounts(podSpec, agentName, tlsBridgeMode)`; call it gated, after the agent-container loop ~`:535`)
- Test: `pod_mutator_test.go`

> **Mode `0440`, not `0400`.** The sidecar runs non-root as `Proxy.UID` (1337); `ensureFSGroup` sets pod `FSGroup=0`. Kubernetes group-owns Secret-volume files by `FSGroup`, so `0440` (owner+group read) lets the proxy read `tls.key` via its `FSGroup=0` supplementary group. `0400` (owner-only) would deny the non-root proxy → no minting → silent fall-open to tunnel. Verify in Task 12.

- [ ] **Step 1: Write the failing test** — enabled pod has the Secret volume, the sidecar `tls.key` mount, and the agent `ca.crt` mount + `SSL_CERT_FILE` env; all hard (`Optional` nil/false):

```go
func TestApplyTLSBridgeMounts_Enabled(t *testing.T) {
	// Build a pod with sidecar + agent containers, tlsBridgeMode=enabled.
	// Assert:
	//  - a Volume named "tls-bridge-ca" backed by Secret "<agent>-tls-bridge-ca",
	//    DefaultMode 0440, Optional not true.
	//  - sidecar container mounts it at /etc/authbridge/tls-bridge-ca (ReadOnly).
	//  - agent container mounts it at /etc/authbridge/tls-bridge-ca (ReadOnly)
	//    AND has env SSL_CERT_FILE=/etc/authbridge/tls-bridge-ca/ca.crt
	//    and NODE_EXTRA_CA_CERTS=... (spot-check two).
	// Disabled: none of the above present.
}
```

- [ ] **Step 2: Run — expect FAIL**

Run: `cd rossoctl-operator && go test ./internal/webhook/injector/ -run TestApplyTLSBridgeMounts -v`

- [ ] **Step 3: Add the volume helper** in `volume_builder.go` and append it from the volume builders when bridging is on (thread an `enabled bool` + `secretName string`, or append unconditionally in a new gated call — follow the existing builder signatures):

```go
func tlsBridgeCAVolume(secretName string) corev1.Volume {
	return corev1.Volume{
		Name: TLSBridgeCAVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  secretName,
				DefaultMode: ptr.To(int32(0o440)),
				// Optional unset (=false): hard mount — pod blocks until the CA
				// Secret exists (cert-manager issues it after the reconciler
				// creates the Certificate). This is the ordering guarantee.
			},
		},
	}
}
```

- [ ] **Step 4: Add `applyTLSBridgeMounts`** in `pod_mutator.go` and call it (gated on `featureGates.TLSBridge && tlsBridgeMode==TLSBridgeModeEnabled && mode ∈ {proxy-sidecar, lite}`) after the agent env loop:

```go
func applyTLSBridgeMounts(podSpec *corev1.PodSpec, agentName string) {
	secretName := agentName + TLSBridgeCASecretSuffix
	// Volume (idempotent).
	if !volumeExists(podSpec.Volumes, TLSBridgeCAVolumeName) {
		podSpec.Volumes = append(podSpec.Volumes, tlsBridgeCAVolume(secretName))
	}
	caFile := TLSBridgeCAMountPath + "/ca.crt"
	trustEnv := []string{
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE",
		"CURL_CA_BUNDLE", "GIT_SSL_CAINFO", "AWS_CA_BUNDLE",
		"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH",
	}
	for i := range podSpec.Containers {
		c := &podSpec.Containers[i]
		switch c.Name {
		case AuthBridgeProxyContainerName, EnvoyProxyContainerName:
			// Sidecar: mount the whole Secret (it needs tls.crt+tls.key to sign).
			c.VolumeMounts = appendMountIfMissing(c.VolumeMounts, corev1.VolumeMount{
				Name: TLSBridgeCAVolumeName, MountPath: TLSBridgeCAMountPath, ReadOnly: true,
			})
		default:
			// Agent: mount the same volume (reads ca.crt only) + trust env.
			c.VolumeMounts = appendMountIfMissing(c.VolumeMounts, corev1.VolumeMount{
				Name: TLSBridgeCAVolumeName, MountPath: TLSBridgeCAMountPath, ReadOnly: true,
			})
			for _, e := range trustEnv {
				setOrAddEnv(c, e, caFile)
			}
		}
	}
}
```

Add the small `volumeExists` / `appendMountIfMissing` helpers if not already present (idempotency for re-admission). The sidecar mount supplies the signing key; `0440`+`FSGroup=0` makes `tls.key` readable by the non-root proxy.

- [ ] **Step 5: Sidecar mount note** — `applyTLSBridgeMounts` already mounts into the sidecar by container name, so no change to `container_builder.go`'s static mount list is strictly required; if the resolved-config path builds sidecar mounts there instead, add the same `VolumeMount` to `BuildProxySidecarContainerWithPorts` (`:237`) gated on bridge-enabled. Pick ONE site (prefer `applyTLSBridgeMounts` for locality) and don't double-mount.

- [ ] **Step 6: Run — expect PASS**

Run: `go test ./internal/webhook/injector/ -run TestApplyTLSBridgeMounts -v`

- [ ] **Step 7: Commit**

```bash
git add operator/internal/webhook/injector/volume_builder.go operator/internal/webhook/injector/pod_mutator.go operator/internal/webhook/injector/pod_mutator_test.go
git commit -s -m "feat(tlsbridge): mount per-agent CA — sidecar key (0440) + agent ca.crt + trust env"
```

---

## Task 10: Wire mode resolution → render + mounts in the mutator

**Files:**
- Modify: `operator/internal/webhook/injector/pod_mutator.go` (the proxy-sidecar mutation path, near the `MTLS_MODE` injection `:519-526`)
- Test: `pod_mutator_test.go`

- [ ] **Step 1: Write the failing end-to-end mutation test** — a pod admitted for an `enabled` proxy-sidecar agent (gate on) gets BOTH the `tls_bridge:` config AND the mounts/env; an `envoy-sidecar`/disabled/gate-off agent gets neither:

```go
func TestMutate_TLSBridge_EndToEnd(t *testing.T) {
	// gate on, proxy-sidecar, tlsBridgeMode=enabled:
	//   -> per-agent CM has tls_bridge block; sidecar+agent have the mount;
	//      agent has trust env.
	// gate OFF (same spec): none of it.
	// proxy-sidecar, disabled: none of it.
}
```

- [ ] **Step 2: Run — expect FAIL**

- [ ] **Step 3: Wire it** — in the proxy-sidecar (and lite) mutation path, after resolving `tlsBridgeMode`, gate and call:

```go
	tlsBridgeMode := resolved.TLSBridgeMode
	bridgeOn := currentGates.TLSBridge &&
		tlsBridgeMode == TLSBridgeModeEnabled &&
		(mode == ModeProxySidecar || mode == ModeLite)
	if bridgeOn {
		applyTLSBridgeMounts(podSpec, agentName)
	}
	// pass tlsBridgeMode (or "" when !bridgeOn) into ensurePerAgentConfigMap so the
	// block is rendered iff bridgeOn:
	renderMode := TLSBridgeModeDisabled
	if bridgeOn { renderMode = TLSBridgeModeEnabled }
	// ... ensurePerAgentConfigMap(..., renderMode)
```

(Use the real `agentName`/`mode`/`resolved` variables in scope at that point. The gate + mode check live here so the reconciler and the mutator agree on "bridge on".)

- [ ] **Step 4: Run — expect PASS**; then the full injector suite (no regressions):

Run: `go test ./internal/webhook/injector/ -v`

- [ ] **Step 5: Commit**

```bash
git add operator/internal/webhook/injector/pod_mutator.go operator/internal/webhook/injector/pod_mutator_test.go
git commit -s -m "feat(tlsbridge): gate + wire render/mounts in the mutating webhook"
```

---

## Task 11: `:9094` hardening — DONE in #522 (no operator work)

**Status: the localhost-bind shipped in PR #522 (`4acd038`), authbridge-side.** When
`tls_bridge.mode == enabled`, `config.Load()` rewrites `listener.session_api_addr` to
`127.0.0.1:<port>` (tests: `TestForceLocalhost`, `TestLoad_TLSBridgeHardensSessionAPI`).
It is **automatic** — the operator renders no session-API config and carries **no work**
for this. Consequence to document in the E2E + user docs: with bridging on, `:9094` is
reachable only from inside the pod (kubectl port-forward / abctl still work — they target
the pod's loopback); **cross-pod scraping of `:9094` stops working by design.**

**Deferred (future, not this PR):** raw-body *redaction* in the session store. The
localhost-bind shrinks who can reach the API; redaction (scrubbing tokens/PII from the
captured decrypted bodies even for an authorized port-forward reader) is a separate
follow-up tracked with the other Phase-1 hardening items.

---

## Task 12: E2E validation

**Files:**
- Create: an e2e script under the operator's e2e harness (mirror an existing one), or a documented manual runbook.

- [ ] **Step 1: Deploy prerequisites** — a cluster (Kind/OCP) with cert-manager installed, the post-#522 authbridge image, and the Phase-2 operator image (feature gate `TLSBridge: true`).

- [ ] **Step 2: Create a bridge-enabled agent** with a runtime that honors the trust env (e.g. a Python `requests` agent): `authBridgeMode: proxy-sidecar`, `tlsBridgeMode: enabled`.

- [ ] **Step 3: Assert provisioning** — the per-agent CA `Certificate` + Secret `<agent>-tls-bridge-ca` exist; the pod started (hard mount satisfied, not stuck Pending); the sidecar log shows `tls-bridge enabled` and the trust self-check `OK` (the agent's `SSL_CERT_FILE` points at the mounted `ca.crt`).

- [ ] **Step 4: Assert decryption end-to-end** — agent makes an outbound HTTPS call (an external origin AND, since scope=all, an in-cluster HTTPS tool). The sidecar logs the **decrypted** request and a plugin acts on it; the response is intact. Verify with an **h2** origin too (Phase-1 tests forced h1.1; this is the first real h2-through-bridge check).

- [ ] **Step 5: Assert no-broken-calls** — a cert-pinned client (or a runtime that ignores the trust env) still reaches its origin (tunneled), and an agent with `tlsBridgeMode: disabled` is unaffected. Confirm an agent created with cert-manager ABSENT stays Pending (hard mount) rather than starting un-bridged — and document that as the expected failure mode.

- [ ] **Step 6: Commit the e2e script/runbook**

```bash
git add <e2e path>
git commit -s -m "test(tlsbridge): e2e — operator-provisioned CA decrypts agent egress"
```

---

## Phase 2 done — definition of done

- Operator builds; `make manifests generate` clean; unit tests green (`go test ./...`).
- `tlsBridgeMode: enabled` on a `proxy-sidecar`/`lite` agent (gate on, cert-manager present) → operator provisions the per-agent CA, hard-mounts the signing key (`0440`) into the sidecar + `ca.crt`+trust env into the agent, renders `tls_bridge: {mode: enabled, ca_dir: …}` → the agent's outbound HTTPS is **decrypted into the pipeline** end-to-end (h1.1 **and** h2), proven by E2E.
- `tlsBridgeMode: enabled` + `envoy-sidecar` is **rejected** at admission.
- Feature gate **off by default**; disabled/gate-off agents are byte-identical to today.
- Un-bridgeable traffic (pinned client, trust-env-ignoring runtime) safely tunnels; cert-manager-absent → pod Pending (documented), never silent un-bridged egress.
- `:9094` is localhost-bound whenever bridging is on (already in #522; redaction deferred).

---

## Release ordering (multi-repo — MANDATORY)

The operator renders a `tls_bridge:` block only the post-#522 authbridge image understands (the `:9094` localhost-bind + `FileSource` validation are already in #522). So:

1. **rossocortex**: merge PR #522 → tag a new `v0.x.0-alpha.N` authbridge/proxy-init image.
2. **rossoctl-operator**: merge this Phase-2 PR → tag `v0.x.0-alpha.M`.
3. **rossoctl**: bump the chart pins (operator image + authbridge sidecar images) to the new tags; this is where it becomes installable. Same operator→extensions→rossoctl order as the alpha.9 release.

---

## Self-review notes

- **Decision coverage:** (1) `tlsBridgeMode disabled|enabled` = Task 1; renders `tls_bridge.mode` 1:1 = Task 8; envoy reject = Task 5. (2) unconstrained CA (no `nameConstraints`) = Task 6. (3) decoupled from SPIRE/mtls = no SPIRE auto-enable anywhere; gate + `proxy-sidecar|lite` + cert-manager are the only deps (Tasks 6/10). Off-by-default = Task 3.
- **Ordering/race:** hard mount (`Optional` unset, Task 9) + reconciler-creates-Certificate-before-pod (Task 6) is the 3-actor liveness guarantee; cert-manager-absent → Pending (Task 12 Step 5), never silent un-bridged egress.
- **0440 vs 0400:** Task 9 uses `0440` deliberately (non-root proxy + `FSGroup=0`); validated in Task 12 Step 3 (self-check OK proves the proxy read its key and minted).
- **Known follow-ups (carried from Phase 1 review #522):** bound the runtime `SkipSet` + per-host verify cache (LRU/TTL); CA rotation is restart-based (cert-manager rotates the Secret; the proxy reads `FileSource` once at boot — zero-downtime rotation via file-watch is a later item); h2-through-bridge gets its first real exercise in Task 12.
- **Anchor caveat for the implementer:** every `pod_mutator.go`/`resolved_config.go` line number is from 2026-06-18; re-grep before editing (the file shifts as tasks land). The mtls plumbing is the canonical template for every "copy this" instruction.
