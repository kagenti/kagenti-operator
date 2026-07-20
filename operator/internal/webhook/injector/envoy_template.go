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
	"bytes"
	_ "embed"
	"fmt"
	"text/template" // text/template (not html/template) — YAML output, no HTML escaping needed
)

//go:embed envoy.yaml.tmpl
var envoyTemplateSrc string

var envoyTemplate = template.Must(template.New("envoy.yaml").Parse(envoyTemplateSrc))

// envoyTemplateData holds the values substituted into the envoy.yaml template.
type envoyTemplateData struct {
	AdminPort    int32
	OutboundPort int32
	InboundPort  int32
	ExtProcPort  int32
	// SpireEnabled gates the TLS listener for verified fetch.
	SpireEnabled bool
	// TLSPort is the port for the HTTPS listener (used when SpireEnabled is true).
	TLSPort int32
	// MTLSEnabled is true when MTLSMode is "permissive" or "strict". When
	// enabled, the inbound listener gets a tls_inspector listener filter
	// and a TLS filter chain backed by /opt/svid*.pem. The plaintext chain
	// is also kept under permissive (filter_chain_match: raw_buffer);
	// strict drops it so non-TLS callers fail at the listener.
	MTLSEnabled bool
	// MTLSPermissive / MTLSStrict are precomputed bool flags driven from
	// MTLSMode and the constants in constants.go. The template uses these
	// directly ({{ if .MTLSStrict }} / {{ if .MTLSPermissive }}) instead
	// of comparing MTLSMode against bare string literals, so the
	// constants stay the single source of truth.
	MTLSPermissive bool
	MTLSStrict     bool
}

// Default ext-proc gRPC port (go-processor).
const defaultExtProcPort int32 = 9090

// RenderEnvoyConfig generates an envoy.yaml from the resolved config.
//
// The TLS blocks are gated on cfg.MTLSMode: permissive renders both a
// TLS chain and a raw_buffer chain on the inbound listener; strict
// renders the TLS chain only and routes outbound to a TLS-originating
// cluster. disabled / "" produces today's plaintext config unchanged.
func RenderEnvoyConfig(cfg *ResolvedConfig) (string, error) {
	if cfg == nil || cfg.Platform == nil {
		return "", fmt.Errorf("resolved config or platform config is nil")
	}

	// MTLSEnabled: empty string is treated as permissive (mTLS is on
	// by default). Only MTLSModeDisabled explicitly disables mTLS.
	effectiveMode := cfg.MTLSMode
	if effectiveMode == "" {
		effectiveMode = MTLSModePermissive
	}
	data := envoyTemplateData{
		AdminPort:      cfg.Platform.Proxy.AdminPort,
		OutboundPort:   cfg.Platform.Proxy.Port,
		InboundPort:    cfg.Platform.Proxy.InboundProxyPort,
		ExtProcPort:    defaultExtProcPort,
		MTLSEnabled:    effectiveMode != MTLSModeDisabled,
		MTLSPermissive: effectiveMode == MTLSModePermissive,
		MTLSStrict:     effectiveMode == MTLSModeStrict,
	}

	var buf bytes.Buffer
	if err := envoyTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing envoy template: %w", err)
	}
	return buf.String(), nil
}
