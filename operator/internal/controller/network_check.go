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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var networkOperatorGVK = schema.GroupVersionKind{
	Group:   "operator.openshift.io",
	Version: "v1",
	Kind:    "Network",
}

// NetworkOperatorCRDExists checks whether the network.operator.openshift.io CRD
// is available on the cluster. Used at startup to conditionally run the network
// check (same pattern as TektonConfigCRDExists).
func NetworkOperatorCRDExists(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false
	}
	resources, err := dc.ServerResourcesForGroupVersion("operator.openshift.io/v1")
	if err != nil {
		return false
	}
	for _, r := range resources.APIResources {
		if r.Kind == "Network" {
			return true
		}
	}
	return false
}

// CheckOVNNetworkConfig reads network.operator.openshift.io/cluster and returns
// a warning message if OVN-Kubernetes is detected without routingViaHost enabled.
// Returns an empty string when the configuration is correct or not applicable.
func CheckOVNNetworkConfig(ctx context.Context, c client.Reader) string {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(networkOperatorGVK)

	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, network); err != nil {
		return fmt.Sprintf("could not read network.operator.openshift.io/cluster: %v", err)
	}

	networkType, found, err := unstructured.NestedString(network.Object, "spec", "defaultNetwork", "type")
	if err != nil || !found {
		return ""
	}

	if networkType != "OVNKubernetes" {
		return ""
	}

	routingViaHost, found, err := unstructured.NestedBool(
		network.Object,
		"spec", "defaultNetwork", "ovnKubernetesConfig", "gatewayConfig", "routingViaHost",
	)
	if err != nil || !found || !routingViaHost {
		return "OVN-Kubernetes detected but routingViaHost is not enabled — " +
			"Istio ambient mTLS will not function correctly. " +
			"Apply the routingViaHost patch: kubectl patch network.operator.openshift.io cluster " +
			"--type=merge -p '{\"spec\":{\"defaultNetwork\":{\"ovnKubernetesConfig\":" +
			"{\"gatewayConfig\":{\"routingViaHost\":true}}}}}'"
	}

	return ""
}
