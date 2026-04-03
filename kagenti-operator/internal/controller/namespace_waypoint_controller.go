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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// Labels for namespace Istio ambient mesh configuration
	IstioDiscoveryLabel     = "istio-discovery"
	IstioDataplaneModeLabel = "istio.io/dataplane-mode"
	IstioUseWaypointLabel   = "istio.io/use-waypoint"
	IstioWaypointForLabel   = "istio.io/waypoint-for"

	// Label values
	IstioDiscoveryEnabled     = "enabled"
	IstioDataplaneModeAmbient = "ambient"
	IstioWaypointForAll       = "all"

	// Kagenti workload type label
	KagentiTypeLabel = "kagenti.io/type"
	KagentiTypeAgent = "agent"
	KagentiTypeTool  = "tool"

	// GatewayClass for Istio waypoint
	IstioWaypointGatewayClass = "istio-waypoint"

	// Waypoint name suffix
	WaypointNameSuffix = "-waypoint"
)

// NamespaceWaypointReconciler watches namespaces and ensures waypoint configuration
// for namespaces containing Kagenti agents or tools.
type NamespaceWaypointReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// EnableWaypointProvisioning controls whether waypoint gateways are automatically created
	EnableWaypointProvisioning bool
}

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures namespace waypoint configuration for namespaces with Kagenti workloads.
func (r *NamespaceWaypointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// DEBUG: Log every reconcile invocation to confirm function is called
	log.Info("DEBUG: Reconcile function called", "namespace", req.Name, "enabled", r.EnableWaypointProvisioning)

	if !r.EnableWaypointProvisioning {
		log.V(1).Info("Waypoint provisioning disabled, skipping")
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling namespace for waypoint configuration", "namespace", req.Name)

	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, req.NamespacedName, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Namespace not found, may have been deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get namespace")
		return ctrl.Result{}, err
	}

	// Check if namespace is being deleted
	if !namespace.ObjectMeta.DeletionTimestamp.IsZero() {
		log.V(1).Info("Namespace is being deleted, skipping waypoint configuration")
		return ctrl.Result{}, nil
	}

	// Check if namespace has any Kagenti agent or tool workloads
	hasKagentiWorkloads, err := r.namespaceHasKagentiWorkloads(ctx, namespace.Name)
	if err != nil {
		log.Error(err, "Failed to check for Kagenti workloads in namespace")
		return ctrl.Result{}, err
	}

	if !hasKagentiWorkloads {
		log.V(1).Info("Namespace has no Kagenti workloads, skipping waypoint configuration")
		return ctrl.Result{}, nil
	}

	log.Info("Namespace has Kagenti workloads, ensuring waypoint configuration")

	// Ensure namespace has Istio ambient mesh labels
	if err := r.ensureIstioLabels(ctx, namespace); err != nil {
		log.Error(err, "Failed to ensure Istio labels on namespace")
		return ctrl.Result{}, err
	}

	// Ensure waypoint gateway exists
	if err := r.ensureWaypointGateway(ctx, namespace); err != nil {
		log.Error(err, "Failed to ensure waypoint gateway")
		return ctrl.Result{}, err
	}

	log.Info("Successfully configured waypoint for namespace")
	return ctrl.Result{}, nil
}

// namespaceHasKagentiWorkloads checks if the namespace contains any pods with kagenti.io/type=agent or tool.
func (r *NamespaceWaypointReconciler) namespaceHasKagentiWorkloads(ctx context.Context, namespace string) (bool, error) {
	log := log.FromContext(ctx)

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	log.Info("Checking for Kagenti workloads in namespace", "namespace", namespace, "totalPods", len(podList.Items))

	for _, pod := range podList.Items {
		if kagentiType, ok := pod.Labels[KagentiTypeLabel]; ok {
			if kagentiType == KagentiTypeAgent || kagentiType == KagentiTypeTool {
				log.Info("Found Kagenti workload pod",
					"namespace", namespace,
					"pod", pod.Name,
					"kagenti.io/type", kagentiType)
				return true, nil
			}
		}
	}

	log.Info("No Kagenti workloads found in namespace", "namespace", namespace)
	return false, nil
}

// ensureIstioLabels ensures the namespace has the required Istio ambient mesh labels.
func (r *NamespaceWaypointReconciler) ensureIstioLabels(ctx context.Context, namespace *corev1.Namespace) error {
	log := log.FromContext(ctx)

	labels := namespace.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	waypointName := namespace.Name + WaypointNameSuffix
	updated := false

	requiredLabels := map[string]string{
		IstioDiscoveryLabel:     IstioDiscoveryEnabled,
		IstioDataplaneModeLabel: IstioDataplaneModeAmbient,
		IstioUseWaypointLabel:   waypointName,
	}

	for key, value := range requiredLabels {
		if labels[key] != value {
			log.Info("Adding/updating Istio label",
				"namespace", namespace.Name,
				"label", key,
				"value", value)
			labels[key] = value
			updated = true
		}
	}

	if updated {
		namespace.SetLabels(labels)
		if err := r.Update(ctx, namespace); err != nil {
			return fmt.Errorf("failed to update namespace labels: %w", err)
		}
		log.Info("Updated namespace Istio labels", "namespace", namespace.Name)
	} else {
		log.V(1).Info("Namespace already has correct Istio labels", "namespace", namespace.Name)
	}

	return nil
}

// ensureWaypointGateway ensures a waypoint gateway exists in the namespace.
func (r *NamespaceWaypointReconciler) ensureWaypointGateway(ctx context.Context, namespace *corev1.Namespace) error {
	log := log.FromContext(ctx)

	gatewayName := namespace.Name + WaypointNameSuffix

	gateway := &gwapiv1.Gateway{}
	err := r.Get(ctx, client.ObjectKey{
		Name:      gatewayName,
		Namespace: namespace.Name,
	}, gateway)

	if err == nil {
		log.V(1).Info("Waypoint gateway already exists", "namespace", namespace.Name, "gateway", gatewayName)
		return r.validateWaypointLabels(ctx, gateway)
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get waypoint gateway: %w", err)
	}

	// Create waypoint gateway
	log.Info("Creating waypoint gateway", "namespace", namespace.Name, "gateway", gatewayName)

	gatewayClassName := gwapiv1.ObjectName(IstioWaypointGatewayClass)
	protocolHBONE := gwapiv1.ProtocolType("HBONE")
	portNumber := gwapiv1.PortNumber(15008)

	gateway = &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayName,
			Namespace: namespace.Name,
			Labels: map[string]string{
				IstioWaypointForLabel: IstioWaypointForAll,
			},
		},
		Spec: gwapiv1.GatewaySpec{
			GatewayClassName: gatewayClassName,
			Listeners: []gwapiv1.Listener{
				{
					Name:     "mesh",
					Port:     portNumber,
					Protocol: protocolHBONE,
				},
			},
		},
	}

	if err := r.Create(ctx, gateway); err != nil {
		return fmt.Errorf("failed to create waypoint gateway: %w", err)
	}

	log.Info("Successfully created waypoint gateway", "namespace", namespace.Name, "gateway", gatewayName)
	return nil
}

// validateWaypointLabels ensures the waypoint gateway has the correct labels.
func (r *NamespaceWaypointReconciler) validateWaypointLabels(ctx context.Context, gateway *gwapiv1.Gateway) error {
	log := log.FromContext(ctx)

	labels := gateway.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	if labels[IstioWaypointForLabel] != IstioWaypointForAll {
		log.Info("Updating waypoint gateway label",
			"namespace", gateway.Namespace,
			"gateway", gateway.Name,
			"label", IstioWaypointForLabel,
			"value", IstioWaypointForAll)

		labels[IstioWaypointForLabel] = IstioWaypointForAll
		gateway.SetLabels(labels)

		if err := r.Update(ctx, gateway); err != nil {
			return fmt.Errorf("failed to update waypoint gateway labels: %w", err)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceWaypointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctrl.Log.Info("DEBUG: Setting up NamespaceWaypointReconciler controller", "enabled", r.EnableWaypointProvisioning)

	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToNamespaceRequest),
		).
		Complete(r)

	if err != nil {
		ctrl.Log.Error(err, "DEBUG: Failed to setup NamespaceWaypointReconciler controller")
	} else {
		ctrl.Log.Info("DEBUG: Successfully setup NamespaceWaypointReconciler controller")
	}

	return err
}

// podToNamespaceRequest maps Pod events to Namespace reconcile requests.
// This ensures we reconcile the namespace when pods with kagenti.io/type labels are created.
func (r *NamespaceWaypointReconciler) podToNamespaceRequest(ctx context.Context, obj client.Object) []reconcile.Request {
	log := log.FromContext(ctx)

	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}

	// Only trigger namespace reconciliation if this is a Kagenti workload
	if kagentiType, ok := pod.Labels[KagentiTypeLabel]; ok {
		if kagentiType == KagentiTypeAgent || kagentiType == KagentiTypeTool {
			reconcileReq := []reconcile.Request{
				{
					NamespacedName: client.ObjectKey{
						Name: pod.Namespace,
					},
				},
			}
			log.Info("Pod event triggered namespace waypoint reconciliation",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"kagenti.io/type", kagentiType,
				"reconcileRequest", reconcileReq)
			return reconcileReq
		}
	}

	log.V(2).Info("Pod does not have kagenti.io/type label, skipping", "pod", pod.Name)
	return nil
}
