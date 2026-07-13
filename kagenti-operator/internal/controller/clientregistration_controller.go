/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
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

	"github.com/kagenti/operator/internal/clientreg"
	"github.com/kagenti/operator/internal/keycloak"
)

// Well-known namespace resources.
const (
	authbridgeConfigConfigMap = "authbridge-config"

	// keycloakInitialAdminSecret is the RHBK-managed secret with keys "username"/"password".
	keycloakInitialAdminSecret = "keycloak-initial-admin"

	// LabelClientRegistrationInject: when not "true", the operator registers the OAuth client and sets
	// AnnotationKeycloakClientSecretName. Value "true" opts the workload into the legacy webhook
	// client-registration sidecar; the operator skips registration for that workload.
	// Re-exported from internal/clientreg so existing callers keep working.
	LabelClientRegistrationInject = clientreg.LabelClientRegistrationInject

	// AnnotationKeycloakClientSecretName must match kagenti-webhook injector.AnnotationKeycloakClientSecretName.
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
	// When true, the operator fetches a JWT-SVID from SPIRE via the go-spiffe SDK
	// and uses it with the Keycloak Admin API (manage-clients role only).
	// When false, uses admin credentials.
	UseSpiffeAuth bool

	// SpiffeSocket is the SPIRE workload API socket address used to fetch JWT-SVIDs.
	// Only used when UseSpiffeAuth is true. Defaults to the verified-fetch socket.
	SpiffeSocket string

	// OperatorClientID is the operator's SPIFFE ID (e.g., spiffe://localtest.me/ns/kagenti-system/sa/controller-manager).
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
		if r.OperatorClientID == "" {
			err := fmt.Errorf("OperatorClientID not configured")
			logger.Error(err, "SPIFFE auth requires OperatorClientID")
			if r.Recorder != nil {
				r.Recorder.Event(owner, corev1.EventTypeWarning, "OperatorClientIDMissing",
					"UseSpiffeAuth=true but OperatorClientID is empty. Check operator configuration.")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		socket := r.SpiffeSocket
		if socket == "" {
			socket = "unix:///spiffe-workload-api/spire-agent.sock"
		}

		// Fetch a JWT-SVID directly from the SPIRE workload API using the go-spiffe SDK.
		// This is the same socket used by verifiedFetch — no sidecar or file needed.
		// The audience must be the Keycloak realm's issuer URL (external URL, not in-cluster).
		// The JWT-SVID audience must be the Keycloak realm's issuer URL (external/public URL).
		// Keycloak's FederatedJWTClientValidator checks aud == its own issuer string.
		audience := fmt.Sprintf("%s/realms/%s", strings.TrimRight(ab.KeycloakURL, "/"), ab.KeycloakRealm)
		spiffeClient, err := workloadapi.New(ctx, workloadapi.WithAddr(socket))
		if err != nil {
			logger.Error(err, "failed to create SPIFFE workload API client")
			if r.Recorder != nil {
				r.Recorder.Eventf(owner, corev1.EventTypeWarning, "SPIFFEClientFailed",
					"Failed to connect to SPIRE workload API at %s: %v", socket, err)
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		defer spiffeClient.Close()

		svid, err := spiffeClient.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
		if err != nil {
			logger.Error(err, "failed to fetch JWT-SVID from SPIRE")
			if r.Recorder != nil {
				r.Recorder.Event(owner, corev1.EventTypeWarning, "JWTSVIDFetchFailed",
					"Failed to fetch JWT-SVID from SPIRE workload API. Check SPIRE is running and the operator has a registered identity.")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// WARNING: JWT-SVID is a bearer token — must never appear in logs or error messages.
		token, err = kc.JWTSVIDGrantToken(ctx, ab.KeycloakRealm, r.OperatorClientID, svid.Marshal())
		if err != nil {
			logger.Error(err, "Keycloak JWT-SVID authentication failed")
			if r.Recorder != nil {
				r.Recorder.Event(owner, corev1.EventTypeWarning, "KeycloakAuthFailed",
					"Failed to authenticate to Keycloak with JWT-SVID. Check SPIFFE IdP configuration in Keycloak.")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		logger.V(1).Info("authenticated with JWT-SVID via SPIRE workload API", "clientId", r.OperatorClientID)
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

	// In federated-jwt mode, agents authenticate using their SPIFFE identity directly.
	// No credential secret is needed — AuthBridge reads JWT-SVIDs from the workload
	// API socket, not from a mounted secret. Skipping secret creation avoids leaving
	// unused Keycloak client secrets in agent namespaces.
	secretName := keycloakClientCredentialsSecretName(ns, workloadName)
	if authType != "federated-jwt" {
		if err := r.ensureClientCredentialsSecret(ctx, owner, secretName, clientID, clientSecret); err != nil {
			logger.Error(err, "ensure client credentials secret")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Annotate the pod template with the secret name so the webhook mounts it.
		if err := patchTemplate(ctx); err != nil {
			logger.Error(err, "patch workload pod template")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	} else {
		// In federated-jwt mode, AuthBridge reads JWT-SVIDs directly from the SPIFFE
		// workload API socket — no credential secret is needed or created. Skip both the
		// secret and the pod template annotation so the webhook does not try to mount a
		// non-existent secret.
		logger.V(1).Info("skipping credential secret and template patch (federated-jwt — AuthBridge uses SPIFFE identity directly)",
			"workload", workloadName, "namespace", ns)
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
		return []string{"kagenti"}
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"kagenti"}
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
