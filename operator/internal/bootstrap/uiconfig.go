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

package bootstrap

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rossoctl/operator/internal/mlflow"
)

const (
	uiConfigMapName       = "rossoctl-ui-config"
	mlflowDashboardURLKey = "MLFLOW_DASHBOARD_URL"
)

// UIConfigBootstrapRunnable patches the rossoctl-ui-config ConfigMap with the
// MLflow dashboard URL. Runs once at startup; skips gracefully if MLflow or
// the UI ConfigMap are not present.
type UIConfigBootstrapRunnable struct {
	Client    client.Client
	APIReader client.Reader
	Namespace string
	Log       logr.Logger
}

func (r *UIConfigBootstrapRunnable) Start(ctx context.Context) error {
	log := r.Log.WithName("ui-config-bootstrap")

	dashboardURL := r.discoverDashboardURL(ctx, log)
	if dashboardURL == "" {
		return nil
	}

	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: uiConfigMapName, Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, cm); err != nil {
		if errors.IsNotFound(err) {
			log.V(1).Info("UI ConfigMap not found, skipping (UI likely not deployed)")
			return nil
		}
		log.Error(err, "Failed to read UI ConfigMap")
		return nil
	}

	if cm.Data != nil && cm.Data[mlflowDashboardURLKey] == dashboardURL {
		log.V(1).Info("MLflow dashboard URL already set")
		return nil
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[mlflowDashboardURLKey] = dashboardURL
	if err := r.Client.Update(ctx, cm); err != nil {
		log.Error(err, "Failed to patch UI ConfigMap with MLflow dashboard URL")
		return nil
	}

	log.Info("Patched UI ConfigMap with MLflow dashboard URL", "url", dashboardURL)
	return nil
}

func (r *UIConfigBootstrapRunnable) NeedLeaderElection() bool {
	return true
}

func (r *UIConfigBootstrapRunnable) discoverDashboardURL(ctx context.Context, log logr.Logger) string {
	list := &mlflow.MLflowList{}
	if err := r.APIReader.List(ctx, list); err != nil {
		log.V(1).Info("Could not list MLflow CRs, skipping", "error", err)
		return ""
	}

	for i := range list.Items {
		cr := &list.Items[i]
		if meta.IsStatusConditionTrue(cr.Status.Conditions, "Available") && cr.Status.URL != "" {
			return strings.TrimRight(cr.Status.URL, "/") + "/"
		}
	}

	log.V(1).Info("No available MLflow CR with external URL found")
	return ""
}
