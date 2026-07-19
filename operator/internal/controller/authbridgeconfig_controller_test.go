/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const authbridgeConfigTestNamespace = "tx-e2e"

// authbridgeTestPlatform returns a platform config whose default realm is the
// platform-global "rossoctl" — i.e. the value that would clobber a per-namespace
// override if the reconciler overwrote existing keys.
func authbridgeTestPlatform() AuthbridgeConfigPlatform {
	return AuthbridgeConfigPlatform{
		KeycloakAdminSecretNamespace: "keycloak",
		KeycloakRealm:                "rossoctl",
		KeycloakPublicURL:            "https://keycloak.example",
	}
}

func rossoctlEnabledNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{labelRossoctlEnabled: "true"},
		},
	}
}

func TestAuthbridgeConfigReconciler_Reconcile(t *testing.T) {
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: authbridgeConfigTestNamespace}}

	getCM := func(t *testing.T, c client.Client) *corev1.ConfigMap {
		t.Helper()
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: authbridgeConfigTestNamespace, Name: authbridgeConfigCM}, cm); err != nil {
			t.Fatalf("get authbridge-config: %v", err)
		}
		return cm
	}

	cases := []struct {
		name  string
		objs  []client.Object
		check func(t *testing.T, c client.Client)
	}{
		{
			// Regression for #433: an explicitly-set per-namespace realm must
			// survive reconciliation rather than being reset to the platform default.
			name: "preserves explicitly-set KEYCLOAK_REALM on existing ConfigMap",
			objs: []client.Object{
				rossoctlEnabledNamespace(authbridgeConfigTestNamespace),
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      authbridgeConfigCM,
						Namespace: authbridgeConfigTestNamespace,
					},
					Data: map[string]string{
						"KEYCLOAK_REALM":   "tx-e2e",
						"KEYCLOAK_URL":     "https://keycloak.example",
						"CLIENT_AUTH_TYPE": "client-secret",
					},
				},
			},
			check: func(t *testing.T, c client.Client) {
				cm := getCM(t, c)
				if got := cm.Data["KEYCLOAK_REALM"]; got != "tx-e2e" {
					t.Fatalf("KEYCLOAK_REALM = %q, want %q (explicit per-namespace value must be preserved)", got, "tx-e2e")
				}
			},
		},
		{
			name: "creates ConfigMap with platform defaults when none exists",
			objs: []client.Object{
				rossoctlEnabledNamespace(authbridgeConfigTestNamespace),
			},
			check: func(t *testing.T, c client.Client) {
				cm := getCM(t, c)
				if got := cm.Data["KEYCLOAK_REALM"]; got != "rossoctl" {
					t.Fatalf("KEYCLOAK_REALM = %q, want platform default %q", got, "rossoctl")
				}
				if got := cm.Data["KEYCLOAK_URL"]; got == "" {
					t.Fatalf("KEYCLOAK_URL should be populated from platform defaults, got empty")
				}
				if cm.Labels["app.kubernetes.io/managed-by"] != "rossoctl-operator" {
					t.Fatalf("missing managed-by label, got %v", cm.Labels)
				}
				if cm.Labels[LabelNamespaceDefaults] != "true" {
					t.Fatalf("missing %s label, got %v", LabelNamespaceDefaults, cm.Labels)
				}
			},
		},
		{
			name: "fills missing/empty keys without overwriting present ones",
			objs: []client.Object{
				rossoctlEnabledNamespace(authbridgeConfigTestNamespace),
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      authbridgeConfigCM,
						Namespace: authbridgeConfigTestNamespace,
					},
					Data: map[string]string{
						"KEYCLOAK_REALM":   "tx-e2e", // explicit, non-empty -> keep
						"CLIENT_AUTH_TYPE": "",       // empty -> should be filled
					},
				},
			},
			check: func(t *testing.T, c client.Client) {
				cm := getCM(t, c)
				if got := cm.Data["KEYCLOAK_REALM"]; got != "tx-e2e" {
					t.Fatalf("KEYCLOAK_REALM = %q, want %q (preserved)", got, "tx-e2e")
				}
				if got := cm.Data["KEYCLOAK_URL"]; got == "" {
					t.Fatalf("KEYCLOAK_URL (missing key) should be filled with default, got empty")
				}
				if got := cm.Data["CLIENT_AUTH_TYPE"]; got != "client-secret" {
					t.Fatalf("CLIENT_AUTH_TYPE = %q, want empty value filled with default %q", got, "client-secret")
				}
			},
		},
		{
			name: "stamps ownership labels on an externally-created ConfigMap",
			objs: []client.Object{
				rossoctlEnabledNamespace(authbridgeConfigTestNamespace),
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      authbridgeConfigCM,
						Namespace: authbridgeConfigTestNamespace,
					},
					Data: map[string]string{"KEYCLOAK_REALM": "tx-e2e"},
				},
			},
			check: func(t *testing.T, c client.Client) {
				cm := getCM(t, c)
				if cm.Labels["app.kubernetes.io/managed-by"] != "rossoctl-operator" {
					t.Fatalf("missing managed-by label, got %v", cm.Labels)
				}
				if cm.Labels[LabelNamespaceDefaults] != "true" {
					t.Fatalf("missing %s label, got %v", LabelNamespaceDefaults, cm.Labels)
				}
			},
		},
		{
			name: "created ConfigMap has no SPIRE_ENABLED key",
			objs: []client.Object{
				rossoctlEnabledNamespace(authbridgeConfigTestNamespace),
			},
			check: func(t *testing.T, c client.Client) {
				cm := getCM(t, c)
				if _, ok := cm.Data["SPIRE_ENABLED"]; ok {
					t.Fatalf("SPIRE_ENABLED should not be present in authbridge-config, got %v", cm.Data)
				}
			},
		},
		{
			name: "skips namespace without rossoctl-enabled label",
			objs: []client.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: authbridgeConfigTestNamespace}},
			},
			check: func(t *testing.T, c client.Client) {
				cm := &corev1.ConfigMap{}
				err := c.Get(ctx, client.ObjectKey{Namespace: authbridgeConfigTestNamespace, Name: authbridgeConfigCM}, cm)
				if err == nil {
					t.Fatalf("expected no authbridge-config to be created for non-rossoctl-enabled namespace")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := clientRegistrationTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			r := &AuthbridgeConfigReconciler{
				Client:   c,
				Platform: authbridgeTestPlatform(),
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			tc.check(t, c)
		})
	}
}
