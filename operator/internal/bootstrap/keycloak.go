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

package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const fieldManager = "rossoctl-operator-bootstrap"

var (
	keycloakGVK    = schema.GroupVersionKind{Group: "k8s.keycloak.org", Version: "v2alpha1", Kind: "Keycloak"}
	realmImportGVK = schema.GroupVersionKind{Group: "k8s.keycloak.org", Version: "v2alpha1", Kind: "KeycloakRealmImport"}
	routeGVK       = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
)

// KeycloakBootstrapRunnable ensures Keycloak infrastructure exists at startup:
// Postgres (StatefulSet + Service + Secret + ConfigMap), Keycloak CR, Route,
// test-users Secret, and KeycloakRealmImport CR.
// Resources are created if absent and spec-patched if they drift. The Keycloak CR
// is never deleted (cascade risk with keycloak-initial-admin secret).
type KeycloakBootstrapRunnable struct {
	Client            client.Client
	APIReader         client.Reader
	Namespace         string
	Realm             string
	KeycloakPublicURL string
	Log               logr.Logger

	// RouteDiscoveryAttempts controls how many times to poll for Route host (default 6, 5s apart).
	RouteDiscoveryAttempts int
}

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=create;get;list;patch;update;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=create;get;list;update;watch
// +kubebuilder:rbac:groups=k8s.keycloak.org,resources=keycloaks,verbs=create;get;list;patch;update
// +kubebuilder:rbac:groups=k8s.keycloak.org,resources=keycloakrealmimports,verbs=create;get;list;patch;update
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=create;get;list;update

func (r *KeycloakBootstrapRunnable) Start(ctx context.Context) error {
	log := r.Log.WithName("keycloak-bootstrap")

	ns := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: r.Namespace}, ns); err != nil {
		log.Info("Keycloak namespace not found, skipping bootstrap", "namespace", r.Namespace)
		return nil
	}

	log.Info("Starting Keycloak infrastructure bootstrap", "namespace", r.Namespace)

	if err := r.ensurePostgres(ctx, log); err != nil {
		log.Error(err, "Failed to ensure Postgres infrastructure")
		return nil
	}

	if err := r.ensureKeycloakCR(ctx, log); err != nil {
		log.Error(err, "Failed to ensure Keycloak CR (CRD may not be installed)")
		return nil
	}

	if err := r.ensureRoute(ctx, log); err != nil {
		log.Error(err, "Failed to ensure Keycloak Route (not on OpenShift?)")
		return nil
	}

	if err := r.ensureRealmBootstrap(ctx, log); err != nil {
		log.Error(err, "Failed to ensure realm bootstrap (KeycloakRealmImport CRD may not be installed)")
		return nil
	}

	log.Info("Keycloak infrastructure bootstrap complete")
	return nil
}

func (r *KeycloakBootstrapRunnable) NeedLeaderElection() bool {
	return true
}

func (r *KeycloakBootstrapRunnable) ensurePostgres(ctx context.Context, log logr.Logger) error {
	if err := r.ensureDBSecret(ctx, log); err != nil {
		return err
	}
	if err := r.ensureInitConfigMap(ctx, log); err != nil {
		return err
	}
	if err := r.ensureStatefulSet(ctx, log); err != nil {
		return err
	}
	return r.ensureService(ctx, log)
}

func (r *KeycloakBootstrapRunnable) ensureDBSecret(ctx context.Context, log logr.Logger) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: "keycloak-db-secret", Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, secret); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("reading keycloak-db-secret: %w", err)
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "keycloak-db-secret",
			Namespace: r.Namespace,
			Labels:    keycloakLabels(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username": []byte("testuser"),
			"password": []byte(randomPassword()),
		},
	}
	if err := r.Client.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating keycloak-db-secret: %w", err)
	}
	log.Info("Created keycloak-db-secret")
	return nil
}

func (r *KeycloakBootstrapRunnable) ensureInitConfigMap(ctx context.Context, log logr.Logger) error {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: "postgres-kc-init-script", Namespace: r.Namespace}

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres-kc-init-script",
			Namespace: r.Namespace,
			Labels:    keycloakLabels(),
		},
		Data: map[string]string{
			"set_passwords.sh": postgresSetPasswordsScript,
			"init.sh":          postgresInitScript,
		},
	}

	if err := r.APIReader.Get(ctx, key, cm); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("reading postgres-kc-init-script: %w", err)
		}
		if err := r.Client.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating postgres-kc-init-script: %w", err)
		}
		log.Info("Created postgres-kc-init-script ConfigMap")
		return nil
	}

	if cm.Data["set_passwords.sh"] == postgresSetPasswordsScript && cm.Data["init.sh"] == postgresInitScript {
		return nil
	}
	cm.Data = desired.Data
	if err := r.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("updating postgres-kc-init-script: %w", err)
	}
	log.Info("Updated postgres-kc-init-script ConfigMap")
	return nil
}

func (r *KeycloakBootstrapRunnable) ensureStatefulSet(ctx context.Context, log logr.Logger) error {
	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: "postgres-kc", Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, sts); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("reading postgres-kc StatefulSet: %w", err)
	}

	sts = postgresStatefulSet(r.Namespace)
	if err := r.Client.Create(ctx, sts); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating postgres-kc StatefulSet: %w", err)
	}
	log.Info("Created postgres-kc StatefulSet")
	return nil
}

func (r *KeycloakBootstrapRunnable) ensureService(ctx context.Context, log logr.Logger) error {
	svc := &corev1.Service{}
	key := types.NamespacedName{Name: "postgres-kc", Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, svc); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("reading postgres-kc Service: %w", err)
	}

	svc = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres-kc",
			Namespace: r.Namespace,
			Labels:    keycloakLabels(),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       "postgres",
				Port:       5432,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt32(5432),
			}},
			Selector: map[string]string{"app": "postgres-kc"},
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
	if err := r.Client.Create(ctx, svc); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating postgres-kc Service: %w", err)
	}
	log.Info("Created postgres-kc Service")
	return nil
}

func (r *KeycloakBootstrapRunnable) ensureKeycloakCR(ctx context.Context, log logr.Logger) error {
	desired := keycloakCRSpec()

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(keycloakGVK)
	cr.SetName("keycloak")
	cr.SetNamespace(r.Namespace)
	cr.SetLabels(keycloakLabels())
	if err := unstructured.SetNestedField(cr.Object, desired, "spec"); err != nil {
		return fmt.Errorf("setting Keycloak spec: %w", err)
	}

	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(cr), client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying Keycloak CR: %w", err)
	}
	log.Info("Applied Keycloak CR")
	return nil
}

func (r *KeycloakBootstrapRunnable) ensureRoute(ctx context.Context, log logr.Logger) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(routeGVK)
	key := types.NamespacedName{Name: "keycloak", Namespace: r.Namespace}

	if err := r.APIReader.Get(ctx, key, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("reading Keycloak Route: %w", err)
	}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK)
	route.SetName("keycloak")
	route.SetNamespace(r.Namespace)
	route.SetLabels(keycloakLabels())
	route.SetAnnotations(map[string]string{"openshift.io/host.generated": "true"})
	if err := unstructured.SetNestedField(route.Object, map[string]any{
		"path": "/",
		"port": map[string]any{"targetPort": int64(8080)},
		"to": map[string]any{
			"kind": "Service",
			"name": "keycloak-service",
		},
		"wildcardPolicy": "None",
		"tls": map[string]any{
			"termination":                   "edge",
			"insecureEdgeTerminationPolicy": "Redirect",
		},
	}, "spec"); err != nil {
		return fmt.Errorf("setting Route spec: %w", err)
	}
	if err := r.Client.Create(ctx, route); err != nil {
		return fmt.Errorf("creating Keycloak Route: %w", err)
	}
	log.Info("Created Keycloak Route")
	return nil
}

// --- Realm bootstrap ---

func (r *KeycloakBootstrapRunnable) ensureRealmBootstrap(ctx context.Context, log logr.Logger) error {
	publicURL := r.KeycloakPublicURL
	if publicURL == "" {
		publicURL = r.discoverPublicURL(ctx, log)
	}
	if publicURL == "" {
		log.Info("KeycloakPublicURL not set and Route host not yet available, skipping realm import")
		return nil
	}

	passwords, err := r.ensureTestUsersSecret(ctx, log)
	if err != nil {
		return err
	}

	return r.ensureRealmImport(ctx, log, passwords, publicURL)
}

// discoverPublicURL polls the Keycloak Route for an assigned host, retrying
// briefly to allow the OpenShift router time to populate spec.host.
func (r *KeycloakBootstrapRunnable) discoverPublicURL(ctx context.Context, log logr.Logger) string {
	maxAttempts := 6
	if r.RouteDiscoveryAttempts > 0 {
		maxAttempts = r.RouteDiscoveryAttempts
	}

	for i := range maxAttempts {
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(routeGVK)
		if err := r.APIReader.Get(ctx, types.NamespacedName{Name: "keycloak", Namespace: r.Namespace}, route); err != nil {
			log.V(1).Info("Cannot read Keycloak Route for public URL discovery", "error", err)
			return ""
		}
		host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
		if host != "" {
			url := "https://" + host
			log.Info("Discovered Keycloak public URL from Route", "url", url)
			return url
		}
		if i < maxAttempts-1 {
			log.V(1).Info("Keycloak Route has no spec.host yet, retrying", "attempt", i+1)
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(5 * time.Second):
			}
		}
	}
	log.Info("Keycloak Route host not populated after retries")
	return ""
}

func (r *KeycloakBootstrapRunnable) ensureTestUsersSecret(ctx context.Context, log logr.Logger) (map[string]string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: "rossoctl-test-users", Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, secret); err == nil {
		return map[string]string{
			"admin-password":    string(secret.Data["admin-password"]),
			"dev-user-password": string(secret.Data["dev-user-password"]),
			"ns-admin-password": string(secret.Data["ns-admin-password"]),
		}, nil
	} else if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("reading rossoctl-test-users: %w", err)
	}

	passwords := map[string]string{
		"admin-password":    randomPassword(),
		"dev-user-password": randomPassword(),
		"ns-admin-password": randomPassword(),
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rossoctl-test-users",
			Namespace: r.Namespace,
			Labels:    keycloakLabels(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"admin-password":    []byte(passwords["admin-password"]),
			"dev-user-password": []byte(passwords["dev-user-password"]),
			"ns-admin-password": []byte(passwords["ns-admin-password"]),
		},
	}
	if err := r.Client.Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			return r.ensureTestUsersSecret(ctx, log)
		}
		return nil, fmt.Errorf("creating rossoctl-test-users: %w", err)
	}
	log.Info("Created rossoctl-test-users Secret")
	return passwords, nil
}

func (r *KeycloakBootstrapRunnable) ensureRealmImport(ctx context.Context, log logr.Logger, passwords map[string]string, publicURL string) error {
	realm := r.Realm
	if realm == "" {
		realm = "rossoctl"
	}

	name := realm + "-realm-import"
	realmSpec := buildRealmSpec(realm, passwords, publicURL)

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(realmImportGVK)
	cr.SetName(name)
	cr.SetNamespace(r.Namespace)
	cr.SetLabels(keycloakLabels())
	if err := unstructured.SetNestedField(cr.Object, "keycloak", "spec", "keycloakCRName"); err != nil {
		return fmt.Errorf("setting keycloakCRName: %w", err)
	}
	if err := unstructured.SetNestedField(cr.Object, realmSpec, "spec", "realm"); err != nil {
		return fmt.Errorf("setting realm spec: %w", err)
	}

	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(cr), client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying KeycloakRealmImport: %w", err)
	}
	log.Info("Applied KeycloakRealmImport CR", "name", name)
	return nil
}

func randomPassword() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:16]
}

func buildRealmSpec(realm string, passwords map[string]string, publicURL string) map[string]any {
	audienceURL := ""
	if publicURL != "" {
		audienceURL = strings.TrimRight(publicURL, "/") + "/realms/" + realm
	}

	realmJSON := strings.NewReplacer(
		"__REALM__", realm,
		"__ADMIN_PASS__", passwords["admin-password"],
		"__DEV_PASS__", passwords["dev-user-password"],
		"__NSADMIN_PASS__", passwords["ns-admin-password"],
		"__AUDIENCE_URL__", audienceURL,
	).Replace(realmTemplate)

	var spec map[string]any
	if err := json.Unmarshal([]byte(realmJSON), &spec); err != nil {
		panic("invalid realm template JSON: " + err.Error())
	}
	return spec
}

// --- Resource definitions ---

func keycloakLabels() map[string]string {
	return map[string]string{
		"app":                          "rossoctl",
		"app.kubernetes.io/managed-by": "rossoctl-operator",
	}
}

func keycloakCRSpec() map[string]any {
	return map[string]any{
		"bootstrapAdmin": map[string]any{
			"user": map[string]any{
				"secret": "keycloak-initial-admin",
			},
		},
		"hostname": map[string]any{"strict": false},
		"http": map[string]any{
			"httpEnabled": true,
			"httpPort":    int64(8080),
		},
		"instances":     int64(1),
		"networkPolicy": map[string]any{"enabled": true},
		"update":        map[string]any{"strategy": "RecreateOnImageChange"},
		"db": map[string]any{
			"host":     "postgres-kc",
			"database": "postgres",
			"passwordSecret": map[string]any{
				"key":  "password",
				"name": "keycloak-db-secret",
			},
			"usernameSecret": map[string]any{
				"key":  "username",
				"name": "keycloak-db-secret",
			},
			"vendor": "postgres",
		},
		"unsupported": map[string]any{
			"podTemplate": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name": "keycloak",
							"env": []any{
								map[string]any{
									"name":  "KC_PROXY_HEADERS",
									"value": "forwarded",
								},
							},
						},
					},
				},
			},
		},
	}
}

func postgresStatefulSet(namespace string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres-kc",
			Namespace: namespace,
			Labels:    keycloakLabels(),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(int32(1)),
			ServiceName: "postgres-kc",
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "postgres-kc"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "postgres-kc"},
					Annotations: map[string]string{
						"prometheus.io/path":   "/metrics",
						"prometheus.io/port":   "9090",
						"prometheus.io/scrape": "true",
					},
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name: "init-script",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "postgres-kc-init-script"},
								DefaultMode:          ptr.To(int32(0o755)),
							},
						},
					}},
					Containers: []corev1.Container{{
						Name:            "postgres",
						Image:           "quay.io/fedora/postgresql-15",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env: []corev1.EnvVar{
							{Name: "POSTGRESQL_DATABASE", Value: "postgres"},
							{Name: "POSTGRESQL_USER", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "keycloak-db-secret"},
									Key:                  "username",
								},
							}},
							{Name: "POSTGRESQL_PASSWORD", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "keycloak-db-secret"},
									Key:                  "password",
								},
							}},
						},
						Ports: []corev1.ContainerPort{{ContainerPort: 5432}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/sh", "-c", "exec pg_isready -U postgres -d postgres"},
								},
							},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             ptr.To(true),
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "postgres-data", MountPath: "/var/lib/pgsql/data"},
							{Name: "init-script", MountPath: "/usr/share/container-scripts/postgresql/start/"},
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "postgres-data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			}},
		},
	}
}

const postgresSetPasswordsScript = `#!/bin/bash

_psql () { psql --set ON_ERROR_STOP=1 "$@" ; }

if [[ ",$postinitdb_actions," = *,simple_db,* ]]; then
_psql --set=username="$POSTGRESQL_USER" \
      --set=password="$POSTGRESQL_PASSWORD" \
<<< "ALTER USER :\"username\" WITH ENCRYPTED PASSWORD :'password';"
fi

if [ -v POSTGRESQL_MASTER_USER ]; then
_psql --set=masteruser="$POSTGRESQL_MASTER_USER" \
      --set=masterpass="$POSTGRESQL_MASTER_PASSWORD" \
<<'EOF'
ALTER USER :"masteruser" WITH REPLICATION;
ALTER USER :"masteruser" WITH ENCRYPTED PASSWORD :'masterpass';
EOF
fi

if [ -v POSTGRESQL_ADMIN_PASSWORD ]; then
_psql --set=adminpass="$POSTGRESQL_ADMIN_PASSWORD" \
<<<"ALTER USER \"postgres\" WITH ENCRYPTED PASSWORD :'adminpass';"
fi
`

const postgresInitScript = `#!/bin/bash
set -e

psql -v ON_ERROR_STOP=1  <<-EOSQL
    ALTER DATABASE postgres OWNER TO testuser;
EOSQL
`

const realmTemplate = `{
  "realm": "__REALM__",
  "enabled": true,
  "registrationAllowed": false,
  "roles": {
    "realm": [
      {"name": "admin", "description": "Platform administrator"},
      {"name": "developer", "description": "Developer with namespace-scoped access"},
      {"name": "ns-admin", "description": "Namespace administrator"}
    ]
  },
  "groups": [
    {"name": "mlflow-admin", "path": "/mlflow-admin"},
    {"name": "mlflow", "path": "/mlflow"}
  ],
  "users": [
    {
      "username": "admin",
      "enabled": true,
      "emailVerified": true,
      "firstName": "Admin",
      "lastName": "User",
      "email": "admin@rossoctl.local",
      "credentials": [{"type": "password", "value": "__ADMIN_PASS__", "temporary": false}],
      "realmRoles": ["admin"],
      "groups": ["mlflow-admin", "mlflow"]
    },
    {
      "username": "dev-user",
      "enabled": true,
      "emailVerified": true,
      "firstName": "Dev",
      "lastName": "User",
      "email": "dev-user@rossoctl.local",
      "credentials": [{"type": "password", "value": "__DEV_PASS__", "temporary": false}],
      "realmRoles": ["developer"],
      "groups": ["mlflow"]
    },
    {
      "username": "ns-admin",
      "enabled": true,
      "emailVerified": true,
      "firstName": "Namespace",
      "lastName": "Admin",
      "email": "ns-admin@rossoctl.local",
      "credentials": [{"type": "password", "value": "__NSADMIN_PASS__", "temporary": false}],
      "realmRoles": ["ns-admin"],
      "groups": ["mlflow"]
    }
  ],
  "clientScopes": [
    {
      "name": "openid",
      "description": "OpenID Connect scope",
      "protocol": "openid-connect",
      "attributes": {"include.in.token.scope": "true"}
    },
    {
      "name": "email",
      "description": "OpenID Connect email scope",
      "protocol": "openid-connect",
      "attributes": {"include.in.token.scope": "true"},
      "protocolMappers": [
        {
          "name": "email",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-usermodel-attribute-mapper",
          "config": {
            "user.attribute": "email",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true",
            "claim.name": "email",
            "jsonType.label": "String"
          }
        },
        {
          "name": "email verified",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-usermodel-attribute-mapper",
          "config": {
            "user.attribute": "emailVerified",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true",
            "claim.name": "email_verified",
            "jsonType.label": "boolean"
          }
        }
      ]
    },
    {
      "name": "profile",
      "description": "OpenID Connect profile scope",
      "protocol": "openid-connect",
      "attributes": {"include.in.token.scope": "true"},
      "protocolMappers": [
        {
          "name": "username",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-usermodel-attribute-mapper",
          "config": {
            "user.attribute": "username",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true",
            "claim.name": "preferred_username",
            "jsonType.label": "String"
          }
        },
        {
          "name": "full name",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-full-name-mapper",
          "config": {
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true"
          }
        }
      ]
    },
    {
      "name": "roles",
      "description": "OpenID Connect roles scope",
      "protocol": "openid-connect",
      "attributes": {"include.in.token.scope": "false"},
      "protocolMappers": [
        {
          "name": "realm roles",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-usermodel-realm-role-mapper",
          "config": {
            "multivalued": "true",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true",
            "claim.name": "realm_access.roles",
            "jsonType.label": "String"
          }
        },
        {
          "name": "client roles",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-usermodel-client-role-mapper",
          "config": {
            "multivalued": "true",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true",
            "claim.name": "resource_access.${client_id}.roles",
            "jsonType.label": "String"
          }
        }
      ]
    },
    {
      "name": "web-origins",
      "description": "OpenID Connect web-origins scope",
      "protocol": "openid-connect",
      "attributes": {"include.in.token.scope": "false"},
      "protocolMappers": [
        {
          "name": "allowed web origins",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-allowed-origins-mapper"
        }
      ]
    },
    {
      "name": "rossoctl-platform-audience",
      "description": "Adds the realm issuer URL as an audience claim so AuthBridge ext-proc accepts the token",
      "protocol": "openid-connect",
      "attributes": {
        "include.in.token.scope": "false",
        "display.on.consent.screen": "false"
      },
      "protocolMappers": [
        {
          "name": "rossoctl-platform-audience-mapper",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-audience-mapper",
          "config": {
            "included.custom.audience": "__AUDIENCE_URL__",
            "id.token.claim": "false",
            "access.token.claim": "true",
            "introspection.token.claim": "true"
          }
        }
      ]
    }
  ],
  "defaultDefaultClientScopes": ["openid", "email", "profile", "roles", "web-origins", "rossoctl-platform-audience"],
  "clients": [
    {
      "clientId": "mlflow",
      "enabled": true,
      "publicClient": false,
      "clientAuthenticatorType": "client-secret",
      "serviceAccountsEnabled": true,
      "standardFlowEnabled": true,
      "redirectUris": ["*"],
      "webOrigins": ["+"],
      "protocolMappers": [
        {
          "name": "groups",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-group-membership-mapper",
          "config": {
            "full.path": "false",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true",
            "claim.name": "groups"
          }
        }
      ]
    }
  ]
}`
