/*
Copyright 2025.

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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestAuthorizationPolicyFromUnstructured(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agent.rossoctl.dev/v1alpha1",
			"kind":       "AuthorizationPolicy",
			"metadata": map[string]any{
				"name":      "test-client",
				"namespace": "default",
			},
			"spec": map[string]any{
				"scope":    "client",
				"clientID": "test-client",
				"policies": []any{
					map[string]any{
						"path":    "inbound/request.rego",
						"content": "package authbridge.client\ndefault allow := true\n",
					},
				},
			},
		},
	}

	ap, err := AuthorizationPolicyFromUnstructured(obj)
	if err != nil {
		t.Fatalf("AuthorizationPolicyFromUnstructured failed: %v", err)
	}
	if ap.Spec.Scope != PolicyScopeClient {
		t.Fatalf("unexpected scope: %s", ap.Spec.Scope)
	}
	if ap.Spec.ClientID != "test-client" {
		t.Fatalf("unexpected clientID: %s", ap.Spec.ClientID)
	}
	if len(ap.Spec.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(ap.Spec.Policies))
	}
}

func TestAuthorizationPolicyFromUnstructured_Global(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agent.rossoctl.dev/v1alpha1",
			"kind":       "AuthorizationPolicy",
			"metadata": map[string]any{
				"name":      "global-policy",
				"namespace": "rossoctl-system",
			},
			"spec": map[string]any{
				"scope": "global",
				"policies": []any{
					map[string]any{
						"path":    "inbound/request.rego",
						"content": "package authbridge.global\ndefault allow := true\n",
					},
				},
			},
		},
	}

	ap, err := AuthorizationPolicyFromUnstructured(obj)
	if err != nil {
		t.Fatalf("AuthorizationPolicyFromUnstructured failed: %v", err)
	}
	if ap.Spec.Scope != PolicyScopeGlobal {
		t.Fatalf("unexpected scope: %s", ap.Spec.Scope)
	}
	if ap.Spec.ClientID != "" {
		t.Fatalf("expected empty clientID for global, got: %s", ap.Spec.ClientID)
	}
}

func TestAuthorizationPolicyRoundTrip(t *testing.T) {
	original := &AuthorizationPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "agent.rossoctl.dev/v1alpha1",
			Kind:       "AuthorizationPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-client",
			Namespace: "default",
		},
		Spec: AuthorizationPolicySpec{
			Scope:    PolicyScopeClient,
			ClientID: "my-client",
			Policies: []PolicyEntry{
				{
					Path:    "inbound/request.rego",
					Content: "package authbridge.client\ndefault allow := false\n",
				},
				{
					Path:    "outbound/request.rego",
					Content: "package authbridge.client\ndefault allow := true\n",
				},
			},
		},
	}

	obj, err := AuthorizationPolicyToUnstructured(original)
	if err != nil {
		t.Fatalf("AuthorizationPolicyToUnstructured failed: %v", err)
	}

	roundTripped, err := AuthorizationPolicyFromUnstructured(obj)
	if err != nil {
		t.Fatalf("AuthorizationPolicyFromUnstructured failed: %v", err)
	}

	if roundTripped.Spec.Scope != original.Spec.Scope {
		t.Fatalf("scope mismatch: %s vs %s", roundTripped.Spec.Scope, original.Spec.Scope)
	}
	if roundTripped.Spec.ClientID != original.Spec.ClientID {
		t.Fatalf("clientID mismatch: %s vs %s", roundTripped.Spec.ClientID, original.Spec.ClientID)
	}
	if len(roundTripped.Spec.Policies) != len(original.Spec.Policies) {
		t.Fatalf("policy count mismatch: %d vs %d", len(roundTripped.Spec.Policies), len(original.Spec.Policies))
	}
}
