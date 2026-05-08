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
	"testing"

	agentv1alpha1 "github.com/kagenti/operator/api/v1alpha1"
	"github.com/kagenti/operator/internal/webhook/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

// newAgentRuntime creates a minimal AgentRuntime CR targeting the given workload name.
func newAgentRuntime(namespace, targetName string) *agentv1alpha1.AgentRuntime {
	return &agentv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetName + "-runtime",
			Namespace: namespace,
		},
		Spec: agentv1alpha1.AgentRuntimeSpec{
			Type: agentv1alpha1.RuntimeTypeAgent,
			TargetRef: agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       targetName,
			},
		},
	}
}

func newTestMutator(objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = agentv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{
		Client:                   fakeClient,
		APIReader:                fakeClient,
		EnableClientRegistration: true,
		GetPlatformConfig:        config.CompiledDefaults,
		GetFeatureGates:          config.DefaultFeatureGates,
	}
}

func TestEnsureServiceAccount_CreatesNew(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}

	sa := &corev1.ServiceAccount{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa); err != nil {
		t.Fatalf("expected ServiceAccount to be created, got error: %v", err)
	}
	if sa.Labels[managedByLabel] != managedByValue {
		t.Errorf("expected label %s=%s, got %s", managedByLabel, managedByValue, sa.Labels[managedByLabel])
	}
}

func TestEnsureServiceAccount_AlreadyExistsWithLabel(t *testing.T) {
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "test-ns",
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
	}
	m := newTestMutator(existing)
	ctx := context.Background()

	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}
}

func TestEnsureServiceAccount_AlreadyExistsWithoutLabel(t *testing.T) {
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "something-else"},
		},
	}
	m := newTestMutator(existing)
	ctx := context.Background()

	// Should still succeed (returns nil) but logs a warning internally.
	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}

	sa := &corev1.ServiceAccount{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa); err != nil {
		t.Fatalf("expected ServiceAccount to still exist, got error: %v", err)
	}
	if sa.Labels[managedByLabel] == managedByValue {
		t.Error("existing SA should NOT have been updated with the managed-by label")
	}
}

func TestEnsureServiceAccount_AlreadyExistsNoLabels(t *testing.T) {
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "test-ns",
		},
	}
	m := newTestMutator(existing)
	ctx := context.Background()

	if err := m.ensureServiceAccount(ctx, "test-ns", "my-agent"); err != nil {
		t.Fatalf("ensureServiceAccount() returned error: %v", err)
	}
}

func TestInjectAuthBridge_NoAgentRuntime_InjectsWithDefaults(t *testing.T) {
	// Agent pod with correct labels but no AgentRuntime CR → inject with
	// defaults-only config (platform + namespace defaults, no CR overrides).
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true with defaults-only config")
	}

	// Verify specific sidecar containers are present
	if !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Errorf("expected %s container to be injected", EnvoyProxyContainerName)
	}
	if !containerExists(podSpec.Containers, SpiffeHelperContainerName) {
		t.Errorf("expected %s container to be injected", SpiffeHelperContainerName)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Errorf("expected %s init container to be injected", ProxyInitContainerName)
	}
}

func TestInjectAuthBridge_SetsServiceAccountName(t *testing.T) {
	// Opt-out model: agent workloads are injected by default (no inject label needed).
	m := newTestMutator(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}
	if podSpec.ServiceAccountName != "my-agent" {
		t.Errorf("expected ServiceAccountName=%q, got %q", "my-agent", podSpec.ServiceAccountName)
	}

	sa := &corev1.ServiceAccount{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa); err != nil {
		t.Fatalf("expected ServiceAccount to be created, got error: %v", err)
	}
}

func TestInjectAuthBridge_RespectsExistingServiceAccountName(t *testing.T) {
	m := newTestMutator(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "custom-sa",
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}
	if podSpec.ServiceAccountName != "custom-sa" {
		t.Errorf("expected ServiceAccountName to remain %q, got %q", "custom-sa", podSpec.ServiceAccountName)
	}
}

func TestInjectAuthBridge_NoSACreationWhenSpiffeHelperDisabled(t *testing.T) {
	// Spiffe-helper is injected by default for agents. SA creation is skipped
	// when spiffe-helper is explicitly opted out via its per-sidecar label.
	m := newTestMutator(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:        KagentiTypeAgent,
		LabelSpiffeHelperInject: "false", // explicitly opt out of spiffe-helper
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true (other sidecars still inject)")
	}
	if podSpec.ServiceAccountName != "" {
		t.Errorf("expected ServiceAccountName to be empty when spiffe-helper is disabled, got %q", podSpec.ServiceAccountName)
	}

	sa := &corev1.ServiceAccount{}
	err = m.Client.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-agent"}, sa)
	if err == nil {
		t.Error("expected ServiceAccount to NOT be created when spiffe-helper is disabled")
	}
}

func TestInjectAuthBridge_Tool_SkipsInjectionByDefault(t *testing.T) {
	// Tool workloads are not injected by default — the injectTools feature gate
	// is false unless explicitly enabled. No inject label needed to confirm this.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeTool,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-tool", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false: injectTools gate is false by default")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_GlobalOptOut_Agent(t *testing.T) {
	// Agent workloads are injected by default; kagenti.io/inject=disabled opts out.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeAgent,
		AuthBridgeInjectLabel: AuthBridgeDisabledValue,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false when kagenti.io/inject=disabled")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers to be injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_Tool_SkippedByGateRegardlessOfOptOut(t *testing.T) {
	// Tool workloads are blocked by the injectTools gate (false by default)
	// before the opt-out label is even evaluated.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:      KagentiTypeTool,
		AuthBridgeInjectLabel: AuthBridgeDisabledValue,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-tool", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false: tool blocked by injectTools gate")
	}
	if len(podSpec.Containers) != 0 || len(podSpec.InitContainers) != 0 {
		t.Errorf("expected no containers to be injected, got containers=%v initContainers=%v",
			podSpec.Containers, podSpec.InitContainers)
	}
}

func TestInjectAuthBridge_DefaultSAOverridden(t *testing.T) {
	m := newTestMutator(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "default",
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}
	if podSpec.ServiceAccountName != "my-agent" {
		t.Errorf("expected ServiceAccountName=%q (overriding 'default'), got %q", "my-agent", podSpec.ServiceAccountName)
	}
}

func TestInjectAuthBridge_OutboundPortsExcludeAnnotation(t *testing.T) {
	m := newTestMutator(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}
	annotations := map[string]string{
		OutboundPortsExcludeAnnotation: "11434",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	for _, ic := range podSpec.InitContainers {
		if ic.Name != ProxyInitContainerName {
			continue
		}
		for _, env := range ic.Env {
			if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
				if env.Value != "8080,11434" {
					t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434")
				}
				return
			}
		}
		t.Fatal("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
	t.Fatal("proxy-init container not found in initContainers")
}

func TestInjectAuthBridge_InboundPortsExcludeAnnotation(t *testing.T) {
	m := newTestMutator(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}
	annotations := map[string]string{
		OutboundPortsExcludeAnnotation: "11434",
		InboundPortsExcludeAnnotation:  "8443,18789",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	for _, ic := range podSpec.InitContainers {
		if ic.Name != ProxyInitContainerName {
			continue
		}
		var foundOutbound, foundInbound bool
		for _, env := range ic.Env {
			if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
				foundOutbound = true
				if env.Value != "8080,11434" {
					t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434")
				}
			}
			if env.Name == "INBOUND_PORTS_EXCLUDE" {
				foundInbound = true
				if env.Value != "8443,18789" {
					t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8443,18789")
				}
			}
		}
		if !foundOutbound {
			t.Fatal("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
		}
		if !foundInbound {
			t.Fatal("proxy-init container missing INBOUND_PORTS_EXCLUDE env var")
		}
		return
	}
	t.Fatal("proxy-init container not found in initContainers")
}

// ========================================
// Combined sidecar mode tests
// ========================================

func newTestMutatorWithCombinedSidecar(objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{
		Client:                   fakeClient,
		EnableClientRegistration: true,
		GetPlatformConfig:        config.CompiledDefaults,
		GetFeatureGates: func() *config.FeatureGates {
			fg := config.DefaultFeatureGates()
			fg.CombinedSidecar = true
			return fg
		},
	}
}

func TestInjectAuthBridge_CombinedMode_SingleContainer(t *testing.T) {
	m := newTestMutatorWithCombinedSidecar(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// Should have exactly 1 sidecar container (authbridge) — NOT envoy-proxy, spiffe-helper, or client-registration
	if !containerExists(podSpec.Containers, AuthBridgeContainerName) {
		t.Error("expected authbridge container to be injected")
	}
	if containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Error("unexpected envoy-proxy container in combined mode")
	}
	if containerExists(podSpec.Containers, SpiffeHelperContainerName) {
		t.Error("unexpected spiffe-helper container in combined mode")
	}
	if containerExists(podSpec.Containers, ClientRegistrationContainerName) {
		t.Error("unexpected client-registration container in combined mode")
	}

	// Should still have proxy-init
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("expected proxy-init init container to be injected")
	}
}

func TestInjectAuthBridge_CombinedMode_EnvoyDisabled_NoInjection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentv1alpha1.AddToScheme(scheme)
	ar := newAgentRuntime("test-ns", "my-agent")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ar).Build()
	m := &PodMutator{
		Client:                   fakeClient,
		EnableClientRegistration: true,
		GetPlatformConfig:        config.CompiledDefaults,
		GetFeatureGates: func() *config.FeatureGates {
			fg := config.DefaultFeatureGates()
			fg.CombinedSidecar = true
			fg.EnvoyProxy = false
			return fg
		},
	}
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	// With envoy-proxy disabled, the combined container should NOT be present
	if containerExists(podSpec.Containers, AuthBridgeContainerName) {
		t.Error("authbridge container should not be injected when envoy-proxy is disabled")
	}
	_ = injected
}

func TestInjectAuthBridge_CombinedMode_SpiffeDisabled_FlagPassed(t *testing.T) {
	m := newTestMutatorWithCombinedSidecar(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel:        KagentiTypeAgent,
		LabelSpiffeHelperInject: "false",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// authbridge container should be present with SPIRE_ENABLED=false
	if !containerExists(podSpec.Containers, AuthBridgeContainerName) {
		t.Fatal("expected authbridge container to be injected")
	}

	for _, c := range podSpec.Containers {
		if c.Name != AuthBridgeContainerName {
			continue
		}
		for _, env := range c.Env {
			if env.Name == "SPIRE_ENABLED" {
				if env.Value != "false" {
					t.Errorf("SPIRE_ENABLED = %q, want %q", env.Value, "false")
				}
				return
			}
		}
		t.Fatal("missing SPIRE_ENABLED env var on authbridge container")
	}
}

func TestInjectAuthBridge_CombinedMode_Idempotency(t *testing.T) {
	m := newTestMutatorWithCombinedSidecar(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: AuthBridgeContainerName, Image: "authbridge:test"},
		},
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	// Should still be exactly 1 authbridge container
	count := 0
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeContainerName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 authbridge container, got %d", count)
	}
}

func TestInjectAuthBridge_NilAnnotations(t *testing.T) {
	m := newTestMutator(newAgentRuntime("test-ns", "my-agent"))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true")
	}

	for _, ic := range podSpec.InitContainers {
		if ic.Name != ProxyInitContainerName {
			continue
		}
		for _, env := range ic.Env {
			if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
				if env.Value != "8080" {
					t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q (nil annotations should default to 8080 only)", env.Value, "8080")
				}
				return
			}
		}
		t.Fatal("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
	t.Fatal("proxy-init container not found in initContainers")
}

// ========================================
// Mode-aware injection tests
// ========================================

func TestInjectAuthBridge_WaypointMode_SkipsInjection(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest"},
		},
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}
	annotations := map[string]string{
		AnnotationAuthBridgeMode: ModeWaypoint,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mutated {
		t.Error("waypoint mode should not mutate the pod (returns false)")
	}
	if len(podSpec.Containers) != 1 {
		t.Errorf("expected 1 container (agent only), got %d", len(podSpec.Containers))
	}
}

func TestInjectAuthBridge_ProxySidecarMode_InjectsCorrectly(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest"},
		},
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}
	annotations := map[string]string{
		AnnotationAuthBridgeMode: ModeProxySidecar,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Error("proxy-sidecar mode should mutate the pod")
	}

	// Should have authbridge-proxy container
	proxyFound := false
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeProxyContainerName {
			proxyFound = true
			if c.Image != config.CompiledDefaults().Images.AuthBridgeLight {
				t.Errorf("proxy container image = %q, want authbridge-light", c.Image)
			}
		}
	}
	if !proxyFound {
		t.Error("authbridge-proxy container not found")
	}

	// Should NOT have proxy-init (no iptables in proxy-sidecar mode)
	for _, c := range podSpec.InitContainers {
		if c.Name == ProxyInitContainerName {
			t.Error("proxy-init should not be injected in proxy-sidecar mode")
		}
	}

	// Should NOT have envoy-proxy container
	for _, c := range podSpec.Containers {
		if c.Name == EnvoyProxyContainerName {
			t.Error("envoy-proxy should not be injected in proxy-sidecar mode")
		}
	}

	// Agent container should have HTTP_PROXY env vars
	for _, c := range podSpec.Containers {
		if c.Name == "agent" {
			httpProxy := ""
			httpsProxy := ""
			noProxy := ""
			for _, env := range c.Env {
				switch env.Name {
				case "HTTP_PROXY":
					httpProxy = env.Value
				case "HTTPS_PROXY":
					httpsProxy = env.Value
				case "NO_PROXY":
					noProxy = env.Value
				}
			}
			if httpProxy != "http://127.0.0.1:8081" {
				t.Errorf("HTTP_PROXY = %q, want http://127.0.0.1:8081", httpProxy)
			}
			if httpsProxy != "http://127.0.0.1:8081" {
				t.Errorf("HTTPS_PROXY = %q, want http://127.0.0.1:8081", httpsProxy)
			}
			if noProxy != "127.0.0.1,localhost" {
				t.Errorf("NO_PROXY = %q, want 127.0.0.1,localhost", noProxy)
			}
		}
	}
}

func TestInjectHTTPProxyEnv_DoesNotDuplicate(t *testing.T) {
	c := &corev1.Container{
		Name: "agent",
		Env: []corev1.EnvVar{
			{Name: "HTTP_PROXY", Value: "http://existing-proxy:3128"},
		},
	}

	injectHTTPProxyEnv(c, 8081)

	count := 0
	for _, env := range c.Env {
		if env.Name == "HTTP_PROXY" {
			count++
			if env.Value != "http://existing-proxy:3128" {
				t.Errorf("HTTP_PROXY should keep existing value, got %q", env.Value)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 HTTP_PROXY env var, got %d", count)
	}

	// HTTPS_PROXY and NO_PROXY should be added since they didn't exist
	httpsFound := false
	noProxyFound := false
	for _, env := range c.Env {
		if env.Name == "HTTPS_PROXY" {
			httpsFound = true
		}
		if env.Name == "NO_PROXY" {
			noProxyFound = true
		}
	}
	if !httpsFound {
		t.Error("HTTPS_PROXY should be added")
	}
	if !noProxyFound {
		t.Error("NO_PROXY should be added")
	}
}

func TestInjectAuthBridge_ProxySidecarMode_PortCollision(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	// Agent uses ports 8000 and 8001 — agent should move to 8002, not 8001
	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{
				Name:  "agent",
				Image: "my-agent:latest",
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: 8000},
					{Name: "grpc", ContainerPort: 8001},
				},
			},
		},
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}
	annotations := map[string]string{
		AnnotationAuthBridgeMode: ModeProxySidecar,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	// Agent's first port should be moved past 8001 to 8002
	for _, c := range podSpec.Containers {
		if c.Name == "agent" {
			if c.Ports[0].ContainerPort == 8001 {
				t.Error("agent port should not be 8001 (collision with gRPC port)")
			}
			if c.Ports[0].ContainerPort != 8002 {
				t.Errorf("agent port = %d, want 8002 (first free port after 8000)", c.Ports[0].ContainerPort)
			}
			// Second port (gRPC) should be unchanged
			if c.Ports[1].ContainerPort != 8001 {
				t.Errorf("gRPC port should remain 8001, got %d", c.Ports[1].ContainerPort)
			}
		}
	}

	// Reverse proxy should be on 8000 (original agent port)
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeProxyContainerName {
			for _, p := range c.Ports {
				if p.Name == "reverse-proxy" && p.ContainerPort != 8000 {
					t.Errorf("reverse-proxy port = %d, want 8000", p.ContainerPort)
				}
			}
		}
	}
}

func TestInjectAuthBridge_ProxySidecarMode_ForwardProxyCollision(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	// Agent uses port 8081 — forward proxy should use 8082 instead of default 8081
	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{
				Name:  "agent",
				Image: "my-agent:latest",
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: 8000},
					{Name: "metrics", ContainerPort: 8081},
				},
			},
		},
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}
	annotations := map[string]string{
		AnnotationAuthBridgeMode: ModeProxySidecar,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	// Forward proxy should NOT be on 8081 (collision with metrics)
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeProxyContainerName {
			for _, p := range c.Ports {
				if p.Name == "forward-proxy" {
					if p.ContainerPort == 8081 {
						t.Error("forward-proxy should not be 8081 (collision with agent metrics)")
					}
					if p.ContainerPort != 8082 {
						t.Errorf("forward-proxy port = %d, want 8082", p.ContainerPort)
					}
				}
			}
		}
	}

	// HTTP_PROXY should use the actual forward proxy port, not hardcoded 8081
	for _, c := range podSpec.Containers {
		if c.Name == "agent" {
			for _, env := range c.Env {
				if env.Name == "HTTP_PROXY" {
					if env.Value == "http://127.0.0.1:8081" {
						t.Error("HTTP_PROXY should not use 8081 (collides with agent metrics)")
					}
					if env.Value != "http://127.0.0.1:8082" {
						t.Errorf("HTTP_PROXY = %q, want http://127.0.0.1:8082", env.Value)
					}
				}
			}
		}
	}
}

func TestSetOrAddEnv_OverwritesExisting(t *testing.T) {
	c := &corev1.Container{
		Name: "agent",
		Env: []corev1.EnvVar{
			{Name: "PORT", Value: "8000"},
			{Name: "HOST", Value: "0.0.0.0"},
		},
	}

	setOrAddEnv(c, "PORT", "8002")

	count := 0
	for _, env := range c.Env {
		if env.Name == "PORT" {
			count++
			if env.Value != "8002" {
				t.Errorf("PORT = %q, want 8002", env.Value)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 PORT env var, got %d", count)
	}
	// HOST should be unchanged
	for _, env := range c.Env {
		if env.Name == "HOST" && env.Value != "0.0.0.0" {
			t.Errorf("HOST should be unchanged, got %q", env.Value)
		}
	}
}

func TestSetOrAddEnv_AddsNew(t *testing.T) {
	c := &corev1.Container{
		Name: "agent",
		Env: []corev1.EnvVar{
			{Name: "HOST", Value: "0.0.0.0"},
		},
	}

	setOrAddEnv(c, "PORT", "8002")

	found := false
	for _, env := range c.Env {
		if env.Name == "PORT" && env.Value == "8002" {
			found = true
		}
	}
	if !found {
		t.Error("PORT env var should be added")
	}
	if len(c.Env) != 2 {
		t.Errorf("expected 2 env vars, got %d", len(c.Env))
	}
}

func TestInjectAuthBridge_ProxySidecarMode_NoPorts_UsesDefault(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	// Agent container with no ports — should use default 8000
	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest"},
		},
	}
	labels := map[string]string{
		KagentiTypeLabel: KagentiTypeAgent,
	}
	annotations := map[string]string{
		AnnotationAuthBridgeMode: ModeProxySidecar,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	// Reverse proxy should use default port 8000
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeProxyContainerName {
			for _, p := range c.Ports {
				if p.Name == "reverse-proxy" && p.ContainerPort != 8000 {
					t.Errorf("reverse-proxy port = %d, want 8000 (default)", p.ContainerPort)
				}
			}
		}
	}

	// Agent should NOT have PORT env var patched (no ports to move)
	for _, c := range podSpec.Containers {
		if c.Name == "agent" {
			for _, env := range c.Env {
				if env.Name == "PORT" {
					t.Error("PORT env var should not be set when agent has no ports")
				}
			}
		}
	}

	// HTTP_PROXY should still be injected
	httpProxyFound := false
	for _, c := range podSpec.Containers {
		if c.Name == "agent" {
			for _, env := range c.Env {
				if env.Name == "HTTP_PROXY" {
					httpProxyFound = true
				}
			}
		}
	}
	if !httpProxyFound {
		t.Error("HTTP_PROXY should be injected even when agent has no ports")
	}
}

// --- ensurePerAgentConfigMap tests ---

// helper to get a ConfigMap from the fake client
func fetchConfigMap(t *testing.T, m *PodMutator, namespace, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := m.Client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		t.Fatalf("failed to get ConfigMap %s/%s: %v", namespace, name, err)
	}
	return cm
}

// helper to parse config.yaml from a ConfigMap into a map
func parseConfigYAML(t *testing.T, cm *corev1.ConfigMap) map[string]interface{} {
	t.Helper()
	raw, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatal("ConfigMap missing config.yaml key")
	}
	var cfg map[string]interface{}
	if err := sigsyaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("failed to parse config.yaml: %v", err)
	}
	return cfg
}

func TestEnsurePerAgentConfigMap_EmptyBaseYAML_FallbackFromNsConfig(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	nsConfig := &NamespaceConfig{
		Issuer:                "http://keycloak:8080/realms/kagenti",
		KeycloakURL:           "http://keycloak:8080",
		KeycloakRealm:         "kagenti",
		DefaultOutboundPolicy: "passthrough",
		ClientAuthType:        "client-secret",
	}

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "weather-service",
		ModeProxySidecar, "", nsConfig, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmName != "authbridge-config-weather-service" {
		t.Errorf("cmName = %q, want authbridge-config-weather-service", cmName)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	if cfg["mode"] != ModeProxySidecar {
		t.Errorf("mode = %v, want %s", cfg["mode"], ModeProxySidecar)
	}

	inbound, _ := cfg["inbound"].(map[string]interface{})
	if inbound == nil || inbound["issuer"] != "http://keycloak:8080/realms/kagenti" {
		t.Errorf("inbound.issuer = %v, want http://keycloak:8080/realms/kagenti", inbound)
	}

	outbound, _ := cfg["outbound"].(map[string]interface{})
	if outbound == nil || outbound["keycloak_url"] != "http://keycloak:8080" {
		t.Errorf("outbound.keycloak_url = %v, want http://keycloak:8080", outbound)
	}

	identity, _ := cfg["identity"].(map[string]interface{})
	if identity == nil || identity["type"] != "client-secret" {
		t.Errorf("identity.type = %v, want client-secret", identity)
	}

	if cfg["bypass"] == nil {
		t.Error("expected default bypass paths")
	}

	// managedBy label
	if cm.Labels[managedByLabel] != managedByValue {
		t.Errorf("managedBy label = %q, want %q", cm.Labels[managedByLabel], managedByValue)
	}
}

func TestEnsurePerAgentConfigMap_BaseYAML_PreservesExistingFields(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	baseYAML := `
mode: envoy-sidecar
inbound:
  issuer: "http://custom-issuer"
outbound:
  keycloak_url: "http://custom-keycloak:8080"
  keycloak_realm: "custom-realm"
identity:
  type: spiffe
  jwt_svid_path: "/opt/jwt_svid.token"
  client_id_file: "/shared/client-id.txt"
  client_secret_file: "/shared/client-secret.txt"
bypass:
  inbound_paths:
    - "/custom-path"
`

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-agent",
		ModeEnvoySidecar, baseYAML, &NamespaceConfig{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	// Mode overridden
	if cfg["mode"] != ModeEnvoySidecar {
		t.Errorf("mode = %v, want %s", cfg["mode"], ModeEnvoySidecar)
	}

	// Existing fields preserved (not overwritten by fallback)
	inbound, _ := cfg["inbound"].(map[string]interface{})
	if inbound["issuer"] != "http://custom-issuer" {
		t.Errorf("inbound.issuer = %v, should be preserved from base YAML", inbound["issuer"])
	}

	identity, _ := cfg["identity"].(map[string]interface{})
	if identity["type"] != IdentityTypeSpiffe {
		t.Errorf("identity.type = %v, should be preserved from base YAML", identity["type"])
	}

	bypass, _ := cfg["bypass"].(map[string]interface{})
	paths, _ := bypass["inbound_paths"].([]interface{})
	if len(paths) != 1 || paths[0] != "/custom-path" {
		t.Errorf("bypass paths = %v, should be preserved from base YAML", paths)
	}
}

func TestEnsurePerAgentConfigMap_ListenerOverrides_Merged(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	baseYAML := `
mode: envoy-sidecar
inbound:
  issuer: "http://issuer"
outbound:
  keycloak_url: "http://keycloak:8080"
  keycloak_realm: "kagenti"
identity:
  type: client-secret
  client_id_file: "/shared/client-id.txt"
  client_secret_file: "/shared/client-secret.txt"
`

	overrides := map[string]string{
		"reverse_proxy_addr":    ":8000",
		"reverse_proxy_backend": "http://127.0.0.1:8002",
		"forward_proxy_addr":    ":8081",
	}

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-agent",
		ModeProxySidecar, baseYAML, &NamespaceConfig{}, overrides)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	listener, _ := cfg["listener"].(map[string]interface{})
	if listener == nil {
		t.Fatal("expected listener section in config")
	}
	if listener["reverse_proxy_addr"] != ":8000" {
		t.Errorf("reverse_proxy_addr = %v, want :8000", listener["reverse_proxy_addr"])
	}
	if listener["reverse_proxy_backend"] != "http://127.0.0.1:8002" {
		t.Errorf("reverse_proxy_backend = %v, want http://127.0.0.1:8002", listener["reverse_proxy_backend"])
	}
	if listener["forward_proxy_addr"] != ":8081" {
		t.Errorf("forward_proxy_addr = %v, want :8081", listener["forward_proxy_addr"])
	}
}

func TestEnsurePerAgentConfigMap_ExistingCM_OwnedByWebhook_Updated(t *testing.T) {
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "authbridge-config-my-agent",
			Namespace: "team1",
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string]string{"config.yaml": "mode: old-mode\n"},
	}
	m := newTestMutator(existingCM)
	ctx := context.Background()

	_, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-agent",
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", "authbridge-config-my-agent")
	cfg := parseConfigYAML(t, cm)

	if cfg["mode"] != ModeEnvoySidecar {
		t.Errorf("mode = %v, want %s (should have been updated)", cfg["mode"], ModeEnvoySidecar)
	}
}

func TestEnsurePerAgentConfigMap_ExistingCM_OverwrittenBySSA(t *testing.T) {
	// Server-side apply with ForceOwnership overwrites regardless of
	// previous ownership — the webhook always converges to desired state.
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "authbridge-config-my-agent",
			Namespace: "team1",
			Labels:    map[string]string{"some-other": "label"},
		},
		Data: map[string]string{"config.yaml": "mode: user-managed\n"},
	}
	m := newTestMutator(existingCM)
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-agent",
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmName != "authbridge-config-my-agent" {
		t.Errorf("cmName = %q, want authbridge-config-my-agent", cmName)
	}

	// SSA overwrites — mode should be updated
	cm := fetchConfigMap(t, m, "team1", "authbridge-config-my-agent")
	cfg := parseConfigYAML(t, cm)
	if cfg["mode"] != ModeEnvoySidecar {
		t.Errorf("mode = %v, want %s (SSA should overwrite)", cfg["mode"], ModeEnvoySidecar)
	}
}

func TestEnsurePerAgentConfigMap_OwnerReference_SetFromDeployment(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "weather-service",
			Namespace: "team1",
			UID:       types.UID("deploy-uid-123"),
		},
	}
	m := newTestMutator(deploy)
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "weather-service",
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	if len(cm.OwnerReferences) == 0 {
		t.Fatal("expected OwnerReference on ConfigMap")
	}
	ref := cm.OwnerReferences[0]
	if ref.Kind != "Deployment" || ref.Name != "weather-service" || ref.UID != "deploy-uid-123" {
		t.Errorf("OwnerReference = %+v, want Deployment/weather-service/deploy-uid-123", ref)
	}
}

func TestEnsurePerAgentConfigMap_OwnerReference_SetFromStatefulSet(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-stateful-agent",
			Namespace: "team1",
			UID:       types.UID("sts-uid-456"),
		},
	}
	m := newTestMutator(sts)
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-stateful-agent",
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	if len(cm.OwnerReferences) == 0 {
		t.Fatal("expected OwnerReference on ConfigMap")
	}
	ref := cm.OwnerReferences[0]
	if ref.Kind != "StatefulSet" || ref.Name != "my-stateful-agent" || ref.UID != "sts-uid-456" {
		t.Errorf("OwnerReference = %+v, want StatefulSet/my-stateful-agent/sts-uid-456", ref)
	}
}

func TestEnsurePerAgentConfigMap_OwnerReference_NoWorkload_Skipped(t *testing.T) {
	// No Deployment or StatefulSet — bare pod
	m := newTestMutator()
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "bare-pod-agent",
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	if len(cm.OwnerReferences) != 0 {
		t.Errorf("expected no OwnerReference for bare pod, got %+v", cm.OwnerReferences)
	}
}

func TestEnsurePerAgentConfigMap_FederatedJWT_MapsToSpiffe(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	nsConfig := &NamespaceConfig{
		Issuer:         "http://keycloak:8080/realms/kagenti",
		KeycloakURL:    "http://keycloak:8080",
		KeycloakRealm:  "kagenti",
		ClientAuthType: "federated-jwt",
	}

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "spiffe-agent",
		ModeEnvoySidecar, "", nsConfig, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	identity, _ := cfg["identity"].(map[string]interface{})
	if identity == nil {
		t.Fatal("expected identity section")
	}
	if identity["type"] != IdentityTypeSpiffe {
		t.Errorf("identity.type = %v, want spiffe (federated-jwt should map to spiffe)", identity["type"])
	}
	if identity["jwt_svid_path"] != "/opt/jwt_svid.token" {
		t.Errorf("identity.jwt_svid_path = %v, want /opt/jwt_svid.token", identity["jwt_svid_path"])
	}
	if identity["client_id_file"] != "/shared/client-id.txt" {
		t.Errorf("identity.client_id_file = %v, want /shared/client-id.txt", identity["client_id_file"])
	}
}
