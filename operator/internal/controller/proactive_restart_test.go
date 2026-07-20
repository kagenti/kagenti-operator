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

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/signature"
)

type mockSignatureProviderWithBundleHash struct {
	pubKeyPEM  []byte
	spiffeID   string
	bundleHash string
	leafExpiry time.Time
}

func (m *mockSignatureProviderWithBundleHash) Name() string       { return "mock" }
func (m *mockSignatureProviderWithBundleHash) BundleHash() string { return m.bundleHash }

func (m *mockSignatureProviderWithBundleHash) VerifySignature(_ context.Context, cardData *agentv1alpha1.AgentCardData,
	signatures []agentv1alpha1.AgentCardSignature) (*signature.VerificationResult, error) {
	for i := range signatures {
		result, err := signature.VerifyJWS(cardData, &signatures[i], m.pubKeyPEM)
		if err == nil && result != nil && result.Verified {
			result.SpiffeID = m.spiffeID
			result.LeafNotAfter = m.leafExpiry
			return result, nil
		}
	}
	return &signature.VerificationResult{Verified: false, Details: "no valid signature"}, nil
}

func setBundleHashAnnotation(ctx context.Context, name, ns, hash string) {
	Eventually(func() error {
		d := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, d); err != nil {
			return err
		}
		if d.Annotations == nil {
			d.Annotations = make(map[string]string)
		}
		d.Annotations[AnnotationBundleHash] = hash
		return k8sClient.Update(ctx, d)
	}).Should(Succeed())
}

func getResignTrigger(ctx context.Context, name, ns string) string {
	d := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, d); err != nil {
		return ""
	}
	if d.Spec.Template.Annotations == nil {
		return ""
	}
	return d.Spec.Template.Annotations[AnnotationResignTrigger]
}

func getResignLeafExpiry(ctx context.Context, name, ns string) string {
	d := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, d); err != nil {
		return ""
	}
	if d.Spec.Template.Annotations == nil {
		return ""
	}
	return d.Spec.Template.Annotations[AnnotationResignLeafExpiry]
}

func setResignTriggerAndLeafExpiry(ctx context.Context, name, ns, triggerTime, leafExpiry string) {
	Eventually(func() error {
		d := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, d); err != nil {
			return err
		}
		if d.Spec.Template.Annotations == nil {
			d.Spec.Template.Annotations = make(map[string]string)
		}
		d.Spec.Template.Annotations[AnnotationResignTrigger] = triggerTime
		d.Spec.Template.Annotations[AnnotationResignLeafExpiry] = leafExpiry
		return k8sClient.Update(ctx, d)
	}).Should(Succeed())
}

// reconcileTwice runs two reconcile cycles: the first adds the finalizer,
// the second performs the actual card fetch, verification, and status update.
func reconcileTwice(reconciler *AgentCardReconciler, name, ns string) {
	nn := types.NamespacedName{Name: name, Namespace: ns}
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

var _ = Describe("Proactive Restart for Re-signing", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("Trust bundle rotation triggers workload restart", func() {
		const (
			deploymentName = "restart-bundle-agent"
			agentCardName  = "restart-bundle-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should patch pod template annotation when trust bundle hash changes", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)
			setBundleHashAnnotation(ctx, deploymentName, namespace, "old-bundle-hash")

			makeCardData := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name: "Bundle Restart Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				AgentFetcher:     &mockFetcherFunc{fn: makeCardData},
				RequireSignature: true,
				SignatureProvider: &mockSignatureProviderWithBundleHash{
					pubKeyPEM: pubPEM, bundleHash: "new-bundle-hash", leafExpiry: time.Now().Add(24 * time.Hour),
				},
			}

			reconcileTwice(reconciler, agentCardName, namespace)

			Eventually(func() string { return getResignTrigger(ctx, deploymentName, namespace) }, timeout, interval).ShouldNot(BeEmpty())

			d := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d)).To(Succeed())
			Expect(d.Annotations[AnnotationBundleHash]).To(Equal("new-bundle-hash"))
		})
	})

	Context("SVID expiry triggers workload restart", func() {
		const (
			deploymentName = "restart-svid-agent"
			agentCardName  = "restart-svid-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should trigger restart when SVID leaf cert is within grace period", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)
			setBundleHashAnnotation(ctx, deploymentName, namespace, "same-hash")

			makeCardData := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name: "SVID Expiry Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				AgentFetcher:     &mockFetcherFunc{fn: makeCardData},
				RequireSignature: true,
				SignatureProvider: &mockSignatureProviderWithBundleHash{
					pubKeyPEM: pubPEM, bundleHash: "same-hash", leafExpiry: time.Now().Add(DefaultSVIDExpiryGracePeriod / 6),
				},
			}

			reconcileTwice(reconciler, agentCardName, namespace)

			Eventually(func() string { return getResignTrigger(ctx, deploymentName, namespace) }, timeout, interval).ShouldNot(BeEmpty())
		})
	})

	Context("No cyclic restart for same expiring cert (issue #246)", func() {
		const (
			deploymentName = "restart-cycle-agent"
			agentCardName  = "restart-cycle-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should not trigger restart when the same cert expiry was already handled", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)
			setBundleHashAnnotation(ctx, deploymentName, namespace, "same-hash")

			// Simulate a cert that is within the grace period (expiring in 5 min).
			leafExpiry := time.Now().Add(5 * time.Minute)

			// Pre-set resign-trigger and resign-leaf-expiry as if a previous
			// reconcile already triggered a restart for this exact cert.
			// The trigger was set > grace period ago (cooldown expired).
			oldTrigger := time.Now().Add(-DefaultSVIDExpiryGracePeriod - time.Minute)
			setResignTriggerAndLeafExpiry(ctx, deploymentName, namespace,
				oldTrigger.Format(time.RFC3339), leafExpiry.Format(time.RFC3339))

			makeCardData := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name: "Cycle Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				AgentFetcher:     &mockFetcherFunc{fn: makeCardData},
				RequireSignature: true,
				SignatureProvider: &mockSignatureProviderWithBundleHash{
					pubKeyPEM: pubPEM, bundleHash: "same-hash", leafExpiry: leafExpiry,
				},
			}

			reconcileTwice(reconciler, agentCardName, namespace)

			// The resign-trigger should NOT be updated — it should remain the old value.
			trigger := getResignTrigger(ctx, deploymentName, namespace)
			Expect(trigger).To(Equal(oldTrigger.Format(time.RFC3339)))
		})

		It("should trigger restart when cert is renewed with a new expiry that is also near grace", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)
			setBundleHashAnnotation(ctx, deploymentName, namespace, "same-hash")

			// Old cert was handled (expiry was stored).
			oldLeafExpiry := time.Now().Add(-10 * time.Minute) // already expired
			oldTrigger := time.Now().Add(-DefaultSVIDExpiryGracePeriod - time.Minute)
			setResignTriggerAndLeafExpiry(ctx, deploymentName, namespace,
				oldTrigger.Format(time.RFC3339), oldLeafExpiry.Format(time.RFC3339))

			// New cert has a DIFFERENT expiry but is also within grace.
			newLeafExpiry := time.Now().Add(5 * time.Minute)

			makeCardData := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name: "Cycle Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				AgentFetcher:     &mockFetcherFunc{fn: makeCardData},
				RequireSignature: true,
				SignatureProvider: &mockSignatureProviderWithBundleHash{
					pubKeyPEM: pubPEM, bundleHash: "same-hash", leafExpiry: newLeafExpiry,
				},
			}

			reconcileTwice(reconciler, agentCardName, namespace)

			// Should trigger restart because the cert is different from the one we already handled.
			trigger := getResignTrigger(ctx, deploymentName, namespace)
			Expect(trigger).NotTo(Equal(oldTrigger.Format(time.RFC3339)))
			Expect(trigger).NotTo(BeEmpty())

			// And the stored leaf expiry should now match the new cert.
			storedExpiry := getResignLeafExpiry(ctx, deploymentName, namespace)
			Expect(storedExpiry).To(Equal(newLeafExpiry.Format(time.RFC3339)))
		})
	})

	Context("SVID restart stores leaf expiry annotation", func() {
		const (
			deploymentName = "restart-expiry-store-agent"
			agentCardName  = "restart-expiry-store-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should store resign-leaf-expiry when triggering SVID restart", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)
			setBundleHashAnnotation(ctx, deploymentName, namespace, "same-hash")

			leafExpiry := time.Now().Add(DefaultSVIDExpiryGracePeriod / 6)

			makeCardData := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name: "Expiry Store Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				AgentFetcher:     &mockFetcherFunc{fn: makeCardData},
				RequireSignature: true,
				SignatureProvider: &mockSignatureProviderWithBundleHash{
					pubKeyPEM: pubPEM, bundleHash: "same-hash", leafExpiry: leafExpiry,
				},
			}

			reconcileTwice(reconciler, agentCardName, namespace)

			Eventually(func() string { return getResignTrigger(ctx, deploymentName, namespace) }, timeout, interval).ShouldNot(BeEmpty())
			storedExpiry := getResignLeafExpiry(ctx, deploymentName, namespace)
			Expect(storedExpiry).To(Equal(leafExpiry.Format(time.RFC3339)))
		})
	})

	Context("No restart when cert is healthy and bundle unchanged", func() {
		const (
			deploymentName = "restart-noop-agent"
			agentCardName  = "restart-noop-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should not trigger restart", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)
			setBundleHashAnnotation(ctx, deploymentName, namespace, "stable-hash")

			makeCardData := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name: "No Restart Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				AgentFetcher:     &mockFetcherFunc{fn: makeCardData},
				RequireSignature: true,
				SignatureProvider: &mockSignatureProviderWithBundleHash{
					pubKeyPEM: pubPEM, bundleHash: "stable-hash", leafExpiry: time.Now().Add(2 * time.Hour),
				},
			}

			reconcileTwice(reconciler, agentCardName, namespace)

			Expect(getResignTrigger(ctx, deploymentName, namespace)).To(BeEmpty())
		})
	})
})
