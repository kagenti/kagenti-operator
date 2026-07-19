//go:build integration
// +build integration

/*
Copyright 2026.

Integration test for Sigstore SignedAgentCard Bundle Verification

Run with: go test -v -tags=integration ./test/integration/... -timeout 5m -run TestSigstoreVerification
Prerequisites: kubectl configured with access to a Kubernetes cluster with rossoctl CRDs installed
*/

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/agentcard"
	"github.com/rossoctl/operator/internal/controller"
	"github.com/rossoctl/operator/internal/signature"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	sigstoreTestNamespace = "sigstore-test"
	testOIDCIssuer        = "https://token.actions.githubusercontent.com"
	testIdentity          = "https://github.com/rossoctl/operator/.github/workflows/sign-agent-card.yml@refs/heads/main"
	testRekorIndex        = "123456789"
	testSLSARepo          = "https://github.com/rossoctl/operator"
	testSLSACommit        = "abc123def456"
)

// mockBundleVerifier simulates Sigstore bundle verification for integration tests
type mockBundleVerifier struct {
	verified       bool
	absent         bool
	identity       string
	rekorLogIndex  string
	slsaRepository string
	slsaCommitSHA  string
	shouldError    bool
	errorMsg       string
}

func (m *mockBundleVerifier) VerifySignedAgentCard(ctx context.Context, signedJSON []byte, override *agentv1alpha1.SigstoreVerification) (*signature.BundleVerificationResult, error) {
	if m.shouldError {
		return nil, fmt.Errorf("%s", m.errorMsg)
	}

	return &signature.BundleVerificationResult{
		Verified:       m.verified,
		Absent:         m.absent,
		Details:        "mock bundle verifier",
		Identity:       m.identity,
		RekorLogIndex:  m.rekorLogIndex,
		SLSARepository: m.slsaRepository,
		SLSACommitSHA:  m.slsaCommitSHA,
	}, nil
}

func (m *mockBundleVerifier) Name() string {
	return "mock-sigstore"
}

// mockFetcherWithSignedCard returns a FetchResult with RawSignedAgentCardJSON
// to simulate fetching a SignedAgentCard from ConfigMap
type mockFetcherWithSignedCard struct {
	cardData               *agentv1alpha1.AgentCardData
	rawSignedAgentCardJSON []byte
}

func (f *mockFetcherWithSignedCard) Fetch(_ context.Context, _, _, _, _ string) (*agentcard.FetchResult, error) {
	return &agentcard.FetchResult{
		CardData:               f.cardData,
		RawSignedAgentCardJSON: f.rawSignedAgentCardJSON,
	}, nil
}

func TestSigstoreVerificationIntegration(t *testing.T) {
	ctx := context.Background()

	setupClient(t)

	// Create dedicated namespace for Sigstore tests
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: sigstoreTestNamespace,
		},
	}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("Failed to create test namespace: %v", err)
	}
	defer func() {
		_ = k8sClient.Delete(ctx, ns)
	}()

	t.Run("Test1_ValidSignedAgentCard", testValidSignedAgentCard)
	t.Run("Test2_AbsentBundle_PlainAgentCard", testAbsentBundlePlainAgentCard)
	t.Run("Test3_InvalidBundle_AuditMode", testInvalidBundleAuditMode)
	t.Run("Test4_InvalidBundle_EnforcementMode", testInvalidBundleEnforcementMode)
	t.Run("Test5_SLSAProvenanceExtraction", testSLSAProvenanceExtraction)
	t.Run("Test6_PerCardIdentityOverride", testPerCardIdentityOverride)
}

func testValidSignedAgentCard(t *testing.T) {
	ctx := context.Background()
	deploymentName := "valid-signed-agent"
	cardName := "valid-signed-card"

	t.Log("\n========================================")
	t.Log("TEST 1: Valid SignedAgentCard Bundle Verification")
	t.Log("========================================")

	// Create deployment and service
	deployment := createSigstoreTestDeployment(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, deployment)

	service := createSigstoreTestService(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, service)

	waitForDeploymentAvailable(t, ctx, deploymentName, sigstoreTestNamespace, 30*time.Second)

	// Create AgentCard CR
	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
		Spec: agentv1alpha1.AgentCardSpec{
			SyncPeriod: "30s",
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
		},
	}

	if err := k8sClient.Create(ctx, agentCard); err != nil {
		t.Fatalf("Failed to create AgentCard: %v", err)
	}
	defer deleteSigstoreResource(ctx, agentCard)

	// Mock fetcher that returns SignedAgentCard JSON
	signedCardJSON := []byte(`{"agentCard":{"name":"test"},"attestations":{"signatureBundle":{}}}`)
	fetcher := &mockFetcherWithSignedCard{
		cardData: &agentv1alpha1.AgentCardData{
			Name:    "Valid Signed Agent",
			Version: "1.0.0",
			URL:     "http://test:8000",
		},
		rawSignedAgentCardJSON: signedCardJSON,
	}

	// Mock bundle verifier that returns success
	bundleVerifier := &mockBundleVerifier{
		verified:      true,
		absent:        false,
		identity:      testIdentity,
		rekorLogIndex: testRekorIndex,
	}

	// Create reconciler with Sigstore verification enabled
	reconciler := &controller.AgentCardReconciler{
		Client:                     k8sClient,
		Scheme:                     k8sClient.Scheme(),
		AgentFetcher:               fetcher,
		BundleVerifier:             bundleVerifier,
		EnableSigstoreVerification: true,
		SigstoreAuditMode:          false,
	}

	// First reconcile: add finalizer
	_, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
	})
	if err != nil {
		t.Fatalf("First reconcile failed: %v", err)
	}

	// Second reconcile: verify bundle
	_, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
	})
	if err != nil {
		t.Fatalf("Second reconcile failed: %v", err)
	}

	// Wait for status update
	time.Sleep(100 * time.Millisecond)

	// Verify status fields
	updatedCard := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}, updatedCard); err != nil {
		t.Fatalf("Failed to get updated AgentCard: %v", err)
	}

	// Assert Sigstore verification succeeded
	if updatedCard.Status.SigstoreBundleVerified == nil || !*updatedCard.Status.SigstoreBundleVerified {
		t.Errorf("Expected sigstoreBundleVerified=true, got: %v", updatedCard.Status.SigstoreBundleVerified)
	}

	if updatedCard.Status.SigstoreIdentity != testIdentity {
		t.Errorf("Expected sigstoreIdentity=%s, got: %s", testIdentity, updatedCard.Status.SigstoreIdentity)
	}

	if updatedCard.Status.RekorLogIndex != testRekorIndex {
		t.Errorf("Expected rekorLogIndex=%s, got: %s", testRekorIndex, updatedCard.Status.RekorLogIndex)
	}

	// Verify SigstoreVerified condition
	verified := false
	for _, cond := range updatedCard.Status.Conditions {
		if cond.Type == "SigstoreVerified" && cond.Status == metav1.ConditionTrue {
			verified = true
			break
		}
	}
	if !verified {
		t.Error("Expected SigstoreVerified condition to be True")
	}

	t.Log("✓ Valid SignedAgentCard bundle verification succeeded")
}

func testAbsentBundlePlainAgentCard(t *testing.T) {
	ctx := context.Background()
	deploymentName := "plain-agent"
	cardName := "plain-card"

	t.Log("\n========================================")
	t.Log("TEST 2: Absent Bundle (Plain AgentCard)")
	t.Log("========================================")

	deployment := createSigstoreTestDeployment(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, deployment)

	service := createSigstoreTestService(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, service)

	waitForDeploymentAvailable(t, ctx, deploymentName, sigstoreTestNamespace, 30*time.Second)

	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
		},
	}

	if err := k8sClient.Create(ctx, agentCard); err != nil {
		t.Fatalf("Failed to create AgentCard: %v", err)
	}
	defer deleteSigstoreResource(ctx, agentCard)

	// Plain agent card (no SignedAgentCard wrapper)
	fetcher := &mockFetcherWithSignedCard{
		cardData: &agentv1alpha1.AgentCardData{
			Name: "Plain Agent",
			URL:  "http://test:8000",
		},
		rawSignedAgentCardJSON: nil, // No signed card JSON
	}

	// Bundle verifier returns "absent"
	bundleVerifier := &mockBundleVerifier{
		verified: false,
		absent:   true,
	}

	reconciler := &controller.AgentCardReconciler{
		Client:                     k8sClient,
		Scheme:                     k8sClient.Scheme(),
		AgentFetcher:               fetcher,
		BundleVerifier:             bundleVerifier,
		EnableSigstoreVerification: true,
		SigstoreAuditMode:          false,
	}

	// Reconcile
	_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify status
	updatedCard := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}, updatedCard); err != nil {
		t.Fatalf("Failed to get updated AgentCard: %v", err)
	}

	// Should have sigstoreBundleVerified=false (absent)
	if updatedCard.Status.SigstoreBundleVerified == nil || *updatedCard.Status.SigstoreBundleVerified {
		t.Errorf("Expected sigstoreBundleVerified=false for absent bundle, got: %v", updatedCard.Status.SigstoreBundleVerified)
	}

	// Verify condition shows bundle not found
	foundAbsentCondition := false
	for _, cond := range updatedCard.Status.Conditions {
		if cond.Type == "SigstoreVerified" && cond.Reason == controller.ReasonSigstoreBundleMissing {
			foundAbsentCondition = true
			break
		}
	}
	if !foundAbsentCondition {
		t.Error("Expected SigstoreVerified condition with reason SigstoreBundleNotFound")
	}

	t.Log("✓ Absent bundle handled gracefully (plain agent card)")
}

func testInvalidBundleAuditMode(t *testing.T) {
	ctx := context.Background()
	deploymentName := "invalid-audit-agent"
	cardName := "invalid-audit-card"

	t.Log("\n========================================")
	t.Log("TEST 3: Invalid Bundle - Audit Mode")
	t.Log("========================================")

	deployment := createSigstoreTestDeployment(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, deployment)

	service := createSigstoreTestService(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, service)

	waitForDeploymentAvailable(t, ctx, deploymentName, sigstoreTestNamespace, 30*time.Second)

	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
		},
	}

	if err := k8sClient.Create(ctx, agentCard); err != nil {
		t.Fatalf("Failed to create AgentCard: %v", err)
	}
	defer deleteSigstoreResource(ctx, agentCard)

	signedCardJSON := []byte(`{"agentCard":{"name":"test"},"attestations":{"signatureBundle":{}}}`)
	fetcher := &mockFetcherWithSignedCard{
		cardData: &agentv1alpha1.AgentCardData{
			Name: "Invalid Bundle Agent",
			URL:  "http://test:8000",
		},
		rawSignedAgentCardJSON: signedCardJSON,
	}

	// Bundle verifier returns failure
	bundleVerifier := &mockBundleVerifier{
		verified: false,
		absent:   false,
	}

	reconciler := &controller.AgentCardReconciler{
		Client:                     k8sClient,
		Scheme:                     k8sClient.Scheme(),
		AgentFetcher:               fetcher,
		BundleVerifier:             bundleVerifier,
		EnableSigstoreVerification: true,
		SigstoreAuditMode:          true, // AUDIT MODE
	}

	// Reconcile
	_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})

	// In audit mode, reconcile should NOT error even with invalid bundle
	if err != nil {
		t.Errorf("Audit mode should not error on invalid bundle, got: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify status
	updatedCard := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}, updatedCard); err != nil {
		t.Fatalf("Failed to get updated AgentCard: %v", err)
	}

	// Should have sigstoreBundleVerified=false
	if updatedCard.Status.SigstoreBundleVerified == nil || *updatedCard.Status.SigstoreBundleVerified {
		t.Errorf("Expected sigstoreBundleVerified=false, got: %v", updatedCard.Status.SigstoreBundleVerified)
	}

	// Verify condition shows audit mode failure
	foundAuditCondition := false
	for _, cond := range updatedCard.Status.Conditions {
		if cond.Type == "SigstoreVerified" && cond.Reason == controller.ReasonSigstoreInvalidAudit {
			foundAuditCondition = true
			break
		}
	}
	if !foundAuditCondition {
		t.Error("Expected SigstoreVerified condition with reason SigstoreVerificationFailedAudit")
	}

	t.Log("✓ Invalid bundle in audit mode logged but did not block reconciliation")
}

func testInvalidBundleEnforcementMode(t *testing.T) {
	ctx := context.Background()
	deploymentName := "invalid-enforce-agent"
	cardName := "invalid-enforce-card"

	t.Log("\n========================================")
	t.Log("TEST 4: Invalid Bundle - Enforcement Mode")
	t.Log("========================================")

	deployment := createSigstoreTestDeployment(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, deployment)

	service := createSigstoreTestService(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, service)

	waitForDeploymentAvailable(t, ctx, deploymentName, sigstoreTestNamespace, 30*time.Second)

	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
		},
	}

	if err := k8sClient.Create(ctx, agentCard); err != nil {
		t.Fatalf("Failed to create AgentCard: %v", err)
	}
	defer deleteSigstoreResource(ctx, agentCard)

	signedCardJSON := []byte(`{"agentCard":{"name":"test"},"attestations":{"signatureBundle":{}}}`)
	fetcher := &mockFetcherWithSignedCard{
		cardData: &agentv1alpha1.AgentCardData{
			Name: "Invalid Bundle Agent",
			URL:  "http://test:8000",
		},
		rawSignedAgentCardJSON: signedCardJSON,
	}

	// Bundle verifier returns failure
	bundleVerifier := &mockBundleVerifier{
		verified: false,
		absent:   false,
	}

	reconciler := &controller.AgentCardReconciler{
		Client:                     k8sClient,
		Scheme:                     k8sClient.Scheme(),
		AgentFetcher:               fetcher,
		BundleVerifier:             bundleVerifier,
		EnableSigstoreVerification: true,
		SigstoreAuditMode:          false, // ENFORCEMENT MODE
	}

	// Reconcile
	_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})

	// In enforcement mode, reconcile should succeed but card should be marked as invalid
	if err != nil {
		t.Errorf("Enforcement mode reconcile failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify status
	updatedCard := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}, updatedCard); err != nil {
		t.Fatalf("Failed to get updated AgentCard: %v", err)
	}

	// Should have sigstoreBundleVerified=false
	if updatedCard.Status.SigstoreBundleVerified == nil || *updatedCard.Status.SigstoreBundleVerified {
		t.Errorf("Expected sigstoreBundleVerified=false in enforcement mode, got: %v", updatedCard.Status.SigstoreBundleVerified)
	}

	// Verify Synced condition is False (card rejected)
	syncedCondition := false
	for _, cond := range updatedCard.Status.Conditions {
		if cond.Type == "Synced" && cond.Status == metav1.ConditionFalse {
			syncedCondition = true
			break
		}
	}
	if !syncedCondition {
		t.Error("Expected Synced condition to be False in enforcement mode with invalid bundle")
	}

	t.Log("✓ Invalid bundle in enforcement mode rejected agent card")
}

func testSLSAProvenanceExtraction(t *testing.T) {
	ctx := context.Background()
	deploymentName := "slsa-agent"
	cardName := "slsa-card"

	t.Log("\n========================================")
	t.Log("TEST 5: SLSA Provenance Extraction")
	t.Log("========================================")

	deployment := createSigstoreTestDeployment(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, deployment)

	service := createSigstoreTestService(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, service)

	waitForDeploymentAvailable(t, ctx, deploymentName, sigstoreTestNamespace, 30*time.Second)

	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
		},
	}

	if err := k8sClient.Create(ctx, agentCard); err != nil {
		t.Fatalf("Failed to create AgentCard: %v", err)
	}
	defer deleteSigstoreResource(ctx, agentCard)

	signedCardJSON := []byte(`{"agentCard":{"name":"test"},"attestations":{"signatureBundle":{},"provenanceBundle":{}}}`)
	fetcher := &mockFetcherWithSignedCard{
		cardData: &agentv1alpha1.AgentCardData{
			Name: "SLSA Agent",
			URL:  "http://test:8000",
		},
		rawSignedAgentCardJSON: signedCardJSON,
	}

	// Bundle verifier returns success with SLSA provenance
	bundleVerifier := &mockBundleVerifier{
		verified:       true,
		absent:         false,
		identity:       testIdentity,
		slsaRepository: testSLSARepo,
		slsaCommitSHA:  testSLSACommit,
	}

	reconciler := &controller.AgentCardReconciler{
		Client:                     k8sClient,
		Scheme:                     k8sClient.Scheme(),
		AgentFetcher:               fetcher,
		BundleVerifier:             bundleVerifier,
		EnableSigstoreVerification: true,
		SigstoreAuditMode:          false,
	}

	// Reconcile
	_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify SLSA provenance fields
	updatedCard := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}, updatedCard); err != nil {
		t.Fatalf("Failed to get updated AgentCard: %v", err)
	}

	if updatedCard.Status.SLSARepository != testSLSARepo {
		t.Errorf("Expected slsaRepository=%s, got: %s", testSLSARepo, updatedCard.Status.SLSARepository)
	}

	if updatedCard.Status.SLSACommitSHA != testSLSACommit {
		t.Errorf("Expected slsaCommitSHA=%s, got: %s", testSLSACommit, updatedCard.Status.SLSACommitSHA)
	}

	t.Log("✓ SLSA provenance extracted successfully")
}

func testPerCardIdentityOverride(t *testing.T) {
	ctx := context.Background()
	deploymentName := "override-agent"
	cardName := "override-card"

	t.Log("\n========================================")
	t.Log("TEST 6: Per-Card Identity Override")
	t.Log("========================================")

	deployment := createSigstoreTestDeployment(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, deployment)

	service := createSigstoreTestService(t, ctx, sigstoreTestNamespace, deploymentName)
	defer deleteSigstoreResource(ctx, service)

	waitForDeploymentAvailable(t, ctx, deploymentName, sigstoreTestNamespace, 30*time.Second)

	// AgentCard with custom Sigstore verification settings
	customIdentity := "https://custom.identity.example.com"
	customIssuer := "https://custom.issuer.example.com"

	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cardName,
			Namespace: sigstoreTestNamespace,
		},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			SigstoreVerification: &agentv1alpha1.SigstoreVerification{
				CertificateIdentity:   customIdentity,
				CertificateOIDCIssuer: customIssuer,
			},
		},
	}

	if err := k8sClient.Create(ctx, agentCard); err != nil {
		t.Fatalf("Failed to create AgentCard: %v", err)
	}
	defer deleteSigstoreResource(ctx, agentCard)

	signedCardJSON := []byte(`{"agentCard":{"name":"test"},"attestations":{"signatureBundle":{}}}`)
	fetcher := &mockFetcherWithSignedCard{
		cardData: &agentv1alpha1.AgentCardData{
			Name: "Override Agent",
			URL:  "http://test:8000",
		},
		rawSignedAgentCardJSON: signedCardJSON,
	}

	bundleVerifier := &mockBundleVerifier{
		verified: true,
		absent:   false,
		identity: customIdentity, // Should match the override
	}

	reconciler := &controller.AgentCardReconciler{
		Client:                     k8sClient,
		Scheme:                     k8sClient.Scheme(),
		AgentFetcher:               fetcher,
		BundleVerifier:             bundleVerifier,
		EnableSigstoreVerification: true,
		SigstoreAuditMode:          false,
	}

	// Reconcile
	_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify custom identity was used
	updatedCard := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: sigstoreTestNamespace}, updatedCard); err != nil {
		t.Fatalf("Failed to get updated AgentCard: %v", err)
	}

	if updatedCard.Status.SigstoreIdentity != customIdentity {
		t.Errorf("Expected custom identity=%s, got: %s", customIdentity, updatedCard.Status.SigstoreIdentity)
	}

	t.Log("✓ Per-card identity override worked correctly")
}

// Helper functions for Sigstore integration tests

func createSigstoreTestDeployment(t *testing.T, ctx context.Context, namespace, name string) *appsv1.Deployment {
	t.Helper()
	labels := map[string]string{
		"app.kubernetes.io/name":  name,
		"rossoctl.io/type":         "agent",
		"protocol.rossoctl.io/a2a": "",
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "agent",
							Image: "registry.k8s.io/pause:3.9",
						},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, deployment); err != nil {
		t.Fatalf("Failed to create deployment: %v", err)
	}

	t.Logf("  Created Deployment: %s", name)
	return deployment
}

func createSigstoreTestService(t *testing.T, ctx context.Context, namespace, name string) *corev1.Service {
	t.Helper()
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     8000,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, service); err != nil {
		t.Fatalf("Failed to create service: %v", err)
	}

	t.Logf("  Created Service: %s", name)
	return service
}

func deleteSigstoreResource(ctx context.Context, obj client.Object) {
	// Remove finalizers first (controller adds finalizer to AgentCards)
	obj.SetFinalizers(nil)
	_ = k8sClient.Update(ctx, obj)
	_ = k8sClient.Delete(ctx, obj)
}
