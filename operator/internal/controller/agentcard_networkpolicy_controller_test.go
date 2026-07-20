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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

func newNPReconciler(enforce bool) *AgentCardNetworkPolicyReconciler {
	return &AgentCardNetworkPolicyReconciler{
		Client:                 k8sClient,
		Scheme:                 k8sClient.Scheme(),
		EnforceNetworkPolicies: enforce,
		OperatorNamespace:      "rossoctl-operator-system",
		KubeAPIServerCIDRs:     []string{"10.0.0.1/32", "10.0.0.2/32"},
	}
}

func reconcileNP(reconciler *AgentCardNetworkPolicyReconciler, name, ns string) {
	nn := types.NamespacedName{Name: name, Namespace: ns}
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func getPolicy(name, ns string) *netv1.NetworkPolicy {
	p := &netv1.NetworkPolicy{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name + "-signature-policy", Namespace: ns}, p)).To(Succeed())
	return p
}

func createCardWithStatus(name, ns, deploymentName string, validSig *bool, identityMatch *bool, binding *agentv1alpha1.IdentityBinding) {
	card := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			IdentityBinding: binding,
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, card)).To(Succeed())
	if validSig != nil || identityMatch != nil {
		card.Status.ValidSignature = validSig
		card.Status.SignatureIdentityMatch = identityMatch

		verified := false
		if validSig != nil && *validSig {
			verified = true
		}
		if binding != nil && identityMatch != nil {
			if *identityMatch {
				verified = true
			} else if binding.Strict {
				verified = false
			} else {
				verified = true
			}
		}
		verifiedStatus := metav1.ConditionFalse
		reason := "SignatureInvalid"
		if verified {
			verifiedStatus = metav1.ConditionTrue
			reason = "SignatureVerified"
		}
		card.Status.Conditions = []metav1.Condition{{
			Type:               ConditionVerified,
			Status:             verifiedStatus,
			Reason:             reason,
			Message:            "test",
			LastTransitionTime: metav1.Now(),
		}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, card)).To(Succeed())
	}
}

func policyHasVerifiedPodIngress(p *netv1.NetworkPolicy) bool {
	for _, peer := range p.Spec.Ingress[0].From {
		if peer.PodSelector != nil && peer.PodSelector.MatchLabels[LabelSignatureVerified] == "true" {
			return true
		}
	}
	return false
}

var _ = Describe("AgentCardNetworkPolicyReconciler", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("Enforcement disabled", func() {
		const (
			deploymentName = "np-disabled-agent"
			agentCardName  = "np-disabled-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should not add finalizer or create NetworkPolicy", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, ptr.To(true), nil, nil)

			reconcileNP(newNPReconciler(false), agentCardName, namespace)

			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(card, NetworkPolicyFinalizer)).To(BeFalse())

			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName + "-signature-policy", Namespace: namespace}, &netv1.NetworkPolicy{})
			Expect(err).To(HaveOccurred())
		})
	})

	Context("Verified agent gets permissive policy", func() {
		const (
			deploymentName = "np-permissive-agent"
			agentCardName  = "np-permissive-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should allow ingress from verified pods and operator namespace", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, ptr.To(true), nil, nil)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", deploymentName))
			Expect(p.Spec.Ingress).To(HaveLen(1))
			Expect(p.Spec.Ingress[0].From).To(HaveLen(3))
			Expect(policyHasVerifiedPodIngress(p)).To(BeTrue())
		})
	})

	Context("Unverified agent gets restrictive policy", func() {
		const (
			deploymentName = "np-restrictive-agent"
			agentCardName  = "np-restrictive-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should restrict ingress to operator namespace only when ValidSignature=false", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, ptr.To(false), nil, nil)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress).To(HaveLen(1))
			Expect(p.Spec.Ingress[0].From).To(HaveLen(2))
			Expect(p.Spec.Ingress[0].From[0].PodSelector).To(BeNil())
			Expect(p.Spec.Egress).To(HaveLen(1))
			Expect(p.Spec.Egress[0].To).To(HaveLen(2))
			Expect(p.Spec.Egress[0].To[0].IPBlock.CIDR).To(Equal("10.0.0.1/32"))
			Expect(p.Spec.Egress[0].To[1].IPBlock.CIDR).To(Equal("10.0.0.2/32"))
			Expect(p.Spec.Egress[0].Ports).To(HaveLen(1))
			Expect(p.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(6443))
		})

		It("should restrict when ValidSignature=nil", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, nil, nil, nil)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress).To(HaveLen(1))
			Expect(p.Spec.Egress).To(HaveLen(1))
			Expect(p.Spec.Egress[0].To).To(HaveLen(2))
		})
	})

	Context("Identity binding uses SignatureIdentityMatch", func() {
		const (
			deploymentName = "np-identity-agent"
			agentCardName  = "np-identity-card"
			namespace      = "default"
		)

		binding := &agentv1alpha1.IdentityBinding{TrustDomain: "test.local"}

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should create permissive policy when SignatureIdentityMatch=true", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, nil, ptr.To(true), binding)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From).To(HaveLen(3))
			Expect(policyHasVerifiedPodIngress(p)).To(BeTrue())
		})

		It("should create restrictive policy when SignatureIdentityMatch=false and strict=true", func() {
			strictBinding := &agentv1alpha1.IdentityBinding{TrustDomain: "test.local", Strict: true}
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, nil, ptr.To(false), strictBinding)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From[0].PodSelector).To(BeNil())
		})

		It("should create permissive policy when SignatureIdentityMatch=false and strict=false", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, nil, ptr.To(false), binding)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From).To(HaveLen(3))
			Expect(policyHasVerifiedPodIngress(p)).To(BeTrue())
		})
	})

	Context("Policy updates when verification status changes", func() {
		const (
			deploymentName = "np-update-agent"
			agentCardName  = "np-update-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should switch from permissive to restrictive when verification fails", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, ptr.To(true), nil, nil)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			Expect(getPolicy(deploymentName, namespace).Spec.Ingress[0].From).To(HaveLen(3))

			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			card.Status.ValidSignature = ptr.To(false)
			for i := range card.Status.Conditions {
				if card.Status.Conditions[i].Type == ConditionVerified {
					card.Status.Conditions[i].Status = metav1.ConditionFalse
					card.Status.Conditions[i].Reason = "SignatureInvalid"
				}
			}
			Expect(k8sClient.Status().Update(ctx, card)).To(Succeed())

			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From).To(HaveLen(2))
			Expect(p.Spec.Ingress[0].From[0].PodSelector).To(BeNil())
		})
	})

	Context("Deletion cleanup", func() {
		const (
			deploymentName = "np-delete-agent"
			agentCardName  = "np-delete-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should delete NetworkPolicy and remove finalizer", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, ptr.To(true), nil, nil)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			getPolicy(deploymentName, namespace)

			toDelete := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, toDelete)).To(Succeed())

			reconcileNP(r, agentCardName, namespace)

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName + "-signature-policy", Namespace: namespace}, &netv1.NetworkPolicy{})
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, &agentv1alpha1.AgentCard{})
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("Missing workload", func() {
		const (
			agentCardName = "np-missing-workload-card"
			namespace     = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
		})

		It("should return without error and not create a policy", func() {
			createCardWithStatus(agentCardName, namespace, "non-existent-deployment", ptr.To(true), nil, nil)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			err := k8sClient.Get(ctx, types.NamespacedName{Name: "non-existent-deployment-signature-policy", Namespace: namespace}, &netv1.NetworkPolicy{})
			Expect(err).To(HaveOccurred())
		})
	})

	Context("Verified condition (Phase 1 mTLS path)", func() {
		const (
			deploymentName = "np-verified-cond-agent"
			agentCardName  = "np-verified-cond-card"
			namespace      = "default"
		)

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should create permissive policy when Verified condition is True", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithVerifiedCondition(agentCardName, namespace, deploymentName, metav1.ConditionTrue)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From).To(HaveLen(3))
			Expect(policyHasVerifiedPodIngress(p)).To(BeTrue())
		})

		It("should create restrictive policy when Verified condition is False", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithVerifiedCondition(agentCardName, namespace, deploymentName, metav1.ConditionFalse)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From).To(HaveLen(2))
			Expect(p.Spec.Ingress[0].From[0].PodSelector).To(BeNil())
		})

		It("should be permissive when Verified condition is True (set via signature)", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)
			createCardWithStatus(agentCardName, namespace, deploymentName, ptr.To(true), nil, nil)

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From).To(HaveLen(3))
			Expect(policyHasVerifiedPodIngress(p)).To(BeTrue())
		})

		It("Verified condition takes precedence over ValidSignature", func() {
			createDeploymentWithService(ctx, deploymentName, namespace)

			card := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, card)).To(Succeed())

			card.Status.ValidSignature = ptr.To(true)
			card.Status.Conditions = []metav1.Condition{
				{
					Type:               ConditionVerified,
					Status:             metav1.ConditionFalse,
					Reason:             "FetchFailed",
					Message:            "mTLS fetch failed",
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, card)).To(Succeed())

			r := newNPReconciler(true)
			reconcileNP(r, agentCardName, namespace)
			reconcileNP(r, agentCardName, namespace)

			p := getPolicy(deploymentName, namespace)
			Expect(p.Spec.Ingress[0].From).To(HaveLen(2))
			Expect(p.Spec.Ingress[0].From[0].PodSelector).To(BeNil())
		})
	})
})

func createCardWithVerifiedCondition(name, ns, deploymentName string, condStatus metav1.ConditionStatus) {
	card := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, card)).To(Succeed())

	card.Status.Conditions = []metav1.Condition{
		{
			Type:               ConditionVerified,
			Status:             condStatus,
			Reason:             "mTLSVerified",
			Message:            "test condition",
			LastTransitionTime: metav1.Now(),
		},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, card)).To(Succeed())
}
