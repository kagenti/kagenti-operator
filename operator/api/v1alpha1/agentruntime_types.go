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

package v1alpha1

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=agent;tool
type RuntimeType string

const (
	RuntimeTypeAgent RuntimeType = "agent"
	RuntimeTypeTool  RuntimeType = "tool"
)

// +kubebuilder:validation:Enum=mtls;http
type TransportSecurity string

const (
	TransportSecurityMTLS TransportSecurity = "mtls"
	TransportSecurityHTTP TransportSecurity = "http"
)

// TLS-bridge shared contract (used by the injector, the CA reconciler, and the
// validating webhook). Lives in this leaf API package so all three reference one
// source without an import cycle.
const (
	// TLSBridgeModeDisabled / Enabled are the spec.tlsBridgeMode enum values.
	TLSBridgeModeDisabled = "disabled"
	TLSBridgeModeEnabled  = "enabled"
	// TLSBridgeCASecretSuffix names the per-agent CA Secret the reconciler
	// creates and the webhook mounts: "<agentName>" + suffix.
	TLSBridgeCASecretSuffix = "-tls-bridge-ca"
)

// AgentRuntimeSpec defines the desired state of AgentRuntime.
type AgentRuntimeSpec struct {
	// Type classifies the workload as an agent or tool
	Type RuntimeType `json:"type"`

	// TargetRef identifies the workload backing this agent runtime (duck typing).
	TargetRef TargetRef `json:"targetRef"`

	// AuthBridgeMode selects the deployment shape for this workload's
	// authbridge sidecar. When unset, the namespace-level
	// authbridge-runtime-config ConfigMap's mode is used; if that is
	// also unset, the operator falls back to "proxy-sidecar".
	//
	// Four valid values:
	//
	//   proxy-sidecar  HTTP_PROXY env + authbridge-proxy (full plugin
	//                  set, including a2a/mcp/inference parsers) +
	//                  spiffe-helper bundled. No Envoy, no iptables.
	//                  Default mode.
	//   envoy-sidecar  Envoy + ext_proc authbridge + spiffe-helper
	//                  bundled. Requires the proxy-init iptables
	//                  container.
	//   lite           Same listener layout as proxy-sidecar but uses
	//                  the authbridge-lite image (jwt-validation +
	//                  token-exchange only, parsers dropped to shrink
	//                  the binary). For size-constrained deployments
	//                  that don't need protocol-aware abctl events.
	//   waypoint       Standalone deployment, not injected as a
	//                  sidecar. Used by Istio ambient mesh.
	//
	// Set this when a single workload needs a different shape than the
	// namespace default. Most deployments leave it unset and let the
	// namespace ConfigMap drive the choice.
	//
	// +optional
	// +kubebuilder:validation:Enum=proxy-sidecar;envoy-sidecar;lite;waypoint
	AuthBridgeMode string `json:"authBridgeMode,omitempty"`

	// MTLSMode selects the mTLS posture between authbridge sidecars on
	// the proxy-sidecar / lite paths. envoy-sidecar handles transport
	// security through Envoy SDS, which is currently not configured by
	// the rossoctl envoy-config — admission rejects mtlsMode != disabled
	// when authBridgeMode is envoy-sidecar (tracked as a follow-up).
	//
	// Three valid values:
	//
	//   disabled    Plaintext between sidecars.
	//   permissive  (default) Inbound: byte-peek listener accepts both TLS and
	//               plaintext on the same port. Outbound: tries TLS,
	//               falls back to plaintext on handshake failure (one-line
	//               WARN log per fallback). Use during rollout.
	//   strict      Inbound: TLS-only, plaintext callers closed at
	//               accept. Outbound: TLS-or-fail. Use after rollout
	//               completes.
	//
	// Resolution: AgentRuntime CR > namespace authbridge-runtime-config
	// mtls.mode > "permissive". Setting mtlsMode != disabled implicitly
	// requires SPIRE — the operator auto-enables spire for the workload.
	//
	// CR-empty vs CR="disabled" are observably different in
	// `kubectl get agentruntime -o yaml` (the former omits the field,
	// the latter shows mtlsMode: disabled) but produce the same
	// effective mode: empty falls through to the namespace ConfigMap,
	// "disabled" is an explicit override that pins mode off even when
	// the namespace default is non-disabled.
	//
	// Note: changing mtlsMode triggers a pod rollout because authbridge
	// cannot hot-reload mTLS config (the byte-peek listener is wired at
	// process start).
	//
	// +optional
	// +kubebuilder:default=permissive
	// +kubebuilder:validation:Enum=disabled;permissive;strict
	MTLSMode string `json:"mtlsMode,omitempty"`

	// TLSBridgeMode controls AuthBridge's outbound TLS bridge (decrypt agent
	// egress HTTPS into the pipeline). Only honored for authBridgeMode
	// proxy-sidecar or lite; rejected with envoy-sidecar. Requires cert-manager.
	// +optional
	// +kubebuilder:default=disabled
	// +kubebuilder:validation:Enum=disabled;enabled
	TLSBridgeMode string `json:"tlsBridgeMode,omitempty"`

	// EgressEnforcement controls whether the proxy-init init container is
	// injected for fail-closed egress capture in proxy-sidecar / lite modes.
	//
	// Values:
	//   enforce-redirect (default) — proxy-init is injected with iptables
	//                     rules that transparently REDIRECT egress bypassing
	//                     HTTP_PROXY to AuthBridge's transparent listener.
	//                     Requires NET_ADMIN capability and a kernel that
	//                     supports iptables (legacy or nft).
	//   none             — proxy-init is NOT injected. Egress enforcement
	//                     relies on HTTP_PROXY (cooperative) + inbound
	//                     AuthBridge on destinations + NetworkPolicy.
	//                     Use on platforms where iptables is unavailable
	//                     (e.g. ROSA HCP, managed OpenShift).
	//
	// Resolution: AgentRuntime CR > namespace authbridge-runtime-config
	// egressEnforcement field > "enforce-redirect" (default).
	//
	// Does not affect envoy-sidecar mode, which always uses proxy-init
	// for its structural iptables redirect.
	//
	// +optional
	// +kubebuilder:validation:Enum=enforce-redirect;none
	EgressEnforcement string `json:"egressEnforcement,omitempty"`

	// Auth configures SPIFFE-based authentication and token exchange for
	// outbound requests. When set with mode: federated-jwt, the operator
	// configures AuthBridge to perform token exchange when calling the
	// specified destinations, requesting the appropriate audiences.
	//
	// +optional
	Auth *AuthConfig `json:"auth,omitempty"`

	// PluginPreset selects an AuthBridge layer-3 plugin-pipeline preset for this
	// workload's per-agent authbridge-config-<name> ConfigMap. When set, the
	// admission webhook renders the full canonical pipeline (all supported plugins
	// in fixed order; unselected ones emitted with on_error: off) instead of the
	// default two-plugin (jwt-validation + token-exchange) synthesis.
	//
	// Presets (membership; every preset seeds its plugins at policy "enforce"):
	//   auth-only  inbound: jwt-validation            outbound: token-exchange
	//   ibac-only  inbound: a2a-parser                outbound: inference-parser, mcp-parser, ibac
	//   full       inbound: a2a-parser, jwt-validation outbound: token-exchange,
	//              inference-parser, mcp-parser, ibac
	//
	// Only honored on the proxy-sidecar / lite paths (the plugin pipeline lives in
	// the per-agent ConfigMap those modes mount). Requires the AuthBridge sidecar to
	// be injected.
	//
	// +optional
	// +kubebuilder:validation:Enum=auth-only;ibac-only;full
	PluginPreset string `json:"pluginPreset,omitempty"`

	// Plugins carries per-plugin policy overrides layered on top of PluginPreset, as
	// "NAME:POLICY" tokens (e.g. "ibac:observe"). POLICY is one of enforce|observe|off
	// and maps to the plugin entry's on_error field: enforce omits on_error (the
	// framework default), observe sets on_error: observe, off sets on_error: off.
	// A plugin named here that isn't part of the preset is added at the given policy.
	// Ignored when PluginPreset is unset.
	//
	// token-exchange and token-broker are mutually exclusive on the outbound chain;
	// a spec that activates both is rejected by admission.
	//
	// +optional
	Plugins []string `json:"plugins,omitempty"`

	// OnError sets the chain-default policy applied to every selected plugin that
	// has no explicit per-plugin override in Plugins. enforce is the framework
	// default (on_error omitted); observe/off are emitted explicitly. Ignored when
	// PluginPreset is unset.
	//
	// +optional
	// +kubebuilder:validation:Enum=enforce;observe;off
	OnError string `json:"onError,omitempty"`
}

// AuthConfig defines authentication configuration for an agent or tool.
type AuthConfig struct {
	// Outbound defines token exchange routes for calling other services.
	// Each route tells AuthBridge which audiences to request when calling
	// a specific destination.
	//
	// Routes are only effective when the namespace is configured with
	// SPIFFE authentication (authBridge.clientAuthType: federated-jwt).
	// The authentication mode is set globally at the namespace level, not
	// per-agent.
	//
	// +optional
	Outbound []OutboundRoute `json:"outbound,omitempty"`
}

// OutboundRoute defines a token exchange route for a specific destination.
type OutboundRoute struct {
	// Destination specifies which service this route matches.
	Destination RouteMatch `json:"destination"`

	// Audiences lists the SPIFFE IDs to request in the token's audience claim.
	// Typically includes the SPIFFE ID of the target service.
	//
	// Example: ["spiffe://localtest.me/ns/team1/sa/weather-tool"]
	//
	// +kubebuilder:validation:MinItems=1
	Audiences []string `json:"audiences"`
}

// RouteMatch defines how to match an outbound destination.
type RouteMatch struct {
	// Host is an exact hostname to match.
	// Example: "weather-tool-mcp.team1.svc.cluster.local"
	//
	// +optional
	Host string `json:"host,omitempty"`

	// HostRegex is a regex pattern to match hostnames.
	// Example: ".*\\.team1\\.svc\\.cluster\\.local"
	//
	// +optional
	HostRegex string `json:"hostRegex,omitempty"`
}

// SupportedAuthBridgePlugins is the set of plugin names accepted in
// AgentRuntimeSpec.Plugins override tokens. Kept in this leaf API package so
// both the validating webhook and the injector's pipeline renderer reference
// one list without an import cycle. Keep in sync with the injector's canonical
// inbound/outbound order lists (internal/webhook/injector/preset_pipeline.go).
var SupportedAuthBridgePlugins = map[string]bool{
	"a2a-parser":       true,
	"jwt-validation":   true,
	"token-exchange":   true,
	"token-broker":     true,
	"inference-parser": true,
	"mcp-parser":       true,
	"ibac":             true,
}

// supportedAuthBridgePolicies is the legal per-plugin / chain-default policy set.
var supportedAuthBridgePolicies = map[string]bool{"enforce": true, "observe": true, "off": true}

// ValidatePlugins checks the spec.plugins override tokens ("NAME:POLICY") for a
// known plugin name and a valid policy. No-op when pluginPreset is unset (the
// preset gates whether Plugins is honored). The token-exchange/token-broker
// mutex is enforced at render time, where preset membership is resolved.
func (s *AgentRuntimeSpec) ValidatePlugins() error {
	if s.PluginPreset == "" {
		return nil
	}
	for _, tok := range s.Plugins {
		t := strings.TrimSpace(tok)
		if t == "" {
			return fmt.Errorf("empty plugin override token in spec.plugins")
		}
		name := t
		policy := "enforce"
		if idx := strings.Index(t, ":"); idx >= 0 {
			name = strings.TrimSpace(t[:idx])
			policy = strings.TrimSpace(t[idx+1:])
		}
		if !SupportedAuthBridgePlugins[name] {
			return fmt.Errorf("unknown plugin %q in spec.plugins token %q", name, tok)
		}
		if !supportedAuthBridgePolicies[policy] {
			return fmt.Errorf("invalid policy %q in spec.plugins token %q (want enforce, observe, or off)", policy, tok)
		}
	}
	return nil
}

// CardStatus holds the fetched A2A agent card data along with fetch metadata
// and optional verification results. Populated by the card discovery phase when
// --enable-card-discovery is set.
type CardStatus struct {
	AgentCardData `json:",inline"`

	// LastCardFetchTime is the timestamp of the last successful card fetch.
	// +optional
	LastCardFetchTime *metav1.Time `json:"lastCardFetchTime,omitempty"`

	// CardHash is a SHA-256 content hash of the fetched card data.
	// +optional
	CardHash string `json:"cardHash,omitempty"`

	// Protocol is the detected agent protocol (e.g., "a2a").
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// TransportSecurity indicates the transport layer used for the card fetch.
	// +optional
	TransportSecurity TransportSecurity `json:"transportSecurity,omitempty"`

	// ValidSignature is the result of JWS signature verification.
	// +optional
	ValidSignature *bool `json:"validSignature,omitempty"`

	// SignatureKeyID is the key ID from the verified JWS header.
	// +optional
	SignatureKeyID string `json:"signatureKeyID,omitempty"`

	// SignatureVerificationDetails contains details or errors from signature verification.
	// +optional
	SignatureVerificationDetails string `json:"signatureVerificationDetails,omitempty"`

	// AttestedAgentSpiffeID is the SPIFFE ID extracted from the mTLS peer certificate.
	// +optional
	AttestedAgentSpiffeID string `json:"attestedAgentSpiffeID,omitempty"`
}

// AgentRuntimeStatus defines the observed state of AgentRuntime.
type AgentRuntimeStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ConfiguredPods is the count of pods with expected labels/config
	// +optional
	ConfiguredPods int32 `json:"configuredPods,omitempty"`

	// Card holds A2A agent card data discovered from the workload's Service endpoint.
	// +optional
	Card *CardStatus `json:"card,omitempty"`

	// LinkedSkills lists skill names discovered from the rossoctl.io/skills
	// annotation on the target workload. This annotation is set by the
	// rossoctl backend (PR #1440) or manually by the user. The operator
	// reads but never sets this annotation.
	// +optional
	LinkedSkills []string `json:"linkedSkills,omitempty"`

	// Conditions represent the current state of the AgentRuntime
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=art;agentrt
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type",description="Workload Type"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetRef.name",description="Target Workload"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Ready Status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentRuntime attaches runtime configuration to a backing workload classified as an
// agent or tool. The controller reports pod configuration coverage and phase in status.
type AgentRuntime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentRuntimeSpec   `json:"spec"`
	Status AgentRuntimeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRuntimeList contains a list of AgentRuntime.
type AgentRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRuntime `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRuntime{}, &AgentRuntimeList{})
}
