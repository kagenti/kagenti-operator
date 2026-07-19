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
	"crypto/rsa"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

func buildTamperedCard(privKey *rsa.PrivateKey) *agentv1alpha1.AgentCardData {
	original := &agentv1alpha1.AgentCardData{
		Name: "Original Agent", Version: "1.0.0", URL: "http://localhost:8000",
	}
	jwsSig := buildTestJWS(original, privKey, "key-1", "")
	return &agentv1alpha1.AgentCardData{
		Name: "Tampered Agent", Version: "1.0.0", URL: "http://localhost:8000",
		Signatures: []agentv1alpha1.AgentCardSignature{jwsSig},
	}
}

var _ = Describe("Negative Signature Verification", func() {
	Context("Tampered card payload in enforce mode", func() {
		const (
			deploymentName = "sig-tamper-agent"
			agentCardName  = "sig-tamper-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should reject card with 1m requeue and set validSignature=false", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:            k8sClient,
				Scheme:            k8sClient.Scheme(),
				AgentFetcher:      &mockFetcher{cardData: buildTamperedCard(privKey)},
				RequireSignature:  true,
				SignatureProvider: &mockSignatureProvider{pubKeyPEM: pubPEM},
			}

			nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(1 * time.Minute))

			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, nn, card)).To(Succeed())
			Expect(card.Status.ValidSignature).NotTo(BeNil())
			Expect(*card.Status.ValidSignature).To(BeFalse())

			syncedCond := findCondition(card.Status.Conditions, "Synced")
			Expect(syncedCond).NotTo(BeNil())
			Expect(syncedCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(syncedCond.Reason).To(Equal(ReasonSignatureInvalid))

			sigCond := findCondition(card.Status.Conditions, "SignatureVerified")
			Expect(sigCond).NotTo(BeNil())
			Expect(sigCond.Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Context("Tampered card in audit mode still syncs", func() {
		const (
			deploymentName = "sig-tamper-audit-agent"
			agentCardName  = "sig-tamper-audit-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should sync card and requeue at sync period despite tampered payload", func() {
			privKey, pubPEM := generateTestRSAKeyPair()
			createDeploymentWithService(ctx, deploymentName, namespace)

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: buildTamperedCard(privKey)},
				RequireSignature:   true,
				SignatureProvider:  &mockSignatureProvider{pubKeyPEM: pubPEM},
				SignatureAuditMode: true,
			}

			nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, nn, card)).To(Succeed())

			syncedCond := findCondition(card.Status.Conditions, "Synced")
			Expect(syncedCond).NotTo(BeNil())
			Expect(syncedCond.Status).To(Equal(metav1.ConditionTrue))

			sigCond := findCondition(card.Status.Conditions, "SignatureVerified")
			Expect(sigCond).NotTo(BeNil())
			Expect(sigCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(sigCond.Reason).To(Equal(ReasonSignatureInvalidAudit))
		})
	})
})
