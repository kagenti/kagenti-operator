package controller

import (
	"context"
	"testing"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

func tlsBridgeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := agentv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := cmv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTLSBridgeCAReconciler_CreatesIssuerAndCert(t *testing.T) {
	scheme := tlsBridgeScheme(t)
	// CR name differs from the target workload name on purpose: the Secret must
	// be keyed on the workload (TargetRef.Name), which is what the webhook mounts.
	ar := &agentv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "myagent-runtime", Namespace: "team1"},
		Spec: agentv1alpha1.AgentRuntimeSpec{
			TLSBridgeMode: agentv1alpha1.TLSBridgeModeEnabled,
			TargetRef:     agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "myworkload"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ar).Build()
	r := &TLSBridgeCAReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "myagent-runtime", Namespace: "team1"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	iss := &cmv1.Issuer{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: tlsBridgeSelfSignedIssuer, Namespace: "team1"}, iss); err != nil {
		t.Fatalf("self-signed issuer not created: %v", err)
	}
	if iss.Spec.SelfSigned == nil {
		t.Error("issuer is not self-signed")
	}

	cert := &cmv1.Certificate{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "myworkload-tls-bridge-ca", Namespace: "team1"}, cert); err != nil {
		t.Fatalf("CA certificate not created (keyed on workload name): %v", err)
	}
	if !cert.Spec.IsCA {
		t.Error("certificate is not isCA (authbridge FileSource would reject the Secret)")
	}
	if cert.Spec.SecretName != "myworkload-tls-bridge-ca" {
		t.Errorf("secretName = %q, want myworkload-tls-bridge-ca (workload-keyed)", cert.Spec.SecretName)
	}
	hasCertSign := false
	for _, u := range cert.Spec.Usages {
		if u == cmv1.UsageCertSign {
			hasCertSign = true
		}
	}
	if !hasCertSign {
		t.Error("certificate lacks cert-sign usage (FileSource validation would reject it)")
	}
	if cert.Spec.NameConstraints != nil {
		t.Error("CA must be unconstrained (no nameConstraints) per decision Q2")
	}
}

func TestTLSBridgeCAReconciler_Disabled(t *testing.T) {
	scheme := tlsBridgeScheme(t)
	certName := types.NamespacedName{Name: "off-tls-bridge-ca", Namespace: "team1"}

	// tlsBridgeMode disabled (the default) → no Certificate provisioned.
	off := &agentv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "off", Namespace: "team1"},
		Spec:       agentv1alpha1.AgentRuntimeSpec{TLSBridgeMode: agentv1alpha1.TLSBridgeModeDisabled},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(off).Build()
	r := &TLSBridgeCAReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "off", Namespace: "team1"}}); err != nil {
		t.Fatalf("reconcile (disabled): %v", err)
	}
	if err := c.Get(context.Background(), certName, &cmv1.Certificate{}); err == nil {
		t.Error("disabled agent must not get a Certificate")
	}
}
