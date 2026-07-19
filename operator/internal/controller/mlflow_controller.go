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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rossoctl/operator/internal/mlflow"
)

const (
	// MLflowExperimentsAPIGroup is the API group used by the MLflow gateway's
	// Kubernetes authorization plugin for SubjectAccessReview checks.
	MLflowExperimentsAPIGroup = "mlflow.kubeflow.org"

	// MLflowExperimentsResource is the virtual resource the MLflow gateway checks
	// when authorizing experiment-level operations via SubjectAccessReview.
	MLflowExperimentsResource = "experiments"

	// MLflow annotation keys stored on the PodTemplateSpec.
	AnnotationMLflowExperimentID   = "mlflow.rossoctl.io/experiment-id"
	AnnotationMLflowExperimentName = "mlflow.rossoctl.io/experiment-name"
	AnnotationMLflowTrackingURI    = "mlflow.rossoctl.io/tracking-uri"
	AnnotationMLflowTrackingAuth   = "mlflow.rossoctl.io/tracking-auth"
)

// MLflowReconciler reconciles Deployments labelled rossoctl.io/type=agent.
// It auto-discovers MLflow availability via the mlflows.mlflow.opendatahub.io CRD.
//
// For each agent Deployment the reconciler creates a scoped Role granting only
// get+update on the agent's specific experiment (via resourceNames), and a
// RoleBinding for the agent SA. This ensures agents cannot access other
// experiments, registered models, or gateway endpoints.
type MLflowReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// MLflowCAFile is the path to a PEM-encoded CA bundle for verifying the
	// MLflow gateway TLS certificate. When set, the CA is appended to the
	// system cert pool. Required on clusters where the MLflow Route uses a
	// non-publicly-trusted certificate (e.g., HyperShift ingress CA).
	MLflowCAFile string

	// NewMLflowClient creates an MLflow client for the given base URL.
	// If nil, a default client is used.
	NewMLflowClient func(baseURL string) *mlflow.Client

	// ResolveTrackingURI overrides the default CRD auto-discovery for the
	// MLflow tracking URI. Primarily used for testing.
	ResolveTrackingURI func(ctx context.Context) string
}

// +kubebuilder:rbac:groups=mlflow.opendatahub.io,resources=mlflows,verbs=create;get;list;update;watch
// +kubebuilder:rbac:groups=mlflow.kubeflow.org,resources=experiments,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;get;list;watch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;get;list;watch;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *MLflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling MLflow for Deployment", "namespacedName", req.NamespacedName)

	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	labels := dep.GetLabels()
	if labels == nil || labels[LabelAgentType] != LabelValueAgent {
		return ctrl.Result{}, nil
	}

	if !dep.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	trackingURI := r.trackingURI(ctx)
	if trackingURI == "" {
		logger.V(1).Info("MLflow not available, skipping")
		return ctrl.Result{}, nil
	}

	experimentName := dep.Name

	annotations := dep.Spec.Template.Annotations
	if annotations != nil &&
		annotations[AnnotationMLflowExperimentID] != "" &&
		annotations[AnnotationMLflowExperimentName] == experimentName &&
		annotations[AnnotationMLflowTrackingURI] == trackingURI {
		logger.V(1).Info("MLflow already configured, no-op")
		return ctrl.Result{}, nil
	}

	mlflowClient := r.mlflowClient(trackingURI)
	experimentID, err := mlflowClient.CreateExperiment(ctx, experimentName, dep.Namespace)
	if err != nil {
		logger.Error(err, "Failed to create/get MLflow experiment", "name", experimentName)
		if r.Recorder != nil {
			r.Recorder.Eventf(dep, nil, corev1.EventTypeWarning, "MLflowExperimentFailed",
				"CreateExperiment", "Failed to create MLflow experiment %q: %v", experimentName, err)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("MLflow experiment ready", "name", experimentName, "id", experimentID)

	saName := dep.Spec.Template.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
		logger.Info("deployment has no explicit serviceAccountName, falling back to 'default'", "deployment", dep.Name)
	}

	if err := r.ensureScopedRBAC(ctx, dep, saName, experimentName); err != nil {
		logger.Error(err, "Failed to ensure scoped MLflow RBAC")
		return ctrl.Result{}, err
	}

	if err := r.configureDeployment(ctx, dep, trackingURI, experimentID, experimentName); err != nil {
		logger.Error(err, "Failed to configure Deployment with MLflow")
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(dep, nil, corev1.EventTypeNormal, "MLflowConfigured",
			"ConfigureMLflow", "Experiment %q (ID: %s) provisioned, RoleBinding created for SA %s",
			experimentName, experimentID, saName)
	}

	return ctrl.Result{}, nil
}

func (r *MLflowReconciler) mlflowClient(baseURL string) *mlflow.Client {
	if r.NewMLflowClient != nil {
		return r.NewMLflowClient(baseURL)
	}
	return &mlflow.Client{BaseURL: baseURL, CAFile: r.MLflowCAFile}
}

// trackingURI returns the MLflow tracking URI, using the override if set.
func (r *MLflowReconciler) trackingURI(ctx context.Context) string {
	if r.ResolveTrackingURI != nil {
		return r.ResolveTrackingURI(ctx)
	}
	return r.resolveTrackingURI(ctx)
}

// resolveTrackingURI discovers the MLflow tracking URI via the
// mlflows.mlflow.opendatahub.io CRD.
func (r *MLflowReconciler) resolveTrackingURI(ctx context.Context) string {
	logger := log.FromContext(ctx)

	list := &mlflow.MLflowList{}
	if err := r.List(ctx, list); err != nil {
		logger.V(2).Info("mlflows.mlflow.opendatahub.io CRD not available", "error", err)
		return ""
	}

	for i := range list.Items {
		cr := &list.Items[i]
		if meta.IsStatusConditionTrue(cr.Status.Conditions, "Available") {
			if cr.Status.URL != "" {
				logger.V(1).Info("Auto-discovered MLflow gateway URL", "uri", cr.Status.URL, "cr", cr.GetName())
				return cr.Status.URL
			}
			logger.Info("MLflow CR is Available but status.url is not set, skipping", "cr", cr.GetName())
		}
	}

	return ""
}

// mlflowEnvVars returns the environment variables to inject into agent containers.
// The tracking URI is typically the external gateway URL which uses a publicly-trusted
// TLS certificate, so no custom CA cert path is needed.
func mlflowEnvVars(trackingURI, experimentID, experimentName string) map[string]string {
	return map[string]string{
		"MLFLOW_TRACKING_URI":    trackingURI,
		"MLFLOW_TRACKING_AUTH":   "kubernetes-namespaced",
		"MLFLOW_EXPERIMENT_ID":   experimentID,
		"MLFLOW_EXPERIMENT_NAME": experimentName,
	}
}

// configureDeployment sets annotations and injects MLflow env vars into the Deployment.
func (r *MLflowReconciler) configureDeployment(ctx context.Context, dep *appsv1.Deployment, trackingURI, experimentID, experimentName string) error {
	logger := log.FromContext(ctx)

	desired := mlflowEnvVars(trackingURI, experimentID, experimentName)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, latest); err != nil {
			return err
		}

		annotations := latest.Spec.Template.Annotations
		if annotations == nil {
			annotations = make(map[string]string)
		}

		annotationsChanged := false
		for k, v := range map[string]string{
			AnnotationMLflowExperimentID:   experimentID,
			AnnotationMLflowExperimentName: experimentName,
			AnnotationMLflowTrackingURI:    trackingURI,
			AnnotationMLflowTrackingAuth:   "kubernetes-namespaced",
		} {
			if annotations[k] != v {
				annotations[k] = v
				annotationsChanged = true
			}
		}
		latest.Spec.Template.Annotations = annotations

		envChanged := false
		for i := range latest.Spec.Template.Spec.Containers {
			for name, value := range desired {
				if setEnvVar(&latest.Spec.Template.Spec.Containers[i], name, value) {
					envChanged = true
				}
			}
		}

		if !envChanged && !annotationsChanged {
			return nil
		}

		if envChanged {
			logger.Info("Injected MLflow env vars into Deployment containers", "deployment", dep.Name)
		}

		return r.Update(ctx, latest)
	})
}

// ensureScopedRBAC creates a Role scoped to a single experiment (by resourceName)
// and a RoleBinding that grants the agent SA only get+update on that experiment.
// Both resources are owned by the Deployment so they are garbage-collected on deletion.
func (r *MLflowReconciler) ensureScopedRBAC(ctx context.Context, dep *appsv1.Deployment, saName, experimentName string) error {
	roleName := fmt.Sprintf("rossoctl-mlflow-%s", dep.Name)

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: dep.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = map[string]string{
			LabelManagedBy: LabelManagedByValue,
		}
		if err := controllerutil.SetOwnerReference(dep, role, r.Scheme); err != nil {
			return err
		}
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{MLflowExperimentsAPIGroup},
				Resources:     []string{MLflowExperimentsResource},
				ResourceNames: []string{experimentName},
				Verbs:         []string{"get", "update"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring scoped Role: %w", err)
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: dep.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = map[string]string{
			LabelManagedBy: LabelManagedByValue,
		}
		if err := controllerutil.SetOwnerReference(dep, rb, r.Scheme); err != nil {
			return err
		}
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     roleName,
		}
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      saName,
				Namespace: dep.Namespace,
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring scoped RoleBinding: %w", err)
	}

	return nil
}

// setEnvVar sets an env var on a container, returning true if a change was made.
func setEnvVar(container *corev1.Container, name, value string) bool {
	for i := range container.Env {
		if container.Env[i].Name == name {
			if container.Env[i].Value == value {
				return false
			}
			container.Env[i].Value = value
			container.Env[i].ValueFrom = nil
			return true
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
	return true
}

// SetupWithManager registers the MLflow controller with the manager.
func (r *MLflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(agentLabelPredicate())).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Named("mlflow").
		Complete(r)
}
