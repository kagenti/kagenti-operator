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
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/agentcard"
)

// mockFetcher implements agentcard.Fetcher for testing
type mockFetcher struct {
	cardData *agentv1alpha1.AgentCardData
	err      error
}

func (m *mockFetcher) Fetch(
	ctx context.Context, protocol, url, _, _ string,
) (*agentcard.FetchResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &agentcard.FetchResult{CardData: m.cardData}, nil
}

// capturingProtocolFetcher records the protocol passed to Fetch (for multi-protocol label tests).
type capturingProtocolFetcher struct {
	mu       sync.Mutex
	protocol string
	cardData *agentv1alpha1.AgentCardData
}

func (c *capturingProtocolFetcher) Fetch(
	ctx context.Context, protocol, _, _, _ string,
) (*agentcard.FetchResult, error) {
	c.mu.Lock()
	c.protocol = protocol
	c.mu.Unlock()
	if c.cardData != nil {
		return &agentcard.FetchResult{CardData: c.cardData}, nil
	}
	return &agentcard.FetchResult{CardData: &agentv1alpha1.AgentCardData{
		Name:    "capture",
		Version: "1.0.0",
		URL:     "http://0.0.0.0:8000",
	}}, nil
}

func (c *capturingProtocolFetcher) lastProtocol() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocol
}

var _ = Describe("AgentCard Controller", func() {
	Context("When reconciling an AgentCard with a ready Deployment", func() {
		const (
			deploymentName = "test-card-agent"
			agentCardName  = "test-agentcard"
			serviceName    = "test-card-agent"
			namespace      = "default"
		)

		ctx := context.Background()

		agentCardNamespacedName := types.NamespacedName{
			Name:      agentCardName,
			Namespace: namespace,
		}

		BeforeEach(func() {
			By("creating a Deployment with agent labels")
			replicas := int32(1)
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
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name": deploymentName,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":    deploymentName,
								LabelAgentType:              LabelValueAgent,
								ProtocolLabelPrefix + "a2a": "",
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

			By("setting Deployment status to Available")
			Eventually(func() error {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err != nil {
					return err
				}
				deployment.Status.Conditions = []appsv1.DeploymentCondition{
					{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
				}
				return k8sClient.Status().Update(ctx, deployment)
			}).Should(Succeed())

			By("creating a Service for the Deployment")
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: namespace,
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     8000,
							Protocol: corev1.ProtocolTCP,
						},
					},
					Selector: map[string]string{
						"app.kubernetes.io/name": deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, service)).To(Succeed())

			By("creating an AgentCard with targetRef")
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{
					Name:      agentCardName,
					Namespace: namespace,
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
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())
		})

		AfterEach(func() {
			By("cleaning up resources")
			agentCard := &agentv1alpha1.AgentCard{}
			if err := k8sClient.Get(ctx, agentCardNamespacedName, agentCard); err == nil {
				Expect(k8sClient.Delete(ctx, agentCard)).To(Succeed())
			}

			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}

			service := &corev1.Service{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: namespace}, service); err == nil {
				Expect(k8sClient.Delete(ctx, service)).To(Succeed())
			}
		})

		It("should fetch agent card and override URL with service URL", func() {
			By("setting up a mock fetcher that returns agent card with 0.0.0.0 URL")
			mockCard := &agentv1alpha1.AgentCardData{
				Name:        "Test Agent",
				Description: "A test agent",
				Version:     "1.0.0",
				URL:         "http://0.0.0.0:8000", // Agent's advertised URL
				Skills: []agentv1alpha1.AgentSkill{
					{
						ID:          "skill-001",
						Name:        "test-skill",
						Description: "A test skill",
						Tags:        []string{"test", "example"},
						Examples:    []string{"Run a test scenario"},
					},
				},
			}

			reconciler := &AgentCardReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				AgentFetcher: &mockFetcher{
					cardData: mockCard,
					err:      nil,
				},
			}

			By("reconciling the AgentCard (first reconcile adds finalizer)")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: agentCardNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling again to fetch the agent card")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: agentCardNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying the AgentCard status was updated")
			agentCard := &agentv1alpha1.AgentCard{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, agentCardNamespacedName, agentCard)
				if err != nil {
					return false
				}
				return agentCard.Status.Card != nil
			}).Should(BeTrue())

			By("verifying the URL was overridden with the service URL")
			expectedURL := "http://test-card-agent.default.svc.cluster.local:8000"
			Expect(agentCard.Status.Card.URL).To(Equal(expectedURL))

			By("verifying other card data was preserved")
			Expect(agentCard.Status.Card.Name).To(Equal("Test Agent"))
			Expect(agentCard.Status.Card.Description).To(Equal("A test agent"))
			Expect(agentCard.Status.Card.Version).To(Equal("1.0.0"))
			Expect(agentCard.Status.Card.Skills).To(HaveLen(1))
			Expect(agentCard.Status.Card.Skills[0].Name).To(Equal("test-skill"))

			By("verifying the protocol was set")
			Expect(agentCard.Status.Protocol).To(Equal("a2a"))

			By("verifying the Synced condition is True")
			syncedCondition := findCondition(agentCard.Status.Conditions, "Synced")
			Expect(syncedCondition).NotTo(BeNil())
			Expect(syncedCondition.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})

var _ = Describe("AgentCard Controller - getWorkload", func() {
	const namespace = "default"

	var (
		ctx        context.Context
		reconciler *AgentCardReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &AgentCardReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	Context("When using targetRef with Deployment", func() {
		const deploymentName = "test-targetref-deployment"

		AfterEach(func() {
			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}
		})

		It("should fetch Deployment by targetRef with agent label", func() {
			By("creating a Deployment with agent labels")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
					Labels: map[string]string{
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": deploymentName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("calling getWorkload")
			workload, err := reconciler.getWorkload(ctx, &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
				Spec:       agentv1alpha1.AgentCardSpec{TargetRef: &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName}},
			})

			By("verifying the Deployment was found")
			Expect(err).NotTo(HaveOccurred())
			Expect(workload).NotTo(BeNil())
			Expect(workload.Name).To(Equal(deploymentName))
			Expect(workload.Kind).To(Equal("Deployment"))
			Expect(workload.APIVersion).To(Equal("apps/v1"))
			Expect(workload.Namespace).To(Equal(namespace))
			Expect(workload.ServiceName).To(Equal(deploymentName))
		})

		It("should detect Deployment readiness when Available condition is True", func() {
			By("creating a Deployment with agent labels")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
					Labels: map[string]string{
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": deploymentName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("updating Deployment status to Available")
			Eventually(func() error {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err != nil {
					return err
				}
				deployment.Status.Conditions = []appsv1.DeploymentCondition{
					{
						Type:   appsv1.DeploymentAvailable,
						Status: corev1.ConditionTrue,
					},
				}
				return k8sClient.Status().Update(ctx, deployment)
			}).Should(Succeed())

			By("calling getWorkload")
			workload, err := reconciler.getWorkload(ctx, &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
				Spec:       agentv1alpha1.AgentCardSpec{TargetRef: &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName}},
			})

			By("verifying readiness is detected")
			Expect(err).NotTo(HaveOccurred())
			Expect(workload.Ready).To(BeTrue())
		})
	})

	Context("When using targetRef with StatefulSet", func() {
		const statefulSetName = "test-targetref-statefulset"

		AfterEach(func() {
			statefulSet := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: statefulSetName, Namespace: namespace}, statefulSet); err == nil {
				Expect(k8sClient.Delete(ctx, statefulSet)).To(Succeed())
			}
		})

		It("should fetch StatefulSet by targetRef with agent label", func() {
			By("creating a StatefulSet with agent labels")
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      statefulSetName,
					Namespace: namespace,
					Labels: map[string]string{
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.StatefulSetSpec{
					ServiceName: statefulSetName,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": statefulSetName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": statefulSetName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, statefulSet)).To(Succeed())

			By("calling getWorkload")
			workload, err := reconciler.getWorkload(ctx, &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
				Spec:       agentv1alpha1.AgentCardSpec{TargetRef: &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: statefulSetName}},
			})

			By("verifying the StatefulSet was found")
			Expect(err).NotTo(HaveOccurred())
			Expect(workload).NotTo(BeNil())
			Expect(workload.Name).To(Equal(statefulSetName))
			Expect(workload.Kind).To(Equal("StatefulSet"))
			Expect(workload.APIVersion).To(Equal("apps/v1"))
		})

		It("should detect StatefulSet readiness when replicas match", func() {
			By("creating a StatefulSet with agent labels")
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      statefulSetName,
					Namespace: namespace,
					Labels: map[string]string{
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.StatefulSetSpec{
					ServiceName: statefulSetName,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": statefulSetName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": statefulSetName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, statefulSet)).To(Succeed())

			By("updating StatefulSet status with ready replicas")
			Eventually(func() error {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: statefulSetName, Namespace: namespace}, statefulSet); err != nil {
					return err
				}
				statefulSet.Status.Replicas = 1
				statefulSet.Status.ReadyReplicas = 1
				return k8sClient.Status().Update(ctx, statefulSet)
			}).Should(Succeed())

			By("calling getWorkload")
			workload, err := reconciler.getWorkload(ctx, &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
				Spec:       agentv1alpha1.AgentCardSpec{TargetRef: &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: statefulSetName}},
			})

			By("verifying readiness is detected")
			Expect(err).NotTo(HaveOccurred())
			Expect(workload.Ready).To(BeTrue())
		})
	})

	Context("When targetRef references non-existent workload", func() {
		It("should return ErrWorkloadNotFound for non-existent Deployment", func() {
			workload, err := reconciler.getWorkload(ctx, &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
				Spec:       agentv1alpha1.AgentCardSpec{TargetRef: &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "nonexistent-deployment"}},
			})

			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, ErrWorkloadNotFound)).To(BeTrue())
			Expect(workload).To(BeNil())
		})

		It("should return ErrWorkloadNotFound for non-existent StatefulSet", func() {
			workload, err := reconciler.getWorkload(ctx, &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
				Spec:       agentv1alpha1.AgentCardSpec{TargetRef: &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "nonexistent-statefulset"}},
			})

			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, ErrWorkloadNotFound)).To(BeTrue())
			Expect(workload).To(BeNil())
		})
	})

	Context("When targetRef references workload without agent label", func() {
		const deploymentName = "test-no-agent-label-deployment"

		AfterEach(func() {
			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}
		})

		It("should return ErrNotAgentWorkload when Deployment lacks agent label", func() {
			By("creating a Deployment without agent label")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
					Labels: map[string]string{
						"app": deploymentName,
						// Missing LabelAgentType
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": deploymentName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("calling getWorkload")
			workload, err := reconciler.getWorkload(ctx, &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
				Spec:       agentv1alpha1.AgentCardSpec{TargetRef: &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName}},
			})

			By("verifying ErrNotAgentWorkload is returned")
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, ErrNotAgentWorkload)).To(BeTrue())
			Expect(workload).To(BeNil())
		})
	})
})

var _ = Describe("AgentCard Controller - getWorkload orchestration", func() {
	const namespace = "default"

	var (
		ctx        context.Context
		reconciler *AgentCardReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &AgentCardReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	Context("When targetRef is specified (with deprecated selector also present)", func() {
		const deploymentName = "test-getworkload-deployment"

		AfterEach(func() {
			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}
		})

		It("should resolve workload via targetRef", func() {
			By("creating a Deployment with agent labels")
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
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": deploymentName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("creating an AgentCard with targetRef")
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-card-both",
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

			By("calling getWorkload")
			workload, err := reconciler.getWorkload(ctx, agentCard)

			By("verifying targetRef resolved the workload")
			Expect(err).NotTo(HaveOccurred())
			Expect(workload).NotTo(BeNil())
			Expect(workload.Name).To(Equal(deploymentName))
		})
	})

	Context("When targetRef is not specified", func() {
		It("should return error requiring targetRef", func() {
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-card-no-ref",
					Namespace: namespace,
				},
				Spec: agentv1alpha1.AgentCardSpec{
					// No TargetRef
				},
			}

			workload, err := reconciler.getWorkload(ctx, agentCard)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.targetRef is required"))
			Expect(workload).To(BeNil())
		})
	})
})

var _ = Describe("getWorkloadProtocol", func() {
	It("should return protocol from prefix label", func() {
		labels := map[string]string{
			ProtocolLabelPrefix + "a2a": "",
		}

		protocol := getWorkloadProtocol(labels)

		Expect(protocol).To(Equal("a2a"))
	})

	It("should prefer prefix labels over deprecated single-value labels", func() {
		labels := map[string]string{
			ProtocolLabelPrefix + "mcp": "",
			LabelRossoctlProtocol:       "a2a",
		}

		protocol := getWorkloadProtocol(labels)

		Expect(protocol).To(Equal("mcp"))
	})

	It("should fall back to deprecated rossoctl.io/protocol label", func() {
		labels := map[string]string{
			LabelRossoctlProtocol: "a2a",
		}

		protocol := getWorkloadProtocol(labels)

		Expect(protocol).To(Equal("a2a"))
	})

	It("should fall back to deprecated rossoctl.io/agent-protocol label", func() {
		labels := map[string]string{
			LabelAgentProtocol: "a2a",
		}

		protocol := getWorkloadProtocol(labels)

		Expect(protocol).To(Equal("a2a"))
	})

	It("should return empty string when neither label is present", func() {
		labels := map[string]string{
			"some-other-label": "value",
		}

		protocol := getWorkloadProtocol(labels)

		Expect(protocol).To(BeEmpty())
	})

	It("should return empty string when labels map is nil or empty", func() {
		Expect(getWorkloadProtocol(nil)).To(BeEmpty())
		Expect(getWorkloadProtocol(map[string]string{})).To(BeEmpty())
	})
})

var _ = Describe("getWorkloadProtocols", func() {
	It("should return all protocols from prefix labels", func() {
		labels := map[string]string{
			ProtocolLabelPrefix + "a2a": "",
			ProtocolLabelPrefix + "mcp": "",
		}

		protocols := getWorkloadProtocols(labels)

		Expect(protocols).To(ConsistOf("a2a", "mcp"))
	})

	It("should ignore prefix label with empty name", func() {
		labels := map[string]string{
			ProtocolLabelPrefix: "", // just the prefix, no protocol name
		}

		protocols := getWorkloadProtocols(labels)

		Expect(protocols).To(BeEmpty())
	})

	It("should return nil when no protocol labels are present", func() {
		labels := map[string]string{
			"some-other-label": "value",
		}

		protocols := getWorkloadProtocols(labels)

		Expect(protocols).To(BeNil())
	})

	It("should return nil for nil labels", func() {
		Expect(getWorkloadProtocols(nil)).To(BeNil())
	})

	It("should fall back to deprecated rossoctl.io/protocol label", func() {
		labels := map[string]string{
			LabelRossoctlProtocol: "a2a",
		}

		protocols := getWorkloadProtocols(labels)

		Expect(protocols).To(Equal([]string{"a2a"}))
	})

	It("should fall back to deprecated rossoctl.io/agent-protocol label", func() {
		labels := map[string]string{
			LabelAgentProtocol: "a2a",
		}

		protocols := getWorkloadProtocols(labels)

		Expect(protocols).To(Equal([]string{"a2a"}))
	})
})

var _ = Describe("hasProtocolLabels", func() {
	It("should return true for prefix labels", func() {
		labels := map[string]string{
			ProtocolLabelPrefix + "a2a": "",
		}
		Expect(hasProtocolLabels(labels)).To(BeTrue())
	})

	It("should return true for deprecated rossoctl.io/protocol", func() {
		labels := map[string]string{
			LabelRossoctlProtocol: "a2a",
		}
		Expect(hasProtocolLabels(labels)).To(BeTrue())
	})

	It("should return true for deprecated rossoctl.io/agent-protocol", func() {
		labels := map[string]string{
			LabelAgentProtocol: "a2a",
		}
		Expect(hasProtocolLabels(labels)).To(BeTrue())
	})

	It("should return false when no protocol labels present", func() {
		labels := map[string]string{
			"some-label": "value",
		}
		Expect(hasProtocolLabels(labels)).To(BeFalse())
	})

	It("should return false for nil labels", func() {
		Expect(hasProtocolLabels(nil)).To(BeFalse())
	})
})

var _ = Describe("getServicePort", func() {
	var reconciler *AgentCardReconciler

	BeforeEach(func() {
		reconciler = &AgentCardReconciler{}
	})

	It("should return the first port when multiple ports exist", func() {
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 9090, Protocol: corev1.ProtocolTCP},
					{Name: "grpc", Port: 50051, Protocol: corev1.ProtocolTCP},
				},
			},
		}
		Expect(reconciler.getServicePort(service)).To(Equal(int32(9090)))
	})

	It("should return a single port", func() {
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "single-port-svc", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Name: "api", Port: 3000, Protocol: corev1.ProtocolTCP},
				},
			},
		}
		Expect(reconciler.getServicePort(service)).To(Equal(int32(3000)))
	})

	It("should fall back to port 8000 when no ports are defined", func() {
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "no-ports-svc", Namespace: "default"},
			Spec:       corev1.ServiceSpec{},
		}
		Expect(reconciler.getServicePort(service)).To(Equal(int32(8000)))
	})
})

var _ = Describe("ignoreOperatorLabelUpdatePredicate", func() {
	pred := ignoreOperatorLabelUpdatePredicate()

	It("should allow updates where no operator annotations changed", func() {
		oldDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
				Annotations: map[string]string{
					AnnotationVerifiedStatePrefix + "card-a": "true",
				},
			},
		}
		newDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
				Annotations: map[string]string{
					AnnotationVerifiedStatePrefix + "card-a": "true",
				},
			},
		}
		result := pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy})
		Expect(result).To(BeTrue(), "should allow event when no operator annotations changed")
	})

	It("should suppress updates where a per-card annotation was added", func() {
		oldDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
			},
		}
		newDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
				Annotations: map[string]string{
					AnnotationVerifiedStatePrefix + "card-a": "true",
				},
			},
		}
		result := pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy})
		Expect(result).To(BeFalse(), "should suppress event when per-card annotation added")
	})

	It("should suppress updates where a per-card annotation value changed", func() {
		oldDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
				Annotations: map[string]string{
					AnnotationVerifiedStatePrefix + "card-a": "true",
				},
			},
		}
		newDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
				Annotations: map[string]string{
					AnnotationVerifiedStatePrefix + "card-a": "false",
				},
			},
		}
		result := pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy})
		Expect(result).To(BeFalse(), "should suppress event when per-card annotation toggled")
	})

	It("should suppress updates where a per-card annotation was removed", func() {
		oldDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
				Annotations: map[string]string{
					AnnotationVerifiedStatePrefix + "card-a": "true",
				},
			},
		}
		newDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
			},
		}
		result := pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy})
		Expect(result).To(BeFalse(), "should suppress event when per-card annotation removed")
	})

	It("should suppress updates where the legacy annotation changed", func() {
		oldDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
			},
		}
		newDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-deploy",
				Namespace:   "default",
				Annotations: map[string]string{AnnotationLastVerifiedState: "true"},
			},
		}
		result := pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy})
		Expect(result).To(BeFalse(), "should suppress event when legacy annotation changed")
	})

	It("should allow Create events", func() {
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
		}
		result := pred.Create(event.CreateEvent{Object: deploy})
		Expect(result).To(BeTrue(), "Create events should always pass through")
	})

	It("should allow Delete events", func() {
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
		}
		result := pred.Delete(event.DeleteEvent{Object: deploy})
		Expect(result).To(BeTrue(), "Delete events should always pass through")
	})
})

var _ = Describe("aggregateVerifiedState", func() {
	reconciler := &AgentCardReconciler{}

	It("should return true when all per-card annotations are true", func() {
		annotations := map[string]string{
			AnnotationVerifiedStatePrefix + "card-a": "true",
			AnnotationVerifiedStatePrefix + "card-b": "true",
			"unrelated-annotation":                   "whatever",
		}
		Expect(reconciler.aggregateVerifiedState(annotations)).To(BeTrue())
	})

	It("should return false when any per-card annotation is false", func() {
		annotations := map[string]string{
			AnnotationVerifiedStatePrefix + "card-a": "true",
			AnnotationVerifiedStatePrefix + "card-b": "false",
		}
		Expect(reconciler.aggregateVerifiedState(annotations)).To(BeFalse())
	})

	It("should return false when there are no per-card annotations", func() {
		annotations := map[string]string{
			"unrelated": "value",
		}
		Expect(reconciler.aggregateVerifiedState(annotations)).To(BeFalse())
	})

	It("should return false for nil annotations", func() {
		Expect(reconciler.aggregateVerifiedState(nil)).To(BeFalse())
	})

	It("should return true for a single card with true", func() {
		annotations := map[string]string{
			AnnotationVerifiedStatePrefix + "only-card": "true",
		}
		Expect(reconciler.aggregateVerifiedState(annotations)).To(BeTrue())
	})

	It("should return false for a single card with false", func() {
		annotations := map[string]string{
			AnnotationVerifiedStatePrefix + "only-card": "false",
		}
		Expect(reconciler.aggregateVerifiedState(annotations)).To(BeFalse())
	})

	It("should ignore the legacy annotation", func() {
		annotations := map[string]string{
			AnnotationLastVerifiedState:                 "true",
			AnnotationVerifiedStatePrefix + "only-card": "false",
		}
		Expect(reconciler.aggregateVerifiedState(annotations)).To(BeFalse())
	})
})

var _ = Describe("AgentCard Controller — deletion finalizer", func() {
	const (
		deploymentName = "del-fin-agent"
		serviceName    = "del-fin-agent"
		agentCardName  = "del-fin-card"
		namespace      = "default"
	)

	ctx := context.Background()
	nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}

	BeforeEach(func() {
		replicas := int32(1)
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
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": deploymentName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/name":    deploymentName,
							LabelAgentType:              LabelValueAgent,
							ProtocolLabelPrefix + "a2a": "",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "agent", Image: "test-image:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err != nil {
				return err
			}
			deployment.Status.Conditions = []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			}
			return k8sClient.Status().Update(ctx, deployment)
		}).Should(Succeed())

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
				Selector: map[string]string{
					"app.kubernetes.io/name": deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())

		ac := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
			Spec: agentv1alpha1.AgentCardSpec{
				SyncPeriod: "30s",
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
	})

	AfterEach(func() {
		ac := &agentv1alpha1.AgentCard{}
		if err := k8sClient.Get(ctx, nn, ac); err == nil {
			ac.Finalizers = nil
			_ = k8sClient.Update(ctx, ac)
			_ = k8sClient.Delete(ctx, ac)
		}
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, dep); err == nil {
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		}
		svc := &corev1.Service{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: namespace}, svc); err == nil {
			Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
		}
	})

	It("should run finalizer, clean per-card state, and allow deletion", func() {
		reconciler := &AgentCardReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			AgentFetcher: &mockFetcher{
				cardData: &agentv1alpha1.AgentCardData{
					Name: "Fin", Version: "1.0.0", URL: "http://0.0.0.0:8000",
				},
			},
		}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		perCardKey := AnnotationVerifiedStatePrefix + agentCardName
		depNN := types.NamespacedName{Name: deploymentName, Namespace: namespace}
		Eventually(func() error {
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, depNN, dep); err != nil {
				return err
			}
			if dep.Annotations == nil {
				dep.Annotations = map[string]string{}
			}
			dep.Annotations[perCardKey] = "true"
			if dep.Spec.Template.Labels == nil {
				dep.Spec.Template.Labels = map[string]string{}
			}
			dep.Spec.Template.Labels[LabelSignatureVerified] = "true"
			return k8sClient.Update(ctx, dep)
		}).Should(Succeed())

		Expect(k8sClient.Delete(ctx, &agentv1alpha1.AgentCard{ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace}})).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depAfter := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, depNN, depAfter)).To(Succeed())
		Expect(depAfter.Annotations).NotTo(HaveKey(perCardKey))
		Expect(depAfter.Spec.Template.Labels).NotTo(HaveKey(LabelSignatureVerified))

		ac := &agentv1alpha1.AgentCard{}
		err = k8sClient.Get(ctx, nn, ac)
		Expect(err).To(HaveOccurred(), "AgentCard should be removed after finalizer clears")
	})
})

var _ = Describe("AgentCard Controller — ConfigMap-backed fetch", func() {
	const (
		deploymentName = "cm-fetch-agent"
		serviceName    = "cm-fetch-agent"
		agentCardName  = "cm-fetch-card"
		namespace      = "default"
	)

	ctx := context.Background()
	nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}

	BeforeEach(func() {
		replicas := int32(1)
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
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": deploymentName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/name":    deploymentName,
							LabelAgentType:              LabelValueAgent,
							ProtocolLabelPrefix + "a2a": "",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "agent", Image: "test-image:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err != nil {
				return err
			}
			deployment.Status.Conditions = []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			}
			return k8sClient.Status().Update(ctx, deployment)
		}).Should(Succeed())

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
				Selector: map[string]string{
					"app.kubernetes.io/name": deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())

		card := agentv1alpha1.AgentCardData{
			Name: "From ConfigMap", Version: "2.0.0", URL: "http://signed.local:8000",
		}
		raw, err := json.Marshal(card)
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName + agentcard.SignedCardConfigMapSuffix,
				Namespace: namespace,
			},
			Data: map[string]string{agentcard.SignedCardLegacyConfigMapKey: string(raw)},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		ac := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
			Spec: agentv1alpha1.AgentCardSpec{
				SyncPeriod: "30s",
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
	})

	AfterEach(func() {
		cm := &corev1.ConfigMap{}
		cmNN := types.NamespacedName{Name: deploymentName + agentcard.SignedCardConfigMapSuffix, Namespace: namespace}
		if err := k8sClient.Get(ctx, cmNN, cm); err == nil {
			_ = k8sClient.Delete(ctx, cm)
		}
		ac := &agentv1alpha1.AgentCard{}
		if err := k8sClient.Get(ctx, nn, ac); err == nil {
			ac.Finalizers = nil
			_ = k8sClient.Update(ctx, ac)
			_ = k8sClient.Delete(ctx, ac)
		}
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, dep); err == nil {
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		}
		svc := &corev1.Service{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: namespace}, svc); err == nil {
			Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
		}
	})

	It("should prefer signed card data from ConfigMap in the reconcile loop", func() {
		reconciler := &AgentCardReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			AgentFetcher: agentcard.NewConfigMapFetcher(k8sClient),
		}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		updated := &agentv1alpha1.AgentCard{}
		Expect(k8sClient.Get(ctx, nn, updated)).To(Succeed())
		Expect(updated.Status.Card).NotTo(BeNil())
		// Card body comes from ConfigMap; reconciler rewrites URL to the in-cluster Service URL.
		Expect(updated.Status.Card.Name).To(Equal("From ConfigMap"))
		Expect(updated.Status.Card.URL).To(Equal(agentcard.GetServiceURL(serviceName, namespace, 8000)))
	})
})

var _ = Describe("AgentCard Controller — protocol label edge cases", func() {
	const namespace = "default"
	ctx := context.Background()

	It("should fetch using one of multiple protocol labels", func() {
		deploymentName := "multi-proto-agent"
		serviceName := deploymentName
		agentCardName := "multi-proto-card"
		replicas := int32(1)

		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":    deploymentName,
					LabelAgentType:              LabelValueAgent,
					ProtocolLabelPrefix + "a2a": "",
					ProtocolLabelPrefix + "mcp": "",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": deploymentName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/name":    deploymentName,
							LabelAgentType:              LabelValueAgent,
							ProtocolLabelPrefix + "a2a": "",
							ProtocolLabelPrefix + "mcp": "",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "agent", Image: "test-image:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, deployment) }()
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err != nil {
				return err
			}
			deployment.Status.Conditions = []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			}
			return k8sClient.Status().Update(ctx, deployment)
		}).Should(Succeed())

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace},
			Spec: corev1.ServiceSpec{
				Ports:    []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
				Selector: map[string]string{"app.kubernetes.io/name": deploymentName},
			},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, service) }()

		capture := &capturingProtocolFetcher{}
		reconciler := &AgentCardReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			AgentFetcher: capture,
		}

		ac := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
			Spec: agentv1alpha1.AgentCardSpec{
				SyncPeriod: "30s",
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		defer func() {
			got := &agentv1alpha1.AgentCard{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, got); err == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got)
			}
		}()

		nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		p := capture.lastProtocol()
		Expect([]string{"a2a", "mcp"}).To(ContainElement(p))
	})

	It("should surface NoProtocol when workload has no protocol labels", func() {
		deploymentName := "no-proto-agent"
		serviceName := deploymentName
		agentCardName := "no-proto-card"
		replicas := int32(1)

		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name": deploymentName,
					LabelAgentType:           LabelValueAgent,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": deploymentName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/name": deploymentName,
							LabelAgentType:           LabelValueAgent,
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "agent", Image: "test-image:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, deployment) }()
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err != nil {
				return err
			}
			deployment.Status.Conditions = []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			}
			return k8sClient.Status().Update(ctx, deployment)
		}).Should(Succeed())

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace},
			Spec: corev1.ServiceSpec{
				Ports:    []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
				Selector: map[string]string{"app.kubernetes.io/name": deploymentName},
			},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, service) }()

		reconciler := &AgentCardReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			AgentFetcher: &mockFetcher{cardData: &agentv1alpha1.AgentCardData{Name: "x", Version: "1", URL: "http://x"}},
		}

		ac := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
			Spec: agentv1alpha1.AgentCardSpec{
				SyncPeriod: "30s",
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, ac)).To(Succeed())
		defer func() {
			got := &agentv1alpha1.AgentCard{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, got); err == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got)
			}
		}()

		nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		updated := &agentv1alpha1.AgentCard{}
		Expect(k8sClient.Get(ctx, nn, updated)).To(Succeed())
		synced := findCondition(updated.Status.Conditions, "Synced")
		Expect(synced).NotTo(BeNil())
		Expect(synced.Status).To(Equal(metav1.ConditionFalse))
		Expect(synced.Reason).To(Equal("NoProtocol"))
	})
})

var _ = Describe("AgentCard Controller - getSyncPeriod", func() {
	const (
		deploymentName = "syncperiod-test-agent"
		serviceName    = "syncperiod-test-agent"
		namespace      = "default"
	)

	ctx := context.Background()

	var reconciler *AgentCardReconciler

	BeforeEach(func() {
		By("creating a Deployment with agent labels")
		replicas := int32(1)
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
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/name": deploymentName,
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/name":    deploymentName,
							LabelAgentType:              LabelValueAgent,
							ProtocolLabelPrefix + "a2a": "",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "agent", Image: "test-image:latest"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		By("setting Deployment status to Available")
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err != nil {
				return err
			}
			deployment.Status.Conditions = []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			}
			return k8sClient.Status().Update(ctx, deployment)
		}).Should(Succeed())

		By("creating a Service for the Deployment")
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: namespace,
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP},
				},
				Selector: map[string]string{
					"app.kubernetes.io/name": deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())

		reconciler = &AgentCardReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			AgentFetcher: &mockFetcher{
				cardData: &agentv1alpha1.AgentCardData{
					Name:    "Test Agent",
					Version: "1.0.0",
					URL:     "http://0.0.0.0:8000",
				},
			},
		}
	})

	AfterEach(func() {
		By("cleaning up resources")
		for _, name := range []string{
			"syncperiod-10s", "syncperiod-5m", "syncperiod-garbage",
			"syncperiod-empty", "syncperiod-update",
		} {
			ac := &agentv1alpha1.AgentCard{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ac); err == nil {
				_ = k8sClient.Delete(ctx, ac)
			}
		}

		deployment := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, deployment); err == nil {
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		}

		service := &corev1.Service{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: namespace}, service); err == nil {
			Expect(k8sClient.Delete(ctx, service)).To(Succeed())
		}
	})

	reconcileCard := func(agentCardName, syncPeriod string) reconcile.Result {
		agentCard := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{
				Name:      agentCardName,
				Namespace: namespace,
			},
			Spec: agentv1alpha1.AgentCardSpec{
				SyncPeriod: syncPeriod,
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deploymentName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

		nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}

		By("reconciling to add finalizer")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		By("reconciling to fetch agent card and apply sync period")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	Context("with valid durations", func() {
		It("should requeue after 10s for syncPeriod '10s'", func() {
			result := reconcileCard("syncperiod-10s", "10s")
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))
		})

		It("should requeue after 5m for syncPeriod '5m'", func() {
			result := reconcileCard("syncperiod-5m", "5m")
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
		})
	})

	Context("with invalid or empty durations", func() {
		It("should fall back to 30s default for invalid syncPeriod 'garbage'", func() {
			result := reconcileCard("syncperiod-garbage", "garbage")
			Expect(result.RequeueAfter).To(Equal(DefaultSyncPeriod))
		})

		It("should fall back to 30s default for empty syncPeriod", func() {
			result := reconcileCard("syncperiod-empty", "")
			Expect(result.RequeueAfter).To(Equal(DefaultSyncPeriod))
		})
	})

	Context("when syncPeriod is updated on a live AgentCard", func() {
		It("should apply the new period on next reconciliation", func() {
			agentCardName := "syncperiod-update"
			nn := types.NamespacedName{Name: agentCardName, Namespace: namespace}

			By("creating AgentCard with syncPeriod '30s' and reconciling")
			result := reconcileCard(agentCardName, "30s")
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("patching syncPeriod to '10s'")
			agentCard := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, nn, agentCard)).To(Succeed())
			agentCard.Spec.SyncPeriod = "10s"
			Expect(k8sClient.Update(ctx, agentCard)).To(Succeed())

			By("reconciling again to pick up the new period")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))
		})
	})

	Context("Deprecation warning for new AgentCards", func() {
		const (
			deprecationNS     = "default"
			deprecationDeploy = "deprecation-deploy"
			deprecationCard   = "deprecation-card"
		)

		ctx := context.Background()

		It("should emit deprecation event for recently created AgentCard", func() {
			replicas := int32(1)
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deprecationDeploy,
					Namespace: deprecationNS,
					Labels: map[string]string{
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deprecationDeploy},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app":                       deprecationDeploy,
								LabelAgentType:              LabelValueAgent,
								ProtocolLabelPrefix + "a2a": "",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "test:latest"}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			Eventually(func() error {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deprecationDeploy, Namespace: deprecationNS}, dep); err != nil {
					return err
				}
				dep.Status.Conditions = []appsv1.DeploymentCondition{
					{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
				}
				return k8sClient.Status().Update(ctx, dep)
			}).Should(Succeed())

			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: deprecationDeploy, Namespace: deprecationNS},
				Spec: corev1.ServiceSpec{
					Ports:    []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
					Selector: map[string]string{"app": deprecationDeploy},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()

			ac := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: deprecationCard, Namespace: deprecationNS},
				Spec: agentv1alpha1.AgentCardSpec{
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deprecationDeploy,
					},
				},
			}
			Expect(k8sClient.Create(ctx, ac)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, ac) }()

			fetcher := &mockFetcher{
				cardData: &agentv1alpha1.AgentCardData{
					Name:    "deprecation-agent",
					Version: "1.0.0",
					URL:     "http://deprecation-deploy.default.svc.cluster.local:8000",
				},
			}
			fakeRecorder := events.NewFakeRecorder(10)
			reconciler := &AgentCardReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				AgentFetcher: fetcher,
				Recorder:     fakeRecorder,
			}

			nn := types.NamespacedName{Name: deprecationCard, Namespace: deprecationNS}
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			var foundDeprecation bool
			for len(fakeRecorder.Events) > 0 {
				evt := <-fakeRecorder.Events
				if strings.Contains(evt, "Deprecated") {
					foundDeprecation = true
					break
				}
			}
			Expect(foundDeprecation).To(BeTrue(), "expected a Deprecated event to be emitted")
		})
	})
})

// Helper function to find a condition by type
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
