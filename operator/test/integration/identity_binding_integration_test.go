//go:build integration
// +build integration

/*
Copyright 2025.

Integration test for AgentCard Identity Binding (Step 1)

Run with: go test -v -tags=integration ./test/integration/... -timeout 5m
Prerequisites: kubectl configured with access to a Kubernetes cluster with rossoctl CRDs installed
*/

package integration

import (
	"context"
	"fmt"
	"os"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	k8sClient     client.Client
	scheme        = runtime.NewScheme()
	testNamespace = "identity-binding-test"
	trustDomain   = "cluster.local"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentv1alpha1.AddToScheme(scheme))
}

func setupClient(t *testing.T) {
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get kubeconfig: %v", err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
}

func setupNamespace(t *testing.T, ctx context.Context) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}

	err := k8sClient.Create(ctx, ns)
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("Failed to create test namespace: %v", err)
	}
	t.Logf("Using test namespace: %s", testNamespace)
}

func cleanupNamespace(t *testing.T, ctx context.Context) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	if err := k8sClient.Delete(ctx, ns); err != nil {
		t.Logf("Warning: failed to delete test namespace: %v", err)
	}
}

// mockFetcher returns minimal valid AgentCardData so the reconciler does not
// panic on nil cardData when it reaches the fetch → verify → bind path.
//
// Known limitation: this mock returns hardcoded data regardless of the actual
// workload, so integration tests exercise the reconciler's verify → bind logic
// but do NOT exercise the full fetch → sign → verify → bind end-to-end path.
type mockFetcher struct{}

func (f *mockFetcher) Fetch(_ context.Context, _, _ string, _ string, _ string) (*agentcard.FetchResult, error) {
	return &agentcard.FetchResult{
		CardData: &agentv1alpha1.AgentCardData{
			Name: "test-agent",
			URL:  "http://test:8000",
		},
	}, nil
}

// mockSignatureProvider returns a controlled VerificationResult so the reconciler
// exercises the full binding path without requiring real JWS signatures.
type mockSignatureProvider struct {
	spiffeID string
	verified bool
}

func (p *mockSignatureProvider) VerifySignature(_ context.Context, _ *agentv1alpha1.AgentCardData, _ []agentv1alpha1.AgentCardSignature) (*signature.VerificationResult, error) {
	return &signature.VerificationResult{
		Verified: p.verified,
		SpiffeID: p.spiffeID,
		Details:  "mock provider",
	}, nil
}

func (p *mockSignatureProvider) Name() string       { return "mock" }
func (p *mockSignatureProvider) BundleHash() string { return "" }

func TestIdentityBindingIntegration(t *testing.T) {
	ctx := context.Background()

	setupClient(t)
	setupNamespace(t, ctx)
	defer cleanupNamespace(t, ctx)

	t.Run("Test1_MatchingBindingEvaluation", testMatchingBindingEvaluation)
	t.Run("Test2_NonMatchingBindingEvaluation", testNonMatchingBindingEvaluation)
}

func testMatchingBindingEvaluation(t *testing.T) {
	ctx := context.Background()
	deploymentName := "test-match-deploy"
	cardName := "test-match-card"
	saName := "test-sa"

	t.Log("\n========================================")
	t.Log("TEST 1: Matching Binding Evaluation")
	t.Log("========================================")

	// Create the ServiceAccount referenced by the Deployment so pods can start.
	sa := createServiceAccount(t, ctx, testNamespace, saName)
	defer deleteResource(ctx, sa)

	// Create Deployment with agent labels
	deployment := createTestDeployment(t, ctx, testNamespace, deploymentName, saName, 1)
	defer deleteResource(ctx, deployment)

	// Create Service
	service := createTestService(t, ctx, testNamespace, deploymentName)
	defer deleteResource(ctx, service)

	// Create AgentCard with matching SPIFFE ID using targetRef
	expectedSpiffeID := fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, testNamespace, saName)
	agentCard := createTestAgentCard(t, ctx, testNamespace, cardName, deploymentName, trustDomain, false)
	defer deleteResource(ctx, agentCard)

	// Wait for the Deployment to become Available — the reconciler checks
	// workload readiness and returns early if the Deployment is not ready,
	// which would leave BindingStatus nil.
	waitForDeploymentAvailable(t, ctx, deploymentName, testNamespace, 30*time.Second)

	// Create and run AgentCard reconciler with a mock signature provider that
	// returns the expected SPIFFE ID, so the reconciler exercises the full
	// binding evaluation path (not just pre-set status).
	reconciler := &controller.AgentCardReconciler{
		Client:           k8sClient,
		Scheme:           scheme,
		AgentFetcher:     &mockFetcher{},
		RequireSignature: true,
		SignatureProvider: &mockSignatureProvider{
			spiffeID: expectedSpiffeID,
			verified: true,
		},
	}

	// First reconcile adds finalizer
	_, _ = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cardName, Namespace: testNamespace},
	})
	time.Sleep(100 * time.Millisecond)

	// Subsequent reconciles evaluate binding via the mock provider
	for i := 0; i < 2; i++ {
		_, _ = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: cardName, Namespace: testNamespace},
		})
		time.Sleep(100 * time.Millisecond)
	}

	// Verify binding status
	card := &agentv1alpha1.AgentCard{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: testNamespace}, card); err != nil {
		t.Fatalf("Failed to get AgentCard: %v", err)
	}

	if card.Status.BindingStatus == nil {
		t.Fatal("❌ BindingStatus is nil")
	}

	if !card.Status.BindingStatus.Bound {
		t.Fatalf("❌ Expected Bound=true, got Bound=%v, Reason=%s, Message=%s",
			card.Status.BindingStatus.Bound,
			card.Status.BindingStatus.Reason,
			card.Status.BindingStatus.Message)
	}

	t.Logf("✓ Binding Status: Bound=%v", card.Status.BindingStatus.Bound)
	t.Logf("✓ Reason: %s", card.Status.BindingStatus.Reason)
	t.Logf("✓ Expected SPIFFE ID: %s", card.Status.ExpectedSpiffeID)
	t.Log("✓ TEST 1 PASSED: Matching binding evaluated as Bound")
}

func testNonMatchingBindingEvaluation(t *testing.T) {
	ctx := context.Background()
	deploymentName := "test-nomatch-deploy"
	cardName := "test-nomatch-card"
	saName := "test-sa"

	t.Log("\n========================================")
	t.Log("TEST 2: Non-Matching Binding Evaluation")
	t.Log("========================================")

	// Create the ServiceAccount referenced by the Deployment so pods can start.
	sa := createServiceAccount(t, ctx, testNamespace, saName)
	defer deleteResource(ctx, sa)

	// Create Deployment with agent labels
	deployment := createTestDeployment(t, ctx, testNamespace, deploymentName, saName, 1)
	defer deleteResource(ctx, deployment)

	// Create Service
	service := createTestService(t, ctx, testNamespace, deploymentName)
	defer deleteResource(ctx, service)

	// Create AgentCard with NON-matching SPIFFE ID in allowlist using targetRef
	agentCard := createTestAgentCard(t, ctx, testNamespace, cardName, deploymentName, trustDomain, false)
	defer deleteResource(ctx, agentCard)

	// Wait for the Deployment to become Available before reconciling.
	waitForDeploymentAvailable(t, ctx, deploymentName, testNamespace, 30*time.Second)

	// The workload's actual SPIFFE ID (from mock provider) is from a different
	// trust domain, so it does NOT match the AgentCard's trust domain.
	actualSpiffeID := fmt.Sprintf("spiffe://other-domain/ns/%s/sa/%s", testNamespace, saName)

	// Create and run AgentCard reconciler with a mock provider returning the actual
	// (non-matching) SPIFFE ID
	reconciler := &controller.AgentCardReconciler{
		Client:           k8sClient,
		Scheme:           scheme,
		AgentFetcher:     &mockFetcher{},
		RequireSignature: true,
		SignatureProvider: &mockSignatureProvider{
			spiffeID: actualSpiffeID,
			verified: true,
		},
	}

	// First reconcile adds finalizer
	reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cardName, Namespace: testNamespace},
	})
	time.Sleep(100 * time.Millisecond)

	// Reconcile again to evaluate binding
	reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: cardName, Namespace: testNamespace},
	})

	// Verify binding status
	card := &agentv1alpha1.AgentCard{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: cardName, Namespace: testNamespace}, card)
	if err != nil {
		t.Fatalf("Failed to get AgentCard: %v", err)
	}

	if card.Status.BindingStatus == nil {
		t.Fatal("❌ BindingStatus is nil")
	}

	if card.Status.BindingStatus.Bound {
		t.Fatalf("❌ Expected Bound=false, got Bound=%v", card.Status.BindingStatus.Bound)
	}

	t.Logf("✓ Binding Status: Bound=%v", card.Status.BindingStatus.Bound)
	t.Logf("✓ Reason: %s", card.Status.BindingStatus.Reason)
	t.Logf("✓ Expected SPIFFE ID: %s", card.Status.ExpectedSpiffeID)
	t.Logf("✓ Trust Domain: %s", agentCard.Spec.IdentityBinding.TrustDomain)
	t.Log("✓ TEST 2 PASSED: Non-matching binding evaluated as NotBound")
}

// Helper functions

func createTestService(t *testing.T, ctx context.Context, ns, name string) *corev1.Service {
	t.Helper()
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{"app.kubernetes.io/name": name},
		},
	}

	err := k8sClient.Create(ctx, service)
	if err != nil {
		t.Fatalf("Failed to create Service: %v", err)
	}

	t.Logf("  Created Service: %s", name)
	return service
}

func createTestAgentCard(t *testing.T, ctx context.Context, ns, name, deploymentName, domain string, strict bool) *agentv1alpha1.AgentCard {
	t.Helper()
	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: agentv1alpha1.AgentCardSpec{
			SyncPeriod: "30s",
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			IdentityBinding: &agentv1alpha1.IdentityBinding{
				TrustDomain: domain,
				Strict:      strict,
			},
		},
	}

	err := k8sClient.Create(ctx, agentCard)
	if err != nil {
		t.Fatalf("Failed to create AgentCard: %v", err)
	}

	t.Logf("  Created AgentCard: %s (strict=%v)", name, strict)
	return agentCard
}

func createTestDeployment(t *testing.T, ctx context.Context, ns, name, saName string, replicas int32) *appsv1.Deployment {
	t.Helper()
	labels := map[string]string{
		"app.kubernetes.io/name":  name,
		"rossoctl.io/type":         "agent",
		"protocol.rossoctl.io/a2a": "",
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					ServiceAccountName: saName,
					Containers: []corev1.Container{
						{Name: "agent", Image: "registry.k8s.io/pause:3.9"},
					},
				},
			},
		},
	}

	err := k8sClient.Create(ctx, deployment)
	if err != nil {
		t.Fatalf("Failed to create Deployment: %v", err)
	}

	t.Logf("  Created Deployment: %s (replicas=%d)", name, replicas)
	return deployment
}

func createServiceAccount(t *testing.T, ctx context.Context, ns, name string) *corev1.ServiceAccount {
	t.Helper()
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if err := k8sClient.Create(ctx, sa); err != nil {
		t.Fatalf("Failed to create ServiceAccount: %v", err)
	}
	t.Logf("  Created ServiceAccount: %s", name)
	return sa
}

func deleteResource(ctx context.Context, obj client.Object) {
	// Remove finalizers first
	obj.SetFinalizers(nil)
	_ = k8sClient.Update(ctx, obj)
	_ = k8sClient.Delete(ctx, obj)
}

func TestMain(m *testing.M) {
	// Don't set logger to avoid recursion issues
	os.Exit(m.Run())
}
