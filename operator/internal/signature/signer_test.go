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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"testing"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

func TestSignCard_ECDSA_P256(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/test"},
	})

	card := testCard()
	output, err := SignCard(card, key, []*x509.Certificate{leaf, ca.Cert})
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

	header, err := DecodeProtectedHeader(parsed.Signatures[0].Protected)
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

func TestSignCard_NilCardData(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err := SignCard(nil, key, []*x509.Certificate{{}})
	if err == nil {
		t.Error("expected error for nil card data")
	}
}

func TestSignCard_NoCertificates(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err := SignCard(testCard(), key, nil)
	if err == nil {
		t.Error("expected error for empty cert chain")
	}
}

// CRITICAL: Round-trip test proving operator-signed cards verify with X5CProvider.
func TestSignCard_RoundTrip_X5CProvider(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := ca.issueLeaf(t, key, leafOpts{
		spiffeIDs: []string{"spiffe://example.org/ns/default/sa/operator"},
	})

	card := testCard()
	output, err := SignCard(card, key, []*x509.Certificate{leaf, ca.Cert})
	if err != nil {
		t.Fatalf("SignCard failed: %v", err)
	}

	var parsed agentv1alpha1.AgentCardData
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	provider := newTestX5CProvider(t, ca)
	cardWithoutSigs := parsed
	cardWithoutSigs.Signatures = nil

	result, err := provider.VerifySignature(context.Background(), &cardWithoutSigs, parsed.Signatures)
	if err != nil {
		t.Fatalf("X5CProvider.VerifySignature error: %v", err)
	}
	if !result.Verified {
		t.Errorf("round-trip failed: X5CProvider rejected SignCard output: %s", result.Details)
	}
	if result.SpiffeID != "spiffe://example.org/ns/default/sa/operator" {
		t.Errorf("expected SPIFFE ID from cert, got %q", result.SpiffeID)
	}
}
