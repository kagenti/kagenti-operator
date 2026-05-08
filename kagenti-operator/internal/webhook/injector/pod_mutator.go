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
	"strings"

	"github.com/kagenti/operator/internal/webhook/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applyconfigscorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyconfigsmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

var mutatorLog = logf.Log.WithName("pod-mutator")

const (
	// Container names
	SpiffeHelperContainerName       = "spiffe-helper"
	ClientRegistrationContainerName = "kagenti-client-registration"

	// Label selector for authbridge injection opt-out.
	// Injection uses opt-out semantics for agents: sidecars are injected by
	// default. Setting AuthBridgeInjectLabel=AuthBridgeDisabledValue on a
	// workload explicitly opts it out. Any other value (including absent) does
	// not block injection.
	AuthBridgeInjectLabel   = "kagenti.io/inject"
	AuthBridgeInjectValue   = "enabled" // retained for backwards compat / tests
	AuthBridgeDisabledValue = "disabled"

	// SPIRE opt-out label. Setting kagenti.io/spire=disabled on a workload blocks
	// spiffe-helper injection (layer 7 of the precedence chain). Any other value
	// (including absence of the label) leaves spiffe-helper injection to the
	// upstream precedence layers.
	SpireEnableLabel   = "kagenti.io/spire"
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
	OutboundPortsExcludeAnnotation = "kagenti.io/outbound-ports-exclude"
	InboundPortsExcludeAnnotation  = "kagenti.io/inbound-ports-exclude"

	// KagentiTypeLabel is the label key that identifies the workload type
	KagentiTypeLabel = "kagenti.io/type"
	// KagentiTypeAgent is the label value that identifies agent workloads
	KagentiTypeAgent = "agent"
	// KagentiTypeTool is the label value that identifies tool workloads
	KagentiTypeTool = "tool"
)

type PodMutator struct {
	Client                   client.Client
	APIReader                client.Reader // uncached reader for cross-namespace ConfigMap reads
	EnableClientRegistration bool
	// Getter functions for hot-reloadable config (used by precedence evaluator)
	GetPlatformConfig func() *config.PlatformConfig
	GetFeatureGates   func() *config.FeatureGates
}

func NewPodMutator(
	c client.Client,
	apiReader client.Reader,
	enableClientRegistration bool,
	getPlatformConfig func() *config.PlatformConfig,
	getFeatureGates func() *config.FeatureGates,
) *PodMutator {
	return &PodMutator{
		Client:                   c,
		APIReader:                apiReader,
		EnableClientRegistration: enableClientRegistration,
		GetPlatformConfig:        getPlatformConfig,
		GetFeatureGates:          getFeatureGates,
	}
}

// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=create;get;list;patch;update;watch

// InjectAuthBridge evaluates the multi-layer precedence chain and conditionally injects sidecars.
//
//nolint:gocyclo // sequential injection steps form a single logical pipeline
func (m *PodMutator) InjectAuthBridge(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName string, labels, annotations map[string]string) (bool, error) {
	mutatorLog.Info("InjectAuthBridge called", "namespace", namespace, "crName", crName, "labels", labels)

	// Pre-filter: kagenti.io/type must be agent or tool.
	kagentiType, hasKagentiLabel := labels[KagentiTypeLabel]
	if !hasKagentiLabel || (kagentiType != KagentiTypeAgent && kagentiType != KagentiTypeTool) {
		mutatorLog.Info("Skipping mutation: workload is not an agent or a tool",
			"hasLabel", hasKagentiLabel,
			"labelValue", kagentiType)
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
	if kagentiType == KagentiTypeTool && !currentGates.InjectTools {
		mutatorLog.Info("Skipping mutation: tool injection disabled via injectTools feature gate",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Opt-out: skip injection when kagenti.io/inject=disabled is explicitly set.
	if labels[AuthBridgeInjectLabel] == AuthBridgeDisabledValue {
		mutatorLog.Info("Skipping mutation: workload opted out via kagenti.io/inject=disabled",
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
		{"client-registration", decision.ClientRegistration},
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

	// Read AgentRuntime CR overrides. If no CR exists the webhook still
	// injects sidecars using defaults-only config (platform + namespace
	// defaults, no per-workload overrides). ResolveConfig handles nil
	// overrides transparently.
	arOverrides, err := ReadAgentRuntimeOverrides(ctx, m.Client, namespace, crName)
	if err != nil {
		mutatorLog.Error(err, "failed to read AgentRuntime",
			"namespace", namespace, "crName", crName)
		return false, err
	}
	if arOverrides == nil {
		mutatorLog.Info("No AgentRuntime CR found, injecting with defaults-only config",
			"namespace", namespace, "crName", crName)
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

	if currentGates.PerWorkloadConfigResolution {
		// Resolved path: build literal env vars from namespace config
		// arOverrides was already read above as a gate check.
		resolved := ResolveConfig(currentConfig, nsConfig, arOverrides)
		builder = NewResolvedContainerBuilder(resolved)
		requiredVolumes = BuildResolvedVolumes(spireEnabled, "")

		mutatorLog.Info("Using resolved config path",
			"namespace", namespace, "crName", crName,
			"hasAgentRuntimeOverrides", arOverrides != nil)
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
	// The authbridge-mode annotation selects the deployment pattern:
	//   envoy-sidecar (default) — iptables + Envoy + ext_proc (authbridge-envoy image)
	//   proxy-sidecar           — HTTP_PROXY env + lightweight authbridge (authbridge-light image)
	//   waypoint                — standalone deployment, not injected as sidecar
	authBridgeMode := annotations[AnnotationAuthBridgeMode]
	if authBridgeMode == "" {
		authBridgeMode = ModeEnvoySidecar
	}

	if authBridgeMode == ModeWaypoint {
		mutatorLog.Info("waypoint mode — skipping sidecar injection (waypoint is a standalone deployment)",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	if authBridgeMode == ModeProxySidecar {
		// Proxy-sidecar mode: inject lightweight authbridge container + HTTP_PROXY env vars.
		// No iptables, no proxy-init, no Envoy.
		//
		// Port-stealing: the reverse proxy takes over the agent's original port so
		// the Service doesn't need patching. The agent is moved to a free port.
		//   Service → :8000 → reverse proxy (validates JWT) → :8002 → agent
		//   Agent outbound → HTTP_PROXY=127.0.0.1:8081 → forward proxy

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
			if c.Name == AuthBridgeProxyContainerName ||
				c.Name == SpiffeHelperContainerName ||
				c.Name == ClientRegistrationContainerName {
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
			})
		if err != nil {
			return false, fmt.Errorf("proxy-sidecar per-agent ConfigMap: %w", err)
		}

		// Inject authbridge-proxy container listening on the original agent port
		if !containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
			podSpec.Containers = append(podSpec.Containers,
				builder.BuildProxySidecarContainerWithPorts(
					spireEnabled,
					originalAgentPort, // reverse proxy listens here
					newAgentPort,      // forwards to agent here
					forwardProxyPort,  // forward proxy listens here
				))
		}

		// Inject HTTP_PROXY env vars into all existing app containers
		for i := range podSpec.Containers {
			c := &podSpec.Containers[i]
			if c.Name == AuthBridgeProxyContainerName ||
				c.Name == SpiffeHelperContainerName ||
				c.Name == ClientRegistrationContainerName {
				continue
			}
			injectHTTPProxyEnv(c, forwardProxyPort)
		}

		// spiffe-helper and client-registration are still injected
		if decision.SpiffeHelper.Inject && !containerExists(podSpec.Containers, SpiffeHelperContainerName) {
			podSpec.Containers = append(podSpec.Containers, builder.BuildSpiffeHelperContainer())
		}
		if decision.ClientRegistration.Inject && !containerExists(podSpec.Containers, ClientRegistrationContainerName) {
			podSpec.Containers = append(podSpec.Containers, builder.BuildClientRegistrationContainerWithSpireOption(crName, namespace, spireEnabled))
		}

		// Inject volumes — use per-agent ConfigMap name for authbridge config.
		// requiredVolumes is always set above (resolved or legacy path) before
		// the mode switch, so it is never nil here.
		proxyVolumes := overrideAuthBridgeConfigMapInVolumes(requiredVolumes, perAgentCMName)
		for i := range proxyVolumes {
			if !volumeExists(podSpec.Volumes, proxyVolumes[i].Name) {
				podSpec.Volumes = append(podSpec.Volumes, proxyVolumes[i])
			}
		}

		mutatorLog.Info("proxy-sidecar mode injection complete",
			"namespace", namespace, "crName", crName,
			"image", builder.cfg.Images.AuthBridgeLight,
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
	// Envoy-sidecar mode (default)
	// ========================================

	// Create per-agent ConfigMap for envoy-sidecar mode (no listener overrides).
	// Skip when combinedSidecar is enabled — that container uses env vars directly
	// and does not mount authbridge-runtime-config.
	if !currentGates.CombinedSidecar {
		perAgentCMName, err := m.ensurePerAgentConfigMap(ctx, namespace, crName,
			ModeEnvoySidecar, nsConfig.AuthBridgeRuntimeYAML, nsConfig, nil)
		if err != nil {
			return false, fmt.Errorf("envoy-sidecar per-agent ConfigMap: %w", err)
		}
		requiredVolumes = overrideAuthBridgeConfigMapInVolumes(requiredVolumes, perAgentCMName)
	}

	// Conditionally inject sidecars based on precedence decisions.
	// Two modes controlled by the combinedSidecar feature gate:
	//   true  → combined mode: single "authbridge" container replaces envoy-proxy +
	//           spiffe-helper + client-registration. proxy-init is still separate.
	//   false → legacy mode: separate sidecar containers (unchanged behavior).
	if currentGates.CombinedSidecar {
		// Combined mode: inject single authbridge container (only when envoy-proxy is enabled)
		if decision.EnvoyProxy.Inject && !containerExists(podSpec.Containers, AuthBridgeContainerName) {
			podSpec.Containers = append(podSpec.Containers,
				builder.BuildAuthBridgeContainer(crName, namespace,
					decision.SpiffeHelper.Inject,
					decision.ClientRegistration.Inject))
		}
		// proxy-init is still injected separately
		if decision.ProxyInit.Inject && !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
			outboundExclude := annotations[OutboundPortsExcludeAnnotation]
			inboundExclude := annotations[InboundPortsExcludeAnnotation]
			podSpec.InitContainers = append(podSpec.InitContainers, builder.BuildProxyInitContainer(outboundExclude, inboundExclude))
		}
	} else {
		// Legacy mode: separate sidecar containers
		if decision.EnvoyProxy.Inject && !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
			podSpec.Containers = append(podSpec.Containers, builder.BuildEnvoyProxyContainerWithSpireOption(spireEnabled))
		}

		if decision.ProxyInit.Inject && !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
			outboundExclude := annotations[OutboundPortsExcludeAnnotation]
			inboundExclude := annotations[InboundPortsExcludeAnnotation]
			podSpec.InitContainers = append(podSpec.InitContainers, builder.BuildProxyInitContainer(outboundExclude, inboundExclude))
		}

		if decision.SpiffeHelper.Inject && !containerExists(podSpec.Containers, SpiffeHelperContainerName) {
			podSpec.Containers = append(podSpec.Containers, builder.BuildSpiffeHelperContainer())
		}

		if decision.ClientRegistration.Inject && !containerExists(podSpec.Containers, ClientRegistrationContainerName) {
			podSpec.Containers = append(podSpec.Containers, builder.BuildClientRegistrationContainerWithSpireOption(crName, namespace, spireEnabled))
		}
	}

	// Inject volumes
	for i := range requiredVolumes {
		if !volumeExists(podSpec.Volumes, requiredVolumes[i].Name) {
			podSpec.Volumes = append(podSpec.Volumes, requiredVolumes[i])
		}
	}

	// Mount operator-managed Keycloak client credentials if annotation is present
	ApplyKeycloakClientCredentialsSecretVolumes(podSpec, annotations)

	// Log how credentials are delivered for this pod
	logClientRegistrationPaths(namespace, crName, labels, currentGates.CombinedSidecar, decision, annotations)

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

const managedByLabel = "kagenti.io/managed-by"
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

// ensurePerAgentConfigMap creates or updates a per-agent ConfigMap that merges the
// namespace-level authbridge-runtime-config with per-agent overrides (mode, listener
// addresses). The authbridge sidecar mounts this instead of the shared ConfigMap.
//
// If baseYAML is empty (namespace has no authbridge-runtime-config), a minimal config
// is generated from the NamespaceConfig fields.
func (m *PodMutator) ensurePerAgentConfigMap(
	ctx context.Context,
	namespace, crName, mode string,
	baseYAML string,
	nsConfig *NamespaceConfig,
	listenerOverrides map[string]string,
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

	// If the base was empty or missing key fields, populate from NamespaceConfig
	if cfg["inbound"] == nil && nsConfig != nil {
		inbound := map[string]interface{}{}
		if nsConfig.Issuer != "" {
			inbound["issuer"] = nsConfig.Issuer
		}
		if len(inbound) > 0 {
			cfg["inbound"] = inbound
		}
	}
	if cfg["outbound"] == nil && nsConfig != nil {
		outbound := map[string]interface{}{}
		if nsConfig.KeycloakURL != "" {
			outbound["keycloak_url"] = nsConfig.KeycloakURL
		}
		if nsConfig.KeycloakRealm != "" {
			outbound["keycloak_realm"] = nsConfig.KeycloakRealm
		}
		if nsConfig.DefaultOutboundPolicy != "" {
			outbound["default_policy"] = nsConfig.DefaultOutboundPolicy
		}
		if len(outbound) > 0 {
			cfg["outbound"] = outbound
		}
	}
	if cfg["identity"] == nil && nsConfig != nil && nsConfig.ClientAuthType != "" {
		identity := map[string]interface{}{
			"client_id_file":     "/shared/client-id.txt",
			"client_secret_file": "/shared/client-secret.txt",
		}
		if nsConfig.ClientAuthType == ClientAuthTypeFederatedJWT {
			identity["type"] = IdentityTypeSpiffe
			identity["jwt_svid_path"] = "/opt/jwt_svid.token"
		} else {
			identity["type"] = nsConfig.ClientAuthType
		}
		cfg["identity"] = identity
	}
	if cfg["bypass"] == nil {
		cfg["bypass"] = map[string]interface{}{
			"inbound_paths": []string{
				"/.well-known/*",
				"/healthz",
				"/readyz",
				"/livez",
			},
		}
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

	if err := m.Client.Apply(ctx, cmApply, client.FieldOwner("kagenti-webhook"), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("failed to apply per-agent ConfigMap %s/%s: %w", namespace, cmName, err)
	}
	mutatorLog.Info("Applied per-agent ConfigMap", "namespace", namespace, "name", cmName, "mode", mode)

	return cmName, nil
}

// buildOwnerReference looks up the Deployment or StatefulSet that owns the
// pod being created and returns an OwnerReference apply configuration.
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

	mutatorLog.V(1).Info("Could not find owner workload for per-agent ConfigMap, skipping OwnerReference",
		"namespace", namespace, "crName", crName)
	return nil
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

// logClientRegistrationPaths logs how Keycloak credentials are delivered to this pod.
func logClientRegistrationPaths(namespace, crName string, labels map[string]string, combinedSidecar bool, decision InjectionDecision, annotations map[string]string) {
	keycloakClientCredentialsSecret := strings.TrimSpace(annotations[AnnotationKeycloakClientSecretName])

	var paths []string
	if keycloakClientCredentialsSecret != "" {
		paths = append(paths, "operator-secret")
	}

	if combinedSidecar {
		if decision.EnvoyProxy.Inject && decision.ClientRegistration.Inject {
			paths = append(paths, "combined-authbridge")
		}
	} else if decision.ClientRegistration.Inject {
		paths = append(paths, "sidecar")
	}

	if len(paths) == 0 {
		paths = append(paths, "skip")
	}

	mutatorLog.Info("AuthBridge client registration: how credentials are supplied for this Pod",
		"namespace", namespace,
		"workloadKey", crName,
		"kagentiType", labels[KagentiTypeLabel],
		"deliveryPaths", strings.Join(paths, ","),
		"keycloakClientCredentialsSecretName", keycloakClientCredentialsSecret,
		"combinedSidecarMode", combinedSidecar,
		"injectClientRegistrationSidecar", decision.ClientRegistration.Inject,
		"injectEnvoyOrAuthbridge", decision.EnvoyProxy.Inject,
	)
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
