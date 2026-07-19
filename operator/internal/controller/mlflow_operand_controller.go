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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/rossoctl/operator/internal/mlflow"
)

const (
	// MLflowOperandNamespace is the namespace where RHOAI MLflow CRs live.
	MLflowOperandNamespace = "redhat-ods-applications"

	// MLflowOperandName is the default name for the operator-managed MLflow CR.
	MLflowOperandName = "mlflow"

	// OTELCollectorRoleBindingName is the name used for the per-namespace
	// RoleBinding granting the otel-collector SA access to MLflow.
	OTELCollectorRoleBindingName = "otel-collector-mlflow"

	// OTELCollectorSAName is the ServiceAccount used by the OTEL collector.
	OTELCollectorSAName = "otel-collector"

	// DSCName is the well-known name of the singleton DataScienceCluster.
	DSCName = "default-dsc"

	// MLflowClusterRoleIntegration is the ClusterRole created by the MLflow operator.
	MLflowClusterRoleIntegration = "mlflow-operator-mlflow-integration"

	// MLflowClusterRoleEdit is the newer ClusterRole name; lacks
	// gatewayendpoints/use needed for OTLP trace ingestion.
	MLflowClusterRoleEdit = "mlflow-operator-mlflow-edit"

	// requeueMLflowNotReady is the requeue delay when MLflow is not yet available.
	requeueMLflowNotReady = 30 * time.Second
)

// MLflowOperandReconciler watches DataScienceCluster resources and ensures:
//  1. An MLflow CR exists in redhat-ods-applications when the DSC has mlflowoperator: Managed
//  2. Per-agent-namespace RoleBindings exist for the otel-collector SA
//
// It is registered under the same --enable-mlflow gate as MLflowReconciler.
type MLflowOperandReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the namespace the operator runs in (default: rossoctl-system).
	OperatorNamespace string

	// ResolveMLflowClusterRole overrides ClusterRole discovery for testing.
	ResolveMLflowClusterRole func(ctx context.Context) string
}

// +kubebuilder:rbac:groups=datasciencecluster.opendatahub.io,resources=datascienceclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=mlflow.opendatahub.io,resources=mlflows,verbs=create;get;list;update;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;delete;get;list;watch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=mlflow-operator-mlflow-integration;mlflow-operator-mlflow-edit,verbs=bind
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

func (r *MLflowOperandReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling MLflow operand", "name", req.Name)

	dsc := &mlflow.DataScienceCluster{}
	if err := r.Get(ctx, req.NamespacedName, dsc); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("DataScienceCluster not found, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if dsc.Spec.Components.MLflowOperator.ManagementState != "Managed" {
		logger.V(1).Info("DSC mlflowoperator is not Managed, skipping",
			"state", dsc.Spec.Components.MLflowOperator.ManagementState)
		return ctrl.Result{}, nil
	}

	if result, err := r.ensureMLflowCR(ctx); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	if result, err := r.waitForMLflowReady(ctx); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	if err := r.ensureOTELRoleBindings(ctx); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("MLflow operand reconciliation complete")
	return ctrl.Result{}, nil
}

// ensureMLflowCR creates the MLflow CR in redhat-ods-applications if absent.
func (r *MLflowOperandReconciler) ensureMLflowCR(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	existing := &mlflow.MLflow{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      MLflowOperandName,
		Namespace: MLflowOperandNamespace,
	}, existing)

	if err == nil {
		logger.V(1).Info("MLflow CR already exists", "name", MLflowOperandName, "namespace", MLflowOperandNamespace)
		return ctrl.Result{}, nil
	}
	if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("checking MLflow CR: %w", err)
	}

	cr := &mlflow.MLflow{
		TypeMeta: metav1.TypeMeta{
			APIVersion: mlflow.SchemeGroupVersion.String(),
			Kind:       "MLflow",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MLflowOperandName,
			Namespace: MLflowOperandNamespace,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
		Spec: mlflow.MLflowSpec{
			Storage: &mlflow.MLflowStorage{
				AccessModes: []string{"ReadWriteOnce"},
				Resources: &mlflow.MLflowStorageResourceReqs{
					Requests: map[string]string{
						"storage": "10Gi",
					},
				},
			},
			BackendStoreURI:      "sqlite:////mlflow/mlflow.db",
			ArtifactsDestination: "file:///mlflow/artifacts",
			ServeArtifacts:       true,
		},
	}

	if err := r.Create(ctx, cr); err != nil {
		if errors.IsAlreadyExists(err) {
			logger.V(1).Info("MLflow CR was created concurrently")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating MLflow CR: %w", err)
	}

	logger.Info("Created MLflow CR", "name", MLflowOperandName, "namespace", MLflowOperandNamespace)
	if r.Recorder != nil {
		r.Recorder.Event(cr, "Normal", "MLflowCRCreated",
			fmt.Sprintf("Created MLflow CR %s/%s", MLflowOperandNamespace, MLflowOperandName))
	}

	return ctrl.Result{RequeueAfter: requeueMLflowNotReady}, nil
}

// waitForMLflowReady checks that the MLflow Service has at least one ready endpoint.
func (r *MLflowOperandReconciler) waitForMLflowReady(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      MLflowOperandName,
		Namespace: MLflowOperandNamespace,
	}, svc)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("MLflow Service not yet created, requeueing")
			return ctrl.Result{RequeueAfter: requeueMLflowNotReady}, nil
		}
		return ctrl.Result{}, fmt.Errorf("checking MLflow Service: %w", err)
	}

	epSlices := &discoveryv1.EndpointSliceList{}
	err = r.List(ctx, epSlices,
		client.InNamespace(MLflowOperandNamespace),
		client.MatchingLabels{"kubernetes.io/service-name": MLflowOperandName},
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing MLflow EndpointSlices: %w", err)
	}

	for i := range epSlices.Items {
		for j := range epSlices.Items[i].Endpoints {
			if epSlices.Items[i].Endpoints[j].Conditions.Ready != nil &&
				*epSlices.Items[i].Endpoints[j].Conditions.Ready {
				logger.V(1).Info("MLflow Service has ready endpoints")
				return ctrl.Result{}, nil
			}
		}
	}

	logger.V(1).Info("MLflow Service has no ready endpoints, requeueing")
	return ctrl.Result{RequeueAfter: requeueMLflowNotReady}, nil
}

// ensureOTELRoleBindings creates a RoleBinding in every namespace that contains
// an agent Deployment, granting the otel-collector SA access to the MLflow
// ClusterRole for trace export.
func (r *MLflowOperandReconciler) ensureOTELRoleBindings(ctx context.Context) error {
	logger := log.FromContext(ctx)

	clusterRole := r.resolveMLflowClusterRole(ctx)
	if clusterRole == "" {
		logger.Info("No MLflow ClusterRole found, skipping OTEL RoleBindings")
		return nil
	}

	namespaces, err := r.agentNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("listing agent namespaces: %w", err)
	}

	operatorNS := r.operatorNamespace()

	// Also ensure RoleBinding in the MLflow namespace itself.
	allNamespaces := append(namespaces, MLflowOperandNamespace)

	for _, ns := range allNamespaces {
		if err := r.ensureOTELRoleBinding(ctx, ns, operatorNS, clusterRole); err != nil {
			return err
		}
	}

	logger.V(1).Info("OTEL RoleBindings ensured", "namespaces", allNamespaces, "clusterRole", clusterRole)
	return nil
}

func (r *MLflowOperandReconciler) ensureOTELRoleBinding(ctx context.Context, namespace, operatorNS, clusterRole string) error {
	desired := rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "ClusterRole",
		Name:     clusterRole,
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OTELCollectorRoleBindingName,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = map[string]string{
			LabelManagedBy: LabelManagedByValue,
		}
		rb.RoleRef = desired
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      OTELCollectorSAName,
				Namespace: operatorNS,
			},
		}
		return nil
	})

	if err != nil && strings.Contains(err.Error(), "cannot change roleRef") {
		logger := log.FromContext(ctx)
		logger.Info("RoleBinding roleRef mismatch, recreating",
			"namespace", namespace, "desiredClusterRole", clusterRole)
		if delErr := r.Delete(ctx, rb); delErr != nil {
			return fmt.Errorf("deleting stale OTEL RoleBinding in %s: %w", namespace, delErr)
		}
		rb = &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      OTELCollectorRoleBindingName,
				Namespace: namespace,
				Labels:    map[string]string{LabelManagedBy: LabelManagedByValue},
			},
			RoleRef: desired,
			Subjects: []rbacv1.Subject{
				{
					Kind:      rbacv1.ServiceAccountKind,
					Name:      OTELCollectorSAName,
					Namespace: operatorNS,
				},
			},
		}
		if createErr := r.Create(ctx, rb); createErr != nil {
			return fmt.Errorf("recreating OTEL RoleBinding in %s: %w", namespace, createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("ensuring OTEL RoleBinding in %s: %w", namespace, err)
	}
	return nil
}

// resolveMLflowClusterRole finds the MLflow ClusterRole, preferring
// "integration" which includes the gatewayendpoints/use verb required for
// OTLP trace ingestion.
func (r *MLflowOperandReconciler) resolveMLflowClusterRole(ctx context.Context) string {
	if r.ResolveMLflowClusterRole != nil {
		return r.ResolveMLflowClusterRole(ctx)
	}

	for _, name := range []string{MLflowClusterRoleIntegration, MLflowClusterRoleEdit} {
		cr := &rbacv1.ClusterRole{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, cr); err == nil {
			return name
		}
	}
	return ""
}

// agentNamespaces returns the set of namespaces containing at least one
// Deployment or StatefulSet with the rossoctl.io/type=agent label.
func (r *MLflowOperandReconciler) agentNamespaces(ctx context.Context) ([]string, error) {
	agentLabel := client.MatchingLabels{LabelAgentType: LabelValueAgent}
	seen := make(map[string]struct{})
	var namespaces []string

	addNamespace := func(ns string) {
		if _, ok := seen[ns]; !ok {
			seen[ns] = struct{}{}
			namespaces = append(namespaces, ns)
		}
	}

	depList := &appsv1.DeploymentList{}
	if err := r.List(ctx, depList, agentLabel); err != nil {
		return nil, err
	}
	for i := range depList.Items {
		addNamespace(depList.Items[i].Namespace)
	}

	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList, agentLabel); err != nil {
		return nil, err
	}
	for i := range stsList.Items {
		addNamespace(stsList.Items[i].Namespace)
	}

	return namespaces, nil
}

func (r *MLflowOperandReconciler) operatorNamespace() string {
	if r.OperatorNamespace != "" {
		return r.OperatorNamespace
	}
	return "rossoctl-system"
}

// dscMLflowChangePredicate fires only when the DSC mlflowoperator managementState changes.
func dscMLflowChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldDSC, ok1 := e.ObjectOld.(*mlflow.DataScienceCluster)
			newDSC, ok2 := e.ObjectNew.(*mlflow.DataScienceCluster)
			if !ok1 || !ok2 {
				return true
			}
			return oldDSC.Spec.Components.MLflowOperator.ManagementState !=
				newDSC.Spec.Components.MLflowOperator.ManagementState
		},
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return true },
	}
}

// SetupWithManager registers the MLflow operand controller.
// It watches DataScienceCluster as the primary resource and enqueues
// reconciliation when agent Deployments appear (to create OTEL RoleBindings
// in new namespaces).
func (r *MLflowOperandReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Check if the DataScienceCluster CRD is registered. If RHOAI is not
	// installed, this controller has nothing to reconcile.
	_, err := mgr.GetRESTMapper().RESTMapping(
		mlflow.DSCSchemeGroupVersion.WithKind("DataScienceCluster").GroupKind(),
		mlflow.DSCSchemeGroupVersion.Version,
	)
	if err != nil {
		if meta.IsNoMatchError(err) {
			log.Log.Info("DataScienceCluster CRD not registered, MLflow operand controller will not start")
			return nil
		}
		return fmt.Errorf("checking DataScienceCluster CRD: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mlflow.DataScienceCluster{}, builder.WithPredicates(dscMLflowChangePredicate())).
		// Re-reconcile when new agent Deployments appear so we can create
		// OTEL RoleBindings in their namespaces.
		Watches(&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.mapAgentDeploymentToDSC),
			builder.WithPredicates(agentLabelPredicate()),
		).
		Named("mlflow-operand").
		Complete(r)
}

// mapAgentDeploymentToDSC maps any agent Deployment event to a reconcile
// request for the singleton DataScienceCluster so the operand controller
// can evaluate whether OTEL RoleBindings need creating.
func (r *MLflowOperandReconciler) mapAgentDeploymentToDSC(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: DSCName}},
	}
}
