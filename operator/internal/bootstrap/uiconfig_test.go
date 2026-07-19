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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newUIConfigRunnable(objs ...client.Object) *UIConfigBootstrapRunnable {
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &UIConfigBootstrapRunnable{
		Client:    cl,
		APIReader: cl,
		Namespace: testNamespace,
		Log:       testLogger(),
	}
}

func TestUIConfig_PatchesConfigMap(t *testing.T) {
	cr := mlflowCR("mlflow", "", true, "https://mlflow-gateway.apps.example.com")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: uiConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{"DOMAIN_NAME": "example.com"},
	}

	r := newUIConfigRunnable(cr, cm)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	got := &corev1.ConfigMap{}
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: uiConfigMapName, Namespace: testNamespace}, got)

	if got.Data[mlflowDashboardURLKey] != "https://mlflow-gateway.apps.example.com/" {
		t.Errorf("got %q, want %q", got.Data[mlflowDashboardURLKey], "https://mlflow-gateway.apps.example.com/")
	}
	if got.Data["DOMAIN_NAME"] != "example.com" {
		t.Error("existing keys were modified")
	}
}

func TestUIConfig_Idempotent(t *testing.T) {
	cr := mlflowCR("mlflow", "", true, "https://mlflow.apps.cluster.com/mlflow")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: uiConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{mlflowDashboardURLKey: "https://mlflow.apps.cluster.com/mlflow/"},
	}

	r := newUIConfigRunnable(cr, cm)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	got := &corev1.ConfigMap{}
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: uiConfigMapName, Namespace: testNamespace}, got)

	if got.Data[mlflowDashboardURLKey] != "https://mlflow.apps.cluster.com/mlflow/" {
		t.Error("URL was unexpectedly modified")
	}
}

func TestUIConfig_SkipsWhenNoMLflow(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: uiConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{},
	}

	r := newUIConfigRunnable(cm)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	got := &corev1.ConfigMap{}
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: uiConfigMapName, Namespace: testNamespace}, got)

	if _, ok := got.Data[mlflowDashboardURLKey]; ok {
		t.Error("should not set URL when no MLflow CR exists")
	}
}

func TestUIConfig_SkipsWhenNoConfigMap(t *testing.T) {
	cr := mlflowCR("mlflow", "", true, "https://mlflow.apps.cluster.com")

	r := newUIConfigRunnable(cr)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
}

func TestUIConfig_NormalizesTrailingSlash(t *testing.T) {
	cr := mlflowCR("mlflow", "", true, "https://mlflow.apps.cluster.com/mlflow")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: uiConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{},
	}

	r := newUIConfigRunnable(cr, cm)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	got := &corev1.ConfigMap{}
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: uiConfigMapName, Namespace: testNamespace}, got)

	if got.Data[mlflowDashboardURLKey] != "https://mlflow.apps.cluster.com/mlflow/" {
		t.Errorf("got %q, expected trailing slash to be added", got.Data[mlflowDashboardURLKey])
	}
}
