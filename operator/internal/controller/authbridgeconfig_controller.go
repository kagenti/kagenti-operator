/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	labelRossoctlEnabled = "rossoctl-enabled"
	authbridgeConfigCM   = "authbridge-config"
	defaultKeycloakRealm = "rossoctl"
)

// AuthbridgeConfigPlatform holds the platform-level settings used to generate authbridge-config.
type AuthbridgeConfigPlatform struct {
	KeycloakAdminSecretNamespace string
	KeycloakRealm                string
	KeycloakPublicURL            string
	ClientAuthType               string
	SpiffeIdpAlias               string
	CredentialWaitTimeout        string
}

// AuthbridgeConfigReconciler reconciles authbridge-config ConfigMaps in namespaces
// labeled with rossoctl-enabled=true.
type AuthbridgeConfigReconciler struct {
	client.Client
	Platform AuthbridgeConfigPlatform
}

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch

func (r *AuthbridgeConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ns := &corev1.Namespace{}
	if err := r.Get(ctx, req.NamespacedName, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if ns.Labels[labelRossoctlEnabled] != "true" {
		return ctrl.Result{}, nil
	}

	if ns.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	desired := r.buildConfigMapData()

	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: authbridgeConfigCM}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      authbridgeConfigCM,
				Namespace: ns.Name,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "rossoctl-operator",
					LabelNamespaceDefaults:         "true",
				},
			},
			Data: desired,
		}
		if createErr := r.Create(ctx, cm); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				// Race: cache was stale. Re-fetch and fall through to update.
				if getErr := r.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: authbridgeConfigCM}, cm); getErr != nil {
					return ctrl.Result{}, getErr
				}
			} else {
				logger.Error(createErr, "Failed to create authbridge-config", "namespace", ns.Name)
				return ctrl.Result{}, createErr
			}
		} else {
			logger.Info("Created authbridge-config", "namespace", ns.Name)
			return ctrl.Result{}, nil
		}
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Update existing ConfigMap
	if cm.Labels == nil {
		cm.Labels = make(map[string]string)
	}
	cm.Labels["app.kubernetes.io/managed-by"] = "rossoctl-operator"
	cm.Labels[LabelNamespaceDefaults] = "true"
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	// Fill in defaults only for keys that are missing or empty; preserve any
	// explicitly-set per-namespace value (e.g. KEYCLOAK_REALM). This ConfigMap
	// is a namespace-defaults source (rossoctl.io/defaults=true), so defaults
	// must not clobber a value provided by an operator/admin/CI. See issue #433.
	for k, v := range desired {
		if existing, ok := cm.Data[k]; ok && existing != "" {
			continue
		}
		cm.Data[k] = v
	}
	if updateErr := r.Update(ctx, cm); updateErr != nil {
		logger.Error(updateErr, "Failed to update authbridge-config", "namespace", ns.Name)
		return ctrl.Result{}, updateErr
	}
	logger.Info("Updated authbridge-config", "namespace", ns.Name)
	return ctrl.Result{}, nil
}

func (r *AuthbridgeConfigReconciler) buildConfigMapData() map[string]string {
	kcNS := r.Platform.KeycloakAdminSecretNamespace
	if kcNS == "" {
		kcNS = "keycloak"
	}

	keycloakURL := fmt.Sprintf("http://keycloak-service.%s.svc:8080", kcNS)
	realm := r.Platform.KeycloakRealm
	if realm == "" {
		realm = defaultKeycloakRealm
	}

	issuer := ""
	if r.Platform.KeycloakPublicURL != "" {
		issuer = r.Platform.KeycloakPublicURL + "/realms/" + realm
	}

	clientAuthType := r.Platform.ClientAuthType
	if clientAuthType == "" {
		clientAuthType = "client-secret"
	}

	spiffeIdpAlias := r.Platform.SpiffeIdpAlias
	if spiffeIdpAlias == "" {
		spiffeIdpAlias = "spire-spiffe"
	}

	data := map[string]string{
		"KEYCLOAK_URL":            keycloakURL,
		"KEYCLOAK_REALM":          realm,
		"KEYCLOAK_NAMESPACE":      kcNS,
		"CLIENT_AUTH_TYPE":        clientAuthType,
		"SPIFFE_IDP_ALIAS":        spiffeIdpAlias,
		"CREDENTIAL_WAIT_TIMEOUT": r.credentialWaitTimeout(),
	}

	if issuer != "" {
		data["ISSUER"] = issuer
		data["EXPECTED_AUDIENCE"] = issuer
		data["JWT_AUDIENCE"] = issuer
	}

	return data
}

func (r *AuthbridgeConfigReconciler) credentialWaitTimeout() string {
	if r.Platform.CredentialWaitTimeout != "" {
		return r.Platform.CredentialWaitTimeout
	}
	return "120s"
}

func (r *AuthbridgeConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("authbridge-config").
		For(&corev1.Namespace{}, builder.WithPredicates(
			predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool {
					return e.Object.GetLabels()[labelRossoctlEnabled] == "true"
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					oldVal := e.ObjectOld.GetLabels()[labelRossoctlEnabled]
					newVal := e.ObjectNew.GetLabels()[labelRossoctlEnabled]
					return newVal == "true" || oldVal != newVal
				},
				DeleteFunc: func(e event.DeleteEvent) bool {
					return false
				},
			},
		)).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
