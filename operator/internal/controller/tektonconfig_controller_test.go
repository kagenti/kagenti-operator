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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/rossoctl/operator/internal/tekton"
)

var _ = Describe("TektonConfigReconciler", func() {
	var (
		reconciler *TektonConfigReconciler
		scheme     *runtime.Scheme
	)

	newTektonConfig := func(spec tekton.TektonConfigSpec) *tekton.TektonConfig {
		return &tekton.TektonConfig{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "operator.tekton.dev/v1alpha1",
				Kind:       "TektonConfig",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "config",
			},
			Spec: spec,
		}
	}

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		Expect(tekton.AddToScheme(scheme)).To(Succeed())
	})

	It("should patch TektonConfig when set-security-context is missing", func() {
		tc := newTektonConfig(tekton.TektonConfigSpec{})

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(tc).
			Build()

		reconciler = &TektonConfigReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "config"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		patched := &tekton.TektonConfig{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{Name: "config"}, patched)).To(Succeed())

		Expect(patched.Spec.Pipeline.SetSecurityContext).NotTo(BeNil())
		Expect(*patched.Spec.Pipeline.SetSecurityContext).To(BeTrue())
		Expect(patched.Spec.Platforms.OpenShift.SCC.Default).To(Equal("pipelines-scc"))
		Expect(patched.Spec.Pruner.Resources).To(Equal([]string{"taskrun", "pipelinerun"}))
	})

	It("should patch TektonConfig when set-security-context is false", func() {
		tc := newTektonConfig(tekton.TektonConfigSpec{
			Pipeline: tekton.TektonPipeline{
				SetSecurityContext: ptr.To(false),
			},
			Platforms: tekton.TektonPlatforms{
				OpenShift: tekton.TektonOpenShift{
					SCC: tekton.TektonSCC{Default: "pipelines-scc"},
				},
			},
			Pruner: tekton.TektonPruner{
				Resources: []string{"taskrun", "pipelinerun"},
				Keep:      ptr.To(100),
				Schedule:  "0 8 * * *",
			},
		})

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(tc).
			Build()

		reconciler = &TektonConfigReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "config"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		patched := &tekton.TektonConfig{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{Name: "config"}, patched)).To(Succeed())

		Expect(patched.Spec.Pipeline.SetSecurityContext).NotTo(BeNil())
		Expect(*patched.Spec.Pipeline.SetSecurityContext).To(BeTrue())
	})

	It("should not patch when TektonConfig is already correctly configured", func() {
		tc := newTektonConfig(tekton.TektonConfigSpec{
			Pipeline: tekton.TektonPipeline{
				SetSecurityContext: ptr.To(true),
			},
			Platforms: tekton.TektonPlatforms{
				OpenShift: tekton.TektonOpenShift{
					SCC: tekton.TektonSCC{Default: "pipelines-scc"},
				},
			},
			Pruner: tekton.TektonPruner{
				Resources: []string{"taskrun", "pipelinerun"},
				Keep:      ptr.To(100),
				Schedule:  "0 8 * * *",
			},
		})

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(tc).
			Build()

		reconciler = &TektonConfigReconciler{Client: fakeClient}

		before := &tekton.TektonConfig{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{Name: "config"}, before)).To(Succeed())
		rvBefore := before.ResourceVersion

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "config"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		after := &tekton.TektonConfig{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{Name: "config"}, after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(rvBefore))
	})

	It("should handle not-found gracefully", func() {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		reconciler = &TektonConfigReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "config"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("should patch SCC default when empty on HyperShift", func() {
		tc := newTektonConfig(tekton.TektonConfigSpec{
			Pipeline: tekton.TektonPipeline{
				SetSecurityContext: ptr.To(true),
			},
			Pruner: tekton.TektonPruner{
				Resources: []string{"taskrun", "pipelinerun"},
				Keep:      ptr.To(100),
				Schedule:  "0 8 * * *",
			},
		})

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(tc).
			Build()

		reconciler = &TektonConfigReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "config"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		patched := &tekton.TektonConfig{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{Name: "config"}, patched)).To(Succeed())

		Expect(patched.Spec.Platforms.OpenShift.SCC.Default).To(Equal("pipelines-scc"))
	})

	It("should patch pruner when resources are missing", func() {
		tc := newTektonConfig(tekton.TektonConfigSpec{
			Pipeline: tekton.TektonPipeline{
				SetSecurityContext: ptr.To(true),
			},
			Platforms: tekton.TektonPlatforms{
				OpenShift: tekton.TektonOpenShift{
					SCC: tekton.TektonSCC{Default: "pipelines-scc"},
				},
			},
		})

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(tc).
			Build()

		reconciler = &TektonConfigReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "config"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		patched := &tekton.TektonConfig{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{Name: "config"}, patched)).To(Succeed())

		Expect(patched.Spec.Pruner.Resources).To(Equal([]string{"taskrun", "pipelinerun"}))
		Expect(patched.Spec.Pruner.Keep).NotTo(BeNil())
		Expect(*patched.Spec.Pruner.Keep).To(Equal(100))
		Expect(patched.Spec.Pruner.Schedule).To(Equal("0 8 * * *"))
	})
})
