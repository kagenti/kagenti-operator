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

package signature

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var x5cLogger = ctrl.Log.WithName("signature").WithName("x5c")

var (
	x5cChainValidationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rossoctl_x5c_chain_validation_total",
			Help: "Total x5c chain validation attempts",
		},
		[]string{"valid", "reason"},
	)
	x5cTrustBundleAgeSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "rossoctl_x5c_trust_bundle_age_seconds",
			Help: "Age of the cached trust bundle in seconds",
		},
	)
	x5cTrustBundleLoadErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rossoctl_x5c_trust_bundle_load_errors_total",
			Help: "Trust bundle load/parse failures",
		},
		[]string{"reason"},
	)
	x5cBindingTrustDomainMismatchTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "rossoctl_x5c_binding_trust_domain_mismatch_total",
			Help: "Count of cert SAN SPIFFE ID trust domain mismatches",
		},
	)
)

func init() {
	for _, c := range []prometheus.Collector{
		x5cChainValidationTotal,
		x5cTrustBundleAgeSeconds,
		x5cTrustBundleLoadErrorsTotal,
		x5cBindingTrustDomainMismatchTotal,
	} {
		if err := metrics.Registry.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// X5CProvider verifies JWS signatures via x5c chains against a SPIRE trust bundle.
// The trust bundle ConfigMap may contain either PEM certificates (e.g. bundle.crt
// from ZTWIM/SPIRE) or SPIFFE JSON (from SPIRE's BundlePublisher k8s_configmap plugin).
// The format is auto-detected at load time.
type X5CProvider struct {
	client          client.Client
	configMapName   string
	configMapNS     string
	configMapKey    string
	refreshInterval time.Duration

	mu             sync.RWMutex
	trustBundle    *x509.CertPool
	lastBundleLoad time.Time
	bundleHash     string // SHA-256 of raw bundle data for change detection
}

func NewX5CProvider(config *Config) (*X5CProvider, error) {
	if config.Client == nil {
		return nil, fmt.Errorf("kubernetes client is required for x5c provider")
	}
	if config.TrustBundleConfigMapName == "" {
		return nil, fmt.Errorf("trust bundle configmap name is required for x5c provider")
	}
	if config.TrustBundleConfigMapNS == "" {
		return nil, fmt.Errorf("trust bundle configmap namespace is required for x5c provider")
	}

	configMapKey := config.TrustBundleConfigMapKey
	if configMapKey == "" {
		configMapKey = "bundle.spiffe"
	}
	refreshInterval := config.TrustBundleRefreshInterval
	if refreshInterval == 0 {
		refreshInterval = 5 * time.Minute
	}

	return &X5CProvider{
		client:          config.Client,
		configMapName:   config.TrustBundleConfigMapName,
		configMapNS:     config.TrustBundleConfigMapNS,
		configMapKey:    configMapKey,
		refreshInterval: refreshInterval,
	}, nil
}

func (p *X5CProvider) Name() string { return "x5c" }

func (p *X5CProvider) VerifySignature(ctx context.Context, cardData *agentv1alpha1.AgentCardData,
	signatures []agentv1alpha1.AgentCardSignature) (*VerificationResult, error) {

	if err := p.maybeRefreshTrustBundle(ctx); err != nil {
		return &VerificationResult{
			Verified: false,
			Details:  fmt.Sprintf("trust bundle unavailable: %v", err),
		}, nil
	}

	for i := range signatures {
		sig := &signatures[i]

		header, err := DecodeProtectedHeader(sig.Protected)
		if err != nil || len(header.X5C) == 0 {
			continue
		}

		certs, err := parseX5CCerts(header.X5C)
		if err != nil || len(certs) == 0 {
			x5cChainValidationTotal.WithLabelValues("false", "parse_error").Inc()
			continue
		}

		leaf := certs[0]
		intermediates := certs[1:]

		if err := p.validateChain(leaf, intermediates); err != nil {
			x5cChainValidationTotal.WithLabelValues("false", "chain_invalid").Inc()
			return &VerificationResult{
				Verified: false,
				Details:  fmt.Sprintf("x5c chain validation failed: %v", err),
			}, nil
		}

		spiffeID, err := extractSpiffeIDFromCert(leaf)
		if err != nil {
			x5cChainValidationTotal.WithLabelValues("false", "san_invalid").Inc()
			return &VerificationResult{
				Verified: false,
				Details:  fmt.Sprintf("leaf certificate SAN validation failed: %v", err),
			}, nil
		}

		publicKeyPEM, err := MarshalPublicKeyToPEM(leaf.PublicKey)
		if err != nil {
			continue
		}

		result, verifyErr := VerifyJWS(cardData, sig, publicKeyPEM)
		if verifyErr == nil && result != nil && result.Verified {
			x5cChainValidationTotal.WithLabelValues("true", "ok").Inc()
			result.SpiffeID = spiffeID
			result.LeafNotAfter = leaf.NotAfter
			return result, nil
		}

		x5cChainValidationTotal.WithLabelValues("false", "jws_invalid").Inc()
	}

	return &VerificationResult{
		Verified: false,
		Details:  "No signature verified via x5c chain validation",
	}, nil
}

// validateChain verifies the x5c chain against the trust bundle.
// Uses ExtKeyUsageAny because SPIRE SVIDs may lack ServerAuth EKU.
//
// Option B: CurrentTime is pinned to just after the leaf's NotBefore so that
// expired SVID leaf certs still verify as long as the issuing CA remains in
// the trust bundle. The operator handles freshness via proactive workload
// restarts before SVID expiry and on CA rotation.
func (p *X5CProvider) validateChain(leaf *x509.Certificate, intermediates []*x509.Certificate) error {
	if len(intermediates)+1 > 3 {
		return fmt.Errorf("certificate chain too deep: %d (max 3)", len(intermediates)+1)
	}

	intermediatePool := x509.NewCertPool()
	for _, cert := range intermediates {
		intermediatePool.AddCert(cert)
	}

	p.mu.RLock()
	roots := p.trustBundle
	p.mu.RUnlock()

	opts := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediatePool,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		CurrentTime:   leaf.NotBefore.Add(time.Second),
	}

	if _, err := leaf.Verify(opts); err != nil {
		return fmt.Errorf("chain verification failed: %w", err)
	}
	return nil
}

// extractSpiffeIDFromCert returns the single spiffe:// URI from leaf cert SANs.
func extractSpiffeIDFromCert(leaf *x509.Certificate) (string, error) {
	var spiffeIDs []string
	for _, uri := range leaf.URIs {
		if uri.Scheme == "spiffe" {
			spiffeIDs = append(spiffeIDs, uri.String())
		}
	}

	if len(spiffeIDs) == 0 {
		return "", fmt.Errorf("no spiffe:// URI in leaf certificate SANs (found %d URIs total)", len(leaf.URIs))
	}
	if len(spiffeIDs) > 1 {
		return "", fmt.Errorf("multiple spiffe:// URIs in leaf certificate SANs (expected exactly 1, found %d)", len(spiffeIDs))
	}

	spiffeID := spiffeIDs[0]
	parsed, err := url.Parse(spiffeID)
	if err != nil {
		return "", fmt.Errorf("malformed SPIFFE ID URI %q: %w", spiffeID, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("SPIFFE ID %q has empty trust domain", spiffeID)
	}

	return spiffeID, nil
}

func parseX5CCerts(x5c []string) ([]*x509.Certificate, error) {
	certs := make([]*x509.Certificate, 0, len(x5c))
	for i, b64 := range x5c {
		// x5c uses standard base64, not base64url (RFC 7515 §4.1.6)
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("x5c[%d]: base64 decode failed: %w", i, err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("x5c[%d]: certificate parse failed: %w", i, err)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

func (p *X5CProvider) maybeRefreshTrustBundle(ctx context.Context) error {
	p.mu.RLock()
	needsInitialLoad := p.trustBundle == nil
	age := time.Since(p.lastBundleLoad)
	p.mu.RUnlock()

	if needsInitialLoad {
		if err := p.refreshTrustBundle(ctx); err != nil {
			return fmt.Errorf("initial trust bundle load failed: %w", err)
		}
		return nil
	}

	x5cTrustBundleAgeSeconds.Set(age.Seconds())

	if age < p.refreshInterval {
		return nil
	}

	if err := p.refreshTrustBundle(ctx); err != nil {
		x5cLogger.Error(err, "trust bundle refresh failed, continuing with cached bundle")
		x5cTrustBundleLoadErrorsTotal.WithLabelValues("refresh_failed").Inc()
	}
	return nil
}

func (p *X5CProvider) BundleHash() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bundleHash
}

// spiffeBundleJSON is the minimal structure of a SPIFFE trust bundle document.
type spiffeBundleJSON struct {
	Keys []spiffeBundleKey `json:"keys"`
}

type spiffeBundleKey struct {
	Use string   `json:"use"`
	X5C []string `json:"x5c"`
}

func (p *X5CProvider) refreshTrustBundle(ctx context.Context) error {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: p.configMapName, Namespace: p.configMapNS}
	if err := p.client.Get(ctx, key, cm); err != nil {
		x5cTrustBundleLoadErrorsTotal.WithLabelValues("configmap_not_found").Inc()
		return fmt.Errorf("failed to get trust bundle configmap %s/%s: %w", p.configMapNS, p.configMapName, err)
	}

	raw, ok := cm.Data[p.configMapKey]
	if !ok || raw == "" {
		x5cTrustBundleLoadErrorsTotal.WithLabelValues("empty_bundle").Inc()
		return fmt.Errorf("trust bundle configmap key %q not found or empty", p.configMapKey)
	}

	newHash := hashString(raw)

	var pool *x509.CertPool
	var count int
	var format string

	if strings.Contains(raw, "-----BEGIN CERTIFICATE-----") {
		pool, count = parsePEMBundle(raw)
		format = "PEM"
	} else {
		var err error
		pool, count, err = parseSPIFFEJSONBundle(raw)
		if err != nil {
			return err
		}
		format = "SPIFFE JSON"
	}

	if count == 0 {
		x5cTrustBundleLoadErrorsTotal.WithLabelValues("empty_bundle").Inc()
		return fmt.Errorf("trust bundle contains no certificates (format: %s)", format)
	}

	p.mu.Lock()
	oldHash := p.bundleHash
	p.trustBundle = pool
	p.lastBundleLoad = time.Now()
	p.bundleHash = newHash
	p.mu.Unlock()

	if oldHash != "" && oldHash != newHash {
		x5cLogger.Info("Trust bundle changed (CA rotation detected)", "format", format, "certificates", count)
	} else {
		x5cLogger.Info("Trust bundle loaded", "format", format, "certificates", count)
	}
	return nil
}

// parsePEMBundle parses PEM-encoded certificates (e.g. bundle.crt from ZTWIM/SPIRE).
func parsePEMBundle(raw string) (*x509.CertPool, int) {
	pool := x509.NewCertPool()
	count := 0
	rest := []byte(raw)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			x5cTrustBundleLoadErrorsTotal.WithLabelValues("invalid_cert").Inc()
			x5cLogger.Error(err, "Skipping invalid PEM certificate block")
			continue
		}
		pool.AddCert(cert)
		count++
	}
	return pool, count
}

// parseSPIFFEJSONBundle parses a SPIFFE trust bundle document (JSON with x5c keys).
func parseSPIFFEJSONBundle(raw string) (*x509.CertPool, int, error) {
	var bundle spiffeBundleJSON
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		x5cTrustBundleLoadErrorsTotal.WithLabelValues("invalid_json").Inc()
		return nil, 0, fmt.Errorf("failed to parse SPIFFE bundle JSON: %w", err)
	}

	pool := x509.NewCertPool()
	count := 0
	for _, k := range bundle.Keys {
		if k.Use != "x509-svid" {
			continue
		}
		for _, b64 := range k.X5C {
			der, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				x5cTrustBundleLoadErrorsTotal.WithLabelValues("invalid_base64").Inc()
				return nil, 0, fmt.Errorf("failed to decode x5c cert from SPIFFE bundle: %w", err)
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				x5cTrustBundleLoadErrorsTotal.WithLabelValues("invalid_cert").Inc()
				return nil, 0, fmt.Errorf("failed to parse certificate from SPIFFE bundle: %w", err)
			}
			pool.AddCert(cert)
			count++
		}
	}
	return pool, count, nil
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func IncrementTrustDomainMismatch() {
	x5cBindingTrustDomainMismatchTotal.Inc()
}

// SetTrustBundleForTest injects a trust bundle for unit testing only.
func (p *X5CProvider) SetTrustBundleForTest(pool *x509.CertPool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trustBundle = pool
	p.lastBundleLoad = time.Now()
	p.refreshInterval = 1 * time.Hour
	p.bundleHash = "test"
}
