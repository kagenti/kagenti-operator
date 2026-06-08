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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/kagenti/operator/api/v1alpha1"
)

var _ = Describe("AgentRuntime Controller", func() {
	const (
		rtName         = "test-agentruntime"
		deploymentName = "test-agent-deploy"
		namespace      = "default"
	)

	ctx := context.Background()

	newDeployment := func(name, ns string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": name},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": name},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "agent", Image: "test-image:latest"},
						},
					},
				},
			},
		}
	}

	newAgentRuntime := func(name, ns, targetName string, rtType agentv1alpha1.RuntimeType) *agentv1alpha1.AgentRuntime {
		return &agentv1alpha1.AgentRuntime{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
			},
			Spec: agentv1alpha1.AgentRuntimeSpec{
				Type: rtType,
				TargetRef: agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       targetName,
				},
			},
		}
	}

	newReconciler := func() *AgentRuntimeReconciler {
		return &AgentRuntimeReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    scheme.Scheme,
		}
	}

	Context("When adding finalizer", func() {
		It("should add finalizer on first reconcile", func() {
			dep := newDeployment("finalizer-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			rt := newAgentRuntime("finalizer-rt", namespace, "finalizer-deploy", agentv1alpha1.RuntimeTypeAgent)
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := newReconciler()
			result, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "finalizer-rt", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			updated := &agentv1alpha1.AgentRuntime{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "finalizer-rt", Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(AgentRuntimeFinalizer))
		})
	})

	Context("When applying labels and config-hash", func() {
		It("should apply labels and config-hash to the Deployment", func() {
			dep := newDeployment("labels-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			rt := newAgentRuntime("labels-rt", namespace, "labels-deploy", agentv1alpha1.RuntimeTypeAgent)
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := newReconciler()
			nn := types.NamespacedName{Name: "labels-rt", Namespace: namespace}

			// First reconcile: adds finalizer
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile: applies labels + hash
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			updatedDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "labels-deploy", Namespace: namespace}, updatedDep)).To(Succeed())

			Expect(updatedDep.Labels[LabelAgentType]).To(Equal("agent"))
			Expect(updatedDep.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
			Expect(updatedDep.Spec.Template.Labels[LabelAgentType]).To(Equal("agent"))
			Expect(updatedDep.Spec.Template.Annotations).To(HaveKey(AnnotationConfigHash))
			Expect(updatedDep.Spec.Template.Annotations[AnnotationConfigHash]).NotTo(BeEmpty())
		})
	})

	Context("When setting status", func() {
		It("should set status to Active with Ready condition", func() {
			dep := newDeployment("status-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			rt := newAgentRuntime("status-rt", namespace, "status-deploy", agentv1alpha1.RuntimeTypeAgent)
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := newReconciler()
			nn := types.NamespacedName{Name: "status-rt", Namespace: namespace}

			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			updated := &agentv1alpha1.AgentRuntime{}
			Expect(k8sClient.Get(ctx, nn, updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal(agentv1alpha1.RuntimePhaseActive))
			Expect(updated.Status.Conditions).NotTo(BeEmpty())

			var readyCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionTypeReady {
					readyCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal("Configured"))
		})
	})

	Context("When reconciling idempotently", func() {
		It("should be idempotent on repeated reconciles", func() {
			dep := newDeployment("idempotent-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			rt := newAgentRuntime("idempotent-rt", namespace, "idempotent-deploy", agentv1alpha1.RuntimeTypeAgent)
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := newReconciler()
			nn := types.NamespacedName{Name: "idempotent-rt", Namespace: namespace}

			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})

			dep1 := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idempotent-deploy", Namespace: namespace}, dep1)).To(Succeed())
			hash1 := dep1.Spec.Template.Annotations[AnnotationConfigHash]
			rv1 := dep1.ResourceVersion

			// Third reconcile: should be a no-op
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			dep2 := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idempotent-deploy", Namespace: namespace}, dep2)).To(Succeed())
			hash2 := dep2.Spec.Template.Annotations[AnnotationConfigHash]
			rv2 := dep2.ResourceVersion

			Expect(hash1).To(Equal(hash2))
			Expect(rv1).To(Equal(rv2), "Deployment should not be updated when already configured")
		})
	})

	Context("When the target Deployment does not exist", func() {
		var rt *agentv1alpha1.AgentRuntime

		BeforeEach(func() {
			rt = newAgentRuntime("rt-no-target", namespace, "nonexistent-deploy", agentv1alpha1.RuntimeTypeAgent)
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, rt)
		})

		It("should set Error phase and TargetNotFound condition", func() {
			r := newReconciler()

			// First reconcile: adds finalizer
			_, _ = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "rt-no-target", Namespace: namespace},
			})
			// Second reconcile: target resolution fails
			result, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "rt-no-target", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero(), "should requeue on target not found")

			updated := &agentv1alpha1.AgentRuntime{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rt-no-target", Namespace: namespace}, updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal(agentv1alpha1.RuntimePhaseError))

			var targetCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionTypeTargetResolved {
					targetCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(targetCond).NotTo(BeNil())
			Expect(targetCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(targetCond.Reason).To(Equal("TargetNotFound"))
		})
	})

	Context("When the AgentRuntime type is tool", func() {
		var dep *appsv1.Deployment
		var rt *agentv1alpha1.AgentRuntime

		BeforeEach(func() {
			dep = newDeployment("tool-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			rt = newAgentRuntime("tool-rt", namespace, "tool-deploy", agentv1alpha1.RuntimeTypeTool)
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, rt)
			_ = k8sClient.Delete(ctx, dep)
		})

		It("should apply kagenti.io/type=tool label", func() {
			r := newReconciler()

			_, _ = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "tool-rt", Namespace: namespace},
			})
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "tool-rt", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tool-deploy", Namespace: namespace}, updatedDep)).To(Succeed())

			Expect(updatedDep.Labels[LabelAgentType]).To(Equal("tool"))
			Expect(updatedDep.Spec.Template.Labels[LabelAgentType]).To(Equal("tool"))
		})
	})

	Context("When the AgentRuntime is deleted", func() {
		var dep *appsv1.Deployment
		var rt *agentv1alpha1.AgentRuntime

		BeforeEach(func() {
			dep = newDeployment("del-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			rt = newAgentRuntime("del-rt", namespace, "del-deploy", agentv1alpha1.RuntimeTypeAgent)
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, dep)
		})

		It("should remove type label and config-hash, and remove managed-by on deletion", func() {
			r := newReconciler()

			// Reconcile to add finalizer + apply config
			_, _ = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "del-rt", Namespace: namespace},
			})
			_, _ = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "del-rt", Namespace: namespace},
			})

			// Confirm labels and hash are set before deletion
			depBefore := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "del-deploy", Namespace: namespace}, depBefore)).To(Succeed())
			Expect(depBefore.Spec.Template.Annotations[AnnotationConfigHash]).NotTo(BeEmpty())
			Expect(depBefore.Labels[LabelAgentType]).NotTo(BeEmpty())
			Expect(depBefore.Spec.Template.Labels[LabelAgentType]).NotTo(BeEmpty())

			// Delete the AgentRuntime
			Expect(k8sClient.Delete(ctx, rt)).To(Succeed())

			// Reconcile handles deletion via finalizer
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "del-rt", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify Deployment state after deletion
			depAfter := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "del-deploy", Namespace: namespace}, depAfter)).To(Succeed())

			// kagenti.io/type removed from both workload metadata and PodTemplateSpec
			Expect(depAfter.Labels).NotTo(HaveKey(LabelAgentType))
			Expect(depAfter.Spec.Template.Labels).NotTo(HaveKey(LabelAgentType))

			// kagenti.io/managed-by removed
			Expect(depAfter.Labels).NotTo(HaveKey(LabelManagedBy))

			// kagenti.io/config-hash removed from PodTemplateSpec
			Expect(depAfter.Spec.Template.Annotations).NotTo(HaveKey(AnnotationConfigHash))

			// Finalizer removed — AgentRuntime should be gone
			deletedRT := &agentv1alpha1.AgentRuntime{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "del-rt", Namespace: namespace}, deletedRT)
			Expect(err).To(HaveOccurred(), "AgentRuntime should be deleted after finalizer removal")
		})
	})

	Context("When the AgentRuntime has identity and trace overrides", func() {
		var dep *appsv1.Deployment
		var rt *agentv1alpha1.AgentRuntime

		BeforeEach(func() {
			dep = newDeployment("override-deploy", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			rt = &agentv1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "override-rt",
					Namespace: namespace,
				},
				Spec: agentv1alpha1.AgentRuntimeSpec{
					Type: agentv1alpha1.RuntimeTypeAgent,
					TargetRef: agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "override-deploy",
					},
					Identity: &agentv1alpha1.IdentitySpec{
						SPIFFE: &agentv1alpha1.SPIFFEIdentity{TrustDomain: "custom.org"},
					},
					Trace: &agentv1alpha1.TraceSpec{
						Endpoint: "custom-collector:4317",
						Protocol: agentv1alpha1.TraceProtocolGRPC,
						Sampling: &agentv1alpha1.SamplingSpec{Rate: 0.5},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, rt)
			_ = k8sClient.Delete(ctx, dep)
		})

		It("should produce a different config-hash than a minimal AgentRuntime", func() {
			r := newReconciler()

			// Reconcile the override RT
			_, _ = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "override-rt", Namespace: namespace},
			})
			_, _ = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "override-rt", Namespace: namespace},
			})

			overrideDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "override-deploy", Namespace: namespace}, overrideDep)).To(Succeed())
			overrideHash := overrideDep.Spec.Template.Annotations[AnnotationConfigHash]

			// Compute hash for a minimal spec (no overrides)
			minimalResult, err := ComputeConfigHash(ctx, k8sClient, namespace, &agentv1alpha1.AgentRuntimeSpec{
				Type:      agentv1alpha1.RuntimeTypeAgent,
				TargetRef: agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "x"},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(overrideHash).NotTo(Equal(minimalResult.Hash), "CR with overrides should have a different hash")
		})
	})

	Context("When targeting a StatefulSet", func() {
		const (
			ssName = "test-agent-sts"
			rtName = "sts-agentruntime"
			ssApp  = "sts-app"
			ssNS   = "default"
		)

		newStatefulSet := func(name, ns string) *appsv1.StatefulSet {
			return &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: appsv1.StatefulSetSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": ssApp}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": ssApp}},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "test-image:latest"}},
						},
					},
				},
			}
		}

		It("should apply labels and config-hash to the StatefulSet pod template", func() {
			ss := newStatefulSet(ssName, ssNS)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, ss) }()

			Eventually(func() error {
				cur := &appsv1.StatefulSet{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ssName, Namespace: ssNS}, cur); err != nil {
					return err
				}
				cur.Status.Replicas = 1
				cur.Status.ReadyReplicas = 1
				return k8sClient.Status().Update(ctx, cur)
			}).Should(Succeed())

			rt := &agentv1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: rtName, Namespace: ssNS},
				Spec: agentv1alpha1.AgentRuntimeSpec{
					Type: agentv1alpha1.RuntimeTypeAgent,
					TargetRef: agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "StatefulSet",
						Name:       ssName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, rt)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, rt) }()

			r := newReconciler()
			nn := types.NamespacedName{Name: rtName, Namespace: ssNS}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			updated := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ssName, Namespace: ssNS}, updated)).To(Succeed())
			Expect(updated.Labels[LabelAgentType]).To(Equal("agent"))
			Expect(updated.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
			Expect(updated.Spec.Template.Labels[LabelAgentType]).To(Equal("agent"))
			Expect(updated.Spec.Template.Annotations).To(HaveKey(AnnotationConfigHash))
			Expect(updated.Spec.Template.Annotations[AnnotationConfigHash]).NotTo(BeEmpty())
		})
	})

	Context("When the AgentRuntime CR does not exist", func() {
		It("should return without error for a not-found CR", func() {
			r := newReconciler()

			result, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent-rt", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("When ensuring namespace ConfigMaps", func() {
		const cmTestNS = "cm-test-ns"

		BeforeEach(func() {
			// Create the kagenti-system namespace for templates
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ClusterDefaultsNamespace}}
			_ = k8sClient.Create(ctx, ns)

			// Create the test namespace
			testNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cmTestNS}}
			_ = k8sClient.Create(ctx, testNS)
		})

		AfterEach(func() {
			for _, name := range templateConfigMapNames {
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ClusterDefaultsNamespace}}
				_ = k8sClient.Delete(ctx, cm)
				cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cmTestNS}}
				_ = k8sClient.Delete(ctx, cm)
			}
		})

		It("should create missing ConfigMaps from templates", func() {
			// Create template ConfigMaps in kagenti-system
			for _, name := range templateConfigMapNames {
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ClusterDefaultsNamespace,
					},
					Data: map[string]string{"config.yaml": "template-content-" + name},
				}
				Expect(k8sClient.Create(ctx, cm)).To(Succeed())
			}

			r := newReconciler()
			Expect(r.ensureNamespaceConfigMaps(ctx, cmTestNS)).To(Succeed())

			for _, name := range templateConfigMapNames {
				created := &corev1.ConfigMap{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: cmTestNS}, created)).To(Succeed())
				Expect(created.Data["config.yaml"]).To(Equal("template-content-" + name))
				Expect(created.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
			}
		})

		It("should skip ConfigMaps that already exist", func() {
			// Create template in kagenti-system
			template := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "authbridge-config",
					Namespace: ClusterDefaultsNamespace,
				},
				Data: map[string]string{"KEYCLOAK_URL": "http://template-url"},
			}
			Expect(k8sClient.Create(ctx, template)).To(Succeed())

			// Pre-create in target namespace with custom content
			existing := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "authbridge-config",
					Namespace: cmTestNS,
				},
				Data: map[string]string{"KEYCLOAK_URL": "http://custom-url"},
			}
			Expect(k8sClient.Create(ctx, existing)).To(Succeed())

			r := newReconciler()
			Expect(r.ensureNamespaceConfigMaps(ctx, cmTestNS)).To(Succeed())

			// Verify custom content was preserved
			result := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "authbridge-config", Namespace: cmTestNS}, result)).To(Succeed())
			Expect(result.Data["KEYCLOAK_URL"]).To(Equal("http://custom-url"))
		})

		It("should skip gracefully when templates are missing", func() {
			r := newReconciler()
			Expect(r.ensureNamespaceConfigMaps(ctx, cmTestNS)).To(Succeed())

			// Verify no ConfigMaps were created
			for _, name := range templateConfigMapNames {
				cm := &corev1.ConfigMap{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: cmTestNS}, cm)
				Expect(err).To(HaveOccurred())
			}
		})

		It("should only create missing ConfigMaps when some already exist", func() {
			// Create all templates in kagenti-system
			for _, name := range templateConfigMapNames {
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: ClusterDefaultsNamespace,
					},
					Data: map[string]string{"config.yaml": "template-" + name},
				}
				Expect(k8sClient.Create(ctx, cm)).To(Succeed())
			}

			// Pre-create only authbridge-config in target namespace
			existing := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "authbridge-config",
					Namespace: cmTestNS,
				},
				Data: map[string]string{"KEYCLOAK_URL": "http://existing"},
			}
			Expect(k8sClient.Create(ctx, existing)).To(Succeed())

			r := newReconciler()
			Expect(r.ensureNamespaceConfigMaps(ctx, cmTestNS)).To(Succeed())

			// authbridge-config should keep its original content
			abCfg := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "authbridge-config", Namespace: cmTestNS}, abCfg)).To(Succeed())
			Expect(abCfg.Data["KEYCLOAK_URL"]).To(Equal("http://existing"))

			// The other 3 should be created from templates
			for _, name := range []string{"authbridge-runtime-config", "envoy-config", "spiffe-helper-config"} {
				cm := &corev1.ConfigMap{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: cmTestNS}, cm)).To(Succeed())
				Expect(cm.Data["config.yaml"]).To(Equal("template-" + name))
				Expect(cm.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
			}
		})
	})

	Context("Sandbox workload support", func() {
		It("should create a Sandbox accessor that reads/writes pod template labels and annotations", func() {
			acc, ok := newRuntimePodTemplateAccessor("Sandbox")
			Expect(ok).To(BeTrue())
			Expect(acc).NotTo(BeNil())

			u := acc.obj.(*unstructured.Unstructured)
			u.Object = map[string]interface{}{
				"apiVersion": "agents.x-k8s.io/v1alpha1",
				"kind":       "Sandbox",
				"metadata": map[string]interface{}{
					"name":      "test-sandbox",
					"namespace": "default",
				},
				"spec": map[string]interface{}{
					"podTemplate": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels":      map[string]interface{}{"app": "my-agent"},
							"annotations": map[string]interface{}{"existing": "value"},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{"name": "agent", "image": "test:latest"},
							},
						},
					},
				},
			}

			// Read existing labels
			labels := acc.getPodLabels(acc.obj)
			Expect(labels).To(HaveKeyWithValue("app", "my-agent"))

			// Write new labels
			labels["kagenti.io/type"] = "agent"
			acc.setPodLabels(acc.obj, labels)

			// Verify labels were set
			updatedLabels := acc.getPodLabels(acc.obj)
			Expect(updatedLabels).To(HaveKeyWithValue("kagenti.io/type", "agent"))
			Expect(updatedLabels).To(HaveKeyWithValue("app", "my-agent"))

			// Read existing annotations
			annotations := acc.getPodAnnotations(acc.obj)
			Expect(annotations).To(HaveKeyWithValue("existing", "value"))

			// Write new annotations
			annotations[AnnotationConfigHash] = "abc123"
			acc.setPodAnnotations(acc.obj, annotations)

			// Verify annotations were set
			updatedAnnotations := acc.getPodAnnotations(acc.obj)
			Expect(updatedAnnotations).To(HaveKeyWithValue(AnnotationConfigHash, "abc123"))
			Expect(updatedAnnotations).To(HaveKeyWithValue("existing", "value"))
		})

		It("should handle Sandbox with no existing pod template metadata", func() {
			acc, ok := newRuntimePodTemplateAccessor("Sandbox")
			Expect(ok).To(BeTrue())

			u := acc.obj.(*unstructured.Unstructured)
			u.Object = map[string]interface{}{
				"apiVersion": "agents.x-k8s.io/v1alpha1",
				"kind":       "Sandbox",
				"metadata": map[string]interface{}{
					"name":      "test-sandbox-empty",
					"namespace": "default",
				},
				"spec": map[string]interface{}{
					"podTemplate": map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{"name": "agent", "image": "test:latest"},
							},
						},
					},
				},
			}

			// Labels should be nil when no metadata.labels exists
			labels := acc.getPodLabels(acc.obj)
			Expect(labels).To(BeNil())

			// Setting labels should work even without existing metadata
			acc.setPodLabels(acc.obj, map[string]string{"kagenti.io/type": "agent"})
			labels = acc.getPodLabels(acc.obj)
			Expect(labels).To(HaveKeyWithValue("kagenti.io/type", "agent"))

			// Annotations should be nil when no metadata.annotations exists
			annotations := acc.getPodAnnotations(acc.obj)
			Expect(annotations).To(BeNil())

			// Setting annotations should work
			acc.setPodAnnotations(acc.obj, map[string]string{AnnotationConfigHash: "hash123"})
			annotations = acc.getPodAnnotations(acc.obj)
			Expect(annotations).To(HaveKeyWithValue(AnnotationConfigHash, "hash123"))
		})

		It("isPodOwnedByWorkload should match Sandbox-owned pods", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-sandbox-pod",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.x-k8s.io/v1alpha1",
							Kind:       "Sandbox",
							Name:       "my-sandbox",
						},
					},
				},
			}

			Expect(isPodOwnedByWorkload(pod, "my-sandbox")).To(BeTrue())
			Expect(isPodOwnedByWorkload(pod, "other-sandbox")).To(BeFalse())
		})

		It("isPodOwnedByWorkload should not match Sandbox name against ReplicaSet ownership", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deploy-pod",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "ReplicaSet",
							Name:       "my-sandbox-abc123",
						},
					},
				},
			}

			// This matches "my-sandbox" as a Deployment (ReplicaSet prefix match)
			Expect(isPodOwnedByWorkload(pod, "my-sandbox")).To(BeTrue())
		})
	})
})
