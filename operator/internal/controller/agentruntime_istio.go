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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ConditionTypeIstioMeshEnrolled = "IstioMeshEnrolled"

	AnnotationIstioMeshOptOut = "rossoctl.io/istio-mesh"

	LabelIstioDiscovery     = "istio-discovery"
	LabelIstioDataplaneMode = "istio.io/dataplane-mode"
)

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;patch

// ensureIstioMeshLabels patches the namespace with Istio ambient mesh labels
// unless the namespace has opted out via the rossoctl.io/istio-mesh annotation.
// Returns true if labels were applied (or already present), false if opted out.
func (r *AgentRuntimeReconciler) ensureIstioMeshLabels(ctx context.Context, namespace string) (bool, error) {
	logger := log.FromContext(ctx)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return false, err
	}

	if ns.Annotations[AnnotationIstioMeshOptOut] == "disabled" {
		logger.V(1).Info("Namespace opted out of Istio mesh enrollment", "namespace", namespace)
		return false, nil
	}

	if ns.Labels[LabelIstioDiscovery] == "enabled" && ns.Labels[LabelIstioDataplaneMode] == "ambient" {
		return true, nil
	}

	patch := client.MergeFrom(ns.DeepCopy())
	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	ns.Labels[LabelIstioDiscovery] = "enabled"
	ns.Labels[LabelIstioDataplaneMode] = "ambient"

	if err := r.Patch(ctx, ns, patch); err != nil {
		return false, err
	}

	logger.V(1).Info("Labeled namespace for Istio ambient mesh", "namespace", namespace)
	return true, nil
}
