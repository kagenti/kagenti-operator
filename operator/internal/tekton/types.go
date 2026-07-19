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

// Package tekton defines a local subset of the Tekton operator API
// (operator.tekton.dev/v1alpha1) used by the TektonConfig controller.
// These types are intentionally minimal to avoid importing the full
// tekton operator and its dependencies.
//
// Only spec fields that the controller patches are modeled; Status is
// intentionally omitted. The Platforms subtree deep-copy relies on all
// fields being value types (no pointers or slices) — if pointer or slice
// fields are added under Platforms in the future, the DeepCopy methods
// must be updated to handle them explicitly.
package tekton

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var SchemeGroupVersion = schema.GroupVersion{
	Group:   "operator.tekton.dev",
	Version: "v1alpha1",
}

type TektonConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TektonConfigSpec `json:"spec,omitempty"`
}

type TektonConfigSpec struct {
	Pipeline  TektonPipeline  `json:"pipeline,omitempty"`
	Platforms TektonPlatforms `json:"platforms,omitempty"`
	Pruner    TektonPruner    `json:"pruner,omitempty"`
}

type TektonPipeline struct {
	SetSecurityContext *bool `json:"set-security-context,omitempty"`
}

type TektonPlatforms struct {
	OpenShift TektonOpenShift `json:"openshift,omitempty"`
}

type TektonOpenShift struct {
	SCC TektonSCC `json:"scc,omitempty"`
}

type TektonSCC struct {
	Default string `json:"default,omitempty"`
}

type TektonPruner struct {
	Resources []string `json:"resources,omitempty"`
	Keep      *int     `json:"keep,omitempty"`
	Schedule  string   `json:"schedule,omitempty"`
}

type TektonConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TektonConfig `json:"items"`
}

// DeepCopy returns a typed deep copy of the TektonConfig.
func (in *TektonConfig) DeepCopy() *TektonConfig {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = *in.Spec.DeepCopy()
	return &out
}

func (in *TektonConfig) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *TektonConfigList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]TektonConfig, len(in.Items))
		for i := range in.Items {
			out.Items[i] = *in.Items[i].DeepCopy()
		}
	}
	return &out
}

func (in *TektonConfigSpec) DeepCopy() *TektonConfigSpec {
	out := *in
	if in.Pipeline.SetSecurityContext != nil {
		val := *in.Pipeline.SetSecurityContext
		out.Pipeline.SetSecurityContext = &val
	}
	if in.Pruner.Resources != nil {
		out.Pruner.Resources = make([]string, len(in.Pruner.Resources))
		copy(out.Pruner.Resources, in.Pruner.Resources)
	}
	if in.Pruner.Keep != nil {
		val := *in.Pruner.Keep
		out.Pruner.Keep = &val
	}
	return &out
}

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&TektonConfig{},
		&TektonConfigList{},
	)
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}
