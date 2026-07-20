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

// Package kuadrant defines a local subset of the Kuadrant API
// (kuadrant.io/v1beta1) used by the Kuadrant operand controller.
// These types are intentionally minimal to avoid importing the full
// Kuadrant operator SDK. Only metadata is modeled because the setup
// script creates a bare Kuadrant CR with no spec fields.
package kuadrant

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	SchemeGroupVersion = schema.GroupVersion{Group: "kuadrant.io", Version: "v1beta1"}

	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&Kuadrant{},
		&KuadrantList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// Kuadrant is a minimal representation of the kuadrant.io/v1beta1 Kuadrant CR.
// The setup script creates this with no spec — just metadata in kuadrant-system.
type Kuadrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

type KuadrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Kuadrant `json:"items"`
}

func (in *Kuadrant) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *Kuadrant) DeepCopy() *Kuadrant {
	if in == nil {
		return nil
	}
	out := new(Kuadrant)
	in.DeepCopyInto(out)
	return out
}

func (in *Kuadrant) DeepCopyInto(out *Kuadrant) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
}

func (in *KuadrantList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *KuadrantList) DeepCopy() *KuadrantList {
	if in == nil {
		return nil
	}
	out := new(KuadrantList)
	in.DeepCopyInto(out)
	return out
}

func (in *KuadrantList) DeepCopyInto(out *KuadrantList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Kuadrant, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
