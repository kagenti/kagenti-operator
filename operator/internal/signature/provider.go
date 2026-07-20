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
	"fmt"
	"time"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VerificationResult holds the outcome of a signature verification.
//
// Error contract: a non-nil error from Provider.VerifySignature indicates an
// infrastructure failure (retriable). Cryptographic failures set Verified=false
// with a nil error.
type VerificationResult struct {
	Verified     bool
	KeyID        string
	SpiffeID     string // from leaf cert SAN URI
	Details      string
	LeafNotAfter time.Time // leaf cert expiry
}

// Provider verifies A2A AgentCard JWS signatures (spec section 8.4).
type Provider interface {
	// VerifySignature returns success if at least one signature verifies.
	VerifySignature(ctx context.Context, cardData *agentv1alpha1.AgentCardData, signatures []agentv1alpha1.AgentCardSignature) (*VerificationResult, error)
	Name() string
	// BundleHash returns a hash of the current trust bundle for change detection.
	BundleHash() string
}

type ProviderType string

const (
	ProviderTypeX5C ProviderType = "x5c"
)

// Config holds configuration for the signature verification provider.
type Config struct {
	Type ProviderType

	TrustBundleConfigMapName   string // ConfigMap name (SPIFFE JSON format)
	TrustBundleConfigMapNS     string
	TrustBundleConfigMapKey    string        // default: "bundle.spiffe"
	TrustBundleRefreshInterval time.Duration // default: 5m

	Client client.Client
}

func NewProvider(config *Config) (Provider, error) {
	if config == nil {
		return nil, fmt.Errorf("provider config cannot be nil")
	}

	switch config.Type {
	case ProviderTypeX5C:
		return NewX5CProvider(config)
	default:
		return nil, fmt.Errorf("unknown provider type: %s (only 'x5c' is supported)", config.Type)
	}
}

// BundleVerificationResult is the outcome of Sigstore bundle verification on a
// SignedAgentCard (sigstore-a2a output).
type BundleVerificationResult struct {
	Verified bool
	Details  string
	// Absent is true when no attestations/signatureBundle was present (plain agent card).
	// Per RFC, this is a graceful adoption path and must not hard-fail Ready.
	Absent bool

	Identity       string // Fulcio/OIDC identity (certificate SAN summary)
	RekorLogIndex  string
	SLSARepository string
	SLSACommitSHA  string
}

// BundleVerifier verifies embedded Sigstore bundles inside SignedAgentCard JSON.
type BundleVerifier interface {
	VerifySignedAgentCard(ctx context.Context, signedAgentCardJSON []byte,
		identityOverride *agentv1alpha1.SigstoreVerification) (*BundleVerificationResult, error)
	Name() string
}
