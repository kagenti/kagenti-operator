package spiffeid

import (
	"fmt"
	"strings"
)

type Identity struct {
	TrustDomain string
	Namespace   string
	Name        string
}

// Parse extracts namespace and service account name from a SPIFFE identity path.
// Expected format: {trust-domain}/ns/{namespace}/sa/{name}
func Parse(raw string) (Identity, error) {
	slashIdx := strings.Index(raw, "/")
	if slashIdx < 0 {
		return Identity{}, fmt.Errorf("SPIFFE ID missing path: %q", raw)
	}

	trustDomain := raw[:slashIdx]
	if trustDomain == "" {
		return Identity{}, fmt.Errorf("SPIFFE ID has empty trust domain: %q", raw)
	}
	path := raw[slashIdx+1:]

	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[0] != "ns" || parts[2] != "sa" {
		return Identity{}, fmt.Errorf("SPIFFE ID path must be ns/{namespace}/sa/{name}, got %q", path)
	}

	ns := parts[1]
	name := parts[3]

	if ns == "" || name == "" {
		return Identity{}, fmt.Errorf("SPIFFE ID has empty namespace or name: %q", raw)
	}

	return Identity{
		TrustDomain: trustDomain,
		Namespace:   ns,
		Name:        name,
	}, nil
}
