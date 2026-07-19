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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func keycloakTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	s.AddKnownTypeWithName(keycloakGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(realmImportGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(routeGVK, &unstructured.Unstructured{})
	return s
}

func newKeycloakRunnable(objs ...client.Object) *KeycloakBootstrapRunnable {
	scheme := keycloakTestScheme()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "keycloak"}}
	allObjs := append([]client.Object{ns}, objs...)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(allObjs...).Build()
	return &KeycloakBootstrapRunnable{
		Client:                 cl,
		APIReader:              cl,
		Namespace:              "keycloak",
		Log:                    testLogger(),
		RouteDiscoveryAttempts: 1,
	}
}

func TestKeycloak_CreatesPostgresResources(t *testing.T) {
	r := newKeycloakRunnable()

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Secret
	secret := &corev1.Secret{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "keycloak-db-secret", Namespace: "keycloak"}, secret); err != nil {
		t.Fatalf("keycloak-db-secret not created: %v", err)
	}
	if string(secret.Data["username"]) != "testuser" {
		t.Errorf("unexpected username: %s", secret.Data["username"])
	}

	// ConfigMap
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "postgres-kc-init-script", Namespace: "keycloak"}, cm); err != nil {
		t.Fatalf("postgres-kc-init-script not created: %v", err)
	}
	if cm.Data["init.sh"] == "" {
		t.Error("init.sh is empty")
	}

	// StatefulSet
	sts := &appsv1.StatefulSet{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "postgres-kc", Namespace: "keycloak"}, sts); err != nil {
		t.Fatalf("postgres-kc StatefulSet not created: %v", err)
	}
	if *sts.Spec.Replicas != 1 {
		t.Errorf("unexpected replicas: %d", *sts.Spec.Replicas)
	}

	// Service
	svc := &corev1.Service{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "postgres-kc", Namespace: "keycloak"}, svc); err != nil {
		t.Fatalf("postgres-kc Service not created: %v", err)
	}
	if svc.Spec.Ports[0].Port != 5432 {
		t.Errorf("unexpected port: %d", svc.Spec.Ports[0].Port)
	}

	// Keycloak CR
	keycloakCR := &unstructured.Unstructured{}
	keycloakCR.SetGroupVersionKind(keycloakGVK)
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "keycloak", Namespace: "keycloak"}, keycloakCR); err != nil {
		t.Fatalf("Keycloak CR not created: %v", err)
	}
	dbVendor, _, _ := unstructured.NestedString(keycloakCR.Object, "spec", "db", "vendor")
	if dbVendor != "postgres" {
		t.Errorf("unexpected db vendor: %s", dbVendor)
	}

	// Route
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK)
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "keycloak", Namespace: "keycloak"}, route); err != nil {
		t.Fatalf("Keycloak Route not created: %v", err)
	}
	toName, _, _ := unstructured.NestedString(route.Object, "spec", "to", "name")
	if toName != "keycloak-service" {
		t.Errorf("unexpected Route target service: %s", toName)
	}
}

func TestKeycloak_IdempotentPostgres(t *testing.T) {
	r := newKeycloakRunnable()

	// Run twice
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("First Start() failed: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Second Start() failed: %v", err)
	}

	// Should still have exactly one of each
	secret := &corev1.Secret{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "keycloak-db-secret", Namespace: "keycloak"}, secret); err != nil {
		t.Fatalf("keycloak-db-secret missing: %v", err)
	}
}

func TestKeycloak_DoesNotOverwriteExistingSecret(t *testing.T) {
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keycloak-db-secret", Namespace: "keycloak"},
		Data: map[string][]byte{
			"username": []byte("customuser"),
			"password": []byte("custompass"),
		},
	}

	r := newKeycloakRunnable(existingSecret)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	secret := &corev1.Secret{}
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: "keycloak-db-secret", Namespace: "keycloak"}, secret)

	if string(secret.Data["username"]) != "customuser" {
		t.Errorf("Secret was overwritten, got username=%s", secret.Data["username"])
	}
}

func TestKeycloak_CreatesTestUsersSecret(t *testing.T) {
	r := newKeycloakRunnable()
	r.KeycloakPublicURL = "https://keycloak.example.com"

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	secret := &corev1.Secret{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "rossoctl-test-users", Namespace: "keycloak"}, secret); err != nil {
		t.Fatalf("rossoctl-test-users not created: %v", err)
	}
	if len(secret.Data["admin-password"]) == 0 {
		t.Error("admin-password is empty")
	}
	if len(secret.Data["dev-user-password"]) == 0 {
		t.Error("dev-user-password is empty")
	}
	if len(secret.Data["ns-admin-password"]) == 0 {
		t.Error("ns-admin-password is empty")
	}
}

func TestKeycloak_DoesNotOverwriteTestUsersSecret(t *testing.T) {
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rossoctl-test-users", Namespace: "keycloak"},
		Data: map[string][]byte{
			"admin-password":    []byte("kept-admin"),
			"dev-user-password": []byte("kept-dev"),
			"ns-admin-password": []byte("kept-ns"),
		},
	}
	r := newKeycloakRunnable(existingSecret)
	r.KeycloakPublicURL = "https://keycloak.example.com"

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	secret := &corev1.Secret{}
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: "rossoctl-test-users", Namespace: "keycloak"}, secret)
	if string(secret.Data["admin-password"]) != "kept-admin" {
		t.Errorf("test-users Secret was overwritten, got admin-password=%s", secret.Data["admin-password"])
	}
}

func TestKeycloak_CreatesRealmImport(t *testing.T) {
	r := newKeycloakRunnable()
	r.Realm = "rossoctl"
	r.KeycloakPublicURL = "https://keycloak.example.com"

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(realmImportGVK)
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "rossoctl-realm-import", Namespace: "keycloak"}, cr); err != nil {
		t.Fatalf("KeycloakRealmImport not created: %v", err)
	}

	realm, _, _ := unstructured.NestedString(cr.Object, "spec", "realm", "realm")
	if realm != "rossoctl" {
		t.Errorf("unexpected realm: %s", realm)
	}

	crName, _, _ := unstructured.NestedString(cr.Object, "spec", "keycloakCRName")
	if crName != "keycloak" {
		t.Errorf("unexpected keycloakCRName: %s", crName)
	}
}
