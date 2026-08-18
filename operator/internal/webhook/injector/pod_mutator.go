/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package injector

import (
	"context"
	"fmt"
	"slices"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/webhook/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	applyconfigscorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyconfigsmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

var mutatorLog = logf.Log.WithName("pod-mutator")

const (
	// Label selector for authbridge injection opt-out.
	// Injection uses opt-out semantics for agents: sidecars are injected by
	// default. Setting AuthBridgeInjectLabel=AuthBridgeDisabledValue on a
	// workload explicitly opts it out. Any other value (including absent) does
	// not block injection.
	AuthBridgeInjectLabel   = "rossoctl.io/inject"
	AuthBridgeInjectValue   = "enabled" // retained for backwards compat / tests
	AuthBridgeDisabledValue = "disabled"

	// SPIRE opt-out label. Setting rossoctl.io/spire=disabled on a workload blocks
	// spiffe-helper injection (layer 7 of the precedence chain). Any other value
	// (including absence of the label) leaves spiffe-helper injection to the
	// upstream precedence layers.
	SpireEnableLabel   = "rossoctl.io/spire"
	SpireDisabledValue = "disabled"
	// SpireEnabledValue is a non-operative label value under the opt-out model.
	// Retained as a named constant so tests can assert that a non-disabled value
	// does not block injection.
	SpireEnabledValue = "enabled"
	// Istio exclusion annotations
	IstioSidecarInjectAnnotation = "sidecar.istio.io/inject"
	AmbientRedirectionAnnotation = "ambient.istio.io/redirection"

	// Port exclusion annotations — per-workload iptables overrides.
	// Values are comma-separated port numbers. Outbound values are appended
	// to the mandatory exclusion (8080). Example: "11434,4317"
	OutboundPortsExcludeAnnotation = "rossoctl.io/outbound-ports-exclude"
	InboundPortsExcludeAnnotation  = "rossoctl.io/inbound-ports-exclude"

	sourceNamespaceConfigMap = "namespace-configmap"

	// RossoctlTypeLabel is the label key that identifies the workload type
	RossoctlTypeLabel = "rossoctl.io/type"
	// RossoctlTypeAgent is the label value that identifies agent workloads
	RossoctlTypeAgent = "agent"
	// RossoctlTypeTool is the label value that identifies tool workloads
	RossoctlTypeTool = "tool"
)

type PodMutator struct {
	Client    client.Client
	APIReader client.Reader // uncached reader for cross-namespace ConfigMap reads
	// Getter functions for hot-reloadable config (used by precedence evaluator)
	GetPlatformConfig func() *config.PlatformConfig
	GetFeatureGates   func() *config.FeatureGates
}

func NewPodMutator(
	c client.Client,
	apiReader client.Reader,
	getPlatformConfig func() *config.PlatformConfig,
	getFeatureGates func() *config.FeatureGates,
) *PodMutator {
	return &PodMutator{
		Client:            c,
		APIReader:         apiReader,
		GetPlatformConfig: getPlatformConfig,
		GetFeatureGates:   getFeatureGates,
	}
}

// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=create;get;list;patch;update;watch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=create;get;list;update;watch

// InjectAuthBridge evaluates the multi-layer precedence chain and conditionally injects sidecars.
//
//nolint:gocyclo // sequential injection steps form a single logical pipeline
func (m *PodMutator) InjectAuthBridge(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName, workloadKind string, labels, annotations map[string]string) (bool, error) {
	mutatorLog.Info("InjectAuthBridge called", "namespace", namespace, "crName", crName, "labels", labels)

	// Pre-filter: rossoctl.io/type must be agent or tool.
	rossoctlType, hasRossoctlLabel := labels[RossoctlTypeLabel]
	if !hasRossoctlLabel || (rossoctlType != RossoctlTypeAgent && rossoctlType != RossoctlTypeTool) {
		mutatorLog.Info("Skipping mutation: workload is not an agent or a tool",
			"hasLabel", hasRossoctlLabel,
			"labelValue", rossoctlType)
		return false, nil
	}

	// Get fresh config snapshots for this request (hot-reloadable)
	currentConfig := m.GetPlatformConfig()
	currentGates := m.GetFeatureGates()

	// Global kill switch — disables all injection cluster-wide.
	if !currentGates.GlobalEnabled {
		mutatorLog.Info("Skipping mutation: global feature gate disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Tool workloads are only injected when the injectTools feature gate is on.
	if rossoctlType == RossoctlTypeTool && !currentGates.InjectTools {
		mutatorLog.Info("Skipping mutation: tool injection disabled via injectTools feature gate",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Opt-out: skip injection when rossoctl.io/inject=disabled is explicitly set.
	if labels[AuthBridgeInjectLabel] == AuthBridgeDisabledValue {
		mutatorLog.Info("Skipping mutation: workload opted out via rossoctl.io/inject=disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Evaluate the per-sidecar precedence chain
	evaluator := NewPrecedenceEvaluator(currentGates)
	decision := evaluator.Evaluate(labels)

	// Log each sidecar decision
	for _, d := range []struct {
		name string
		sd   SidecarDecision
	}{
		{"envoy-proxy", decision.EnvoyProxy},
		{"proxy-init", decision.ProxyInit},
		{"spiffe-helper", decision.SpiffeHelper},
	} {
		mutatorLog.Info("injection decision",
			"sidecar", d.name,
			"inject", d.sd.Inject,
			"reason", d.sd.Reason,
			"layer", d.sd.Layer,
		)
	}

	if !decision.AnyInjected() {
		mutatorLog.Info("Skipping mutation (no sidecars to inject)", "namespace", namespace, "crName", crName)
		return false, nil
	}

	// Derive SPIRE mode from the injection decision: if spiffe-helper is being
	// injected then SPIRE volumes and a dedicated ServiceAccount are needed.
	spireEnabled := decision.SpiffeHelper.Inject

	// When SPIRE is enabled, ensure a dedicated ServiceAccount exists so
	// the SPIFFE ID reflects the workload name instead of "default".
	if spireEnabled && (podSpec.ServiceAccountName == "" || podSpec.ServiceAccountName == "default") {
		if err := m.ensureServiceAccount(ctx, namespace, crName); err != nil {
			mutatorLog.Error(err, "Failed to ensure ServiceAccount", "namespace", namespace, "name", crName)
			return false, fmt.Errorf("failed to ensure service account: %w", err)
		}
		podSpec.ServiceAccountName = crName
		mutatorLog.Info("Set ServiceAccountName for SPIRE identity", "namespace", namespace, "serviceAccount", crName)
	}

	// Initialize slices
	if podSpec.Containers == nil {
		podSpec.Containers = []corev1.Container{}
	}
	if podSpec.InitContainers == nil {
		podSpec.InitContainers = []corev1.Container{}
	}
	if podSpec.Volumes == nil {
		podSpec.Volumes = []corev1.Volume{}
	}

	// ========================================
	// Build containers + volumes
	// ========================================
	//
	// Two modes controlled by the perWorkloadConfigResolution feature gate:
	//   false (default) → legacy path: ValueFrom refs for env vars, kubelet
	//                     resolves ConfigMap/Secret values at container start.
	//   true            → resolved path: webhook reads namespace ConfigMaps/
	//                     Secrets at admission time and injects literal values.

	var builder *ContainerBuilder
	var requiredVolumes []corev1.Volume

	// Always read namespace config — needed for per-agent ConfigMap generation
	// regardless of the per-workload config resolution feature gate.
	// Use the uncached APIReader so ConfigMaps in agent namespaces (which may
	// not be in the manager's cache scope) are readable.
	reader := m.apiReader()
	nsConfig, nsConfigErr := ReadNamespaceConfig(ctx, reader, namespace)
	if nsConfigErr != nil {
		mutatorLog.Error(nsConfigErr, "failed to read namespace config, using empty defaults",
			"namespace", namespace)
		nsConfig = &NamespaceConfig{}
	}

	// Fetch the AgentRuntime CR for this workload (if it exists).
	// The CR may not exist if the workload was deployed before AgentRuntime CRDs were created.
	// In that case, we proceed with defaults (no spec.auth configuration).
	var agentRuntime *agentv1alpha1.AgentRuntime
	agentRuntimeList := &agentv1alpha1.AgentRuntimeList{}
	if err := reader.List(ctx, agentRuntimeList, client.InNamespace(namespace)); err != nil {
		mutatorLog.Info("failed to list AgentRuntimes (proceeding with defaults)",
			"namespace", namespace, "crName", crName, "error", err)
	} else {
		// Find AgentRuntime that targets this workload
		for i := range agentRuntimeList.Items {
			rt := &agentRuntimeList.Items[i]
			if rt.Spec.TargetRef.Name == crName && rt.Spec.TargetRef.Kind == workloadKind {
				agentRuntime = rt
				mutatorLog.Info("found AgentRuntime CR for workload",
					"namespace", namespace, "crName", crName, "agentRuntime", rt.Name)
				break
			}
		}
	}

	// ========================================
	// Resolve mTLS posture (namespace > "disabled")
	// ========================================
	//
	// Done BEFORE the volume-building / per-workload resolution so that
	// "mtlsMode != disabled implies SPIRE" can flip spireEnabled and
	// fall through to the existing SPIRE-aware code paths (volumes,
	// ServiceAccount, container env).
	// Resolution chain: namespace ConfigMap > "disabled".
	mtlsMode := ""
	mtlsSource := ""
	if m := ExtractMTLSMode(nsConfig.AuthBridgeRuntimeYAML); m != "" {
		mtlsMode = m
		mtlsSource = sourceNamespaceConfigMap
	}
	if mtlsMode == "" {
		mtlsMode = MTLSModePermissive
		mtlsSource = "default"
	}
	// Defense in depth: the CRD enum check rejects unknown values at
	// the API server, but the namespace ConfigMap and any future
	// non-CRD source feed in raw strings. A typo (e.g. "strikt") would
	// otherwise flow through unchecked. Same defensive pattern as the
	// authBridgeMode resolution above; do not drop this switch as
	// "redundant with CRD validation" — it covers paths the CRD doesn't.
	switch mtlsMode {
	case MTLSModeDisabled, MTLSModePermissive, MTLSModeStrict:
		// recognized, keep as-is
	default:
		mutatorLog.Info("WARN: unrecognized mtlsMode; defaulting to permissive",
			"namespace", namespace, "crName", crName,
			"unrecognized", mtlsMode, "source", mtlsSource)
		mtlsMode = MTLSModePermissive
		mtlsSource = "default-invalid-fallback"
	}
	mutatorLog.Info("resolved mTLS mode",
		"namespace", namespace, "crName", crName,
		"mode", mtlsMode, "source", mtlsSource)

	// Auto-enable SPIRE when mtls is on. The bundled spiffe-helper
	// writes /opt/svid*.pem from SPIRE-issued X.509 SVIDs; without
	// SPIRE the cert files never appear and authbridge stays in its
	// startup wait loop. Setting mtlsMode is sufficient declaration of
	// intent — the operator does not require a separate SPIRE label.
	if mtlsMode != MTLSModeDisabled && !spireEnabled {
		mutatorLog.Info("mtlsMode set; auto-enabling SPIRE for this workload",
			"namespace", namespace, "crName", crName, "mtlsMode", mtlsMode)
		spireEnabled = true
		if podSpec.ServiceAccountName == "" || podSpec.ServiceAccountName == "default" {
			if err := m.ensureServiceAccount(ctx, namespace, crName); err != nil {
				mutatorLog.Error(err, "Failed to ensure ServiceAccount for auto-enabled SPIRE",
					"namespace", namespace, "name", crName)
				return false, fmt.Errorf("failed to ensure service account for mtls auto-spire: %w", err)
			}
			podSpec.ServiceAccountName = crName
			mutatorLog.Info("Set ServiceAccountName for auto-enabled SPIRE identity",
				"namespace", namespace, "serviceAccount", crName)
		}
	}

	// ========================================
	// Resolve egressEnforcement (namespace > enforce-redirect)
	// ========================================
	//
	// Controls proxy-init injection in proxy-sidecar / lite modes.
	// "enforce-redirect" (default): proxy-init injected with iptables.
	// "none": proxy-init skipped — cooperative HTTP_PROXY only.
	// envoy-sidecar mode ignores this (proxy-init is structural there).
	egressEnforcement := ""
	egressEnforcementSource := ""
	if ee := ExtractEgressEnforcement(nsConfig.AuthBridgeRuntimeYAML); ee != "" {
		egressEnforcement = ee
		egressEnforcementSource = sourceNamespaceConfigMap
	}
	if egressEnforcement == "" {
		egressEnforcement = EgressEnforcementEnforceRedirect
		egressEnforcementSource = "cluster-default"
	}
	switch egressEnforcement {
	case EgressEnforcementEnforceRedirect, EgressEnforcementNone:
		// recognized, keep as-is
	default:
		mutatorLog.Info("WARN: unrecognized egressEnforcement; defaulting to enforce-redirect (fail closed)",
			"namespace", namespace, "crName", crName,
			"unrecognized", egressEnforcement, "source", egressEnforcementSource)
		egressEnforcement = EgressEnforcementEnforceRedirect
		egressEnforcementSource = "default-invalid-fallback"
	}
	// Validate against the platform's allowed list. If the resolved value
	// is not permitted, fall back to the first allowed value (fail closed
	// when only enforce-redirect is allowed, fail open when only none is
	// allowed — the admin controls the list).
	allowed := currentConfig.Proxy.AllowedEgressEnforcement
	if !slices.Contains(allowed, egressEnforcement) {
		mutatorLog.Info("WARN: egressEnforcement value not in platform allowedEgressEnforcement; overriding",
			"namespace", namespace, "crName", crName,
			"requested", egressEnforcement, "allowed", allowed,
			"overrideTo", allowed[0])
		egressEnforcement = allowed[0]
		egressEnforcementSource = "platform-policy-override"
	}
	mutatorLog.Info("resolved egress enforcement",
		"namespace", namespace, "crName", crName,
		"mode", egressEnforcement, "source", egressEnforcementSource)

	// ========================================
	// Resolve TLS bridge posture (CR > namespace > "disabled")
	// ========================================
	//
	// tlsBridgeMode controls whether the Go forward proxy terminates and
	// re-originates the agent's outbound TLS (the "TLS bridge"). Like mtlsMode,
	// it is a plain per-workload field (no cluster feature gate): the workload
	// (or its namespace) opts in with tlsBridgeMode=enabled. The bridge only
	// exists in the proxy-sidecar / lite shapes (the Go forward proxy); the
	// AgentRuntime validating webhook rejects enabled+envoy-sidecar upstream of
	// pod admission, so here we only resolve the value and let the proxy-sidecar
	// branch act on it. Resolution chain mirrors mtlsMode: an explicit CR value
	// pins; the namespace ConfigMap is the fallback; the default is "disabled".
	tlsBridgeMode := agentv1alpha1.TLSBridgeModeDisabled
	tlsBridgeSource := "default"
	if m := ExtractTLSBridgeMode(nsConfig.AuthBridgeRuntimeYAML); m != "" {
		tlsBridgeMode = m
		tlsBridgeSource = "namespace-configmap"
	}
	switch tlsBridgeMode {
	case agentv1alpha1.TLSBridgeModeDisabled, agentv1alpha1.TLSBridgeModeEnabled:
		// recognized, keep as-is
	default:
		mutatorLog.Info("WARN: unrecognized tlsBridgeMode; defaulting to disabled",
			"namespace", namespace, "crName", crName,
			"unrecognized", tlsBridgeMode, "source", tlsBridgeSource)
		tlsBridgeMode = agentv1alpha1.TLSBridgeModeDisabled
		tlsBridgeSource = "default-invalid-fallback"
	}
	mutatorLog.Info("resolved TLS bridge mode",
		"namespace", namespace, "crName", crName,
		"mode", tlsBridgeMode, "source", tlsBridgeSource)

	if currentGates.PerWorkloadConfigResolution {
		// Resolved path: build literal env vars from namespace config
		resolved := ResolveConfig(currentConfig, nsConfig)
		builder = NewResolvedContainerBuilder(resolved)
		requiredVolumes = BuildResolvedVolumes(spireEnabled, "")
	} else {
		// Legacy path: ValueFrom refs, kubelet resolves at runtime
		builder = NewContainerBuilder(currentConfig)
		if spireEnabled {
			requiredVolumes = BuildRequiredVolumes()
		} else {
			requiredVolumes = BuildRequiredVolumesNoSpire()
		}
		mutatorLog.Info("Using legacy ValueFrom config path",
			"namespace", namespace, "crName", crName)
	}

	// ========================================
	// Mode-aware injection
	// ========================================
	//
	// Three deployment shapes:
	//   proxy-sidecar (default) — HTTP_PROXY env + authbridge-proxy container (authbridge image)
	//   envoy-sidecar           — iptables + Envoy + ext_proc (authbridge-envoy image)
	//   waypoint                — standalone deployment, not injected as sidecar
	//
	// Resolution chain (first non-empty wins):
	//   1. namespace authbridge-runtime-config `mode:` field (namespace default)
	//   2. rossoctl.io/authbridge-mode annotation             (deprecated)
	//   3. ModeProxySidecar                                  (cluster-wide fallback)
	authBridgeMode := ""
	modeSource := ""
	if m := ExtractMode(nsConfig.AuthBridgeRuntimeYAML); m != "" {
		authBridgeMode = m
		modeSource = sourceNamespaceConfigMap
	}
	if authBridgeMode == "" {
		if m := annotations[AnnotationAuthBridgeMode]; m != "" {
			authBridgeMode = m
			modeSource = "annotation-deprecated"
			mutatorLog.Info("DEPRECATED: rossoctl.io/authbridge-mode annotation used; set mode in namespace authbridge-runtime-config ConfigMap instead",
				"namespace", namespace, "crName", crName, "mode", authBridgeMode)
		}
	}
	if authBridgeMode == "" {
		authBridgeMode = ModeProxySidecar
		modeSource = "cluster-default"
	}
	// Validate the resolved value. The CRD path is enum-checked by the
	// API server, but the namespace ConfigMap and the deprecated
	// annotation accept arbitrary strings — a typo (e.g.
	// "proxy-sidecart") would otherwise flow through to the
	// envoy-sidecar branch silently. Fall back to the cluster default
	// and log a warning so operators can spot the typo. Per PR #361
	// review feedback.
	switch authBridgeMode {
	case ModeProxySidecar, ModeEnvoySidecar, ModeLite, ModeWaypoint:
		// recognized, keep as-is
	default:
		mutatorLog.Info("WARN: unrecognized authBridgeMode; defaulting to proxy-sidecar",
			"namespace", namespace, "crName", crName,
			"unrecognized", authBridgeMode, "source", modeSource)
		authBridgeMode = ModeProxySidecar
		modeSource = "cluster-default-invalid-fallback"
	}
	mutatorLog.Info("resolved authbridge mode",
		"namespace", namespace, "crName", crName,
		"mode", authBridgeMode, "source", modeSource)

	if authBridgeMode == ModeWaypoint {
		mutatorLog.Info("waypoint mode — skipping sidecar injection (waypoint is a standalone deployment)",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	if authBridgeMode == ModeProxySidecar || authBridgeMode == ModeLite {
		// Proxy-sidecar / lite mode: inject the authbridge container + HTTP_PROXY env vars.
		// No iptables, no proxy-init, no Envoy.
		//
		// proxy-sidecar uses Images.AuthBridge (full plugin set including parsers).
		// lite           uses Images.AuthBridgeLite (auth-only — parsers dropped).
		// Listener layout, ports, ConfigMap shape, and SPIRE wiring are identical;
		// only the image differs.
		//
		// Port-stealing: the reverse proxy takes over the agent's original port so
		// the Service doesn't need patching. The agent is moved to a free port.
		//   Service → :8000 → reverse proxy (validates JWT) → :8002 → agent
		//   Agent outbound → HTTP_PROXY=127.0.0.1:8081 → forward proxy

		// Pick the image based on mode. The lite binary still accepts
		// mode=proxy-sidecar in its YAML config (lite is a build
		// variant, not a runtime mode), so the per-agent ConfigMap's
		// `mode:` field stays "proxy-sidecar" regardless.
		proxyImage := builder.cfg.Images.AuthBridge
		if authBridgeMode == ModeLite {
			proxyImage = builder.cfg.Images.AuthBridgeLite
		}

		// TLS bridge: engaged purely by the resolved per-workload mode (like
		// mtlsMode — there is no cluster feature gate). We are already inside the
		// proxy-sidecar / lite branch — the only shapes that host the Go forward
		// proxy — so no further authBridgeMode check is needed. tlsBridgeMode
		// ("disabled" omits the config block, "enabled" renders it + mounts the CA)
		// is passed straight to the per-agent ConfigMap renderer below.
		tlsBridgeOn := tlsBridgeMode == agentv1alpha1.TLSBridgeModeEnabled

		// Collect all ports in use across all containers in the pod.
		usedPorts := map[int32]bool{}
		for _, c := range podSpec.Containers {
			for _, p := range c.Ports {
				usedPorts[p.ContainerPort] = true
			}
		}

		// Find the first app container's port and steal it for the reverse proxy.
		var originalAgentPort int32
		var agentContainer *corev1.Container
		for i := range podSpec.Containers {
			c := &podSpec.Containers[i]
			if c.Name == AuthBridgeProxyContainerName {
				continue
			}
			if len(c.Ports) > 0 {
				originalAgentPort = c.Ports[0].ContainerPort
				agentContainer = c
				break
			}
		}

		if originalAgentPort == 0 {
			originalAgentPort = 8000
			mutatorLog.Info("no container port found, using default", "port", originalAgentPort)
		}
		if agentContainer == nil {
			mutatorLog.Info("no agent container found to relocate — reverse proxy backend may be unreachable")
		}

		// findFreePort returns the first port >= start that isn't in usedPorts,
		// and marks it as used.
		findFreePort := func(start int32) (int32, error) {
			p := start
			for usedPorts[p] && p <= 65535 {
				p++
			}
			if p > 65535 {
				return 0, fmt.Errorf("no free port available starting from %d", start)
			}
			usedPorts[p] = true
			return p, nil
		}

		// Reserve the original agent port for the reverse proxy
		usedPorts[originalAgentPort] = true

		newAgentPort, err := findFreePort(originalAgentPort + 1)
		if err != nil {
			return false, fmt.Errorf("proxy-sidecar port assignment: %w", err)
		}
		forwardProxyPort, err := findFreePort(8081)
		if err != nil {
			return false, fmt.Errorf("proxy-sidecar port assignment: %w", err)
		}

		// Move the agent to the free port.
		// Most agent frameworks (Python/uvicorn, Node/express, FastAPI) read the
		// PORT env var to determine the bind address. Go agents that hardcode their
		// listen port won't be affected by this env var — they must use PORT or
		// be configured via their own config mechanism.
		if agentContainer != nil {
			agentContainer.Ports[0].ContainerPort = newAgentPort
			setOrAddEnv(agentContainer, "PORT", fmt.Sprintf("%d", newAgentPort))
			mutatorLog.Info("proxy-sidecar port stealing",
				"container", agentContainer.Name,
				"originalPort", originalAgentPort,
				"movedTo", newAgentPort,
				"forwardProxyPort", forwardProxyPort)
		}

		// Create per-agent ConfigMap with proxy-sidecar listener addresses
		perAgentCMName, err := m.ensurePerAgentConfigMap(ctx, namespace, crName,
			ModeProxySidecar, nsConfig.AuthBridgeRuntimeYAML, nsConfig,
			map[string]string{
				"reverse_proxy_addr":    fmt.Sprintf(":%d", originalAgentPort),
				"reverse_proxy_backend": fmt.Sprintf("http://127.0.0.1:%d", newAgentPort),
				"forward_proxy_addr":    fmt.Sprintf(":%d", forwardProxyPort),
			},
			mtlsMode, tlsBridgeMode, spireEnabled, agentRuntime)
		if err != nil {
			return false, fmt.Errorf("proxy-sidecar per-agent ConfigMap: %w", err)
		}

		// Inject authbridge-proxy container listening on the original agent port
		if !containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
			podSpec.Containers = append(podSpec.Containers,
				builder.BuildProxySidecarContainerWithPorts(
					spireEnabled,
					proxyImage,
					originalAgentPort, // reverse proxy listens here
					newAgentPort,      // forwards to agent here
					forwardProxyPort,  // forward proxy listens here
				))
		}

		// Set MTLS_MODE env var on the authbridge container so it knows the
		// resolved mTLS posture at runtime.
		for i := range podSpec.Containers {
			if podSpec.Containers[i].Name == AuthBridgeProxyContainerName {
				setOrAddEnv(&podSpec.Containers[i], "MTLS_MODE", mtlsMode)
				break
			}
		}

		// Inject HTTP_PROXY env vars into all existing app containers
		for i := range podSpec.Containers {
			c := &podSpec.Containers[i]
			if c.Name == AuthBridgeProxyContainerName {
				continue
			}
			injectHTTPProxyEnv(c, forwardProxyPort)
		}

		// Egress enforcement for proxy-sidecar / lite.
		// HTTP_PROXY above is cooperative — an app that ignores it egresses
		// directly. When egressEnforcement is "enforce-redirect" (default),
		// proxy-init is injected with iptables rules that transparently
		// REDIRECT bypass egress to AuthBridge's transparent listener.
		// When "none", proxy-init is skipped — use on platforms where
		// iptables is unavailable (ROSA HCP, managed OpenShift).
		// envoy-sidecar mode has its own structural proxy-init and ignores
		// this setting.
		if egressEnforcement == EgressEnforcementEnforceRedirect {
			if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
				podSpec.InitContainers = append(podSpec.InitContainers,
					builder.BuildProxyInitContainer(ProxyInitModeEnforceRedirect, "", ""))
				mutatorLog.Info("proxy-sidecar egress enforcement enabled (enforce-redirect)",
					"namespace", namespace, "crName", crName)
			}
		} else {
			mutatorLog.Info("proxy-sidecar egress enforcement disabled (cooperative mode)",
				"namespace", namespace, "crName", crName,
				"egressEnforcement", egressEnforcement,
				"source", egressEnforcementSource)
		}

		// spiffe-helper is bundled in the authbridge combined image and
		// gated by SPIRE_ENABLED; client-registration is operator-managed.

		// Inject volumes — use per-agent ConfigMap name for authbridge config.
		// requiredVolumes is always set above (resolved or legacy path) before
		// the mode switch, so it is never nil here.
		proxyVolumes := overrideAuthBridgeConfigMapInVolumes(requiredVolumes, perAgentCMName)
		for i := range proxyVolumes {
			if !volumeExists(podSpec.Volumes, proxyVolumes[i].Name) {
				podSpec.Volumes = append(podSpec.Volumes, proxyVolumes[i])
			}
		}

		// Mount operator-managed Keycloak client credentials for any container that uses
		// shared-data (authbridge-proxy reads /shared/client-id.txt and /shared/client-secret.txt
		// for its jwt-validation + token-exchange plugins). Without this, proxy-sidecar mode
		// polls the credential files forever and rejects every inbound request with
		// 503 "identity not yet configured (credentials pending)". Envoy-sidecar mode
		// already calls this helper further down; the proxy-sidecar branch returns early,
		// so it needs its own invocation.
		ApplyKeycloakClientCredentialsSecretVolumes(podSpec, annotations)

		// TLS bridge: mount the per-agent CA so the sidecar can forge leaves and
		// the agent trusts them. The Secret is named after the workload (crName
		// == spec.targetRef.name), matching what TLSBridgeCAReconciler provisions.
		// The volume is a HARD mount, so if the gate is on but cert-manager has
		// not yet issued the Secret the pod blocks on start — which is the
		// intended startup-ordering barrier.
		if tlsBridgeOn {
			applyTLSBridgeMounts(podSpec, crName)
			mutatorLog.Info("TLS bridge enabled — mounted per-agent CA",
				"namespace", namespace, "crName", crName,
				"caSecret", crName+agentv1alpha1.TLSBridgeCASecretSuffix)
		}

		mutatorLog.Info("proxy-sidecar mode injection complete",
			"namespace", namespace, "crName", crName,
			"resolvedMode", authBridgeMode,
			"image", proxyImage,
			"perAgentConfigMap", perAgentCMName,
			"reverseProxyPort", originalAgentPort,
			"agentPort", newAgentPort,
			"forwardProxyPort", forwardProxyPort)

		if spireEnabled {
			ensureFSGroup(podSpec)
		}
		return true, nil
	}

	// ========================================
	// Envoy-sidecar mode
	// ========================================
	//
	// Single combined container (authbridge-envoy image): Envoy + ext_proc
	// authbridge + bundled spiffe-helper. proxy-init is a separate
	// init container. spiffe-helper starts conditionally on SPIRE_ENABLED.

	// The TLS bridge lives only in the Go forward proxy (proxy-sidecar / lite).
	// The validating webhook rejects a CR that sets BOTH authBridgeMode=envoy-sidecar
	// and tlsBridgeMode=enabled, but tlsBridgeMode can also resolve from the
	// namespace default while the effective mode is envoy-sidecar — that path
	// reaches here and the bridge silently does nothing. Warn loudly, mirroring
	// the gate-off log in the proxy-sidecar branch.
	if tlsBridgeMode == agentv1alpha1.TLSBridgeModeEnabled {
		mutatorLog.Info("WARN: tlsBridgeMode=enabled has no effect under authBridgeMode=envoy-sidecar "+
			"(the TLS bridge runs only in the proxy-sidecar/lite forward proxy); ignoring",
			"namespace", namespace, "crName", crName, "tlsBridgeSource", tlsBridgeSource)
	}

	// envoy-sidecar threads the resolved mtlsMode through to the per-agent
	// authbridge-config CM (so the Provider's spiffe block + any future
	// authbridge-side mTLS knobs reflect the CR's posture) AND renders a
	// per-agent envoy-config CM with the matching TLS blocks. The envoy
	// data plane terminates the actual TLS — DownstreamTlsContext on the
	// inbound listener (gated on MTLSEnabled) and UpstreamTlsContext on
	// original_destination_tls (strict only).
	perAgentCMName, err := m.ensurePerAgentConfigMap(ctx, namespace, crName,
		ModeEnvoySidecar, nsConfig.AuthBridgeRuntimeYAML, nsConfig, nil, mtlsMode, "", spireEnabled, agentRuntime) // bridge never runs under envoy-sidecar
	if err != nil {
		return false, fmt.Errorf("envoy-sidecar per-agent ConfigMap: %w", err)
	}
	requiredVolumes = overrideAuthBridgeConfigMapInVolumes(requiredVolumes, perAgentCMName)

	resolvedForEnvoy := ResolveConfig(currentConfig, nsConfig)
	resolvedForEnvoy.MTLSMode = mtlsMode
	envoyCMName, err := m.ensurePerAgentEnvoyConfigMap(ctx, namespace, crName, resolvedForEnvoy)
	if err != nil {
		return false, fmt.Errorf("envoy-sidecar per-agent envoy ConfigMap: %w", err)
	}
	requiredVolumes = overrideEnvoyConfigMapInVolumes(requiredVolumes, envoyCMName)

	if decision.EnvoyProxy.Inject && !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		podSpec.Containers = append(podSpec.Containers, builder.BuildEnvoyProxyContainerWithSpireOption(spireEnabled))
	}

	// Set MTLS_MODE env var on the envoy-sidecar authbridge container.
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == EnvoyProxyContainerName {
			setOrAddEnv(&podSpec.Containers[i], "MTLS_MODE", mtlsMode)
			break
		}
	}

	if decision.ProxyInit.Inject && !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		outboundExclude := annotations[OutboundPortsExcludeAnnotation]
		inboundExclude := annotations[InboundPortsExcludeAnnotation]
		podSpec.InitContainers = append(podSpec.InitContainers, builder.BuildProxyInitContainer(ProxyInitModeRedirect, outboundExclude, inboundExclude))
	}

	// Inject volumes
	for i := range requiredVolumes {
		if !volumeExists(podSpec.Volumes, requiredVolumes[i].Name) {
			podSpec.Volumes = append(podSpec.Volumes, requiredVolumes[i])
		}
	}

	// Mount operator-managed Keycloak client credentials if annotation is present
	ApplyKeycloakClientCredentialsSecretVolumes(podSpec, annotations)

	// Set fsGroup for shared volume access when SPIRE is enabled
	if spireEnabled {
		ensureFSGroup(podSpec)
	}

	mutatorLog.Info("Successfully mutated pod spec", "namespace", namespace, "crName", crName,
		"containers", len(podSpec.Containers),
		"initContainers", len(podSpec.InitContainers),
		"volumes", len(podSpec.Volumes),
		"spireEnabled", spireEnabled)
	return true, nil
}

const managedByLabel = "rossoctl.io/managed-by"
const managedByValue = "webhook"

// ensureServiceAccount creates a ServiceAccount in the given namespace if it
// does not already exist. This gives SPIRE-enabled workloads a dedicated
// identity so the SPIFFE ID is spiffe://<trust-domain>/ns/<ns>/sa/<name>
// rather than .../sa/default.
func (m *PodMutator) ensureServiceAccount(ctx context.Context, namespace, name string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
	}
	if err := m.Client.Create(ctx, sa); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &corev1.ServiceAccount{}
			if getErr := m.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing); getErr != nil {
				return fmt.Errorf("failed to fetch existing ServiceAccount %s/%s: %w", namespace, name, getErr)
			}
			if existing.Labels[managedByLabel] != managedByValue {
				mutatorLog.Info("WARNING: ServiceAccount exists but is not managed by this webhook",
					"namespace", namespace, "name", name,
					"existingLabels", existing.Labels)
			} else {
				mutatorLog.Info("ServiceAccount already exists", "namespace", namespace, "name", name)
			}
			return nil
		}
		return fmt.Errorf("failed to create ServiceAccount %s/%s: %w", namespace, name, err)
	}
	mutatorLog.Info("Created ServiceAccount", "namespace", namespace, "name", name)
	return nil
}

// apiReader returns the uncached API reader if available, otherwise falls back
// to the cached client. This ensures ConfigMap reads work in namespaces that
// are outside the manager's cache scope.
func (m *PodMutator) apiReader() client.Reader {
	if m.APIReader != nil {
		return m.APIReader
	}
	return m.Client
}

// perAgentConfigMapName returns the ConfigMap name for a specific agent's authbridge config.
func perAgentConfigMapName(crName string) string {
	return "authbridge-config-" + crName
}

// perAgentEnvoyConfigMapName returns the ConfigMap name for a
// specific agent's rendered Envoy YAML. Envoy-sidecar workloads with
// a non-disabled mtlsMode get a per-agent envoy-config CM so the TLS
// blocks vary per workload; without mTLS the namespace-level
// `envoy-config` is reused.
func perAgentEnvoyConfigMapName(crName string) string {
	return "envoy-config-" + crName
}

// synthesizePipeline builds the per-plugin pipeline section that
// maps 1:1 to NamespaceConfig (env-var-style authbridge-config
// values). Used only when the namespace's authbridge-runtime-config
// ConfigMap has no `pipeline:` of its own — typically a demo or
// operator-managed namespace that bypassed the Rossoctl Helm chart.
//
// The synthesized shape matches what plugins expect:
//   - jwt-validation.config.issuer from NamespaceConfig.Issuer
//     (for matching the JWT `iss` claim — the PUBLIC Keycloak URL in
//     split-horizon deployments). keycloak_url + keycloak_realm are
//     also passed through so the plugin can derive jwks_url from the
//     INTERNAL URL — the sidecar actually GETs this URL from inside
//     the cluster, and the public hostname typically won't resolve
//     from inside the mesh. See cortex#383 for why the
//     jwt-validation plugin needs its own copy of these fields.
//     Other plugin settings fall back to their own defaults —
//     audience_file=/shared/client-id.txt, bypass_paths=standard
//     probes.
//   - token-exchange.config with Keycloak URL/realm, default_policy,
//     and identity block keyed off ClientAuthType. File paths fall
//     through to plugin defaults so operators don't have to
//     boilerplate them.
//
// Empty NamespaceConfig fields are not emitted — the plugin's own
// defaults apply. That matches the minimum-viable config shown in
// authbridge/cmd/authbridge/README.md.
func synthesizePipeline(nsConfig *NamespaceConfig) map[string]interface{} {
	jwtCfg := map[string]interface{}{}
	if nsConfig.Issuer != "" {
		jwtCfg["issuer"] = nsConfig.Issuer
	}
	// Pass keycloak_url + keycloak_realm through to jwt-validation so
	// it can derive jwks_url from the internal URL rather than the
	// public issuer. Mirrors the pair already handed to token-exchange.
	if nsConfig.KeycloakURL != "" {
		jwtCfg["keycloak_url"] = nsConfig.KeycloakURL
	}
	if nsConfig.KeycloakRealm != "" {
		jwtCfg["keycloak_realm"] = nsConfig.KeycloakRealm
	}

	tokenCfg := map[string]interface{}{}
	if nsConfig.KeycloakURL != "" {
		tokenCfg["keycloak_url"] = nsConfig.KeycloakURL
	}
	if nsConfig.KeycloakRealm != "" {
		tokenCfg["keycloak_realm"] = nsConfig.KeycloakRealm
	}
	if nsConfig.DefaultOutboundPolicy != "" {
		tokenCfg["default_policy"] = nsConfig.DefaultOutboundPolicy
	}
	// Identity: only set type (file paths default per-plugin). Spiffe
	// mode carries over the jwt_svid_path explicitly because that
	// default lives in the plugin only when the operator actually
	// selected spiffe — the Helm chart reads ClientAuthType to pick.
	if nsConfig.ClientAuthType != "" {
		identity := map[string]interface{}{}
		if nsConfig.ClientAuthType == ClientAuthTypeFederatedJWT {
			identity["type"] = IdentityTypeSpiffe
			if nsConfig.JWTAudience != "" {
				identity["jwt_audience"] = nsConfig.JWTAudience
			}
		} else {
			identity["type"] = nsConfig.ClientAuthType
		}
		tokenCfg["identity"] = identity
	}

	return map[string]interface{}{
		"inbound": map[string]interface{}{
			"plugins": []interface{}{
				map[string]interface{}{
					"name":   "jwt-validation",
					"config": jwtCfg,
				},
			},
		},
		"outbound": map[string]interface{}{
			"plugins": []interface{}{
				map[string]interface{}{
					"name":   "token-exchange",
					"config": tokenCfg,
				},
			},
		},
	}
}

// ensurePerAgentConfigMap creates or updates a per-agent ConfigMap that merges the
// namespace-level authbridge-runtime-config with per-agent overrides (mode, listener
// addresses, mtls). The authbridge sidecar mounts this instead of the shared ConfigMap.
//
// If baseYAML is empty (namespace has no authbridge-runtime-config), a minimal config
// is generated from the NamespaceConfig fields.
//
// mtlsMode is the resolved mTLS posture (disabled / permissive / strict).
// All three modes (proxy-sidecar, lite, envoy-sidecar) can reach this
// function with any mtlsMode value. proxy-sidecar / lite consume the
// rendered `mtls:` block via the authbridge listener directly;
// envoy-sidecar uses a parallel per-agent envoy-config CM rendered by
// ensurePerAgentEnvoyConfigMap (Envoy terminates TLS in its data plane).
func (m *PodMutator) ensurePerAgentConfigMap(
	ctx context.Context,
	namespace, crName, mode string,
	baseYAML string,
	nsConfig *NamespaceConfig,
	listenerOverrides map[string]string,
	mtlsMode string,
	tlsBridgeMode string,
	spireEnabled bool,
	agentRuntime *agentv1alpha1.AgentRuntime,
) (string, error) {
	cmName := perAgentConfigMapName(crName)

	// Parse the base YAML into a generic map
	cfg := make(map[string]interface{})
	if baseYAML != "" {
		if err := yaml.Unmarshal([]byte(baseYAML), &cfg); err != nil {
			mutatorLog.Error(err, "failed to parse authbridge-runtime-config, using empty base",
				"namespace", namespace, "crName", crName, "baseYAMLLen", len(baseYAML))
			cfg = make(map[string]interface{})
		}
	}

	// If the base YAML has no `pipeline:` section, synthesize one
	// from NamespaceConfig. Happens in two cases:
	//
	//   1. baseYAML was empty (namespace has no authbridge-runtime-
	//      config ConfigMap at all).
	//   2. baseYAML was present but stale pre-migration shape — the
	//      parse succeeded but yielded top-level `inbound:` /
	//      `outbound:` / etc., which the authbridge binary now
	//      rejects at Validate time. Any top-level key the parser
	//      found is left alone and ignored; the synthesized
	//      `pipeline:` is what authbridge actually reads.
	//
	// When the base YAML already has `pipeline:` (Rossoctl Helm chart
	// emits it), this branch is skipped and we only layer mode +
	// listener overrides on top — the chart owns the plugin config
	// contents. See authbridge/authlib/plugins/CONVENTIONS.md for
	// the per-plugin config schema.
	if cfg["pipeline"] == nil && nsConfig != nil {
		cfg["pipeline"] = synthesizePipeline(nsConfig)
	}

	// Override mode
	cfg["mode"] = mode

	// Merge listener overrides
	if len(listenerOverrides) > 0 {
		listener, _ := cfg["listener"].(map[string]interface{})
		if listener == nil {
			listener = make(map[string]interface{})
		}
		for k, v := range listenerOverrides {
			listener[k] = v
		}
		cfg["listener"] = listener
	}

	// mTLS block. Cert paths are omitted on purpose — they match the
	// authbridge defaults (/opt/svid.pem, /opt/svid_key.pem,
	// /opt/svid_bundle.pem) written by the bundled spiffe-helper.
	// Surfacing them here would couple the operator to authbridge's
	// internal layout for no benefit.
	if mtlsMode != "" && mtlsMode != MTLSModeDisabled {
		cfg["mtls"] = map[string]interface{}{"mode": mtlsMode}
	} else {
		// Defensive: scrub any stale mtls block from the base YAML when
		// mTLS is off. Otherwise toggling mtlsMode back to disabled
		// without restarting the namespace ConfigMap would leak the
		// previous setting through to the per-agent CM.
		delete(cfg, "mtls")
	}

	// TLS bridge block. Rendered only when the caller decided bridging is on
	// (mode==enabled + proxy-sidecar/lite — see the mutate path). ca_dir is the
	// mounted cert-manager Secret dir (tls.crt/tls.key/ca.crt). Scrubbed
	// otherwise so toggling off doesn't leak a stale block from the base YAML.
	if tlsBridgeMode == agentv1alpha1.TLSBridgeModeEnabled {
		cfg["tls_bridge"] = map[string]interface{}{
			"mode":   agentv1alpha1.TLSBridgeModeEnabled,
			"ca_dir": TLSBridgeCAMountPath,
		}
	} else {
		delete(cfg, "tls_bridge")
	}

	if spireEnabled {
		cfg["spiffe"] = map[string]interface{}{
			"socket": m.GetPlatformConfig().Spiffe.SocketPath,
		}
	} else {
		delete(cfg, "spiffe")
	}

	// Generate token-exchange routes from AgentRuntime spec.auth.outbound
	// when mode is federated-jwt. Routes tell AuthBridge which audiences
	// to request when calling specific destinations.
	if agentRuntime != nil && agentRuntime.Spec.Auth != nil &&
		agentRuntime.Spec.Auth.Mode == "federated-jwt" &&
		len(agentRuntime.Spec.Auth.Outbound) > 0 {

		// Navigate to pipeline.outbound.plugins[token-exchange].config
		pipeline, _ := cfg["pipeline"].(map[string]interface{})
		if pipeline == nil {
			mutatorLog.Info("WARN: no pipeline block found, cannot inject routes",
				"namespace", namespace, "crName", crName)
		} else {
			outbound, _ := pipeline["outbound"].(map[string]interface{})
			if outbound == nil {
				mutatorLog.Info("WARN: no outbound block found, cannot inject routes",
					"namespace", namespace, "crName", crName)
			} else {
				plugins, _ := outbound["plugins"].([]interface{})
				if plugins == nil || len(plugins) == 0 {
					mutatorLog.Info("WARN: no outbound plugins found, cannot inject routes",
						"namespace", namespace, "crName", crName)
				} else {
					// Find the token-exchange plugin
					for i := range plugins {
						plugin, _ := plugins[i].(map[string]interface{})
						if plugin == nil {
							continue
						}
						pluginName, _ := plugin["name"].(string)
						if pluginName == "token-exchange" {
							pluginConfig, _ := plugin["config"].(map[string]interface{})
							if pluginConfig == nil {
								pluginConfig = make(map[string]interface{})
								plugin["config"] = pluginConfig
							}

							// Generate routes from spec.auth.outbound
							routes := make([]interface{}, 0, len(agentRuntime.Spec.Auth.Outbound))
							for _, outboundRoute := range agentRuntime.Spec.Auth.Outbound {
								route := map[string]interface{}{
									"audiences": outboundRoute.Audiences,
								}

								// Add destination match (host or hostRegex)
								destination := make(map[string]interface{})
								if outboundRoute.Destination.Host != "" {
									destination["host"] = outboundRoute.Destination.Host
								}
								if outboundRoute.Destination.HostRegex != "" {
									destination["hostRegex"] = outboundRoute.Destination.HostRegex
								}
								route["destination"] = destination

								routes = append(routes, route)
							}

							pluginConfig["routes"] = routes
							mutatorLog.Info("injected token-exchange routes from AgentRuntime spec.auth",
								"namespace", namespace, "crName", crName, "routeCount", len(routes))
							break
						}
					}
				}
			}
		}
	}

	// Marshal back to YAML
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal per-agent config for %s/%s: %w", namespace, crName, err)
	}

	// Server-side apply: atomic create-or-update in a single API call.
	// No race condition — concurrent pod admissions safely converge.
	cmApply := applyconfigscorev1.ConfigMap(cmName, namespace).
		WithLabels(map[string]string{managedByLabel: managedByValue}).
		WithData(map[string]string{"config.yaml": string(data)})

	// Set OwnerReference to the owning Deployment or StatefulSet so the
	// ConfigMap is garbage-collected when the workload is deleted.
	if ownerRef := m.buildOwnerReference(ctx, namespace, crName); ownerRef != nil {
		cmApply = cmApply.WithOwnerReferences(ownerRef)
	}

	if err := m.Client.Apply(ctx, cmApply, client.FieldOwner("rossoctl-webhook"), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("failed to apply per-agent ConfigMap %s/%s: %w", namespace, cmName, err)
	}
	mutatorLog.Info("Applied per-agent ConfigMap",
		"namespace", namespace, "name", cmName, "mode", mode, "mtlsMode", mtlsMode)

	return cmName, nil
}

// ensurePerAgentEnvoyConfigMap renders an envoy.yaml from the
// resolved config and writes it to a per-agent ConfigMap named
// envoy-config-<crName>. Used by the envoy-sidecar mode when
// mtlsMode != disabled so each workload's Envoy config carries the
// TLS blocks (DownstreamTlsContext on inbound, UpstreamTlsContext on
// the strict-only original_destination_tls cluster) derived from the
// resolved config's MTLSMode.
//
// Returns the ConfigMap name on success. The caller wires the
// returned name into the envoy-config volume reference so the
// envoy-proxy container mounts the per-agent CM instead of the
// namespace-level `envoy-config`.
//
// SSA + OwnerReference machinery mirrors ensurePerAgentConfigMap;
// the only differences are the CM name, the data key (envoy.yaml vs
// config.yaml), and the YAML rendering source (RenderEnvoyConfig vs
// the per-plugin authbridge runtime config).
func (m *PodMutator) ensurePerAgentEnvoyConfigMap(
	ctx context.Context,
	namespace, crName string,
	resolved *ResolvedConfig,
) (string, error) {
	cmName := perAgentEnvoyConfigMapName(crName)

	envoyYAML, err := RenderEnvoyConfig(resolved)
	if err != nil {
		return "", fmt.Errorf("rendering per-agent envoy.yaml for %s/%s: %w", namespace, crName, err)
	}

	cmApply := applyconfigscorev1.ConfigMap(cmName, namespace).
		WithLabels(map[string]string{managedByLabel: managedByValue}).
		WithData(map[string]string{"envoy.yaml": envoyYAML})

	if ownerRef := m.buildOwnerReference(ctx, namespace, crName); ownerRef != nil {
		cmApply = cmApply.WithOwnerReferences(ownerRef)
	}

	if err := m.Client.Apply(ctx, cmApply, client.FieldOwner("rossoctl-webhook"), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("failed to apply per-agent envoy ConfigMap %s/%s: %w", namespace, cmName, err)
	}
	mutatorLog.Info("Applied per-agent envoy ConfigMap",
		"namespace", namespace, "name", cmName, "mtlsMode", resolved.MTLSMode)

	return cmName, nil
}

// buildOwnerReference looks up the Deployment, StatefulSet, or Sandbox that
// owns the pod being created and returns an OwnerReference apply configuration.
// Returns nil if the workload cannot be found (best-effort).
func (m *PodMutator) buildOwnerReference(ctx context.Context, namespace, crName string) *applyconfigsmetav1.OwnerReferenceApplyConfiguration {
	// Uses the cached client (not APIReader) because Deployments/StatefulSets
	// are in the manager's default cache scope, unlike ConfigMaps which need
	// the uncached APIReader for agent namespaces.
	// Note: on the very first pod admission for a new Deployment, the informer
	// may not have synced yet, producing a ConfigMap without OwnerReference.
	// SSA re-convergence on subsequent pod admissions will add it.
	deploy := &appsv1.Deployment{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: crName}, deploy); err == nil {
		return applyconfigsmetav1.OwnerReference().
			WithAPIVersion("apps/v1").
			WithKind("Deployment").
			WithName(deploy.Name).
			WithUID(deploy.UID).
			WithController(true).
			WithBlockOwnerDeletion(true)
	}

	// Try StatefulSet
	sts := &appsv1.StatefulSet{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: crName}, sts); err == nil {
		return applyconfigsmetav1.OwnerReference().
			WithAPIVersion("apps/v1").
			WithKind("StatefulSet").
			WithName(sts.Name).
			WithUID(sts.UID).
			WithController(true).
			WithBlockOwnerDeletion(true)
	}

	// Try Sandbox (agents.x-k8s.io). The Sandbox CR name == the workload name,
	// so the per-agent ConfigMap is garbage-collected with the Sandbox, matching
	// Deployment/StatefulSet behavior.
	//
	// This relies on the shared manager cache having a warm Sandbox informer,
	// which the AgentRuntime controller keeps populated. If the webhook is ever
	// split into its own cache/process, this Get may miss until that cache syncs;
	// it degrades gracefully (any error → nil → no OwnerReference, the pre-Sandbox
	// behavior), so it never blocks injection.
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxOwnerGVK)
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: crName}, sandbox); err == nil {
		return applyconfigsmetav1.OwnerReference().
			WithAPIVersion(sandboxOwnerGVK.GroupVersion().String()).
			WithKind(sandboxOwnerGVK.Kind).
			WithName(sandbox.GetName()).
			WithUID(sandbox.GetUID()).
			WithController(true).
			WithBlockOwnerDeletion(true)
	}

	mutatorLog.V(1).Info("Could not find owner workload for per-agent ConfigMap, skipping OwnerReference",
		"namespace", namespace, "crName", crName)
	return nil
}

// sandboxOwnerGVK is the agent-sandbox CR GVK used to look up the owning Sandbox
// for per-agent ConfigMap garbage collection. Mirrors the controller's sandboxGVK.
var sandboxOwnerGVK = schema.GroupVersionKind{
	Group:   "agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    "Sandbox",
}

func containerExists(containers []corev1.Container, name string) bool {
	for i := range containers {
		if containers[i].Name == name {
			return true
		}
	}
	return false
}

func volumeExists(volumes []corev1.Volume, name string) bool {
	for i := range volumes {
		if volumes[i].Name == name {
			return true
		}
	}
	return false
}

// ensureFSGroup sets fsGroup in the pod security context to enable shared volume access.
// This allows containers with different UIDs (spiffe-helper, client-registration, envoy-proxy)
// to read/write files in shared volumes like svid-output.
func ensureFSGroup(podSpec *corev1.PodSpec) {
	fsGroupValue := int64(SharedVolumesFSGroup)

	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}

	if podSpec.SecurityContext.FSGroup == nil {
		podSpec.SecurityContext.FSGroup = &fsGroupValue
		mutatorLog.Info("Set fsGroup for shared volume access", "fsGroup", fsGroupValue)
	}
}

// injectHTTPProxyEnv adds HTTP_PROXY, HTTPS_PROXY, and NO_PROXY env vars to a container.
// Used in proxy-sidecar mode so the app routes outbound traffic through authbridge's
// forward proxy without iptables interception.
func injectHTTPProxyEnv(c *corev1.Container, forwardProxyPort int32) {
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", forwardProxyPort)
	envs := []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: proxyURL},
		{Name: "HTTPS_PROXY", Value: proxyURL},
		{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
	}
	for _, env := range envs {
		if !envExists(c.Env, env.Name) {
			c.Env = append(c.Env, env)
		}
	}
}

// setOrAddEnv sets an env var value, or adds it if it doesn't exist.
func setOrAddEnv(c *corev1.Container, name, value string) {
	for i := range c.Env {
		if c.Env[i].Name == name {
			c.Env[i].Value = value
			c.Env[i].ValueFrom = nil // clear any ValueFrom
			return
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
}

// envExists checks if an env var with the given name already exists.
func envExists(envs []corev1.EnvVar, name string) bool {
	for _, e := range envs {
		if e.Name == name {
			return true
		}
	}
	return false
}
