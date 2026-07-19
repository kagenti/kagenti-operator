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
	"strings"
	"testing"

	"github.com/rossoctl/operator/internal/webhook/config"
)

func TestRenderEnvoyConfig_TemplateRendering(t *testing.T) {
	cfg := &ResolvedConfig{
		Platform: config.CompiledDefaults(),
	}

	result, err := RenderEnvoyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the template was rendered with correct ports
	if !strings.Contains(result, "port_value: 9901") {
		t.Error("expected admin port 9901 in rendered config")
	}
	if !strings.Contains(result, "port_value: 15123") {
		t.Error("expected outbound port 15123 in rendered config")
	}
	if !strings.Contains(result, "port_value: 15124") {
		t.Error("expected inbound port 15124 in rendered config")
	}
	if !strings.Contains(result, "ext_proc_cluster") {
		t.Error("expected ext_proc_cluster in rendered config")
	}
	if !strings.Contains(result, "port_value: 9090") {
		t.Error("expected ext_proc port 9090 in rendered config")
	}
	if !strings.Contains(result, "original_destination") {
		t.Error("expected original_destination cluster in rendered config")
	}
}

func TestRenderEnvoyConfig_MTLSDisabled_NoTLSBlocks(t *testing.T) {
	// Explicitly disabled mode — no TLS blocks should render.
	// Empty string is now treated as permissive (mTLS on by default).
	for _, mode := range []string{MTLSModeDisabled} {
		t.Run("mode="+mode, func(t *testing.T) {
			cfg := &ResolvedConfig{
				Platform: config.CompiledDefaults(),
				MTLSMode: mode,
			}
			result, err := RenderEnvoyConfig(cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, banned := range []string{
				"tls_inspector",
				"DownstreamTlsContext",
				"UpstreamTlsContext",
				"original_destination_tls",
			} {
				if strings.Contains(result, banned) {
					t.Errorf("disabled mode should not render %q", banned)
				}
			}
		})
	}
}

func TestRenderEnvoyConfig_MTLSPermissive(t *testing.T) {
	cfg := &ResolvedConfig{
		Platform: config.CompiledDefaults(),
		MTLSMode: MTLSModePermissive,
	}
	result, err := RenderEnvoyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Inbound: tls_inspector + both TLS and raw_buffer chains.
	for _, want := range []string{
		"tls_inspector",
		"DownstreamTlsContext",
		"transport_protocol: tls",
		"transport_protocol: raw_buffer",
		"/opt/svid.pem",
		"/opt/svid_key.pem",
		"/opt/svid_bundle.pem",
		"require_client_certificate: true",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("permissive should render %q", want)
		}
	}

	// Outbound: stays plaintext — no UpstreamTlsContext, no TLS cluster.
	for _, banned := range []string{
		"UpstreamTlsContext",
		"original_destination_tls",
	} {
		if strings.Contains(result, banned) {
			t.Errorf("permissive outbound should stay plaintext; got %q", banned)
		}
	}
}

func TestRenderEnvoyConfig_MTLSStrict(t *testing.T) {
	cfg := &ResolvedConfig{
		Platform: config.CompiledDefaults(),
		MTLSMode: MTLSModeStrict,
	}
	result, err := RenderEnvoyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Inbound: tls_inspector + TLS chain only.
	for _, want := range []string{
		"tls_inspector",
		"DownstreamTlsContext",
		"transport_protocol: tls",
		"require_client_certificate: true",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("strict should render %q", want)
		}
	}
	// Strict: no raw_buffer chain (plaintext drops at filter chain match).
	if strings.Contains(result, "transport_protocol: raw_buffer") {
		t.Error("strict mode must not render the raw_buffer chain — plaintext should drop at filter chain match")
	}

	// Outbound: TLS-originating cluster present + outbound listener
	// routes to it.
	for _, want := range []string{
		"UpstreamTlsContext",
		"original_destination_tls",
		"cluster: original_destination_tls",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("strict outbound should render %q", want)
		}
	}
}

func TestRenderEnvoyConfig_CustomPorts(t *testing.T) {
	platform := config.CompiledDefaults()
	platform.Proxy.Port = 20000
	platform.Proxy.InboundProxyPort = 20001
	platform.Proxy.AdminPort = 20002

	cfg := &ResolvedConfig{
		Platform: platform,
	}

	result, err := RenderEnvoyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "port_value: 20000") {
		t.Error("expected custom outbound port 20000")
	}
	if !strings.Contains(result, "port_value: 20001") {
		t.Error("expected custom inbound port 20001")
	}
	if !strings.Contains(result, "port_value: 20002") {
		t.Error("expected custom admin port 20002")
	}
}
