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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

var nsConfigLog = logf.Log.WithName("namespace-config")

// Well-known ConfigMap/Secret names in the target namespace.
const (
	AuthBridgeConfigMapName        = "authbridge-config"
	AuthBridgeRuntimeConfigMapName = "authbridge-runtime-config"
	KeycloakAdminSecretName        = "keycloak-admin-secret"
	SpiffeHelperConfigMapName      = "spiffe-helper-config"
	EnvoyConfigMapName             = "envoy-config"
	AuthproxyRoutesConfigMapName   = "authproxy-routes"
)

// NamespaceConfig holds resolved values from namespace ConfigMaps/Secrets.
type NamespaceConfig struct {
	// From "authbridge-config" ConfigMap
	KeycloakURL           string
	KeycloakRealm         string
	PlatformClientIDs     string
	TokenURL              string
	Issuer                string
	ExpectedAudience      string
	TargetAudience        string
	TargetScopes          string
	DefaultOutboundPolicy string
	ClientAuthType        string // "client-secret" or "federated-jwt"
	SpiffeIdpAlias        string // Keycloak SPIFFE Identity Provider alias (e.g., "spire-spiffe")
	JWTAudience           string // Audience for SPIFFE JWT-SVIDs (token-exchange identity.jwt_audience)

	// From "authproxy-routes" ConfigMap
	AuthproxyRoutesYAML string // raw routes.yaml content

	// From "authbridge-runtime-config" ConfigMap
	AuthBridgeRuntimeYAML string // raw config.yaml for unified authbridge binary
}

// ReadNamespaceConfig reads the well-known ConfigMaps/Secrets from the target
// namespace at admission time. Missing resources result in empty strings for
// those fields; each read is independent.
func ReadNamespaceConfig(ctx context.Context, c client.Reader, namespace string) (*NamespaceConfig, error) {
	cfg := &NamespaceConfig{}

	// Read "authbridge-config" ConfigMap (all identity + token exchange settings)
	if cm, err := getConfigMap(ctx, c, namespace, AuthBridgeConfigMapName); err != nil {
		nsConfigLog.V(1).Info("ConfigMap not found", "name", AuthBridgeConfigMapName, "namespace", namespace, "error", err)
	} else {
		cfg.KeycloakURL = cm.Data["KEYCLOAK_URL"]
		cfg.KeycloakRealm = cm.Data["KEYCLOAK_REALM"]
		cfg.PlatformClientIDs = cm.Data["PLATFORM_CLIENT_IDS"]
		cfg.TokenURL = cm.Data["TOKEN_URL"]
		cfg.Issuer = cm.Data["ISSUER"]
		cfg.ExpectedAudience = cm.Data["EXPECTED_AUDIENCE"]
		cfg.TargetAudience = cm.Data["TARGET_AUDIENCE"]
		cfg.TargetScopes = cm.Data["TARGET_SCOPES"]
		cfg.DefaultOutboundPolicy = cm.Data["DEFAULT_OUTBOUND_POLICY"]
		cfg.ClientAuthType = cm.Data["CLIENT_AUTH_TYPE"]
		cfg.SpiffeIdpAlias = cm.Data["SPIFFE_IDP_ALIAS"]
		cfg.JWTAudience = cm.Data["JWT_AUDIENCE"]
	}

	// Note: keycloak-admin-secret is not read here. The resolved container builder
	// uses SecretKeyRef to reference the secret by name, keeping credentials out of
	// the NamespaceConfig struct and the webhook's memory.

	// Note: spiffe-helper-config is NOT read here. The controller derives it
	// from PlatformConfig via ensureSpiffeHelperConfigMap(). The volume mount
	// still references SpiffeHelperConfigMapName directly.

	// Read "authproxy-routes" ConfigMap
	if cm, err := getConfigMap(ctx, c, namespace, AuthproxyRoutesConfigMapName); err != nil {
		nsConfigLog.V(1).Info("ConfigMap not found", "name", AuthproxyRoutesConfigMapName, "namespace", namespace, "error", err)
	} else {
		cfg.AuthproxyRoutesYAML = cm.Data["routes.yaml"]
	}

	// Read "authbridge-runtime-config" ConfigMap (YAML config for unified authbridge binary)
	if cm, err := getConfigMap(ctx, c, namespace, AuthBridgeRuntimeConfigMapName); err != nil {
		nsConfigLog.V(1).Info("ConfigMap not found", "name", AuthBridgeRuntimeConfigMapName, "namespace", namespace, "error", err)
	} else {
		cfg.AuthBridgeRuntimeYAML = cm.Data["config.yaml"]
	}

	return cfg, nil
}

func getConfigMap(ctx context.Context, c client.Reader, namespace, name string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return nil, err
	}
	return cm, nil
}

// ExtractMode parses an authbridge-runtime-config config.yaml string and
// returns the value of its top-level `mode:` key. Returns "" if the YAML
// is empty, malformed, or has no `mode` field — in any of those cases the
// caller should fall back to the cluster default.
//
// Used by pod_mutator's mode-resolution chain. Stays a small surgical
// parse rather than a full YAML decode so it tolerates older or
// hand-edited ConfigMaps that may have other unknown top-level keys.
func ExtractMode(authbridgeYAML string) string {
	if authbridgeYAML == "" {
		return ""
	}
	var top struct {
		Mode string `json:"mode"`
	}
	if err := yaml.Unmarshal([]byte(authbridgeYAML), &top); err != nil {
		// Fail-safe: empty string lets the resolution chain fall through
		// to the next layer. Log a warning so operators can spot a
		// malformed authbridge-runtime-config — silent failure here was
		// flagged in PR #361 review.
		nsConfigLog.Info("WARN: failed to parse authbridge-runtime-config config.yaml; falling back to next resolution layer",
			"error", err.Error())
		return ""
	}
	return top.Mode
}

// ExtractEgressEnforcement parses an authbridge-runtime-config config.yaml
// string and returns the value of its top-level `egressEnforcement:` key.
// Returns "" if the YAML is empty, malformed, or has no `egressEnforcement`
// field — in any of those cases the caller should fall back to the next
// resolution layer (or the "enforce-redirect" default).
func ExtractEgressEnforcement(authbridgeYAML string) string {
	if authbridgeYAML == "" {
		return ""
	}
	var top struct {
		EgressEnforcement string `json:"egressEnforcement"`
	}
	if err := yaml.Unmarshal([]byte(authbridgeYAML), &top); err != nil {
		nsConfigLog.Info("WARN: failed to parse authbridge-runtime-config config.yaml for egressEnforcement; falling back to next resolution layer",
			"error", err.Error())
		return ""
	}
	return top.EgressEnforcement
}

// ExtractMTLSMode parses an authbridge-runtime-config config.yaml string
// and returns the value of its `mtls.mode` field. Returns "" if the YAML
// is empty, malformed, has no `mtls` block, or its `mode` field is unset
// — in any of those cases the caller should fall back to the next
// resolution layer (or the "disabled" default).
//
// Same surgical-parse pattern as ExtractMode: tolerates extra top-level
// keys so older or hand-edited ConfigMaps round-trip cleanly.
func ExtractMTLSMode(authbridgeYAML string) string {
	if authbridgeYAML == "" {
		return ""
	}
	var top struct {
		MTLS struct {
			Mode string `json:"mode"`
		} `json:"mtls"`
	}
	if err := yaml.Unmarshal([]byte(authbridgeYAML), &top); err != nil {
		nsConfigLog.Info("WARN: failed to parse authbridge-runtime-config config.yaml for mtls.mode; falling back to next resolution layer",
			"error", err.Error())
		return ""
	}
	return top.MTLS.Mode
}

// ExtractTLSBridgeMode parses an authbridge-runtime-config config.yaml string
// and returns the value of its `tls_bridge.mode` field. Returns "" if the YAML
// is empty, malformed, has no `tls_bridge` block, or its `mode` field is unset
// — the caller then falls back to the next resolution layer (or "disabled").
// Same surgical-parse pattern as ExtractMTLSMode.
func ExtractTLSBridgeMode(authbridgeYAML string) string {
	if authbridgeYAML == "" {
		return ""
	}
	var top struct {
		TLSBridge struct {
			Mode string `json:"mode"`
		} `json:"tls_bridge"`
	}
	if err := yaml.Unmarshal([]byte(authbridgeYAML), &top); err != nil {
		nsConfigLog.Info("WARN: failed to parse authbridge-runtime-config config.yaml for tls_bridge.mode; falling back to next resolution layer",
			"error", err.Error())
		return ""
	}
	return top.TLSBridge.Mode
}
