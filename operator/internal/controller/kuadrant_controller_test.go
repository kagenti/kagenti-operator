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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/rossoctl/operator/internal/kuadrant"
)

var _ = Describe("KuadrantReconciler", func() {
	var (
		reconciler *KuadrantReconciler
		scheme     *runtime.Scheme
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		Expect(kuadrant.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
	})

	It("should create namespace and Kuadrant CR when neither exists", func() {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		reconciler = &KuadrantReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      kuadrantCRName,
				Namespace: kuadrantNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		ns := &corev1.Namespace{}
		Expect(fakeClient.Get(context.Background(),
			types.NamespacedName{Name: kuadrantNamespace}, ns)).To(Succeed())

		kq := &kuadrant.Kuadrant{}
		Expect(fakeClient.Get(context.Background(),
			types.NamespacedName{Name: kuadrantCRName, Namespace: kuadrantNamespace}, kq)).To(Succeed())
		Expect(kq.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
	})

	It("should create Kuadrant CR when namespace exists but CR does not", func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: kuadrantNamespace},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ns).
			Build()

		reconciler = &KuadrantReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      kuadrantCRName,
				Namespace: kuadrantNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		kq := &kuadrant.Kuadrant{}
		Expect(fakeClient.Get(context.Background(),
			types.NamespacedName{Name: kuadrantCRName, Namespace: kuadrantNamespace}, kq)).To(Succeed())
		Expect(kq.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
	})

	It("should add managed-by label when CR exists without it", func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: kuadrantNamespace},
		}
		kq := &kuadrant.Kuadrant{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "kuadrant.io/v1beta1",
				Kind:       "Kuadrant",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      kuadrantCRName,
				Namespace: kuadrantNamespace,
			},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ns, kq).
			Build()

		reconciler = &KuadrantReconciler{Client: fakeClient}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      kuadrantCRName,
				Namespace: kuadrantNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		patched := &kuadrant.Kuadrant{}
		Expect(fakeClient.Get(context.Background(),
			types.NamespacedName{Name: kuadrantCRName, Namespace: kuadrantNamespace}, patched)).To(Succeed())
		Expect(patched.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
	})

	It("should be a no-op when CR exists with correct labels", func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: kuadrantNamespace},
		}
		kq := &kuadrant.Kuadrant{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "kuadrant.io/v1beta1",
				Kind:       "Kuadrant",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      kuadrantCRName,
				Namespace: kuadrantNamespace,
				Labels: map[string]string{
					LabelManagedBy: LabelManagedByValue,
				},
			},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ns, kq).
			Build()

		reconciler = &KuadrantReconciler{Client: fakeClient}

		before := &kuadrant.Kuadrant{}
		Expect(fakeClient.Get(context.Background(),
			types.NamespacedName{Name: kuadrantCRName, Namespace: kuadrantNamespace}, before)).To(Succeed())
		rvBefore := before.ResourceVersion

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      kuadrantCRName,
				Namespace: kuadrantNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		after := &kuadrant.Kuadrant{}
		Expect(fakeClient.Get(context.Background(),
			types.NamespacedName{Name: kuadrantCRName, Namespace: kuadrantNamespace}, after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(rvBefore))
	})

	It("should require leader election for the bootstrap runnable", func() {
		b := &kuadrantBootstrap{reconciler: &KuadrantReconciler{}}
		Expect(b.NeedLeaderElection()).To(BeTrue())
	})
})
