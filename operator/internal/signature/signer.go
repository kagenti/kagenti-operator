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
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

// SignCard signs AgentCard data with the given private key and certificate chain,
// producing a JWS x5c signed JSON output. Used by the agentcard-signer init-container.
func SignCard(cardData *agentv1alpha1.AgentCardData, privateKey crypto.Signer, certs []*x509.Certificate) ([]byte, error) {
	if cardData == nil {
		return nil, fmt.Errorf("card data is nil")
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificates in SVID chain")
	}
	leaf := certs[0]

	alg, err := AlgorithmForKey(privateKey.Public())
	if err != nil {
		return nil, err
	}

	kid := ComputeKID(leaf)

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
		return nil, fmt.Errorf("failed to encode protected header: %w", err)
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		return nil, fmt.Errorf("failed to create canonical JSON: %w", err)
	}

	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	sigBytes, err := SignInput(privateKey, alg, signingInput)
	if err != nil {
		return nil, fmt.Errorf("signing failed: %w", err)
	}

	sigB64 := base64.RawURLEncoding.EncodeToString(sigBytes)

	cardData.Signatures = []agentv1alpha1.AgentCardSignature{
		{
			Protected: protectedB64,
			Signature: sigB64,
		},
	}

	output, err := json.MarshalIndent(cardData, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal signed card: %w", err)
	}

	return output, nil
}

// AlgorithmForKey maps a public key type to its JWS algorithm.
func AlgorithmForKey(pub crypto.PublicKey) (string, error) {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return "", fmt.Errorf("RSA key too small: %d bits (minimum 2048)", k.N.BitLen())
		}
		return "RS256", nil
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return "ES256", nil
		case elliptic.P384():
			return "ES384", nil
		case elliptic.P521():
			return "ES512", nil
		default:
			return "", fmt.Errorf("unsupported ECDSA curve: %s", k.Curve.Params().Name)
		}
	default:
		return "", fmt.Errorf("unsupported key type: %T", pub)
	}
}

// ComputeKID derives a key ID from the leaf cert's SHA-256 fingerprint (first 8 bytes).
func ComputeKID(leaf *x509.Certificate) string {
	fp := sha256.Sum256(leaf.Raw)
	return fmt.Sprintf("%x", fp[:8])
}

// SignInput signs the JWS signing input with the appropriate algorithm.
func SignInput(signer crypto.Signer, alg string, input []byte) ([]byte, error) {
	hashFunc, err := HashForAlgorithm(alg)
	if err != nil {
		return nil, err
	}

	h := hashFunc.New()
	h.Write(input)
	hashed := h.Sum(nil)

	switch alg { //nolint:goconst // JWS algorithm identifiers are well-known strings
	case "RS256", "RS384", "RS512":
		return signer.Sign(rand.Reader, hashed, hashFunc)
	case "ES256", "ES384", "ES512":
		return SignECDSARaw(signer, hashed, alg)
	default:
		return nil, fmt.Errorf("unsupported algorithm: %s", alg)
	}
}

// SignECDSARaw signs with ECDSA and encodes as fixed-width R||S (RFC 7518 §3.4).
func SignECDSARaw(signer crypto.Signer, hashed []byte, alg string) ([]byte, error) {
	ecKey, ok := signer.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("expected *ecdsa.PrivateKey, got %T", signer)
	}

	r, s, err := ecdsa.Sign(rand.Reader, ecKey, hashed)
	if err != nil {
		return nil, fmt.Errorf("ECDSA sign failed: %w", err)
	}

	keySize := CurveByteSize(ecKey.Curve)
	sig := make([]byte, 2*keySize)
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	copy(sig[keySize-len(rBytes):keySize], rBytes)
	copy(sig[2*keySize-len(sBytes):], sBytes)

	return sig, nil
}

// ZeroPrivateKey zeroes private key material in memory (best-effort).
func ZeroPrivateKey(key crypto.Signer) {
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		if k.D != nil {
			k.D.SetInt64(0)
		}
	case *rsa.PrivateKey:
		if k.D != nil {
			k.D.SetInt64(0)
		}
		for _, p := range k.Primes {
			if p != nil {
				p.SetInt64(0)
			}
		}
	}
}
