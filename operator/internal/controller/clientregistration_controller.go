/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	"github.com/rossoctl/operator/internal/clientreg"
	"github.com/rossoctl/operator/internal/keycloak"
)

// Well-known namespace resources.
const (
	authbridgeConfigConfigMap = "authbridge-config"

	// keycloakInitialAdminSecret is the RHBK-managed secret with keys "username"/"password".
	keycloakInitialAdminSecret = "keycloak-initial-admin"

	// AnnotationKeycloakClientSecretName must match rossoctl-webhook injector.AnnotationKeycloakClientSecretName.
	// Re-exported from internal/clientreg to give both the controller and the webhook one source of truth.
	AnnotationKeycloakClientSecretName = clientreg.AnnotationKeycloakClientSecretName
)

// ClientRegistrationReconciler registers OAuth clients in Keycloak and patches agent/tool workloads that
// use the default path (label absent or not "true") so the webhook injects envoy/SPIRE without the
// legacy registration sidecar. The Secret is created before the pod template annotation is set so new Pods
// never reference a missing Secret; the webhook mounts the Secret for injected sidecars that use shared-data.
type ClientRegistrationReconciler struct {
	client.Client
	// APIReader reads authbridge-config from agent namespaces and keycloak-initial-admin
	// from the Keycloak namespace via the API server (bypasses manager cache).
	APIReader client.Reader
	Scheme    *runtime.Scheme

	// OperatorNamespace is the namespace where the operator is running.
	OperatorNamespace string

	// KeycloakAdminSecretNamespace is where keycloak-initial-admin lives (default: "keycloak").
	KeycloakAdminSecretNamespace string

	SpireTrustDomain string
	// KeycloakAdminTokenCache caches admin password-grant tokens by Keycloak URL and credentials to
	// avoid a token request on every reconcile. If nil, PasswordGrantToken is used without caching.
	// Only used when UseSpiffeAuth is false.
	KeycloakAdminTokenCache *keycloak.CachedAdminTokenProvider

	// UseSpiffeAuth enables JWT-SVID authentication instead of admin credentials.
	// When true, the operator authenticates to Keycloak with its JWT-SVID and uses
	// the Admin API with manage-clients role. When false, uses admin credentials.
	UseSpiffeAuth bool

	// SpiffeSocket is the path to the SPIFFE Workload API socket (e.g., unix:///run/spire/sockets/agent.sock).
	// Only used when UseSpiffeAuth is true. Used to fetch JWT-SVIDs via go-spiffe SDK.
	SpiffeSocket string

	// OperatorClientID is the operator's SPIFFE ID (e.g., spiffe://localtest.me/ns/rossoctl-operator-system/sa/...).
	// Only used when UseSpiffeAuth is true.
	OperatorClientID string

	// Recorder emits Kubernetes Events to surface configuration issues visible in kubectl describe.
	Recorder record.EventRecorder
}

func (r *ClientRegistrationReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// resolveKeycloakAdminCredentials reads keycloak-initial-admin from the Keycloak namespace.
func (r *ClientRegistrationReconciler) resolveKeycloakAdminCredentials(ctx context.Context) (string, string, error) {
	kcNS := r.KeycloakAdminSecretNamespace
	if kcNS == "" {
		kcNS = "keycloak"
	}

	secret := &corev1.Secret{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: kcNS, Name: keycloakInitialAdminSecret}, secret); err != nil {
		return "", "", err
	}
	return string(secret.Data["username"]), string(secret.Data["password"]), nil
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch

func (r *ClientRegistrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	globalOn, clientRegGate, injectTools, err := readClusterFeatureGates(ctx, r.Client)
	if err != nil {
		logger.Error(err, "read cluster feature gates")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !globalOn || !clientRegGate {
		logger.V(1).Info("skipping operator client registration: cluster feature gates disabled injection")
		return ctrl.Result{}, nil
	}

	dep := &appsv1.Deployment{}
	err = r.Get(ctx, req.NamespacedName, dep)
	if err == nil {
		return r.reconcileOne(ctx, dep, injectTools, dep.Name, &dep.Spec.Template,
			func(ctx context.Context) error {
				return retry.RetryOnConflict(retry.DefaultRetry, func() error {
					d := &appsv1.Deployment{}
					if err := r.Get(ctx, req.NamespacedName, d); err != nil {
						return err
					}
					if !injectKeycloakClientCredentialsAnnotation(&d.Spec.Template, keycloakClientCredentialsSecretName(d.Namespace, d.Name)) {
						return nil
					}
					return r.Update(ctx, d)
				})
			})
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	sts := &appsv1.StatefulSet{}
	err = r.Get(ctx, req.NamespacedName, sts)
	if err == nil {
		return r.reconcileOne(ctx, sts, injectTools, sts.Name, &sts.Spec.Template,
			func(ctx context.Context) error {
				return retry.RetryOnConflict(retry.DefaultRetry, func() error {
					s := &appsv1.StatefulSet{}
					if err := r.Get(ctx, req.NamespacedName, s); err != nil {
						return err
					}
					if !injectKeycloakClientCredentialsAnnotation(&s.Spec.Template, keycloakClientCredentialsSecretName(s.Namespace, s.Name)) {
						return nil
					}
					return r.Update(ctx, s)
				})
			})
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	sbx := &unstructured.Unstructured{}
	sbx.SetGroupVersionKind(sandboxGVK)
	if err = r.Get(ctx, req.NamespacedName, sbx); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	podLabels, _, _ := unstructured.NestedStringMap(sbx.Object, "spec", "podTemplate", "metadata", "labels")
	podAnnotations, _, _ := unstructured.NestedStringMap(sbx.Object, "spec", "podTemplate", "metadata", "annotations")
	saName, _, _ := unstructured.NestedString(sbx.Object, "spec", "podTemplate", "spec", "serviceAccountName")
	syntheticTemplate := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: podAnnotations},
		Spec:       corev1.PodSpec{ServiceAccountName: saName},
	}
	return r.reconcileOne(ctx, sbx, injectTools, sbx.GetName(), syntheticTemplate,
		func(ctx context.Context) error {
			return retry.RetryOnConflict(retry.DefaultRetry, func() error {
				fresh := &unstructured.Unstructured{}
				fresh.SetGroupVersionKind(sandboxGVK)
				if err := r.Get(ctx, req.NamespacedName, fresh); err != nil {
					return err
				}
				secretName := keycloakClientCredentialsSecretName(fresh.GetNamespace(), fresh.GetName())
				annotations, _, _ := unstructured.NestedStringMap(fresh.Object, "spec", "podTemplate", "metadata", "annotations")
				if annotations != nil && annotations[AnnotationKeycloakClientSecretName] == secretName {
					return nil
				}
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations[AnnotationKeycloakClientSecretName] = secretName
				if err := unstructured.SetNestedStringMap(fresh.Object, annotations, "spec", "podTemplate", "metadata", "annotations"); err != nil {
					return fmt.Errorf("setting podTemplate annotations: %w", err)
				}
				return r.Update(ctx, fresh)
			})
		})
}

func (r *ClientRegistrationReconciler) reconcileOne(
	ctx context.Context,
	owner client.Object,
	injectTools bool,
	workloadName string,
	template *corev1.PodTemplateSpec,
	patchTemplate func(context.Context) error,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	labels := template.Labels
	if reason := keycloakClientCredentialsSkipReason(labels, injectTools); reason != "" {
		logger.Info("skipping operator client registration for workload",
			"namespace", owner.GetNamespace(),
			"workload", workloadName,
			"reason", reason)
		return ctrl.Result{}, nil
	}

	ns := owner.GetNamespace()

	ab, err := readAuthbridgeConfigMap(ctx, r.uncachedReader(), ns)
	if err != nil {
		logger.Error(err, "read authbridge-config")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if ab.KeycloakURL == "" || ab.KeycloakRealm == "" {
		logger.Info("waiting for KEYCLOAK_URL/KEYCLOAK_REALM in authbridge-config", "namespace", ns)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	spireEnabled := r.SpireTrustDomain != ""
	clientName := ns + "/" + workloadName
	clientID, err := resolveKeycloakClientID(ns, workloadName, template.Spec.ServiceAccountName, spireEnabled, r.SpireTrustDomain)
	if err != nil {
		logger.Info("cannot resolve Keycloak client id yet", "reason", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	authType := strings.TrimSpace(ab.ClientAuthType)
	if authType == "" {
		authType = "client-secret"
	}
	tokenExch := strings.TrimSpace(ab.KeycloakTokenExchangeEnabled) != "false"
	audienceScopeOn := strings.TrimSpace(ab.KeycloakAudienceScopeEnabled) != "false"

	kc := keycloak.Admin{BaseURL: ab.KeycloakURL, HTTPClient: keycloak.DefaultHTTPClient()}
	var token string

	// Authenticate to Keycloak: use JWT-SVID if enabled, otherwise admin credentials
	if r.UseSpiffeAuth {
		// SPIFFE authentication path
		// Validate OperatorClientID before file I/O to fail fast on misconfiguration
		if r.OperatorClientID == "" {
			err := fmt.Errorf("OperatorClientID not configured")
			logger.Error(err, "SPIFFE auth requires OperatorClientID")
			if r.Recorder != nil {
				r.Recorder.Event(owner, corev1.EventTypeWarning, "OperatorClientIDMissing",
					"UseSpiffeAuth=true but OperatorClientID is empty. Check operator configuration.")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		if r.SpiffeSocket == "" {
			err := fmt.Errorf("SpiffeSocket is required when UseSpiffeAuth=true")
			logger.Error(err, "missing SPIFFE socket path")
			if r.Recorder != nil {
				r.Recorder.Event(owner, corev1.EventTypeWarning, "SpiffeSocketMissing",
					"UseSpiffeAuth=true but SpiffeSocket is empty. Check operator configuration.")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Fetch JWT-SVID from SPIRE via Workload API
		// Per RFC 7523 and Keycloak SPIFFE authentication: the JWT audience must match
		// Keycloak's realm issuer URL exactly. Query the OIDC discovery endpoint to get
		// the authoritative issuer value, since it may differ from the in-cluster service URL.
		realmIssuer, err := r.getKeycloakIssuer(ctx, ab.KeycloakURL, ab.KeycloakRealm)
		if err != nil {
			logger.Error(err, "Failed to get Keycloak issuer URL")
			if r.Recorder != nil {
				r.Recorder.Eventf(owner, corev1.EventTypeWarning, "IssuerLookupFailed",
					"Failed to query Keycloak OIDC discovery: %v", err)
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		jwtSVID, err := r.fetchJWTSVID(ctx, realmIssuer)
		if err != nil {
			logger.Error(err, "JWT-SVID fetch failed")
			if r.Recorder != nil {
				r.Recorder.Eventf(owner, corev1.EventTypeWarning, "JWTSVIDFetchFailed",
					"Failed to fetch JWT-SVID from SPIRE: %v", err)
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// WARNING: JWT-SVID is a bearer token - must never appear in logs or error messages
		// to prevent token exposure. All code paths must handle jwtSVID as sensitive data.
		token, err = kc.JWTSVIDGrantToken(ctx, ab.KeycloakRealm, r.OperatorClientID, jwtSVID)
		if err != nil {
			logger.Error(err, "Keycloak JWT-SVID authentication failed")
			if r.Recorder != nil {
				r.Recorder.Event(owner, corev1.EventTypeWarning, "KeycloakAuthFailed",
					"Failed to authenticate to Keycloak with JWT-SVID. Check SPIFFE IdP configuration in Keycloak.")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		logger.V(1).Info("authenticated with JWT-SVID", "clientId", r.OperatorClientID)
	} else {
		// Admin credentials path (legacy)
		adminUser, adminPass, err := r.resolveKeycloakAdminCredentials(ctx)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("waiting for Keycloak admin secret")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		if adminUser == "" || adminPass == "" {
			logger.Info("Keycloak admin secret missing credentials")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if r.KeycloakAdminTokenCache != nil {
			token, err = r.KeycloakAdminTokenCache.Token(ctx, &kc, adminUser, adminPass)
		} else {
			token, err = kc.PasswordGrantToken(ctx, adminUser, adminPass)
		}
		if err != nil {
			logger.Error(err, "Keycloak admin token failed")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		logger.V(1).Info("authenticated with admin credentials")
	}
	agentClientUUID, clientSecret, err := kc.RegisterOrFetchClientWithToken(ctx, token, keycloak.ClientRegistrationParams{
		Realm:               ab.KeycloakRealm,
		ClientID:            clientID,
		ClientName:          clientName,
		ClientAuthType:      authType,
		SpiffeIDPAlias:      ab.SpiffeIDPAlias,
		TokenExchangeEnable: tokenExch,
	})
	if err != nil {
		logger.Error(err, "Keycloak client registration failed", "clientId", clientID)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := kc.EnsureAudienceScope(ctx, token, keycloak.AudienceParams{
		Realm:                ab.KeycloakRealm,
		ClientName:           clientName,
		AudienceClientID:     clientID,
		PlatformClientIDs:    parsePlatformClientIDs(ab.PlatformClientIDs),
		AudienceScopeEnabled: audienceScopeOn,
		AgentClientUUID:      agentClientUUID,
	}); err != nil {
		logger.Error(err, "Keycloak audience scope management failed (credentials will still be written)",
			"clientId", clientID)
	}

	// In federated-jwt mode pass an empty clientSecret so ensureClientCredentialsSecret
	// writes only client-id.txt (not client-secret.txt). The inbound jwt-validation
	// plugin in AuthBridge reads client-id.txt to determine the expected audience; without
	// it, all inbound requests to the agent are rejected with 503 indefinitely.
	secretName := keycloakClientCredentialsSecretName(ns, workloadName)
	credSecret := clientSecret
	if authType == "federated-jwt" {
		credSecret = ""
	}
	if err := r.ensureClientCredentialsSecret(ctx, owner, secretName, clientID, credSecret); err != nil {
		logger.Error(err, "ensure client credentials secret")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err := patchTemplate(ctx); err != nil {
		logger.Error(err, "patch workload pod template")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("operator client registration applied",
		"workload", workloadName, "namespace", ns, "secret", secretName)
	return ctrl.Result{}, nil
}

func injectKeycloakClientCredentialsAnnotation(template *corev1.PodTemplateSpec, secretName string) bool {
	if template.Annotations != nil && template.Annotations[AnnotationKeycloakClientSecretName] == secretName {
		return false
	}
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[AnnotationKeycloakClientSecretName] = secretName
	return true
}

// keycloakClientCredentialsSkipReason and workloadWantsOperatorClientReg delegate to internal/clientreg
// so the controller's reconcile decision and the webhook's admission decision stay in lockstep.
func keycloakClientCredentialsSkipReason(labels map[string]string, injectTools bool) string {
	return clientreg.SkipReason(labels, injectTools)
}

func workloadWantsOperatorClientReg(labels map[string]string, injectTools bool) bool {
	return clientreg.WorkloadWantsOperatorClientReg(labels, injectTools)
}

type authbridgeConfig struct {
	KeycloakURL                  string
	KeycloakRealm                string
	ClientAuthType               string
	SpiffeIDPAlias               string
	KeycloakTokenExchangeEnabled string
	PlatformClientIDs            string
	KeycloakAudienceScopeEnabled string
}

func readAuthbridgeConfigMap(ctx context.Context, c client.Reader, namespace string) (authbridgeConfig, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: authbridgeConfigConfigMap}, cm)
	if apierrors.IsNotFound(err) {
		return authbridgeConfig{}, nil
	}
	if err != nil {
		return authbridgeConfig{}, err
	}
	if cm.Data == nil {
		return authbridgeConfig{}, nil
	}
	return authbridgeConfig{
		KeycloakURL:                  cm.Data["KEYCLOAK_URL"],
		KeycloakRealm:                cm.Data["KEYCLOAK_REALM"],
		ClientAuthType:               cm.Data["CLIENT_AUTH_TYPE"],
		SpiffeIDPAlias:               cm.Data["SPIFFE_IDP_ALIAS"],
		KeycloakTokenExchangeEnabled: cm.Data["KEYCLOAK_TOKEN_EXCHANGE_ENABLED"],
		PlatformClientIDs:            cm.Data["PLATFORM_CLIENT_IDS"],
		KeycloakAudienceScopeEnabled: cm.Data["KEYCLOAK_AUDIENCE_SCOPE_ENABLED"],
	}, nil
}

func parsePlatformClientIDs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{"rossoctl"}
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"rossoctl"}
	}
	return out
}

func readClusterFeatureGates(ctx context.Context, c client.Reader) (globalOn, clientReg, injectTools bool, err error) {
	globalOn, clientReg, injectTools = true, true, false
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ClusterDefaultsNamespace, Name: ClusterFeatureGatesConfigMapName}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return globalOn, clientReg, injectTools, nil
		}
		return false, false, false, err
	}
	if cm.Data == nil {
		return globalOn, clientReg, injectTools, nil
	}

	parseGates := func(raw string) (bool, map[string]interface{}) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return false, nil
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(raw), &m); err != nil || len(m) == 0 {
			return false, nil
		}
		return true, m
	}

	applyGates := func(m map[string]interface{}) {
		if v, ok := m["globalEnabled"].(bool); ok {
			globalOn = v
		}
		if v, ok := m["clientRegistration"].(bool); ok {
			clientReg = v
		}
		if v, ok := m["injectTools"].(bool); ok {
			injectTools = v
		}
	}

	// Prefer the well-known key used by the webhook's FeatureGateLoader.
	if raw, ok := cm.Data["feature-gates.yaml"]; ok {
		if valid, m := parseGates(raw); valid {
			applyGates(m)
			return globalOn, clientReg, injectTools, nil
		}
	}

	// Fallback: try remaining keys in sorted order for determinism.
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		if k == "feature-gates.yaml" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if valid, m := parseGates(cm.Data[k]); valid {
			applyGates(m)
			return globalOn, clientReg, injectTools, nil
		}
	}
	return globalOn, clientReg, injectTools, nil
}

func resolveKeycloakClientID(namespace, workloadName, serviceAccount string, spireEnabled bool, trustDomain string) (string, error) {
	sa := strings.TrimSpace(serviceAccount)
	if sa == "" {
		sa = "default"
	}
	if !spireEnabled {
		return namespace + "/" + workloadName, nil
	}
	if sa == "default" {
		return "", fmt.Errorf("SPIRE enabled: set spec.template.spec.serviceAccountName to a dedicated ServiceAccount (not default) on the workload for a stable SPIFFE client ID")
	}
	if trustDomain == "" {
		return "", fmt.Errorf("SPIRE enabled: operator --spire-trust-domain is required for operator-managed client registration")
	}
	return fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, namespace, sa), nil
}

// keycloakClientCredentialsSecretName is a thin alias over the shared helper so call sites in this
// package stay unchanged. The shared implementation lives in internal/clientreg so the AuthBridge
// mutating webhook can compute the same name at admission time.
func keycloakClientCredentialsSecretName(namespace, workload string) string {
	return clientreg.KeycloakClientCredentialsSecretName(namespace, workload)
}

func (r *ClientRegistrationReconciler) ensureClientCredentialsSecret(ctx context.Context, owner client.Object, secretName, clientID, clientSecret string) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: owner.GetNamespace(),
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels[LabelManagedBy] = LabelManagedByValue
		sec.Type = corev1.SecretTypeOpaque
		if sec.StringData == nil {
			sec.StringData = map[string]string{}
		}
		// In federated-jwt mode clientSecret is empty — agents authenticate via JWT-SVID.
		// We still write client-secret.txt (as an empty string) so the webhook's subPath
		// mount produces a file rather than a directory (Kubernetes creates a directory
		// when a subPath key is absent from the Secret).
		sec.StringData["client-secret.txt"] = clientSecret
		sec.StringData["client-id.txt"] = clientID
		return controllerutil.SetControllerReference(owner, sec, r.Scheme)
	})
	return err
}

func clientRegistrationWorkloadPredicate(obj client.Object) bool {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return workloadWantsOperatorClientReg(o.Spec.Template.Labels, true)
	case *appsv1.StatefulSet:
		return workloadWantsOperatorClientReg(o.Spec.Template.Labels, true)
	case *unstructured.Unstructured:
		if o.GroupVersionKind() != sandboxGVK {
			return false
		}
		labels, _, _ := unstructured.NestedStringMap(o.Object, "spec", "podTemplate", "metadata", "labels")
		return workloadWantsOperatorClientReg(labels, true)
	default:
		return false
	}
}

// SetupWithManager registers the controller. injectTools is resolved at reconcile time from cluster
// feature gates; the predicate uses injectTools=true so tool workloads are not dropped before gates load.
func (r *ClientRegistrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pred := predicate.NewPredicateFuncs(clientRegistrationWorkloadPredicate)
	b := ctrl.NewControllerManagedBy(mgr).
		Named("clientregistration").
		For(&appsv1.Deployment{}, builder.WithPredicates(pred)).
		Watches(
			&appsv1.StatefulSet{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(pred),
		)

	if SandboxCRDExists(mgr.GetConfig()) {
		sandboxObj := &unstructured.Unstructured{}
		sandboxObj.SetGroupVersionKind(sandboxGVK)
		b = b.Watches(
			sandboxObj,
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(pred),
		)
	}

	return b.Complete(r)
}

// fetchJWTSVID fetches a JWT-SVID from the SPIRE Workload API for the given audience.
// Returns the JWT token as a string or an error if fetching fails.
func (r *ClientRegistrationReconciler) fetchJWTSVID(ctx context.Context, audience string) (string, error) {
	client, err := workloadapi.New(ctx, workloadapi.WithAddr(r.SpiffeSocket))
	if err != nil {
		return "", fmt.Errorf("failed to create SPIFFE Workload API client: %w", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			// Log close error but don't override the function's return error
			ctrl.Log.WithName("fetchJWTSVID").Error(closeErr, "failed to close SPIFFE Workload API client")
		}
	}()

	svid, err := client.FetchJWTSVID(ctx, jwtsvid.Params{
		Audience: audience,
	})
	if err != nil {
		return "", fmt.Errorf("failed to fetch JWT-SVID: %w", err)
	}

	return svid.Marshal(), nil
}

// getKeycloakIssuer queries the Keycloak OIDC discovery endpoint to get the authoritative
// issuer URL. This is necessary because the issuer may be a public URL (e.g., keycloak.localtest.me)
// while the KeycloakURL in authbridge-config is the in-cluster service address.
func (r *ClientRegistrationReconciler) getKeycloakIssuer(ctx context.Context, keycloakURL, realm string) (string, error) {
	discoveryURL := strings.TrimSuffix(keycloakURL, "/") + "/realms/" + realm + "/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create OIDC discovery request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query OIDC discovery endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC discovery returned status %d", resp.StatusCode)
	}

	var config struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return "", fmt.Errorf("failed to decode OIDC discovery response: %w", err)
	}

	if config.Issuer == "" {
		return "", fmt.Errorf("OIDC discovery response missing issuer field")
	}

	return config.Issuer, nil
}
