package identity

import (
	"net/http/httptest"
	"testing"
)

func TestFromRequest(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantNS  string
		wantN   string
		wantErr bool
	}{
		{
			name:   "valid spiffe",
			url:    "/bundles?spiffe=localtest.me/ns/default/sa/my-agent",
			wantNS: "default",
			wantN:  "my-agent",
		},
		{
			name:   "different namespace",
			url:    "/bundles?spiffe=prod.io/ns/kube-system/sa/gateway",
			wantNS: "kube-system",
			wantN:  "gateway",
		},
		{
			name:    "missing query param",
			url:     "/bundles",
			wantErr: true,
		},
		{
			name:    "invalid spiffe value",
			url:     "/bundles?spiffe=bad-format",
			wantErr: true,
		},
		{
			name:    "empty spiffe value",
			url:     "/bundles?spiffe=",
			wantErr: true,
		},
		{
			name:    "unknown scheme only",
			url:     "/bundles?unknown=something",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.url, nil)
			id, err := FromRequest(req)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", id)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id.Namespace != tt.wantNS {
				t.Errorf("namespace = %q, want %q", id.Namespace, tt.wantNS)
			}
			if id.Name != tt.wantN {
				t.Errorf("name = %q, want %q", id.Name, tt.wantN)
			}
		})
	}
}

func TestClientIdentity_CacheKey(t *testing.T) {
	id := ClientIdentity{Namespace: "ns1", Name: "agent1"}
	if got := id.CacheKey(); got != "ns1/agent1" {
		t.Errorf("CacheKey() = %q, want %q", got, "ns1/agent1")
	}
}

func TestNoopVerifier(t *testing.T) {
	req := httptest.NewRequest("GET", "/bundles/test.tar.gz", nil)
	id := ClientIdentity{Namespace: "default", Name: "agent"}
	if err := (NoopVerifier{}).Verify(req, id); err != nil {
		t.Fatalf("NoopVerifier should never error, got: %v", err)
	}
}
