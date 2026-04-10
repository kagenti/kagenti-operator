package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	agentv1alpha1 "github.com/kagenti/operator/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	clientRegistrationTestNamespace      = "test-ns"
	clientRegistrationTestDeploymentName = "my-dep"
)

func TestWorkloadWantsOperatorClientReg(t *testing.T) {
	cases := []struct {
		name        string
		labels      map[string]string
		injectTools bool
		want        bool
	}{
		{
			name: "agent default — operator-managed registration",
			labels: map[string]string{
				LabelAgentType: LabelValueAgent,
			},
			want: true,
		},
		{
			name: "agent with legacy sidecar opt-in — operator skips",
			labels: map[string]string{
				LabelAgentType:                LabelValueAgent,
				LabelClientRegistrationInject: "true",
			},
			want: false,
		},
		{
			name: "agent explicit false — same as default (operator-managed)",
			labels: map[string]string{
				LabelAgentType:                LabelValueAgent,
				LabelClientRegistrationInject: "false",
			},
			want: true,
		},
		{
			name: "tool default with injectTools",
			labels: map[string]string{
				LabelAgentType: string(agentv1alpha1.RuntimeTypeTool),
			},
			injectTools: true,
			want:        true,
		},
		{
			name: "tool default no injectTools",
			labels: map[string]string{
				LabelAgentType: string(agentv1alpha1.RuntimeTypeTool),
			},
			injectTools: false,
			want:        false,
		},
		{
			name: "tool with legacy opt-in — operator skips regardless of injectTools",
			labels: map[string]string{
				LabelAgentType:                string(agentv1alpha1.RuntimeTypeTool),
				LabelClientRegistrationInject: "true",
			},
			injectTools: true,
			want:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workloadWantsOperatorClientReg(tc.labels, tc.injectTools); got != tc.want {
				t.Fatalf("want %v got %v", tc.want, got)
			}
		})
	}
}

func TestInjectKeycloakClientCredentialsAnnotation(t *testing.T) {
	pt := &corev1.PodTemplateSpec{}
	secretName := "kagenti-keycloak-client-credentials-deadbeefcafe4242"
	if !injectKeycloakClientCredentialsAnnotation(pt, secretName) {
		t.Fatal("expected change")
	}
	if pt.Annotations[AnnotationKeycloakClientSecretName] != secretName {
		t.Fatalf("annotation: %v", pt.Annotations)
	}
	if injectKeycloakClientCredentialsAnnotation(pt, secretName) {
		t.Fatal("expected no change")
	}
}

func TestParsePlatformClientIDs(t *testing.T) {
	if got := parsePlatformClientIDs(""); len(got) != 1 || got[0] != "kagenti" {
		t.Fatalf("empty: %v", got)
	}
	if got := parsePlatformClientIDs("a, b"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("list: %v", got)
	}
	if got := parsePlatformClientIDs("  ,  "); len(got) != 1 || got[0] != "kagenti" {
		t.Fatalf("all blank: %v", got)
	}
}

func TestResolveKeycloakClientID(t *testing.T) {
	defaultTemplate := "spiffe://{{.TrustDomain}}/ns/{{.Namespace}}/sa/{{.ServiceAccount}}"

	id, err := resolveKeycloakClientID("ns1", "dep", "", false, "", "")
	if err != nil || id != "ns1/dep" {
		t.Fatalf("non-spire: %q %v", id, err)
	}
	_, err = resolveKeycloakClientID("ns1", "dep", "", true, "example.org", defaultTemplate)
	if err == nil {
		t.Fatal("expected error for default SA with SPIRE")
	}
	id, err = resolveKeycloakClientID("ns1", "dep", "mysa", true, "example.org", defaultTemplate)
	if err != nil || id != "spiffe://example.org/ns/ns1/sa/mysa" {
		t.Fatalf("spire: %q %v", id, err)
	}

	// Test custom template
	customTemplate := "spiffe://{{.TrustDomain}}/workload/{{.Namespace}}/{{.ServiceAccount}}"
	id, err = resolveKeycloakClientID("ns1", "dep", "mysa", true, "example.org", customTemplate)
	if err != nil || id != "spiffe://example.org/workload/ns1/mysa" {
		t.Fatalf("custom template: %q %v", id, err)
	}
}

func clientRegistrationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func clusterFeatureGatesConfigMap(clientRegistration bool) *corev1.ConfigMap {
	reg := "false"
	if clientRegistration {
		reg = "true"
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ClusterDefaultsNamespace,
			Name:      ClusterFeatureGatesConfigMapName,
		},
		Data: map[string]string{
			"gates.yaml": "globalEnabled: true\nclientRegistration: " + reg + "\ninjectTools: false\n",
		},
	}
}

func testDeploymentForClientReg() *appsv1.Deployment {
	name := clientRegistrationTestDeploymentName
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: clientRegistrationTestNamespace,
			Name:      name,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":          name,
						LabelAgentType: LabelValueAgent,
					},
				},
				Spec: corev1.PodSpec{},
			},
		},
	}
}

func authbridgeConfigMapForTest(ns, keycloakURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      authbridgeConfigConfigMap,
		},
		Data: map[string]string{
			"KEYCLOAK_URL":                    keycloakURL,
			"KEYCLOAK_REALM":                  "kagenti",
			"KEYCLOAK_AUDIENCE_SCOPE_ENABLED": "false",
		},
	}
}

func keycloakAdminSecretForTest(ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      keycloakAdminSecret,
		},
		Data: map[string][]byte{
			"KEYCLOAK_ADMIN_USERNAME": []byte("admin"),
			"KEYCLOAK_ADMIN_PASSWORD": []byte("secret"),
		},
	}
}

func startTestKeycloakServer(t *testing.T) *httptest.Server {
	t.Helper()
	// Matches keycloak.RegisterOrFetchClientWithToken defaults for the happy-path deployment:
	// client ID test-ns/my-dep, client-secret auth, token exchange on (AuthBridge omits KEYCLOAK_TOKEN_EXCHANGE_ENABLED).
	inSyncClientRep := map[string]any{
		"id":                        "uuid-1",
		"clientId":                  "test-ns/my-dep",
		"name":                      "test-ns/my-dep",
		"standardFlowEnabled":       true,
		"directAccessGrantsEnabled": true,
		"serviceAccountsEnabled":    true,
		"fullScopeAllowed":          false,
		"publicClient":              false,
		"clientAuthenticatorType":   "client-secret",
		"attributes":                map[string]any{"standard.token.exchange.enabled": []any{"true"}},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/realms/master/protocol/openid-connect/token" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case r.Method == http.MethodGet && r.URL.Query().Get("clientId") != "":
			cid := r.URL.Query().Get("clientId")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "uuid-1", "clientId": cid}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/client-secret"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"value": "client-secret-value"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/admin/realms/"):
			// GET .../realms/{realm}/clients/{uuid} (reconcile path; not list ?clientId=, not .../client-secret)
			trim := strings.TrimPrefix(r.URL.Path, "/admin/realms/")
			parts := strings.Split(trim, "/")
			if len(parts) == 3 && parts[1] == "clients" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(inSyncClientRep)
				return
			}
			fallthrough
		default:
			t.Errorf("unexpected Keycloak request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestClientRegistrationReconciler_Reconcile(t *testing.T) {
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: clientRegistrationTestNamespace, Name: clientRegistrationTestDeploymentName}}
	requeue := 30 * time.Second
	ctx := context.Background()

	globalOffGates := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ClusterDefaultsNamespace,
			Name:      ClusterFeatureGatesConfigMapName,
		},
		Data: map[string]string{
			"gates.yaml": "globalEnabled: false\nclientRegistration: true\ninjectTools: false\n",
		},
	}

	reconcileCases := []struct {
		name        string
		objs        []client.Object
		wantRequeue time.Duration
		check       func(t *testing.T, c client.Client)
	}{
		{
			name: "feature gates disable client registration",
			objs: []client.Object{
				clusterFeatureGatesConfigMap(false),
				testDeploymentForClientReg(),
			},
			check: func(t *testing.T, c client.Client) {
				dep := &appsv1.Deployment{}
				if err := c.Get(ctx, req.NamespacedName, dep); err != nil {
					t.Fatal(err)
				}
				if dep.Spec.Template.Annotations != nil && dep.Spec.Template.Annotations[AnnotationKeycloakClientSecretName] != "" {
					t.Fatalf("expected no credentials annotation when gates off, got %v", dep.Spec.Template.Annotations)
				}
			},
		},
		{
			name: "global feature gate disabled skips before workload fetch",
			objs: []client.Object{globalOffGates},
		},
		{
			name: "deployment and statefulset not found",
			objs: []client.Object{clusterFeatureGatesConfigMap(true)},
		},
		{
			name: "missing authbridge config waits with requeue",
			objs: []client.Object{
				clusterFeatureGatesConfigMap(true),
				testDeploymentForClientReg(),
			},
			wantRequeue: requeue,
		},
		{
			name: "missing keycloak admin secret waits with requeue",
			objs: []client.Object{
				clusterFeatureGatesConfigMap(true),
				testDeploymentForClientReg(),
				authbridgeConfigMapForTest(clientRegistrationTestNamespace, "https://keycloak.example"),
			},
			wantRequeue: requeue,
		},
	}

	for _, tc := range reconcileCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := clientRegistrationTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			r := &ClientRegistrationReconciler{Client: c, Scheme: scheme}
			res, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if tc.wantRequeue != 0 {
				if res.RequeueAfter != tc.wantRequeue {
					t.Fatalf("got RequeueAfter=%v, want %v", res.RequeueAfter, tc.wantRequeue)
				}
			} else if res != (ctrl.Result{}) {
				t.Fatalf("got %#v, want zero ctrl.Result", res)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}

	t.Run("happy path registers client patches deployment and creates secret", func(t *testing.T) {
		srv := startTestKeycloakServer(t)
		defer srv.Close()

		scheme := clientRegistrationTestScheme(t)
		dep := testDeploymentForClientReg()
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			clusterFeatureGatesConfigMap(true),
			dep,
			authbridgeConfigMapForTest(clientRegistrationTestNamespace, srv.URL),
			keycloakAdminSecretForTest(clientRegistrationTestNamespace),
		).Build()
		r := &ClientRegistrationReconciler{Client: c, Scheme: scheme}
		res, err := r.Reconcile(ctx, req)
		if err != nil || res != (ctrl.Result{}) {
			t.Fatalf("got (%v, %v), want (zero Result, nil)", res, err)
		}

		secretName := keycloakClientCredentialsSecretName(clientRegistrationTestNamespace, clientRegistrationTestDeploymentName)
		got := &appsv1.Deployment{}
		if err := c.Get(ctx, req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Template.Annotations == nil || got.Spec.Template.Annotations[AnnotationKeycloakClientSecretName] != secretName {
			t.Fatalf("pod template annotation: %#v", got.Spec.Template.Annotations)
		}

		sec := &corev1.Secret{}
		secKey := types.NamespacedName{Namespace: clientRegistrationTestNamespace, Name: secretName}
		if err := c.Get(ctx, secKey, sec); err != nil {
			t.Fatal(err)
		}
		// Fake client may leave credential keys in StringData (like an apiserver write path) instead of Data.
		clientID := string(sec.Data["client-id.txt"])
		if clientID == "" && sec.StringData != nil {
			clientID = sec.StringData["client-id.txt"]
		}
		clientSecret := string(sec.Data["client-secret.txt"])
		if clientSecret == "" && sec.StringData != nil {
			clientSecret = sec.StringData["client-secret.txt"]
		}
		if clientID != clientRegistrationTestNamespace+"/"+clientRegistrationTestDeploymentName {
			t.Fatalf("client-id: %q (secret %#v)", clientID, sec)
		}
		if clientSecret != "client-secret-value" {
			t.Fatalf("client-secret: %q", clientSecret)
		}
	})
}
