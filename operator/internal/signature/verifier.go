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
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"sort"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

// ProtectedHeader represents the decoded JWS protected header (A2A spec section 8.4.2).
type ProtectedHeader struct {
	Algorithm string   `json:"alg"`
	Type      string   `json:"typ,omitempty"`
	KeyID     string   `json:"kid,omitempty"`
	X5C       []string `json:"x5c,omitempty"` // X.509 certificate chain (base64, NOT base64url) per RFC 7515 §4.1.6
}

func DecodeProtectedHeader(protected string) (*ProtectedHeader, error) {
	headerJSON, err := base64.RawURLEncoding.DecodeString(protected)
	if err != nil {
		return nil, fmt.Errorf("failed to decode protected header: %w", err)
	}
	var header ProtectedHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("failed to parse protected header: %w", err)
	}
	return &header, nil
}

func EncodeProtectedHeader(header *ProtectedHeader) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("failed to marshal protected header: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(headerJSON), nil
}

// VerifyJWS verifies a JWS signature against card data using a PEM public key.
func VerifyJWS(cardData *agentv1alpha1.AgentCardData, sig *agentv1alpha1.AgentCardSignature, publicKeyPEM []byte) (*VerificationResult, error) {
	if sig == nil {
		return &VerificationResult{
			Verified: false,
			Details:  "AgentCard does not contain a signature",
		}, nil
	}

	header, err := DecodeProtectedHeader(sig.Protected)
	if err != nil {
		return &VerificationResult{
			Verified: false,
			Details:  fmt.Sprintf("Failed to decode JWS protected header: %v", err),
		}, err
	}

	if err := validateAlgorithm(header.Algorithm); err != nil {
		return &VerificationResult{
			Verified: false,
			Details:  fmt.Sprintf("Algorithm validation failed: %v", err),
		}, err
	}

	block, _ := pem.Decode(publicKeyPEM)
	if block == nil {
		return &VerificationResult{
			Verified: false,
			Details:  "Invalid PEM format",
		}, fmt.Errorf("failed to decode PEM block")
	}

	publicKey, err := parsePublicKey(block.Bytes)
	if err != nil {
		return &VerificationResult{
			Verified: false,
			Details:  fmt.Sprintf("Failed to parse public key: %v", err),
		}, err
	}

	if rsaKey, ok := publicKey.(*rsa.PublicKey); ok {
		if rsaKey.N.BitLen() < 2048 {
			return &VerificationResult{
				Verified: false,
				Details:  fmt.Sprintf("RSA key too small: %d bits (minimum 2048)", rsaKey.N.BitLen()),
			}, fmt.Errorf("RSA key too small: %d bits (minimum 2048)", rsaKey.N.BitLen())
		}
	}

	payload, err := CreateCanonicalCardJSON(cardData)
	if err != nil {
		return &VerificationResult{
			Verified: false,
			Details:  fmt.Sprintf("Failed to create canonical JSON payload: %v", err),
		}, err
	}

	// JWS signing input per RFC 7515 §5.2
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(sig.Protected + "." + payloadB64)

	hashFunc, err := HashForAlgorithm(header.Algorithm)
	if err != nil {
		return &VerificationResult{
			Verified: false,
			Details:  fmt.Sprintf("Hash function lookup failed: %v", err),
		}, err
	}

	hasher := hashFunc.New()
	hasher.Write(signingInput)
	hashed := hasher.Sum(nil)

	signatureBytes, err := base64.RawURLEncoding.DecodeString(sig.Signature)
	if err != nil {
		return &VerificationResult{
			Verified: false,
			Details:  "Failed to decode JWS signature from base64url",
		}, err
	}

	// Enforce alg-key type match to prevent algorithm confusion.
	var verified bool
	switch pub := publicKey.(type) {
	case *rsa.PublicKey:
		if !isRSAAlgorithm(header.Algorithm) {
			return &VerificationResult{
				Verified: false,
				Details:  fmt.Sprintf("Algorithm mismatch: protected header specifies %q but public key is RSA (expected RS256/RS384/RS512/PS256/PS384/PS512)", header.Algorithm),
			}, fmt.Errorf("algorithm mismatch: header alg=%q but key is RSA", header.Algorithm)
		}
		if isPSSAlgorithm(header.Algorithm) {
			err = rsa.VerifyPSS(pub, hashFunc, hashed, signatureBytes, &rsa.PSSOptions{
				SaltLength: rsa.PSSSaltLengthEqualsHash,
			})
		} else {
			err = rsa.VerifyPKCS1v15(pub, hashFunc, hashed, signatureBytes)
		}
		verified = (err == nil)
	case *ecdsa.PublicKey:
		if !isECDSAAlgorithm(header.Algorithm) {
			return &VerificationResult{
				Verified: false,
				Details:  fmt.Sprintf("Algorithm mismatch: protected header specifies %q but public key is ECDSA (expected ES256/ES384/ES512)", header.Algorithm),
			}, fmt.Errorf("algorithm mismatch: header alg=%q but key is ECDSA", header.Algorithm)
		}
		if err := validateECDSACurve(pub, header.Algorithm); err != nil {
			return &VerificationResult{
				Verified: false,
				Details:  fmt.Sprintf("ECDSA curve/algorithm mismatch: %v", err),
			}, err
		}
		// JWS uses raw R||S encoding; fall back to ASN.1 DER for compatibility.
		verified = verifyECDSARaw(pub, hashed, signatureBytes)
		if !verified {
			verified = ecdsa.VerifyASN1(pub, hashed, signatureBytes)
		}
	default:
		return &VerificationResult{
			Verified: false,
			Details:  "Public key type not supported (expected RSA or ECDSA)",
		}, fmt.Errorf("unsupported key type")
	}

	result := &VerificationResult{
		Verified: verified,
		KeyID:    header.KeyID,
	}

	if !verified {
		result.Details = "JWS signature verification failed"
	} else {
		result.Details = fmt.Sprintf("JWS signature verified successfully (alg=%s, kid=%s)", header.Algorithm, header.KeyID)
	}

	return result, nil
}

// verifyECDSARaw verifies ECDSA in JWS raw R||S format (RFC 7518 §3.4).
func verifyECDSARaw(pub *ecdsa.PublicKey, hash, sig []byte) bool {
	keySize := CurveByteSize(pub.Curve)
	if len(sig) != 2*keySize {
		return false
	}

	r := new(big.Int).SetBytes(sig[:keySize])
	s := new(big.Int).SetBytes(sig[keySize:])
	return ecdsa.Verify(pub, hash, r, s)
}

func CurveByteSize(curve elliptic.Curve) int {
	return (curve.Params().BitSize + 7) / 8
}

var supportedAlgorithms = map[string]bool{
	"RS256": true, "RS384": true, "RS512": true,
	"PS256": true, "PS384": true, "PS512": true,
	"ES256": true, "ES384": true, "ES512": true,
}

func validateAlgorithm(alg string) error {
	if alg == "" {
		return fmt.Errorf("JWS protected header missing required 'alg' field")
	}
	if alg == "none" {
		return fmt.Errorf("JWS algorithm 'none' is not permitted — signatures must be cryptographically verified")
	}
	if !supportedAlgorithms[alg] {
		return fmt.Errorf("unsupported JWS algorithm %q (supported: RS256, RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512)", alg)
	}
	return nil
}

func isRSAAlgorithm(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
		return true
	}
	return false
}

func isECDSAAlgorithm(alg string) bool {
	switch alg {
	case "ES256", "ES384", "ES512":
		return true
	}
	return false
}

func isPSSAlgorithm(alg string) bool {
	switch alg {
	case "PS256", "PS384", "PS512":
		return true
	}
	return false
}

func HashForAlgorithm(alg string) (crypto.Hash, error) {
	switch alg {
	case "RS256", "PS256", "ES256":
		return crypto.SHA256, nil
	case "RS384", "PS384", "ES384":
		return crypto.SHA384, nil
	case "RS512", "PS512", "ES512":
		return crypto.SHA512, nil
	default:
		return 0, fmt.Errorf("no hash function for algorithm %q", alg)
	}
}

// validateECDSACurve enforces ES256→P-256, ES384→P-384, ES512→P-521 (RFC 7518 §3.4).
func validateECDSACurve(pub *ecdsa.PublicKey, alg string) error {
	var expectedCurve elliptic.Curve
	switch alg {
	case "ES256":
		expectedCurve = elliptic.P256()
	case "ES384":
		expectedCurve = elliptic.P384()
	case "ES512":
		expectedCurve = elliptic.P521()
	default:
		return fmt.Errorf("unknown ECDSA algorithm %q", alg)
	}
	if pub.Curve.Params().Name != expectedCurve.Params().Name {
		return fmt.Errorf("curve mismatch: algorithm %s requires %s but key uses %s",
			alg, expectedCurve.Params().Name, pub.Curve.Params().Name)
	}
	return nil
}

func MarshalPublicKeyToPEM(publicKey interface{}) ([]byte, error) {
	pkixBytes, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key: %w", err)
	}

	pemBlock := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pkixBytes,
	}

	return pem.EncodeToMemory(pemBlock), nil
}

// parsePublicKey tries PKIX first, then falls back to PKCS#1 RSA.
func parsePublicKey(derBytes []byte) (crypto.PublicKey, error) {
	if key, err := x509.ParsePKIXPublicKey(derBytes); err == nil {
		return key, nil
	}

	key, err := x509.ParsePKCS1PublicKey(derBytes)
	if err == nil {
		return key, nil
	}

	return nil, fmt.Errorf("failed to parse public key (tried PKIX and PKCS#1): %w", err)
}

// CreateCanonicalCardJSON builds the JWS payload: sorted-key, compact JSON
// with the "signatures" field excluded.
func CreateCanonicalCardJSON(cardData *agentv1alpha1.AgentCardData) ([]byte, error) {
	rawJSON, err := json.Marshal(cardData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal card data: %w", err)
	}

	var cardMap map[string]interface{}
	if err := json.Unmarshal(rawJSON, &cardMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal to map: %w", err)
	}

	delete(cardMap, "signatures")
	cleanMap := removeEmptyFields(cardMap)
	return marshalCanonical(cleanMap)
}

// removeEmptyFields strips nil/empty values for canonical JSON.
func removeEmptyFields(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range m {
		if v == nil {
			continue
		}
		switch val := v.(type) {
		case map[string]interface{}:
			cleaned := removeEmptyFields(val)
			if len(cleaned) > 0 {
				result[k] = cleaned
			}
		case []interface{}:
			var cleaned []interface{}
			for _, elem := range val {
				switch e := elem.(type) {
				case map[string]interface{}:
					c := removeEmptyFields(e)
					if len(c) > 0 {
						cleaned = append(cleaned, c)
					}
				case string:
					if e != "" {
						cleaned = append(cleaned, e)
					}
				case nil:
					// skip nil elements
				default:
					cleaned = append(cleaned, e)
				}
			}
			if len(cleaned) > 0 {
				result[k] = cleaned
			}
		case string:
			if val != "" {
				result[k] = val
			}
		default:
			result[k] = v
		}
	}
	return result
}

func marshalCanonical(data map[string]interface{}) ([]byte, error) {
	var buf bytes.Buffer

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		valueJSON, err := marshalValue(data[k])
		if err != nil {
			return nil, err
		}
		buf.Write(valueJSON)
	}
	buf.WriteByte('}')

	return buf.Bytes(), nil
}

func marshalValue(v interface{}) ([]byte, error) {
	switch val := v.(type) {
	case map[string]interface{}:
		return marshalCanonical(val)
	case nil:
		return []byte("null"), nil
	case bool:
		if val {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		return json.Marshal(val)
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return json.Marshal(val)
	case []interface{}:
		return marshalArray(val)
	default:
		genericValue, err := toGenericValue(val)
		if err != nil {
			return nil, err
		}
		return marshalValue(genericValue)
	}
}

// toGenericValue converts unknown types via a Marshal→Unmarshal round-trip.
func toGenericValue(v interface{}) (interface{}, error) {
	jsonBytes, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal value: %w", err)
	}
	var generic interface{}
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return nil, fmt.Errorf("failed to unmarshal to generic: %w", err)
	}
	return generic, nil
}

func marshalArray(arr []interface{}) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range arr {
		if i > 0 {
			buf.WriteByte(',')
		}
		itemJSON, err := marshalValue(item)
		if err != nil {
			return nil, err
		}
		buf.Write(itemJSON)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}
