package injector

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/rossoctl/operator/internal/webhook/config"
)

// inboundCM builds a namespace authbridge-runtime-config pinning the inbound
// mechanism (and optionally the egress posture, which transparent depends on).
func inboundCM(inbound, egress string) *corev1.ConfigMap {
	yaml := "mode: " + ModeProxySidecar + "\n"
	if inbound != "" {
		yaml += "inboundInterception: " + inbound + "\n"
	}
	if egress != "" {
		yaml += "egressEnforcement: " + egress + "\n"
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeRuntimeConfigMapName, Namespace: "test-ns"},
		Data:       map[string]string{"config.yaml": yaml},
	}
}

func newTestMutatorWithAllowedInbound(allowed []string, objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{
		Client:    fakeClient,
		APIReader: fakeClient,
		GetPlatformConfig: func() *config.PlatformConfig {
			cfg := config.CompiledDefaults()
			cfg.Proxy.AllowedInboundInterception = allowed
			return cfg
		},
		GetFeatureGates: config.DefaultFeatureGates,
	}
}

func agentPodSpec() *corev1.PodSpec {
	return &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "agent",
			Image: "my-agent:latest",
			Ports: []corev1.ContainerPort{{ContainerPort: 8000}},
		}},
	}
}

func containerByName(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == name {
			return &spec.InitContainers[i]
		}
	}
	return nil
}

func envVarOf(c *corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// TestTransparentInbound_DoesNotStealAgentPort is the central behavioral claim:
// transparent interception leaves the agent exactly where it is. That removes
// both failure modes of port stealing — an agent that hardcodes its listen port
// (which collides with AuthBridge) and the relocated port that another pod can
// reach without JWT validation.
func TestTransparentInbound_DoesNotStealAgentPort(t *testing.T) {
	m := newTestMutator(inboundCM(InboundInterceptionTransparent, ""))
	ctx := context.Background()
	podSpec := agentPodSpec()

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}
	if !injected {
		t.Fatal("expected injection")
	}

	agent := containerByName(podSpec, "agent")
	if agent == nil {
		t.Fatal("agent container disappeared")
	}
	if got := agent.Ports[0].ContainerPort; got != 8000 {
		t.Errorf("agent port = %d, want 8000 unchanged (transparent must not relocate the agent)", got)
	}
	if _, ok := envVarOf(agent, "PORT"); ok {
		t.Error("transparent inbound must not inject PORT — the agent keeps its own port")
	}
}

// TestReverseProxyDefault_StillStealsPort locks the opt-in contract: with the
// field unset, behavior must be byte-identical to before this feature existed.
func TestReverseProxyDefault_StillStealsPort(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()
	podSpec := agentPodSpec()

	if _, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	agent := containerByName(podSpec, "agent")
	if got := agent.Ports[0].ContainerPort; got == 8000 {
		t.Error("default (reverse-proxy) must still relocate the agent off its original port")
	}
	if _, ok := envVarOf(agent, "PORT"); !ok {
		t.Error("default (reverse-proxy) must still inject PORT")
	}
}

// TestTransparentInbound_SidecarDeclaresTransparentPort checks the sidecar binds
// the REDIRECT target and no longer claims the agent's port.
func TestTransparentInbound_SidecarDeclaresTransparentPort(t *testing.T) {
	m := newTestMutator(inboundCM(InboundInterceptionTransparent, ""))
	ctx := context.Background()
	podSpec := agentPodSpec()

	if _, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	proxy := containerByName(podSpec, AuthBridgeProxyContainerName)
	if proxy == nil {
		t.Fatal("authbridge-proxy container not injected")
	}
	names := make([]string, 0, len(proxy.Ports))
	found := false
	for _, p := range proxy.Ports {
		names = append(names, fmt.Sprintf("%s=%d", p.Name, p.ContainerPort))
		if p.ContainerPort == 8083 {
			found = true
			if p.Name != "transparent-in" {
				t.Errorf("inbound port name = %q, want transparent-in (so the shape is visible in the pod spec)", p.Name)
			}
		}
		if p.ContainerPort == 8000 {
			t.Error("sidecar must not claim the agent's port under transparent interception")
		}
	}
	if !found {
		t.Errorf("sidecar does not declare the transparent inbound port 8083; got %v", names)
	}
}

// TestTransparentInbound_ConfigMapOmitsReverseProxyKeys matters because
// authbridge's own config validation REJECTS a reverse_proxy_addr alongside
// inbound_interception: transparent — emitting both would crash-loop the pod.
func TestTransparentInbound_ConfigMapOmitsReverseProxyKeys(t *testing.T) {
	m := newTestMutator(inboundCM(InboundInterceptionTransparent, ""))
	ctx := context.Background()

	if _, err := m.InjectAuthBridge(ctx, agentPodSpec(), "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	cm := fetchConfigMap(t, m, "test-ns", perAgentConfigMapName("my-agent"))
	cfg := parseConfigYAML(t, cm)
	listener, ok := cfg["listener"].(map[string]interface{})
	if !ok {
		t.Fatalf("listener block missing or not a map: %T", cfg["listener"])
	}

	if listener["inbound_interception"] != InboundInterceptionTransparent {
		t.Errorf("inbound_interception = %v, want %s", listener["inbound_interception"], InboundInterceptionTransparent)
	}
	if listener["transparent_inbound_addr"] != ":8083" {
		t.Errorf("transparent_inbound_addr = %v, want :8083", listener["transparent_inbound_addr"])
	}
	for _, key := range []string{"reverse_proxy_addr", "reverse_proxy_backend"} {
		if v, present := listener[key]; present {
			t.Errorf("%s must be absent under transparent interception (authbridge rejects it), got %v", key, v)
		}
	}
	// Egress is unaffected.
	if listener["forward_proxy_addr"] == nil {
		t.Error("forward_proxy_addr must still be set — transparent inbound changes only ingress")
	}
}

// TestTransparentInbound_ProxyInitEnv covers the two env vars without which the
// feature is either inert or half-enforced: the REDIRECT target, and POD_IP for
// the Istio ambient path that never traverses PREROUTING.
func TestTransparentInbound_ProxyInitEnv(t *testing.T) {
	m := newTestMutator(inboundCM(InboundInterceptionTransparent, ""))
	ctx := context.Background()
	podSpec := agentPodSpec()

	if _, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	init := containerByName(podSpec, ProxyInitContainerName)
	if init == nil {
		t.Fatal("proxy-init not injected — transparent inbound has nothing to REDIRECT with")
	}

	port, ok := envVarOf(init, "INBOUND_TRANSPARENT_PORT")
	if !ok || port.Value != "8083" {
		t.Errorf("INBOUND_TRANSPARENT_PORT = %q (present=%v), want 8083", port.Value, ok)
	}

	podIP, ok := envVarOf(init, "POD_IP")
	if !ok {
		t.Fatal("POD_IP missing: without it proxy-init cannot capture the ambient inbound path and refuses to start")
	}
	if podIP.ValueFrom == nil || podIP.ValueFrom.FieldRef == nil ||
		podIP.ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Errorf("POD_IP must come from the downward API status.podIP, got %+v", podIP.ValueFrom)
	}

	// POD_IPS covers the dual-stack case: with only POD_IP (the primary address)
	// the other family's ambient delivery would pass unvalidated while its
	// PREROUTING rules were installed.
	podIPs, ok := envVarOf(init, "POD_IPS")
	if !ok {
		t.Fatal("POD_IPS missing — IPv6 ambient inbound is uncaptured on a dual-stack pod")
	}
	if podIPs.ValueFrom == nil || podIPs.ValueFrom.FieldRef == nil ||
		podIPs.ValueFrom.FieldRef.FieldPath != "status.podIPs" {
		t.Errorf("POD_IPS must come from the downward API status.podIPs, got %+v", podIPs.ValueFrom)
	}

	// SIDECAR_PORTS_EXCLUDE must carry the RESOLVED forward-proxy port. If the
	// script's 8081 default were used and findFreePort had moved it, the inbound
	// REDIRECT would swallow the forward proxy's own port.
	excl, ok := envVarOf(init, "SIDECAR_PORTS_EXCLUDE")
	if !ok {
		t.Fatal("SIDECAR_PORTS_EXCLUDE missing — health/stats/session ports would be JWT-gated")
	}
	for _, want := range []string{"9091", "9093", "9094"} {
		if !strings.Contains(excl.Value, want) {
			t.Errorf("SIDECAR_PORTS_EXCLUDE=%q missing %s", excl.Value, want)
		}
	}

	proxy := containerByName(podSpec, AuthBridgeProxyContainerName)
	var fwd int32
	for _, p := range proxy.Ports {
		if p.Name == "forward-proxy" {
			fwd = p.ContainerPort
		}
	}
	if fwd == 0 {
		t.Fatal("could not determine the resolved forward-proxy port")
	}
	if !strings.Contains(excl.Value, fmt.Sprintf("%d", fwd)) {
		t.Errorf("SIDECAR_PORTS_EXCLUDE=%q must contain the resolved forward-proxy port %d", excl.Value, fwd)
	}
}

// TestTransparentInbound_HonorsInboundPortsExcludeAnnotation covers the escape
// hatch for app ports that must not be validated (e.g. an oauth-proxy doing its
// own auth).
func TestTransparentInbound_HonorsInboundPortsExcludeAnnotation(t *testing.T) {
	m := newTestMutator(inboundCM(InboundInterceptionTransparent, ""))
	ctx := context.Background()
	podSpec := agentPodSpec()

	annotations := map[string]string{InboundPortsExcludeAnnotation: "8443"}
	if _, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, annotations); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	init := containerByName(podSpec, ProxyInitContainerName)
	excl, ok := envVarOf(init, "INBOUND_PORTS_EXCLUDE")
	if !ok || !strings.Contains(excl.Value, "8443") {
		t.Errorf("INBOUND_PORTS_EXCLUDE = %q (present=%v), want it to contain 8443", excl.Value, ok)
	}
}

// TestTransparentInbound_RequiresEnforceRedirect: the two features share one
// proxy-init container. With egressEnforcement "none" there is no init container
// to carry the PREROUTING rules, so the listener would bind a port nothing
// redirects to — inbound silently unenforced. Fall back to port stealing, which
// at least validates Service-routed traffic.
func TestTransparentInbound_RequiresEnforceRedirect(t *testing.T) {
	m := newTestMutator(inboundCM(InboundInterceptionTransparent, EgressEnforcementNone))
	ctx := context.Background()
	podSpec := agentPodSpec()

	if _, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	agent := containerByName(podSpec, "agent")
	if got := agent.Ports[0].ContainerPort; got == 8000 {
		t.Error("expected fallback to port stealing when egressEnforcement=none (no proxy-init to REDIRECT with)")
	}
	cm := fetchConfigMap(t, m, "test-ns", perAgentConfigMapName("my-agent"))
	listener := parseConfigYAML(t, cm)["listener"].(map[string]interface{})
	if _, present := listener["transparent_inbound_addr"]; present {
		t.Error("must not configure a transparent inbound listener that nothing can REDIRECT to")
	}
}

// TestTransparentInbound_PlatformPolicyCanForbid: transparent inbound grants a
// NET_ADMIN init container, so an admin must be able to forbid it cluster-wide.
func TestTransparentInbound_PlatformPolicyCanForbid(t *testing.T) {
	m := newTestMutatorWithAllowedInbound(
		[]string{InboundInterceptionReverseProxy},
		inboundCM(InboundInterceptionTransparent, ""),
	)
	ctx := context.Background()
	podSpec := agentPodSpec()

	if _, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	agent := containerByName(podSpec, "agent")
	if got := agent.Ports[0].ContainerPort; got == 8000 {
		t.Error("platform policy forbidding transparent must force the port-stealing shape")
	}
}

// TestTransparentInbound_UnrecognizedValueFallsBackToNoPrivilege: an unknown
// value must not silently grant a privileged init container.
func TestTransparentInbound_UnrecognizedValueFallsBackToNoPrivilege(t *testing.T) {
	m := newTestMutator(inboundCM("tranparent", "")) // plausible typo (dropped letter)
	ctx := context.Background()
	podSpec := agentPodSpec()

	if _, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment",
		map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}, nil); err != nil {
		t.Fatalf("InjectAuthBridge() error: %v", err)
	}

	init := containerByName(podSpec, ProxyInitContainerName)
	if init != nil {
		if _, ok := envVarOf(init, "INBOUND_TRANSPARENT_PORT"); ok {
			t.Error("an unrecognized inboundInterception must not enable inbound capture")
		}
	}
	agent := containerByName(podSpec, "agent")
	if got := agent.Ports[0].ContainerPort; got == 8000 {
		t.Error("unrecognized value must fall back to the port-stealing default")
	}
}

// TestExtractInboundInterception covers the namespace-config parse layer,
// including malformed YAML falling through rather than erroring.
func TestExtractInboundInterception(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"present", "mode: proxy-sidecar\ninboundInterception: transparent\n", "transparent"},
		{"absent", "mode: proxy-sidecar\n", ""},
		{"empty", "", ""},
		{"malformed falls through", "mode: [unclosed\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractInboundInterception(tc.yaml); got != tc.want {
				t.Errorf("ExtractInboundInterception(%q) = %q, want %q", tc.yaml, got, tc.want)
			}
		})
	}
}
