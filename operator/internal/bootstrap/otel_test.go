/*
Copyright 2026.

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

package bootstrap

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/rossoctl/operator/internal/mlflow"
)

const testNamespace = "rossoctl-system"

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = mlflow.AddToScheme(s)
	return s
}

func testLogger() logr.Logger {
	return zap.New(zap.UseDevMode(true))
}

func newRunnable(cl client.Client, isOCP func(context.Context) (bool, error), mlflowCRD func(context.Context) (bool, error)) *OtelBootstrapRunnable {
	return &OtelBootstrapRunnable{
		Client:          cl,
		APIReader:       cl,
		Scheme:          testScheme(),
		Namespace:       testNamespace,
		Log:             testLogger(),
		IsOpenShift:     isOCP,
		MLflowCRDExists: mlflowCRD,
	}
}

func otelCollectorDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      collectorDeployment,
			Namespace: testNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "otel-collector"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "otel-collector"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "otel-collector", Image: "otel/opentelemetry-collector-contrib:0.122.1"},
					},
				},
			},
		},
	}
}

func mlflowCR(name, namespace string, available bool, url string) *mlflow.MLflow {
	conditions := []metav1.Condition{}
	if available {
		conditions = append(conditions, metav1.Condition{
			Type:               "Available",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "Ready",
		})
	}
	return &mlflow.MLflow{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "mlflow.opendatahub.io/v1",
			Kind:       "MLflow",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: mlflow.MLflowStatus{
			Conditions: conditions,
			URL:        url,
		},
	}
}

// --- OCP detection tests ---

func TestStart_NonOCP_SkipsIngressCA(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	r := newRunnable(cl, notOpenShift, noMLflowCRD)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	err := cl.Get(context.Background(), types.NamespacedName{
		Name: ingressCAConfigMap, Namespace: testNamespace,
	}, cm)
	if err == nil {
		t.Error("Expected ingress CA ConfigMap to not exist on non-OCP, but it was created")
	}
}

func TestStart_OCP_CreatesIngressCA(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()

	ingressCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressCertConfigMap,
			Namespace: ingressCertNamespace,
		},
		Data: map[string]string{caBundleKey: "-----BEGIN CERTIFICATE-----\nINGRESS\n-----END CERTIFICATE-----"},
	}
	rootCACM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rootCAConfigMap,
			Namespace: rootCANamespace,
		},
		Data: map[string]string{rootCAKey: "-----BEGIN CERTIFICATE-----\nROOT\n-----END CERTIFICATE-----"},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, ingressCertCM, rootCACM).Build()
	r := newRunnable(cl, isOpenShift, noMLflowCRD)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: ingressCAConfigMap, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected ingress CA ConfigMap to exist: %v", err)
	}

	bundle := cm.Data[caBundleKey]
	if bundle == "" {
		t.Fatal("Ingress CA ConfigMap has empty ca-bundle.crt")
	}
	if len(bundle) <= 50 {
		t.Error("Expected concatenated ingress + root CA bundle")
	}
}

func TestIngressCA_Idempotent_NoUpdateWhenUnchanged(t *testing.T) {
	scheme := testScheme()

	caBundle := "-----BEGIN CERTIFICATE-----\nINGRESS\n-----END CERTIFICATE-----"
	ingressCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressCertConfigMap,
			Namespace: ingressCertNamespace,
		},
		Data: map[string]string{caBundleKey: caBundle},
	}
	existingCA := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressCAConfigMap,
			Namespace: testNamespace,
		},
		Data: map[string]string{caBundleKey: caBundle},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ingressCertCM, existingCA, otelCollectorDeployment()).Build()
	r := newRunnable(cl, isOpenShift, noMLflowCRD)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: ingressCAConfigMap, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Failed to get CA ConfigMap: %v", err)
	}
	if cm.Data[caBundleKey] != caBundle {
		t.Error("CA bundle was modified when it should have been unchanged")
	}
}

// --- MLflow CRD missing tests ---

func TestMLflowCRDMissing_SkipsMLflowPresets(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	r := newRunnable(cl, notOpenShift, noMLflowCRD)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorConfigMapName, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected collector ConfigMap to exist: %v", err)
	}

	config := cm.Data[configMapDataKey]
	if config == "" {
		t.Fatal("Collector config is empty")
	}

	assertContains(t, config, "traces/default", "Expected default pipeline when MLflow CRD missing")
	assertNotContains(t, config, "otlphttp/mlflow", "Expected no MLflow exporter when CRD missing")
}

// --- MLflow service discovery tests ---

func TestMLflowCRDPresent_DiscoversCRNamespace(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()
	cr := mlflowCR("mlflow", "redhat-ods-applications", true, "https://mlflow.example.com")

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, cr).Build()
	r := newRunnable(cl, notOpenShift, mlflowCRDPresent)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorConfigMapName, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected collector ConfigMap to exist: %v", err)
	}

	config := cm.Data[configMapDataKey]
	assertContains(t, config, "otlphttp/mlflow", "Expected MLflow exporter in config")
	assertContains(t, config, "traces/mlflow", "Expected MLflow pipeline in config")
}

func TestMLflowCRDPresent_OCP_UsesRHOAIAuth(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()
	cr := mlflowCR("mlflow", "redhat-ods-applications", true, "https://mlflow.example.com")

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, cr).Build()
	r := newRunnable(cl, isOpenShift, mlflowCRDPresent)

	ingressCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressCertConfigMap,
			Namespace: ingressCertNamespace,
		},
		Data: map[string]string{caBundleKey: "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----"},
	}
	if err := cl.Create(context.Background(), ingressCertCM); err != nil {
		t.Fatalf("Failed to create ingress cert: %v", err)
	}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorConfigMapName, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected collector ConfigMap to exist: %v", err)
	}

	config := cm.Data[configMapDataKey]
	assertContains(t, config, "bearertokenauth/mlflow", "Expected RHOAI bearer token auth on OCP")
	assertNotContains(t, config, "x-mlflow-workspace",
		"Workspace header should not be set when mlflow.workspace is not configured")
	assertContains(t, config, "ca_file", "Expected service-ca.crt TLS config on OCP")
}

func TestMLflowCRDPresent_OCP_WithWorkspace_SetsHeaders(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()
	cr := mlflowCR("mlflow", "redhat-ods-applications", true, "")
	cr.Status.Address = &mlflow.MLflowAddress{URL: "https://mlflow.redhat-ods-applications.svc:8443"}

	ingressCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressCertConfigMap,
			Namespace: ingressCertNamespace,
		},
		Data: map[string]string{caBundleKey: "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----"},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, cr, ingressCertCM).Build()
	r := newRunnable(cl, isOpenShift, mlflowCRDPresent)
	r.MLflowWorkspace = "team1"
	r.MLflowExperimentName = "rossoctl-traces"
	r.EnsureExperiment = func(_ context.Context, _, _ string) (string, error) {
		return "42", nil
	}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorConfigMapName, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected collector ConfigMap to exist: %v", err)
	}

	config := cm.Data[configMapDataKey]
	assertContains(t, config, "x-mlflow-workspace", "Expected workspace header when workspace configured")
	assertContains(t, config, "team1", "Expected workspace to match configured value")
	assertContains(t, config, "x-mlflow-experiment-id", "Expected experiment-id header")
	assertContains(t, config, "42", "Expected experiment ID from MLflow API")
}

func TestMLflowCRDPresent_NonOCP_UsesOAuthAuth(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()
	cr := mlflowCR("mlflow", "default", true, "http://mlflow:5000")

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, cr).Build()
	r := newRunnable(cl, notOpenShift, mlflowCRDPresent)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorConfigMapName, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected collector ConfigMap to exist: %v", err)
	}

	config := cm.Data[configMapDataKey]
	assertContains(t, config, "oauth2client/mlflow", "Expected OAuth2 client auth on non-OCP")
}

// --- mlflowInfoFromCR negative tests ---

func TestMLflowInfoFromCR_EmptyAddressURL(t *testing.T) {
	cr := &mlflow.MLflow{
		ObjectMeta: metav1.ObjectMeta{Name: "mlflow"},
		Status: mlflow.MLflowStatus{
			Address: &mlflow.MLflowAddress{URL: ""},
			URL:     "https://mlflow-external.example.com",
		},
	}
	info := mlflowInfoFromCR(cr, logr.Discard())
	if !info.available {
		t.Fatal("Expected available=true")
	}
	if info.tracesURL != "https://mlflow-external.example.com/v1/traces" {
		t.Fatalf("Expected fallback to Status.URL, got tracesURL=%q", info.tracesURL)
	}
}

func TestMLflowInfoFromCR_MalformedURL(t *testing.T) {
	cr := &mlflow.MLflow{
		ObjectMeta: metav1.ObjectMeta{Name: "mlflow"},
		Status: mlflow.MLflowStatus{
			Address: &mlflow.MLflowAddress{URL: "://badurl"},
			URL:     "https://mlflow-fallback.example.com",
		},
	}
	info := mlflowInfoFromCR(cr, logr.Discard())
	if !info.available {
		t.Fatal("Expected available=true")
	}
	if info.tracesURL != "https://mlflow-fallback.example.com/v1/traces" {
		t.Fatalf("Expected fallback to Status.URL for malformed address, got tracesURL=%q", info.tracesURL)
	}
}

func TestMLflowInfoFromCR_MissingSchemeFallback(t *testing.T) {
	cr := &mlflow.MLflow{
		ObjectMeta: metav1.ObjectMeta{Name: "mlflow"},
		Status: mlflow.MLflowStatus{
			Address: &mlflow.MLflowAddress{URL: "//mlflow.ns.svc:8443"},
			URL:     "https://mlflow-fallback.example.com",
		},
	}
	info := mlflowInfoFromCR(cr, logr.Discard())
	if !info.available {
		t.Fatal("Expected available=true")
	}
	if info.tracesURL != "https://mlflow-fallback.example.com/v1/traces" {
		t.Fatalf("Expected fallback when scheme is empty, got tracesURL=%q", info.tracesURL)
	}
}

// --- Phoenix tests ---

func TestPhoenixPresent_MergesPhoenixPreset(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()
	phoenixSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      phoenixServiceName,
			Namespace: testNamespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 4317}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, phoenixSvc).Build()
	r := newRunnable(cl, notOpenShift, noMLflowCRD)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorConfigMapName, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected collector ConfigMap to exist: %v", err)
	}

	config := cm.Data[configMapDataKey]
	assertContains(t, config, "otlp/phoenix", "Expected Phoenix exporter in config")
	assertContains(t, config, "traces/phoenix", "Expected Phoenix pipeline in config")
	assertNotContains(t, config, "traces/default", "Expected no default pipeline when Phoenix is active")
}

func TestPhoenixAbsent_NoPhoenixPreset(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	r := newRunnable(cl, notOpenShift, noMLflowCRD)

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorConfigMapName, Namespace: testNamespace,
	}, cm); err != nil {
		t.Fatalf("Expected collector ConfigMap to exist: %v", err)
	}

	config := cm.Data[configMapDataKey]
	assertNotContains(t, config, "otlp/phoenix", "Expected no Phoenix exporter when service absent")
}

// --- ConfigMap diff detection / idempotency tests ---

func TestConfigMapUnchanged_NoDeploymentRestart(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	r := newRunnable(cl, notOpenShift, noMLflowCRD)

	// First run: creates ConfigMap and restarts
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("First Start() failed: %v", err)
	}

	// Capture the annotation after first run
	updatedDep := &appsv1.Deployment{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorDeployment, Namespace: testNamespace,
	}, updatedDep); err != nil {
		t.Fatalf("Failed to get deployment: %v", err)
	}
	firstHash := updatedDep.Spec.Template.Annotations[restartAnnotation]
	if firstHash == "" {
		t.Fatal("Expected config hash annotation on Deployment after first run")
	}

	// Second run: should detect no change
	r2 := newRunnable(cl, notOpenShift, noMLflowCRD)
	if err := r2.Start(context.Background()); err != nil {
		t.Fatalf("Second Start() failed: %v", err)
	}

	updatedDep2 := &appsv1.Deployment{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: collectorDeployment, Namespace: testNamespace,
	}, updatedDep2); err != nil {
		t.Fatalf("Failed to get deployment: %v", err)
	}
	secondHash := updatedDep2.Spec.Template.Annotations[restartAnnotation]
	if firstHash != secondHash {
		t.Errorf("Config hash changed between idempotent runs: %s != %s", firstHash, secondHash)
	}
}

func TestConfigMapChanged_TriggersRestart(t *testing.T) {
	scheme := testScheme()
	dep := otelCollectorDeployment()

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()

	// First run without Phoenix
	r1 := newRunnable(cl, notOpenShift, noMLflowCRD)
	if err := r1.Start(context.Background()); err != nil {
		t.Fatalf("First Start() failed: %v", err)
	}

	updatedDep := &appsv1.Deployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{
		Name: collectorDeployment, Namespace: testNamespace,
	}, updatedDep)
	firstHash := updatedDep.Spec.Template.Annotations[restartAnnotation]

	// Add Phoenix service
	phoenixSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      phoenixServiceName,
			Namespace: testNamespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 4317}},
		},
	}
	if err := cl.Create(context.Background(), phoenixSvc); err != nil {
		t.Fatalf("Failed to create Phoenix service: %v", err)
	}

	// Second run with Phoenix: config changes, triggers restart
	r2 := newRunnable(cl, notOpenShift, noMLflowCRD)
	if err := r2.Start(context.Background()); err != nil {
		t.Fatalf("Second Start() failed: %v", err)
	}

	updatedDep2 := &appsv1.Deployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{
		Name: collectorDeployment, Namespace: testNamespace,
	}, updatedDep2)
	secondHash := updatedDep2.Spec.Template.Annotations[restartAnnotation]

	if firstHash == secondHash {
		t.Error("Expected config hash to change when Phoenix becomes available")
	}
}

// --- assembleCollectorConfig unit tests ---

func TestAssembleConfig_DefaultOnly(t *testing.T) {
	cfg, err := assembleCollectorConfig(false, &mlflowInfo{}, false)
	if err != nil {
		t.Fatalf("assembleCollectorConfig failed: %v", err)
	}

	svc, ok := cfg["service"].(map[string]any)
	if !ok {
		t.Fatal("Expected 'service' key in config")
	}
	pipelines, ok := svc["pipelines"].(map[string]any)
	if !ok {
		t.Fatal("Expected 'pipelines' in service")
	}
	if _, ok := pipelines["traces/default"]; !ok {
		t.Error("Expected traces/default pipeline")
	}
}

func TestAssembleConfig_PhoenixAndMLflow(t *testing.T) {
	cfg, err := assembleCollectorConfig(false, &mlflowInfo{available: true, tracesURL: "http://mlflow.mlflow-ns.svc:5000/v1/traces"}, true)
	if err != nil {
		t.Fatalf("assembleCollectorConfig failed: %v", err)
	}

	svc := cfg["service"].(map[string]any)
	pipelines := svc["pipelines"].(map[string]any)

	if _, ok := pipelines["traces/phoenix"]; !ok {
		t.Error("Expected traces/phoenix pipeline")
	}
	if _, ok := pipelines["traces/mlflow"]; !ok {
		t.Error("Expected traces/mlflow pipeline")
	}
	if _, ok := pipelines["traces/default"]; ok {
		t.Error("Expected no traces/default pipeline when components are active")
	}
}

func TestAssembleConfig_OCP_MLflow_ServiceCATLS(t *testing.T) {
	cfg, err := assembleCollectorConfig(true, &mlflowInfo{available: true, tracesURL: "https://mlflow.rhoai-ns.svc:8443/v1/traces"}, false)
	if err != nil {
		t.Fatalf("assembleCollectorConfig failed: %v", err)
	}

	extensions := cfg["extensions"].(map[string]any)
	bearer, ok := extensions["bearertokenauth/mlflow"].(map[string]any)
	if !ok {
		t.Fatal("Expected bearertokenauth/mlflow extension on OCP")
	}
	if bearer["filename"] != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Error("Expected SA token filename for bearer auth")
	}

	exporters := cfg["exporters"].(map[string]any)
	mlflowExp := exporters["otlphttp/mlflow"].(map[string]any)
	auth, ok := mlflowExp["auth"].(map[string]any)
	if !ok {
		t.Fatal("Expected auth on MLflow exporter")
	}
	if auth["authenticator"] != "bearertokenauth/mlflow" {
		t.Error("Expected bearertokenauth/mlflow authenticator on OCP")
	}

	tlsCfg, ok := mlflowExp["tls"].(map[string]any)
	if !ok {
		t.Fatal("Expected tls config on MLflow exporter")
	}
	if tlsCfg["ca_file"] != "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt" {
		t.Errorf("Expected service-ca.crt ca_file, got %v", tlsCfg["ca_file"])
	}
	if _, hasInsecure := tlsCfg["insecure"]; hasInsecure {
		t.Error("Expected insecure flag to be cleared on OCP")
	}

	// No workspace/experiment headers when mlflowInfo has no workspaceNS
	if headers, ok := mlflowExp["headers"].(map[string]any); ok {
		if _, hasWorkspace := headers["x-mlflow-workspace"]; hasWorkspace {
			t.Error("x-mlflow-workspace should not be set when no workspace is configured")
		}
	}
}

func TestAssembleConfig_OCP_MLflow_WithWorkspace(t *testing.T) {
	cfg, err := assembleCollectorConfig(true, &mlflowInfo{
		available:    true,
		tracesURL:    "https://mlflow.rhoai-ns.svc:8443/v1/traces",
		workspaceNS:  "team1",
		experimentID: "5",
	}, false)
	if err != nil {
		t.Fatalf("assembleCollectorConfig failed: %v", err)
	}

	exporters := cfg["exporters"].(map[string]any)
	mlflowExp := exporters["otlphttp/mlflow"].(map[string]any)

	headers, ok := mlflowExp["headers"].(map[string]any)
	if !ok {
		t.Fatal("Expected headers on MLflow exporter when workspace is set")
	}
	if headers["x-mlflow-workspace"] != "team1" {
		t.Errorf("Expected x-mlflow-workspace=team1, got %v", headers["x-mlflow-workspace"])
	}
	if headers["x-mlflow-experiment-id"] != "5" {
		t.Errorf("Expected x-mlflow-experiment-id=5, got %v", headers["x-mlflow-experiment-id"])
	}
}

// --- mergeDeep tests ---

func TestMergeDeep_Basic(t *testing.T) {
	dst := map[string]any{
		"a": "1",
		"b": map[string]any{"c": "2"},
	}
	src := map[string]any{
		"a": "overwritten",
		"b": map[string]any{"d": "3"},
		"e": "new",
	}
	mergeDeep(dst, src)

	if dst["a"] != "overwritten" {
		t.Error("Expected 'a' to be overwritten")
	}
	b := dst["b"].(map[string]any)
	if b["c"] != "2" {
		t.Error("Expected 'b.c' to be preserved")
	}
	if b["d"] != "3" {
		t.Error("Expected 'b.d' to be merged")
	}
	if dst["e"] != "new" {
		t.Error("Expected 'e' to be added")
	}
}

func TestMergeDeep_SliceOverwrite(t *testing.T) {
	dst := map[string]any{
		"list": []string{"a", "b"},
	}
	src := map[string]any{
		"list": []string{"c"},
	}
	mergeDeep(dst, src)

	list, ok := dst["list"].([]string)
	if !ok {
		t.Fatal("Expected list to be []string")
	}
	if len(list) != 1 || list[0] != "c" {
		t.Errorf("Expected list to be overwritten to [c], got %v", list)
	}
}

// --- NeedLeaderElection ---

func TestNeedLeaderElection(t *testing.T) {
	r := &OtelBootstrapRunnable{}
	if !r.NeedLeaderElection() {
		t.Error("Expected NeedLeaderElection to return true")
	}
}

// --- helpers ---

func isOpenShift(_ context.Context) (bool, error)      { return true, nil }
func notOpenShift(_ context.Context) (bool, error)     { return false, nil }
func noMLflowCRD(_ context.Context) (bool, error)      { return false, nil }
func mlflowCRDPresent(_ context.Context) (bool, error) { return true, nil }

func assertContains(t *testing.T, s, substr, msg string) {
	t.Helper()
	if !contains(s, substr) {
		t.Errorf("%s: %q not found in output", msg, substr)
	}
}

func assertNotContains(t *testing.T, s, substr, msg string) {
	t.Helper()
	if contains(s, substr) {
		t.Errorf("%s: %q unexpectedly found in output", msg, substr)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
