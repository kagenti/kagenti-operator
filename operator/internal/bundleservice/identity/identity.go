package identity

import (
	"fmt"
	"net/http"

	"github.com/rossoctl/operator/internal/bundleservice/spiffeid"
)

// ClientIdentity represents a client's namespace and name derived from the
// request query parameters. The query parameter name identifies the scheme
// (e.g. "spiffe"), and the value is the scheme-specific identifier validated
// against that scheme's parser.
type ClientIdentity struct {
	Namespace string
	Name      string
}

// CacheKey returns a unique key for caching bundles per identity.
func (c ClientIdentity) CacheKey() string {
	return c.Namespace + "/" + c.Name
}

// FromRequest extracts the ClientIdentity from request query parameters.
// The query parameter name is the scheme, the value is the identifier:
//
//	GET /bundles?spiffe=trust-domain/ns/{ns}/sa/{name}
//
// The value is always validated against the scheme's strict parser.
// Supported schemes: spiffe.
func FromRequest(r *http.Request) (ClientIdentity, error) {
	q := r.URL.Query()

	if v := q.Get("spiffe"); v != "" {
		id, err := spiffeid.Parse(v)
		if err != nil {
			return ClientIdentity{}, err
		}
		return ClientIdentity{
			Namespace: id.Namespace,
			Name:      id.Name,
		}, nil
	}

	return ClientIdentity{}, fmt.Errorf("missing client identity query parameter")
}

// Verifier verifies that the caller is authorized to request bundles for the
// given identity. Implementations may check mTLS client certificates (SPIFFE)
// or JWT bearer tokens (Kubernetes service accounts).
type Verifier interface {
	Verify(r *http.Request, id ClientIdentity) error
}

// NoopVerifier performs no verification. Used when the service does not yet
// terminate TLS or validate tokens (verification is handled upstream).
type NoopVerifier struct{}

func (NoopVerifier) Verify(_ *http.Request, _ ClientIdentity) error {
	return nil
}
