package controller

import (
	"context"
	"fmt"
	"time"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

// tlsBridgeSelfSignedIssuer is the per-namespace SelfSigned issuer that
// bootstraps each agent's CA Certificate.
const tlsBridgeSelfSignedIssuer = "authbridge-tls-bridge-selfsigned"

// RBAC for the per-agent CA reconciler — create/manage cert-manager Issuers +
// Certificates. Free-standing comment group (not the struct doc comment) so
// controller-gen's rbac generator collects it, matching SharedTrustReconciler.
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers,verbs=get;list;watch;create;update;patch;delete

// TLSBridgeCAReconciler provisions the per-agent cert-manager CA that backs the
// AuthBridge TLS bridge. For an AgentRuntime with spec.tlsBridgeMode=enabled it
// ensures a namespace SelfSigned Issuer and a CA Certificate (isCA, cert-sign,
// NO name constraints) whose Secret the webhook hard-mounts into the sidecar.
// cert-manager issues the Secret; the hard mount blocks pod start until it
// exists, which solves the startup ordering race.
//
// Enablement is keyed solely on the per-agent CR field (like mtlsMode — no
// cluster feature gate). The controller is only registered when cert-manager
// CRDs are present (see cmd/main.go).
type TLSBridgeCAReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *TLSBridgeCAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ar := &agentv1alpha1.AgentRuntime{}
	if err := r.Get(ctx, req.NamespacedName, ar); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ar.Spec.TLSBridgeMode != agentv1alpha1.TLSBridgeModeEnabled {
		// Disabled (or unset). The Secret, if any, is garbage-collected via the
		// owner reference when the agent's mode flips or the AgentRuntime is deleted.
		return ctrl.Result{}, nil
	}

	// 1) Namespace SelfSigned Issuer (shared, idempotent).
	issuer := &cmv1.Issuer{ObjectMeta: metav1.ObjectMeta{Name: tlsBridgeSelfSignedIssuer, Namespace: ar.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.Spec = cmv1.IssuerSpec{IssuerConfig: cmv1.IssuerConfig{SelfSigned: &cmv1.SelfSignedIssuer{}}}
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure selfsigned issuer: %w", err)
	}

	// 2) Per-agent CA Certificate (isCA + cert-sign; NO nameConstraints). Must
	//    satisfy authbridge FileSource's load-time validation (IsCA +
	//    KeyUsageCertSign + cert/key match), or the sidecar refuses to start.
	//    Future hardening (deferred): cert-manager nameConstraints could scope
	//    this CA to the agent's egress hosts so an exfiltrated sidecar key can't
	//    mint a leaf for arbitrary hosts. Left out for now to avoid the
	//    cert-manager NameConstraints feature-gate dependency; containment today
	//    is per-agent isolation + sidecar-only 0o444 key + rotation.
	// Name the Secret after the TARGET WORKLOAD, not the AgentRuntime CR: the
	// injecting webhook keys every per-agent resource (incl. this mount) off the
	// workload name (resourceName == spec.targetRef.name), which can differ from
	// the CR's metadata.name. Mismatch here => the webhook mounts a Secret that
	// never exists => pod stuck Pending.
	secretName := ar.Spec.TargetRef.Name + agentv1alpha1.TLSBridgeCASecretSuffix
	cert := &cmv1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ar.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.Spec = cmv1.CertificateSpec{
			IsCA:        true,
			CommonName:  "authbridge-tls-bridge-ca-" + ar.Name,
			SecretName:  secretName,
			Duration:    &metav1.Duration{Duration: 90 * 24 * time.Hour},
			RenewBefore: &metav1.Duration{Duration: 15 * 24 * time.Hour},
			PrivateKey:  &cmv1.CertificatePrivateKey{Algorithm: cmv1.ECDSAKeyAlgorithm, Size: 256},
			Usages:      []cmv1.KeyUsage{cmv1.UsageCertSign, cmv1.UsageDigitalSignature},
			IssuerRef:   cmmeta.IssuerReference{Name: tlsBridgeSelfSignedIssuer, Kind: "Issuer", Group: "cert-manager.io"},
		}
		return controllerutil.SetControllerReference(ar, cert, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure CA certificate: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *TLSBridgeCAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentv1alpha1.AgentRuntime{}).
		Owns(&cmv1.Certificate{}).
		Named("tlsbridge-ca").
		Complete(r)
}
