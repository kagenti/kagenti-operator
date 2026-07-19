/*
Copyright 2026.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

// createClusterDefaults creates the rossoctl-webhook-defaults ConfigMap in envtest.
func createClusterDefaults(ctx context.Context, data map[string]string) *corev1.ConfigMap {
	// Ensure the namespace exists
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ClusterDefaultsNamespace}}
	_ = k8sClient.Create(ctx, ns)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClusterDefaultsConfigMapName,
			Namespace: ClusterDefaultsNamespace,
		},
		Data: data,
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, cm)).To(Succeed())
	return cm
}

// createClusterFeatureGates creates the rossoctl-webhook-feature-gates ConfigMap in envtest.
func createClusterFeatureGates(ctx context.Context, data map[string]string) *corev1.ConfigMap {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ClusterDefaultsNamespace}}
	_ = k8sClient.Create(ctx, ns)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClusterFeatureGatesConfigMapName,
			Namespace: ClusterDefaultsNamespace,
		},
		Data: data,
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, cm)).To(Succeed())
	return cm
}

// createNamespaceDefaults creates a namespace-level defaults ConfigMap in envtest.
//
//nolint:unparam // namespace is parameterized for reuse across test contexts
func createNamespaceDefaults(ctx context.Context, name, namespace string, data map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelNamespaceDefaults: "true",
			},
		},
		Data: data,
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, cm)).To(Succeed())
	return cm
}

func newTestDeployment(name, ns string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "test:latest"}}},
			},
		},
	}
}

var _ = Describe("AgentRuntime Config", func() {
	const namespace = "default"
	ctx := context.Background()

	Context("ComputeConfigHash", func() {
		It("should be deterministic", func() {
			result1, err := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(err).NotTo(HaveOccurred())

			result2, err := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(err).NotTo(HaveOccurred())

			Expect(result1.Hash).To(Equal(result2.Hash))
		})

		It("should NOT change when spec type changes", func() {
			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).To(Equal(r2.Hash))
		})

		It("should NOT change when CR-level fields change", func() {
			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).To(Equal(r2.Hash))
		})

		It("should NOT change when MTLSMode changes", func() {
			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).To(Equal(r2.Hash))
		})

		It("should NOT change when AuthBridgeMode changes", func() {
			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).To(Equal(r2.Hash))
		})

		It("should produce a non-empty hash even with missing ConfigMaps", func() {
			result, err := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Hash).NotTo(BeEmpty())
		})

		It("should change when cluster defaults change", func() {
			cm := createClusterDefaults(ctx, map[string]string{"otel-endpoint": "collector-v1:4317"})
			defer func() { _ = k8sClient.Delete(ctx, cm) }()

			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)

			cm.Data["otel-endpoint"] = "collector-v2:4317"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).NotTo(Equal(r2.Hash))
		})

		It("should change when feature gates change", func() {
			fg := createClusterFeatureGates(ctx, map[string]string{"globalEnabled": "true"})
			defer func() { _ = k8sClient.Delete(ctx, fg) }()

			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)

			fg.Data["globalEnabled"] = "false"
			Expect(k8sClient.Update(ctx, fg)).To(Succeed())

			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).NotTo(Equal(r2.Hash))
		})

		It("should change when injectTools (Auth Bridge tool injection gate) changes", func() {
			fg := createClusterFeatureGates(ctx, map[string]string{
				"globalEnabled": "true",
				"injectTools":   "false",
			})
			defer func() { _ = k8sClient.Delete(ctx, fg) }()

			r1, err := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(err).NotTo(HaveOccurred())

			fg.Data["injectTools"] = "true"
			Expect(k8sClient.Update(ctx, fg)).To(Succeed())

			r2, err := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.Hash).NotTo(Equal(r2.Hash))
		})

		It("should change when namespace defaults change", func() {
			nsCM := createNamespaceDefaults(ctx, "ns-defaults-hash", namespace, map[string]string{"sampling-rate": "0.1"})
			defer func() { _ = k8sClient.Delete(ctx, nsCM) }()

			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)

			nsCM.Data["sampling-rate"] = "1.0"
			Expect(k8sClient.Update(ctx, nsCM)).To(Succeed())

			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).NotTo(Equal(r2.Hash))
		})

		It("should change when authbridge-runtime-config edits", func() {
			abCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      AuthBridgeRuntimeConfigMapName,
					Namespace: namespace,
				},
				Data: map[string]string{
					"config.yaml": "mode: proxy-sidecar\n",
				},
			}
			Expect(k8sClient.Create(ctx, abCM)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, abCM) }()

			r1, _ := ComputeConfigHash(ctx, k8sClient, namespace)

			abCM.Data["config.yaml"] = "mode: proxy-sidecar\nmtls:\n  mode: strict\n"
			Expect(k8sClient.Update(ctx, abCM)).To(Succeed())

			r2, _ := ComputeConfigHash(ctx, k8sClient, namespace)
			Expect(r1.Hash).NotTo(Equal(r2.Hash))
		})

		It("should change when spiffe-helper-config content changes", func() {
			const shHashNS = "sh-hash-ns"
			shNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: shHashNS}}
			_ = k8sClient.Create(ctx, shNS)

			shCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spiffe-helper-config",
					Namespace: shHashNS,
				},
				Data: map[string]string{
					"helper.conf": "agent_address = \"/old/socket\"",
				},
			}
			Expect(k8sClient.Create(ctx, shCM)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, shCM) }()

			r1, _ := ComputeConfigHash(ctx, k8sClient, shHashNS)

			shCM.Data["helper.conf"] = "agent_address = \"/new/socket\""
			Expect(k8sClient.Update(ctx, shCM)).To(Succeed())

			r2, _ := ComputeConfigHash(ctx, k8sClient, shHashNS)
			Expect(r1.Hash).NotTo(Equal(r2.Hash))
		})
	})

	Context("resolveConfig two-layer merge", func() {
		It("should merge cluster → namespace with correct precedence", func() {
			clusterCM := createClusterDefaults(ctx, map[string]string{
				"otel-endpoint":       "cluster-collector:4317",
				"spiffe-trust-domain": "cluster.local",
				"cluster-only-key":    "cluster-value",
			})
			defer func() { _ = k8sClient.Delete(ctx, clusterCM) }()

			fgCM := createClusterFeatureGates(ctx, map[string]string{
				"globalEnabled": "true",
				"envoyProxy":    "true",
			})
			defer func() { _ = k8sClient.Delete(ctx, fgCM) }()

			nsCM := createNamespaceDefaults(ctx, "ns-defaults-merge", namespace, map[string]string{
				"otel-endpoint": "ns-collector:4317",
				"ns-only-key":   "ns-value",
			})
			defer func() { _ = k8sClient.Delete(ctx, nsCM) }()

			resolved, _ := resolveConfig(ctx, k8sClient, namespace)

			// Namespace overrides cluster
			Expect(resolved.Defaults["otel-endpoint"]).To(Equal("ns-collector:4317"))

			// Cluster values preserved when not overridden
			Expect(resolved.Defaults["cluster-only-key"]).To(Equal("cluster-value"))

			// Namespace-only values present
			Expect(resolved.Defaults["ns-only-key"]).To(Equal("ns-value"))

			// Feature gates
			Expect(resolved.FeatureGates["globalEnabled"]).To(Equal("true"))
		})

		It("should not include CR fields in resolved config", func() {
			clusterCM := createClusterDefaults(ctx, map[string]string{
				"otel-endpoint": "cluster-collector:4317",
			})
			defer func() { _ = k8sClient.Delete(ctx, clusterCM) }()

			resolved, _ := resolveConfig(ctx, k8sClient, namespace)

			// ConfigMap value untouched in Defaults
			Expect(resolved.Defaults["otel-endpoint"]).To(Equal("cluster-collector:4317"))
		})
	})

	Context("readNamespaceDefaults", func() {
		It("should handle multiple ConfigMaps by using the first one and returning a warning", func() {
			cm1 := createNamespaceDefaults(ctx, "multi-defaults-1", namespace, map[string]string{"key": "from-first"})
			defer func() { _ = k8sClient.Delete(ctx, cm1) }()

			cm2 := createNamespaceDefaults(ctx, "multi-defaults-2", namespace, map[string]string{"key": "from-second"})
			defer func() { _ = k8sClient.Delete(ctx, cm2) }()

			data, warning := readNamespaceDefaults(ctx, k8sClient, namespace)
			Expect(data["key"]).NotTo(BeEmpty())
			Expect(warning).To(ContainSubstring("multiple namespace defaults ConfigMaps found"))
			Expect(warning).To(ContainSubstring("multi-defaults-"))
		})

		It("should return no warning when exactly one ConfigMap exists", func() {
			cm := createNamespaceDefaults(ctx, "single-defaults", namespace, map[string]string{"key": "value"})
			defer func() { _ = k8sClient.Delete(ctx, cm) }()

			data, warning := readNamespaceDefaults(ctx, k8sClient, namespace)
			Expect(data["key"]).To(Equal("value"))
			Expect(warning).To(BeEmpty())
		})

		It("should return no warning when no ConfigMap exists", func() {
			data, warning := readNamespaceDefaults(ctx, k8sClient, "nonexistent-ns")
			Expect(data).To(BeEmpty())
			Expect(warning).To(BeEmpty())
		})
	})

	Context("ConfigResolved condition with warnings", func() {
		It("should surface a ConfigWarning condition when multiple namespace defaults exist", func() {
			dep := newTestDeployment("warn-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			rt := &agentv1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: "warn-rt", Namespace: namespace},
				Spec: agentv1alpha1.AgentRuntimeSpec{
					Type:      agentv1alpha1.RuntimeTypeAgent,
					TargetRef: agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "warn-deploy"},
				},
			}
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			cm1 := createNamespaceDefaults(ctx, "warn-defaults-1", namespace, map[string]string{"k": "v1"})
			defer func() { _ = k8sClient.Delete(ctx, cm1) }()
			cm2 := createNamespaceDefaults(ctx, "warn-defaults-2", namespace, map[string]string{"k": "v2"})
			defer func() { _ = k8sClient.Delete(ctx, cm2) }()

			r := &AgentRuntimeReconciler{Client: k8sClient, Scheme: scheme.Scheme}
			nn := types.NamespacedName{Name: "warn-rt", Namespace: namespace}

			// Reconcile: finalizer
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			// Reconcile: apply config
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			updated := &agentv1alpha1.AgentRuntime{}
			Expect(k8sClient.Get(ctx, nn, updated)).To(Succeed())

			// Should have ConfigResolved condition with warning
			var configCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionTypeConfigResolved {
					configCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(configCond).NotTo(BeNil())
			Expect(configCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(configCond.Reason).To(Equal("ConfigWarning"))
			Expect(configCond.Message).To(ContainSubstring("multiple namespace defaults ConfigMaps found"))
		})

		It("should set ConfigResolved without warning when config is clean", func() {
			dep := newTestDeployment("clean-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			rt := &agentv1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: "clean-rt", Namespace: namespace},
				Spec: agentv1alpha1.AgentRuntimeSpec{
					Type:      agentv1alpha1.RuntimeTypeAgent,
					TargetRef: agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "clean-deploy"},
				},
			}
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := &AgentRuntimeReconciler{Client: k8sClient, Scheme: scheme.Scheme}
			nn := types.NamespacedName{Name: "clean-rt", Namespace: namespace}

			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			updated := &agentv1alpha1.AgentRuntime{}
			Expect(k8sClient.Get(ctx, nn, updated)).To(Succeed())

			var configCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionTypeConfigResolved {
					configCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(configCond).NotTo(BeNil())
			Expect(configCond.Reason).To(Equal("ConfigResolved"))
			Expect(configCond.Message).To(Equal("Configuration resolved successfully"))
		})
	})

	Context("Mapper functions", func() {
		It("should map cluster ConfigMap to all AgentRuntimes", func() {
			rt := &agentv1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: "mapper-cluster-rt", Namespace: namespace},
				Spec: agentv1alpha1.AgentRuntimeSpec{
					Type:      agentv1alpha1.RuntimeTypeAgent,
					TargetRef: agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "mapper-deploy"},
				},
			}
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := &AgentRuntimeReconciler{Client: k8sClient, Scheme: scheme.Scheme}

			// Should match cluster defaults ConfigMap
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ClusterDefaultsConfigMapName,
					Namespace: ClusterDefaultsNamespace,
				},
			}
			requests := r.mapClusterConfigMapToAgentRuntimes(ctx, cm)
			Expect(requests).NotTo(BeEmpty())

			found := false
			for _, req := range requests {
				if req.Name == "mapper-cluster-rt" {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "expected mapper-cluster-rt in requests")

			// Should not match random ConfigMap
			randomCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "random", Namespace: ClusterDefaultsNamespace},
			}
			Expect(r.mapClusterConfigMapToAgentRuntimes(ctx, randomCM)).To(BeNil())

			// Should not match ConfigMap in wrong namespace
			wrongNsCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: ClusterDefaultsConfigMapName, Namespace: "other"},
			}
			Expect(r.mapClusterConfigMapToAgentRuntimes(ctx, wrongNsCM)).To(BeNil())
		})

		It("should map namespace defaults ConfigMap to same-namespace AgentRuntimes only", func() {
			rt := &agentv1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: "mapper-ns-rt", Namespace: namespace},
				Spec: agentv1alpha1.AgentRuntimeSpec{
					Type:      agentv1alpha1.RuntimeTypeAgent,
					TargetRef: agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "mapper-ns-deploy"},
				},
			}
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := &AgentRuntimeReconciler{Client: k8sClient, Scheme: scheme.Scheme}

			// Should match namespace defaults ConfigMap
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ns-defaults", Namespace: namespace,
					Labels: map[string]string{LabelNamespaceDefaults: "true"},
				},
			}
			requests := r.mapNamespaceConfigMapToAgentRuntimes(ctx, cm)
			Expect(requests).NotTo(BeEmpty())

			// Should also match authbridge-runtime-config (matched by name,
			// no label required). Editing this CM is the operator-facing
			// way to change mtls / mode / pipeline plugins for the whole
			// namespace; without this watch the rollout never fires.
			abCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: AuthBridgeRuntimeConfigMapName, Namespace: namespace,
				},
			}
			Expect(r.mapNamespaceConfigMapToAgentRuntimes(ctx, abCM)).NotTo(BeEmpty())

			// Should not match ConfigMap without label and not named
			// authbridge-runtime-config (random unrelated CM).
			noLabel := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "no-label", Namespace: namespace},
			}
			Expect(r.mapNamespaceConfigMapToAgentRuntimes(ctx, noLabel)).To(BeNil())
		})

		It("should map workload events to matching AgentRuntimes", func() {
			rt := &agentv1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: "mapper-wl-rt", Namespace: namespace},
				Spec: agentv1alpha1.AgentRuntimeSpec{
					Type:      agentv1alpha1.RuntimeTypeAgent,
					TargetRef: agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "mapper-wl-deploy"},
				},
			}
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := &AgentRuntimeReconciler{Client: k8sClient, Scheme: scheme.Scheme}
			mapper := r.mapWorkloadToAgentRuntime("apps/v1", "Deployment")

			// Matching
			deploy := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "mapper-wl-deploy", Namespace: namespace},
			}
			requests := mapper(ctx, deploy)
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Name).To(Equal("mapper-wl-rt"))

			// Non-matching
			other := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "other-deploy", Namespace: namespace},
			}
			Expect(mapper(ctx, other)).To(BeEmpty())
		})
	})

	Context("Pure logic helpers", func() {
		It("mergeMaps should override base with override values", func() {
			result := mergeMaps(
				map[string]string{"a": "1", "b": "2"},
				map[string]string{"b": "overridden", "c": "3"},
			)
			Expect(result["a"]).To(Equal("1"))
			Expect(result["b"]).To(Equal("overridden"))
			Expect(result["c"]).To(Equal("3"))
		})

		It("mergeMaps should handle nil inputs", func() {
			result := mergeMaps(nil, nil)
			Expect(result).To(BeEmpty())

			result = mergeMaps(map[string]string{"a": "1"}, nil)
			Expect(result["a"]).To(Equal("1"))
		})

		It("hashResolvedConfig should be deterministic and produce 64-char hex", func() {
			config := resolvedConfig{
				Defaults: map[string]string{"b": "2", "a": "1"},
			}
			hash1, _ := hashResolvedConfig(config)
			hash2, _ := hashResolvedConfig(config)

			Expect(hash1).To(Equal(hash2))
			Expect(hash1).To(HaveLen(64))
		})

		It("isPodOwnedByWorkload should match correctly", func() {
			// Deployment match
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "my-deploy-abc123", Controller: ptr.To(true)}},
				},
			}
			Expect(isPodOwnedByWorkload(pod, "my-deploy")).To(BeTrue())

			// No prefix collision
			pod.OwnerReferences[0] = metav1.OwnerReference{Kind: "ReplicaSet", Name: "my-deploy-v2-abc123", Controller: ptr.To(true)}
			Expect(isPodOwnedByWorkload(pod, "my-deploy")).To(BeFalse())
			Expect(isPodOwnedByWorkload(pod, "my-deploy-v2")).To(BeTrue())

			// StatefulSet match
			pod.OwnerReferences[0] = metav1.OwnerReference{Kind: "StatefulSet", Name: "my-sts", Controller: ptr.To(true)}
			Expect(isPodOwnedByWorkload(pod, "my-sts")).To(BeTrue())
			Expect(isPodOwnedByWorkload(pod, "other-sts")).To(BeFalse())
		})

		It("agentRuntimesToRequests should return nil for empty input", func() {
			Expect(agentRuntimesToRequests(nil)).To(BeNil())
			Expect(agentRuntimesToRequests([]agentv1alpha1.AgentRuntime{})).To(BeNil())

			items := []agentv1alpha1.AgentRuntime{
				{ObjectMeta: metav1.ObjectMeta{Name: "rt1", Namespace: "ns1"}},
			}
			result := agentRuntimesToRequests(items)
			Expect(result).To(Equal([]reconcile.Request{
				{NamespacedName: types.NamespacedName{Name: "rt1", Namespace: "ns1"}},
			}))
		})

		It("newRuntimePodTemplateAccessor should support Deployment and StatefulSet", func() {
			_, ok := newRuntimePodTemplateAccessor("Deployment")
			Expect(ok).To(BeTrue())

			_, ok = newRuntimePodTemplateAccessor("StatefulSet")
			Expect(ok).To(BeTrue())

			_, ok = newRuntimePodTemplateAccessor("DaemonSet")
			Expect(ok).To(BeFalse())
		})
	})
})
