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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"testing"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

// --- Test helpers ---

// generateRSAKeyPair generates an RSA key pair and returns PEM-encoded public key.
func generateRSAKeyPair(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate RSA key: %v", err)
	}
	pubKeyDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to marshal RSA public key: %v", err)
	}
	pubKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyDER})
	return privKey, pubKeyPEM
}

// generateECDSAKeyPair generates an ECDSA key pair and returns PEM-encoded public key.
func generateECDSAKeyPair(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate ECDSA key: %v", err)
	}
	pubKeyDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to marshal ECDSA public key: %v", err)
	}
	pubKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyDER})
	return privKey, pubKeyPEM
}

// buildJWSSignature creates a JWS signature for testing.
// It builds a protected header, constructs the signing input per RFC 7515,
// and signs with the given RSA private key.
func buildJWSSignature(t *testing.T, cardData *agentv1alpha1.AgentCardData, privKey *rsa.PrivateKey, kid, _ string) agentv1alpha1.AgentCardSignature {
	t.Helper()
	header := &ProtectedHeader{
		Algorithm: "RS256",
		Type:      "JOSE",
		KeyID:     kid,
	}
	protectedB64, err := EncodeProtectedHeader(header)
	if err != nil {
		t.Fatalf("Failed to encode protected header: %v", err)
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("Failed to create canonical JSON: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	hash := sha256.Sum256(signingInput)
	sigBytes, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatalf("Failed to sign: %v", err)
	}

	return agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes),
	}
}

// buildJWSSignatureECDSA creates a JWS signature using ECDSA (ASN.1 DER, for fallback compat).
func buildJWSSignatureECDSA(t *testing.T, cardData *agentv1alpha1.AgentCardData, privKey *ecdsa.PrivateKey, kid string) agentv1alpha1.AgentCardSignature {
	t.Helper()
	header := &ProtectedHeader{
		Algorithm: "ES256",
		Type:      "JOSE",
		KeyID:     kid,
	}
	protectedB64, err := EncodeProtectedHeader(header)
	if err != nil {
		t.Fatalf("Failed to encode protected header: %v", err)
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("Failed to create canonical JSON: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	hash := sha256.Sum256(signingInput)
	sigBytes, err := ecdsa.SignASN1(rand.Reader, privKey, hash[:])
	if err != nil {
		t.Fatalf("Failed to sign: %v", err)
	}

	return agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes),
	}
}

// newCardData creates a simple AgentCardData for testing.
func newCardData(name, url, version string) *agentv1alpha1.AgentCardData {
	return &agentv1alpha1.AgentCardData{
		Name:    name,
		URL:     url,
		Version: version,
	}
}

// --- ProtectedHeader tests ---

func TestDecodeProtectedHeader(t *testing.T) {
	header := &ProtectedHeader{
		Algorithm: "RS256",
		KeyID:     "test-key",
	}
	encoded, err := EncodeProtectedHeader(header)
	if err != nil {
		t.Fatalf("EncodeProtectedHeader failed: %v", err)
	}

	decoded, err := DecodeProtectedHeader(encoded)
	if err != nil {
		t.Fatalf("DecodeProtectedHeader failed: %v", err)
	}
	if decoded.Algorithm != "RS256" {
		t.Errorf("Expected alg=RS256, got %s", decoded.Algorithm)
	}
	if decoded.KeyID != "test-key" {
		t.Errorf("Expected kid=test-key, got %s", decoded.KeyID)
	}
}

func TestDecodeProtectedHeader_InvalidBase64(t *testing.T) {
	_, err := DecodeProtectedHeader("not-valid-base64!!!")
	if err == nil {
		t.Error("Expected error for invalid base64url")
	}
}

func TestDecodeProtectedHeader_InvalidJSON(t *testing.T) {
	encoded := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	_, err := DecodeProtectedHeader(encoded)
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
}

// --- Canonical JSON tests ---

func TestCanonicalJSON_SortedKeys(t *testing.T) {
	cardData := &agentv1alpha1.AgentCardData{
		Name:    "Test Agent",
		URL:     "http://localhost:8000",
		Version: "1.0.0",
	}

	canonical, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("CreateCanonicalCardJSON failed: %v", err)
	}

	// Verify it's valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(canonical, &parsed); err != nil {
		t.Fatalf("Canonical JSON is not valid JSON: %v", err)
	}

	// Verify no structural whitespace (newlines/tabs)
	for _, b := range canonical {
		if b == '\n' || b == '\t' {
			t.Fatalf("Canonical JSON should not contain newlines or tabs: %s", canonical)
		}
	}

	// Verify keys are sorted
	got := string(canonical)
	nameIdx := indexOf(got, `"name"`)
	urlIdx := indexOf(got, `"url"`)
	versionIdx := indexOf(got, `"version"`)

	if nameIdx >= urlIdx || urlIdx >= versionIdx {
		t.Errorf("Keys not sorted: name@%d, url@%d, version@%d in %s", nameIdx, urlIdx, versionIdx, got)
	}
}

func TestCanonicalJSON_ExcludesSignatures(t *testing.T) {
	cardData := &agentv1alpha1.AgentCardData{
		Name:    "Test Agent",
		Version: "1.0.0",
		Signatures: []agentv1alpha1.AgentCardSignature{
			{Protected: "abc", Signature: "should-be-excluded"},
		},
	}

	canonical, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("CreateCanonicalCardJSON failed: %v", err)
	}

	got := string(canonical)
	if indexOf(got, "signatures") >= 0 {
		t.Errorf("Canonical JSON should NOT contain 'signatures' field: %s", got)
	}
	if indexOf(got, "should-be-excluded") >= 0 {
		t.Errorf("Canonical JSON should NOT contain signature value: %s", got)
	}
}

func TestCanonicalJSON_ExcludesEmptyFields(t *testing.T) {
	cardData := &agentv1alpha1.AgentCardData{
		Name:    "Test Agent",
		Version: "1.0.0",
	}

	canonical, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("CreateCanonicalCardJSON failed: %v", err)
	}

	got := string(canonical)
	if indexOf(got, `"url"`) >= 0 {
		t.Errorf("Canonical JSON should not include empty 'url' field: %s", got)
	}
	if indexOf(got, `"description"`) >= 0 {
		t.Errorf("Canonical JSON should not include empty 'description' field: %s", got)
	}
}

func TestCanonicalJSON_Deterministic(t *testing.T) {
	cardData := &agentv1alpha1.AgentCardData{
		Name:               "Agent",
		Description:        "A test agent",
		Version:            "2.0.0",
		URL:                "http://localhost:9000",
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"application/json"},
	}

	first, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}

	for i := 0; i < 10; i++ {
		again, err := CreateCanonicalCardJSON(cardData)
		if err != nil {
			t.Fatalf("Call %d failed: %v", i, err)
		}
		if string(first) != string(again) {
			t.Fatalf("Canonical JSON is non-deterministic:\n  first: %s\n  got:   %s", first, again)
		}
	}
}

func TestCanonicalJSON_NestedCapabilities(t *testing.T) {
	streaming := true
	push := false
	cardData := &agentv1alpha1.AgentCardData{
		Name: "Agent",
		Capabilities: &agentv1alpha1.AgentCapabilities{
			Streaming:         &streaming,
			PushNotifications: &push,
		},
	}

	canonical, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("CreateCanonicalCardJSON failed: %v", err)
	}

	got := string(canonical)
	pushIdx := indexOf(got, `"pushNotifications"`)
	streamIdx := indexOf(got, `"streaming"`)
	if pushIdx < 0 || streamIdx < 0 {
		t.Fatalf("Missing expected nested keys in: %s", got)
	}
	if pushIdx >= streamIdx {
		t.Errorf("Nested keys not sorted: pushNotifications@%d >= streaming@%d in %s", pushIdx, streamIdx, got)
	}
}

// --- JWS RSA signature verification tests ---

func TestVerifyJWS_RSA_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("Weather Agent", "http://weather:8000", "1.0.0")
	jwsSig := buildJWSSignature(t, cardData, privKey, "test-key", "")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true, got false. Details: %s", result.Details)
	}
	if result.KeyID != "test-key" {
		t.Errorf("Expected keyID=test-key, got %s", result.KeyID)
	}
}

func TestVerifyJWS_RSA_WrongKey(t *testing.T) {
	privKey, _ := generateRSAKeyPair(t)
	_, wrongPubKeyPEM := generateRSAKeyPair(t)

	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")
	jwsSig := buildJWSSignature(t, cardData, privKey, "wrong-key", "")

	result, err := VerifyJWS(cardData, &jwsSig, wrongPubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error (should return result with Verified=false): %v", err)
	}
	if result.Verified {
		t.Error("Expected verified=false for wrong key, got true")
	}
}

func TestVerifyJWS_RSA_TamperedCard(t *testing.T) {
	privKey, pubKeyPEM := generateRSAKeyPair(t)

	original := newCardData("Agent", "http://agent:8000", "1.0.0")
	jwsSig := buildJWSSignature(t, original, privKey, "key-1", "")

	// Tamper with card data after signing
	tampered := newCardData("Agent", "http://evil:8000", "1.0.0")

	result, err := VerifyJWS(tampered, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("Expected verified=false for tampered card, got true")
	}
}

// --- JWS ECDSA signature verification tests ---

func TestVerifyJWS_ECDSA_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateECDSAKeyPair(t)
	cardData := newCardData("ECDSA Agent", "http://ecdsa:8000", "1.0.0")
	jwsSig := buildJWSSignatureECDSA(t, cardData, privKey, "ecdsa-key")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true, got false. Details: %s", result.Details)
	}
}

func TestVerifyJWS_ECDSA_WrongKey(t *testing.T) {
	privKey, _ := generateECDSAKeyPair(t)
	_, wrongPubKeyPEM := generateECDSAKeyPair(t)

	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")
	jwsSig := buildJWSSignatureECDSA(t, cardData, privKey, "wrong-key")

	result, err := VerifyJWS(cardData, &jwsSig, wrongPubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result.Verified {
		t.Error("Expected verified=false for wrong key, got true")
	}
}

// --- Edge case tests ---

func TestVerifyJWS_NilSignature(t *testing.T) {
	_, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	result, err := VerifyJWS(cardData, nil, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error for nil signature: %v", err)
	}
	if result.Verified {
		t.Error("Expected verified=false for nil signature, got true")
	}
	if indexOf(result.Details, "does not contain a signature") < 0 {
		t.Errorf("Expected details to mention missing signature, got: %s", result.Details)
	}
}

func TestVerifyJWS_InvalidProtectedHeader(t *testing.T) {
	_, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	sig := &agentv1alpha1.AgentCardSignature{
		Protected: "not-valid-base64!!!",
		Signature: "fake",
	}

	_, err := VerifyJWS(cardData, sig, pubKeyPEM)
	if err == nil {
		t.Error("Expected error for invalid protected header")
	}
}

func TestVerifyJWS_InvalidPEM(t *testing.T) {
	privKey, _ := generateRSAKeyPair(t)
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")
	jwsSig := buildJWSSignature(t, cardData, privKey, "key-1", "")

	invalidPEM := []byte("not a PEM block")
	result, err := VerifyJWS(cardData, &jwsSig, invalidPEM)
	if err == nil {
		t.Error("Expected error for invalid PEM")
	}
	if result.Verified {
		t.Error("Expected verified=false for invalid PEM")
	}
}

func TestVerifyJWS_InvalidSignatureBase64(t *testing.T) {
	_, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	header := &ProtectedHeader{Algorithm: "RS256", KeyID: "k"}
	protectedB64, _ := EncodeProtectedHeader(header)

	sig := &agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: "not-valid-base64!!!",
	}

	_, err := VerifyJWS(cardData, sig, pubKeyPEM)
	if err == nil {
		t.Error("Expected error for invalid base64url signature")
	}
}

// --- Cross-algorithm compatibility test ---

func TestVerifyJWS_RSAKeyWithECDSASignature(t *testing.T) {
	ecPriv, _ := generateECDSAKeyPair(t)
	_, rsaPubPEM := generateRSAKeyPair(t)

	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")
	jwsSig := buildJWSSignatureECDSA(t, cardData, ecPriv, "ec-key")

	// Try verifying ECDSA signature with RSA public key — should fail with algorithm mismatch
	result, err := VerifyJWS(cardData, &jwsSig, rsaPubPEM)
	if err == nil {
		t.Fatal("Expected algorithm mismatch error when using RSA key with ES256 header")
	}
	if result.Verified {
		t.Error("Expected verified=false when using RSA key to verify ECDSA signature")
	}
	if indexOf(err.Error(), "algorithm mismatch") == -1 {
		t.Errorf("Expected 'algorithm mismatch' in error, got: %v", err)
	}
}

// TestVerifyJWS_AlgNone_Rejected verifies that "alg: none" is explicitly rejected.
// This prevents CVE-2015-9235 (the classic JWS "alg: none" attack).
func TestVerifyJWS_AlgNone_Rejected(t *testing.T) {
	_, rsaPubPEM := generateRSAKeyPair(t)
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	// Craft a signature with alg: "none"
	header := &ProtectedHeader{
		Algorithm: "none",
		Type:      "JOSE",
		KeyID:     "attack-key",
	}
	headerJSON, _ := json.Marshal(header)
	protectedB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	sig := agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString([]byte("fake-signature")),
	}

	result, err := VerifyJWS(cardData, &sig, rsaPubPEM)
	if err == nil {
		t.Fatal("Expected error when alg is 'none'")
	}
	if result.Verified {
		t.Error("Expected verified=false when alg is 'none'")
	}
	if indexOf(err.Error(), "not permitted") == -1 {
		t.Errorf("Expected 'not permitted' in error, got: %v", err)
	}
}

// TestVerifyJWS_EmptyAlg_Rejected verifies that missing alg is rejected.
func TestVerifyJWS_EmptyAlg_Rejected(t *testing.T) {
	_, rsaPubPEM := generateRSAKeyPair(t)
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	// Craft a signature with empty alg
	header := &ProtectedHeader{
		Type:  "JOSE",
		KeyID: "some-key",
	}
	headerJSON, _ := json.Marshal(header)
	protectedB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	sig := agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString([]byte("fake-signature")),
	}

	result, err := VerifyJWS(cardData, &sig, rsaPubPEM)
	if err == nil {
		t.Fatal("Expected error when alg is empty")
	}
	if result.Verified {
		t.Error("Expected verified=false when alg is empty")
	}
}

// TestVerifyJWS_UnsupportedAlg_Rejected verifies that unknown algorithms are rejected.
func TestVerifyJWS_UnsupportedAlg_Rejected(t *testing.T) {
	_, rsaPubPEM := generateRSAKeyPair(t)
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	header := &ProtectedHeader{
		Algorithm: "HS256", // HMAC — not supported (and dangerous if accepted with asymmetric keys)
		Type:      "JOSE",
		KeyID:     "some-key",
	}
	headerJSON, _ := json.Marshal(header)
	protectedB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	sig := agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString([]byte("fake-signature")),
	}

	result, err := VerifyJWS(cardData, &sig, rsaPubPEM)
	if err == nil {
		t.Fatal("Expected error when alg is unsupported (HS256)")
	}
	if result.Verified {
		t.Error("Expected verified=false when alg is unsupported")
	}
}

// --- Generic test helpers for multi-algorithm testing ---

// generateECDSAKeyPairWithCurve generates an ECDSA key pair on the given curve.
func generateECDSAKeyPairWithCurve(t *testing.T, curve elliptic.Curve) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	privKey, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate ECDSA key on %s: %v", curve.Params().Name, err)
	}
	pubKeyDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to marshal ECDSA public key: %v", err)
	}
	pubKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyDER})
	return privKey, pubKeyPEM
}

// buildJWSSignatureRSAGeneric creates a JWS signature using the specified RSA algorithm and hash.
// Works for RS256/RS384/RS512 (PKCS#1 v1.5) and PS256/PS384/PS512 (RSA-PSS).
func buildJWSSignatureRSAGeneric(t *testing.T, cardData *agentv1alpha1.AgentCardData, privKey *rsa.PrivateKey, kid, alg string) agentv1alpha1.AgentCardSignature {
	t.Helper()
	header := &ProtectedHeader{
		Algorithm: alg,
		Type:      "JOSE",
		KeyID:     kid,
	}
	protectedB64, err := EncodeProtectedHeader(header)
	if err != nil {
		t.Fatalf("Failed to encode protected header: %v", err)
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("Failed to create canonical JSON: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	hashFunc, err := HashForAlgorithm(alg)
	if err != nil {
		t.Fatalf("HashForAlgorithm(%s) failed: %v", alg, err)
	}
	hasher := hashFunc.New()
	hasher.Write(signingInput)
	hashed := hasher.Sum(nil)

	var sigBytes []byte
	if isPSSAlgorithm(alg) {
		sigBytes, err = rsa.SignPSS(rand.Reader, privKey, hashFunc, hashed, &rsa.PSSOptions{
			SaltLength: rsa.PSSSaltLengthEqualsHash,
		})
	} else {
		sigBytes, err = rsa.SignPKCS1v15(rand.Reader, privKey, hashFunc, hashed)
	}
	if err != nil {
		t.Fatalf("Failed to sign with %s: %v", alg, err)
	}

	return agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes),
	}
}

// buildJWSSignatureECDSAGeneric creates a JWS signature using the specified ECDSA algorithm.
// Uses ASN.1 DER encoding (for backward-compat fallback path).
func buildJWSSignatureECDSAGeneric(t *testing.T, cardData *agentv1alpha1.AgentCardData, privKey *ecdsa.PrivateKey, kid, alg string) agentv1alpha1.AgentCardSignature {
	t.Helper()
	header := &ProtectedHeader{
		Algorithm: alg,
		Type:      "JOSE",
		KeyID:     kid,
	}
	protectedB64, err := EncodeProtectedHeader(header)
	if err != nil {
		t.Fatalf("Failed to encode protected header: %v", err)
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("Failed to create canonical JSON: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	hashFunc, err := HashForAlgorithm(alg)
	if err != nil {
		t.Fatalf("HashForAlgorithm(%s) failed: %v", alg, err)
	}
	hasher := hashFunc.New()
	hasher.Write(signingInput)
	hashed := hasher.Sum(nil)

	sigBytes, err := ecdsa.SignASN1(rand.Reader, privKey, hashed)
	if err != nil {
		t.Fatalf("Failed to sign with %s: %v", alg, err)
	}

	return agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes),
	}
}

// --- RSA-PSS algorithm tests ---

func TestVerifyJWS_PS256_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("PSS Agent", "http://pss:8000", "1.0.0")
	jwsSig := buildJWSSignatureRSAGeneric(t, cardData, privKey, "pss-key", "PS256")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for PS256, got false. Details: %s", result.Details)
	}
}

func TestVerifyJWS_PS384_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("PSS Agent", "http://pss:8000", "1.0.0")
	jwsSig := buildJWSSignatureRSAGeneric(t, cardData, privKey, "pss-key", "PS384")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for PS384, got false. Details: %s", result.Details)
	}
}

func TestVerifyJWS_PS512_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("PSS Agent", "http://pss:8000", "1.0.0")
	jwsSig := buildJWSSignatureRSAGeneric(t, cardData, privKey, "pss-key", "PS512")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for PS512, got false. Details: %s", result.Details)
	}
}

func TestVerifyJWS_PSS_WrongKey(t *testing.T) {
	privKey, _ := generateRSAKeyPair(t)
	_, wrongPubKeyPEM := generateRSAKeyPair(t)

	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")
	jwsSig := buildJWSSignatureRSAGeneric(t, cardData, privKey, "wrong-key", "PS256")

	result, err := VerifyJWS(cardData, &jwsSig, wrongPubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error (should return result with Verified=false): %v", err)
	}
	if result.Verified {
		t.Error("Expected verified=false for PSS wrong key, got true")
	}
}

// --- RS384/RS512 algorithm tests ---

func TestVerifyJWS_RS384_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("RS384 Agent", "http://rs384:8000", "1.0.0")
	jwsSig := buildJWSSignatureRSAGeneric(t, cardData, privKey, "rs384-key", "RS384")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for RS384, got false. Details: %s", result.Details)
	}
}

func TestVerifyJWS_RS512_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateRSAKeyPair(t)
	cardData := newCardData("RS512 Agent", "http://rs512:8000", "1.0.0")
	jwsSig := buildJWSSignatureRSAGeneric(t, cardData, privKey, "rs512-key", "RS512")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for RS512, got false. Details: %s", result.Details)
	}
}

// --- ECDSA P-384 / P-521 algorithm tests ---

func TestVerifyJWS_ES384_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateECDSAKeyPairWithCurve(t, elliptic.P384())
	cardData := newCardData("ES384 Agent", "http://es384:8000", "1.0.0")
	jwsSig := buildJWSSignatureECDSAGeneric(t, cardData, privKey, "es384-key", "ES384")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for ES384, got false. Details: %s", result.Details)
	}
}

func TestVerifyJWS_ES512_ValidSignature(t *testing.T) {
	privKey, pubKeyPEM := generateECDSAKeyPairWithCurve(t, elliptic.P521())
	cardData := newCardData("ES512 Agent", "http://es512:8000", "1.0.0")
	jwsSig := buildJWSSignatureECDSAGeneric(t, cardData, privKey, "es512-key", "ES512")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for ES512, got false. Details: %s", result.Details)
	}
}

// --- ECDSA curve mismatch test ---

func TestVerifyJWS_ECDSA_CurveMismatch(t *testing.T) {
	// Generate P-384 key but claim ES256 (which requires P-256)
	privKey, pubKeyPEM := generateECDSAKeyPairWithCurve(t, elliptic.P384())
	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	// Sign with ES384 (correct for P-384 key) but change the header to ES256
	header := &ProtectedHeader{Algorithm: "ES256", Type: "JOSE", KeyID: "mismatch-key"}
	protectedB64, _ := EncodeProtectedHeader(header)

	payload, _ := CreateCanonicalCardJSON(cardData)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	// Hash with SHA-256 (matches ES256 header) and sign with P-384 key
	hash := sha256.Sum256(signingInput)
	sigBytes, err := ecdsa.SignASN1(rand.Reader, privKey, hash[:])
	if err != nil {
		t.Fatalf("Failed to sign: %v", err)
	}

	sig := &agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes),
	}

	result, err := VerifyJWS(cardData, sig, pubKeyPEM)
	if err == nil {
		t.Fatal("Expected curve mismatch error when using P-384 key with ES256 header")
	}
	if result.Verified {
		t.Error("Expected verified=false for curve mismatch")
	}
	if indexOf(err.Error(), "curve mismatch") == -1 {
		t.Errorf("Expected 'curve mismatch' in error, got: %v", err)
	}
}

// --- Test: bool(false) preserved in canonical JSON (#8) ---

func TestCanonicalJSON_BoolFalse_Preserved(t *testing.T) {
	streaming := false
	push := false
	cardData := &agentv1alpha1.AgentCardData{
		Name: "Agent",
		Capabilities: &agentv1alpha1.AgentCapabilities{
			Streaming:         &streaming,
			PushNotifications: &push,
		},
	}

	canonical, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("CreateCanonicalCardJSON failed: %v", err)
	}

	got := string(canonical)
	// ptr(false) must appear in canonical output, not be stripped
	if indexOf(got, `"streaming":false`) < 0 {
		t.Errorf("Expected '\"streaming\":false' in canonical JSON, got: %s", got)
	}
	if indexOf(got, `"pushNotifications":false`) < 0 {
		t.Errorf("Expected '\"pushNotifications\":false' in canonical JSON, got: %s", got)
	}
}

// --- Test: ECDSA raw R||S format (primary JWS path) (#1) ---

// buildJWSSignatureECDSARawRS creates a JWS signature using ECDSA raw R||S encoding,
// which is the spec-compliant primary path per RFC 7518 §3.4.
func buildJWSSignatureECDSARawRS(t *testing.T, cardData *agentv1alpha1.AgentCardData, privKey *ecdsa.PrivateKey, kid, alg string) agentv1alpha1.AgentCardSignature {
	t.Helper()
	header := &ProtectedHeader{
		Algorithm: alg,
		Type:      "JOSE",
		KeyID:     kid,
	}
	protectedB64, err := EncodeProtectedHeader(header)
	if err != nil {
		t.Fatalf("Failed to encode protected header: %v", err)
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		t.Fatalf("Failed to create canonical JSON: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	hashFunc, err := HashForAlgorithm(alg)
	if err != nil {
		t.Fatalf("HashForAlgorithm(%s) failed: %v", alg, err)
	}
	hasher := hashFunc.New()
	hasher.Write(signingInput)
	hashed := hasher.Sum(nil)

	r, s, err := ecdsa.Sign(rand.Reader, privKey, hashed)
	if err != nil {
		t.Fatalf("Failed to sign with %s: %v", alg, err)
	}

	// Encode as raw R||S (fixed-length, zero-padded)
	byteSize := CurveByteSize(privKey.Curve)
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	rawSig := make([]byte, 2*byteSize)
	copy(rawSig[byteSize-len(rBytes):byteSize], rBytes)
	copy(rawSig[2*byteSize-len(sBytes):], sBytes)

	return agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(rawSig),
	}
}

func TestVerifyJWS_ECDSA_RawRS_Format(t *testing.T) {
	privKey, pubKeyPEM := generateECDSAKeyPair(t)
	cardData := newCardData("RawRS Agent", "http://rawrs:8000", "1.0.0")
	jwsSig := buildJWSSignatureECDSARawRS(t, cardData, privKey, "rawrs-key", "ES256")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for raw R||S ES256, got false. Details: %s", result.Details)
	}
}

func TestVerifyJWS_ECDSA_RawRS_P384(t *testing.T) {
	privKey, pubKeyPEM := generateECDSAKeyPairWithCurve(t, elliptic.P384())
	cardData := newCardData("P384 RawRS Agent", "http://p384:8000", "1.0.0")
	jwsSig := buildJWSSignatureECDSARawRS(t, cardData, privKey, "p384-key", "ES384")

	result, err := VerifyJWS(cardData, &jwsSig, pubKeyPEM)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Verified {
		t.Errorf("Expected verified=true for raw R||S ES384, got false. Details: %s", result.Details)
	}
}

// --- Test: RSA minimum key size (#2) ---

func TestVerifyJWS_RSA_MinKeySize_Rejected(t *testing.T) {
	// Generate a 1024-bit RSA key (below minimum)
	smallPrivKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("Failed to generate 1024-bit RSA key: %v", err)
	}
	pubKeyDER, err := x509.MarshalPKIXPublicKey(&smallPrivKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to marshal public key: %v", err)
	}
	smallPubKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyDER})

	cardData := newCardData("Agent", "http://agent:8000", "1.0.0")

	// Build a signature with the small key
	header := &ProtectedHeader{Algorithm: "RS256", Type: "JOSE", KeyID: "small-key"}
	protectedB64, _ := EncodeProtectedHeader(header)
	payload, _ := CreateCanonicalCardJSON(cardData)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)
	hash := sha256.Sum256(signingInput)
	sigBytes, err := rsa.SignPKCS1v15(rand.Reader, smallPrivKey, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatalf("Failed to sign: %v", err)
	}
	sig := &agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes),
	}

	result, err := VerifyJWS(cardData, sig, smallPubKeyPEM)
	if err == nil {
		t.Fatal("Expected error for RSA key smaller than 2048 bits")
	}
	if result.Verified {
		t.Error("Expected verified=false for 1024-bit RSA key")
	}
	if indexOf(err.Error(), "RSA key too small") == -1 {
		t.Errorf("Expected 'RSA key too small' in error, got: %v", err)
	}
	if indexOf(result.Details, "RSA key too small") == -1 {
		t.Errorf("Expected 'RSA key too small' in details, got: %s", result.Details)
	}
}

// --- Unit tests for algorithm helpers ---

func TestHashForAlgorithm_AllSupported(t *testing.T) {
	tests := []struct {
		alg      string
		expected crypto.Hash
	}{
		{"RS256", crypto.SHA256},
		{"PS256", crypto.SHA256},
		{"ES256", crypto.SHA256},
		{"RS384", crypto.SHA384},
		{"PS384", crypto.SHA384},
		{"ES384", crypto.SHA384},
		{"RS512", crypto.SHA512},
		{"PS512", crypto.SHA512},
		{"ES512", crypto.SHA512},
	}
	for _, tt := range tests {
		t.Run(tt.alg, func(t *testing.T) {
			h, err := HashForAlgorithm(tt.alg)
			if err != nil {
				t.Fatalf("HashForAlgorithm(%s) unexpected error: %v", tt.alg, err)
			}
			if h != tt.expected {
				t.Errorf("HashForAlgorithm(%s) = %v, want %v", tt.alg, h, tt.expected)
			}
		})
	}
}

func TestHashForAlgorithm_Unsupported(t *testing.T) {
	for _, alg := range []string{"HS256", "none", "", "RS1024"} {
		_, err := HashForAlgorithm(alg)
		if err == nil {
			t.Errorf("HashForAlgorithm(%q) expected error, got nil", alg)
		}
	}
}

func TestIsPSSAlgorithm(t *testing.T) {
	trueAlgs := []string{"PS256", "PS384", "PS512"}
	falseAlgs := []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "none", "HS256", ""}
	for _, alg := range trueAlgs {
		if !isPSSAlgorithm(alg) {
			t.Errorf("isPSSAlgorithm(%q) = false, want true", alg)
		}
	}
	for _, alg := range falseAlgs {
		if isPSSAlgorithm(alg) {
			t.Errorf("isPSSAlgorithm(%q) = true, want false", alg)
		}
	}
}

func TestIsRSAAlgorithm(t *testing.T) {
	trueAlgs := []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}
	falseAlgs := []string{"ES256", "ES384", "ES512", "none", "HS256", ""}
	for _, alg := range trueAlgs {
		if !isRSAAlgorithm(alg) {
			t.Errorf("isRSAAlgorithm(%q) = false, want true", alg)
		}
	}
	for _, alg := range falseAlgs {
		if isRSAAlgorithm(alg) {
			t.Errorf("isRSAAlgorithm(%q) = true, want false", alg)
		}
	}
}

func TestIsECDSAAlgorithm(t *testing.T) {
	trueAlgs := []string{"ES256", "ES384", "ES512"}
	falseAlgs := []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "none", "HS256", ""}
	for _, alg := range trueAlgs {
		if !isECDSAAlgorithm(alg) {
			t.Errorf("isECDSAAlgorithm(%q) = false, want true", alg)
		}
	}
	for _, alg := range falseAlgs {
		if isECDSAAlgorithm(alg) {
			t.Errorf("isECDSAAlgorithm(%q) = true, want false", alg)
		}
	}
}

func TestValidateAlgorithm_Supported(t *testing.T) {
	for alg := range supportedAlgorithms {
		if err := validateAlgorithm(alg); err != nil {
			t.Errorf("validateAlgorithm(%q) unexpected error: %v", alg, err)
		}
	}
}

func TestValidateAlgorithm_Rejected(t *testing.T) {
	rejected := []struct {
		alg       string
		wantInErr string
	}{
		{"", "missing required"},
		{"none", "not permitted"},
		{"HS256", "unsupported"},
		{"RS1024", "unsupported"},
	}
	for _, tt := range rejected {
		t.Run(tt.alg, func(t *testing.T) {
			err := validateAlgorithm(tt.alg)
			if err == nil {
				t.Fatalf("validateAlgorithm(%q) expected error", tt.alg)
			}
			if indexOf(err.Error(), tt.wantInErr) == -1 {
				t.Errorf("validateAlgorithm(%q) error = %v, want substring %q", tt.alg, err, tt.wantInErr)
			}
		})
	}
}

func TestCurveByteSize(t *testing.T) {
	tests := []struct {
		curve    elliptic.Curve
		expected int
	}{
		{elliptic.P256(), 32},
		{elliptic.P384(), 48},
		{elliptic.P521(), 66},
	}
	for _, tt := range tests {
		t.Run(tt.curve.Params().Name, func(t *testing.T) {
			got := CurveByteSize(tt.curve)
			if got != tt.expected {
				t.Errorf("CurveByteSize(%s) = %d, want %d", tt.curve.Params().Name, got, tt.expected)
			}
		})
	}
}

// --- removeEmptyFields edge cases ---

func TestRemoveEmptyFields_NestedEmptyMapOmitted(t *testing.T) {
	input := map[string]interface{}{
		"name": "Agent",
		"nested": map[string]interface{}{
			"inner": map[string]interface{}{}, // empty → should be omitted
		},
	}
	result := removeEmptyFields(input)
	if _, exists := result["nested"]; exists {
		t.Error("Expected nested empty map to be omitted")
	}
	if result["name"] != "Agent" {
		t.Errorf("Expected name='Agent', got %v", result["name"])
	}
}

func TestRemoveEmptyFields_EmptySliceOmitted(t *testing.T) {
	input := map[string]interface{}{
		"name":  "Agent",
		"items": []interface{}{},
	}
	result := removeEmptyFields(input)
	if _, exists := result["items"]; exists {
		t.Error("Expected empty slice field to be omitted")
	}
}

func TestRemoveEmptyFields_BoolFalsePreserved(t *testing.T) {
	input := map[string]interface{}{
		"name":   "Agent",
		"active": false,
	}
	result := removeEmptyFields(input)
	val, exists := result["active"]
	if !exists {
		t.Fatal("Expected bool(false) to be preserved")
	}
	if val != false {
		t.Errorf("Expected active=false, got %v", val)
	}
}

func TestRemoveEmptyFields_NumberZeroPreserved(t *testing.T) {
	input := map[string]interface{}{
		"name":  "Agent",
		"count": float64(0),
	}
	result := removeEmptyFields(input)
	val, exists := result["count"]
	if !exists {
		t.Fatal("Expected float64(0) to be preserved")
	}
	if val != float64(0) {
		t.Errorf("Expected count=0, got %v", val)
	}
}

func TestRemoveEmptyFields_DeeplyNestedPreservesNonEmpty(t *testing.T) {
	input := map[string]interface{}{
		"outer": map[string]interface{}{
			"middle": map[string]interface{}{
				"inner": "value",
			},
			"empty": map[string]interface{}{},
		},
	}
	result := removeEmptyFields(input)
	outer, ok := result["outer"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected outer to be a map")
	}
	if _, exists := outer["empty"]; exists {
		t.Error("Expected nested empty map to be omitted")
	}
	middle, ok := outer["middle"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected middle to be a map")
	}
	if middle["inner"] != "value" {
		t.Errorf("Expected inner='value', got %v", middle["inner"])
	}
}

// --- Helper ---

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
