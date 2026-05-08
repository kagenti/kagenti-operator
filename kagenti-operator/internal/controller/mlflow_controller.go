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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kagenti/operator/internal/mlflow"
)

const (
	// DefaultMLflowClusterRole is the ClusterRole managed by the MLflow operator
	// for agent access to MLflow resources (RHOAI 3.4+).
	DefaultMLflowClusterRole = "mlflow-operator-mlflow-integration"

	// MLflow annotation keys stored on the PodTemplateSpec.
	AnnotationMLflowExperimentID   = "mlflow.kagenti.io/experiment-id"
	AnnotationMLflowExperimentName = "mlflow.kagenti.io/experiment-name"
	AnnotationMLflowTrackingURI    = "mlflow.kagenti.io/tracking-uri"
	AnnotationMLflowTrackingAuth   = "mlflow.kagenti.io/tracking-auth"
)

// MLflowReconciler reconciles Deployments labelled kagenti.io/type=agent.
// It auto-discovers MLflow availability via the mlflows.mlflow.opendatahub.io CRD.
type MLflowReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MLflowClusterRole is the ClusterRole to bind agent SAs to.
	// Defaults to DefaultMLflowClusterRole if empty.
	MLflowClusterRole string

	// NewMLflowClient creates an MLflow client for the given base URL.
	// If nil, a default client is used.
	NewMLflowClient func(baseURL string) *mlflow.Client

	// ResolveTrackingURI overrides the default CRD auto-discovery for the
	// MLflow tracking URI. Primarily used for testing.
	ResolveTrackingURI func(ctx context.Context) string
}

// +kubebuilder:rbac:groups=mlflow.opendatahub.io,resources=mlflows,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;get;list;watch;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;update;patch

func (r *MLflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling MLflow", "namespacedName", req.NamespacedName)

	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, dep); err == nil {
		return r.reconcileWorkload(ctx, dep, dep.GetLabels(),
			dep.Spec.Template.Annotations, dep.Spec.Template.Spec.ServiceAccountName, dep.Name,
			func(ctx context.Context, trackingURI, experimentID, experimentName string) error {
				return r.configureDeployment(ctx, dep, trackingURI, experimentID, experimentName)
			})
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, req.NamespacedName, sts); err == nil {
		return r.reconcileWorkload(ctx, sts, sts.GetLabels(),
			sts.Spec.Template.Annotations, sts.Spec.Template.Spec.ServiceAccountName, sts.Name,
			func(ctx context.Context, trackingURI, experimentID, experimentName string) error {
				return r.configureStatefulSet(ctx, sts, trackingURI, experimentID, experimentName)
			})
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	sbx := &unstructured.Unstructured{}
	sbx.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(ctx, req.NamespacedName, sbx); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	annotations, _, _ := unstructured.NestedStringMap(sbx.Object, "spec", "podTemplate", "metadata", "annotations")
	saName, _, _ := unstructured.NestedString(sbx.Object, "spec", "podTemplate", "spec", "serviceAccountName")
	return r.reconcileWorkload(ctx, sbx, sbx.GetLabels(),
		annotations, saName, sbx.GetName(),
		func(ctx context.Context, trackingURI, experimentID, experimentName string) error {
			return r.configureSandbox(ctx, sbx, trackingURI, experimentID, experimentName)
		})
}

func (r *MLflowReconciler) reconcileWorkload(
	ctx context.Context,
	obj client.Object,
	labels map[string]string,
	annotations map[string]string,
	serviceAccountName string,
	workloadName string,
	configure func(ctx context.Context, trackingURI, experimentID, experimentName string) error,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if labels == nil || labels[LabelAgentType] != LabelValueAgent {
		return ctrl.Result{}, nil
	}

	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	trackingURI := r.trackingURI(ctx)
	if trackingURI == "" {
		logger.V(1).Info("MLflow not available, skipping")
		return ctrl.Result{}, nil
	}

	experimentName := workloadName

	if annotations != nil &&
		annotations[AnnotationMLflowExperimentID] != "" &&
		annotations[AnnotationMLflowExperimentName] == experimentName &&
		annotations[AnnotationMLflowTrackingURI] == trackingURI {
		logger.V(1).Info("MLflow already configured, no-op")
		return ctrl.Result{}, nil
	}

	mlflowClient := r.mlflowClient(trackingURI)
	experimentID, err := mlflowClient.CreateExperiment(ctx, experimentName, obj.GetNamespace())
	if err != nil {
		logger.Error(err, "Failed to create/get MLflow experiment", "name", experimentName)
		if r.Recorder != nil {
			r.Recorder.Eventf(obj, "Warning", "MLflowExperimentFailed",
				"Failed to create MLflow experiment %q: %v", experimentName, err)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("MLflow experiment ready", "name", experimentName, "id", experimentID)

	saName := serviceAccountName
	if saName == "" {
		saName = "default"
		logger.Info("workload has no explicit serviceAccountName, falling back to 'default'", "workload", workloadName)
	}

	if err := r.ensureRoleBinding(ctx, obj, saName); err != nil {
		logger.Error(err, "Failed to ensure MLflow RoleBinding")
		return ctrl.Result{}, err
	}

	if err := configure(ctx, trackingURI, experimentID, experimentName); err != nil {
		logger.Error(err, "Failed to configure workload with MLflow")
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(obj, "Normal", "MLflowConfigured",
			"Experiment %q (ID: %s) provisioned, RoleBinding created for SA %s",
			experimentName, experimentID, saName)
	}

	return ctrl.Result{}, nil
}

func (r *MLflowReconciler) clusterRoleName() string {
	if r.MLflowClusterRole != "" {
		return r.MLflowClusterRole
	}
	return DefaultMLflowClusterRole
}

func (r *MLflowReconciler) mlflowClient(baseURL string) *mlflow.Client {
	if r.NewMLflowClient != nil {
		return r.NewMLflowClient(baseURL)
	}
	return &mlflow.Client{BaseURL: baseURL}
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
		annotations[AnnotationMLflowExperimentID] = experimentID
		annotations[AnnotationMLflowExperimentName] = experimentName
		annotations[AnnotationMLflowTrackingURI] = trackingURI
		annotations[AnnotationMLflowTrackingAuth] = "kubernetes-namespaced"
		latest.Spec.Template.Annotations = annotations

		changed := false
		for i := range latest.Spec.Template.Spec.Containers {
			for name, value := range desired {
				if setEnvVar(&latest.Spec.Template.Spec.Containers[i], name, value) {
					changed = true
				}
			}
		}

		if changed {
			logger.Info("Injected MLflow env vars into Deployment containers", "deployment", dep.Name)
		}

		return r.Update(ctx, latest)
	})
}

// ensureRoleBinding creates or updates the RoleBinding for the agent SA.
func (r *MLflowReconciler) ensureRoleBinding(ctx context.Context, owner client.Object, saName string) error {
	rbName := fmt.Sprintf("kagenti-mlflow-%s", owner.GetName())
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbName,
			Namespace: owner.GetNamespace(),
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = map[string]string{
			LabelManagedBy: LabelManagedByValue,
		}

		if err := controllerutil.SetOwnerReference(owner, rb, r.Scheme); err != nil {
			return err
		}

		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     r.clusterRoleName(),
		}
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      saName,
				Namespace: owner.GetNamespace(),
			},
		}
		return nil
	})
	return err
}

// configureStatefulSet sets annotations and injects MLflow env vars into the StatefulSet.
func (r *MLflowReconciler) configureStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, trackingURI, experimentID, experimentName string) error {
	logger := log.FromContext(ctx)

	desired := mlflowEnvVars(trackingURI, experimentID, experimentName)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, latest); err != nil {
			return err
		}

		annotations := latest.Spec.Template.Annotations
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[AnnotationMLflowExperimentID] = experimentID
		annotations[AnnotationMLflowExperimentName] = experimentName
		annotations[AnnotationMLflowTrackingURI] = trackingURI
		annotations[AnnotationMLflowTrackingAuth] = "kubernetes-namespaced"
		latest.Spec.Template.Annotations = annotations

		changed := false
		for i := range latest.Spec.Template.Spec.Containers {
			for name, value := range desired {
				if setEnvVar(&latest.Spec.Template.Spec.Containers[i], name, value) {
					changed = true
				}
			}
		}

		if changed {
			logger.Info("Injected MLflow env vars into StatefulSet containers", "statefulset", sts.Name)
		}

		return r.Update(ctx, latest)
	})
}

// configureSandbox sets annotations and injects MLflow env vars into a Sandbox resource.
func (r *MLflowReconciler) configureSandbox(ctx context.Context, sbx *unstructured.Unstructured, trackingURI, experimentID, experimentName string) error {
	logger := log.FromContext(ctx)

	desired := mlflowEnvVars(trackingURI, experimentID, experimentName)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(sandboxGVK)
		if err := r.Get(ctx, types.NamespacedName{Name: sbx.GetName(), Namespace: sbx.GetNamespace()}, latest); err != nil {
			return err
		}

		annotations, _, _ := unstructured.NestedStringMap(latest.Object, "spec", "podTemplate", "metadata", "annotations")
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[AnnotationMLflowExperimentID] = experimentID
		annotations[AnnotationMLflowExperimentName] = experimentName
		annotations[AnnotationMLflowTrackingURI] = trackingURI
		annotations[AnnotationMLflowTrackingAuth] = "kubernetes-namespaced"
		if err := unstructured.SetNestedStringMap(latest.Object, annotations, "spec", "podTemplate", "metadata", "annotations"); err != nil {
			return fmt.Errorf("failed to set annotations on Sandbox: %w", err)
		}

		containers, _, _ := unstructured.NestedSlice(latest.Object, "spec", "podTemplate", "spec", "containers")
		changed := false
		for i, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			envSlice, _, _ := unstructured.NestedSlice(container, "env")
			for name, value := range desired {
				envSlice, _ = setUnstructuredEnvVar(envSlice, name, value)
				changed = true
			}
			container["env"] = envSlice
			containers[i] = container
		}
		if changed {
			if err := unstructured.SetNestedSlice(latest.Object, containers, "spec", "podTemplate", "spec", "containers"); err != nil {
				return fmt.Errorf("failed to set containers on Sandbox: %w", err)
			}
			logger.Info("Injected MLflow env vars into Sandbox containers", "sandbox", sbx.GetName())
		}

		return r.Update(ctx, latest)
	})
}

// setUnstructuredEnvVar sets or adds an environment variable in an unstructured env slice.
func setUnstructuredEnvVar(envSlice []interface{}, name, value string) ([]interface{}, bool) {
	for i, e := range envSlice {
		envVar, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if envVar["name"] == name {
			if envVar["value"] == value {
				return envSlice, false
			}
			envVar["value"] = value
			delete(envVar, "valueFrom")
			envSlice[i] = envVar
			return envSlice, true
		}
	}
	envSlice = append(envSlice, map[string]interface{}{
		"name":  name,
		"value": value,
	})
	return envSlice, true
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
	b := ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(agentLabelPredicate())).
		Watches(
			&appsv1.StatefulSet{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(agentLabelPredicate()),
		).
		Owns(&rbacv1.RoleBinding{})

	if SandboxCRDExists(mgr.GetConfig()) {
		sandboxObj := &unstructured.Unstructured{}
		sandboxObj.SetGroupVersionKind(sandboxGVK)
		b = b.Watches(
			sandboxObj,
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(agentLabelPredicate()),
		)
	}

	return b.Named("mlflow").Complete(r)
}
