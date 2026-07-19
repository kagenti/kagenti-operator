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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"k8s.io/utils/ptr"
)

var _ = Describe("AgentCardSync Controller", func() {
	Context("When reconciling a Deployment with agent labels", func() {
		const (
			deploymentName = "test-sync-deployment"
			agentCardName  = "test-sync-deployment-deployment-card"
			namespace      = "default"
		)

		ctx := context.Background()

		deploymentNamespacedName := types.NamespacedName{
			Name:      deploymentName,
			Namespace: namespace,
		}

		BeforeEach(func() {
			By("creating a Deployment with rossoctl.io/type=agent label")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/name":    deploymentName,
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": deploymentName,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": deploymentName,
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "agent",
									Image: "test-image:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		})

		AfterEach(func() {
			By("cleaning up the Deployment resource")
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			if err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}

			By("cleaning up any created AgentCard resource")
			agentCard := &agentv1alpha1.AgentCard{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      agentCardName,
				Namespace: namespace,
			}, agentCard)
			if err == nil {
				Expect(k8sClient.Delete(ctx, agentCard)).To(Succeed())
			}
		})

		It("should automatically create an AgentCard with targetRef for Deployment", func() {
			By("reconciling the Deployment")
			reconciler := &AgentCardSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.ReconcileDeployment(ctx, reconcile.Request{
				NamespacedName: deploymentNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("checking that an AgentCard was created")
			agentCard := &agentv1alpha1.AgentCard{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      agentCardName,
					Namespace: namespace,
				}, agentCard)
				return err == nil
			}).Should(BeTrue())

			By("verifying the AgentCard has correct targetRef")
			Expect(agentCard.Spec.TargetRef).NotTo(BeNil())
			Expect(agentCard.Spec.TargetRef.APIVersion).To(Equal("apps/v1"))
			Expect(agentCard.Spec.TargetRef.Kind).To(Equal("Deployment"))
			Expect(agentCard.Spec.TargetRef.Name).To(Equal(deploymentName))

			By("verifying the AgentCard has owner reference")
			Expect(agentCard.OwnerReferences).NotTo(BeEmpty())
			Expect(agentCard.OwnerReferences[0].Kind).To(Equal("Deployment"))
			Expect(agentCard.OwnerReferences[0].Name).To(Equal(deploymentName))
		})

		It("should not create AgentCard for Deployments without protocol label", func() {
			const deploymentNoProtocol = "test-no-protocol-deployment"

			By("creating a Deployment without protocol label")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentNoProtocol,
					Namespace: namespace,
					Labels: map[string]string{
						LabelAgentType: LabelValueAgent,
						// No protocol label
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": deploymentNoProtocol,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": deploymentNoProtocol,
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "agent",
									Image: "test-image:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("reconciling the Deployment")
			reconciler := &AgentCardSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.ReconcileDeployment(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      deploymentNoProtocol,
					Namespace: namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying no AgentCard was created")
			agentCard := &agentv1alpha1.AgentCard{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      deploymentNoProtocol + "-deployment-card",
				Namespace: namespace,
			}, agentCard)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("cleaning up the test Deployment")
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})

	Context("When a manual AgentCard targets the same workload as an auto-created one", func() {
		const (
			deploymentName = "test-supersede-deployment"
			autoCardName   = "test-supersede-deployment-deployment-card"
			manualCardName = "test-supersede-manual"
			namespace      = "default"
		)

		ctx := context.Background()

		deploymentNN := types.NamespacedName{Name: deploymentName, Namespace: namespace}

		BeforeEach(func() {
			By("creating a labelled Deployment")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/name":    deploymentName,
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": deploymentName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "test:latest"}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		})

		AfterEach(func() {
			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, deploymentNN, deployment); err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}
			for _, name := range []string{autoCardName, manualCardName} {
				card := &agentv1alpha1.AgentCard{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, card); err == nil {
					Expect(k8sClient.Delete(ctx, card)).To(Succeed())
				}
			}
		})

		It("should delete the auto-created card when a manual card exists", func() {
			reconciler := &AgentCardSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("auto-creating the card via first reconcile")
			_, err := reconciler.ReconcileDeployment(ctx, reconcile.Request{NamespacedName: deploymentNN})
			Expect(err).NotTo(HaveOccurred())

			autoCard := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: autoCardName, Namespace: namespace}, autoCard)).To(Succeed())

			By("creating a manual AgentCard targeting the same Deployment")
			manualCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{
					Name:      manualCardName,
					Namespace: namespace,
				},
				Spec: agentv1alpha1.AgentCardSpec{
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, manualCard)).To(Succeed())

			By("reconciling again -- auto-card should be deleted")
			_, err = reconciler.ReconcileDeployment(ctx, reconcile.Request{NamespacedName: deploymentNN})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: autoCardName, Namespace: namespace}, autoCard)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "auto-created card should have been deleted")

			By("verifying the manual card still exists")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: manualCardName, Namespace: namespace}, manualCard)).To(Succeed())
		})

		It("should not delete a manually-created card even if another manual card exists", func() {
			By("creating two manual AgentCards targeting the same Deployment")
			for _, name := range []string{manualCardName, manualCardName + "-2"} {
				card := &agentv1alpha1.AgentCard{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
					Spec: agentv1alpha1.AgentCardSpec{
						TargetRef: &agentv1alpha1.TargetRef{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Name:       deploymentName,
						},
					},
				}
				Expect(k8sClient.Create(ctx, card)).To(Succeed())
			}

			reconciler := &AgentCardSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("reconciling -- sync controller should skip creation, not delete manual cards")
			_, err := reconciler.ReconcileDeployment(ctx, reconcile.Request{NamespacedName: deploymentNN})
			Expect(err).NotTo(HaveOccurred())

			By("verifying both manual cards still exist")
			for _, name := range []string{manualCardName, manualCardName + "-2"} {
				card := &agentv1alpha1.AgentCard{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, card)).To(Succeed())
			}

			By("verifying no auto-card was created")
			autoCard := &agentv1alpha1.AgentCard{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: autoCardName, Namespace: namespace}, autoCard)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("cleaning up second manual card")
			card := &agentv1alpha1.AgentCard{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: manualCardName + "-2", Namespace: namespace}, card); err == nil {
				Expect(k8sClient.Delete(ctx, card)).To(Succeed())
			}
		})
	})

	Context("When reconciling a StatefulSet with agent labels", func() {
		const (
			statefulsetName = "test-sync-statefulset"
			agentCardName   = "test-sync-statefulset-statefulset-card"
			namespace       = "default"
		)

		ctx := context.Background()

		statefulsetNamespacedName := types.NamespacedName{
			Name:      statefulsetName,
			Namespace: namespace,
		}

		BeforeEach(func() {
			By("creating a StatefulSet with rossoctl.io/type=agent label")
			statefulset := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      statefulsetName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/name":    statefulsetName,
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas:    ptr.To(int32(1)),
					ServiceName: statefulsetName,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": statefulsetName,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": statefulsetName,
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "agent",
									Image: "test-image:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, statefulset)).To(Succeed())
		})

		AfterEach(func() {
			By("cleaning up the StatefulSet resource")
			statefulset := &appsv1.StatefulSet{}
			err := k8sClient.Get(ctx, statefulsetNamespacedName, statefulset)
			if err == nil {
				Expect(k8sClient.Delete(ctx, statefulset)).To(Succeed())
			}

			By("cleaning up any created AgentCard resource")
			agentCard := &agentv1alpha1.AgentCard{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      agentCardName,
				Namespace: namespace,
			}, agentCard)
			if err == nil {
				Expect(k8sClient.Delete(ctx, agentCard)).To(Succeed())
			}
		})

		It("should automatically create an AgentCard with targetRef for StatefulSet", func() {
			By("reconciling the StatefulSet")
			reconciler := &AgentCardSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.ReconcileStatefulSet(ctx, reconcile.Request{
				NamespacedName: statefulsetNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("checking that an AgentCard was created")
			agentCard := &agentv1alpha1.AgentCard{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      agentCardName,
					Namespace: namespace,
				}, agentCard)
				return err == nil
			}).Should(BeTrue())

			By("verifying the AgentCard has correct targetRef")
			Expect(agentCard.Spec.TargetRef).NotTo(BeNil())
			Expect(agentCard.Spec.TargetRef.APIVersion).To(Equal("apps/v1"))
			Expect(agentCard.Spec.TargetRef.Kind).To(Equal("StatefulSet"))
			Expect(agentCard.Spec.TargetRef.Name).To(Equal(statefulsetName))

			By("verifying the AgentCard has owner reference")
			Expect(agentCard.OwnerReferences).NotTo(BeEmpty())
			Expect(agentCard.OwnerReferences[0].Kind).To(Equal("StatefulSet"))
			Expect(agentCard.OwnerReferences[0].Name).To(Equal(statefulsetName))
		})
	})

})
