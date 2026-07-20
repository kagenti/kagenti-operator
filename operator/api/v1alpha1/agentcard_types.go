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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentCardSpec defines the desired state of AgentCard.
type AgentCardSpec struct {
	// SyncPeriod is how often to re-fetch the agent card (e.g., "30s", "5m")
	// +optional
	// +kubebuilder:default="30s"
	SyncPeriod string `json:"syncPeriod,omitempty"`

	// TargetRef identifies the workload backing this agent (duck typing).
	// The workload must have the rossoctl.io/type=agent label.
	// +optional
	TargetRef *TargetRef `json:"targetRef,omitempty"`

	// IdentityBinding specifies SPIFFE identity binding configuration
	// +optional
	IdentityBinding *IdentityBinding `json:"identityBinding,omitempty"`

	// SigstoreVerification optionally overrides operator-level Sigstore identity
	// constraints for supply-chain bundle verification on SignedAgentCard documents.
	// +optional
	SigstoreVerification *SigstoreVerification `json:"sigstoreVerification,omitempty"`
}

// SigstoreVerification configures expected Fulcio certificate identity for Sigstore bundle verification.
type SigstoreVerification struct {
	// CertificateIdentity is the expected OIDC subject in the Fulcio certificate SAN (exact match unless paired with regex elsewhere).
	// +optional
	CertificateIdentity string `json:"certificateIdentity,omitempty"`

	// CertificateOIDCIssuer is the expected OIDC issuer URL (e.g. https://token.actions.githubusercontent.com).
	// +optional
	CertificateOIDCIssuer string `json:"certificateOIDCIssuer,omitempty"`
}

// IdentityBinding configures workload identity binding for an AgentCard.
// The SPIFFE ID is extracted from the leaf certificate SAN URI in the x5c chain.
// Binding validates that the SPIFFE ID belongs to the configured trust domain.
type IdentityBinding struct {
	// TrustDomain overrides the operator-level --spire-trust-domain for this AgentCard.
	// If empty, the operator flag value is used.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([a-zA-Z0-9\-\.]*[a-zA-Z0-9])?$`
	TrustDomain string `json:"trustDomain,omitempty"`

	// Strict controls whether binding failures trigger enforcement actions
	// (label removal, restrictive NetworkPolicy).
	// When true, binding failure removes the verified label and applies restrictive NetworkPolicy.
	// When false (default), binding results are recorded in status only;
	// the workload retains its verified label and permissive policy.
	// +optional
	// +kubebuilder:default=false
	Strict bool `json:"strict,omitempty"`
}

// TargetRef identifies a workload backing this agent via duck typing.
type TargetRef struct {
	// APIVersion is the API version of the target resource (e.g., "apps/v1")
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind is the kind of the target resource (e.g., "Deployment", "StatefulSet")
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AgentCardStatus defines the observed state of AgentCard.
type AgentCardStatus struct {
	// Card contains the cached agent card data
	// +optional
	Card *AgentCardData `json:"card,omitempty"`

	// Conditions represent the current state of the indexing process
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastSyncTime is when the agent card was last successfully fetched
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// Protocol is the detected agent protocol (e.g., "a2a")
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// TargetRef contains the resolved reference to the backing workload.
	// This is populated after the controller successfully locates the workload.
	// +optional
	TargetRef *TargetRef `json:"targetRef,omitempty"`

	// ValidSignature indicates if the agent card signature was validated
	// +optional
	ValidSignature *bool `json:"validSignature,omitempty"`

	// SignatureVerificationDetails contains details about the last signature verification
	// +optional
	SignatureVerificationDetails string `json:"signatureVerificationDetails,omitempty"`

	// SignatureKeyID is the key ID used for verification (from JWS protected header kid)
	// +optional
	SignatureKeyID string `json:"signatureKeyId,omitempty"`

	// SignatureSpiffeID is the SPIFFE ID from the leaf certificate SAN URI (set only when valid).
	// +optional
	SignatureSpiffeID string `json:"signatureSpiffeId,omitempty"`

	// SignatureIdentityMatch is true when both signature and identity binding pass.
	// +optional
	SignatureIdentityMatch *bool `json:"signatureIdentityMatch,omitempty"`

	// CardId is the SHA-256 hash of the card content for drift detection.
	// +optional
	CardId string `json:"cardId,omitempty"`

	// AttestedAgentSpiffeID is the SPIFFE ID extracted from the agent's TLS peer certificate
	// during authenticated (mTLS) fetch. Set only when verifiedFetch is enabled and successful.
	// +optional
	AttestedAgentSpiffeID string `json:"attestedAgentSpiffeId,omitempty"`

	// ExpectedSpiffeID is the SPIFFE ID used for binding evaluation.
	// +optional
	ExpectedSpiffeID string `json:"expectedSpiffeID,omitempty"`

	// BindingStatus contains the result of identity binding evaluation
	// +optional
	BindingStatus *BindingStatus `json:"bindingStatus,omitempty"`

	// SigstoreBundleVerified is true when SignedAgentCard attestations.signatureBundle verifies successfully.
	// +optional
	SigstoreBundleVerified *bool `json:"sigstoreBundleVerified,omitempty"`

	// SigstoreIdentity is the verified Fulcio signing identity when Sigstore verification succeeds.
	// +optional
	SigstoreIdentity string `json:"sigstoreIdentity,omitempty"`

	// RekorLogIndex is the transparency log index when available from the bundle.
	// +optional
	RekorLogIndex string `json:"rekorLogIndex,omitempty"`

	// SLSARepository from provenanceBundle when present and parsed.
	// +optional
	SLSARepository string `json:"slsaRepository,omitempty"`

	// SLSACommitSHA from provenanceBundle when present and parsed.
	// +optional
	SLSACommitSHA string `json:"slsaCommitSHA,omitempty"`
}

// BindingStatus represents the result of identity binding evaluation
type BindingStatus struct {
	// Bound indicates whether the verified SPIFFE ID belongs to the configured trust domain
	Bound bool `json:"bound"`

	// Reason is a machine-readable reason for the binding status
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable description of the binding status
	// +optional
	Message string `json:"message,omitempty"`

	// LastEvaluationTime is when the binding was last evaluated
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`
}

// AgentCardData represents the A2A agent card structure.
// Based on the A2A specification.
type AgentCardData struct {
	// A human-readable name for the agent.
	// +optional
	Name string `json:"name,omitempty"`

	// A human-readable description of the agent, assisting users and other
	// agents in understanding its purpose.
	// +optional
	Description string `json:"description,omitempty"`

	// The version of the agent.
	// +optional
	Version string `json:"version,omitempty"`

	// The URL of the agent's endpoint.
	// +optional
	URL string `json:"url,omitempty"`

	// A URL providing additional documentation about the agent.
	// +optional
	DocumentationURL string `json:"documentationUrl,omitempty"`

	// A URL to an icon for the agent.
	// +optional
	IconURL string `json:"iconUrl,omitempty"`

	// The service provider of the agent.
	// +optional
	Provider *AgentProvider `json:"provider,omitempty"`

	// The A2A capability set supported by the agent.
	// +optional
	Capabilities *AgentCapabilities `json:"capabilities,omitempty"`

	// The set of interaction modes that the agent supports across all skills,
	// defined as media types.
	// +optional
	DefaultInputModes []string `json:"defaultInputModes,omitempty"`

	// The media types supported as outputs from this agent.
	// +optional
	DefaultOutputModes []string `json:"defaultOutputModes,omitempty"`

	// Skills represent the abilities of an agent. A skill is a focused set of
	// behaviors that the agent is likely to succeed at.
	// +optional
	Skills []AgentSkill `json:"skills,omitempty"`

	// Indicates if the agent supports providing an extended agent card when
	// authenticated.
	// +optional
	SupportsAuthenticatedExtendedCard *bool `json:"supportsAuthenticatedExtendedCard,omitempty"`

	// JWS signatures per A2A spec §8.4.2.
	// +optional
	Signatures []AgentCardSignature `json:"signatures,omitempty"`
}

// AgentCardSignature represents a JWS signature on an AgentCard (A2A spec §8.4.2).
type AgentCardSignature struct {
	// Protected is the base64url-encoded JWS protected header (contains alg, kid, x5c).
	// +required
	Protected string `json:"protected"`

	// Signature is the base64url-encoded JWS signature value.
	// +required
	Signature string `json:"signature"`

	// Header contains optional unprotected JWS header parameters.
	// +optional
	Header *SignatureHeader `json:"header,omitempty"`
}

// SignatureHeader contains unprotected JWS header parameters.
type SignatureHeader struct {
	// Timestamp is when the signature was created (ISO 8601 string)
	// +optional
	Timestamp string `json:"timestamp,omitempty"`
}

// AgentProvider describes the service provider of the agent.
type AgentProvider struct {
	// The name of the agent provider's organization.
	// +optional
	Organization string `json:"organization,omitempty"`

	// A URL for the agent provider's website or relevant documentation.
	// +optional
	URL string `json:"url,omitempty"`
}

// AgentExtension describes an A2A protocol extension supported by the agent.
type AgentExtension struct {
	// The unique URI identifying the extension.
	// +optional
	URI string `json:"uri,omitempty"`

	// A human-readable description of how this agent uses the extension.
	// +optional
	Description string `json:"description,omitempty"`

	// If true, the client must understand and comply with the extension's
	// requirements.
	// +optional
	Required *bool `json:"required,omitempty"`

	// Extension-specific configuration parameters.
	// +optional
	Params map[string]apiextensionsv1.JSON `json:"params,omitempty"`
}

// AgentCapabilities defines the A2A capability set supported by the agent.
type AgentCapabilities struct {
	// Indicates if the agent supports streaming responses.
	// +optional
	Streaming *bool `json:"streaming,omitempty"`

	// Indicates if the agent supports sending push notifications for
	// asynchronous task updates.
	// +optional
	PushNotifications *bool `json:"pushNotifications,omitempty"`

	// A list of protocol extensions supported by the agent.
	// +optional
	Extensions []AgentExtension `json:"extensions,omitempty"`
}

// AgentSkill represents a skill offered by the agent.
type AgentSkill struct {
	// A unique identifier for the agent's skill.
	// +optional
	ID string `json:"id,omitempty"`

	// A human-readable name for the skill.
	// +optional
	Name string `json:"name,omitempty"`

	// A detailed description of the skill.
	// +optional
	Description string `json:"description,omitempty"`

	// A set of keywords describing the skill's capabilities.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// Example prompts or scenarios that this skill can handle.
	// +optional
	Examples []string `json:"examples,omitempty"`

	// The set of supported input media types for this skill, overriding the
	// agent's defaults.
	// +optional
	InputModes []string `json:"inputModes,omitempty"`

	// The set of supported output media types for this skill, overriding the
	// agent's defaults.
	// +optional
	OutputModes []string `json:"outputModes,omitempty"`

	// The parameters accepted by this skill.
	// +optional
	Parameters []SkillParameter `json:"parameters,omitempty"`
}

// SkillParameter defines a parameter accepted by a skill.
type SkillParameter struct {
	// The name of the parameter.
	// +optional
	Name string `json:"name,omitempty"`

	// The type of the parameter (e.g., "string", "number", "boolean", "object", "array").
	// +optional
	Type string `json:"type,omitempty"`

	// A human-readable description of the parameter.
	// +optional
	Description string `json:"description,omitempty"`

	// Indicates if this parameter must be provided.
	// +optional
	Required *bool `json:"required,omitempty"`

	// The default value for this parameter.
	// +optional
	Default string `json:"default,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=agentcards;cards
// +kubebuilder:printcolumn:name="Protocol",type="string",JSONPath=".status.protocol",description="Agent Protocol"
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".status.targetRef.kind",description="Workload Kind"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".status.targetRef.name",description="Target Workload"
// +kubebuilder:printcolumn:name="Agent",type="string",JSONPath=".status.card.name",description="Agent Name"
// +kubebuilder:printcolumn:name="Verified",type="string",JSONPath=".status.conditions[?(@.type=='Verified')].status",description="Identity Verified"
// +kubebuilder:printcolumn:name="Sigstore",type="boolean",JSONPath=".status.sigstoreBundleVerified",description="Sigstore Bundle Verified"
// +kubebuilder:printcolumn:name="Bound",type="boolean",JSONPath=".status.bindingStatus.bound",description="Identity Bound"
// +kubebuilder:printcolumn:name="Synced",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status",description="Sync Status"
// +kubebuilder:printcolumn:name="LastSync",type="date",JSONPath=".status.lastSyncTime",description="Last Sync Time"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="AttestedAgent",type="string",JSONPath=".status.attestedAgentSpiffeId",description="Attested Agent SPIFFE ID",priority=1

// AgentCard binds an A2A agent card to a backing workload. The controller periodically
// fetches the card from the referenced workload, verifies its JWS signature and SPIFFE
// identity against the configured trust domain, and caches the result in status.
type AgentCard struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentCardSpec   `json:"spec,omitempty"`
	Status AgentCardStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentCardList contains a list of AgentCard.
type AgentCardList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentCard `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentCard{}, &AgentCardList{})
}
