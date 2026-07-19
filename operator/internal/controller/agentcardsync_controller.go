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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

var syncLogger = ctrl.Log.WithName("controller").WithName("AgentCardSync")

const (
	// LabelManagedBy identifies auto-created AgentCards so the sync controller
	// can distinguish its own cards from manually-created ones.
	LabelManagedBy      = "app.kubernetes.io/managed-by"
	LabelManagedByValue = "rossoctl-operator"
)

// AgentCardSyncReconciler auto-creates AgentCards for labelled agent workloads.
type AgentCardSyncReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	SpireTrustDomain string
}

// +kubebuilder:rbac:groups=agent.rossoctl.dev,resources=agentcards,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/finalizers,verbs=update

func (r *AgentCardSyncReconciler) ReconcileDeployment(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	syncLogger.V(1).Info("Reconciling Deployment for auto-sync", "namespacedName", req.NamespacedName)

	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !r.shouldSyncWorkload(deployment.Labels) {
		return ctrl.Result{}, nil
	}

	// Create or update AgentCard with targetRef
	gvk := appsv1.SchemeGroupVersion.WithKind("Deployment")
	return r.ensureAgentCard(ctx, deployment, gvk)
}

func (r *AgentCardSyncReconciler) ReconcileStatefulSet(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	syncLogger.V(1).Info("Reconciling StatefulSet for auto-sync", "namespacedName", req.NamespacedName)

	statefulset := &appsv1.StatefulSet{}
	if err := r.Get(ctx, req.NamespacedName, statefulset); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !r.shouldSyncWorkload(statefulset.Labels) {
		return ctrl.Result{}, nil
	}

	gvk := appsv1.SchemeGroupVersion.WithKind("StatefulSet")
	return r.ensureAgentCard(ctx, statefulset, gvk)
}

func (r *AgentCardSyncReconciler) ReconcileSandbox(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	syncLogger.V(1).Info("Reconciling Sandbox for auto-sync", "namespacedName", req.NamespacedName)

	sbx := &unstructured.Unstructured{}
	sbx.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(ctx, req.NamespacedName, sbx); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !r.shouldSyncWorkload(sbx.GetLabels()) {
		return ctrl.Result{}, nil
	}

	return r.ensureAgentCard(ctx, sbx, sandboxGVK)
}

func (r *AgentCardSyncReconciler) shouldSyncWorkload(labels map[string]string) bool {
	if labels == nil || labels[LabelAgentType] != LabelValueAgent {
		return false
	}
	return hasProtocolLabels(labels)
}

// getAgentCardNameFromWorkload includes the kind suffix to prevent name collisions.
func (r *AgentCardSyncReconciler) getAgentCardNameFromWorkload(workloadName string, kind string) string {
	return fmt.Sprintf("%s-%s-card", workloadName, strings.ToLower(kind))
}

func (r *AgentCardSyncReconciler) ensureAgentCard(ctx context.Context, obj client.Object, gvk schema.GroupVersionKind) (ctrl.Result, error) {
	cardName := r.getAgentCardNameFromWorkload(obj.GetName(), gvk.Kind)

	existingCard := &agentv1alpha1.AgentCard{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      cardName,
		Namespace: obj.GetNamespace(),
	}, existingCard)

	if err == nil {
		if r.isAutoCreatedCard(existingCard) {
			if manualCard, found := r.findManualCardForWorkload(ctx, obj, gvk, cardName); found {
				syncLogger.Info("Deleting auto-created AgentCard superseded by manual card",
					"autoCard", cardName, "manualCard", manualCard,
					"workload", obj.GetName(), "kind", gvk.Kind)
				if err := r.Delete(ctx, existingCard); err != nil && !errors.IsNotFound(err) {
					syncLogger.Error(err, "Failed to delete superseded auto-created AgentCard")
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}

		needsUpdate := false

		if !r.hasOwnerReferenceForObject(existingCard, obj) {
			syncLogger.Info("Adding owner reference to existing AgentCard",
				"agentCard", cardName, "owner", obj.GetName(), "kind", gvk.Kind)
			if err := controllerutil.SetControllerReference(obj, existingCard, r.Scheme); err != nil {
				syncLogger.Error(err, "Failed to set owner reference")
				return ctrl.Result{}, err
			}
			needsUpdate = true
		}

		if existingCard.Spec.TargetRef != nil {
			tr := existingCard.Spec.TargetRef
			expectedAPIVersion := gvk.GroupVersion().String()
			expectedKind := gvk.Kind
			expectedName := obj.GetName()

			if tr.APIVersion == "" && tr.Kind == "" && tr.Name == "" {
				syncLogger.Info("Initializing empty AgentCard TargetRef to match workload",
					"agentCard", cardName,
					"apiVersion", expectedAPIVersion,
					"kind", expectedKind,
					"name", expectedName)
				tr.APIVersion = expectedAPIVersion
				tr.Kind = expectedKind
				tr.Name = expectedName
				needsUpdate = true
			} else if tr.APIVersion != expectedAPIVersion || tr.Kind != expectedKind || tr.Name != expectedName {
				syncLogger.Error(fmt.Errorf("targetRef mismatch"), "AgentCard TargetRef does not match reconciled workload",
					"agentCard", cardName,
					"expectedAPIVersion", expectedAPIVersion,
					"expectedKind", expectedKind,
					"expectedName", expectedName,
					"foundAPIVersion", tr.APIVersion,
					"foundKind", tr.Kind,
					"foundName", tr.Name)
				return ctrl.Result{}, fmt.Errorf(
					"AgentCard TargetRef conflict for %s/%s: expected targetRef %s/%s %s, found %s/%s %s",
					obj.GetNamespace(), cardName,
					expectedAPIVersion, expectedKind, expectedName,
					tr.APIVersion, tr.Kind, tr.Name,
				)
			}
		}
		if needsUpdate {
			if err := r.Update(ctx, existingCard); err != nil {
				syncLogger.Error(err, "Failed to update AgentCard")
				return ctrl.Result{}, err
			}
			syncLogger.Info("Successfully updated AgentCard", "agentCard", cardName)
		}
		return ctrl.Result{}, nil
	}

	if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if existingCard, found := r.findExistingCardForWorkload(ctx, obj, gvk); found {
		syncLogger.Info("Skipping auto-creation: another AgentCard already targets this workload",
			"existingCard", existingCard,
			"workload", obj.GetName(),
			"kind", gvk.Kind)
		return ctrl.Result{}, nil
	}

	return r.createAgentCardForWorkload(ctx, obj, gvk, cardName)
}

// findExistingCardForWorkload returns the name of an existing card targeting this workload, if any.
func (r *AgentCardSyncReconciler) findExistingCardForWorkload(ctx context.Context, obj client.Object, gvk schema.GroupVersionKind) (string, bool) {
	return r.findCardForWorkload(ctx, obj, gvk, "")
}

// findManualCardForWorkload returns the name of a manually-created card targeting this workload,
// excluding the auto-created card identified by excludeName.
func (r *AgentCardSyncReconciler) findManualCardForWorkload(ctx context.Context, obj client.Object, gvk schema.GroupVersionKind, excludeName string) (string, bool) {
	return r.findCardForWorkload(ctx, obj, gvk, excludeName)
}

// findCardForWorkload lists cards targeting the workload, optionally excluding one by name.
// Uses the targetRef.name field index when available, falling back to a full namespace list.
func (r *AgentCardSyncReconciler) findCardForWorkload(ctx context.Context, obj client.Object, gvk schema.GroupVersionKind, excludeName string) (string, bool) {
	cardList := &agentv1alpha1.AgentCardList{}

	err := r.List(ctx, cardList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{TargetRefNameIndex: obj.GetName()},
	)
	if err != nil {
		// Field index may not be available (e.g. unit tests); fall back to unindexed list.
		cardList = &agentv1alpha1.AgentCardList{}
		if fallbackErr := r.List(ctx, cardList, client.InNamespace(obj.GetNamespace())); fallbackErr != nil {
			syncLogger.Error(fallbackErr, "Failed to list AgentCards for duplicate check")
			return "", false
		}
	}

	expectedAPIVersion := gvk.GroupVersion().String()

	for i := range cardList.Items {
		card := &cardList.Items[i]
		if card.Name == excludeName {
			continue
		}
		if card.Spec.TargetRef != nil &&
			card.Spec.TargetRef.APIVersion == expectedAPIVersion &&
			card.Spec.TargetRef.Kind == gvk.Kind &&
			card.Spec.TargetRef.Name == obj.GetName() {
			return card.Name, true
		}
	}
	return "", false
}

func (r *AgentCardSyncReconciler) isAutoCreatedCard(card *agentv1alpha1.AgentCard) bool {
	return card.Labels != nil && card.Labels[LabelManagedBy] == LabelManagedByValue
}

// createAgentCardForWorkload creates a new AgentCard for a workload using targetRef
func (r *AgentCardSyncReconciler) createAgentCardForWorkload(ctx context.Context, obj client.Object, gvk schema.GroupVersionKind, cardName string) (ctrl.Result, error) {
	syncLogger.Info("Creating AgentCard for workload",
		"agentCard", cardName,
		"kind", gvk.Kind,
		"workload", obj.GetName())

	labels := obj.GetLabels()
	appName := labels["app.kubernetes.io/name"]
	if appName == "" {
		appName = obj.GetName()
	}

	agentCard := &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cardName,
			Namespace: obj.GetNamespace(),
			Labels: map[string]string{
				"app.kubernetes.io/name": appName,
				LabelManagedBy:           LabelManagedByValue,
			},
		},
		Spec: agentv1alpha1.AgentCardSpec{
			SyncPeriod: "30s",
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: gvk.GroupVersion().String(),
				Kind:       gvk.Kind,
				Name:       obj.GetName(),
			},
		},
	}

	if r.SpireTrustDomain != "" {
		agentCard.Spec.IdentityBinding = &agentv1alpha1.IdentityBinding{}
	}

	if err := controllerutil.SetControllerReference(obj, agentCard, r.Scheme); err != nil {
		syncLogger.Error(err, "Failed to set controller reference for AgentCard")
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, agentCard); err != nil {
		if errors.IsAlreadyExists(err) {
			return r.handleAlreadyExistsOnCreate(ctx, obj, gvk, cardName)
		}
		syncLogger.Error(err, "Failed to create AgentCard",
			"agentCard", cardName, "workload", obj.GetName(), "kind", gvk.Kind)
		return ctrl.Result{}, err
	}

	syncLogger.Info("Successfully created AgentCard", "agentCard", agentCard.Name)
	return ctrl.Result{}, nil
}

func (r *AgentCardSyncReconciler) handleAlreadyExistsOnCreate(ctx context.Context, obj client.Object, gvk schema.GroupVersionKind, cardName string) (ctrl.Result, error) {
	syncLogger.Info("AgentCard already exists, validating ownership", "agentCard", cardName)

	existingCard := &agentv1alpha1.AgentCard{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cardName,
		Namespace: obj.GetNamespace(),
	}, existingCard); err != nil {
		if errors.IsNotFound(err) {
			syncLogger.Info("AgentCard was deleted after AlreadyExists, requeueing", "agentCard", cardName)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	expectedAPIVersion := gvk.GroupVersion().String()
	expectedKind := gvk.Kind
	expectedName := obj.GetName()

	if existingCard.Spec.TargetRef != nil {
		tr := existingCard.Spec.TargetRef
		if tr.APIVersion != expectedAPIVersion || tr.Kind != expectedKind || tr.Name != expectedName {
			syncLogger.Error(fmt.Errorf("targetRef conflict"), "AgentCard targetRef conflict detected",
				"agentCard", cardName,
				"expectedAPIVersion", expectedAPIVersion,
				"expectedKind", expectedKind,
				"expectedName", expectedName,
				"foundAPIVersion", tr.APIVersion,
				"foundKind", tr.Kind,
				"foundName", tr.Name)
			return ctrl.Result{}, fmt.Errorf(
				"AgentCard %s/%s already exists with conflicting targetRef: expected %s/%s %s, found %s/%s %s",
				obj.GetNamespace(), cardName,
				expectedAPIVersion, expectedKind, expectedName,
				tr.APIVersion, tr.Kind, tr.Name,
			)
		}
	}

	if !r.hasOwnerReferenceForObject(existingCard, obj) {
		syncLogger.Info("Adding owner reference to existing AgentCard",
			"agentCard", cardName, "owner", obj.GetName(), "kind", gvk.Kind)
		if err := controllerutil.SetControllerReference(obj, existingCard, r.Scheme); err != nil {
			syncLogger.Error(err, "Failed to set owner reference on existing AgentCard")
			return ctrl.Result{}, err
		}

		if existingCard.Spec.TargetRef == nil {
			existingCard.Spec.TargetRef = &agentv1alpha1.TargetRef{
				APIVersion: expectedAPIVersion,
				Kind:       expectedKind,
				Name:       expectedName,
			}
		}

		if err := r.Update(ctx, existingCard); err != nil {
			syncLogger.Error(err, "Failed to update existing AgentCard")
			return ctrl.Result{}, err
		}
		syncLogger.Info("Successfully updated existing AgentCard", "agentCard", cardName)
	}

	return ctrl.Result{}, nil
}

func (r *AgentCardSyncReconciler) hasOwnerReferenceForObject(agentCard *agentv1alpha1.AgentCard, obj client.Object) bool {
	for _, ownerRef := range agentCard.OwnerReferences {
		if ownerRef.UID == obj.GetUID() {
			return true
		}
	}
	return false
}

func (r *AgentCardSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := RegisterAgentCardTargetRefIndex(mgr); err != nil {
		return err
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("agentcardsync-deployment").
		For(&appsv1.Deployment{}).
		WithEventFilter(agentLabelPredicate()).
		Complete(&deploymentReconcilerAdapter{r}); err != nil {
		return err
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("agentcardsync-statefulset").
		For(&appsv1.StatefulSet{}).
		WithEventFilter(agentLabelPredicate()).
		Complete(&statefulSetReconcilerAdapter{r}); err != nil {
		return err
	}

	if SandboxCRDExists(mgr.GetConfig()) {
		sandboxObj := &unstructured.Unstructured{}
		sandboxObj.SetGroupVersionKind(sandboxGVK)
		if err := ctrl.NewControllerManagedBy(mgr).
			Named("agentcardsync-sandbox").
			For(sandboxObj).
			WithEventFilter(agentLabelPredicate()).
			Complete(&sandboxReconcilerAdapter{r}); err != nil {
			return err
		}
	}

	return nil
}

type deploymentReconcilerAdapter struct {
	*AgentCardSyncReconciler
}

func (a *deploymentReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return a.ReconcileDeployment(ctx, req)
}

type statefulSetReconcilerAdapter struct {
	*AgentCardSyncReconciler
}

func (a *statefulSetReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return a.ReconcileStatefulSet(ctx, req)
}

type sandboxReconcilerAdapter struct {
	*AgentCardSyncReconciler
}

func (a *sandboxReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return a.ReconcileSandbox(ctx, req)
}
