/*
Copyright 2025-2026.

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

package v1alpha1

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo usage
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega usage
	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/webhook/injector"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var testNsCounter int

// createAgentRuntime creates an AgentRuntime CR in the given namespace targeting
// the given workload name. The webhook requires a matching AgentRuntime to exist.
func createAgentRuntime(namespace, targetName string) {
	createAgentRuntimeWithMode(namespace, targetName, "")
}

// createAgentRuntimeWithMode is the same as createAgentRuntime but pins
// the per-workload AuthBridgeMode field. Pass an empty string to leave
// mode resolution to the namespace ConfigMap / cluster default.
func createAgentRuntimeWithMode(namespace, targetName, mode string) {
	ar := &agentv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetName + "-runtime",
			Namespace: namespace,
		},
		Spec: agentv1alpha1.AgentRuntimeSpec{
			Type: agentv1alpha1.RuntimeTypeAgent,
			TargetRef: agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       targetName,
			},
			AuthBridgeMode: mode,
		},
	}
	err := k8sClient.Create(ctx, ar)
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("AuthBridge Pod Webhook", func() {
	var testNamespace string

	BeforeEach(func() {
		// Create a unique namespace with rossoctl-enabled=true for each test
		testNsCounter++
		testNamespace = fmt.Sprintf("test-webhook-%d", testNsCounter)

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNamespace,
				Labels: map[string]string{
					"rossoctl-enabled": "true",
				},
			},
		}
		err := k8sClient.Create(ctx, ns)
		Expect(err).NotTo(HaveOccurred())
	})

	newTestPod := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: name + "-",
				Namespace:    testNamespace,
				Labels:       labels,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "app",
						Image: "busybox:latest",
					},
				},
			},
		}
	}

	Context("when a Pod has rossoctl.io/type=agent and rossoctl.io/inject=enabled", func() {
		It("should inject sidecars", func() {
			// Namespace ConfigMap pins envoy-sidecar mode so this test exercises
			// the envoy-proxy + proxy-init injection path.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      injector.AuthBridgeRuntimeConfigMapName,
					Namespace: testNamespace,
				},
				Data: map[string]string{
					"config.yaml": "mode: " + injector.ModeEnvoySidecar + "\n",
				},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			pod := newTestPod("agent-pod", map[string]string{
				"rossoctl.io/type":   "agent",
				"rossoctl.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch from the API server to get server-side mutations
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			// Verify sidecars were injected
			Expect(containerNames(pod.Spec.Containers)).To(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).To(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod has rossoctl.io/type=tool and rossoctl.io/inject=enabled", func() {
		It("should not inject sidecars (injectTools feature gate is disabled by default)", func() {
			pod := newTestPod("tool-pod", map[string]string{
				"rossoctl.io/type":   "tool",
				"rossoctl.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).NotTo(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod does not have rossoctl.io/type label", func() {
		It("should not inject sidecars", func() {
			pod := newTestPod("no-type-pod", map[string]string{
				"rossoctl.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).NotTo(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod has rossoctl.io/inject=disabled", func() {
		It("should not inject sidecars", func() {
			pod := newTestPod("disabled-pod", map[string]string{
				"rossoctl.io/type":   "agent",
				"rossoctl.io/inject": "disabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).NotTo(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod has rossoctl.io/type=agent but no AgentRuntime CR", func() {
		It("should inject sidecars with defaults-only config", func() {
			pod := newTestPod("no-runtime-pod", map[string]string{
				"rossoctl.io/type":   "agent",
				"rossoctl.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			// Default mode is proxy-sidecar — expect authbridge-proxy and the
			// always-on enforce-redirect proxy-init guard; no envoy-proxy.
			Expect(containerNames(pod.Spec.Containers)).To(ContainElement(injector.AuthBridgeProxyContainerName))
			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).To(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod already has injected containers (idempotency)", func() {
		It("should not double-inject", func() {
			// AgentRuntime CR must exist
			createAgentRuntime(testNamespace, "already-injected-pod")

			pod := newTestPod("already-injected-pod", map[string]string{
				"rossoctl.io/type":   "agent",
				"rossoctl.io/inject": "enabled",
			})
			// Pre-add the envoy-proxy container to simulate prior injection
			pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
				Name:  injector.EnvoyProxyContainerName,
				Image: "envoy:test",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			// Count envoy-proxy containers — should be exactly 1 (the pre-existing one)
			count := 0
			for _, c := range pod.Spec.Containers {
				if c.Name == injector.EnvoyProxyContainerName {
					count++
				}
			}
			Expect(count).To(Equal(1))
		})
	})

	// Pre-population of the Keycloak client-credentials annotation ensures that the first pod
	// created for an agent workload mounts the operator-produced Secret without having to wait
	// for the ClientRegistration controller to patch the workload's pod template and trigger a
	// pod recreate. Kubelet resolves the Secret lazily (Optional=false), so the pod simply sits
	// in ContainerCreating until the controller creates the Secret.
	Context("operator-managed Keycloak credentials: annotation pre-population", func() {
		It("sets the annotation for an agent workload missing it", func() {
			createAgentRuntime(testNamespace, "prepop-agent")

			pod := newTestPod("prepop-agent", map[string]string{
				"rossoctl.io/type":   "agent",
				"rossoctl.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(pod.Annotations).To(HaveKey(injector.AnnotationKeycloakClientSecretName))
			Expect(pod.Annotations[injector.AnnotationKeycloakClientSecretName]).
				To(HavePrefix("rossoctl-keycloak-client-credentials-"))

			// The webhook should also have declared the Secret volume (lazy-resolved by kubelet).
			found := false
			for _, v := range pod.Spec.Volumes {
				if v.Secret != nil && v.Secret.SecretName == pod.Annotations[injector.AnnotationKeycloakClientSecretName] {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "expected a Secret volume referencing the pre-populated credentials secret")
		})

		It("does not overwrite an existing annotation", func() {
			createAgentRuntime(testNamespace, "prepop-existing")

			const preset = "rossoctl-keycloak-client-credentials-deadbeef"
			pod := newTestPod("prepop-existing", map[string]string{
				"rossoctl.io/type":   "agent",
				"rossoctl.io/inject": "enabled",
			})
			pod.Annotations = map[string]string{
				injector.AnnotationKeycloakClientSecretName: preset,
			}

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(pod.Annotations[injector.AnnotationKeycloakClientSecretName]).To(Equal(preset))
		})

		It("skips tool workloads when the injectTools gate is disabled", func() {
			pod := newTestPod("prepop-tool", map[string]string{
				"rossoctl.io/type":   "tool",
				"rossoctl.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			// No injection happens at all when injectTools is off, so the annotation should not be set.
			Expect(pod.Annotations).NotTo(HaveKey(injector.AnnotationKeycloakClientSecretName))
		})

	})
})

var _ = Describe("deriveWorkloadName", func() {
	DescribeTable("should extract the correct workload name",
		func(generateName, name, uid, expected string) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: generateName,
					Name:         name,
					UID:          types.UID(uid),
				},
			}
			Expect(deriveWorkloadName(pod)).To(Equal(expected))
		},
		Entry("Deployment Pod (GenerateName with ReplicaSet hash)",
			"myapp-7d4f8b9c5-", "", "", "myapp-7d4f8b9c5"),
		Entry("StatefulSet Pod (GenerateName without hash)",
			"myapp-", "", "", "myapp"),
		Entry("Bare Pod with Name only",
			"", "my-bare-pod", "", "my-bare-pod"),
		Entry("Pod with both GenerateName and Name (GenerateName wins)",
			"myapp-abc12-", "myapp-abc12-xyz", "", "myapp-abc12"),
		Entry("GenerateName with no trailing hyphen",
			"myapp", "", "", "myapp"),
		Entry("Both empty — falls back to UID",
			"", "", "abc-123-uid", "abc-123-uid"),
	)
})

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for i, c := range containers {
		names[i] = c.Name
	}
	return names
}

func initContainerNames(containers []corev1.Container) []string {
	return containerNames(containers)
}
