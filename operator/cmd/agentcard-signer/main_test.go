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

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/signature"
)

// --- Test CA helpers (mirrors x5c_test.go pattern) ---

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
		NotBefore:             time.Now().Add(-1 * time.Hour),
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

func (ca *testCA) issueLeaf(t *testing.T, pub interface{}, spiffeID string) (*x509.Certificate, []byte) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "Test Leaf"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	if spiffeID != "" {
		u, _ := url.Parse(spiffeID)
		tmpl.URIs = append(tmpl.URIs, u)
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	return cert, certDER
}

func testCard() *agentv1alpha1.AgentCardData {
	return &agentv1alpha1.AgentCardData{
		Name:    "test-agent",
		Version: "1.0.0",
		URL:     "https://test.example.com/.well-known/agent-card.json",
	}
}

// --- SignCard tests (via shared library) ---

func TestSignCard_ECDSA_P256(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/ns/default/sa/test")

	card := testCard()
	output, err := signature.SignCard(card, key, []*x509.Certificate{leaf, ca.Cert})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(parsed.Signatures) != 1 {
		t.Fatalf("expected 1 signature, got %d", len(parsed.Signatures))
	}

	header, err := signature.DecodeProtectedHeader(parsed.Signatures[0].Protected)
	if err != nil {
		t.Fatalf("failed to decode protected header: %v", err)
	}
	if header.Algorithm != "ES256" {
		t.Errorf("expected alg=ES256, got %s", header.Algorithm)
	}
	if header.Type != "JOSE" {
		t.Errorf("expected typ=JOSE, got %s", header.Type)
	}
	if len(header.X5C) != 2 {
		t.Errorf("expected 2 certs in x5c, got %d", len(header.X5C))
	}
}

func TestSignCard_ECDSA_P384(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/agent")

	card := testCard()
	output, err := signature.SignCard(card, key, []*x509.Certificate{leaf})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	header, _ := signature.DecodeProtectedHeader(parsed.Signatures[0].Protected)
	if header.Algorithm != "ES384" {
		t.Errorf("expected alg=ES384, got %s", header.Algorithm)
	}
}

func TestSignCard_RSA(t *testing.T) {
	ca := newTestCA(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/rsa-agent")

	card := testCard()
	output, err := signature.SignCard(card, key, []*x509.Certificate{leaf})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	header, _ := signature.DecodeProtectedHeader(parsed.Signatures[0].Protected)
	if header.Algorithm != "RS256" {
		t.Errorf("expected alg=RS256, got %s", header.Algorithm)
	}
}

func TestSignCard_NilCardData(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err := signature.SignCard(nil, key, []*x509.Certificate{{}})
	if err == nil {
		t.Error("expected error for nil card data")
	}
}

func TestSignCard_NoCertificates(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err := signature.SignCard(testCard(), key, nil)
	if err == nil {
		t.Error("expected error for empty cert chain")
	}
}

// --- ECDSA raw R||S encoding test ---

func TestSignCard_ECDSA_RawRS_ByteLength(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/agent")

	card := testCard()
	output, err := signature.SignCard(card, key, []*x509.Certificate{leaf})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("failed to unmarshal signed card: %v", err)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parsed.Signatures[0].Signature)
	if err != nil {
		t.Fatalf("failed to decode signature: %v", err)
	}

	if len(sigBytes) != 64 {
		t.Errorf("ES256 raw R||S signature must be 64 bytes, got %d (likely DER-encoded)", len(sigBytes))
	}
}

func TestSignCard_ECDSA_P384_RawRS_ByteLength(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/agent")

	card := testCard()
	output, err := signature.SignCard(card, key, []*x509.Certificate{leaf})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("failed to unmarshal signed card: %v", err)
	}

	sigBytes, _ := base64.RawURLEncoding.DecodeString(parsed.Signatures[0].Signature)
	if len(sigBytes) != 96 {
		t.Errorf("ES384 raw R||S signature must be 96 bytes, got %d", len(sigBytes))
	}
}

// --- x5c header construction tests ---

func TestSignCard_X5C_StandardBase64(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, leafDER := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/agent")

	card := testCard()
	output, _ := signature.SignCard(card, key, []*x509.Certificate{leaf, ca.Cert})

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("failed to unmarshal signed card: %v", err)
	}

	header, _ := signature.DecodeProtectedHeader(parsed.Signatures[0].Protected)

	decoded, err := base64.StdEncoding.DecodeString(header.X5C[0])
	if err != nil {
		t.Fatalf("x5c[0] is not valid standard base64: %v", err)
	}
	if string(decoded) != string(leafDER) {
		t.Error("x5c[0] does not match leaf certificate DER")
	}
}

func TestSignCard_X5C_LeafFirst(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/agent")

	card := testCard()
	output, _ := signature.SignCard(card, key, []*x509.Certificate{leaf, ca.Cert})

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("failed to unmarshal signed card: %v", err)
	}

	header, _ := signature.DecodeProtectedHeader(parsed.Signatures[0].Protected)

	leafDER, _ := base64.StdEncoding.DecodeString(header.X5C[0])
	parsedLeaf, _ := x509.ParseCertificate(leafDER)
	if parsedLeaf.IsCA {
		t.Error("x5c[0] should be the leaf (non-CA), not the CA")
	}

	caDER, _ := base64.StdEncoding.DecodeString(header.X5C[1])
	parsedCA, _ := x509.ParseCertificate(caDER)
	if !parsedCA.IsCA {
		t.Error("x5c[1] should be the CA certificate")
	}
}

// --- kid derivation test ---

func TestComputeKID(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/agent")

	kid := signature.ComputeKID(leaf)

	fp := sha256.Sum256(leaf.Raw)
	_ = big.NewInt(0).SetBytes(fp[:8]).Text(16)
	if len(kid) != 16 {
		t.Errorf("expected kid length 16, got %d: %s", len(kid), kid)
	}
}

// --- AlgorithmForKey tests ---

func TestAlgorithmForKey_ECDSA_P256(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	alg, err := signature.AlgorithmForKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if alg != "ES256" {
		t.Errorf("expected ES256, got %s", alg)
	}
}

func TestAlgorithmForKey_ECDSA_P384(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	alg, err := signature.AlgorithmForKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if alg != "ES384" {
		t.Errorf("expected ES384, got %s", alg)
	}
}

func TestAlgorithmForKey_ECDSA_P521(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	alg, err := signature.AlgorithmForKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if alg != "ES512" {
		t.Errorf("expected ES512, got %s", alg)
	}
}

func TestAlgorithmForKey_RSA(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	alg, err := signature.AlgorithmForKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if alg != "RS256" {
		t.Errorf("expected RS256, got %s", alg)
	}
}

func TestAlgorithmForKey_RSA_TooSmall(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	_, err := signature.AlgorithmForKey(&key.PublicKey)
	if err == nil {
		t.Error("expected error for 1024-bit RSA key")
	}
}

// --- ZeroPrivateKey tests ---

func TestZeroPrivateKey_ECDSA(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signature.ZeroPrivateKey(key)
	if key.D.Sign() != 0 {
		t.Error("expected ECDSA D to be zeroed")
	}
}

func TestZeroPrivateKey_RSA(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signature.ZeroPrivateKey(key)
	if key.D.Sign() != 0 {
		t.Error("expected RSA D to be zeroed")
	}
	for i, p := range key.Primes {
		if p.Sign() != 0 {
			t.Errorf("expected RSA prime[%d] to be zeroed", i)
		}
	}
}

// --- Canonical JSON cross-validation ---

func TestSignCard_CanonicalJSON_CrossValidation(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/agent")

	card := testCard()
	output, err := signature.SignCard(card, key, []*x509.Certificate{leaf})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("failed to unmarshal signed card: %v", err)
	}

	cardWithoutSigs := parsed
	cardWithoutSigs.Signatures = nil

	sig := parsed.Signatures[0]
	pubPEM, _ := signature.MarshalPublicKeyToPEM(&key.PublicKey)
	result, err := signature.VerifyJWS(&cardWithoutSigs, &sig, pubPEM)
	if err != nil {
		t.Fatalf("VerifyJWS error: %v", err)
	}
	if !result.Verified {
		t.Errorf("cross-validation failed: signer output not verified by VerifyJWS: %s", result.Details)
	}
}

// --- End-to-end: signer output verified by X5CProvider ---

func TestSignCard_VerifiedByX5CProvider(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, _ := ca.issueLeaf(t, &key.PublicKey, "spiffe://example.org/ns/default/sa/test")

	card := testCard()
	output, err := signature.SignCard(card, key, []*x509.Certificate{leaf, ca.Cert})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("failed to unmarshal signed card: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	provider := &signature.X5CProvider{}
	provider.SetTrustBundleForTest(pool)

	cardWithoutSigs := parsed
	cardWithoutSigs.Signatures = nil
	result, err := provider.VerifySignature(t.Context(), &cardWithoutSigs, parsed.Signatures)
	if err != nil {
		t.Fatalf("X5CProvider.VerifySignature error: %v", err)
	}
	if !result.Verified {
		t.Errorf("X5CProvider rejected signer output: %s", result.Details)
	}
	if result.SpiffeID != "spiffe://example.org/ns/default/sa/test" {
		t.Errorf("expected SPIFFE ID from cert SAN, got %q", result.SpiffeID)
	}
}

// --- writeConfigMap tests ---

func TestWriteConfigMapWithClient_Create(t *testing.T) {
	fakeClient := k8sfake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	cardJSON := []byte(`{"name":"test-agent","version":"1.0"}`)

	err := writeConfigMapWithClient(context.Background(), fakeClient, "my-agent", "test-ns", cardJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := fakeClient.CoreV1().ConfigMaps("test-ns").
		Get(context.Background(), "my-agent-card-signed", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap not created: %v", err)
	}
	if cm.Data["agent-card.json"] != string(cardJSON) {
		t.Errorf("ConfigMap data mismatch: got %q", cm.Data["agent-card.json"])
	}
}

func TestWriteConfigMapWithClient_Update(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-agent-card-signed", Namespace: "test-ns"},
		Data:       map[string]string{"agent-card.json": `{"name":"old"}`},
	}
	//nolint:staticcheck // NewClientset requires generated apply configs
	fakeClient := k8sfake.NewSimpleClientset(existing)

	newCardJSON := []byte(`{"name":"updated-agent","version":"2.0"}`)
	err := writeConfigMapWithClient(context.Background(), fakeClient, "my-agent", "test-ns", newCardJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, _ := fakeClient.CoreV1().ConfigMaps("test-ns").
		Get(context.Background(), "my-agent-card-signed", metav1.GetOptions{})
	if cm.Data["agent-card.json"] != string(newCardJSON) {
		t.Errorf("ConfigMap not updated: got %q", cm.Data["agent-card.json"])
	}
}

func TestWriteConfigMap_MissingEnvVars(t *testing.T) {
	t.Setenv("AGENT_NAME", "")
	t.Setenv("POD_NAMESPACE", "")

	err := writeConfigMap(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected error when env vars are missing")
	}
}
