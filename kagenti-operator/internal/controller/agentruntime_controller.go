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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/kagenti/operator/api/v1alpha1"
)

const (
	AgentRuntimeFinalizer = "kagenti.io/cleanup"

	// AnnotationConfigHash is the annotation applied to PodTemplateSpec to trigger rolling updates.
	AnnotationConfigHash = "kagenti.io/config-hash"

	// AnnotationRestartPending marks a Sandbox that was scaled to 0 and needs
	// to be scaled back to 1 on the next reconcile cycle. Two-phase restart
	// avoids a race with the Sandbox controller's pod-name annotation.
	AnnotationRestartPending = "kagenti.io/restart-pending"

	// Condition types for AgentRuntime status.
	ConditionTypeReady          = "Ready"
	ConditionTypeTargetResolved = "TargetResolved"
	ConditionTypeConfigResolved = "ConfigResolved"

	// KindSandbox is the workload kind for agent-sandbox CRs.
	KindSandbox = "Sandbox"

	// AnnotationRestartPendingValue is the value set on AnnotationRestartPending.
	AnnotationRestartPendingValue = "true"
)

var sandboxGVK = schema.GroupVersionKind{
	Group:   "agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    KindSandbox,
}

// AgentRuntimeReconciler reconciles AgentRuntime objects.
type AgentRuntimeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	APIReader client.Reader // uncached reader for cross-namespace ConfigMap reads
}

// +kubebuilder:rbac:groups=agent.kagenti.dev,resources=agentruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agent.kagenti.dev,resources=agentruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agent.kagenti.dev,resources=agentruntimes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *AgentRuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling AgentRuntime", "namespacedName", req.NamespacedName)

	// 1. Fetch the AgentRuntime CR
	rt := &agentv1alpha1.AgentRuntime{}
	if err := r.Get(ctx, req.NamespacedName, rt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Handle deletion
	if !rt.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, rt)
	}

	// 3. Ensure finalizer
	if !controllerutil.ContainsFinalizer(rt, AgentRuntimeFinalizer) {
		controllerutil.AddFinalizer(rt, AgentRuntimeFinalizer)
		if err := r.Update(ctx, rt); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 4. Resolve targetRef (existence check)
	if err := r.resolveTargetRef(ctx, rt); err != nil {
		logger.Error(err, "Failed to resolve targetRef")
		r.setPhase(rt, agentv1alpha1.RuntimePhaseError)
		r.setCondition(rt, ConditionTypeTargetResolved, metav1.ConditionFalse, "TargetNotFound", err.Error())
		if statusErr := r.Status().Update(ctx, rt); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		if r.Recorder != nil {
			r.Recorder.Event(rt, corev1.EventTypeWarning, "TargetNotFound", err.Error())
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.setCondition(rt, ConditionTypeTargetResolved, metav1.ConditionTrue, "TargetFound",
		fmt.Sprintf("%s %s resolved", rt.Spec.TargetRef.Kind, rt.Spec.TargetRef.Name))

	// 4.1. Complete two-phase Sandbox restart if pending.
	if rt.Spec.TargetRef.Kind == KindSandbox {
		if result, done, err := r.completeSandboxRestart(ctx, rt); done {
			return result, err
		}
	}

	// 4.5. Ensure required authbridge ConfigMaps exist in the namespace.
	// Copies templates from kagenti-system if missing, matching the backend's
	// _ensure_authbridge_configmaps() semantics (create-if-not-exists).
	if err := r.ensureNamespaceConfigMaps(ctx, rt.Namespace); err != nil {
		logger.Error(err, "Failed to ensure namespace ConfigMaps")
		if r.Recorder != nil {
			r.Recorder.Event(rt, corev1.EventTypeWarning, "ConfigMapEnsureError", err.Error())
		}
	}

	// 5. Compute config hash from merged configuration (cluster → namespace → CR)
	configResult, err := ComputeConfigHash(ctx, r.Client, rt.Namespace, &rt.Spec)
	if err != nil {
		logger.Error(err, "Failed to compute config hash")
		r.setPhase(rt, agentv1alpha1.RuntimePhaseError)
		r.setCondition(rt, ConditionTypeReady, metav1.ConditionFalse, "ConfigHashError", err.Error())
		if statusErr := r.Status().Update(ctx, rt); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Surface config resolution warnings (e.g., multiple namespace defaults ConfigMaps)
	if len(configResult.Warnings) > 0 {
		r.setCondition(rt, ConditionTypeConfigResolved, metav1.ConditionTrue, "ConfigWarning",
			strings.Join(configResult.Warnings, "; "))
		if r.Recorder != nil {
			for _, w := range configResult.Warnings {
				r.Recorder.Event(rt, corev1.EventTypeWarning, "ConfigWarning", w)
			}
		}
	} else {
		r.setCondition(rt, ConditionTypeConfigResolved, metav1.ConditionTrue, "ConfigResolved",
			"Configuration resolved successfully")
	}

	// 6. Apply labels and annotations to the target workload
	if err := r.applyWorkloadConfig(ctx, rt, configResult.Hash); err != nil {
		logger.Error(err, "Failed to apply workload config")
		r.setPhase(rt, agentv1alpha1.RuntimePhaseError)
		r.setCondition(rt, ConditionTypeReady, metav1.ConditionFalse, "ConfigApplyError", err.Error())
		if statusErr := r.Status().Update(ctx, rt); statusErr != nil {
			logger.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 7. Count configured pods
	configuredPods, err := r.countConfiguredPods(ctx, rt)
	if err != nil {
		logger.V(1).Info("Failed to count configured pods", "error", err)
	}

	// 8. Update status
	rt.Status.ConfiguredPods = configuredPods
	r.setPhase(rt, agentv1alpha1.RuntimePhaseActive)
	r.setCondition(rt, ConditionTypeReady, metav1.ConditionTrue, "Configured",
		fmt.Sprintf("Workload %s configured with config-hash %s", rt.Spec.TargetRef.Name, configResult.Hash[:12]))
	if err := r.Status().Update(ctx, rt); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Event(rt, corev1.EventTypeNormal, "Configured",
			fmt.Sprintf("Applied config to %s %s", rt.Spec.TargetRef.Kind, rt.Spec.TargetRef.Name))
	}

	return ctrl.Result{}, nil
}

// resolveTargetRef verifies that the workload referenced by spec.targetRef exists.
func (r *AgentRuntimeReconciler) resolveTargetRef(ctx context.Context, rt *agentv1alpha1.AgentRuntime) error {
	ref := rt.Spec.TargetRef

	if _, err := schema.ParseGroupVersion(ref.APIVersion); err != nil {
		return fmt.Errorf("invalid apiVersion %s: %w", ref.APIVersion, err)
	}

	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if !ok {
		return fmt.Errorf("unsupported workload kind: %s", ref.Kind)
	}

	key := client.ObjectKey{Namespace: rt.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, acc.obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%s/%s %s not found in namespace %s", ref.APIVersion, ref.Kind, ref.Name, rt.Namespace)
		}
		return err
	}

	return nil
}

// applyWorkloadConfig applies kagenti labels and config-hash annotation to the
// target workload's metadata and PodTemplateSpec.
func (r *AgentRuntimeReconciler) applyWorkloadConfig(ctx context.Context, rt *agentv1alpha1.AgentRuntime, configHash string) error {
	logger := log.FromContext(ctx)
	ref := rt.Spec.TargetRef

	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if !ok {
		return fmt.Errorf("unsupported workload kind: %s", ref.Kind)
	}

	key := types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}

	var configHashChanged bool

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, key, acc.obj); err != nil {
			return err
		}

		// Check if update is needed before mutating
		currentWorkloadLabels := acc.obj.GetLabels()
		currentPodLabels := acc.getPodLabels(acc.obj)
		currentPodAnnotations := acc.getPodAnnotations(acc.obj)

		alreadyConfigured := currentWorkloadLabels[LabelAgentType] == string(rt.Spec.Type) &&
			currentWorkloadLabels[LabelManagedBy] == LabelManagedByValue &&
			currentPodLabels[LabelAgentType] == string(rt.Spec.Type) &&
			currentPodAnnotations[AnnotationConfigHash] == configHash

		if alreadyConfigured {
			return nil
		}

		// Track whether config-hash actually changed (for Sandbox rollout)
		previousHash := currentPodAnnotations[AnnotationConfigHash]
		configHashChanged = previousHash != "" && previousHash != configHash

		// Apply labels to workload metadata
		workloadLabels := acc.obj.GetLabels()
		if workloadLabels == nil {
			workloadLabels = make(map[string]string)
		}
		workloadLabels[LabelAgentType] = string(rt.Spec.Type)
		workloadLabels[LabelManagedBy] = LabelManagedByValue
		acc.obj.SetLabels(workloadLabels)

		// Apply labels to PodTemplateSpec
		podLabels := acc.getPodLabels(acc.obj)
		if podLabels == nil {
			podLabels = make(map[string]string)
		}
		podLabels[LabelAgentType] = string(rt.Spec.Type)
		acc.setPodLabels(acc.obj, podLabels)

		// Apply config-hash annotation to PodTemplateSpec
		podAnnotations := acc.getPodAnnotations(acc.obj)
		if podAnnotations == nil {
			podAnnotations = make(map[string]string)
		}
		podAnnotations[AnnotationConfigHash] = configHash
		acc.setPodAnnotations(acc.obj, podAnnotations)

		logger.Info("Applying config to workload",
			"workload", ref.Name,
			"kind", ref.Kind,
			"type", string(rt.Spec.Type),
			"configHash", configHash[:12])

		return r.Update(ctx, acc.obj)
	})
	if err != nil {
		return err
	}

	// Sandbox pods don't restart on podTemplate changes (upstream limitation).
	// Phase 1: scale to 0 and mark restart-pending. Phase 2 runs on the next
	// reconcile (triggered by the Sandbox watch) to clear stale annotations
	// and scale back to 1. Two-phase avoids a race with the Sandbox controller.
	if ref.Kind == KindSandbox && configHashChanged {
		if err := r.beginSandboxRestart(ctx, key); err != nil {
			return fmt.Errorf("sandbox restart (phase 1) failed: %w", err)
		}
	}

	return nil
}

// beginSandboxRestart is phase 1 of a two-phase Sandbox restart.
// It scales the Sandbox to 0 replicas and sets the restart-pending annotation.
// Phase 2 (completeSandboxRestart) runs on the next reconcile to clear the
// stale pod-name annotation and scale back to 1.
func (r *AgentRuntimeReconciler) beginSandboxRestart(ctx context.Context, key types.NamespacedName) error {
	logger := log.FromContext(ctx)
	logger.Info("Sandbox restart phase 1: scaling to 0", "sandbox", key.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(sandboxGVK)
		if err := r.Get(ctx, key, obj); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(obj.Object, int64(0), "spec", "replicas"); err != nil {
			return err
		}
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[AnnotationRestartPending] = AnnotationRestartPendingValue
		obj.SetAnnotations(annotations)
		return r.Update(ctx, obj)
	})
}

// completeSandboxRestart is phase 2 of a two-phase Sandbox restart.
// It checks for the restart-pending annotation on a Sandbox with replicas=0,
// clears the stale pod-name annotation, removes restart-pending, and scales
// back to 1. Returns (result, true, err) if it handled the restart, or
// (_, false, nil) if no restart was pending.
func (r *AgentRuntimeReconciler) completeSandboxRestart(ctx context.Context, rt *agentv1alpha1.AgentRuntime) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ref := rt.Spec.TargetRef
	key := types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(ctx, key, obj); err != nil {
		return ctrl.Result{}, false, nil
	}

	annotations := obj.GetAnnotations()
	if annotations[AnnotationRestartPending] != AnnotationRestartPendingValue {
		return ctrl.Result{}, false, nil
	}

	logger.Info("Sandbox restart phase 2: clearing pod-name and scaling to 1", "sandbox", key.Name)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(sandboxGVK)
		if err := r.Get(ctx, key, obj); err != nil {
			return err
		}
		annotations := obj.GetAnnotations()
		delete(annotations, "agents.x-k8s.io/pod-name")
		delete(annotations, AnnotationRestartPending)
		obj.SetAnnotations(annotations)
		if err := unstructured.SetNestedField(obj.Object, int64(1), "spec", "replicas"); err != nil {
			return err
		}
		return r.Update(ctx, obj)
	})
	if err != nil {
		logger.Error(err, "Sandbox restart phase 2 failed, will retry")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, err
	}

	if r.Recorder != nil {
		r.Recorder.Event(rt, corev1.EventTypeNormal, "SandboxRestarted",
			fmt.Sprintf("Sandbox %s restarted via scale 0→1", ref.Name))
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
}

// countConfiguredPods counts pods that have the kagenti.io/type label matching the runtime type.
func (r *AgentRuntimeReconciler) countConfiguredPods(ctx context.Context, rt *agentv1alpha1.AgentRuntime) (int32, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(rt.Namespace),
		client.MatchingLabels{LabelAgentType: string(rt.Spec.Type)},
	); err != nil {
		return 0, err
	}

	var count int32
	for i := range podList.Items {
		pod := &podList.Items[i]
		if isPodOwnedByWorkload(pod, rt.Spec.TargetRef.Name) {
			count++
		}
	}
	return count, nil
}

// isPodOwnedByWorkload checks if a pod is transitively owned by the named workload.
// For Deployments: Pod → ReplicaSet (<deployment>-<pod-template-hash>) → Deployment.
// For StatefulSets: Pod is directly owned by the StatefulSet.
// For Sandboxes: Pod is directly owned by the Sandbox CR.
func isPodOwnedByWorkload(pod *corev1.Pod, workloadName string) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ReplicaSet" {
			// ReplicaSet name is <deployment-name>-<pod-template-hash>.
			// Extract the deployment name by trimming the last "-<hash>" suffix.
			if idx := strings.LastIndex(ref.Name, "-"); idx > 0 && ref.Name[:idx] == workloadName {
				return true
			}
		}
		if ref.Kind == "StatefulSet" && ref.Name == workloadName {
			return true
		}
		if ref.Kind == KindSandbox && ref.Name == workloadName {
			return true
		}
	}
	return false
}

// handleDeletion runs finalizer logic when an AgentRuntime is deleted.
// It removes the kagenti.io/type label and kagenti.io/config-hash annotation so that
// the next rolling update creates pods without sidecars, returning the workload to its
// pre-AR state.
func (r *AgentRuntimeReconciler) handleDeletion(ctx context.Context, rt *agentv1alpha1.AgentRuntime) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(rt, AgentRuntimeFinalizer) {
		return ctrl.Result{}, nil
	}

	logger.Info("Handling AgentRuntime deletion", "name", rt.Name)

	ref := rt.Spec.TargetRef
	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if ok {
		key := types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}
		updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, acc.obj); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}

			// Remove kagenti.io/type and kagenti.io/managed-by from workload metadata.
			workloadLabels := acc.obj.GetLabels()
			delete(workloadLabels, LabelAgentType)
			delete(workloadLabels, LabelManagedBy)
			acc.obj.SetLabels(workloadLabels)

			// Remove kagenti.io/type from PodTemplateSpec pod labels so future pods
			// are not presented to the webhook with the type label.
			podLabels := acc.getPodLabels(acc.obj)
			delete(podLabels, LabelAgentType)
			acc.setPodLabels(acc.obj, podLabels)

			// Remove kagenti.io/config-hash from PodTemplateSpec pod annotations.
			// This triggers the rolling update that replaces existing injected pods,
			// and leaves the workload annotation-clean for any future AR.
			podAnnotations := acc.getPodAnnotations(acc.obj)
			delete(podAnnotations, AnnotationConfigHash)
			acc.setPodAnnotations(acc.obj, podAnnotations)

			logger.Info("Removed kagenti labels and config-hash from workload on AgentRuntime deletion",
				"workload", ref.Name, "kind", ref.Kind)
			return r.Update(ctx, acc.obj)
		})
		if updateErr != nil {
			// Return the error to requeue — don't remove the finalizer until the
			// workload is cleaned up. This prevents the CR from being deleted while
			// the workload retains stale managed-by labels and wrong config-hash.
			logger.Error(updateErr, "Failed to update workload on deletion, will retry")
			return ctrl.Result{}, updateErr
		}
	}

	// Remove finalizer
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &agentv1alpha1.AgentRuntime{}
		if err := r.Get(ctx, types.NamespacedName{Name: rt.Name, Namespace: rt.Namespace}, latest); err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(latest, AgentRuntimeFinalizer)
		return r.Update(ctx, latest)
	}); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("Removed finalizer from AgentRuntime", "name", rt.Name)
	return ctrl.Result{}, nil
}

func (r *AgentRuntimeReconciler) setPhase(rt *agentv1alpha1.AgentRuntime, phase agentv1alpha1.RuntimePhase) {
	rt.Status.Phase = phase
}

func (r *AgentRuntimeReconciler) setCondition(rt *agentv1alpha1.AgentRuntime, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&rt.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: rt.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// templateConfigMapNames lists the well-known ConfigMaps that authbridge sidecars
// require. The Helm chart and backend API create these in agent namespaces; the
// operator copies templates from kagenti-system for namespaces created by other
// means (GitOps, manual kubectl).
var templateConfigMapNames = []string{
	"authbridge-config",
	"authbridge-runtime-config",
	"envoy-config",
	"spiffe-helper-config",
}

// ensureNamespaceConfigMaps copies template ConfigMaps from kagenti-system to the
// target namespace if they don't already exist. This mirrors the backend's
// ensure_configmap() semantics: create-if-not-exists, preserving user customizations.
func (r *AgentRuntimeReconciler) ensureNamespaceConfigMaps(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	reader := r.uncachedReader()

	for _, name := range templateConfigMapNames {
		existing := &corev1.ConfigMap{}
		err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check ConfigMap %s/%s: %w", namespace, name, err)
		}

		template := &corev1.ConfigMap{}
		templateKey := client.ObjectKey{Namespace: ClusterDefaultsNamespace, Name: name}
		if err := reader.Get(ctx, templateKey, template); err != nil {
			if apierrors.IsNotFound(err) {
				logger.V(1).Info("Template ConfigMap not found in kagenti-system, skipping",
					"name", name)
				continue
			}
			return fmt.Errorf("failed to read template ConfigMap %s/%s: %w", ClusterDefaultsNamespace, name, err)
		}

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					LabelManagedBy: LabelManagedByValue,
				},
			},
			Data: template.Data,
		}
		if err := r.Create(ctx, cm); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("failed to create ConfigMap %s/%s: %w", namespace, name, err)
		}
		logger.Info("Created ConfigMap from template", "namespace", namespace, "name", name)
	}
	return nil
}

func (r *AgentRuntimeReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// mapWorkloadToAgentRuntime maps workload events to AgentRuntime reconcile requests.
func (r *AgentRuntimeReconciler) mapWorkloadToAgentRuntime(apiVersion, kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		rtList := &agentv1alpha1.AgentRuntimeList{}
		if err := r.List(ctx, rtList, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}

		var requests []reconcile.Request
		for _, rt := range rtList.Items {
			if rt.Spec.TargetRef.Name == obj.GetName() &&
				rt.Spec.TargetRef.Kind == kind &&
				rt.Spec.TargetRef.APIVersion == apiVersion {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      rt.Name,
						Namespace: rt.Namespace,
					},
				})
			}
		}
		return requests
	}
}

// mapClusterConfigMapToAgentRuntimes maps changes to cluster-level ConfigMaps
// (kagenti-webhook-defaults and kagenti-webhook-feature-gates) to all AgentRuntime
// reconcile requests across all namespaces.
func (r *AgentRuntimeReconciler) mapClusterConfigMapToAgentRuntimes(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != ClusterDefaultsNamespace {
		return nil
	}
	if obj.GetName() != ClusterDefaultsConfigMapName && obj.GetName() != ClusterFeatureGatesConfigMapName {
		return nil
	}

	rtList := &agentv1alpha1.AgentRuntimeList{}
	if err := r.List(ctx, rtList); err != nil {
		return nil
	}
	return agentRuntimesToRequests(rtList.Items)
}

// mapNamespaceConfigMapToAgentRuntimes maps changes to namespace-level defaults
// ConfigMaps (kagenti.io/defaults=true) to AgentRuntimes in the same namespace.
func (r *AgentRuntimeReconciler) mapNamespaceConfigMapToAgentRuntimes(ctx context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[LabelNamespaceDefaults] != "true" {
		return nil
	}

	rtList := &agentv1alpha1.AgentRuntimeList{}
	if err := r.List(ctx, rtList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	return agentRuntimesToRequests(rtList.Items)
}

// agentRuntimesToRequests converts a list of AgentRuntimes to reconcile requests.
// Returns nil if the list is empty.
func agentRuntimesToRequests(items []agentv1alpha1.AgentRuntime) []reconcile.Request {
	if len(items) == 0 {
		return nil
	}
	requests := make([]reconcile.Request, len(items))
	for i, rt := range items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      rt.Name,
				Namespace: rt.Namespace,
			},
		}
	}
	return requests
}

// mapConfigMapToAgentRuntimes dispatches ConfigMap events to either the cluster
// or namespace mapper based on the ConfigMap's location and labels.
func (r *AgentRuntimeReconciler) mapConfigMapToAgentRuntimes(ctx context.Context, obj client.Object) []reconcile.Request {
	// Check cluster-level ConfigMaps first
	if requests := r.mapClusterConfigMapToAgentRuntimes(ctx, obj); requests != nil {
		return requests
	}
	// Then namespace-level defaults
	return r.mapNamespaceConfigMapToAgentRuntimes(ctx, obj)
}

// SandboxCRDExists checks whether the agent-sandbox CRD is installed on the cluster.
func SandboxCRDExists(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false
	}
	resources, err := dc.ServerResourcesForGroupVersion("agents.x-k8s.io/v1alpha1")
	if err != nil {
		return false
	}
	for _, r := range resources.APIResources {
		if r.Kind == KindSandbox {
			return true
		}
	}
	return false
}

// SetupWithManager registers the AgentRuntime controller with the manager.
func (r *AgentRuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&agentv1alpha1.AgentRuntime{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentRuntime("apps/v1", "Deployment")),
		).
		Watches(
			&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentRuntime("apps/v1", "StatefulSet")),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.mapConfigMapToAgentRuntimes),
		)

	if SandboxCRDExists(mgr.GetConfig()) {
		sandboxObj := &unstructured.Unstructured{}
		sandboxObj.SetGroupVersionKind(sandboxGVK)
		builder = builder.Watches(
			sandboxObj,
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentRuntime("agents.x-k8s.io/v1alpha1", KindSandbox)),
		)
	}

	return builder.
		Named("agentruntime").
		Complete(r)
}

// runtimePodTemplateAccessor provides uniform access to PodTemplateSpec
// labels and annotations across Deployment and StatefulSet.
type runtimePodTemplateAccessor struct {
	obj               client.Object
	getPodLabels      func(client.Object) map[string]string
	setPodLabels      func(client.Object, map[string]string)
	getPodAnnotations func(client.Object) map[string]string
	setPodAnnotations func(client.Object, map[string]string)
}

func newRuntimePodTemplateAccessor(kind string) (*runtimePodTemplateAccessor, bool) {
	switch kind {
	case "Deployment":
		return &runtimePodTemplateAccessor{
			obj: &appsv1.Deployment{},
			getPodLabels: func(o client.Object) map[string]string {
				return o.(*appsv1.Deployment).Spec.Template.Labels
			},
			setPodLabels: func(o client.Object, l map[string]string) {
				o.(*appsv1.Deployment).Spec.Template.Labels = l
			},
			getPodAnnotations: func(o client.Object) map[string]string {
				return o.(*appsv1.Deployment).Spec.Template.Annotations
			},
			setPodAnnotations: func(o client.Object, a map[string]string) {
				o.(*appsv1.Deployment).Spec.Template.Annotations = a
			},
		}, true
	case "StatefulSet":
		return &runtimePodTemplateAccessor{
			obj: &appsv1.StatefulSet{},
			getPodLabels: func(o client.Object) map[string]string {
				return o.(*appsv1.StatefulSet).Spec.Template.Labels
			},
			setPodLabels: func(o client.Object, l map[string]string) {
				o.(*appsv1.StatefulSet).Spec.Template.Labels = l
			},
			getPodAnnotations: func(o client.Object) map[string]string {
				return o.(*appsv1.StatefulSet).Spec.Template.Annotations
			},
			setPodAnnotations: func(o client.Object, a map[string]string) {
				o.(*appsv1.StatefulSet).Spec.Template.Annotations = a
			},
		}, true
	case KindSandbox:
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(sandboxGVK)
		return &runtimePodTemplateAccessor{
			obj: u,
			getPodLabels: func(o client.Object) map[string]string {
				u := o.(*unstructured.Unstructured)
				labels, _, _ := unstructured.NestedStringMap(u.Object, "spec", "podTemplate", "metadata", "labels")
				return labels
			},
			setPodLabels: func(o client.Object, l map[string]string) {
				u := o.(*unstructured.Unstructured)
				_ = unstructured.SetNestedStringMap(u.Object, l, "spec", "podTemplate", "metadata", "labels")
			},
			getPodAnnotations: func(o client.Object) map[string]string {
				u := o.(*unstructured.Unstructured)
				annotations, _, _ := unstructured.NestedStringMap(u.Object, "spec", "podTemplate", "metadata", "annotations")
				return annotations
			},
			setPodAnnotations: func(o client.Object, a map[string]string) {
				u := o.(*unstructured.Unstructured)
				_ = unstructured.SetNestedStringMap(u.Object, a, "spec", "podTemplate", "metadata", "annotations")
			},
		}, true
	default:
		return nil, false
	}
}
