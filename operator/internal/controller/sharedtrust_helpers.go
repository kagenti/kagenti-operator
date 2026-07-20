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
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

func verifyCAFingerprint(rootCertPEM, intermediateCACertPEM []byte) (bool, error) {
	rootFP, err := certFingerprint(rootCertPEM)
	if err != nil {
		return false, fmt.Errorf("parsing root CA: %w", err)
	}

	intFP, err := certFingerprint(intermediateCACertPEM)
	if err != nil {
		return false, fmt.Errorf("parsing intermediate CA: %w", err)
	}

	return rootFP == intFP, nil
}

// certFingerprint returns the SHA256 of the first certificate's raw DER.
// Only the first PEM block is decoded; this matches cert-manager's layout
// where ca.crt contains a single issuing CA certificate.
func certFingerprint(certPEM []byte) ([sha256.Size]byte, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return [sha256.Size]byte{}, fmt.Errorf("no PEM block found")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("parsing certificate: %w", err)
	}

	return sha256.Sum256(cert.Raw), nil
}

func buildCacertsData(tlsCrt, tlsKey, caCrt []byte) map[string][]byte {
	certChain := make([]byte, 0, len(tlsCrt)+len(caCrt))
	certChain = append(certChain, tlsCrt...)
	certChain = append(certChain, caCrt...)

	return map[string][]byte{
		"ca-cert.pem":    tlsCrt,
		"ca-key.pem":     tlsKey,
		"root-cert.pem":  caCrt,
		"cert-chain.pem": certChain,
	}
}
