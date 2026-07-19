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

package agentcard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var fetcherLogger = ctrl.Log.WithName("agentcard").WithName("fetcher")

const (
	A2AProtocol            = "a2a"
	A2AAgentCardPath       = "/.well-known/agent-card.json"
	A2ALegacyAgentCardPath = "/.well-known/agent.json"
	DefaultFetchTimeout    = 10 * time.Second
)

const (
	SignedCardConfigMapSuffix = "-card-signed"
	// SignedAgentCardConfigMapKey is the preferred ConfigMap key for sigstore-a2a SignedAgentCard JSON.
	SignedAgentCardConfigMapKey = "signed-agent-card.json"
	// SignedCardLegacyConfigMapKey holds a plain A2A agent card or legacy layouts.
	SignedCardLegacyConfigMapKey = "agent-card.json"
)

// ConfigMapName returns the expected ConfigMap name for a signed agent card.
func ConfigMapName(agentName string) string {
	return agentName + SignedCardConfigMapSuffix
}

// FetchResult is the outcome of fetching agent card content from ConfigMap and/or HTTP.
type FetchResult struct {
	CardData      *agentv1alpha1.AgentCardData
	AgentSpiffeID string
	// RawSignedAgentCardJSON is non-nil when the source document was a SignedAgentCard
	// (agentCard + attestations). Used for Sigstore bundle verification.
	RawSignedAgentCardJSON []byte
}

type Fetcher interface {
	Fetch(ctx context.Context, protocol, serviceURL, agentName, namespace string,
	) (*FetchResult, error)
}

type DefaultFetcher struct {
	httpClient *http.Client
}

func NewFetcher() Fetcher {
	return &DefaultFetcher{
		httpClient: &http.Client{
			Timeout: DefaultFetchTimeout,
		},
	}
}

func (f *DefaultFetcher) Fetch(
	ctx context.Context, protocol, serviceURL, _, _ string,
) (*FetchResult, error) {
	switch protocol {
	case A2AProtocol:
		return f.fetchA2ACard(ctx, serviceURL)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", protocol)
	}
}

func (f *DefaultFetcher) fetchA2ACard(ctx context.Context, serviceURL string) (*FetchResult, error) {
	res, err := f.fetchAgentCardFromPath(ctx, serviceURL, A2AAgentCardPath)
	if err == nil {
		return res, nil
	}

	if !errors.Is(err, errNotFound) {
		return nil, err
	}

	fetcherLogger.Info("Agent card not found at current endpoint, trying legacy endpoint",
		"currentPath", A2AAgentCardPath,
		"legacyPath", A2ALegacyAgentCardPath)

	res, legacyErr := f.fetchAgentCardFromPath(ctx, serviceURL, A2ALegacyAgentCardPath)
	if legacyErr != nil {
		return nil, legacyErr
	}

	fetcherLogger.Info("Agent card served from deprecated endpoint",
		"deprecated", true,
		"migrateTo", A2AAgentCardPath,
		"legacyPath", A2ALegacyAgentCardPath,
		"agentName", res.CardData.Name)

	return res, nil
}

// errNotFound is returned when the agent card endpoint returns HTTP 404.
var errNotFound = errors.New("agent card not found")

// maxCardSize caps the response body read to prevent memory exhaustion.
const maxCardSize = 1 << 20 // 1 MiB

// doHTTPFetch performs a GET request and returns the raw response body and TLS
// connection state. It handles 404 (errNotFound), non-200 errors, and limits
// the body read to maxCardSize.
func doHTTPFetch(ctx context.Context, httpClient *http.Client, fetchURL string) ([]byte, *tls.ConnectionState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch agent card: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, errNotFound
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxCardSize))
		return nil, nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCardSize))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return body, resp.TLS, nil
}

func (f *DefaultFetcher) fetchAgentCardFromPath(
	ctx context.Context, serviceURL, path string,
) (*FetchResult, error) {
	agentCardURL := serviceURL + path
	fetcherLogger.Info("Fetching A2A agent card", "url", agentCardURL)

	body, _, err := doHTTPFetch(ctx, f.httpClient, agentCardURL)
	if err != nil {
		return nil, err
	}

	return decodeAgentCardPayload(body)
}

func decodeAgentCardPayload(body []byte) (*FetchResult, error) {
	var envelopeProbe struct {
		AgentCard            json.RawMessage `json:"agentCard"`
		Attestations         json.RawMessage `json:"attestations"`
		VerificationMaterial json.RawMessage `json:"verificationMaterial"`
	}
	if err := json.Unmarshal(body, &envelopeProbe); err != nil {
		return nil, fmt.Errorf("failed to parse agent card JSON: %w", err)
	}
	hasAtt := len(envelopeProbe.Attestations) > 0 || len(envelopeProbe.VerificationMaterial) > 0
	if len(envelopeProbe.AgentCard) > 0 && hasAtt {
		var inner agentv1alpha1.AgentCardData
		if err := json.Unmarshal(envelopeProbe.AgentCard, &inner); err != nil {
			return nil, fmt.Errorf("failed to parse embedded agentCard: %w", err)
		}
		fetcherLogger.Info("Fetched SignedAgentCard envelope over HTTP",
			"name", inner.Name,
			"version", inner.Version)
		return &FetchResult{CardData: &inner, RawSignedAgentCardJSON: append([]byte(nil), body...)}, nil
	}

	var agentCardData agentv1alpha1.AgentCardData
	if err := json.Unmarshal(body, &agentCardData); err != nil {
		return nil, fmt.Errorf("failed to parse agent card JSON: %w", err)
	}

	fetcherLogger.Info("Successfully fetched agent card",
		"name", agentCardData.Name,
		"version", agentCardData.Version,
		"url", agentCardData.URL)

	return &FetchResult{CardData: &agentCardData}, nil
}

// ConfigMapFetcher reads signed agent cards from a ConfigMap before falling
// back to the standard HTTP fetch. The init-container agentcard-signer writes
// the signed card to a ConfigMap named "{agentName}-card-signed".
type ConfigMapFetcher struct {
	reader   client.Reader
	fallback *DefaultFetcher
}

func NewConfigMapFetcher(reader client.Reader) Fetcher {
	return &ConfigMapFetcher{
		reader:   reader,
		fallback: &DefaultFetcher{httpClient: &http.Client{Timeout: DefaultFetchTimeout}},
	}
}

func (f *ConfigMapFetcher) Fetch(
	ctx context.Context, protocol, serviceURL, agentName, namespace string,
) (*FetchResult, error) {
	if agentName != "" && namespace != "" {
		cmName := agentName + SignedCardConfigMapSuffix
		var cm corev1.ConfigMap
		err := f.reader.Get(ctx, types.NamespacedName{Name: cmName, Namespace: namespace}, &cm)
		if err == nil {
			var raw string
			var keyUsed string
			if s, ok := cm.Data[SignedAgentCardConfigMapKey]; ok {
				raw = s
				keyUsed = SignedAgentCardConfigMapKey
			} else if s, ok := cm.Data[SignedCardLegacyConfigMapKey]; ok {
				raw = s
				keyUsed = SignedCardLegacyConfigMapKey
			}
			if raw != "" {
				result, decErr := decodeAgentCardPayload([]byte(raw))
				if decErr == nil {
					fetcherLogger.Info("Fetched agent card from ConfigMap",
						"configMap", cmName, "namespace", namespace, "key", keyUsed, "agentName", result.CardData.Name)
					return result, nil
				}
				fetcherLogger.Info("ConfigMap contains invalid JSON, falling back to HTTP",
					"configMap", cmName, "namespace", namespace)
			}
		} else if !apierrors.IsNotFound(err) {
			fetcherLogger.Error(err, "Failed to read ConfigMap, falling back to HTTP",
				"configMap", cmName, "namespace", namespace)
		}
	}

	return f.fallback.Fetch(ctx, protocol, serviceURL, agentName, namespace)
}

func GetServiceURL(agentName, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", agentName, namespace, port)
}

// GetSecureServiceURL returns an HTTPS URL for the agent's TLS port.
func GetSecureServiceURL(agentName, namespace string, port int32) string {
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", agentName, namespace, port)
}

// AuthenticatedFetcher performs mTLS-authenticated fetches and returns
// identity information from the TLS handshake alongside the card data.
type AuthenticatedFetcher interface {
	FetchAuthenticated(ctx context.Context, protocol, serviceURL string) (*FetchResult, error)
}

// SpiffeFetcher implements AuthenticatedFetcher using go-spiffe mTLS.
// It verifies the agent belongs to the configured trust domain and extracts
// the SPIFFE ID from the peer certificate. The HTTP client is reused across
// fetches for connection pooling; the TLS config dynamically reads the latest
// SVID from the X509Source on each handshake.
type SpiffeFetcher struct {
	x509Source  *workloadapi.X509Source
	trustDomain string
	httpClient  *http.Client
}

// NewSpiffeFetcher creates a SpiffeFetcher that uses the provided X509Source
// for mTLS and validates peers against the given trust domain.
func NewSpiffeFetcher(
	source *workloadapi.X509Source, trustDomain string,
) (*SpiffeFetcher, error) {
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return nil, fmt.Errorf("invalid SPIFFE trust domain %q: %w", trustDomain, err)
	}
	tlsCfg := tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeMemberOf(td))
	return &SpiffeFetcher{
		x509Source:  source,
		trustDomain: trustDomain,
		httpClient: &http.Client{
			Timeout:   DefaultFetchTimeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

func (f *SpiffeFetcher) FetchAuthenticated(ctx context.Context, protocol, serviceURL string) (*FetchResult, error) {
	switch protocol {
	case A2AProtocol:
		return f.fetchA2ACardAuthenticated(ctx, serviceURL)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", protocol)
	}
}

func (f *SpiffeFetcher) fetchA2ACardAuthenticated(ctx context.Context, serviceURL string) (*FetchResult, error) {
	result, err := f.fetchAuthenticatedFromPath(ctx, serviceURL, A2AAgentCardPath)
	if err == nil {
		return result, nil
	}

	if !errors.Is(err, errNotFound) {
		return nil, err
	}

	fetcherLogger.Info("Agent card not found at current endpoint, trying legacy endpoint (authenticated)",
		"currentPath", A2AAgentCardPath,
		"legacyPath", A2ALegacyAgentCardPath)

	result, legacyErr := f.fetchAuthenticatedFromPath(ctx, serviceURL, A2ALegacyAgentCardPath)
	if legacyErr != nil {
		return nil, legacyErr
	}

	fetcherLogger.Info("Agent card served from deprecated endpoint (authenticated)",
		"deprecated", true,
		"migrateTo", A2AAgentCardPath,
		"legacyPath", A2ALegacyAgentCardPath,
		"agentName", result.CardData.Name)

	return result, nil
}

func (f *SpiffeFetcher) fetchAuthenticatedFromPath(ctx context.Context, serviceURL, path string) (*FetchResult, error) {
	agentCardURL := serviceURL + path
	fetcherLogger.Info("Fetching A2A agent card (mTLS)", "url", agentCardURL)

	body, tlsState, err := doHTTPFetch(ctx, f.httpClient, agentCardURL)
	if err != nil {
		return nil, err
	}

	result, err := decodeAgentCardPayload(body)
	if err != nil {
		return nil, err
	}

	agentSpiffeID := extractSpiffeIDFromTLS(tlsState)
	result.AgentSpiffeID = agentSpiffeID

	fetcherLogger.Info("Successfully fetched agent card (mTLS)",
		"name", result.CardData.Name,
		"version", result.CardData.Version,
		"agentSpiffeID", agentSpiffeID)

	return result, nil
}

// extractSpiffeIDFromTLS returns the SPIFFE ID from the verified peer
// certificate's URI SANs. Prefers VerifiedChains (post-validation) over
// PeerCertificates (pre-validation) for defense-in-depth.
func extractSpiffeIDFromTLS(state *tls.ConnectionState) string {
	if state == nil {
		return ""
	}
	if len(state.VerifiedChains) > 0 && len(state.VerifiedChains[0]) > 0 {
		return spiffeIDFromCert(state.VerifiedChains[0][0])
	}
	if len(state.PeerCertificates) > 0 {
		return spiffeIDFromCert(state.PeerCertificates[0])
	}
	return ""
}

func spiffeIDFromCert(cert *x509.Certificate) string {
	for _, uri := range cert.URIs {
		parsed, err := url.Parse(uri.String())
		if err != nil {
			continue
		}
		if parsed.Scheme == "spiffe" {
			return uri.String()
		}
	}
	return ""
}
