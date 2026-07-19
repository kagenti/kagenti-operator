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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kd "github.com/rossoctl/operator/internal/discovery"
)

const testTrustDomain = "example.test"

func newSpireTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	return s
}

func newSpireReconciler(objs ...runtime.Object) (*SpireOperandReconciler, *fake.ClientBuilder) {
	s := newSpireTestScheme()
	builder := fake.NewClientBuilder().WithScheme(s)
	if len(objs) > 0 {
		clientObjs := make([]runtime.Object, len(objs))
		copy(clientObjs, objs)
	}
	return &SpireOperandReconciler{
		Scheme:      s,
		TrustDomain: testTrustDomain,
	}, builder
}

func newZTWIM(trustDomain string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kd.ZTWIMGVK)
	obj.SetName(spireOperandName)
	obj.SetLabels(map[string]string{
		LabelManagedBy: LabelManagedByValue,
	})
	_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
		"trustDomain":     trustDomain,
		"clusterName":     "agent-platform",
		"bundleConfigMap": "spire-bundle",
	}, "spec")
	return obj
}

func newChildCR(gvk schema.GroupVersionKind, spec map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(spireOperandName)
	obj.SetLabels(map[string]string{
		LabelManagedBy: LabelManagedByValue,
	})
	if spec != nil {
		_ = unstructured.SetNestedField(obj.Object, spec, "spec")
	}
	return obj
}

func spireReconcileRequest() reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Name: spireOperandName},
	}
}

var _ = Describe("SPIRE Operand Controller", func() {
	ctx := context.Background()

	Context("When ZTWIM CR does not exist", func() {
		It("should return without error (bootstrap pending)", func() {
			r, builder := newSpireReconciler()
			cl := builder.Build()
			r.Client = cl

			result, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("When ZTWIM CR exists", func() {
		It("should ensure ZTWIM spec matches desired state", func() {
			ztwim := newZTWIM(testTrustDomain)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			result, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(kd.ZTWIMGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, got)).To(Succeed())

			td, _, _ := unstructured.NestedString(got.Object, "spec", "trustDomain")
			Expect(td).To(Equal(testTrustDomain))

			cn, _, _ := unstructured.NestedString(got.Object, "spec", "clusterName")
			Expect(cn).To(Equal("agent-platform"))

			Expect(got.GetLabels()[LabelManagedBy]).To(Equal(LabelManagedByValue))
		})

		It("should create all 4 child CRs", func() {
			ztwim := newZTWIM(testTrustDomain)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			result, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			for _, gvk := range []schema.GroupVersionKind{
				spiffeCSIDriverGVK, spireServerGVK, spireAgentGVK, spireOIDCProviderGVK,
			} {
				child := &unstructured.Unstructured{}
				child.SetGroupVersionKind(gvk)
				Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, child)).To(Succeed(),
					"expected %s CR to exist", gvk.Kind)
				Expect(child.GetLabels()[LabelManagedBy]).To(Equal(LabelManagedByValue))
			}
		})

		It("should set correct SpiffeCSIDriver spec", func() {
			ztwim := newZTWIM(testTrustDomain)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			_, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			child := &unstructured.Unstructured{}
			child.SetGroupVersionKind(spiffeCSIDriverGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, child)).To(Succeed())

			path, _, _ := unstructured.NestedString(child.Object, "spec", "agentSocketPath")
			Expect(path).To(Equal("/run/spire/agent-sockets"))

			plugin, _, _ := unstructured.NestedString(child.Object, "spec", "pluginName")
			Expect(plugin).To(Equal("csi.spiffe.io"))
		})

		It("should set correct SpireServer spec", func() {
			ztwim := newZTWIM(testTrustDomain)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			_, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			child := &unstructured.Unstructured{}
			child.SetGroupVersionKind(spireServerGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, child)).To(Succeed())

			cn, _, _ := unstructured.NestedString(child.Object, "spec", "caSubject", "commonName")
			Expect(cn).To(Equal(testTrustDomain))

			dbType, _, _ := unstructured.NestedString(child.Object, "spec", "datastore", "databaseType")
			Expect(dbType).To(Equal("sqlite3"))

			issuer, _, _ := unstructured.NestedString(child.Object, "spec", "jwtIssuer")
			Expect(issuer).To(Equal("https://oidc-discovery-provider." + testTrustDomain))
		})

		It("should set correct SpireAgent spec (no trustDomain/clusterName)", func() {
			ztwim := newZTWIM(testTrustDomain)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			_, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			child := &unstructured.Unstructured{}
			child.SetGroupVersionKind(spireAgentGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, child)).To(Succeed())

			psat, _, _ := unstructured.NestedString(child.Object, "spec", "nodeAttestor", "k8sPSATEnabled")
			Expect(psat).To(Equal("true"))

			// Children must NOT have trustDomain or clusterName
			_, found, _ := unstructured.NestedString(child.Object, "spec", "trustDomain")
			Expect(found).To(BeFalse())
			_, found, _ = unstructured.NestedString(child.Object, "spec", "clusterName")
			Expect(found).To(BeFalse())
		})

		It("should set correct SpireOIDCDiscoveryProvider spec", func() {
			ztwim := newZTWIM(testTrustDomain)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			_, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			child := &unstructured.Unstructured{}
			child.SetGroupVersionKind(spireOIDCProviderGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, child)).To(Succeed())

			driver, _, _ := unstructured.NestedString(child.Object, "spec", "csiDriverName")
			Expect(driver).To(Equal("csi.spiffe.io"))

			issuer, _, _ := unstructured.NestedString(child.Object, "spec", "jwtIssuer")
			Expect(issuer).To(Equal("https://oidc-discovery-provider." + testTrustDomain))
		})
	})

	Context("Drift correction", func() {
		It("should preserve immutable ZTWIM trustDomain", func() {
			existingTD := "existing-domain.org"
			ztwim := newZTWIM(existingTD)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			_, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(kd.ZTWIMGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, got)).To(Succeed())

			td, _, _ := unstructured.NestedString(got.Object, "spec", "trustDomain")
			Expect(td).To(Equal(existingTD), "trustDomain is immutable — must preserve existing value")
		})

		It("should correct drifted child spec", func() {
			ztwim := newZTWIM(testTrustDomain)
			driftedCSI := newChildCR(spiffeCSIDriverGVK, map[string]interface{}{
				"agentSocketPath": "/wrong/path",
				"pluginName":      "wrong.plugin",
			})
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim, driftedCSI).Build()
			r.Client = cl

			_, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(spiffeCSIDriverGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, got)).To(Succeed())

			path, _, _ := unstructured.NestedString(got.Object, "spec", "agentSocketPath")
			Expect(path).To(Equal("/run/spire/agent-sockets"))

			plugin, _, _ := unstructured.NestedString(got.Object, "spec", "pluginName")
			Expect(plugin).To(Equal("csi.spiffe.io"))
		})
	})

	Context("Idempotency", func() {
		It("should produce same result on double reconcile", func() {
			ztwim := newZTWIM(testTrustDomain)
			r, builder := newSpireReconciler()
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			result1, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			result2, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result2).To(Equal(result1))

			// All 5 CRs still exist
			for _, gvk := range []schema.GroupVersionKind{
				kd.ZTWIMGVK, spiffeCSIDriverGVK, spireServerGVK, spireAgentGVK, spireOIDCProviderGVK,
			} {
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(gvk)
				Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, obj)).To(Succeed())
			}
		})
	})

	Context("Bootstrap Runnable", func() {
		It("should create ZTWIM when absent", func() {
			s := newSpireTestScheme()
			cl := fake.NewClientBuilder().WithScheme(s).Build()
			b := &SpireBootstrapRunnable{
				Client:      cl,
				TrustDomain: testTrustDomain,
				Log:         ctrl.Log.WithName("test-bootstrap"),
			}

			err := b.Start(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(kd.ZTWIMGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, got)).To(Succeed())

			td, _, _ := unstructured.NestedString(got.Object, "spec", "trustDomain")
			Expect(td).To(Equal(testTrustDomain))
			Expect(got.GetLabels()[LabelManagedBy]).To(Equal(LabelManagedByValue))
		})

		It("should skip when ZTWIM already exists", func() {
			ztwim := newZTWIM(testTrustDomain)
			s := newSpireTestScheme()
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(ztwim).Build()
			b := &SpireBootstrapRunnable{
				Client:      cl,
				TrustDomain: testTrustDomain,
				Log:         ctrl.Log.WithName("test-bootstrap"),
			}

			err := b.Start(ctx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should implement NeedLeaderElection", func() {
			b := &SpireBootstrapRunnable{}
			Expect(b.NeedLeaderElection()).To(BeTrue())
		})
	})

	Context("Spec builders", func() {
		It("should not include trustDomain in child specs", func() {
			r := &SpireOperandReconciler{TrustDomain: testTrustDomain}

			for name, spec := range map[string]map[string]interface{}{
				"SpiffeCSIDriver":            r.spiffeCSIDriverSpec(),
				"SpireAgent":                 r.spireAgentSpec(),
				"SpireOIDCDiscoveryProvider": r.spireOIDCProviderSpec(testTrustDomain),
			} {
				_, hasTD := spec["trustDomain"]
				Expect(hasTD).To(BeFalse(), "%s should not have trustDomain", name)
				_, hasCN := spec["clusterName"]
				Expect(hasCN).To(BeFalse(), "%s should not have clusterName", name)
			}
		})

		It("should include trustDomain only in ZTWIM spec", func() {
			r := &SpireOperandReconciler{TrustDomain: testTrustDomain}
			spec := r.ztwimSpec(testTrustDomain, "")
			Expect(spec["trustDomain"]).To(Equal(testTrustDomain))
			Expect(spec["clusterName"]).To(Equal("agent-platform"))
			Expect(spec["bundleConfigMap"]).To(Equal("spire-bundle"))
		})

		It("should use custom clusterName when provided", func() {
			r := &SpireOperandReconciler{TrustDomain: testTrustDomain, ClusterName: "custom-cluster"}
			spec := r.ztwimSpec(testTrustDomain, "custom-cluster")
			Expect(spec["clusterName"]).To(Equal("custom-cluster"))
		})

		It("should preserve existing trustDomain on reconcile", func() {
			existingTD := "existing-domain.org"
			ztwim := newZTWIM(existingTD)
			r, builder := newSpireReconciler()
			r.TrustDomain = "different-discovered-domain.com"
			cl := builder.WithObjects(ztwim).Build()
			r.Client = cl

			_, err := r.Reconcile(ctx, spireReconcileRequest())
			Expect(err).NotTo(HaveOccurred())

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(kd.ZTWIMGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: spireOperandName}, got)).To(Succeed())

			td, _, _ := unstructured.NestedString(got.Object, "spec", "trustDomain")
			Expect(td).To(Equal(existingTD), "should preserve existing trustDomain, not overwrite with discovered one")
		})
	})
})
