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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

var _ = Describe("mapWorkloadToAgentCards", func() {
	const namespace = "default"

	ctx := context.Background()
	logger := log.Log.WithName("indexers-test")

	Context("when the workload does not have agent labels", func() {
		It("should return no reconcile requests", func() {
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-agent-deploy",
					Namespace: namespace,
					Labels: map[string]string{
						"app": "something-else",
					},
				},
			}

			mapFn := mapWorkloadToAgentCards(k8sClient, "apps/v1", "Deployment", logger)
			requests := mapFn(ctx, deploy)
			Expect(requests).To(BeEmpty())
		})
	})

	Context("when the workload has nil labels", func() {
		It("should return no reconcile requests", func() {
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-labels-deploy",
					Namespace: namespace,
				},
			}

			mapFn := mapWorkloadToAgentCards(k8sClient, "apps/v1", "Deployment", logger)
			requests := mapFn(ctx, deploy)
			Expect(requests).To(BeEmpty())
		})
	})

	Context("when the workload has agent label with wrong value", func() {
		It("should return no reconcile requests", func() {
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wrong-label-deploy",
					Namespace: namespace,
					Labels: map[string]string{
						LabelAgentType: "not-agent",
					},
				},
			}

			mapFn := mapWorkloadToAgentCards(k8sClient, "apps/v1", "Deployment", logger)
			requests := mapFn(ctx, deploy)
			Expect(requests).To(BeEmpty())
		})
	})

	Context("when a Sandbox workload has agent labels and matching AgentCard exists", func() {
		It("should return a reconcile request for the AgentCard", func() {
			card := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-card",
					Namespace: namespace,
				},
				Spec: agentv1alpha1.AgentCardSpec{
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "agents.x-k8s.io/v1alpha1",
						Kind:       "Sandbox",
						Name:       "my-sandbox",
					},
				},
			}
			Expect(k8sClient.Create(ctx, card)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, card) })

			sbx := &unstructured.Unstructured{}
			sbx.SetGroupVersionKind(sandboxGVK)
			sbx.SetName("my-sandbox")
			sbx.SetNamespace(namespace)
			sbx.SetLabels(map[string]string{LabelAgentType: LabelValueAgent})

			mapFn := mapWorkloadToAgentCards(indexedClient, "agents.x-k8s.io/v1alpha1", "Sandbox", logger)
			Eventually(func() int {
				return len(mapFn(ctx, sbx))
			}).Should(Equal(1))
			requests := mapFn(ctx, sbx)
			Expect(requests[0].Name).To(Equal("sandbox-card"))
		})
	})

	Context("when a Sandbox workload lacks agent labels", func() {
		It("should return no reconcile requests", func() {
			sbx := &unstructured.Unstructured{}
			sbx.SetGroupVersionKind(sandboxGVK)
			sbx.SetName("unlabeled-sandbox")
			sbx.SetNamespace(namespace)
			sbx.SetLabels(map[string]string{"app": "something"})

			mapFn := mapWorkloadToAgentCards(k8sClient, "agents.x-k8s.io/v1alpha1", "Sandbox", logger)
			requests := mapFn(ctx, sbx)
			Expect(requests).To(BeEmpty())
		})
	})
})
