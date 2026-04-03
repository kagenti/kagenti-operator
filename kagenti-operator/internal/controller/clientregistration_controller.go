/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	agentv1alpha1 "github.com/kagenti/operator/api/v1alpha1"
	"github.com/kagenti/operator/internal/keycloak"
)

// Well-known namespace resources (same contract as kagenti-webhook injector).
const (
	authbridgeConfigConfigMap = "authbridge-config"
	// operatorConfigConfigMap is the centralized config in the operator namespace (kagenti-system)
	operatorConfigConfigMap = "kagenti-operator-config"
	keycloakAdminSecret     = "keycloak-admin-secret"

	// LabelClientRegistrationInject: when not "true", the operator registers the OAuth client and sets
	// AnnotationKeycloakClientSecretName. Value "true" opts the workload into the legacy webhook
	// client-registration sidecar; the operator skips registration for that workload.
	LabelClientRegistrationInject = "kagenti.io/client-registration-inject"

	// AnnotationKeycloakClientSecretName must match kagenti-webhook injector.AnnotationKeycloakClientSecretName.
	AnnotationKeycloakClientSecretName = "kagenti.io/keycloak-client-credentials-secret-name"
)

// ClientRegistrationReconciler registers OAuth clients in Keycloak and patches agent/tool workloads that
// use the default path (label absent or not "true") so the webhook injects envoy/SPIRE without the
// legacy registration sidecar. The Secret is created before the pod template annotation is set so new Pods
// never reference a missing Secret; the webhook mounts the Secret for injected sidecars that use shared-data.
type ClientRegistrationReconciler struct {
	client.Client
	// APIReader reads authbridge-config and keycloak-admin-secret from the API server. Those objects
	// are not in the manager's ConfigMap cache (see cmd/main.go cache.ByObject for ConfigMap).
	APIReader client.Reader
	Scheme    *runtime.Scheme

	// OperatorNamespace is the namespace where the operator is running (e.g., "kagenti-system").
	// Used to read centralized kagenti-operator-config ConfigMap.
	OperatorNamespace string

	SpireTrustDomain string
	// KeycloakAdminTokenCache caches admin password-grant tokens by Keycloak URL and credentials to
	// avoid a token request on every reconcile. If nil, PasswordGrantToken is used without caching.
	KeycloakAdminTokenCache *keycloak.CachedAdminTokenProvider
}

func (r *ClientRegistrationReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
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
	if err := r.Get(ctx, req.NamespacedName, dep); err == nil {
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
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, req.NamespacedName, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
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

	ab, err := readAuthbridgeConfigMap(ctx, r.uncachedReader(), r.OperatorNamespace, ns)
	if err != nil {
		logger.Error(err, "read authbridge-config or kagenti-operator-config")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if ab.KeycloakURL == "" || ab.KeycloakRealm == "" {
		logger.Info("waiting for KEYCLOAK_URL/KEYCLOAK_REALM in kagenti-operator-config or authbridge-config",
			"operatorNamespace", r.OperatorNamespace, "workloadNamespace", ns)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	adminSecret := &corev1.Secret{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: keycloakAdminSecret}, adminSecret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("waiting for keycloak-admin-secret", "namespace", ns)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	adminUser := string(adminSecret.Data["KEYCLOAK_ADMIN_USERNAME"])
	adminPass := string(adminSecret.Data["KEYCLOAK_ADMIN_PASSWORD"])
	if adminUser == "" || adminPass == "" {
		logger.Info("keycloak-admin-secret missing username/password keys")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	spireEnabled := strings.EqualFold(strings.TrimSpace(ab.SpireEnabled), "true")
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
	if r.KeycloakAdminTokenCache != nil {
		token, err = r.KeycloakAdminTokenCache.Token(ctx, &kc, adminUser, adminPass)
	} else {
		token, err = kc.PasswordGrantToken(ctx, adminUser, adminPass)
	}
	if err != nil {
		logger.Error(err, "Keycloak admin token failed")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	_, clientSecret, err := kc.RegisterOrFetchClientWithToken(ctx, token, keycloak.ClientRegistrationParams{
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
	}); err != nil {
		logger.Error(err, "Keycloak audience scope management failed (credentials will still be written)",
			"clientId", clientID)
	}

	secretName := keycloakClientCredentialsSecretName(ns, workloadName)
	if err := r.ensureClientCredentialsSecret(ctx, owner, secretName, clientID, clientSecret); err != nil {
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

// keycloakClientCredentialsSkipReason returns a non-empty human-readable reason when this controller should
// not process the workload; empty string means reconcile should continue.
func keycloakClientCredentialsSkipReason(labels map[string]string, injectTools bool) string {
	if labels == nil {
		return "pod template has no labels"
	}
	if labels[LabelClientRegistrationInject] == "true" {
		return fmt.Sprintf("%s is \"true\" (legacy webhook client-registration sidecar; operator-managed registration disabled for this workload)", LabelClientRegistrationInject)
	}
	switch labels[LabelAgentType] {
	case LabelValueAgent:
		return ""
	case string(agentv1alpha1.RuntimeTypeTool):
		if !injectTools {
			return "kagenti.io/type is tool but cluster injectTools feature gate is disabled"
		}
		return ""
	default:
		t := labels[LabelAgentType]
		if t == "" {
			return "kagenti.io/type label is missing or not agent/tool"
		}
		return fmt.Sprintf("kagenti.io/type=%q is not agent or tool", t)
	}
}

func workloadWantsOperatorClientReg(labels map[string]string, injectTools bool) bool {
	return keycloakClientCredentialsSkipReason(labels, injectTools) == ""
}

type authbridgeConfig struct {
	KeycloakURL                  string
	KeycloakRealm                string
	SpireEnabled                 string
	ClientAuthType               string
	SpiffeIDPAlias               string
	KeycloakTokenExchangeEnabled string
	PlatformClientIDs            string
	KeycloakAudienceScopeEnabled string
}

// readAuthbridgeConfigMap reads Keycloak configuration from:
// 1. First priority: operatorNamespace/kagenti-operator-config (centralized config)
// 2. Fallback: workloadNamespace/authbridge-config (per-namespace config for backward compatibility)
//
// The centralized config is preferred for waypoint mode where agent pods don't need the ConfigMap.
// Per-namespace config is still supported for sidecar mode and backward compatibility.
func readAuthbridgeConfigMap(ctx context.Context, c client.Reader, operatorNamespace, workloadNamespace string) (authbridgeConfig, error) {
	logger := log.FromContext(ctx)

	// Try centralized operator config first (preferred for waypoint mode)
	if operatorNamespace != "" {
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, types.NamespacedName{Namespace: operatorNamespace, Name: operatorConfigConfigMap}, cm)
		if err == nil && cm.Data != nil {
			config := extractAuthbridgeConfig(cm.Data)
			// Only use operator config if it has the required fields
			if config.KeycloakURL != "" && config.KeycloakRealm != "" {
				logger.V(1).Info("using centralized operator config",
					"configMap", operatorNamespace+"/"+operatorConfigConfigMap)
				return config, nil
			}
		}
		// If operator config doesn't exist or is incomplete, fall back to namespace config
		if err != nil && !apierrors.IsNotFound(err) {
			return authbridgeConfig{}, err
		}
	}

	// Fall back to per-namespace authbridge-config (backward compatibility)
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: authbridgeConfigConfigMap}, cm)
	if apierrors.IsNotFound(err) {
		return authbridgeConfig{}, nil
	}
	if err != nil {
		return authbridgeConfig{}, err
	}
	if cm.Data == nil {
		return authbridgeConfig{}, nil
	}
	logger.V(1).Info("using per-namespace authbridge config (fallback)",
		"configMap", workloadNamespace+"/"+authbridgeConfigConfigMap)
	return extractAuthbridgeConfig(cm.Data), nil
}

func extractAuthbridgeConfig(data map[string]string) authbridgeConfig {
	return authbridgeConfig{
		KeycloakURL:                  data["KEYCLOAK_URL"],
		KeycloakRealm:                data["KEYCLOAK_REALM"],
		SpireEnabled:                 data["SPIRE_ENABLED"],
		ClientAuthType:               data["CLIENT_AUTH_TYPE"],
		SpiffeIDPAlias:               data["SPIFFE_IDP_ALIAS"],
		KeycloakTokenExchangeEnabled: data["KEYCLOAK_TOKEN_EXCHANGE_ENABLED"],
		PlatformClientIDs:            data["PLATFORM_CLIENT_IDS"],
		KeycloakAudienceScopeEnabled: data["KEYCLOAK_AUDIENCE_SCOPE_ENABLED"],
	}
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
	// Only one ConfigMap data entry is consulted: we return after the first non-empty
	// value that unmarshals to a non-empty YAML map (map iteration order); other keys are ignored.
	for _, raw := range cm.Data {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(raw), &m); err != nil || len(m) == 0 {
			continue
		}
		if v, ok := m["globalEnabled"].(bool); ok {
			globalOn = v
		}
		if v, ok := m["clientRegistration"].(bool); ok {
			clientReg = v
		}
		if v, ok := m["injectTools"].(bool); ok {
			injectTools = v
		}
		return globalOn, clientReg, injectTools, nil
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

func keycloakClientCredentialsSecretName(namespace, workload string) string {
	sum := sha256.Sum256([]byte(namespace + "\000" + workload + "\000kagenti-keycloak-client-credentials"))
	return "kagenti-keycloak-client-credentials-" + hex.EncodeToString(sum[:8])
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
	default:
		return false
	}
}

// SetupWithManager registers the controller. injectTools is resolved at reconcile time from cluster
// feature gates; the predicate uses injectTools=true so tool workloads are not dropped before gates load.
func (r *ClientRegistrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pred := predicate.NewPredicateFuncs(clientRegistrationWorkloadPredicate)
	return ctrl.NewControllerManagedBy(mgr).
		Named("clientregistration").
		For(&appsv1.Deployment{}, builder.WithPredicates(pred)).
		Watches(
			&appsv1.StatefulSet{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(pred),
		).
		Complete(r)
}
