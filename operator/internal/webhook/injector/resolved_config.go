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
	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/webhook/config"
)

// ResolvedConfig is the fully-merged configuration for a single workload injection.
// It combines PlatformConfig (images, ports, resources) with namespace ConfigMap values.
type ResolvedConfig struct {
	// Platform config (images, ports, resources) — from PlatformConfig
	Platform *config.PlatformConfig

	// Identity — from namespace CMs
	KeycloakURL                string
	KeycloakRealm              string
	AdminCredentialsSecretName string // Secret name for KEYCLOAK_ADMIN_USERNAME/PASSWORD (default: "keycloak-admin-secret")
	SpiffeTrustDomain          string
	PlatformClientIDs          string

	// Token exchange — from namespace CMs
	TokenURL              string
	Issuer                string
	ExpectedAudience      string
	TargetAudience        string
	TargetScopes          string
	DefaultOutboundPolicy string
	ClientAuthType        string // "client-secret" or "federated-jwt"
	SpiffeIdpAlias        string // Keycloak SPIFFE Identity Provider alias

	// Sidecar configs — from namespace CMs
	AuthproxyRoutesYAML string

	// AuthBridge runtime config — from namespace "authbridge-runtime-config" ConfigMap
	AuthBridgeRuntimeYAML string // raw config.yaml (base for per-agent ConfigMap)

	// AuthBridgeMode and MTLSMode are resolved from the namespace
	// ConfigMap (or left empty for caller-side defaults).
	// AuthBridgeMode "" → caller picks default. MTLSMode "" → "permissive".
	AuthBridgeMode string
	MTLSMode       string
	// TLSBridgeMode is "disabled" unless CR/namespace set it to "enabled".
	TLSBridgeMode string
}

// ResolveConfig merges platform defaults with namespace ConfigMap values.
func ResolveConfig(platform *config.PlatformConfig, ns *NamespaceConfig) *ResolvedConfig {
	if platform == nil {
		platform = config.CompiledDefaults()
	}
	if ns == nil {
		ns = &NamespaceConfig{}
	}

	resolved := &ResolvedConfig{
		Platform: platform,

		// Start with namespace CM values
		KeycloakURL:                ns.KeycloakURL,
		KeycloakRealm:              ns.KeycloakRealm,
		AdminCredentialsSecretName: KeycloakAdminSecretName,
		SpiffeTrustDomain:          platform.Spiffe.TrustDomain,
		PlatformClientIDs:          ns.PlatformClientIDs,
		TokenURL:                   ns.TokenURL,
		Issuer:                     ns.Issuer,
		ExpectedAudience:           ns.ExpectedAudience,
		TargetAudience:             ns.TargetAudience,
		TargetScopes:               ns.TargetScopes,
		DefaultOutboundPolicy:      ns.DefaultOutboundPolicy,
		ClientAuthType:             ns.ClientAuthType,
		SpiffeIdpAlias:             ns.SpiffeIdpAlias,
		AuthproxyRoutesYAML:        ns.AuthproxyRoutesYAML,
		AuthBridgeRuntimeYAML:      ns.AuthBridgeRuntimeYAML,
	}

	if m := ExtractMode(resolved.AuthBridgeRuntimeYAML); m != "" {
		resolved.AuthBridgeMode = m
	}
	if m := ExtractMTLSMode(resolved.AuthBridgeRuntimeYAML); m != "" {
		resolved.MTLSMode = m
	}
	// TLS bridge defaults to disabled; namespace > disabled.
	resolved.TLSBridgeMode = agentv1alpha1.TLSBridgeModeDisabled
	if m := ExtractTLSBridgeMode(resolved.AuthBridgeRuntimeYAML); m != "" {
		resolved.TLSBridgeMode = m
	}

	return resolved
}
