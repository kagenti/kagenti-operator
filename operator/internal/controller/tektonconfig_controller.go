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
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rossoctl/operator/internal/tekton"
)

var tektonConfigLogger = ctrl.Log.WithName("controller").WithName("TektonConfig")

// TektonConfigReconciler patches TektonConfig to ensure proper fsGroup handling in Shipwright build pods with PVCs.
type TektonConfigReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=operator.tekton.dev,resources=tektonconfigs,verbs=get;list;watch;patch

func (r *TektonConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling TektonConfig", "name", req.Name)

	tc := &tekton.TektonConfig{}
	if err := r.Get(ctx, req.NamespacedName, tc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	needsPatch := false
	patch := tc.DeepCopy()

	if tc.Spec.Pipeline.SetSecurityContext == nil || !*tc.Spec.Pipeline.SetSecurityContext {
		patch.Spec.Pipeline.SetSecurityContext = ptr.To(true)
		needsPatch = true
	}

	if tc.Spec.Platforms.OpenShift.SCC.Default == "" {
		patch.Spec.Platforms.OpenShift.SCC.Default = "pipelines-scc"
		needsPatch = true
	}

	// Ensure spec.pruner.resources is populated. Newer Tekton versions require this field;
	// without it the webhook rejects TektonConfig with "missing field(s): spec.pruner.resources".
	if len(tc.Spec.Pruner.Resources) == 0 {
		patch.Spec.Pruner = tekton.TektonPruner{
			Resources: []string{"taskrun", "pipelinerun"},
			Keep:      ptr.To(100),
			Schedule:  "0 8 * * *",
		}
		needsPatch = true
	}

	if !needsPatch {
		logger.V(1).Info("TektonConfig already configured correctly", "name", req.Name)
		return ctrl.Result{}, nil
	}

	if err := r.Patch(ctx, patch, client.MergeFrom(tc)); err != nil {
		logger.Error(err, "Failed to patch TektonConfig")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully patched TektonConfig", "name", req.Name)
	return ctrl.Result{}, nil
}

func (r *TektonConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tekton.TektonConfig{}).
		Named("TektonConfig").
		Complete(r)
}

func TektonConfigCRDExists(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		tektonConfigLogger.Error(err, "Failed to create discovery client for TektonConfig check - controller will not start")
		return false
	}

	for attempt := range 3 {
		if attempt > 0 {
			delay := time.Duration(attempt) * 5 * time.Second
			tektonConfigLogger.Info("Retrying TektonConfig CRD discovery", "attempt", attempt+1, "delay", delay)
			time.Sleep(delay)
		}

		resources, err := dc.ServerResourcesForGroupVersion("operator.tekton.dev/v1alpha1")
		if err != nil {
			tektonConfigLogger.Info("TektonConfig CRD not found", "attempt", attempt+1, "error", err)
			continue
		}

		for _, r := range resources.APIResources {
			if r.Kind == "TektonConfig" {
				tektonConfigLogger.Info("TektonConfig CRD detected: will manage Tekton pipeline security settings")
				return true
			}
		}

		tektonConfigLogger.Info("operator.tekton.dev/v1alpha1 group exists but TektonConfig kind not found")
		return false
	}

	tektonConfigLogger.Info("TektonConfig CRD not found after retries: not on OpenShift or Tekton not installed")
	return false
}
