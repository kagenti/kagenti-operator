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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeReader(objs ...client.Object) client.Reader {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestReadNamespaceConfig_AllPresent(t *testing.T) {
	abCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeConfigMapName, Namespace: "ns1"},
		Data: map[string]string{
			"KEYCLOAK_URL":            "http://keycloak:8080",
			"KEYCLOAK_REALM":          "rossoctl",
			"PLATFORM_CLIENT_IDS":     "id1,id2",
			"TOKEN_URL":               "http://keycloak:8080/realms/demo/protocol/openid-connect/token",
			"ISSUER":                  "http://keycloak:8080/realms/demo",
			"EXPECTED_AUDIENCE":       "my-audience",
			"TARGET_AUDIENCE":         "auth-target",
			"TARGET_SCOPES":           "openid auth-target-aud",
			"DEFAULT_OUTBOUND_POLICY": "passthrough",
		},
	}
	routesCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthproxyRoutesConfigMapName, Namespace: "ns1"},
		Data:       map[string]string{"routes.yaml": "routes: []"},
	}

	reader := newFakeReader(abCM, routesCM)
	cfg, err := ReadNamespaceConfig(context.Background(), reader, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.KeycloakURL != "http://keycloak:8080" {
		t.Errorf("KeycloakURL = %q, want %q", cfg.KeycloakURL, "http://keycloak:8080")
	}
	if cfg.KeycloakRealm != "rossoctl" {
		t.Errorf("KeycloakRealm = %q, want %q", cfg.KeycloakRealm, "rossoctl")
	}
	if cfg.TokenURL != "http://keycloak:8080/realms/demo/protocol/openid-connect/token" {
		t.Errorf("TokenURL = %q", cfg.TokenURL)
	}
	if cfg.Issuer != "http://keycloak:8080/realms/demo" {
		t.Errorf("Issuer = %q", cfg.Issuer)
	}
	if cfg.TargetAudience != "auth-target" {
		t.Errorf("TargetAudience = %q", cfg.TargetAudience)
	}
	if cfg.DefaultOutboundPolicy != "passthrough" {
		t.Errorf("DefaultOutboundPolicy = %q", cfg.DefaultOutboundPolicy)
	}
	if cfg.AuthproxyRoutesYAML != "routes: []" {
		t.Errorf("AuthproxyRoutesYAML = %q", cfg.AuthproxyRoutesYAML)
	}
}

func TestReadNamespaceConfig_EmptyNamespace(t *testing.T) {
	reader := newFakeReader() // no objects
	cfg, err := ReadNamespaceConfig(context.Background(), reader, "empty-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All fields should be empty strings
	if cfg.KeycloakURL != "" || cfg.KeycloakRealm != "" || cfg.TokenURL != "" {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

func TestReadNamespaceConfig_PartialConfig(t *testing.T) {
	// Only authbridge-config with subset of keys
	abCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AuthBridgeConfigMapName, Namespace: "ns1"},
		Data: map[string]string{
			"KEYCLOAK_URL":   "http://keycloak:8080",
			"KEYCLOAK_REALM": "rossoctl",
		},
	}

	reader := newFakeReader(abCM)
	cfg, err := ReadNamespaceConfig(context.Background(), reader, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.KeycloakURL != "http://keycloak:8080" {
		t.Errorf("KeycloakURL = %q", cfg.KeycloakURL)
	}
	// Other fields should be empty
	if cfg.TokenURL != "" || cfg.Issuer != "" {
		t.Errorf("expected missing fields to be empty, got tokenURL=%q issuer=%q",
			cfg.TokenURL, cfg.Issuer)
	}
}

func TestExtractMTLSMode(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"empty input", "", ""},
		{"no mtls block", "mode: proxy-sidecar\n", ""},
		{"mtls block, mode strict", "mtls:\n  mode: strict\n", "strict"},
		{"mtls block, mode permissive", "mtls:\n  mode: permissive\n", "permissive"},
		{"mtls block, mode disabled", "mtls:\n  mode: disabled\n", "disabled"},
		{"mtls block with cert paths overridden", "mtls:\n  mode: strict\n  cert_file: /custom/cert.pem\n", "strict"},
		{"mtls block, no mode key", "mtls:\n  cert_file: /opt/svid.pem\n", ""},
		{"mtls and other blocks", "mode: proxy-sidecar\nmtls:\n  mode: permissive\nlistener:\n  reverse_proxy_addr: \":8080\"\n", "permissive"},
		// Malformed YAML produces "" — caller falls through to the next
		// resolution layer. Internal WARN log is logged but not asserted
		// here (slog handler is a global; capturing it adds harness noise).
		{"malformed yaml falls through", "mtls:\n  mode: strict\n  - this is invalid", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractMTLSMode(tt.yaml)
			if got != tt.want {
				t.Errorf("ExtractMTLSMode(%q) = %q, want %q", tt.yaml, got, tt.want)
			}
		})
	}
}
