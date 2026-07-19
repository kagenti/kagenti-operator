package spiffeid

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Identity
		wantErr bool
	}{
		{
			name:  "valid spiffe id",
			input: "localtest.me/ns/default/sa/my-agent",
			want:  Identity{TrustDomain: "localtest.me", Namespace: "default", Name: "my-agent"},
		},
		{
			name:  "different trust domain",
			input: "prod.example.com/ns/kube-system/sa/gateway",
			want:  Identity{TrustDomain: "prod.example.com", Namespace: "kube-system", Name: "gateway"},
		},
		{
			name:    "no slash",
			input:   "localtest.me",
			wantErr: true,
		},
		{
			name:    "wrong path format",
			input:   "localtest.me/ns/foo/bar/baz",
			wantErr: true,
		},
		{
			name:    "missing sa segment",
			input:   "localtest.me/ns/foo/notsa/bar",
			wantErr: true,
		},
		{
			name:    "empty namespace",
			input:   "localtest.me/ns//sa/bar",
			wantErr: true,
		},
		{
			name:    "empty name",
			input:   "localtest.me/ns/foo/sa/",
			wantErr: true,
		},
		{
			name:    "extra path segments",
			input:   "localtest.me/ns/foo/sa/bar/extra",
			wantErr: true,
		},
		{
			name:    "empty trust domain",
			input:   "/ns/foo/sa/bar",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Parse(%q) expected error, got %+v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}
