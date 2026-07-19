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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

type testCA struct {
	Key  *ecdsa.PrivateKey
	Cert *x509.Certificate
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{Key: key, Cert: cert}
}

type leafOpts struct {
	spiffeIDs   []string
	notBefore   time.Time
	notAfter    time.Time
	extKeyUsage []x509.ExtKeyUsage
}

func (ca *testCA) issueLeaf(t *testing.T, key crypto.Signer, opts leafOpts) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "Test Leaf"},
		NotBefore:    opts.notBefore,
		NotAfter:     opts.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  opts.extKeyUsage,
	}
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore = time.Now().Add(-1 * time.Hour)
	}
	if tmpl.NotAfter.IsZero() {
		tmpl.NotAfter = time.Now().Add(1 * time.Hour)
	}
	if tmpl.ExtKeyUsage == nil {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	}

	for _, id := range opts.spiffeIDs {
		u, _ := url.Parse(id)
		tmpl.URIs = append(tmpl.URIs, u)
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func newTestX5CProvider(t *testing.T, ca *testCA) *X5CProvider {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return &X5CProvider{
		trustBundle:     pool,
		lastBundleLoad:  time.Now(),
		refreshInterval: 1 * time.Hour,
		bundleHash:      "test-hash",
	}
}

func buildTestJWSWithX5C(t *testing.T, cardData *agentv1alpha1.AgentCardData, key crypto.Signer, certs []*x509.Certificate) agentv1alpha1.AgentCardSignature {
	t.Helper()

	alg := algForKey(t, key.Public())
	kid := fmt.Sprintf("%x", sha256.Sum256(certs[0].Raw))[:16]

	x5c := make([]string, len(certs))
	for i, cert := range certs {
		x5c[i] = base64.StdEncoding.EncodeToString(cert.Raw)
	}

	header := &ProtectedHeader{
		Algorithm: alg,
		KeyID:     kid,
		Type:      "JOSE",
		X5C:       x5c,
	}
	protectedB64, err := EncodeProtectedHeader(header)
	if err != nil {
		t.Fatal(err)
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatal(err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	hashFunc := crypto.SHA256
	if alg == "ES384" {
		hashFunc = crypto.SHA384
	} else if alg == "ES512" {
		hashFunc = crypto.SHA512
	}
	h := hashFunc.New()
	h.Write(signingInput)
	hashed := h.Sum(nil)

	var sigBytes []byte
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, hashed)
		if err != nil {
			t.Fatal(err)
		}
		keySize := (k.Curve.Params().BitSize + 7) / 8
		sigBytes = make([]byte, 2*keySize)
		rBytes := r.Bytes()
		sBytes := s.Bytes()
		copy(sigBytes[keySize-len(rBytes):keySize], rBytes)
		copy(sigBytes[2*keySize-len(sBytes):], sBytes)
	case *rsa.PrivateKey:
		sigBytes, err = rsa.SignPKCS1v15(rand.Reader, k, hashFunc, hashed)
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported key type: %T", key)
	}

	return agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes),
	}
}

func algForKey(t *testing.T, pub crypto.PublicKey) string {
	t.Helper()
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return "ES256"
		case elliptic.P384():
			return "ES384"
		case elliptic.P521():
			return "ES512"
		}
	case *rsa.PublicKey:
		return "RS256"
	}
	t.Fatal("unsupported key type")
	return ""
}

func testCard() *agentv1alpha1.AgentCardData {
	return &agentv1alpha1.AgentCardData{
		Name:    "test-agent",
		Version: "1.0.0",
		URL:     "https://test.example.com/.well-known/agent-card.json",
	}
}

// Valid x5c chain, valid ECDSA signature, SPIFFE ID extracted from cert SAN
func TestX5CProvider_ValidChainValidSignature(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("expected Verified=true, got false: %s", result.Details)
	}
	if result.SpiffeID != "spiffe://example.org/ns/default/sa/test" {
		t.Errorf("expected SpiffeID from cert SAN, got %q", result.SpiffeID)
	}
}

// Cert signed by untrusted CA is rejected
func TestX5CProvider_UnknownCA(t *testing.T) {
	ca := newTestCA(t)
	otherCA := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := otherCA.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for unknown CA")
	}
	if !strings.Contains(result.Details, "chain validation failed") {
		t.Errorf("expected chain validation failure, got: %s", result.Details)
	}
}

// Option B: expired leaf cert still verifies as long as the CA is in the trust bundle.
// The operator handles freshness via proactive workload restarts.
func TestX5CProvider_ExpiredLeaf_OptionB(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
		notBefore: time.Now().Add(-2 * time.Hour),
		notAfter:  time.Now().Add(-1 * time.Hour),
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Option B: expected Verified=true for expired leaf (CA still trusted), got: %s", result.Details)
	}
	if result.LeafNotAfter.IsZero() {
		t.Error("expected LeafNotAfter to be set")
	}
}

// No x5c in header falls through to "no signature verified"
func TestX5CProvider_MissingX5C(t *testing.T) {
	ca := newTestCA(t)
	provider := newTestX5CProvider(t, ca)
	card := testCard()

	header := &ProtectedHeader{Algorithm: "ES256", Type: "JOSE"}
	protectedB64, _ := EncodeProtectedHeader(header)
	sig := agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString([]byte("fake")),
	}

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for missing x5c")
	}
}

// Chain depth > 3 is rejected
func TestX5CProvider_ChainTooDeep(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})

	extras := make([]*x509.Certificate, 3)
	for i := range extras {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		extras[i] = ca.issueLeaf(t, k, leafOpts{
			spiffeIDs: []string{"spiffe://example.org/intermediate"},
		})
	}

	chain := append([]*x509.Certificate{leaf}, extras...)
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, chain)

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for chain too deep")
	}
	if !strings.Contains(result.Details, "chain too deep") {
		t.Errorf("expected 'chain too deep' in details, got: %s", result.Details)
	}
}

// Tampered payload detected by signature verification
func TestX5CProvider_TamperedPayload(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})
	provider := newTestX5CProvider(t, ca)

	originalCard := testCard()
	sig := buildTestJWSWithX5C(t, originalCard, key, []*x509.Certificate{leaf})

	tamperedCard := testCard()
	tamperedCard.Name = "tampered-agent"

	result, err := provider.VerifySignature(context.Background(), tamperedCard, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for tampered payload")
	}
}

// Tampered protected header detected by signature verification
func TestX5CProvider_TamperedHeader(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	header, _ := DecodeProtectedHeader(sig.Protected)
	header.KeyID = "tampered-kid"
	tamperedProtected, _ := EncodeProtectedHeader(header)
	sig.Protected = tamperedProtected

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for tampered header")
	}
}

// Leaf cert with no SAN URIs is rejected
func TestX5CProvider_NoSANURIs(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for cert with no SAN URIs")
	}
	if !strings.Contains(result.Details, "no spiffe:// URI") {
		t.Errorf("expected SAN validation failure, got: %s", result.Details)
	}
}

// Multiple spiffe:// URIs in leaf cert is rejected (exactly one required)
func TestX5CProvider_MultipleSpiffeURIs(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{
			"spiffe://example.org/ns/default/sa/test1",
			"spiffe://example.org/ns/default/sa/test2",
		},
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for multiple spiffe:// URIs")
	}
	if !strings.Contains(result.Details, "multiple spiffe:// URIs") {
		t.Errorf("expected multiple URIs failure, got: %s", result.Details)
	}
}

// Empty trust domain in SPIFFE ID is rejected
func TestX5CProvider_EmptyTrustDomain(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe:///workload"},
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for empty trust domain")
	}
}

// Trust bundle hot-swap: old CA fails, new CA succeeds
func TestX5CProvider_TrustBundleRotation(t *testing.T) {
	oldCA := newTestCA(t)
	newCA := newTestCA(t)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := newCA.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})

	provider := newTestX5CProvider(t, oldCA)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, _ := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if result.Verified {
		t.Error("expected Verified=false with old trust bundle")
	}

	newPool := x509.NewCertPool()
	newPool.AddCert(newCA.Cert)
	provider.mu.Lock()
	provider.trustBundle = newPool
	provider.mu.Unlock()

	result, _ = provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if !result.Verified {
		t.Errorf("expected Verified=true after trust bundle rotation, got: %s", result.Details)
	}
}

// --- Golden vector tests: signer output verified by both X5CProvider and raw VerifyJWS ---

func TestGoldenVector_SignerToX5CProvider(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/golden"},
	})

	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf, ca.Cert})

	provider := newTestX5CProvider(t, ca)
	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("golden vector: X5CProvider rejected: %s", result.Details)
	}

	pubPEM, _ := MarshalPublicKeyToPEM(&key.PublicKey)
	rawResult, err := VerifyJWS(card, &sig, pubPEM)
	if err != nil {
		t.Fatalf("VerifyJWS error: %v", err)
	}
	if !rawResult.Verified {
		t.Errorf("golden vector: raw VerifyJWS rejected: %s", rawResult.Details)
	}
}

func TestGoldenVector_RSA_SignerToX5CProvider(t *testing.T) {
	ca := newTestCA(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/rsa-golden"},
	})

	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf, ca.Cert})

	provider := newTestX5CProvider(t, ca)
	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("RSA golden vector: X5CProvider rejected: %s", result.Details)
	}

	pubPEM, _ := MarshalPublicKeyToPEM(&key.PublicKey)
	rawResult, err := VerifyJWS(card, &sig, pubPEM)
	if err != nil {
		t.Fatalf("VerifyJWS error: %v", err)
	}
	if !rawResult.Verified {
		t.Errorf("RSA golden vector: raw VerifyJWS rejected: %s", rawResult.Details)
	}
}

// --- Trust bundle rotation: old CA removed causes old signatures to fail ---

func TestX5CProvider_TrustBundleRotation_OldCARemoved(t *testing.T) {
	oldCA := newTestCA(t)
	newCA := newTestCA(t)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafSignedByOldCA := oldCA.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})

	// Start with both CAs trusted
	bothPool := x509.NewCertPool()
	bothPool.AddCert(oldCA.Cert)
	bothPool.AddCert(newCA.Cert)
	provider := &X5CProvider{
		trustBundle:     bothPool,
		lastBundleLoad:  time.Now(),
		refreshInterval: 1 * time.Hour,
	}

	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leafSignedByOldCA})

	result, _ := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if !result.Verified {
		t.Fatalf("expected Verified=true with both CAs trusted, got: %s", result.Details)
	}

	// Rotate: remove old CA, keep only new CA
	newOnlyPool := x509.NewCertPool()
	newOnlyPool.AddCert(newCA.Cert)
	provider.mu.Lock()
	provider.trustBundle = newOnlyPool
	provider.mu.Unlock()

	result, _ = provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if result.Verified {
		t.Error("expected Verified=false after old CA removed from trust bundle")
	}
	if !strings.Contains(result.Details, "chain validation failed") {
		t.Errorf("expected chain validation failure, got: %s", result.Details)
	}
}

// --- Additional negative/security tests ---

func TestX5CProvider_TamperedSignatureBytes(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	sigBytes, _ := base64.RawURLEncoding.DecodeString(sig.Signature)
	sigBytes[0] ^= 0xFF // flip first byte
	sig.Signature = base64.RawURLEncoding.EncodeToString(sigBytes)

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for tampered signature bytes")
	}
}

func TestX5CProvider_InvalidX5CBase64(t *testing.T) {
	ca := newTestCA(t)
	provider := newTestX5CProvider(t, ca)
	card := testCard()

	header := &ProtectedHeader{
		Algorithm: "ES256",
		Type:      "JOSE",
		X5C:       []string{"not-valid-base64!!!"},
	}
	protectedB64, _ := EncodeProtectedHeader(header)
	sig := agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString([]byte("fake")),
	}

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for invalid x5c base64")
	}
}

func TestX5CProvider_NonSpiffeURI(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "Test Leaf"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	u, _ := url.Parse("https://not-spiffe.example.com/identity")
	tmpl.URIs = append(tmpl.URIs, u)

	certDER, _ := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	leaf, _ := x509.ParseCertificate(certDER)

	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for non-spiffe URI in cert SAN")
	}
	if !strings.Contains(result.Details, "no spiffe:// URI") {
		t.Errorf("expected 'no spiffe:// URI' in details, got: %s", result.Details)
	}
}

// Option B: not-yet-valid leaf still verifies because time validation is skipped.
// The init-container only signs with certs it just received from SPIRE, so a
// not-yet-valid cert in practice means clock skew, which Option B tolerates.
func TestX5CProvider_NotYetValidLeaf_OptionB(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
		notBefore: time.Now().Add(1 * time.Hour),
		notAfter:  time.Now().Add(2 * time.Hour),
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Option B: expected Verified=true for not-yet-valid leaf (CA trusted), got: %s", result.Details)
	}
}

func TestX5CProvider_EmptySignatures(t *testing.T) {
	ca := newTestCA(t)
	provider := newTestX5CProvider(t, ca)
	card := testCard()

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("expected Verified=false for empty signatures slice")
	}
}

// BundleHash returns a stable hash and changes when the bundle changes
func TestX5CProvider_BundleHash(t *testing.T) {
	ca1 := newTestCA(t)
	ca2 := newTestCA(t)

	provider := newTestX5CProvider(t, ca1)
	hash1 := provider.BundleHash()
	if hash1 == "" {
		t.Error("expected non-empty bundle hash")
	}

	pool2 := x509.NewCertPool()
	pool2.AddCert(ca2.Cert)
	provider.mu.Lock()
	provider.trustBundle = pool2
	provider.bundleHash = "different"
	provider.mu.Unlock()

	hash2 := provider.BundleHash()
	if hash1 == hash2 {
		t.Error("expected different hash after bundle change")
	}
}

// LeafNotAfter is populated in verification result
func TestX5CProvider_LeafNotAfter(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	expiry := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
		notBefore: time.Now().Add(-1 * time.Minute),
		notAfter:  expiry,
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Verified {
		t.Fatalf("expected Verified=true, got: %s", result.Details)
	}
	if !result.LeafNotAfter.Truncate(time.Second).Equal(expiry) {
		t.Errorf("expected LeafNotAfter=%v, got %v", expiry, result.LeafNotAfter)
	}
}

// RSA key path works end-to-end
func TestX5CProvider_RSA_CrossValidation(t *testing.T) {
	ca := newTestCA(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/rsa-agent"},
	})
	provider := newTestX5CProvider(t, ca)
	card := testCard()
	sig := buildTestJWSWithX5C(t, card, key, []*x509.Certificate{leaf})

	result, err := provider.VerifySignature(context.Background(), card, []agentv1alpha1.AgentCardSignature{sig})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("RSA cross-validation failed: %s", result.Details)
	}
	if result.SpiffeID != "spiffe://example.org/ns/default/sa/rsa-agent" {
		t.Errorf("SpiffeID mismatch: %s", result.SpiffeID)
	}
}
