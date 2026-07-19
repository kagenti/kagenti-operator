/*
Copyright 2026.

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

package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func generateSelfSignedCert(t *testing.T, cn string) (certPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestVerifyCAFingerprint(t *testing.T) {
	certA := generateSelfSignedCert(t, "root-a")
	certB := generateSelfSignedCert(t, "root-b")

	t.Run("matching fingerprints", func(t *testing.T) {
		match, err := verifyCAFingerprint(certA, certA)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !match {
			t.Fatal("expected match for identical certs")
		}
	})

	t.Run("mismatching fingerprints", func(t *testing.T) {
		match, err := verifyCAFingerprint(certA, certB)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if match {
			t.Fatal("expected mismatch for different certs")
		}
	})

	t.Run("invalid PEM", func(t *testing.T) {
		_, err := verifyCAFingerprint([]byte("not-pem"), certA)
		if err == nil {
			t.Fatal("expected error for invalid PEM")
		}
	})

	t.Run("invalid certificate DER", func(t *testing.T) {
		badPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("bad-der")})
		_, err := verifyCAFingerprint(badPEM, certA)
		if err == nil {
			t.Fatal("expected error for invalid DER")
		}
	})
}

func TestBuildCacertsData(t *testing.T) {
	tlsCrt := []byte("intermediate-cert\n")
	tlsKey := []byte("intermediate-key\n")
	caCrt := []byte("root-ca-cert\n")

	data := buildCacertsData(tlsCrt, tlsKey, caCrt)

	if string(data["ca-cert.pem"]) != string(tlsCrt) {
		t.Errorf("ca-cert.pem: got %q, want %q", data["ca-cert.pem"], tlsCrt)
	}

	if string(data["ca-key.pem"]) != string(tlsKey) {
		t.Errorf("ca-key.pem: got %q, want %q", data["ca-key.pem"], tlsKey)
	}

	if string(data["root-cert.pem"]) != string(caCrt) {
		t.Errorf("root-cert.pem: got %q, want %q", data["root-cert.pem"], caCrt)
	}

	expectedChain := string(tlsCrt) + string(caCrt)
	if string(data["cert-chain.pem"]) != expectedChain {
		t.Errorf("cert-chain.pem: got %q, want %q", data["cert-chain.pem"], expectedChain)
	}

	if len(data) != 4 {
		t.Errorf("expected 4 keys, got %d", len(data))
	}
}
