package injector

import (
	"testing"

	"github.com/rossoctl/operator/internal/webhook/config"
)

func allEnabledGates() *config.FeatureGates {
	return config.DefaultFeatureGates()
}

func noLabels() map[string]string {
	return map[string]string{}
}

func TestPrecedenceEvaluator(t *testing.T) {
	tests := []struct {
		name             string
		featureGates     *config.FeatureGates
		workloadLabels   map[string]string
		expectEnvoy      bool
		expectProxyInit  bool
		expectSpiffe     bool
		expectEnvoyLayer string
	}{
		// === Per-sidecar feature gate ===
		{
			name: "envoy gate off - envoy and proxy-init skipped",
			featureGates: &config.FeatureGates{
				GlobalEnabled: true,
				EnvoyProxy:    false,
			},
			workloadLabels:   noLabels(),
			expectEnvoy:      false,
			expectProxyInit:  false, // follows envoy
			expectSpiffe:     true,  // spiffe gate is implicit-true
			expectEnvoyLayer: "feature-gate",
		},

		// === Workload label ===
		{
			name:             "envoy label false - envoy+proxy-init skipped",
			featureGates:     allEnabledGates(),
			workloadLabels:   map[string]string{LabelEnvoyProxyInject: "false"},
			expectEnvoy:      false,
			expectProxyInit:  false,
			expectSpiffe:     true,
			expectEnvoyLayer: "workload-label",
		},
		{
			name:            "spiffe label false - spiffe skipped, envoy unaffected",
			featureGates:    allEnabledGates(),
			workloadLabels:  map[string]string{LabelSpiffeHelperInject: "false"},
			expectEnvoy:     true,
			expectProxyInit: true,
			expectSpiffe:    false,
		},
		{
			name:             "no labels - envoy+spiffe injected",
			featureGates:     allEnabledGates(),
			workloadLabels:   noLabels(),
			expectEnvoy:      true,
			expectProxyInit:  true,
			expectSpiffe:     true,
			expectEnvoyLayer: "default",
		},
		{
			name:         "all opt-out labels set - all skipped",
			featureGates: allEnabledGates(),
			workloadLabels: map[string]string{
				LabelEnvoyProxyInject:   "false",
				LabelSpiffeHelperInject: "false",
			},
			expectEnvoy:      false,
			expectProxyInit:  false,
			expectSpiffe:     false,
			expectEnvoyLayer: "workload-label",
		},

		// === Precedence ordering: gate beats workload label ===
		{
			name: "envoy gate off + label true - gate wins",
			featureGates: &config.FeatureGates{
				GlobalEnabled: true,
				EnvoyProxy:    false,
			},
			workloadLabels:   map[string]string{LabelEnvoyProxyInject: "true"},
			expectEnvoy:      false,
			expectProxyInit:  false,
			expectSpiffe:     true,
			expectEnvoyLayer: "feature-gate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := NewPrecedenceEvaluator(tt.featureGates)
			decision := evaluator.Evaluate(tt.workloadLabels)

			if decision.EnvoyProxy.Inject != tt.expectEnvoy {
				t.Errorf("EnvoyProxy.Inject = %v, want %v (reason: %s, layer: %s)",
					decision.EnvoyProxy.Inject, tt.expectEnvoy,
					decision.EnvoyProxy.Reason, decision.EnvoyProxy.Layer)
			}
			if decision.ProxyInit.Inject != tt.expectProxyInit {
				t.Errorf("ProxyInit.Inject = %v, want %v (reason: %s, layer: %s)",
					decision.ProxyInit.Inject, tt.expectProxyInit,
					decision.ProxyInit.Reason, decision.ProxyInit.Layer)
			}
			if decision.SpiffeHelper.Inject != tt.expectSpiffe {
				t.Errorf("SpiffeHelper.Inject = %v, want %v (reason: %s, layer: %s)",
					decision.SpiffeHelper.Inject, tt.expectSpiffe,
					decision.SpiffeHelper.Reason, decision.SpiffeHelper.Layer)
			}
			if tt.expectEnvoyLayer != "" && decision.EnvoyProxy.Layer != tt.expectEnvoyLayer {
				t.Errorf("EnvoyProxy.Layer = %q, want %q", decision.EnvoyProxy.Layer, tt.expectEnvoyLayer)
			}
		})
	}
}

func TestAnyInjected(t *testing.T) {
	tests := []struct {
		name     string
		decision InjectionDecision
		want     bool
	}{
		{
			name: "envoy + spiffe injected",
			decision: InjectionDecision{
				EnvoyProxy:   SidecarDecision{Inject: true},
				SpiffeHelper: SidecarDecision{Inject: true},
			},
			want: true,
		},
		{
			name: "only envoy injected",
			decision: InjectionDecision{
				EnvoyProxy:   SidecarDecision{Inject: true},
				SpiffeHelper: SidecarDecision{Inject: false},
			},
			want: true,
		},
		{
			name: "none injected",
			decision: InjectionDecision{
				EnvoyProxy:   SidecarDecision{Inject: false},
				SpiffeHelper: SidecarDecision{Inject: false},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.decision.AnyInjected(); got != tt.want {
				t.Errorf("AnyInjected() = %v, want %v", got, tt.want)
			}
		})
	}
}
