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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gowebpki/jcs"
	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	sigbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// SigstoreConfig configures Sigstore bundle verification (SignedAgentCard).
type SigstoreConfig struct {
	// TrustedRootJSON when non-nil loads trust material via root.NewTrustedRootFromJSON (private/air-gapped).
	TrustedRootJSON []byte
	// UseStagingTUF uses Sigstore staging TUF mirror and root (for staging-signed cards).
	UseStagingTUF bool

	OIDCIssuer             string
	CertificateIdentity    string
	CertificateIssuerReg   string
	CertificateIdentityReg string
}

// SigstoreProvider implements BundleVerifier using sigstore-go.
type SigstoreProvider struct {
	verifier        *verify.Verifier
	cfg             SigstoreConfig
	trustedLoadedAt time.Time
}

// NewSigstoreProvider builds a verifier. Either TrustedRootJSON is set or the public good / staging TUF root is fetched.
func NewSigstoreProvider(cfg *SigstoreConfig) (*SigstoreProvider, error) {
	if cfg == nil {
		return nil, errors.New("sigstore config is nil")
	}
	if cfg.OIDCIssuer == "" || cfg.CertificateIdentity == "" {
		return nil, errors.New("sigstore: certificate OIDC issuer and certificate identity are required")
	}

	var tm root.TrustedMaterial
	var err error
	switch {
	case len(cfg.TrustedRootJSON) > 0:
		tr, err2 := root.NewTrustedRootFromJSON(cfg.TrustedRootJSON)
		if err2 != nil {
			return nil, fmt.Errorf("sigstore: parse trusted root: %w", err2)
		}
		tm = tr
	default:
		opts := tuf.DefaultOptions()
		if cfg.UseStagingTUF {
			opts.RepositoryBaseURL = tuf.StagingMirror
			opts.Root = tuf.StagingRoot()
		}
		tm, err = root.NewLiveTrustedRoot(opts)
		if err != nil {
			return nil, fmt.Errorf("sigstore: load trusted root from TUF: %w", err)
		}
	}

	v, err := verify.NewVerifier(tm,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
		verify.WithSignedCertificateTimestamps(1),
	)
	if err != nil {
		return nil, fmt.Errorf("sigstore: new verifier: %w", err)
	}

	ObserveSigstoreTrustedRootAge(0)
	return &SigstoreProvider{
		verifier:        v,
		cfg:             *cfg,
		trustedLoadedAt: time.Now(),
	}, nil
}

func (s *SigstoreProvider) Name() string {
	return "sigstore"
}

// TrustedRootAgeSeconds returns seconds since the provider was constructed (root loaded).
func (s *SigstoreProvider) TrustedRootAgeSeconds() float64 {
	if s.trustedLoadedAt.IsZero() {
		return 0
	}
	return time.Since(s.trustedLoadedAt).Seconds()
}

// VerifySignedAgentCard verifies attestations.signatureBundle from a SignedAgentCard JSON document.
func (s *SigstoreProvider) VerifySignedAgentCard(ctx context.Context, signedJSON []byte,
	override *agentv1alpha1.SigstoreVerification,
) (*BundleVerificationResult, error) {
	_ = ctx
	ObserveSigstoreTrustedRootAge(s.TrustedRootAgeSeconds())

	parsed, err := parseSignedAgentCardStructure(signedJSON)
	if err != nil {
		return nil, fmt.Errorf("sigstore: parse signed agent card: %w", err)
	}
	if parsed.Absent {
		return &BundleVerificationResult{
			Verified: false,
			Absent:   true,
			Details:  "no attestations.signatureBundle on agent card document",
		}, nil
	}

	canonicalAgentCard, err := jcs.Transform(parsed.AgentCardRaw)
	if err != nil {
		return nil, fmt.Errorf("sigstore: canonicalize agentCard (RFC 8785): %w", err)
	}

	sigEntity := sigbundle.Bundle{}
	if err := sigEntity.UnmarshalJSON(parsed.BundleRaw); err != nil {
		return nil, fmt.Errorf("sigstore: load signature bundle: %w", err)
	}

	issuer := s.cfg.OIDCIssuer
	san := s.cfg.CertificateIdentity
	issuerReg := s.cfg.CertificateIssuerReg
	sanReg := s.cfg.CertificateIdentityReg
	if override != nil {
		if override.CertificateOIDCIssuer != "" {
			issuer = override.CertificateOIDCIssuer
		}
		if override.CertificateIdentity != "" {
			san = override.CertificateIdentity
		}
	}

	certID, err := verify.NewShortCertificateIdentity(issuer, issuerReg, san, sanReg)
	if err != nil {
		return nil, fmt.Errorf("sigstore: certificate identity policy: %w", err)
	}

	policy := verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(canonicalAgentCard)),
		verify.WithCertificateIdentity(certID),
	)

	start := time.Now()
	res, err := s.verifier.Verify(&sigEntity, policy)
	duration := time.Since(start).Seconds()
	SigstoreVerificationDuration.WithLabelValues("sigstore").Observe(duration)

	if err != nil {
		RecordSigstoreVerification(false, "verification_failed")
		return &BundleVerificationResult{
			Verified: false,
			Details:  err.Error(),
		}, nil
	}

	out := &BundleVerificationResult{
		Verified: true,
		Details:  "Sigstore bundle verification succeeded",
	}
	if res.VerifiedIdentity != nil {
		out.Identity = res.VerifiedIdentity.SubjectAlternativeName.SubjectAlternativeName
	}
	out.RekorLogIndex = rekorIndexFromBundle(&sigEntity)

	slsaRepo, slsaSha := parseProvenance(parsed.ProvenanceRaw)
	out.SLSARepository = slsaRepo
	out.SLSACommitSHA = slsaSha

	RecordSigstoreVerification(true, "verified")
	if len(parsed.ProvenanceRaw) > 0 {
		if slsaRepo != "" || slsaSha != "" {
			RecordSLSAProvenance("ok")
		} else {
			RecordSLSAProvenance("unparsed")
		}
	}

	return out, nil
}

type parsedSignedCard struct {
	AgentCardRaw  []byte
	BundleRaw     []byte
	ProvenanceRaw []byte
	Absent        bool
}

func parseSignedAgentCardStructure(signedJSON []byte) (*parsedSignedCard, error) {
	var outer struct {
		AgentCard            json.RawMessage `json:"agentCard"`
		Attestations         json.RawMessage `json:"attestations"`
		VerificationMaterial json.RawMessage `json:"verificationMaterial"`
	}
	if err := json.Unmarshal(signedJSON, &outer); err != nil {
		return nil, err
	}

	var att struct {
		SignatureBundle  json.RawMessage `json:"signatureBundle"`
		ProvenanceBundle json.RawMessage `json:"provenanceBundle"`
	}
	var legacy struct {
		SignatureBundle  json.RawMessage `json:"signatureBundle"`
		ProvenanceBundle json.RawMessage `json:"provenanceBundle"`
	}

	var bundleRaw []byte
	var provRaw []byte

	switch {
	case len(outer.Attestations) > 0:
		if err := json.Unmarshal(outer.Attestations, &att); err != nil {
			return nil, err
		}
		bundleRaw = att.SignatureBundle
		if len(att.ProvenanceBundle) > 0 {
			provRaw = att.ProvenanceBundle
		}
	case len(outer.VerificationMaterial) > 0:
		if err := json.Unmarshal(outer.VerificationMaterial, &legacy); err != nil {
			return nil, err
		}
		bundleRaw = legacy.SignatureBundle
		if len(legacy.ProvenanceBundle) > 0 {
			provRaw = legacy.ProvenanceBundle
		}
	}

	if len(bundleRaw) == 0 || string(bundleRaw) == "null" {
		return &parsedSignedCard{Absent: true}, nil
	}
	if len(outer.AgentCard) == 0 {
		return nil, errors.New("signed agent card missing agentCard field")
	}

	return &parsedSignedCard{
		AgentCardRaw:  outer.AgentCard,
		BundleRaw:     bundleRaw,
		ProvenanceRaw: provRaw,
	}, nil
}

func rekorIndexFromBundle(b *sigbundle.Bundle) string {
	entries, err := b.TlogEntries()
	if err != nil || len(entries) == 0 {
		return ""
	}
	return strconv.FormatInt(entries[0].LogIndex(), 10)
}

// parseProvenance extracts repository and commit from a minimal SLSA / in-toto provenance JSON blob.
func parseProvenance(raw []byte) (repository, revision string) {
	if len(raw) == 0 {
		return "", ""
	}
	var wrap struct {
		Provenance json.RawMessage `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &wrap); err == nil && len(wrap.Provenance) > 0 {
		raw = wrap.Provenance
	}

	var p struct {
		BuildDefinition struct {
			ExternalParameters struct {
				Source struct {
					Repository string `json:"repository"`
					Revision   string `json:"revision"`
				} `json:"source"`
			} `json:"externalParameters"`
		} `json:"buildDefinition"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", ""
	}
	return p.BuildDefinition.ExternalParameters.Source.Repository,
		p.BuildDefinition.ExternalParameters.Source.Revision
}
