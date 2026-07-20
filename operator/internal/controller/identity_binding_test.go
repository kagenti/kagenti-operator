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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

var _ = Describe("Identity Binding — Trust Domain Only", func() {

	Context("computeBinding unit tests", func() {
		It("should return nil when identityBinding is not configured", func() {
			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				SpireTrustDomain: "example.org",
			}

			agentCard := &agentv1alpha1.AgentCard{
				Spec: agentv1alpha1.AgentCardSpec{},
			}

			result := reconciler.computeBinding(agentCard, "spiffe://example.org/ns/default/sa/test")
			Expect(result).To(BeNil())
		})

		It("should bind when SPIFFE ID matches operator trust domain", func() {
			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				SpireTrustDomain: "example.org",
			}

			agentCard := &agentv1alpha1.AgentCard{
				Spec: agentv1alpha1.AgentCardSpec{
					IdentityBinding: &agentv1alpha1.IdentityBinding{},
				},
			}

			result := reconciler.computeBinding(agentCard, "spiffe://example.org/ns/default/sa/agent")
			Expect(result).NotTo(BeNil())
			Expect(result.Bound).To(BeTrue())
			Expect(result.Reason).To(Equal(ReasonBound))
		})

		It("should not bind when SPIFFE ID belongs to wrong trust domain", func() {
			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				SpireTrustDomain: "example.org",
			}

			agentCard := &agentv1alpha1.AgentCard{
				Spec: agentv1alpha1.AgentCardSpec{
					IdentityBinding: &agentv1alpha1.IdentityBinding{},
				},
			}

			result := reconciler.computeBinding(agentCard, "spiffe://evil.com/ns/default/sa/agent")
			Expect(result).NotTo(BeNil())
			Expect(result.Bound).To(BeFalse())
			Expect(result.Reason).To(Equal(ReasonNotBound))
		})

		It("should use per-card trust domain override", func() {
			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				SpireTrustDomain: "default-domain.org",
			}

			agentCard := &agentv1alpha1.AgentCard{
				Spec: agentv1alpha1.AgentCardSpec{
					IdentityBinding: &agentv1alpha1.IdentityBinding{
						TrustDomain: "override-domain.org",
					},
				},
			}

			result := reconciler.computeBinding(agentCard, "spiffe://override-domain.org/ns/default/sa/agent")
			Expect(result).NotTo(BeNil())
			Expect(result.Bound).To(BeTrue())

			resultWrong := reconciler.computeBinding(agentCard, "spiffe://default-domain.org/ns/default/sa/agent")
			Expect(resultWrong).NotTo(BeNil())
			Expect(resultWrong.Bound).To(BeFalse())
		})

		It("should fail binding when no SPIFFE ID is provided", func() {
			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				SpireTrustDomain: "example.org",
			}

			agentCard := &agentv1alpha1.AgentCard{
				Spec: agentv1alpha1.AgentCardSpec{
					IdentityBinding: &agentv1alpha1.IdentityBinding{},
				},
			}

			result := reconciler.computeBinding(agentCard, "")
			Expect(result).NotTo(BeNil())
			Expect(result.Bound).To(BeFalse())
		})

		It("should fail binding when no trust domain is configured", func() {
			reconciler := &AgentCardReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			agentCard := &agentv1alpha1.AgentCard{
				Spec: agentv1alpha1.AgentCardSpec{
					IdentityBinding: &agentv1alpha1.IdentityBinding{},
				},
			}

			result := reconciler.computeBinding(agentCard, "spiffe://example.org/ns/default/sa/agent")
			Expect(result).NotTo(BeNil())
			Expect(result.Bound).To(BeFalse())
			Expect(result.Message).To(ContainSubstring("No trust domain configured"))
		})

		It("should not bind when SPIFFE ID exactly matches trust domain without path", func() {
			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				SpireTrustDomain: "example.org",
			}

			agentCard := &agentv1alpha1.AgentCard{
				Spec: agentv1alpha1.AgentCardSpec{
					IdentityBinding: &agentv1alpha1.IdentityBinding{},
				},
			}

			// spiffe://example.org/ with no path after the slash should not bind
			result := reconciler.computeBinding(agentCard, "spiffe://example.org/")
			Expect(result).NotTo(BeNil())
			Expect(result.Bound).To(BeFalse())
		})
	})

	Context("Card ID Drift Detection", func() {
		It("should compute consistent card ID for same card data", func() {
			reconciler := &AgentCardReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			cardData := &agentv1alpha1.AgentCardData{
				Name:        "Test Agent",
				Description: "A test agent",
				Version:     "1.0.0",
				URL:         "http://localhost:8000",
			}

			cardID1 := reconciler.computeCardID(cardData)
			cardID2 := reconciler.computeCardID(cardData)

			Expect(cardID1).NotTo(BeEmpty())
			Expect(cardID1).To(Equal(cardID2))
		})

		It("should compute different card ID for different card data", func() {
			reconciler := &AgentCardReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			cardData1 := &agentv1alpha1.AgentCardData{
				Name:    "Test Agent",
				Version: "1.0.0",
			}

			cardData2 := &agentv1alpha1.AgentCardData{
				Name:    "Test Agent",
				Version: "2.0.0",
			}

			cardID1 := reconciler.computeCardID(cardData1)
			cardID2 := reconciler.computeCardID(cardData2)

			Expect(cardID1).NotTo(BeEmpty())
			Expect(cardID2).NotTo(BeEmpty())
			Expect(cardID1).NotTo(Equal(cardID2))
		})
	})
})

// cleanupResource removes a resource and waits for it to be fully deleted
func cleanupResource(ctx context.Context, obj client.Object, name, namespace string) {
	key := types.NamespacedName{Name: name, Namespace: namespace}

	if err := k8sClient.Get(ctx, key, obj); err != nil {
		return
	}

	obj.SetFinalizers(nil)
	_ = k8sClient.Update(ctx, obj)
	_ = k8sClient.Delete(ctx, obj)

	Eventually(func() bool {
		err := k8sClient.Get(ctx, key, obj)
		return err != nil
	}, time.Second*5, time.Millisecond*100).Should(BeTrue())
}
