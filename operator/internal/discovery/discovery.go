package discovery

// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list
// +kubebuilder:rbac:groups=operator.openshift.io,resources=zerotrustworkloadidentitymanagers,verbs=get;list

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	routeGVK = schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	}
	// ZTWIMGVK is the GroupVersionKind for the ZeroTrustWorkloadIdentityManager CR.
	ZTWIMGVK = schema.GroupVersionKind{
		Group:   "operator.openshift.io",
		Version: "v1alpha1",
		Kind:    "ZeroTrustWorkloadIdentityManager",
	}
)

// DiscoverKeycloakPublicURL discovers the external Keycloak URL from cluster resources.
// It tries OpenShift Route first, then Kubernetes Ingress.
func DiscoverKeycloakPublicURL(ctx context.Context, c client.Reader, keycloakNamespace string) (string, error) {
	url, routeErr := discoverFromRoute(ctx, c, keycloakNamespace)
	if routeErr == nil && url != "" {
		return url, nil
	}

	url, ingressErr := discoverFromIngress(ctx, c, keycloakNamespace)
	if ingressErr == nil && url != "" {
		return url, nil
	}

	return "", fmt.Errorf("could not discover Keycloak public URL in namespace %q: route: %v, ingress: %v",
		keycloakNamespace, routeErr, ingressErr)
}

func discoverFromRoute(ctx context.Context, c client.Reader, ns string) (string, error) {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "keycloak"}, route); err != nil {
		return "", err
	}
	host, found, err := unstructured.NestedString(route.Object, "spec", "host")
	if err != nil || !found || host == "" {
		return "", fmt.Errorf("route keycloak in %s has no spec.host", ns)
	}
	return "https://" + host, nil
}

func discoverFromIngress(ctx context.Context, c client.Reader, ns string) (string, error) {
	ingress := &networkingv1.Ingress{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "keycloak"}, ingress); err != nil {
		return "", err
	}
	if len(ingress.Spec.Rules) > 0 && ingress.Spec.Rules[0].Host != "" {
		return "https://" + ingress.Spec.Rules[0].Host, nil
	}
	return "", fmt.Errorf("ingress keycloak in %s has no host rule", ns)
}

// DiscoverSPIRETrustDomain discovers the SPIRE trust domain from cluster resources.
// It tries the ZTWIM CR first (OpenShift), then falls back to reading the spire-bundle
// ConfigMap which contains the trust domain as a JSON key in SPIFFE bundle format.
func DiscoverSPIRETrustDomain(ctx context.Context, c client.Reader, spireNamespace string) (string, error) {
	td, ztwimErr := discoverFromZTWIM(ctx, c)
	if ztwimErr == nil && td != "" {
		return td, nil
	}

	td, bundleErr := discoverFromSpireBundle(ctx, c, spireNamespace)
	if bundleErr == nil && td != "" {
		return td, nil
	}

	return "", fmt.Errorf("could not discover SPIRE trust domain: ztwim: %v, spire-bundle: %v",
		ztwimErr, bundleErr)
}

func discoverFromZTWIM(ctx context.Context, c client.Reader) (string, error) {
	ztwim := &unstructured.Unstructured{}
	ztwim.SetGroupVersionKind(ZTWIMGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, ztwim); err != nil {
		return "", err
	}
	td, found, err := unstructured.NestedString(ztwim.Object, "spec", "trustDomain")
	if err != nil || !found || td == "" {
		return "", fmt.Errorf("ZTWIM CR 'cluster' has no spec.trustDomain")
	}
	return td, nil
}

// discoverFromSpireBundle reads the spire-bundle ConfigMap and extracts the trust domain
// from the SPIFFE bundle JSON format (the top-level keys are trust domains).
func discoverFromSpireBundle(ctx context.Context, c client.Reader, ns string) (string, error) {
	if ns == "" {
		ns = "zero-trust-workload-identity-manager"
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "spire-bundle"}, cm); err != nil {
		return "", err
	}

	// Try well-known keys first in deterministic order.
	for _, key := range []string{"bundle.spiffe", "bundle.json"} {
		if val, ok := cm.Data[key]; ok {
			td, err := extractTrustDomainFromBundle(val)
			if err == nil && td != "" {
				return td, nil
			}
		}
	}

	// Fallback: try remaining keys in sorted order for determinism.
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		if k != "bundle.spiffe" && k != "bundle.json" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		td, err := extractTrustDomainFromBundle(cm.Data[k])
		if err == nil && td != "" {
			return td, nil
		}
	}

	return "", fmt.Errorf("spire-bundle ConfigMap in %s has no parseable trust domain", ns)
}

// extractTrustDomainFromBundle parses SPIFFE bundle JSON where top-level keys are trust domains.
// Format: {"example.org": {"keys": [...]}}
//
// This function assumes a single-domain bundle (the common case). When
// multiple trust domains exist (federated deployments), the lexicographically
// smallest is returned for deterministic behaviour across restarts. This may
// pick a federated peer's domain over the local one — if that becomes a
// problem, callers should pass an expected trust domain to select explicitly.
func extractTrustDomainFromBundle(raw string) (string, error) {
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return "", err
	}
	domains := make([]string, 0, len(bundle))
	for domain := range bundle {
		if domain != "" {
			domains = append(domains, domain)
		}
	}
	if len(domains) == 0 {
		return "", fmt.Errorf("empty bundle")
	}
	sort.Strings(domains)
	return domains[0], nil
}
