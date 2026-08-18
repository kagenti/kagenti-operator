package config

import "testing"

// TestValidate_TransparentInboundPort covers the misconfigurations that would
// otherwise surface as a pod that passes admission and then fails to start.
func TestValidate_TransparentInboundPort(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*PlatformConfig)
		wantErr bool
	}{
		{
			name:   "defaults are valid",
			mutate: func(*PlatformConfig) {},
		},
		{
			name:    "below the privileged range",
			mutate:  func(c *PlatformConfig) { c.Proxy.TransparentInboundPort = 80 },
			wantErr: true,
		},
		{
			name:    "above the port range",
			mutate:  func(c *PlatformConfig) { c.Proxy.TransparentInboundPort = 70000 },
			wantErr: true,
		},
		{
			name:    "unset (zero) is rejected rather than silently defaulted",
			mutate:  func(c *PlatformConfig) { c.Proxy.TransparentInboundPort = 0 },
			wantErr: true,
		},
		{
			// Both listeners live in one container, so a shared value makes the
			// second bind fail at pod start — long after admission succeeded.
			name: "colliding with the egress transparent port",
			mutate: func(c *PlatformConfig) {
				c.Proxy.TransparentInboundPort = c.Proxy.TransparentPort
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := CompiledDefaults()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidate_AllowedInboundInterception(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		wantErr bool
	}{
		{name: "both", allowed: []string{"reverse-proxy", "transparent"}},
		{name: "forbid transparent", allowed: []string{"reverse-proxy"}},
		{name: "mandate transparent", allowed: []string{"transparent"}},
		{
			// An empty list would make the fallback index panic, and "allow
			// nothing" has no sensible meaning.
			name: "empty is rejected", allowed: []string{}, wantErr: true,
		},
		{name: "unknown value is rejected", allowed: []string{"reverse-proxy", "tranparent"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := CompiledDefaults()
			cfg.Proxy.AllowedInboundInterception = tc.allowed
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestCompiledDefaults_TransparentInboundDefaults pins the values that must stay
// in lockstep with authbridge's proxy-sidecar preset; a drift here silently
// redirects inbound traffic to a dead port.
func TestCompiledDefaults_TransparentInboundDefaults(t *testing.T) {
	cfg := CompiledDefaults()
	if cfg.Proxy.TransparentInboundPort != 8083 {
		t.Errorf("TransparentInboundPort = %d, want 8083 (authbridge preset transparent_inbound_addr)", cfg.Proxy.TransparentInboundPort)
	}
	if len(cfg.Proxy.AllowedInboundInterception) != 2 {
		t.Errorf("AllowedInboundInterception = %v, want both mechanisms allowed by default", cfg.Proxy.AllowedInboundInterception)
	}
	// Order matters: the first entry is the fallback when a workload requests a
	// value outside the list, and the no-privilege shape must win.
	if cfg.Proxy.AllowedInboundInterception[0] != "reverse-proxy" {
		t.Errorf("AllowedInboundInterception[0] = %q, want reverse-proxy (the fallback must not grant NET_ADMIN)",
			cfg.Proxy.AllowedInboundInterception[0])
	}
}
