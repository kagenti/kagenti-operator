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

package mlflow

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is the GVK for the mlflows.mlflow.opendatahub.io CRD.
	SchemeGroupVersion = schema.GroupVersion{Group: "mlflow.opendatahub.io", Version: "v1"}

	// DSCSchemeGroupVersion is the GVK for datascienceclusters.datasciencecluster.opendatahub.io.
	DSCSchemeGroupVersion = schema.GroupVersion{Group: "datasciencecluster.opendatahub.io", Version: "v2"}

	// SchemeBuilder is used to add the MLflow and DSC types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme registers the MLflow and DSC types with a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&MLflow{},
		&MLflowList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)

	scheme.AddKnownTypes(DSCSchemeGroupVersion,
		&DataScienceCluster{},
		&DataScienceClusterList{},
	)
	metav1.AddToGroupVersion(scheme, DSCSchemeGroupVersion)
	return nil
}

// MLflow is a minimal representation of the mlflows.mlflow.opendatahub.io/v1 CR.
type MLflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MLflowSpec   `json:"spec,omitempty"`
	Status            MLflowStatus `json:"status,omitempty"`
}

// MLflowSpec contains the desired state for an MLflow instance.
// Only the fields used by the operator for CR creation are included.
type MLflowSpec struct {
	Storage              *MLflowStorage `json:"storage,omitempty"`
	BackendStoreURI      string         `json:"backendStoreUri,omitempty"`
	ArtifactsDestination string         `json:"artifactsDestination,omitempty"`
	ServeArtifacts       bool           `json:"serveArtifacts,omitempty"`
}

type MLflowStorage struct {
	AccessModes []string                   `json:"accessModes,omitempty"`
	Resources   *MLflowStorageResourceReqs `json:"resources,omitempty"`
}

type MLflowStorageResourceReqs struct {
	Requests map[string]string `json:"requests,omitempty"`
}

type MLflowStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// URL is the external gateway URL for the MLflow server (e.g. via the RHOAI data-science gateway).
	URL string `json:"url,omitempty"`
	// Address holds the in-cluster address for the MLflow server.
	Address *MLflowAddress `json:"address,omitempty"`
}

type MLflowAddress struct {
	// URL is the in-cluster service URL (e.g. https://mlflow.redhat-ods-applications.svc:8443).
	URL string `json:"url,omitempty"`
}

type MLflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MLflow `json:"items"`
}

func (in *MLflow) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *MLflow) DeepCopy() *MLflow {
	if in == nil {
		return nil
	}
	out := new(MLflow)
	in.DeepCopyInto(out)
	return out
}

func (in *MLflow) DeepCopyInto(out *MLflow) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *MLflowSpec) DeepCopyInto(out *MLflowSpec) {
	*out = *in
	if in.Storage != nil {
		out.Storage = new(MLflowStorage)
		in.Storage.DeepCopyInto(out.Storage)
	}
}

func (in *MLflowStorage) DeepCopyInto(out *MLflowStorage) {
	*out = *in
	if in.AccessModes != nil {
		out.AccessModes = make([]string, len(in.AccessModes))
		copy(out.AccessModes, in.AccessModes)
	}
	if in.Resources != nil {
		out.Resources = new(MLflowStorageResourceReqs)
		in.Resources.DeepCopyInto(out.Resources)
	}
}

func (in *MLflowStorageResourceReqs) DeepCopyInto(out *MLflowStorageResourceReqs) {
	*out = *in
	if in.Requests != nil {
		out.Requests = make(map[string]string, len(in.Requests))
		for k, v := range in.Requests {
			out.Requests[k] = v
		}
	}
}

func (in *MLflowStatus) DeepCopyInto(out *MLflowStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
	if in.Address != nil {
		out.Address = &MLflowAddress{URL: in.Address.URL}
	}
}

func (in *MLflowList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *MLflowList) DeepCopy() *MLflowList {
	if in == nil {
		return nil
	}
	out := new(MLflowList)
	in.DeepCopyInto(out)
	return out
}

func (in *MLflowList) DeepCopyInto(out *MLflowList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]MLflow, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// ---------------------------------------------------------------------------
// DataScienceCluster — minimal representation for DSC state inspection.
// Only spec.components.mlflowoperator.managementState is needed.
// ---------------------------------------------------------------------------

type DataScienceCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DSCSpec `json:"spec,omitempty"`
}

type DSCSpec struct {
	Components DSCComponents `json:"components,omitempty"`
}

type DSCComponents struct {
	MLflowOperator DSCComponentState `json:"mlflowoperator,omitempty"`
}

type DSCComponentState struct {
	ManagementState string `json:"managementState,omitempty"`
}

type DataScienceClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DataScienceCluster `json:"items"`
}

func (in *DataScienceCluster) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *DataScienceCluster) DeepCopy() *DataScienceCluster {
	if in == nil {
		return nil
	}
	out := new(DataScienceCluster)
	in.DeepCopyInto(out)
	return out
}

func (in *DataScienceCluster) DeepCopyInto(out *DataScienceCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

func (in *DataScienceClusterList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *DataScienceClusterList) DeepCopy() *DataScienceClusterList {
	if in == nil {
		return nil
	}
	out := new(DataScienceClusterList)
	in.DeepCopyInto(out)
	return out
}

func (in *DataScienceClusterList) DeepCopyInto(out *DataScienceClusterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]DataScienceCluster, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
