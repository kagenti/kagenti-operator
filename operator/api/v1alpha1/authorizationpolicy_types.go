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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	AuthorizationPolicyResource = "authorizationpolicies"
	AuthorizationPolicyKind     = "AuthorizationPolicy"
)

func AuthorizationPolicyGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: AuthorizationPolicyResource,
	}
}

type PolicyScope string

const (
	PolicyScopeGlobal    PolicyScope = "global"
	PolicyScopeNamespace PolicyScope = "namespace"
	PolicyScopeClient    PolicyScope = "client"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ap
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="ClientID",type=string,JSONPath=`.spec.clientID`
// +kubebuilder:printcolumn:name="Hash",type=string,JSONPath=`.status.bundleHash`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AuthorizationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AuthorizationPolicySpec   `json:"spec"`
	Status            AuthorizationPolicyStatus `json:"status,omitempty"`
}

type AuthorizationPolicySpec struct {
	// +kubebuilder:validation:Enum=global;namespace;client
	// +kubebuilder:default=client
	Scope PolicyScope `json:"scope"`

	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$"
	ClientID string `json:"clientID,omitempty"`

	// +kubebuilder:validation:MinItems=1
	Policies []PolicyEntry `json:"policies"`
}

type PolicyEntry struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern="^[a-z0-9][a-z0-9/_.-]*\\.rego$"
	Path string `json:"path"`

	// +kubebuilder:validation:MinLength=1
	Content string `json:"content"`
}

type AuthorizationPolicyStatus struct {
	BundleHash string             `json:"bundleHash,omitempty"`
	LastBuilt  *metav1.Time       `json:"lastBuilt,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type AuthorizationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AuthorizationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AuthorizationPolicy{}, &AuthorizationPolicyList{})
}
