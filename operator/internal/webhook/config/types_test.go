package config

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TransparentPort is the REDIRECT target for the enforce-redirect guard; it must
// be a valid, non-privileged port (the proxy binds it as a non-root user).
func TestValidate_TransparentPort(t *testing.T) {
	tests := []struct {
		name    string
		port    int32
		wantErr bool
	}{
		{"default 8082 ok", 8082, false},
		{"zero rejected", 0, true},
		{"privileged rejected", 80, true},
		{"too large rejected", 70000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CompiledDefaults()
			c.Proxy.TransparentPort = tt.port
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

// IptablesCmd pins the proxy-init backend; only the binaries shipped in the
// image (plus "" = auto-detect) are accepted, so a chart typo fails at operator
// startup rather than as a per-injected-pod init crash.
func TestValidate_IptablesCmd(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{"empty (auto-detect) ok", "", false},
		{"iptables (nft) ok", "iptables", false},
		{"iptables-nft ok", "iptables-nft", false},
		{"iptables-legacy ok", "iptables-legacy", false},
		{"typo rejected", "iptable", true},
		{"arbitrary rejected", "nft", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CompiledDefaults()
			c.Proxy.IptablesCmd = tt.cmd
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestCompiledDefaults_SpiffeHelperConfig(t *testing.T) {
	cfg := CompiledDefaults()
	if cfg.Spiffe.HelperConfig == "" {
		t.Fatal("CompiledDefaults().Spiffe.HelperConfig must not be empty")
	}
	if !strings.Contains(cfg.Spiffe.HelperConfig, "agent_address") {
		t.Error("HelperConfig must contain agent_address")
	}
	if !strings.Contains(cfg.Spiffe.HelperConfig, "svid_file_name") {
		t.Error("HelperConfig must contain svid_file_name")
	}
}

func TestSpiffeHelperConfig_YAMLRoundTrip(t *testing.T) {
	input := `
spiffe:
  trustDomain: example.org
  socketPath: "unix:///custom/socket.sock"
  helperConfig: |
    agent_address = "/custom/socket.sock"
    cmd = ""
`
	var cfg PlatformConfig
	if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("YAML unmarshal failed: %v", err)
	}
	if cfg.Spiffe.TrustDomain != "example.org" {
		t.Errorf("TrustDomain = %q, want example.org", cfg.Spiffe.TrustDomain)
	}
	if !strings.Contains(cfg.Spiffe.HelperConfig, "agent_address") {
		t.Error("HelperConfig not loaded from YAML")
	}
}

func TestDeepCopy_PreservesHelperConfig(t *testing.T) {
	cfg := CompiledDefaults()
	cfg.Spiffe.HelperConfig = "custom_helper_config"

	copied := cfg.DeepCopy()
	if copied.Spiffe.HelperConfig != "custom_helper_config" {
		t.Errorf("DeepCopy lost HelperConfig: got %q", copied.Spiffe.HelperConfig)
	}
}
