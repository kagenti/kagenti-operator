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
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newNetworkResource(networkType string, routingViaHost *bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata": map[string]interface{}{
				"name": "cluster",
			},
			"spec": map[string]interface{}{
				"defaultNetwork": map[string]interface{}{
					"type": networkType,
				},
			},
		},
	}
	if routingViaHost != nil {
		_ = unstructured.SetNestedField(obj.Object, *routingViaHost,
			"spec", "defaultNetwork", "ovnKubernetesConfig", "gatewayConfig", "routingViaHost")
	}
	return obj
}

var _ = Describe("CheckOVNNetworkConfig", func() {
	ctx := context.Background()

	It("should return warning when OVN routingViaHost is not set", func() {
		network := newNetworkResource("OVNKubernetes", nil)
		fc := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(network).
			Build()

		warning := CheckOVNNetworkConfig(ctx, fc)

		Expect(warning).To(ContainSubstring("routingViaHost is not enabled"))
	})

	It("should return warning when routingViaHost is explicitly false", func() {
		rvh := false
		network := newNetworkResource("OVNKubernetes", &rvh)
		fc := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(network).
			Build()

		warning := CheckOVNNetworkConfig(ctx, fc)

		Expect(warning).To(ContainSubstring("routingViaHost is not enabled"))
	})

	It("should return empty string when routingViaHost is true", func() {
		rvh := true
		network := newNetworkResource("OVNKubernetes", &rvh)
		fc := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(network).
			Build()

		warning := CheckOVNNetworkConfig(ctx, fc)

		Expect(warning).To(BeEmpty())
	})

	It("should return empty string for non-OVN network types", func() {
		network := newNetworkResource("OpenShiftSDN", nil)
		fc := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(network).
			Build()

		warning := CheckOVNNetworkConfig(ctx, fc)

		Expect(warning).To(BeEmpty())
	})

	It("should return error message when network resource is missing", func() {
		fc := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			Build()

		warning := CheckOVNNetworkConfig(ctx, fc)

		Expect(warning).To(ContainSubstring("could not read"))
	})
})
