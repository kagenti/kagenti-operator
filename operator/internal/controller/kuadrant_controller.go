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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rossoctl/operator/internal/kuadrant"
)

const (
	kuadrantNamespace = "kuadrant-system"
	kuadrantCRName    = "kuadrant"
)

var kuadrantLogger = ctrl.Log.WithName("controller").WithName("Kuadrant")

// KuadrantReconciler ensures a Kuadrant CR exists in kuadrant-system.
// It replaces the manual `kubectl apply` in setup-rossoctl.sh, providing
// drift reconciliation and idempotent creation.
type KuadrantReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=kuadrant.io,resources=kuadrants,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create

func (r *KuadrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling Kuadrant", "name", req.Name, "namespace", req.Namespace)

	// Ensure kuadrant-system namespace exists.
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: kuadrantNamespace}, ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: kuadrantNamespace,
			},
		}
		if err := r.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create kuadrant-system namespace")
			return ctrl.Result{}, err
		}
		logger.Info("Created kuadrant-system namespace")
	}

	// Get or create the Kuadrant CR.
	kq := &kuadrant.Kuadrant{}
	err := r.Get(ctx, types.NamespacedName{Name: kuadrantCRName, Namespace: kuadrantNamespace}, kq)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		kq = &kuadrant.Kuadrant{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "kuadrant.io/v1beta1",
				Kind:       "Kuadrant",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      kuadrantCRName,
				Namespace: kuadrantNamespace,
				Labels: map[string]string{
					LabelManagedBy: LabelManagedByValue,
				},
			},
		}
		if err := r.Create(ctx, kq); err != nil {
			if apierrors.IsAlreadyExists(err) {
				logger.V(1).Info("Kuadrant CR already exists (concurrent create)")
				return ctrl.Result{}, nil
			}
			logger.Error(err, "Failed to create Kuadrant CR")
			return ctrl.Result{}, err
		}
		logger.Info("Created Kuadrant CR", "namespace", kuadrantNamespace, "name", kuadrantCRName)
		return ctrl.Result{}, nil
	}

	// CR exists — ensure our managed-by label is present for drift detection.
	if kq.Labels == nil || kq.Labels[LabelManagedBy] != LabelManagedByValue {
		patch := kq.DeepCopy()
		if patch.Labels == nil {
			patch.Labels = make(map[string]string)
		}
		patch.Labels[LabelManagedBy] = LabelManagedByValue
		if err := r.Patch(ctx, patch, client.MergeFrom(kq)); err != nil {
			logger.Error(err, "Failed to patch Kuadrant CR labels")
			return ctrl.Result{}, err
		}
		logger.Info("Patched Kuadrant CR with managed-by label")
	}

	logger.V(1).Info("Kuadrant CR is present and correct")
	return ctrl.Result{}, nil
}

func (r *KuadrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&kuadrant.Kuadrant{}).
		Named("Kuadrant").
		Complete(r); err != nil {
		return err
	}

	// Bootstrap: the For() watch only fires on existing CR events, so we
	// need an initial reconcile to create the CR when none exists yet.
	return mgr.Add(&kuadrantBootstrap{reconciler: r})
}

// kuadrantBootstrap is a manager.Runnable that triggers one reconcile after
// the manager cache is synced, ensuring the Kuadrant CR is created on first
// startup even when no CR exists to generate watch events.
type kuadrantBootstrap struct {
	reconciler *KuadrantReconciler
}

func (b *kuadrantBootstrap) NeedLeaderElection() bool { return true }

func (b *kuadrantBootstrap) Start(ctx context.Context) error {
	kuadrantLogger.Info("Bootstrap: triggering initial Kuadrant reconcile")
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      kuadrantCRName,
			Namespace: kuadrantNamespace,
		},
	}

	const maxAttempts = 5
	for attempt := range maxAttempts {
		_, err := b.reconciler.Reconcile(ctx, req)
		if err == nil {
			return nil
		}
		kuadrantLogger.Error(err, "Bootstrap reconcile attempt failed", "attempt", attempt+1, "maxAttempts", maxAttempts)
		delay := time.Duration(attempt+1) * 5 * time.Second
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	kuadrantLogger.Info("Bootstrap reconcile exhausted retries — CR will be created on the next watch event or operator restart")
	return nil
}

// KuadrantCRDExists checks whether the kuadrant.io/v1beta1 Kuadrant CRD is
// registered on the cluster. Returns false if the CRD is not installed,
// making the controller a no-op.
func KuadrantCRDExists(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		kuadrantLogger.Error(err, "Failed to create discovery client for Kuadrant check - controller will not start")
		return false
	}

	for attempt := range 3 {
		if attempt > 0 {
			delay := time.Duration(attempt) * 5 * time.Second
			kuadrantLogger.Info("Retrying Kuadrant CRD discovery", "attempt", attempt+1, "delay", delay)
			time.Sleep(delay)
		}

		resources, err := dc.ServerResourcesForGroupVersion("kuadrant.io/v1beta1")
		if err != nil {
			kuadrantLogger.Info("Kuadrant CRD not found", "attempt", attempt+1, "error", err)
			continue
		}

		for _, r := range resources.APIResources {
			if r.Kind == "Kuadrant" {
				kuadrantLogger.Info("Kuadrant CRD detected: will manage Kuadrant operand CR")
				return true
			}
		}

		kuadrantLogger.Info("kuadrant.io/v1beta1 group exists but Kuadrant kind not found")
		return false
	}

	kuadrantLogger.Info("Kuadrant CRD not found after retries: Kuadrant operator not installed")
	return false
}
