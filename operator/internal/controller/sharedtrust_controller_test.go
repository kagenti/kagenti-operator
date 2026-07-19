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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		cmv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("adding scheme: %v", err)
		}
	}
	return s
}

type testPKI struct {
	RootCertPEM []byte
	RootKeyPEM  []byte

	IntCertPEM []byte
	IntKeyPEM  []byte
	IntCAPEM   []byte // == RootCertPEM (the CA that signed the intermediate)
}

func generateTestPKI(t *testing.T) testPKI {
	t.Helper()

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating root key: %v", err)
	}

	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-root-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("creating root cert: %v", err)
	}
	rootCert, _ := x509.ParseCertificate(rootDER)
	rootCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})

	rootKeyDER, _ := x509.MarshalECPrivateKey(rootKey)
	rootKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: rootKeyDER})

	intKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating intermediate key: %v", err)
	}

	intTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "test-intermediate"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	intDER, err := x509.CreateCertificate(rand.Reader, intTmpl, rootCert, &intKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("creating intermediate cert: %v", err)
	}
	intCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intDER})

	intKeyDER, _ := x509.MarshalECPrivateKey(intKey)
	intKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: intKeyDER})

	return testPKI{
		RootCertPEM: rootCertPEM,
		RootKeyPEM:  rootKeyPEM,
		IntCertPEM:  intCertPEM,
		IntKeyPEM:   intKeyPEM,
		IntCAPEM:    rootCertPEM,
	}
}

func readyCertificate(name, namespace string) *cmv1.Certificate {
	return &cmv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: cmv1.CertificateStatus{
			Conditions: []cmv1.CertificateCondition{
				{
					Type:   cmv1.CertificateConditionReady,
					Status: cmmeta.ConditionTrue,
				},
			},
		},
	}
}

func notReadyCertificate(name, namespace string) *cmv1.Certificate {
	return &cmv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: cmv1.CertificateStatus{
			Conditions: []cmv1.CertificateCondition{
				{
					Type:   cmv1.CertificateConditionReady,
					Status: cmmeta.ConditionFalse,
				},
			},
		},
	}
}

func intermediateSecret(name, namespace string, pki testPKI) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"tls.crt": pki.IntCertPEM,
			"tls.key": pki.IntKeyPEM,
			"ca.crt":  pki.IntCAPEM,
		},
	}
}

func rootCASecret(pki testPKI) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: RootCASecretName, Namespace: RootCANamespace},
		Data: map[string][]byte{
			"tls.crt": pki.RootCertPEM,
			"tls.key": pki.RootKeyPEM,
		},
	}
}

func testNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func newReconciler(t *testing.T, objs ...runtime.Object) *SharedTrustReconciler {
	t.Helper()
	scheme := newTestScheme(t)
	clientObjs := make([]runtime.Object, 0, len(objs))
	clientObjs = append(clientObjs, objs...)

	cb := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(clientObjs...)
	return &SharedTrustReconciler{
		Client:   cb.Build(),
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
}

var testReq = ctrl.Request{NamespacedName: types.NamespacedName{Name: "shared-trust", Namespace: IstioSystemNamespace}}

func TestReconcile_CertificatesMissing(t *testing.T) {
	r := newReconciler(t,
		testNamespace(RootCANamespace),
		testNamespace(IstioSystemNamespace),
		testNamespace(OpenShiftIngressNamespace),
	)

	result, err := r.Reconcile(context.Background(), testReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != requeuePrecondition {
		t.Errorf("expected requeue after %v, got %v", requeuePrecondition, result.RequeueAfter)
	}
}

func TestReconcile_CertificateNotReady(t *testing.T) {
	r := newReconciler(t,
		testNamespace(RootCANamespace),
		testNamespace(IstioSystemNamespace),
		testNamespace(OpenShiftIngressNamespace),
		readyCertificate(RootCACertName, RootCANamespace),
		notReadyCertificate(IstioSystemCertName, IstioSystemNamespace),
		readyCertificate(OpenShiftIngressCertName, OpenShiftIngressNamespace),
	)

	result, err := r.Reconcile(context.Background(), testReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != requeueReadiness {
		t.Errorf("expected requeue after %v, got %v", requeueReadiness, result.RequeueAfter)
	}
}

func TestReconcile_FingerprintMismatch(t *testing.T) {
	pki := generateTestPKI(t)
	otherRootCert := generateSelfSignedCert(t, "other-root")

	r := newReconciler(t,
		testNamespace(RootCANamespace),
		testNamespace(IstioSystemNamespace),
		testNamespace(OpenShiftIngressNamespace),
		readyCertificate(RootCACertName, RootCANamespace),
		readyCertificate(IstioSystemCertName, IstioSystemNamespace),
		readyCertificate(OpenShiftIngressCertName, OpenShiftIngressNamespace),
		rootCASecret(pki),
		// Default intermediate has mismatched ca.crt
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: IstioSystemSecretName, Namespace: IstioSystemNamespace},
			Data: map[string][]byte{
				"tls.crt": pki.IntCertPEM,
				"tls.key": pki.IntKeyPEM,
				"ca.crt":  otherRootCert, // wrong root
			},
		},
		intermediateSecret(OpenShiftIngressSecretName, OpenShiftIngressNamespace, pki),
	)

	result, err := r.Reconcile(context.Background(), testReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 after fingerprint mismatch")
	}

	// Verify the mismatched secret was deleted
	secret := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{Name: IstioSystemSecretName, Namespace: IstioSystemNamespace}, secret)
	if err == nil {
		t.Error("expected mismatched secret to be deleted")
	}
}

func TestReconcile_HappyPath_CreatesCacerts(t *testing.T) {
	pki := generateTestPKI(t)

	r := newReconciler(t,
		testNamespace(RootCANamespace),
		testNamespace(IstioSystemNamespace),
		testNamespace(OpenShiftIngressNamespace),
		readyCertificate(RootCACertName, RootCANamespace),
		readyCertificate(IstioSystemCertName, IstioSystemNamespace),
		readyCertificate(OpenShiftIngressCertName, OpenShiftIngressNamespace),
		rootCASecret(pki),
		intermediateSecret(IstioSystemSecretName, IstioSystemNamespace, pki),
		intermediateSecret(OpenShiftIngressSecretName, OpenShiftIngressNamespace, pki),
	)

	result, err := r.Reconcile(context.Background(), testReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %+v", result)
	}

	for _, ns := range []string{IstioSystemNamespace, OpenShiftIngressNamespace} {
		secret := &corev1.Secret{}
		err := r.Get(context.Background(), types.NamespacedName{Name: CacertsSecretName, Namespace: ns}, secret)
		if err != nil {
			t.Fatalf("cacerts not created in %s: %v", ns, err)
		}

		for _, key := range []string{"ca-cert.pem", "ca-key.pem", "root-cert.pem", "cert-chain.pem"} {
			if _, ok := secret.Data[key]; !ok {
				t.Errorf("cacerts in %s missing key %q", ns, key)
			}
		}

		expectedChain := string(pki.IntCertPEM) + string(pki.IntCAPEM)
		if string(secret.Data["cert-chain.pem"]) != expectedChain {
			t.Error("cert-chain.pem is not concatenation of intermediate + root")
		}
	}
}

func TestReconcile_UpdatesCacertsOnChange(t *testing.T) {
	pki := generateTestPKI(t)

	existingCacerts := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CacertsSecretName, Namespace: IstioSystemNamespace},
		Data: map[string][]byte{
			"ca-cert.pem":    []byte("old-data"),
			"ca-key.pem":     []byte("old-key"),
			"root-cert.pem":  []byte("old-root"),
			"cert-chain.pem": []byte("old-chain"),
		},
	}

	r := newReconciler(t,
		testNamespace(RootCANamespace),
		testNamespace(IstioSystemNamespace),
		testNamespace(OpenShiftIngressNamespace),
		readyCertificate(RootCACertName, RootCANamespace),
		readyCertificate(IstioSystemCertName, IstioSystemNamespace),
		readyCertificate(OpenShiftIngressCertName, OpenShiftIngressNamespace),
		rootCASecret(pki),
		intermediateSecret(IstioSystemSecretName, IstioSystemNamespace, pki),
		intermediateSecret(OpenShiftIngressSecretName, OpenShiftIngressNamespace, pki),
		existingCacerts,
	)

	result, err := r.Reconcile(context.Background(), testReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %+v", result)
	}

	updated := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: CacertsSecretName, Namespace: IstioSystemNamespace}, updated); err != nil {
		t.Fatalf("getting updated cacerts: %v", err)
	}

	if string(updated.Data["ca-cert.pem"]) == "old-data" {
		t.Error("cacerts data was not updated")
	}
}

func TestReconcile_NoRestartWhenUnchanged(t *testing.T) {
	pki := generateTestPKI(t)
	expectedData := buildCacertsData(pki.IntCertPEM, pki.IntKeyPEM, pki.IntCAPEM)

	existingCacertsDefault := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CacertsSecretName, Namespace: IstioSystemNamespace},
		Data:       expectedData,
	}

	ogData := buildCacertsData(pki.IntCertPEM, pki.IntKeyPEM, pki.IntCAPEM)
	existingCacertsOG := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CacertsSecretName, Namespace: OpenShiftIngressNamespace},
		Data:       ogData,
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: IstiodDeployment, Namespace: IstioSystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "istiod"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "istiod"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "istiod", Image: "istiod:latest"}}},
			},
		},
	}

	r := newReconciler(t,
		testNamespace(RootCANamespace),
		testNamespace(IstioSystemNamespace),
		testNamespace(OpenShiftIngressNamespace),
		readyCertificate(RootCACertName, RootCANamespace),
		readyCertificate(IstioSystemCertName, IstioSystemNamespace),
		readyCertificate(OpenShiftIngressCertName, OpenShiftIngressNamespace),
		rootCASecret(pki),
		intermediateSecret(IstioSystemSecretName, IstioSystemNamespace, pki),
		intermediateSecret(OpenShiftIngressSecretName, OpenShiftIngressNamespace, pki),
		existingCacertsDefault,
		existingCacertsOG,
		deploy,
	)

	_, err := r.Reconcile(context.Background(), testReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify deployment was NOT restarted (no annotation)
	d := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: IstiodDeployment, Namespace: IstioSystemNamespace}, d); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	if _, ok := d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; ok {
		t.Error("deployment should NOT be restarted when cacerts unchanged")
	}
}
