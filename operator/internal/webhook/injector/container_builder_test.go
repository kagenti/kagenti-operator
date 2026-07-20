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
	"strconv"
	"testing"

	"github.com/rossoctl/operator/internal/webhook/config"
)

func TestBuildEnvoyProxyContainer_SpireEnabled_HasSvidOutputMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			found = true
			if vm.MountPath != "/opt" {
				t.Errorf("svid-output mount path = %q, want /opt", vm.MountPath)
			}
			// Must be RW: the in-process spiffe Provider mirror inside the
			// authbridge-envoy combined image writes /opt/svid*.pem there.
			// The previous assertion (ReadOnly required) was wrong — it was
			// asserting the bug as the desired behavior. Envoy's file-based
			// DownstreamTlsContext / UpstreamTlsContext (used when mtlsMode
			// != disabled) refuses to boot if these files don't exist.
			if vm.ReadOnly {
				t.Error("svid-output mount must be read-write so the spiffe Provider mirror can write SVIDs")
			}
			break
		}
	}
	if !found {
		t.Error("envoy-proxy container missing svid-output volume mount when SPIRE is enabled")
	}
}

func TestBuildEnvoyProxyContainer_SpireDisabled_NoSvidOutputMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(false)

	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			t.Error("envoy-proxy container should NOT have svid-output mount when SPIRE is disabled")
		}
	}
}

func TestBuildEnvoyProxyContainer_DefaultIncludesSvidOutput(t *testing.T) {
	// The no-arg BuildEnvoyProxyContainer defaults to SPIRE enabled
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainer()

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			found = true
			break
		}
	}
	if !found {
		t.Error("default BuildEnvoyProxyContainer should include svid-output mount")
	}
}

func TestBuildEnvoyProxyContainer_HasAllRequiredMounts(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	requiredMounts := map[string]string{
		"envoy-config":              "/etc/envoy",
		"shared-data":               "/shared",
		"svid-output":               "/opt",
		"authbridge-runtime-config": "/etc/authbridge",
	}

	mountsByName := make(map[string]string)
	for _, vm := range container.VolumeMounts {
		mountsByName[vm.Name] = vm.MountPath
	}

	for name, expectedPath := range requiredMounts {
		path, ok := mountsByName[name]
		if !ok {
			t.Errorf("missing volume mount %q", name)
			continue
		}
		if path != expectedPath {
			t.Errorf("volume mount %q path = %q, want %q", name, path, expectedPath)
		}
	}
}

func TestBuildEnvoyProxyContainer_Name(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainer()

	if container.Name != EnvoyProxyContainerName {
		t.Errorf("container name = %q, want %q", container.Name, EnvoyProxyContainerName)
	}
}

func TestBuildEnvoyProxyContainer_HasKeycloakURLAndRealm(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	for _, key := range []string{"KEYCLOAK_URL", "KEYCLOAK_REALM"} {
		found := false
		for _, env := range container.Env {
			if env.Name == key {
				found = true
				if env.ValueFrom == nil || env.ValueFrom.ConfigMapKeyRef == nil {
					t.Errorf("env %q must use ConfigMapKeyRef", key)
					break
				}
				if env.ValueFrom.ConfigMapKeyRef.Name != "authbridge-config" {
					t.Errorf("env %q ConfigMapKeyRef.Name = %q, want %q", key, env.ValueFrom.ConfigMapKeyRef.Name, "authbridge-config")
				}
				if env.ValueFrom.ConfigMapKeyRef.Optional == nil || !*env.ValueFrom.ConfigMapKeyRef.Optional {
					t.Errorf("env %q should be optional", key)
				}
				break
			}
		}
		if !found {
			t.Errorf("envoy-proxy container missing env var %q", key)
		}
	}
}

func TestBuildOutboundExcludeValue_Empty(t *testing.T) {
	got := buildOutboundExcludeValue("")
	if got != "8080" {
		t.Errorf("buildOutboundExcludeValue(\"\") = %q, want %q", got, "8080")
	}
}

func TestBuildOutboundExcludeValue_SinglePort(t *testing.T) {
	got := buildOutboundExcludeValue("11434")
	if got != "8080,11434" {
		t.Errorf("buildOutboundExcludeValue(\"11434\") = %q, want %q", got, "8080,11434")
	}
}

func TestBuildOutboundExcludeValue_MultiplePorts(t *testing.T) {
	got := buildOutboundExcludeValue("11434,4317")
	if got != "8080,11434,4317" {
		t.Errorf("buildOutboundExcludeValue(\"11434,4317\") = %q, want %q", got, "8080,11434,4317")
	}
}

func TestBuildOutboundExcludeValue_Deduplicates8080(t *testing.T) {
	got := buildOutboundExcludeValue("8080,11434")
	if got != "8080,11434" {
		t.Errorf("buildOutboundExcludeValue(\"8080,11434\") = %q, want %q", got, "8080,11434")
	}
}

func TestBuildOutboundExcludeValue_TrimsWhitespace(t *testing.T) {
	got := buildOutboundExcludeValue(" 11434 , 4317 ")
	if got != "8080,11434,4317" {
		t.Errorf("buildOutboundExcludeValue(\" 11434 , 4317 \") = %q, want %q", got, "8080,11434,4317")
	}
}

func TestBuildOutboundExcludeValue_DropsInvalidTokens(t *testing.T) {
	got := buildOutboundExcludeValue("11434,abc,0,65536,-1,,99999")
	if got != "8080,11434" {
		t.Errorf("buildOutboundExcludeValue with invalid tokens = %q, want %q", got, "8080,11434")
	}
}

func TestBuildOutboundExcludeValue_BoundaryPorts(t *testing.T) {
	got := buildOutboundExcludeValue("1,65535")
	if got != "8080,1,65535" {
		t.Errorf("buildOutboundExcludeValue(\"1,65535\") = %q, want %q", got, "8080,1,65535")
	}
}

func TestBuildProxyInitContainer_DefaultExclude(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("redirect", "", "")

	var foundOutbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			foundOutbound = true
			if env.Value != "8080" {
				t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080")
			}
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			t.Error("INBOUND_PORTS_EXCLUDE should not be set when inbound exclude is empty")
		}
	}
	if !foundOutbound {
		t.Error("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
}

func TestBuildProxyInitContainer_WithAnnotationPorts(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("redirect", "11434,4317", "")

	var foundOutbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			foundOutbound = true
			if env.Value != "8080,11434,4317" {
				t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434,4317")
			}
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			t.Error("INBOUND_PORTS_EXCLUDE should not be set when inbound exclude is empty")
		}
	}
	if !foundOutbound {
		t.Error("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
}

func TestBuildProxyInitContainer_WithInboundExclude(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("redirect", "", "8443,18789")

	var foundInbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" && env.Value != "8080" {
			t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080")
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			foundInbound = true
			if env.Value != "8443,18789" {
				t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8443,18789")
			}
		}
	}
	if !foundInbound {
		t.Error("proxy-init container missing INBOUND_PORTS_EXCLUDE env var")
	}
}

func TestBuildProxyInitContainer_WithBothExcludes(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("redirect", "11434", "8443")

	var foundOutbound, foundInbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			foundOutbound = true
			if env.Value != "8080,11434" {
				t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434")
			}
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			foundInbound = true
			if env.Value != "8443" {
				t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8443")
			}
		}
	}
	if !foundOutbound {
		t.Error("missing OUTBOUND_PORTS_EXCLUDE")
	}
	if !foundInbound {
		t.Error("missing INBOUND_PORTS_EXCLUDE")
	}
}

// The proxy-sidecar container must run as the configured Proxy.UID — the same
// value the enforce-redirect guard exempts via --uid-owner. If these drift, the
// proxy's own egress would be redirected back (or the agent would share the exempt UID).
func TestBuildProxySidecarContainer_RunAsProxyUID(t *testing.T) {
	cfg := config.CompiledDefaults()
	builder := NewContainerBuilder(cfg)
	container := builder.BuildProxySidecarContainer(true)

	sc := container.SecurityContext
	if sc == nil || sc.RunAsUser == nil {
		t.Fatal("proxy-sidecar container missing SecurityContext.RunAsUser")
	}
	if got := *sc.RunAsUser; got != cfg.Proxy.UID {
		t.Errorf("proxy-sidecar RunAsUser = %d, want Proxy.UID %d", got, cfg.Proxy.UID)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != cfg.Proxy.UID {
		t.Errorf("proxy-sidecar RunAsGroup = %v, want Proxy.UID %d", sc.RunAsGroup, cfg.Proxy.UID)
	}
}

// enforce-redirect mode emits MODE / PROXY_UID / TRANSPARENT_PORT, no CLUSTER_CIDRS
// (DNS exemption is resolv.conf-driven in the init script), and none of the
// redirect-only vars (POD_IP, OUTBOUND_PORTS_EXCLUDE).
func TestBuildProxyInitContainer_EnforceRedirect(t *testing.T) {
	cfg := config.CompiledDefaults()
	builder := NewContainerBuilder(cfg)
	container := builder.BuildProxyInitContainer("enforce-redirect", "", "")

	got := map[string]string{}
	for _, e := range container.Env {
		if e.ValueFrom != nil {
			t.Errorf("enforce-redirect env %q must be a literal, not ValueFrom", e.Name)
		}
		got[e.Name] = e.Value
	}
	if got["MODE"] != "enforce-redirect" {
		t.Errorf("MODE = %q, want enforce-redirect", got["MODE"])
	}
	if want := strconv.FormatInt(cfg.Proxy.UID, 10); got["PROXY_UID"] != want {
		t.Errorf("PROXY_UID = %q, want %q", got["PROXY_UID"], want)
	}
	if _, ok := got["CLUSTER_CIDRS"]; ok {
		t.Error("enforce-redirect must not set CLUSTER_CIDRS (DNS exemption is resolv.conf-driven in the init script)")
	}
	if want := strconv.FormatInt(int64(cfg.Proxy.TransparentPort), 10); got["TRANSPARENT_PORT"] != want {
		t.Errorf("TRANSPARENT_PORT = %q, want %q", got["TRANSPARENT_PORT"], want)
	}
	if _, ok := got["POD_IP"]; ok {
		t.Error("enforce-redirect must not set POD_IP")
	}
	if _, ok := got["OUTBOUND_PORTS_EXCLUDE"]; ok {
		t.Error("enforce-redirect must not set OUTBOUND_PORTS_EXCLUDE")
	}
	if _, ok := got["IPTABLES_CMD"]; ok {
		t.Error("enforce-redirect must not set IPTABLES_CMD by default (proxy-init auto-detects from /proc/modules)")
	}
}

// IPTABLES_CMD is injected only when Proxy.IptablesCmd is configured, in both
// modes, so the init script's /proc/modules auto-detection stays the default
// and the backend override is strictly opt-in.
func TestBuildProxyInitContainer_IptablesCmd(t *testing.T) {
	modes := []ProxyInitMode{ProxyInitModeRedirect, ProxyInitModeEnforceRedirect}

	// Default (empty): env var absent in both modes.
	def := NewContainerBuilder(config.CompiledDefaults())
	for _, mode := range modes {
		for _, e := range def.BuildProxyInitContainer(mode, "", "").Env {
			if e.Name == "IPTABLES_CMD" {
				t.Errorf("mode %q: IPTABLES_CMD must be absent when unset, got %q", mode, e.Value)
			}
		}
	}

	// Configured: env var present with the configured value in both modes.
	cfg := config.CompiledDefaults()
	cfg.Proxy.IptablesCmd = "iptables"
	b := NewContainerBuilder(cfg)
	for _, mode := range modes {
		found := false
		for _, e := range b.BuildProxyInitContainer(mode, "", "").Env {
			if e.Name == "IPTABLES_CMD" {
				found = true
				if e.Value != "iptables" {
					t.Errorf("mode %q: IPTABLES_CMD = %q, want %q", mode, e.Value, "iptables")
				}
			}
		}
		if !found {
			t.Errorf("mode %q: IPTABLES_CMD env not set when configured", mode)
		}
	}
}

// An unknown mode must fail closed: BuildProxyInitContainer returns a
// zero-value container (no name/image/env) rather than silently degrading to
// redirect, which would ship a proxy-init with no egress guard (fail-open).
// This guards the exported API against a typo'd mode literal at a future
// callsite — a typo in a fail-closed control is the worst failure mode.
func TestBuildProxyInitContainer_InvalidMode(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer(ProxyInitMode("enfore-drop"), "", "")

	if container.Name != "" || container.Image != "" || container.Env != nil {
		t.Errorf("invalid mode must return a zero-value container, got name=%q image=%q env=%v",
			container.Name, container.Image, container.Env)
	}
}

func TestBuildPortExcludeValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"single port", "8443", "8443"},
		{"multiple ports", "8443,18789", "8443,18789"},
		{"whitespace", " 8443 , 18789 ", "8443,18789"},
		{"duplicates", "8443,8443,18789", "8443,18789"},
		{"invalid tokens", "8443,abc,18789", "8443,18789"},
		{"out of range", "0,8443,99999", "8443"},
		{"all invalid", "abc,0,99999", ""},
		{"empty segments", "8443,,18789", "8443,18789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPortExcludeValue(tt.input, "test-annotation")
			if got != tt.want {
				t.Errorf("buildPortExcludeValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ========================================
// AuthBridge combined container tests
// ========================================

func TestBuildEnvoyProxyContainer_HasExpectedAudienceFromConfigMap(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	found := false
	for _, env := range container.Env {
		if env.Name == "EXPECTED_AUDIENCE" {
			found = true
			if env.ValueFrom == nil || env.ValueFrom.ConfigMapKeyRef == nil {
				t.Error("EXPECTED_AUDIENCE must use ConfigMapKeyRef")
				break
			}
			if env.ValueFrom.ConfigMapKeyRef.Name != "authbridge-config" {
				t.Errorf("EXPECTED_AUDIENCE ConfigMapKeyRef.Name = %q, want %q",
					env.ValueFrom.ConfigMapKeyRef.Name, "authbridge-config")
			}
			if env.ValueFrom.ConfigMapKeyRef.Optional == nil || !*env.ValueFrom.ConfigMapKeyRef.Optional {
				t.Error("EXPECTED_AUDIENCE should be optional")
			}
			break
		}
	}
	if !found {
		t.Error("envoy-proxy container missing EXPECTED_AUDIENCE env var from ConfigMap")
	}
}

func TestBuildProxySidecarContainer_SpireDisabled(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxySidecarContainer(false)

	if container.Name != AuthBridgeProxyContainerName {
		t.Errorf("container name = %q, want %q", container.Name, AuthBridgeProxyContainerName)
	}
	if container.Image != config.CompiledDefaults().Images.AuthBridge {
		t.Errorf("image = %q, want %q", container.Image, config.CompiledDefaults().Images.AuthBridge)
	}

	// Should have --config args (mode comes from per-agent ConfigMap, not CLI)
	if len(container.Args) < 2 || container.Args[0] != "--config" {
		t.Errorf("args = %v, want [--config /etc/authbridge/config.yaml]", container.Args)
	}

	// Should have reverse-proxy and forward-proxy ports
	portNames := map[string]bool{}
	for _, p := range container.Ports {
		portNames[p.Name] = true
	}
	if !portNames["reverse-proxy"] {
		t.Error("missing reverse-proxy port")
	}
	if !portNames["forward-proxy"] {
		t.Error("missing forward-proxy port")
	}

	// Should NOT have svid-output volume mount (SPIRE disabled)
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			t.Error("svid-output volume mount should not be present when SPIRE is disabled")
		}
	}
}

func TestBuildProxySidecarContainer_SpireEnabled(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxySidecarContainer(true)

	// Should have svid-output volume mount
	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			found = true
			break
		}
	}
	if !found {
		t.Error("svid-output volume mount should be present when SPIRE is enabled")
	}
}

// TestBuildEnvoyProxyContainer_SpireEnabled_HasSocketMount asserts that
// the SPIRE workload-API socket volume is mounted into the envoy-proxy
// container when SPIRE is on. The bundled spiffe-helper inside the
// combined image dials this socket; without the mount it sits in a
// silent dial-loop and never writes /opt/svid*.pem.
func TestBuildEnvoyProxyContainer_SpireEnabled_HasSocketMount(t *testing.T) {
	cfg := config.CompiledDefaults()
	builder := NewContainerBuilder(cfg)
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	// Derive the expected mount path from SpiffeConfig.SocketPath the
	// same way the production code does, so a future change to the
	// canonical SocketPath in defaults.go can't leave this test
	// asserting against a stale literal.
	wantPath := spireSocketDir(cfg.Spiffe.SocketPath)
	if wantPath == "" {
		t.Fatalf("spireSocketDir(%q) returned empty — defaults must declare a valid socket path", cfg.Spiffe.SocketPath)
	}

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "spire-agent-socket" {
			found = true
			if vm.MountPath != wantPath {
				t.Errorf("spire-agent-socket mount path = %q, want %q (derived from SpiffeConfig.SocketPath %q)",
					vm.MountPath, wantPath, cfg.Spiffe.SocketPath)
			}
			if !vm.ReadOnly {
				t.Error("spire-agent-socket mount should be read-only (CSI volume itself is read-only)")
			}
			break
		}
	}
	if !found {
		t.Error("envoy-proxy container missing spire-agent-socket mount when SPIRE is enabled — bundled spiffe-helper can't reach the workload API")
	}
}

// TestBuildEnvoyProxyContainer_SpireDisabled_NoSocketMount: with SPIRE
// off the socket mount must be absent — there's no spiffe-helper to
// dial the socket, and mounting it would still try to schedule the
// CSI volume.
func TestBuildEnvoyProxyContainer_SpireDisabled_NoSocketMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(false)

	for _, vm := range container.VolumeMounts {
		if vm.Name == "spire-agent-socket" {
			t.Error("envoy-proxy container should NOT have spire-agent-socket mount when SPIRE is disabled")
		}
	}
}

// TestBuildProxySidecarContainer_SpireEnabled_HasSocketMount: same as
// the envoy-proxy variant but for the proxy-sidecar combined image.
// The bundled spiffe-helper has the same workload-API requirement.
func TestBuildProxySidecarContainer_SpireEnabled_HasSocketMount(t *testing.T) {
	cfg := config.CompiledDefaults()
	builder := NewContainerBuilder(cfg)
	container := builder.BuildProxySidecarContainer(true)

	wantPath := spireSocketDir(cfg.Spiffe.SocketPath)
	if wantPath == "" {
		t.Fatalf("spireSocketDir(%q) returned empty — defaults must declare a valid socket path", cfg.Spiffe.SocketPath)
	}

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "spire-agent-socket" {
			found = true
			if vm.MountPath != wantPath {
				t.Errorf("spire-agent-socket mount path = %q, want %q (derived from SpiffeConfig.SocketPath %q)",
					vm.MountPath, wantPath, cfg.Spiffe.SocketPath)
			}
			if !vm.ReadOnly {
				t.Error("spire-agent-socket mount should be read-only (CSI volume itself is read-only)")
			}
			break
		}
	}
	if !found {
		t.Error("proxy-sidecar container missing spire-agent-socket mount when SPIRE is enabled — bundled spiffe-helper can't reach the workload API")
	}
}

// TestBuildProxySidecarContainer_SpireDisabled_NoSocketMount mirrors
// the envoy-proxy negative test for the proxy-sidecar variant.
func TestBuildProxySidecarContainer_SpireDisabled_NoSocketMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxySidecarContainer(false)

	for _, vm := range container.VolumeMounts {
		if vm.Name == "spire-agent-socket" {
			t.Error("proxy-sidecar container should NOT have spire-agent-socket mount when SPIRE is disabled")
		}
	}
}
