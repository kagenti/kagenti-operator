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

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
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
)

const (
	RootCACertName   = "istio-mesh-root-ca"
	RootCASecretName = "istio-mesh-root-ca-secret"
	RootCANamespace  = "cert-manager"

	IstioSystemCertName   = "istio-cacerts-default"
	IstioSystemSecretName = "istio-cacerts-default-cert"
	IstioSystemNamespace  = "istio-system"

	OpenShiftIngressCertName   = "istio-cacerts-openshift-gateway"
	OpenShiftIngressSecretName = "istio-cacerts-og-cert"
	OpenShiftIngressNamespace  = "openshift-ingress"

	CacertsSecretName = "cacerts"

	IstiodDeployment                 = "istiod"
	IstiodOpenShiftIngressDeployment = "istiod-openshift-gateway"
	ZtunnelDaemonSet                 = "ztunnel"
	ZtunnelNamespace                 = "istio-ztunnel"

	StaleConfigMapName = "istio-ca-root-cert"

	requeuePrecondition = 5 * time.Minute
	requeueReadiness    = 30 * time.Second
)

var (
	sharedTrustLogger = ctrl.Log.WithName("controller").WithName("SharedTrust")

	managedCertificates = []types.NamespacedName{
		{Name: RootCACertName, Namespace: RootCANamespace},
		{Name: IstioSystemCertName, Namespace: IstioSystemNamespace},
		{Name: OpenShiftIngressCertName, Namespace: OpenShiftIngressNamespace},
	}

	staleConfigMapNamespaces = []string{
		"rossoctl-system", "gateway-system", "keycloak",
		"mcp-system", "istio-system", "istio-ztunnel",
	}

	// intermediateConfigs matches the standard RHOAI/OSSM3 layout.
	// Values are intentionally hardcoded to match setup-rossoctl.sh.
	intermediateConfigs = []struct {
		SecretName string
		Namespace  string
	}{
		{IstioSystemSecretName, IstioSystemNamespace},
		{OpenShiftIngressSecretName, OpenShiftIngressNamespace},
	}
)

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=clusterissuers,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

type SharedTrustReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *SharedTrustReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling SharedTrust")

	if result, err := r.checkCertificates(ctx); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	if result, err := r.verifyFingerprints(ctx); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	needsRestart, err := r.reconcileCacertsSecrets(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if needsRestart {
		if err := r.restartWorkloads(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.Info("SharedTrust reconciliation complete", "restarted", needsRestart)
	return ctrl.Result{}, nil
}

func (r *SharedTrustReconciler) checkCertificates(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	for _, nn := range managedCertificates {
		cert := &cmv1.Certificate{}
		if err := r.Get(ctx, nn, cert); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("Certificate not found, requeueing", "certificate", nn)
				r.Recorder.Eventf(cert, corev1.EventTypeWarning, "CertificateMissing",
					"Certificate %s/%s not found", nn.Namespace, nn.Name)
				return ctrl.Result{RequeueAfter: requeuePrecondition}, nil
			}
			return ctrl.Result{}, fmt.Errorf("getting certificate %s: %w", nn, err)
		}

		ready := false
		for _, cond := range cert.Status.Conditions {
			if cond.Type == cmv1.CertificateConditionReady && cond.Status == cmmeta.ConditionTrue {
				ready = true
				break
			}
		}

		if !ready {
			logger.Info("Certificate not ready, requeueing", "certificate", nn)
			r.Recorder.Eventf(cert, corev1.EventTypeWarning, "CertificateNotReady",
				"Certificate %s/%s is not Ready", nn.Namespace, nn.Name)
			return ctrl.Result{RequeueAfter: requeueReadiness}, nil
		}
	}
	return ctrl.Result{}, nil
}

func (r *SharedTrustReconciler) verifyFingerprints(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	rootSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: RootCASecretName, Namespace: RootCANamespace}, rootSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting root CA secret: %w", err)
	}

	rootCertPEM := rootSecret.Data["tls.crt"]

	for _, ic := range intermediateConfigs {
		intSecret := &corev1.Secret{}
		nn := types.NamespacedName{Name: ic.SecretName, Namespace: ic.Namespace}
		if err := r.Get(ctx, nn, intSecret); err != nil {
			return ctrl.Result{}, fmt.Errorf("getting intermediate secret %s: %w", nn, err)
		}

		match, err := verifyCAFingerprint(rootCertPEM, intSecret.Data["ca.crt"])
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("verifying fingerprint for %s: %w", nn, err)
		}

		if !match {
			logger.Info("Fingerprint mismatch, deleting intermediate secret for re-issuance",
				"secret", nn)
			r.Recorder.Eventf(intSecret, corev1.EventTypeWarning, "FingerprintMismatch",
				"Root CA fingerprint mismatch for %s/%s — deleting to force re-issuance",
				nn.Namespace, nn.Name)
			if err := r.Delete(ctx, intSecret); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting mismatched secret %s: %w", nn, err)
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	return ctrl.Result{}, nil
}

func (r *SharedTrustReconciler) reconcileCacertsSecrets(ctx context.Context) (bool, error) {
	anyChanged := false

	for _, ic := range intermediateConfigs {
		intSecret := &corev1.Secret{}
		nn := types.NamespacedName{Name: ic.SecretName, Namespace: ic.Namespace}
		if err := r.Get(ctx, nn, intSecret); err != nil {
			return false, fmt.Errorf("getting intermediate secret %s: %w", nn, err)
		}

		desired := buildCacertsData(intSecret.Data["tls.crt"], intSecret.Data["tls.key"], intSecret.Data["ca.crt"])

		cacertsNN := types.NamespacedName{Name: CacertsSecretName, Namespace: ic.Namespace}
		existing := &corev1.Secret{}
		err := r.Get(ctx, cacertsNN, existing)

		switch {
		case apierrors.IsNotFound(err):
			secret := &corev1.Secret{}
			secret.Name = CacertsSecretName
			secret.Namespace = ic.Namespace
			secret.Type = corev1.SecretTypeOpaque
			secret.Data = desired
			if err := controllerutil.SetOwnerReference(intSecret, secret, r.Scheme); err != nil {
				return false, fmt.Errorf("setting owner reference for cacerts secret in %s: %w", ic.Namespace, err)
			}
			if err := r.Create(ctx, secret); err != nil {
				return false, fmt.Errorf("creating cacerts secret in %s: %w", ic.Namespace, err)
			}
			anyChanged = true

		case err != nil:
			return false, fmt.Errorf("getting cacerts secret in %s: %w", ic.Namespace, err)

		default:
			if !secretDataEqual(existing.Data, desired) {
				existing.Data = desired
				if err := r.Update(ctx, existing); err != nil {
					return false, fmt.Errorf("updating cacerts secret in %s: %w", ic.Namespace, err)
				}
				anyChanged = true
			}
		}
	}

	return anyChanged, nil
}

func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || string(v) != string(bv) {
			return false
		}
	}
	return true
}

func (r *SharedTrustReconciler) restartWorkloads(ctx context.Context) error {
	logger := log.FromContext(ctx)

	r.deleteStaleConfigMaps(ctx)

	restartAnnotation := map[string]string{
		"kubectl.kubernetes.io/restartedAt": time.Now().Format(time.RFC3339),
	}

	deployments := []types.NamespacedName{
		{Name: IstiodDeployment, Namespace: IstioSystemNamespace},
		{Name: IstiodOpenShiftIngressDeployment, Namespace: OpenShiftIngressNamespace},
	}

	for _, nn := range deployments {
		if err := r.rolloutRestartDeployment(ctx, nn, restartAnnotation); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("Deployment not found, skipping restart", "deployment", nn)
				continue
			}
			return err
		}
		logger.Info("Triggered deployment restart", "deployment", nn)
	}

	ztunnelNN := types.NamespacedName{Name: ZtunnelDaemonSet, Namespace: ZtunnelNamespace}
	if err := r.rolloutRestartDaemonSet(ctx, ztunnelNN, restartAnnotation); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("DaemonSet not found, skipping restart", "daemonset", ztunnelNN)
			return nil
		}
		return err
	}
	logger.Info("Triggered daemonset restart", "daemonset", ztunnelNN)

	return nil
}

func (r *SharedTrustReconciler) deleteStaleConfigMaps(ctx context.Context) {
	logger := log.FromContext(ctx)
	for _, ns := range staleConfigMapNamespaces {
		cm := &corev1.ConfigMap{}
		nn := types.NamespacedName{Name: StaleConfigMapName, Namespace: ns}
		if err := r.Get(ctx, nn, cm); err != nil {
			continue
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to delete stale ConfigMap", "configmap", nn)
		}
	}
}

func (r *SharedTrustReconciler) rolloutRestartDeployment(ctx context.Context, nn types.NamespacedName, annotations map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deploy := &appsv1.Deployment{}
		if err := r.Get(ctx, nn, deploy); err != nil {
			return err
		}
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			deploy.Spec.Template.Annotations[k] = v
		}
		return r.Update(ctx, deploy)
	})
}

func (r *SharedTrustReconciler) rolloutRestartDaemonSet(ctx context.Context, nn types.NamespacedName, annotations map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ds := &appsv1.DaemonSet{}
		if err := r.Get(ctx, nn, ds); err != nil {
			return err
		}
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			ds.Spec.Template.Annotations[k] = v
		}
		return r.Update(ctx, ds)
	})
}

func (r *SharedTrustReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToCertificateReconcile := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			for _, mc := range managedCertificates {
				if obj.GetName() == mc.Name && obj.GetNamespace() == mc.Namespace {
					return []reconcile.Request{{
						NamespacedName: types.NamespacedName{
							Name:      "shared-trust",
							Namespace: IstioSystemNamespace,
						},
					}}
				}
			}
			return nil
		},
	)

	mapCacertsSecretToReconcile := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			if obj.GetName() != CacertsSecretName {
				return nil
			}
			for _, ic := range intermediateConfigs {
				if obj.GetNamespace() == ic.Namespace {
					return []reconcile.Request{{
						NamespacedName: types.NamespacedName{
							Name:      "shared-trust",
							Namespace: IstioSystemNamespace,
						},
					}}
				}
			}
			return nil
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named("SharedTrust").
		Watches(&cmv1.Certificate{}, mapToCertificateReconcile).
		Watches(&corev1.Secret{}, mapCacertsSecretToReconcile).
		Complete(r)
}

func CertManagerCRDExists(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		sharedTrustLogger.Error(err, "Failed to create discovery client for cert-manager check")
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var found bool
	_ = wait.PollUntilContextTimeout(ctx, 5*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		resources, err := dc.ServerResourcesForGroupVersion("cert-manager.io/v1")
		if err != nil {
			sharedTrustLogger.Info("cert-manager CRDs not found, retrying", "error", err)
			return false, nil
		}

		for _, r := range resources.APIResources {
			if r.Kind == "Certificate" {
				found = true
				return true, nil
			}
		}

		sharedTrustLogger.Info("cert-manager.io/v1 group exists but Certificate kind not found")
		return true, nil
	})

	if found {
		sharedTrustLogger.Info("cert-manager CRDs detected: SharedTrust controller will start")
	} else {
		sharedTrustLogger.Info("cert-manager CRDs not found after retries: SharedTrust controller will not start")
	}

	return found
}
