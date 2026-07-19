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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("ensureIstioMeshLabels", func() {
	var (
		reconciler *AgentRuntimeReconciler
		nsCounter  int
	)

	newTestNamespace := func(labels map[string]string, annotations map[string]string) *corev1.Namespace {
		nsCounter++
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        fmt.Sprintf("istio-test-%d", nsCounter),
				Labels:      labels,
				Annotations: annotations,
			},
		}
		Expect(k8sClient.Create(context.Background(), ns)).To(Succeed())
		return ns
	}

	BeforeEach(func() {
		reconciler = &AgentRuntimeReconciler{
			Client: k8sClient,
		}
	})

	It("should add Istio labels to a bare namespace", func() {
		ns := newTestNamespace(nil, nil)

		labeled, err := reconciler.ensureIstioMeshLabels(ctx, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(labeled).To(BeTrue())

		updated := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), updated)).To(Succeed())
		Expect(updated.Labels[LabelIstioDiscovery]).To(Equal("enabled"))
		Expect(updated.Labels[LabelIstioDataplaneMode]).To(Equal("ambient"))
	})

	It("should be idempotent on already-labeled namespace", func() {
		ns := newTestNamespace(map[string]string{
			LabelIstioDiscovery:     "enabled",
			LabelIstioDataplaneMode: "ambient",
		}, nil)

		labeled, err := reconciler.ensureIstioMeshLabels(ctx, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(labeled).To(BeTrue())

		updated := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), updated)).To(Succeed())
		Expect(updated.Labels[LabelIstioDiscovery]).To(Equal("enabled"))
		Expect(updated.Labels[LabelIstioDataplaneMode]).To(Equal("ambient"))
	})

	It("should skip namespace with opt-out annotation", func() {
		ns := newTestNamespace(nil, map[string]string{
			AnnotationIstioMeshOptOut: "disabled",
		})

		labeled, err := reconciler.ensureIstioMeshLabels(ctx, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(labeled).To(BeFalse())

		updated := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), updated)).To(Succeed())
		Expect(updated.Labels).NotTo(HaveKey(LabelIstioDiscovery))
		Expect(updated.Labels).NotTo(HaveKey(LabelIstioDataplaneMode))
	})

	It("should preserve existing labels", func() {
		ns := newTestNamespace(map[string]string{
			"team": "platform",
			"env":  "test",
		}, nil)

		labeled, err := reconciler.ensureIstioMeshLabels(ctx, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(labeled).To(BeTrue())

		updated := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), updated)).To(Succeed())
		Expect(updated.Labels["team"]).To(Equal("platform"))
		Expect(updated.Labels["env"]).To(Equal("test"))
		Expect(updated.Labels[LabelIstioDiscovery]).To(Equal("enabled"))
		Expect(updated.Labels[LabelIstioDataplaneMode]).To(Equal("ambient"))
	})

	It("should label namespace when annotation value is not 'disabled'", func() {
		ns := newTestNamespace(nil, map[string]string{
			AnnotationIstioMeshOptOut: "enabled",
		})

		labeled, err := reconciler.ensureIstioMeshLabels(ctx, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(labeled).To(BeTrue())

		updated := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), updated)).To(Succeed())
		Expect(updated.Labels[LabelIstioDiscovery]).To(Equal("enabled"))
		Expect(updated.Labels[LabelIstioDataplaneMode]).To(Equal("ambient"))
	})
})
