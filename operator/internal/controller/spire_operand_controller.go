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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kd "github.com/rossoctl/operator/internal/discovery"
)

const (
	spireOperandName = "cluster"
)

var (
	spireOperandLogger = ctrl.Log.WithName("controller").WithName("SpireOperand")

	spiffeCSIDriverGVK = schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1alpha1", Kind: "SpiffeCSIDriver",
	}
	spireServerGVK = schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1alpha1", Kind: "SpireServer",
	}
	spireAgentGVK = schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1alpha1", Kind: "SpireAgent",
	}
	spireOIDCProviderGVK = schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1alpha1", Kind: "SpireOIDCDiscoveryProvider",
	}
)

// SpireOperandReconciler watches the ZTWIM CR and ensures all 5 SPIRE operand
// CRs exist with the desired spec, correcting drift on each reconcile.
type SpireOperandReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	TrustDomain string
	ClusterName string
}

// +kubebuilder:rbac:groups=operator.openshift.io,resources=zerotrustworkloadidentitymanagers,verbs=create;get;list;update;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=spiffecsidrivers,verbs=create;get;list;update;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=spireservers,verbs=create;get;list;update;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=spireagents,verbs=create;get;list;update;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=spireoidcdiscoveryproviders,verbs=create;get;list;update;watch

func (r *SpireOperandReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling SPIRE operand", "name", req.Name)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(kd.ZTWIMGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: spireOperandName}, existing); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("ZTWIM CR not found, bootstrap pending")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// trustDomain and clusterName are immutable on the ZTWIM CR — once set
	// by the initial creator (bootstrap or Helm), they cannot be changed.
	// Read existing values and use them for spec builders so we never
	// trigger a rejected update. Fall back to configured values only for
	// new CRs.
	effectiveTD := r.TrustDomain
	if td, found, _ := unstructured.NestedString(existing.Object, "spec", "trustDomain"); found && td != "" {
		effectiveTD = td
	}
	effectiveCN := r.ClusterName
	if cn, found, _ := unstructured.NestedString(existing.Object, "spec", "clusterName"); found && cn != "" {
		effectiveCN = cn
	}

	if err := r.ensureUnstructuredCR(ctx, kd.ZTWIMGVK, spireOperandName, r.ztwimSpec(effectiveTD, effectiveCN)); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring ZTWIM CR: %w", err)
	}

	if err := r.ensureChildren(ctx, effectiveTD); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring SPIRE children: %w", err)
	}

	logger.Info("SPIRE operand reconciliation complete")
	return ctrl.Result{}, nil
}

func (r *SpireOperandReconciler) ensureChildren(ctx context.Context, trustDomain string) error {
	children := []struct {
		gvk  schema.GroupVersionKind
		spec map[string]interface{}
	}{
		{spiffeCSIDriverGVK, r.spiffeCSIDriverSpec()},
		{spireServerGVK, r.spireServerSpec(trustDomain)},
		{spireAgentGVK, r.spireAgentSpec()},
		{spireOIDCProviderGVK, r.spireOIDCProviderSpec(trustDomain)},
	}
	for _, child := range children {
		if err := r.ensureUnstructuredCR(ctx, child.gvk, spireOperandName, child.spec); err != nil {
			return fmt.Errorf("ensuring %s: %w", child.gvk.Kind, err)
		}
	}
	return nil
}

func (r *SpireOperandReconciler) ensureUnstructuredCR(
	ctx context.Context, gvk schema.GroupVersionKind,
	name string, spec map[string]interface{},
) error {
	logger := log.FromContext(ctx)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		lbls := obj.GetLabels()
		if lbls == nil {
			lbls = make(map[string]string)
		}
		lbls[LabelManagedBy] = LabelManagedByValue
		obj.SetLabels(lbls)

		// Merge our fields into existing spec rather than replacing it.
		// The ZTWIM operator adds default fields (e.g. disableMigration)
		// that we don't manage — replacing the whole spec would cause an
		// update loop as each side overwrites the other's fields.
		existingSpec, _, _ := unstructured.NestedMap(obj.Object, "spec")
		if existingSpec == nil {
			existingSpec = make(map[string]interface{})
		}
		mergeNestedMap(existingSpec, spec)
		if err := unstructured.SetNestedField(obj.Object, existingSpec, "spec"); err != nil {
			return fmt.Errorf("setting spec on %s: %w", gvk.Kind, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	switch result {
	case controllerutil.OperationResultCreated:
		logger.Info("Created SPIRE operand CR", "kind", gvk.Kind, "name", name)
		if r.Recorder != nil {
			r.Recorder.Event(obj, "Normal", "Created",
				fmt.Sprintf("Created %s/%s", gvk.Kind, name))
		}
	case controllerutil.OperationResultUpdated:
		logger.Info("Updated SPIRE operand CR (drift corrected)", "kind", gvk.Kind, "name", name)
		if r.Recorder != nil {
			r.Recorder.Event(obj, "Normal", "Updated",
				fmt.Sprintf("Updated %s/%s (drift corrected)", gvk.Kind, name))
		}
	}

	return nil
}

// mergeNestedMap recursively merges src into dst. For nested maps, it recurses.
// For all other types, src values overwrite dst values. Keys in dst not present
// in src are preserved — this prevents overwriting fields set by the ZTWIM operator.
func mergeNestedMap(dst, src map[string]interface{}) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				mergeNestedMap(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

// --- Spec builders ---

func (r *SpireOperandReconciler) ztwimSpec(trustDomain, clusterName string) map[string]interface{} {
	if clusterName == "" {
		clusterName = "agent-platform"
	}
	return map[string]interface{}{
		"trustDomain":     trustDomain,
		"clusterName":     clusterName,
		"bundleConfigMap": "spire-bundle",
	}
}

func (r *SpireOperandReconciler) spiffeCSIDriverSpec() map[string]interface{} {
	return map[string]interface{}{
		"agentSocketPath": "/run/spire/agent-sockets",
		"pluginName":      "csi.spiffe.io",
	}
}

func (r *SpireOperandReconciler) spireServerSpec(trustDomain string) map[string]interface{} {
	return map[string]interface{}{
		"caSubject": map[string]interface{}{
			"commonName":   trustDomain,
			"country":      "US",
			"organization": "RH",
		},
		"persistence": map[string]interface{}{
			"size":       "5Gi",
			"accessMode": "ReadWriteOnce",
		},
		"datastore": map[string]interface{}{
			"databaseType":     "sqlite3",
			"connectionString": "/run/spire/data/datastore.sqlite3",
			"maxOpenConns":     int64(100),
			"maxIdleConns":     int64(2),
			"connMaxLifetime":  int64(3600),
		},
		"jwtIssuer": fmt.Sprintf("https://oidc-discovery-provider.%s", trustDomain),
	}
}

func (r *SpireOperandReconciler) spireAgentSpec() map[string]interface{} {
	return map[string]interface{}{
		"nodeAttestor": map[string]interface{}{
			"k8sPSATEnabled": "true",
		},
		"workloadAttestors": map[string]interface{}{
			"k8sEnabled": "true",
			"workloadAttestorsVerification": map[string]interface{}{
				"type": "auto",
			},
		},
	}
}

func (r *SpireOperandReconciler) spireOIDCProviderSpec(trustDomain string) map[string]interface{} {
	return map[string]interface{}{
		"csiDriverName": "csi.spiffe.io",
		"jwtIssuer":     fmt.Sprintf("https://oidc-discovery-provider.%s", trustDomain),
	}
}

// --- Controller setup ---

func mapChildToZTWIM(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: spireOperandName}},
	}
}

func (r *SpireOperandReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_, err := mgr.GetRESTMapper().RESTMapping(
		kd.ZTWIMGVK.GroupKind(), kd.ZTWIMGVK.Version,
	)
	if err != nil {
		if meta.IsNoMatchError(err) {
			log.Log.Info("ZTWIM CRD not registered, SPIRE operand controller will not start")
			return nil
		}
		return fmt.Errorf("checking ZTWIM CRD: %w", err)
	}

	ztwim := &unstructured.Unstructured{}
	ztwim.SetGroupVersionKind(kd.ZTWIMGVK)

	bld := ctrl.NewControllerManagedBy(mgr).
		For(ztwim).
		Named("spire-operand")

	for _, gvk := range []schema.GroupVersionKind{
		spiffeCSIDriverGVK, spireServerGVK, spireAgentGVK, spireOIDCProviderGVK,
	} {
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(gvk)
		bld.Watches(child, handler.EnqueueRequestsFromMapFunc(mapChildToZTWIM))
	}

	return bld.Complete(r)
}

// --- CRD existence check ---

func SpireOperandCRDExists(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		spireOperandLogger.Error(err, "Failed to create discovery client for SPIRE operand check")
		return false
	}

	for attempt := range 3 {
		if attempt > 0 {
			delay := time.Duration(attempt) * time.Second
			spireOperandLogger.Info("Retrying ZTWIM CRD discovery", "attempt", attempt+1, "delay", delay)
			time.Sleep(delay)
		}

		resources, err := dc.ServerResourcesForGroupVersion("operator.openshift.io/v1alpha1")
		if err != nil {
			spireOperandLogger.Info("ZTWIM CRD not found", "attempt", attempt+1, "error", err)
			continue
		}

		for _, r := range resources.APIResources {
			if r.Kind == "ZeroTrustWorkloadIdentityManager" {
				spireOperandLogger.Info("ZTWIM CRD detected: will manage SPIRE operand CRs")
				return true
			}
		}

		spireOperandLogger.Info("operator.openshift.io/v1alpha1 group exists but ZTWIM kind not found")
		return false
	}

	spireOperandLogger.Info("ZTWIM CRD not found after retries: SPIRE operand controller will not start")
	return false
}

// --- Bootstrap runnable ---

// SpireBootstrapRunnable creates the ZTWIM CR at startup to trigger the controller's watch.
type SpireBootstrapRunnable struct {
	Client      client.Client
	TrustDomain string
	ClusterName string
	Log         logr.Logger
}

func (b *SpireBootstrapRunnable) Start(ctx context.Context) error {
	b.Log.Info("SPIRE bootstrap: ensuring ZTWIM CR exists")

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(kd.ZTWIMGVK)
	if err := b.Client.Get(ctx, types.NamespacedName{Name: spireOperandName}, existing); err == nil {
		b.Log.Info("SPIRE bootstrap: ZTWIM CR already exists, skipping creation")
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("checking ZTWIM CR existence: %w", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kd.ZTWIMGVK)
	obj.SetName(spireOperandName)
	obj.SetLabels(map[string]string{
		LabelManagedBy: LabelManagedByValue,
	})

	clusterName := b.ClusterName
	if clusterName == "" {
		clusterName = "agent-platform"
	}
	spec := map[string]interface{}{
		"trustDomain":     b.TrustDomain,
		"clusterName":     clusterName,
		"bundleConfigMap": "spire-bundle",
	}
	if err := unstructured.SetNestedField(obj.Object, spec, "spec"); err != nil {
		return fmt.Errorf("setting ZTWIM spec: %w", err)
	}

	if err := b.Client.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			b.Log.Info("SPIRE bootstrap: ZTWIM CR created concurrently, skipping")
			return nil
		}
		return fmt.Errorf("creating ZTWIM CR: %w", err)
	}

	b.Log.Info("SPIRE bootstrap: created ZTWIM CR", "trustDomain", b.TrustDomain)
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (b *SpireBootstrapRunnable) NeedLeaderElection() bool {
	return true
}
