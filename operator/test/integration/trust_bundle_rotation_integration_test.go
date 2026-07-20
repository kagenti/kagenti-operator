//go:build integration
// +build integration

/*
Copyright 2026.

Integration test for AgentCard trust bundle rotation (RHAIENG-3718).

Verifies that when the trust bundle hash changes (simulating CA rotation),
the operator detects the mismatch and triggers a workload rollout restart.

Run with: go test -v -tags=integration ./test/integration/... -timeout 5m -run TestTrustBundleRotation
Prerequisites: kubectl configured with access to a Kubernetes cluster with rossoctl CRDs installed.
               The operator should NOT be running (scale to 0 or uninstall) — this test calls Reconcile manually.
*/

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/controller"
	"github.com/rossoctl/operator/internal/signature"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// rotationMockProvider implements signature.Provider with a mutable BundleHash
// for simulating trust bundle rotation between reconcile calls.
type rotationMockProvider struct {
	mu         sync.RWMutex
	bundleHash string
	keyID      string
	spiffeID   string
	verified   bool
}

func (p *rotationMockProvider) VerifySignature(_ context.Context, _ *agentv1alpha1.AgentCardData, _ []agentv1alpha1.AgentCardSignature) (*signature.VerificationResult, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return &signature.VerificationResult{
		Verified: p.verified,
		SpiffeID: p.spiffeID,
		KeyID:    p.keyID,
		Details:  "rotation mock provider",
	}, nil
}

func (p *rotationMockProvider) Name() string { return "rotation-mock" }

func (p *rotationMockProvider) BundleHash() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bundleHash
}

func (p *rotationMockProvider) setBundleHash(hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bundleHash = hash
	p.keyID = "key-" + hash
}

const rotationTestNamespace = "trust-bundle-rotation-test"

func TestTrustBundleRotation(t *testing.T) {
	ctx := context.Background()

	setupClient(t)

	ns := createNamespace(t, ctx, rotationTestNamespace)
	defer deleteResource(ctx, ns)

	deploymentName := "rotation-test-agent"
	cardName := "rotation-test-card"
	saName := "default"

	t.Log("=== Creating test resources ===")

	deployment := createTestDeployment(t, ctx, rotationTestNamespace, deploymentName, saName, 1)
	defer deleteResource(ctx, deployment)

	service := createTestService(t, ctx, rotationTestNamespace, deploymentName)
	defer deleteResource(ctx, service)

	agentCard := createTestAgentCard(t, ctx, rotationTestNamespace, cardName, deploymentName, "example.org", true)
	defer deleteResource(ctx, agentCard)

	provider := &rotationMockProvider{
		bundleHash: "hash-v1",
		keyID:      "key-v1",
		spiffeID:   "spiffe://example.org/ns/" + rotationTestNamespace + "/sa/" + saName,
		verified:   true,
	}

	reconciler := &controller.AgentCardReconciler{
		Client:                k8sClient,
		Scheme:                scheme,
		AgentFetcher:          &mockFetcher{},
		SignatureProvider:     provider,
		RequireSignature:      true,
		SVIDExpiryGracePeriod: 1 * time.Millisecond,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cardName, Namespace: rotationTestNamespace},
	}

	// --- Phase 1: Initial reconciliation records the bundle hash ---
	t.Log("=== Phase 1: Initial reconciliation (bundle hash-v1) ===")

	waitForDeploymentAvailable(t, ctx, deploymentName, rotationTestNamespace, 30*time.Second)

	for i := 0; i < 5; i++ {
		result, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Logf("  Reconcile pass %d error (may be expected): %v", i+1, err)
		} else {
			t.Logf("  Reconcile pass %d: requeue=%v, requeueAfter=%v", i+1, result.Requeue, result.RequeueAfter)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Assert AgentCard conditions and capture initial keyID
	phase1Card := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: rotationTestNamespace}, phase1Card); err != nil {
		t.Fatalf("Failed to get AgentCard in Phase 1: %v", err)
	}
	for _, c := range phase1Card.Status.Conditions {
		t.Logf("  AgentCard condition: %s=%s (%s: %s)", c.Type, c.Status, c.Reason, c.Message)
	}
	sigCond := meta.FindStatusCondition(phase1Card.Status.Conditions, "SignatureVerified")
	if sigCond == nil || sigCond.Status != metav1.ConditionTrue {
		t.Fatalf("Expected SignatureVerified=True in Phase 1, got %v", sigCond)
	}
	initialKeyID := phase1Card.Status.SignatureKeyID
	t.Logf("  SignatureVerified=True, keyID=%s", initialKeyID)

	dep := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: rotationTestNamespace}, dep); err != nil {
		t.Fatalf("Failed to get Deployment: %v", err)
	}

	bundleHash := dep.Annotations[controller.AnnotationBundleHash]
	if bundleHash != "hash-v1" {
		t.Fatalf("Expected bundle-hash=hash-v1, got %q", bundleHash)
	}
	t.Logf("  bundle-hash annotation: %s", bundleHash)

	podResignTrigger := dep.Spec.Template.Annotations[controller.AnnotationResignTrigger]
	t.Logf("  resign-trigger (before rotation): %q", podResignTrigger)

	// --- Phase 2: Simulate trust bundle rotation ---
	t.Log("=== Phase 2: Simulate CA rotation (bundle hash-v2) ===")

	provider.setBundleHash("hash-v2")

	time.Sleep(100 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Logf("  Reconcile pass %d error (may be expected): %v", i+1, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: rotationTestNamespace}, dep); err != nil {
		t.Fatalf("Failed to get Deployment after rotation: %v", err)
	}

	newBundleHash := dep.Annotations[controller.AnnotationBundleHash]
	if newBundleHash != "hash-v2" {
		t.Fatalf("Expected bundle-hash=hash-v2 after rotation, got %q", newBundleHash)
	}
	t.Logf("  bundle-hash annotation updated: %s -> %s", bundleHash, newBundleHash)

	newResignTrigger := dep.Spec.Template.Annotations[controller.AnnotationResignTrigger]
	if newResignTrigger == "" {
		t.Fatal("Expected resign-trigger annotation to be set after rotation, but it is empty")
	}

	if _, err := time.Parse(time.RFC3339, newResignTrigger); err != nil {
		t.Fatalf("resign-trigger is not valid RFC3339: %q (%v)", newResignTrigger, err)
	}
	t.Logf("  resign-trigger set: %s", newResignTrigger)

	// --- Phase 3: Verify AgentCard status ---
	t.Log("=== Phase 3: Verify AgentCard conditions ===")

	card := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: rotationTestNamespace}, card); err != nil {
		t.Fatalf("Failed to get AgentCard: %v", err)
	}

	for _, expectedCond := range []struct {
		Type   string
		Status metav1.ConditionStatus
	}{
		{"SignatureVerified", metav1.ConditionTrue},
		{"Synced", metav1.ConditionTrue},
		{"Ready", metav1.ConditionTrue},
		{"Bound", metav1.ConditionTrue},
	} {
		cond := meta.FindStatusCondition(card.Status.Conditions, expectedCond.Type)
		if cond == nil {
			t.Errorf("Expected condition %s to exist", expectedCond.Type)
		} else if cond.Status != expectedCond.Status {
			t.Errorf("Expected %s=%s, got %s (%s: %s)",
				expectedCond.Type, expectedCond.Status, cond.Status, cond.Reason, cond.Message)
		} else {
			t.Logf("  %s=%s (%s)", expectedCond.Type, cond.Status, cond.Reason)
		}
	}

	postRotationKeyID := card.Status.SignatureKeyID
	if postRotationKeyID == initialKeyID {
		t.Errorf("Expected SignatureKeyID to change after rotation, still %q", postRotationKeyID)
	}
	t.Logf("  SignatureKeyID changed: %s -> %s", initialKeyID, postRotationKeyID)

	t.Log("=== PASSED: Trust bundle rotation detected and rollout restart triggered ===")
}

func waitForDeploymentAvailable(t *testing.T, ctx context.Context, name, ns string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, dep); err == nil {
			for _, c := range dep.Status.Conditions {
				if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
					t.Logf("  Deployment %s available", name)
					return
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("Deployment %s in namespace %s did not become available within %s", name, ns, timeout)
}

func createNamespace(t *testing.T, ctx context.Context, name string) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		if !errors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", name, err)
		}
	}
	t.Logf("  Using namespace: %s", name)
	return ns
}
