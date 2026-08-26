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

package injector

import (
	"context"
	"testing"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/webhook/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

// TestEnsurePerAgentConfigMap_WithAuthRoutes verifies that when an AgentRuntime
// has spec.auth.mode=federated-jwt and spec.auth.outbound routes, those routes
// are injected into the token-exchange plugin config in the per-agent ConfigMap.
func TestEnsurePerAgentConfigMap_WithAuthRoutes(t *testing.T) {
	ctx := context.Background()

	// Create an AgentRuntime with spec.auth configured
	agentRuntime := &agentv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "weather-agent-runtime",
			Namespace: "team1",
		},
		Spec: agentv1alpha1.AgentRuntimeSpec{
			Type: agentv1alpha1.RuntimeTypeAgent,
			TargetRef: agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "weather-agent",
			},
			Auth: &agentv1alpha1.AuthConfig{
				Outbound: []agentv1alpha1.OutboundRoute{
					{
						Destination: agentv1alpha1.RouteMatch{
							Host: "weather-tool-mcp.team1.svc.cluster.local",
						},
						Audiences: []string{
							"spiffe://localtest.me/ns/team1/sa/weather-tool",
						},
					},
					{
						Destination: agentv1alpha1.RouteMatch{
							HostRegex: `.*\.team1\.svc\.cluster\.local`,
						},
						Audiences: []string{
							"spiffe://localtest.me/ns/team1/sa/default",
						},
					},
				},
			},
		},
	}

	// Create a fake client with the AgentRuntime scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agentRuntime).
		Build()

	// Create PodMutator
	m := NewPodMutator(
		fakeClient,
		fakeClient,
		func() *config.PlatformConfig {
			return &config.PlatformConfig{
				Spiffe: config.SpiffeConfig{
					SocketPath: "/run/spire/sockets/agent.sock",
				},
			}
		},
		func() *config.FeatureGates {
			return &config.FeatureGates{
				GlobalEnabled: true,
			}
		},
	)

	// Base YAML with pipeline structure (namespace config)
	baseYAML := `
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
        config:
          issuer: "http://keycloak.localtest.me:8080/realms/rossoctl"
  outbound:
    plugins:
      - name: token-exchange
        config:
          keycloak_url: "http://keycloak-service.keycloak.svc:8080"
          keycloak_realm: "rossoctl"
          default_policy: "passthrough"
          identity:
            type: "spiffe"
`

	nsConfig := &NamespaceConfig{
		KeycloakURL:           "http://keycloak-service.keycloak.svc:8080",
		KeycloakRealm:         "rossoctl",
		ClientAuthType:        "federated-jwt",
		DefaultOutboundPolicy: "passthrough",
	}

	// Call ensurePerAgentConfigMap with the AgentRuntime
	cmName, routesCMName, err := m.ensurePerAgentConfigMap(ctx, "team1", "weather-agent",
		ModeProxySidecar, baseYAML, nsConfig, nil, "", "", true, agentRuntime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fetch the created ConfigMap
	cm := &corev1.ConfigMap{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "team1", Name: cmName}, cm); err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	// Parse the config.yaml
	configYAML, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatal("ConfigMap missing config.yaml key")
	}

	var cfg map[string]interface{}
	if err := yaml.Unmarshal([]byte(configYAML), &cfg); err != nil {
		t.Fatalf("failed to parse config.yaml: %v", err)
	}

	// Navigate to pipeline.outbound.plugins[token-exchange].config.routes
	pipeline, ok := cfg["pipeline"].(map[string]interface{})
	if !ok {
		t.Fatal("missing pipeline block")
	}

	outbound, ok := pipeline["outbound"].(map[string]interface{})
	if !ok {
		t.Fatal("missing outbound block")
	}

	plugins, ok := outbound["plugins"].([]interface{})
	if !ok || len(plugins) == 0 {
		t.Fatal("missing or empty plugins array")
	}

	var tokenExchangeConfig map[string]interface{}
	for _, p := range plugins {
		plugin, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if plugin["name"] == "token-exchange" {
			tokenExchangeConfig, _ = plugin["config"].(map[string]interface{})
			break
		}
	}

	if tokenExchangeConfig == nil {
		t.Fatal("token-exchange plugin not found")
	}

	// Verify routes is a file reference, not an inline array
	routesRef, ok := tokenExchangeConfig["routes"].(map[string]interface{})
	if !ok {
		t.Fatal("routes not found or not a map")
	}
	if routesRef["file"] != "/etc/authproxy/routes.yaml" {
		t.Errorf("routes file mismatch: got %v, want /etc/authproxy/routes.yaml", routesRef["file"])
	}

	// Fetch the routes ConfigMap
	if routesCMName == "" {
		t.Fatal("routesCMName is empty")
	}
	routesCM := &corev1.ConfigMap{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "team1", Name: routesCMName}, routesCM); err != nil {
		t.Fatalf("failed to get routes ConfigMap: %v", err)
	}

	// Parse routes.yaml
	routesYAML, ok := routesCM.Data["routes.yaml"]
	if !ok {
		t.Fatal("routes ConfigMap missing routes.yaml key")
	}

	var routes []interface{}
	if err := yaml.Unmarshal([]byte(routesYAML), &routes); err != nil {
		t.Fatalf("failed to parse routes.yaml: %v", err)
	}

	// Verify we have 2 routes
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}

	// Verify first route (exact host match) - now in flat format
	route1, _ := routes[0].(map[string]interface{})
	if route1["host"] != "weather-tool-mcp.team1.svc.cluster.local" {
		t.Errorf("route 1 host mismatch: got %v", route1["host"])
	}
	if route1["target_audience"] != "spiffe://localtest.me/ns/team1/sa/weather-tool" {
		t.Errorf("route 1 target_audience mismatch: got %v", route1["target_audience"])
	}

	// Verify second route (regex match) - now in flat format
	route2, _ := routes[1].(map[string]interface{})
	if route2["host"] != `.*\.team1\.svc\.cluster\.local` {
		t.Errorf("route 2 host mismatch: got %v", route2["host"])
	}
	if route2["target_audience"] != "spiffe://localtest.me/ns/team1/sa/default" {
		t.Errorf("route 2 target_audience mismatch: got %v", route2["target_audience"])
	}
}

// TestEnsurePerAgentConfigMap_NoAuth verifies that when AgentRuntime is nil
// or has no spec.auth, no routes are injected (existing behavior preserved).
func TestEnsurePerAgentConfigMap_NoAuth(t *testing.T) {
	ctx := context.Background()

	fakeClient := fake.NewClientBuilder().Build()

	m := NewPodMutator(
		fakeClient,
		fakeClient,
		func() *config.PlatformConfig {
			return &config.PlatformConfig{
				Spiffe: config.SpiffeConfig{
					SocketPath: "/run/spire/sockets/agent.sock",
				},
			}
		},
		func() *config.FeatureGates {
			return &config.FeatureGates{
				GlobalEnabled: true,
			}
		},
	)

	baseYAML := `
pipeline:
  outbound:
    plugins:
      - name: token-exchange
        config:
          default_policy: "passthrough"
`

	nsConfig := &NamespaceConfig{}

	// Call with nil agentRuntime
	cmName, _, err := m.ensurePerAgentConfigMap(ctx, "team1", "my-agent",
		ModeProxySidecar, baseYAML, nsConfig, nil, "", "", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fetch the ConfigMap
	cm := &corev1.ConfigMap{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "team1", Name: cmName}, cm); err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	configYAML, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatal("ConfigMap missing config.yaml key")
	}

	var cfg map[string]interface{}
	if err := yaml.Unmarshal([]byte(configYAML), &cfg); err != nil {
		t.Fatalf("failed to parse config.yaml: %v", err)
	}

	// Verify no routes were added
	pipeline, _ := cfg["pipeline"].(map[string]interface{})
	if pipeline != nil {
		outbound, _ := pipeline["outbound"].(map[string]interface{})
		if outbound != nil {
			plugins, _ := outbound["plugins"].([]interface{})
			for _, p := range plugins {
				plugin, _ := p.(map[string]interface{})
				if plugin["name"] == "token-exchange" {
					config, _ := plugin["config"].(map[string]interface{})
					if routes, ok := config["routes"]; ok && routes != nil {
						t.Error("routes should not be present when agentRuntime is nil")
					}
				}
			}
		}
	}
}
