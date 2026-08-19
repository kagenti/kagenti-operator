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

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/webhook/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

func newTestMutator(objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{
		Client:            fakeClient,
		APIReader:         fakeClient,
		GetPlatformConfig: config.CompiledDefaults,
		GetFeatureGates:   config.DefaultFeatureGates,
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
	// Default mode is proxy-sidecar so the authbridge-proxy container is injected.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if !injected {
		t.Fatal("expected InjectAuthBridge to return true with defaults-only config")
	}

	// Default mode is proxy-sidecar — expect authbridge-proxy container and the
	// always-on enforce-redirect proxy-init guard; no envoy-proxy.
	if !containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Errorf("expected %s container to be injected", AuthBridgeProxyContainerName)
	}
	if containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Errorf("unexpected %s container in proxy-sidecar mode", EnvoyProxyContainerName)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Errorf("expected %s init container in proxy-sidecar mode (default enforce-redirect)", ProxyInitContainerName)
	}
}

func TestInjectAuthBridge_SetsServiceAccountName(t *testing.T) {
	// Opt-out model: agent workloads are injected by default (no inject label needed).
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
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
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "custom-sa",
	}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
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
	// MTLSMode must be set to "disabled" because the default (permissive) would
	// auto-enable SPIRE, creating a ServiceAccount regardless of the spiffe-helper label.
	// Set via namespace ConfigMap since AR overrides are removed.
	runtimeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeRuntimeConfigMapName, Namespace: "test-ns"},
		Data:       map[string]string{"config.yaml": "mtls:\n  mode: disabled"},
	}
	m := newTestMutator(runtimeCM)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		RossoctlTypeLabel:       RossoctlTypeAgent,
		LabelSpiffeHelperInject: "false", // explicitly opt out of spiffe-helper
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
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
		RossoctlTypeLabel: RossoctlTypeTool,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-tool", "Deployment", labels, nil)
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
	// Agent workloads are injected by default; rossoctl.io/inject=disabled opts out.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		RossoctlTypeLabel:     RossoctlTypeAgent,
		AuthBridgeInjectLabel: AuthBridgeDisabledValue,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("InjectAuthBridge() returned error: %v", err)
	}
	if injected {
		t.Fatal("expected InjectAuthBridge to return false when rossoctl.io/inject=disabled")
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
		RossoctlTypeLabel:     RossoctlTypeTool,
		AuthBridgeInjectLabel: AuthBridgeDisabledValue,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-tool", "Deployment", labels, nil)
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
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "default",
	}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
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
	// proxy-init is only injected in envoy-sidecar mode.
	m := newTestMutator(authbridgeRuntimeConfigMap("test-ns", ModeEnvoySidecar))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}
	annotations := map[string]string{
		OutboundPortsExcludeAnnotation: "11434",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, annotations)
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
	// proxy-init is only injected in envoy-sidecar mode.
	m := newTestMutator(authbridgeRuntimeConfigMap("test-ns", ModeEnvoySidecar))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}
	annotations := map[string]string{
		OutboundPortsExcludeAnnotation: "11434",
		InboundPortsExcludeAnnotation:  "8443,18789",
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, annotations)
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

func TestInjectAuthBridge_NilAnnotations(t *testing.T) {
	// proxy-init is only injected in envoy-sidecar mode.
	m := newTestMutator(authbridgeRuntimeConfigMap("test-ns", ModeEnvoySidecar))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	injected, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
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

// authbridgeRuntimeConfigMap returns a fake authbridge-runtime-config
// ConfigMap pinning the given mode. Used by mode-resolution tests that
// exercise the namespace-config layer of the chain.
func authbridgeRuntimeConfigMap(namespace, mode string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AuthBridgeRuntimeConfigMapName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"config.yaml": "mode: " + mode + "\n",
		},
	}
}

// Mode resolution chain (first non-empty wins):
//   1. namespace authbridge-runtime-config mode field
//   2. rossoctl.io/authbridge-mode annotation (deprecated)
//   3. ModeProxySidecar (cluster default)

func TestInjectAuthBridge_ModeResolution_NamespaceConfigMap(t *testing.T) {
	// Namespace ConfigMap pins envoy-sidecar.
	m := newTestMutator(
		authbridgeRuntimeConfigMap("team1", ModeEnvoySidecar),
	)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	// envoy-sidecar shape: envoy-proxy + proxy-init, no authbridge-proxy
	if !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Errorf("expected %s container (namespace ConfigMap selected envoy-sidecar)", EnvoyProxyContainerName)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Errorf("expected %s init container", ProxyInitContainerName)
	}
	if containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Error("unexpected authbridge-proxy container in envoy-sidecar mode")
	}
}

func TestInjectAuthBridge_ModeResolution_NamespaceConfigMapWinsOverCR(t *testing.T) {
	// With AgentRuntime overrides removed, the namespace ConfigMap is
	// the highest-priority mode source. Verify envoy-sidecar is selected.
	m := newTestMutator(
		authbridgeRuntimeConfigMap("team1", ModeEnvoySidecar),
	)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers:         []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	if !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Errorf("expected %s container (namespace ConfigMap wins)", EnvoyProxyContainerName)
	}
	if containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Error("unexpected authbridge-proxy container — namespace ConfigMap selected envoy-sidecar")
	}
}

func TestInjectAuthBridge_ModeResolution_DeprecatedAnnotation(t *testing.T) {
	// No namespace ConfigMap; deprecated annotation pins envoy-sidecar.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}
	annotations := map[string]string{AnnotationAuthBridgeMode: ModeEnvoySidecar}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	if !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Errorf("expected %s container (annotation fallback selected envoy-sidecar)", EnvoyProxyContainerName)
	}
}

func TestInjectAuthBridge_ModeResolution_AnnotationWinsOverCR(t *testing.T) {
	// With AgentRuntime overrides removed, the annotation is a valid
	// mode source. Verify envoy-sidecar is selected from the annotation.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers:         []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}
	annotations := map[string]string{AnnotationAuthBridgeMode: ModeEnvoySidecar}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	if !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Errorf("expected %s container (annotation wins)", EnvoyProxyContainerName)
	}
	if containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Error("unexpected authbridge-proxy container — annotation selected envoy-sidecar")
	}
}

func TestInjectAuthBridge_ModeResolution_ClusterDefault(t *testing.T) {
	// No namespace ConfigMap, no annotation — expect proxy-sidecar default.
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers:         []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	if !containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Errorf("expected %s container (cluster default is proxy-sidecar)", AuthBridgeProxyContainerName)
	}
	if containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Error("unexpected envoy-proxy container under default fallback")
	}
}

func TestInjectAuthBridge_LiteMode_UsesAuthBridgeLiteImage(t *testing.T) {
	// Lite mode is structurally proxy-sidecar but uses Images.AuthBridgeLite.
	m := newTestMutator(authbridgeRuntimeConfigMap("team1", ModeLite))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers:         []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	// Same shape as proxy-sidecar: authbridge-proxy container, no envoy-proxy.
	if !containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Errorf("expected %s container (lite mode uses proxy-sidecar shape)", AuthBridgeProxyContainerName)
	}
	if containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Error("unexpected envoy-proxy container in lite mode")
	}

	// But the image must be AuthBridgeLite, not AuthBridge.
	wantImage := config.CompiledDefaults().Images.AuthBridgeLite
	gotImage := ""
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeProxyContainerName {
			gotImage = c.Image
			break
		}
	}
	if gotImage != wantImage {
		t.Errorf("authbridge-proxy image = %q, want %q (Images.AuthBridgeLite)", gotImage, wantImage)
	}
}

func TestInjectAuthBridge_LiteMode_FromNamespaceConfigMap(t *testing.T) {
	// Namespace ConfigMap pins lite.
	m := newTestMutator(
		authbridgeRuntimeConfigMap("team1", ModeLite),
	)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers:         []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	wantImage := config.CompiledDefaults().Images.AuthBridgeLite
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeProxyContainerName && c.Image != wantImage {
			t.Errorf("namespace ConfigMap selected lite but image = %q, want %q", c.Image, wantImage)
		}
	}
}

func TestInjectAuthBridge_ModeResolution_UnrecognizedFallsBackToProxySidecar(t *testing.T) {
	// A typo in the namespace ConfigMap (e.g. "proxy-sidecart") should
	// not silently flow through to the envoy-sidecar branch. The
	// resolution chain validates the resolved value and falls back to
	// proxy-sidecar with a WARN log.
	m := newTestMutator(
		authbridgeRuntimeConfigMap("team1", "proxy-sidecart"),
	)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers:         []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation despite unrecognized mode")
	}

	// Should land on proxy-sidecar (the safe fallback), not envoy-sidecar.
	if !containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Errorf("expected %s container (typo should fall back to proxy-sidecar)", AuthBridgeProxyContainerName)
	}
	if containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		t.Error("unexpected envoy-proxy container — typo should not silently route to envoy-sidecar")
	}
}

func TestInjectAuthBridge_WaypointMode_SkipsInjection(t *testing.T) {
	m := newTestMutator(authbridgeRuntimeConfigMap("team1", ModeWaypoint))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest"},
		},
	}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
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

// Egress enforcement is always-on for proxy-sidecar: a proxy-init container is
// always injected in enforce-redirect mode; envoy-sidecar is unaffected (it
// uses redirect mode, tested elsewhere).
func TestInjectAuthBridge_ProxySidecar_EgressEnforcement(t *testing.T) {
	ctx := context.Background()
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}
	makePod := func() *corev1.PodSpec {
		return &corev1.PodSpec{
			ServiceAccountName: "my-agent",
			Containers:         []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
		}
	}
	findProxyInit := func(spec *corev1.PodSpec) *corev1.Container {
		for i := range spec.InitContainers {
			if spec.InitContainers[i].Name == ProxyInitContainerName {
				return &spec.InitContainers[i]
			}
		}
		return nil
	}

	t.Run("always injects proxy-init in enforce-redirect mode", func(t *testing.T) {
		m := newTestMutator()
		spec := makePod()
		if _, err := m.InjectAuthBridge(ctx, spec, "team1", "my-agent", "Deployment", labels, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ic := findProxyInit(spec)
		if ic == nil {
			t.Fatal("proxy-init should always be injected for proxy-sidecar")
		}
		var mode, transparentPort string
		for _, e := range ic.Env {
			switch e.Name {
			case "MODE":
				mode = e.Value
			case "TRANSPARENT_PORT":
				transparentPort = e.Value
			}
		}
		if mode != "enforce-redirect" {
			t.Errorf("proxy-init MODE = %q, want enforce-redirect", mode)
		}
		if transparentPort == "" {
			t.Error("enforce-redirect must set TRANSPARENT_PORT")
		}
	})

	t.Run("does not duplicate an existing proxy-init", func(t *testing.T) {
		m := newTestMutator()
		spec := makePod()
		spec.InitContainers = []corev1.Container{{Name: ProxyInitContainerName, Image: "preexisting"}}
		if _, err := m.InjectAuthBridge(ctx, spec, "team1", "my-agent", "Deployment", labels, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		count := 0
		for _, c := range spec.InitContainers {
			if c.Name == ProxyInitContainerName {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected proxy-init not duplicated, got %d", count)
		}
	})
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
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
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
			if c.Image != config.CompiledDefaults().Images.AuthBridge {
				t.Errorf("proxy container image = %q, want %q", c.Image, config.CompiledDefaults().Images.AuthBridge)
			}
		}
	}
	if !proxyFound {
		t.Error("authbridge-proxy container not found")
	}

	// Should have the always-on enforce-redirect proxy-init guard.
	proxyInitFound := false
	for _, c := range podSpec.InitContainers {
		if c.Name == ProxyInitContainerName {
			proxyInitFound = true
		}
	}
	if !proxyInitFound {
		t.Error("proxy-init (enforce-redirect) should be injected in proxy-sidecar mode")
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

func TestInjectAuthBridge_ProxySidecarMode_MountsKeycloakCredentials(t *testing.T) {
	// Regression: the proxy-sidecar branch used to return before reaching
	// ApplyKeycloakClientCredentialsSecretVolumes. That left authbridge-proxy polling
	// /shared/client-id.txt forever and returning 503 "identity not yet configured".
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest"},
		},
	}
	labels := map[string]string{
		RossoctlTypeLabel: RossoctlTypeAgent,
	}
	annotations := map[string]string{
		AnnotationKeycloakClientSecretName: "rossoctl-keycloak-client-credentials-abc12345",
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, annotations)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("proxy-sidecar mode should mutate the pod")
	}

	// The Secret volume must be declared so kubelet can resolve it.
	volFound := false
	for _, v := range podSpec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "rossoctl-keycloak-client-credentials-abc12345" {
			volFound = true
			break
		}
	}
	if !volFound {
		t.Error("expected a Secret volume for operator-managed Keycloak client credentials; got none")
	}

	// The authbridge-proxy container must mount client-id.txt and client-secret.txt
	// via subPath so its plugins can read them.
	var proxyMounts []corev1.VolumeMount
	for _, c := range podSpec.Containers {
		if c.Name == AuthBridgeProxyContainerName {
			proxyMounts = c.VolumeMounts
			break
		}
	}
	haveIDMount, haveSecretMount := false, false
	for _, m := range proxyMounts {
		if m.MountPath == "/shared/client-id.txt" && m.SubPath == "client-id.txt" {
			haveIDMount = true
		}
		if m.MountPath == "/shared/client-secret.txt" && m.SubPath == "client-secret.txt" {
			haveSecretMount = true
		}
	}
	if !haveIDMount || !haveSecretMount {
		t.Errorf("authbridge-proxy missing Keycloak credential subPath mounts: id=%v secret=%v",
			haveIDMount, haveSecretMount)
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
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
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
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
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
					// 8084, not 8082: the sidecar's own ports are now reserved, so
					// findFreePort skips the transparent egress listener (8082) and
					// the transparent inbound listener (8083). This expectation used
					// to be 8082, which would have put the forward proxy on top of a
					// listener that is always on in proxy-sidecar mode.
					if p.ContainerPort != 8084 {
						t.Errorf("forward-proxy port = %d, want 8084", p.ContainerPort)
					}
					for _, owned := range []int32{8082, 8083, 9091, 9093, 9094} {
						if p.ContainerPort == owned {
							t.Errorf("forward-proxy assigned %d, a port the sidecar binds", owned)
						}
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
					if env.Value != "http://127.0.0.1:8084" {
						t.Errorf("HTTP_PROXY = %q, want http://127.0.0.1:8084", env.Value)
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
		RossoctlTypeLabel: RossoctlTypeAgent,
	}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
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
		Issuer:                "http://keycloak:8080/realms/rossoctl",
		KeycloakURL:           "http://keycloak:8080",
		KeycloakRealm:         "rossoctl",
		DefaultOutboundPolicy: "passthrough",
		ClientAuthType:        "client-secret",
	}

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "weather-service",
		ModeProxySidecar, "", nsConfig, nil, "", "", false, nil)
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

	// Synthesized pipeline: jwt-validation inbound, token-exchange
	// outbound. Plugin-level defaults (audience_file, bypass_paths,
	// identity file paths) are not emitted by the webhook — the
	// authbridge binary applies them from its own convention layer
	// when it reads this config. See
	// authbridge/authlib/plugins/CONVENTIONS.md.
	jwtCfg := pluginConfigAt(t, cfg, "inbound", "jwt-validation")
	if got, want := jwtCfg["issuer"], "http://keycloak:8080/realms/rossoctl"; got != want {
		t.Errorf("jwt-validation.config.issuer = %v, want %v", got, want)
	}
	// keycloak_url + keycloak_realm are passed to jwt-validation so the
	// plugin derives jwks_url from the internal URL. Required for
	// split-horizon deployments where `issuer` (public) isn't reachable
	// from inside the pod. See cortex#383.
	if got, want := jwtCfg["keycloak_url"], "http://keycloak:8080"; got != want {
		t.Errorf("jwt-validation.config.keycloak_url = %v, want %v", got, want)
	}
	if got, want := jwtCfg["keycloak_realm"], "rossoctl"; got != want {
		t.Errorf("jwt-validation.config.keycloak_realm = %v, want %v", got, want)
	}

	tokCfg := pluginConfigAt(t, cfg, "outbound", "token-exchange")
	if got, want := tokCfg["keycloak_url"], "http://keycloak:8080"; got != want {
		t.Errorf("token-exchange.config.keycloak_url = %v, want %v", got, want)
	}
	if got, want := tokCfg["keycloak_realm"], "rossoctl"; got != want {
		t.Errorf("token-exchange.config.keycloak_realm = %v, want %v", got, want)
	}
	if got, want := tokCfg["default_policy"], "passthrough"; got != want {
		t.Errorf("token-exchange.config.default_policy = %v, want %v", got, want)
	}
	identity, _ := tokCfg["identity"].(map[string]interface{})
	if identity == nil || identity["type"] != "client-secret" {
		t.Errorf("token-exchange.config.identity.type = %v, want client-secret", identity)
	}

	// managedBy label
	if cm.Labels[managedByLabel] != managedByValue {
		t.Errorf("managedBy label = %q, want %q", cm.Labels[managedByLabel], managedByValue)
	}
}

// pluginConfigAt navigates pipeline.<direction>.plugins[<name>].config
// and returns the config map. Fails the test if the path is missing
// or the shape is unexpected. Keeps assertions in tests compact.
func pluginConfigAt(t *testing.T, cfg map[string]interface{}, direction, pluginName string) map[string]interface{} {
	t.Helper()
	pipeline, ok := cfg["pipeline"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected pipeline section, got %v", cfg["pipeline"])
	}
	dir, ok := pipeline[direction].(map[string]interface{})
	if !ok {
		t.Fatalf("expected pipeline.%s section", direction)
	}
	plugins, ok := dir["plugins"].([]interface{})
	if !ok || len(plugins) == 0 {
		t.Fatalf("expected pipeline.%s.plugins list, got %v", direction, dir["plugins"])
	}
	for _, raw := range plugins {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if entry["name"] == pluginName {
			cfg, _ := entry["config"].(map[string]interface{})
			return cfg
		}
	}
	t.Fatalf("plugin %q not found under pipeline.%s.plugins", pluginName, direction)
	return nil
}

func TestEnsurePerAgentConfigMap_BaseYAML_PreservesExistingFields(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	// baseYAML uses the per-plugin schema the Rossoctl Helm chart
	// emits post-migration. When pipeline: is already present, the
	// webhook must not touch plugin config — only mode + listener
	// overrides layer on top.
	baseYAML := `
mode: envoy-sidecar
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
        config:
          issuer: "http://custom-issuer"
          bypass_paths:
            - "/custom-path"
  outbound:
    plugins:
      - name: token-exchange
        config:
          keycloak_url: "http://custom-keycloak:8080"
          keycloak_realm: "custom-realm"
          identity:
            type: spiffe
`

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-agent",
		ModeEnvoySidecar, baseYAML, &NamespaceConfig{}, nil, "", "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	// Mode overridden
	if cfg["mode"] != ModeEnvoySidecar {
		t.Errorf("mode = %v, want %s", cfg["mode"], ModeEnvoySidecar)
	}

	// Existing plugin config preserved (not overwritten by fallback)
	jwtCfg := pluginConfigAt(t, cfg, "inbound", "jwt-validation")
	if jwtCfg["issuer"] != "http://custom-issuer" {
		t.Errorf("jwt-validation.config.issuer = %v, should be preserved from base YAML", jwtCfg["issuer"])
	}
	paths, _ := jwtCfg["bypass_paths"].([]interface{})
	if len(paths) != 1 || paths[0] != "/custom-path" {
		t.Errorf("bypass_paths = %v, should be preserved from base YAML", paths)
	}

	tokCfg := pluginConfigAt(t, cfg, "outbound", "token-exchange")
	identity, _ := tokCfg["identity"].(map[string]interface{})
	if identity["type"] != IdentityTypeSpiffe {
		t.Errorf("token-exchange.config.identity.type = %v, should be preserved from base YAML", identity["type"])
	}
}

func TestEnsurePerAgentConfigMap_ListenerOverrides_Merged(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	baseYAML := `
mode: envoy-sidecar
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
        config:
          issuer: "http://issuer"
  outbound:
    plugins:
      - name: token-exchange
        config:
          keycloak_url: "http://keycloak:8080"
          keycloak_realm: "rossoctl"
          identity:
            type: client-secret
`

	overrides := map[string]string{
		"reverse_proxy_addr":    ":8000",
		"reverse_proxy_backend": "http://127.0.0.1:8002",
		"forward_proxy_addr":    ":8081",
	}

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-agent",
		ModeProxySidecar, baseYAML, &NamespaceConfig{}, overrides, "", "", false, nil)
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
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, "", "", false, nil)
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
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, "", "", false, nil)
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
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, "", "", false, nil)
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
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, "", "", false, nil)
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

func TestEnsurePerAgentConfigMap_OwnerReference_SetFromSandbox(t *testing.T) {
	// Sandbox is an agents.x-k8s.io CR (unstructured). The per-agent ConfigMap
	// should be owned by it so it's garbage-collected with the Sandbox, matching
	// the Deployment/StatefulSet behavior.
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = agentv1alpha1.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(sandboxOwnerGVK, &unstructured.Unstructured{})

	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxOwnerGVK)
	sandbox.SetNamespace("team1")
	sandbox.SetName("my-sandbox-agent")
	sandbox.SetUID(types.UID("sandbox-uid-789"))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	m := &PodMutator{
		Client:            fakeClient,
		APIReader:         fakeClient,
		GetPlatformConfig: config.CompiledDefaults,
		GetFeatureGates:   config.DefaultFeatureGates,
	}
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-sandbox-agent",
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, "", "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	if len(cm.OwnerReferences) == 0 {
		t.Fatal("expected OwnerReference on ConfigMap")
	}
	ref := cm.OwnerReferences[0]
	if ref.Kind != "Sandbox" || ref.Name != "my-sandbox-agent" || ref.UID != "sandbox-uid-789" {
		t.Errorf("OwnerReference = %+v, want Sandbox/my-sandbox-agent/sandbox-uid-789", ref)
	}
}

func TestEnsurePerAgentConfigMap_OwnerReference_NoWorkload_Skipped(t *testing.T) {
	// No Deployment or StatefulSet — bare pod
	m := newTestMutator()
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "bare-pod-agent",
		ModeEnvoySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, "", "", false, nil)
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
		Issuer:         "http://keycloak:8080/realms/rossoctl",
		KeycloakURL:    "http://keycloak:8080",
		KeycloakRealm:  "rossoctl",
		ClientAuthType: "federated-jwt",
	}

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "spiffe-agent",
		ModeEnvoySidecar, "", nsConfig, nil, "", "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	tokCfg := pluginConfigAt(t, cfg, "outbound", "token-exchange")
	identity, _ := tokCfg["identity"].(map[string]interface{})
	if identity == nil {
		t.Fatal("expected identity block under token-exchange config")
	}
	if identity["type"] != IdentityTypeSpiffe {
		t.Errorf("identity.type = %v, want spiffe (federated-jwt should map to spiffe)", identity["type"])
	}
	// Note: the webhook no longer emits default credential file
	// paths (client_id_file, client_secret_file, jwt_svid_path).
	// The authbridge plugin applies those defaults itself from its
	// own convention layer — keeping the webhook schema-agnostic
	// about file paths. See
	// authbridge/authlib/plugins/CONVENTIONS.md.
}

func TestEnsurePerAgentConfigMap_FederatedJWT_SetsJWTAudience(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	nsConfig := &NamespaceConfig{
		Issuer:         "http://keycloak:8080/realms/rossoctl",
		KeycloakURL:    "http://keycloak:8080",
		KeycloakRealm:  "rossoctl",
		ClientAuthType: "federated-jwt",
		JWTAudience:    "http://keycloak:8080/realms/rossoctl",
	}

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "spiffe-agent",
		ModeEnvoySidecar, "", nsConfig, nil, "", "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	tokCfg := pluginConfigAt(t, cfg, "outbound", "token-exchange")
	identity, _ := tokCfg["identity"].(map[string]interface{})
	if identity == nil {
		t.Fatal("expected identity block under token-exchange config")
	}
	if identity["jwt_audience"] != "http://keycloak:8080/realms/rossoctl" {
		t.Errorf("identity.jwt_audience = %v, want http://keycloak:8080/realms/rossoctl", identity["jwt_audience"])
	}
}

func TestEnsurePerAgentConfigMap_SpireEnabled_InjectsSpiffeBlock(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "spiffe-agent",
		ModeProxySidecar, "", &NamespaceConfig{}, nil, "", "", true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	spiffe, ok := cfg["spiffe"].(map[string]interface{})
	if !ok || spiffe == nil {
		t.Fatal("expected spiffe block when spireEnabled=true")
	}
	if spiffe["socket"] != "unix:///spiffe-workload-api/spire-agent.sock" {
		t.Errorf("spiffe.socket = %v, want default socket path", spiffe["socket"])
	}
}

func TestEnsurePerAgentConfigMap_SpireDisabled_NoSpiffeBlock(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "no-spiffe-agent",
		ModeProxySidecar, "", &NamespaceConfig{}, nil, "", "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	if _, ok := cfg["spiffe"]; ok {
		t.Fatal("expected no spiffe block when spireEnabled=false")
	}
}

// --- mTLS rendering tests ---
//
// These cover the per-agent ConfigMap rendering with the new mtlsMode
// argument. The validating webhook upstream rejects mtlsMode != disabled
// with envoy-sidecar mode, so the renderer doesn't need to gate by mode
// — but we still test the negative ("disabled" / "" should not emit a
// block) and the scrub case (toggling back to disabled wipes a stale
// block from the base YAML).

// TestEnsurePerAgentConfigMap_MTLSStrict_RendersBlock verifies that
// mtlsMode=strict produces a top-level mtls: {mode: strict} block.
// Cert paths are intentionally NOT emitted — they default to the
// authbridge-side defaults (/opt/svid*.pem) written by spiffe-helper.
func TestEnsurePerAgentConfigMap_MTLSStrict_RendersBlock(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "mtls-agent",
		ModeProxySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, MTLSModeStrict, "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	mtls, ok := cfg["mtls"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected mtls block to be a map; got %T (cfg=%+v)", cfg["mtls"], cfg)
	}
	if mtls["mode"] != MTLSModeStrict {
		t.Errorf("mtls.mode = %v, want %s", mtls["mode"], MTLSModeStrict)
	}
	// Cert paths are NOT rendered — operator stays decoupled from
	// authbridge's internal layout.
	for _, key := range []string{"cert_file", "key_file", "bundle_file"} {
		if _, present := mtls[key]; present {
			t.Errorf("mtls.%s should not be emitted (authbridge supplies defaults)", key)
		}
	}
}

// TestEnsurePerAgentConfigMap_MTLSPermissive_RendersBlock mirrors the
// strict test for permissive mode — same shape, different mode value.
func TestEnsurePerAgentConfigMap_MTLSPermissive_RendersBlock(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "mtls-agent",
		ModeProxySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, MTLSModePermissive, "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	mtls, ok := cfg["mtls"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected mtls block to be a map; got %T", cfg["mtls"])
	}
	if mtls["mode"] != MTLSModePermissive {
		t.Errorf("mtls.mode = %v, want %s", mtls["mode"], MTLSModePermissive)
	}
}

// TestEnsurePerAgentConfigMap_MTLSDisabled_OmitsBlock verifies that the
// renderer does NOT emit mtls when mtlsMode is disabled or empty.
// Empty-string is the envoy-sidecar carve-out path — the call site
// passes "" explicitly so we test that too.
func TestEnsurePerAgentConfigMap_MTLSDisabled_OmitsBlock(t *testing.T) {
	tests := []struct {
		name     string
		mtlsMode string
	}{
		{"empty string", ""},
		{"disabled", MTLSModeDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestMutator()
			ctx := context.Background()

			cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "no-mtls-"+tt.name,
				ModeProxySidecar, "", &NamespaceConfig{ClientAuthType: "client-secret"}, nil, tt.mtlsMode, "", false, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			cm := fetchConfigMap(t, m, "team1", cmName)
			cfg := parseConfigYAML(t, cm)

			if _, present := cfg["mtls"]; present {
				t.Errorf("mtls block should not be emitted when mtlsMode=%q (cfg=%+v)", tt.mtlsMode, cfg)
			}
		})
	}
}

// TestEnsurePerAgentConfigMap_MTLSScrubsStaleBlock guards against a
// regression where toggling mtlsMode from strict back to disabled would
// leak the previous mtls block through to the per-agent CM. The
// renderer must explicitly delete cfg["mtls"] when mode is off.
func TestEnsurePerAgentConfigMap_MTLSScrubsStaleBlock(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	// Base YAML with a stale mtls: strict — simulates a namespace
	// ConfigMap that was rendered earlier with mtls on.
	baseYAML := "mode: proxy-sidecar\nmtls:\n  mode: strict\n"

	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "scrub-agent",
		ModeProxySidecar, baseYAML, &NamespaceConfig{ClientAuthType: "client-secret"}, nil, MTLSModeDisabled, "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm := fetchConfigMap(t, m, "team1", cmName)
	cfg := parseConfigYAML(t, cm)

	if _, present := cfg["mtls"]; present {
		t.Errorf("stale mtls block should be scrubbed when mtlsMode=disabled; got cfg=%+v", cfg)
	}
}

// ========================================
// EgressEnforcement tests
// ========================================

func egressCM(mode, ee, mtls string) *corev1.ConfigMap {
	yaml := "mode: " + mode + "\n"
	if ee != "" {
		yaml += "egressEnforcement: " + ee + "\n"
	}
	if mtls != "" {
		yaml += "mtls:\n  mode: " + mtls + "\n"
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeRuntimeConfigMapName, Namespace: "test-ns"},
		Data:       map[string]string{"config.yaml": yaml},
	}
}

func TestInjectAuthBridge_EgressEnforcement_DefaultInjectsProxyInit(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("expected proxy-init when egressEnforcement is unset (default enforce-redirect)")
	}
}

func TestInjectAuthBridge_EgressEnforcement_NoneSkipsProxyInit(t *testing.T) {
	m := newTestMutator(egressCM(ModeProxySidecar, EgressEnforcementNone, MTLSModeDisabled))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("proxy-init should NOT be injected when egressEnforcement=none")
	}
	if !containerExists(podSpec.Containers, AuthBridgeProxyContainerName) {
		t.Error("authbridge-proxy should still be injected when egressEnforcement=none")
	}
}

func TestInjectAuthBridge_EgressEnforcement_EnforceRedirectInjectsProxyInit(t *testing.T) {
	m := newTestMutator(egressCM(ModeProxySidecar, EgressEnforcementEnforceRedirect, MTLSModeDisabled))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("expected proxy-init when egressEnforcement=enforce-redirect")
	}
}

func TestInjectAuthBridge_EgressEnforcement_NamespaceConfigMapNone(t *testing.T) {
	m := newTestMutator(egressCM(ModeProxySidecar, EgressEnforcementNone, MTLSModeDisabled))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("proxy-init should NOT be injected when namespace ConfigMap sets egressEnforcement=none")
	}
}

func TestInjectAuthBridge_EgressEnforcement_UnknownValueFailsClosed(t *testing.T) {
	m := newTestMutator(egressCM(ModeProxySidecar, "typo-value", MTLSModeDisabled))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("unknown egressEnforcement value should fail closed (inject proxy-init)")
	}
}

func TestInjectAuthBridge_EgressEnforcement_EnvoySidecarIgnoresNone(t *testing.T) {
	m := newTestMutator(egressCM(ModeEnvoySidecar, EgressEnforcementNone, MTLSModeDisabled))
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("envoy-sidecar mode should inject proxy-init regardless of egressEnforcement=none")
	}
}

func newTestMutatorWithAllowedEgress(allowed []string, objs ...client.Object) *PodMutator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodMutator{
		Client:    fakeClient,
		APIReader: fakeClient,
		GetPlatformConfig: func() *config.PlatformConfig {
			cfg := config.CompiledDefaults()
			cfg.Proxy.AllowedEgressEnforcement = allowed
			return cfg
		},
		GetFeatureGates: config.DefaultFeatureGates,
	}
}

func TestInjectAuthBridge_EgressEnforcement_PlatformPolicyBlocksNone(t *testing.T) {
	cm := egressCM(ModeProxySidecar, EgressEnforcementNone, MTLSModeDisabled)
	m := newTestMutatorWithAllowedEgress([]string{EgressEnforcementEnforceRedirect}, cm)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("platform policy allows only enforce-redirect; proxy-init should be injected despite namespace requesting none")
	}
}

func TestInjectAuthBridge_EgressEnforcement_PlatformPolicyAllowsNone(t *testing.T) {
	cm := egressCM(ModeProxySidecar, EgressEnforcementNone, MTLSModeDisabled)
	m := newTestMutatorWithAllowedEgress([]string{EgressEnforcementEnforceRedirect, EgressEnforcementNone}, cm)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("platform allows none; namespace requests none; proxy-init should NOT be injected")
	}
}

func TestInjectAuthBridge_EgressEnforcement_PlatformPolicyOnlyNone(t *testing.T) {
	m := newTestMutatorWithAllowedEgress([]string{EgressEnforcementNone})
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent", Image: "my-agent:latest"}},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	_, err := m.InjectAuthBridge(ctx, podSpec, "test-ns", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		t.Error("platform only allows none; proxy-init should NOT be injected even with default enforce-redirect")
	}
}

func TestEnsurePerAgentConfigMap_TLSBridgeBlock(t *testing.T) {
	m := newTestMutator()
	ctx := context.Background()

	// enabled => tls_bridge: {mode: enabled, ca_dir: <mount>}
	cmName, err := m.ensurePerAgentConfigMap(ctx, "team1", "bridge-agent",
		ModeProxySidecar, "", &NamespaceConfig{}, nil, "", "enabled", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := parseConfigYAML(t, fetchConfigMap(t, m, "team1", cmName))
	tb, ok := cfg["tls_bridge"].(map[string]interface{})
	if !ok {
		t.Fatalf("tls_bridge block missing or wrong type: %v", cfg["tls_bridge"])
	}
	if tb["mode"] != "enabled" {
		t.Errorf("tls_bridge.mode = %v, want enabled", tb["mode"])
	}
	if tb["ca_dir"] != TLSBridgeCAMountPath {
		t.Errorf("tls_bridge.ca_dir = %v, want %s", tb["ca_dir"], TLSBridgeCAMountPath)
	}

	// disabled ("") => no tls_bridge block
	cmName2, err := m.ensurePerAgentConfigMap(ctx, "team1", "no-bridge",
		ModeProxySidecar, "", &NamespaceConfig{}, nil, "", "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg2 := parseConfigYAML(t, fetchConfigMap(t, m, "team1", cmName2))
	if _, present := cfg2["tls_bridge"]; present {
		t.Error("tls_bridge block should be absent when disabled")
	}
}

// findVolume returns the named volume from the pod spec, or nil.
func findVolume(podSpec *corev1.PodSpec, name string) *corev1.Volume {
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == name {
			return &podSpec.Volumes[i]
		}
	}
	return nil
}

func TestInjectAuthBridge_TLSBridge_Enabled_MountsCA(t *testing.T) {
	// tlsBridgeMode=enabled in proxy-sidecar mode → the FULL keypair Secret is
	// mounted into the sidecar only; the agent gets a ca.crt-only volume + trust
	// env. No cluster feature gate is involved (per-agent field only, like mtls).
	runtimeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeRuntimeConfigMapName, Namespace: "team1"},
		Data:       map[string]string{"config.yaml": "mode: proxy-sidecar\ntls_bridge:\n  mode: enabled\nmtls:\n  mode: disabled\n"},
	}
	m := newTestMutator(runtimeCM)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8000}}},
		},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	mutated, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected pod to be mutated")
	}

	// Volume: Secret-backed, named after the workload, hard mount, key mode 0440.
	vol := findVolume(podSpec, TLSBridgeCAVolumeName)
	if vol == nil {
		t.Fatalf("expected %q volume to be injected", TLSBridgeCAVolumeName)
	}
	if vol.Secret == nil {
		t.Fatalf("%q volume must be Secret-backed", TLSBridgeCAVolumeName)
	}
	if vol.Secret.SecretName != "my-agent"+agentv1alpha1.TLSBridgeCASecretSuffix {
		t.Errorf("secretName = %q, want %q", vol.Secret.SecretName, "my-agent"+agentv1alpha1.TLSBridgeCASecretSuffix)
	}
	if vol.Secret.Optional != nil && *vol.Secret.Optional {
		t.Error("CA volume must be a HARD mount (Optional unset/false) to gate pod start")
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o444 {
		t.Errorf("keypair DefaultMode = %v, want 0444", vol.Secret.DefaultMode)
	}
	if len(vol.Secret.Items) != 0 {
		t.Errorf("keypair volume must project the full Secret (no Items), got %v", vol.Secret.Items)
	}

	// (fsGroup may be set here by the SPIRE path, which is on by default in this
	// test; the bridge's own no-fsGroup behavior is covered by the SPIRE-off test.)

	// ca.crt-only volume: same Secret, projects ONLY ca.crt (no private key).
	caCert := findVolume(podSpec, TLSBridgeCACertVolumeName)
	if caCert == nil || caCert.Secret == nil {
		t.Fatalf("expected Secret-backed %q volume", TLSBridgeCACertVolumeName)
	}
	if len(caCert.Secret.Items) != 1 || caCert.Secret.Items[0].Key != "ca.crt" {
		t.Errorf("ca.crt volume must project only ca.crt, got Items=%v", caCert.Secret.Items)
	}

	// Sidecar: mounts the CA dir (needs the keypair to mint leaves), but does
	// NOT get the agent trust env vars.
	var sidecar, agent *corev1.Container
	for i := range podSpec.Containers {
		switch podSpec.Containers[i].Name {
		case AuthBridgeProxyContainerName:
			sidecar = &podSpec.Containers[i]
		case "agent":
			agent = &podSpec.Containers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("authbridge-proxy sidecar not found")
	}
	if !hasMount(sidecar, TLSBridgeCAVolumeName, TLSBridgeCAMountPath) {
		t.Errorf("sidecar missing CA mount at %s", TLSBridgeCAMountPath)
	}
	for _, env := range tlsBridgeTrustEnvVars {
		if envValue(sidecar, env) != "" {
			t.Errorf("sidecar should not get agent trust env %s", env)
		}
	}

	// Agent: mounts ONLY the ca.crt volume (never the keypair — no private key
	// exposure) and has every trust env var pointing at ca.crt.
	if agent == nil {
		t.Fatal("agent container not found")
	}
	if hasMount(agent, TLSBridgeCAVolumeName, TLSBridgeCAMountPath) {
		t.Error("agent must NOT mount the keypair volume (would expose the CA private key)")
	}
	if !hasMount(agent, TLSBridgeCACertVolumeName, TLSBridgeCAMountPath) {
		t.Errorf("agent missing ca.crt mount at %s", TLSBridgeCAMountPath)
	}
	wantCA := TLSBridgeCAMountPath + "/ca.crt"
	for _, env := range tlsBridgeTrustEnvVars {
		if got := envValue(agent, env); got != wantCA {
			t.Errorf("agent env %s = %q, want %q", env, got, wantCA)
		}
	}
}

func TestInjectAuthBridge_TLSBridge_Disabled_NoMount(t *testing.T) {
	// Default tlsBridgeMode (disabled / unset) → no CA volume, no trust env.
	// The bridge is off unless the agent explicitly opts in.
	runtimeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeRuntimeConfigMapName, Namespace: "team1"},
		Data:       map[string]string{"config.yaml": "mode: proxy-sidecar\nmtls:\n  mode: disabled\n"},
	}
	m := newTestMutator(runtimeCM)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8000}}},
		},
	}
	labels := map[string]string{RossoctlTypeLabel: RossoctlTypeAgent}

	if _, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if findVolume(podSpec, TLSBridgeCAVolumeName) != nil {
		t.Error("CA volume must not be injected when tlsBridgeMode is disabled")
	}
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name != "agent" {
			continue
		}
		for _, env := range tlsBridgeTrustEnvVars {
			if envValue(&podSpec.Containers[i], env) != "" {
				t.Errorf("agent trust env %s must not be set when disabled", env)
			}
		}
	}
}

func TestApplyTLSBridgeMounts_Idempotent(t *testing.T) {
	// The mutating webhook can re-run on pod updates, so applyTLSBridgeMounts must
	// be idempotent: a second pass must not duplicate volumes, mounts, or env.
	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: AuthBridgeProxyContainerName},
			{Name: "agent"},
		},
	}
	applyTLSBridgeMounts(podSpec, "my-agent")
	applyTLSBridgeMounts(podSpec, "my-agent") // re-injection

	countVol := func(name string) int {
		n := 0
		for _, v := range podSpec.Volumes {
			if v.Name == name {
				n++
			}
		}
		return n
	}
	if got := countVol(TLSBridgeCAVolumeName); got != 1 {
		t.Errorf("keypair volume count = %d, want 1", got)
	}
	if got := countVol(TLSBridgeCACertVolumeName); got != 1 {
		t.Errorf("ca.crt volume count = %d, want 1", got)
	}

	countMount := func(c *corev1.Container, name string) int {
		n := 0
		for _, m := range c.VolumeMounts {
			if m.Name == name {
				n++
			}
		}
		return n
	}
	sidecar, agent := &podSpec.Containers[0], &podSpec.Containers[1]
	if got := countMount(sidecar, TLSBridgeCAVolumeName); got != 1 {
		t.Errorf("sidecar keypair mount count = %d, want 1", got)
	}
	if got := countMount(agent, TLSBridgeCACertVolumeName); got != 1 {
		t.Errorf("agent ca.crt mount count = %d, want 1", got)
	}
	for _, env := range tlsBridgeTrustEnvVars {
		n := 0
		for _, e := range agent.Env {
			if e.Name == env {
				n++
			}
		}
		if n != 1 {
			t.Errorf("agent env %s count = %d, want 1", env, n)
		}
	}
}

func TestInjectAuthBridge_TLSBridge_NoSPIRE_NoForcedFSGroup(t *testing.T) {
	// With SPIRE off (spiffe-helper disabled + mTLS disabled) the bridge must
	// still mount its CA, and must NOT force a fixed fsGroup — the keypair is
	// 0444 so the non-root sidecar reads it without one (OpenShift restricted-v2
	// SCC would reject a fixed fsGroup=0).
	runtimeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeRuntimeConfigMapName, Namespace: "team1"},
		Data:       map[string]string{"config.yaml": "mode: proxy-sidecar\ntls_bridge:\n  mode: enabled\nmtls:\n  mode: disabled\n"},
	}
	m := newTestMutator(runtimeCM)
	ctx := context.Background()

	podSpec := &corev1.PodSpec{
		ServiceAccountName: "my-agent",
		Containers: []corev1.Container{
			{Name: "agent", Image: "my-agent:latest", Ports: []corev1.ContainerPort{{ContainerPort: 8000}}},
		},
	}
	labels := map[string]string{
		RossoctlTypeLabel:       RossoctlTypeAgent,
		LabelSpiffeHelperInject: "false", // SPIRE off
	}

	if _, err := m.InjectAuthBridge(ctx, podSpec, "team1", "my-agent", "Deployment", labels, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if podSpec.SecurityContext != nil && podSpec.SecurityContext.FSGroup != nil {
		t.Errorf("with SPIRE off the bridge must not force fsGroup, got %d", *podSpec.SecurityContext.FSGroup)
	}
	kp := findVolume(podSpec, TLSBridgeCAVolumeName)
	if kp == nil || kp.Secret == nil {
		t.Fatalf("expected keypair volume %q to be mounted even without SPIRE", TLSBridgeCAVolumeName)
	}
	if kp.Secret.DefaultMode == nil || *kp.Secret.DefaultMode != 0o444 {
		t.Errorf("keypair DefaultMode = %v, want 0444 (readable by non-root sidecar without fsGroup)", kp.Secret.DefaultMode)
	}
}

// hasMount reports whether the container has a volume mount with the given
// name at the given path.
func hasMount(c *corev1.Container, name, path string) bool {
	for _, vm := range c.VolumeMounts {
		if vm.Name == name && vm.MountPath == path {
			return true
		}
	}
	return false
}

// envValue returns the value of the named env var on the container, or "".
func envValue(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
