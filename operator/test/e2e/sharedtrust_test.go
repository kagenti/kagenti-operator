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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rossoctl/operator/test/utils"
)

var _ = Describe("SharedTrust E2E", Ordered, func() {
	controllerNamespace := controllerNS()

	BeforeAll(func() {
		Expect(utils.DeployController(controllerNamespace, projectImage)).To(Succeed(), "Failed to deploy controller")

		By("waiting for controller-manager to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace,
				"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .status.phase }}{{ end }}{{ end }}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Running"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("creating required namespaces")
		for _, ns := range []string{"cert-manager", "istio-system", "openshift-ingress", "istio-ztunnel"} {
			cmd := exec.Command("kubectl", "create", "ns", ns, "--dry-run=client", "-o", "yaml")
			yaml, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.KubectlApplyStdin(yaml, "")
			Expect(err).NotTo(HaveOccurred())
		}

		By("creating SelfSigned ClusterIssuer")
		_, err := utils.KubectlApplyStdin(selfSignedClusterIssuerYAML(), "")
		Expect(err).NotTo(HaveOccurred())

		By("creating root CA Certificate")
		_, err = utils.KubectlApplyStdin(rootCACertificateYAML(), "")
		Expect(err).NotTo(HaveOccurred())

		By("creating root CA ClusterIssuer")
		_, err = utils.KubectlApplyStdin(rootCAClusterIssuerYAML(), "")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for root CA secret")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "secret", "istio-mesh-root-ca-secret",
				"-n", "cert-manager", "-o", "jsonpath={.data.tls\\.crt}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("creating intermediate Certificates")
		defaultCertYAML := intermediateCertificateYAML(
			"istio-cacerts-default", "istio-cacerts-default-cert", "istio-system")
		_, err = utils.KubectlApplyStdin(defaultCertYAML, "")
		Expect(err).NotTo(HaveOccurred())
		gatewayCertYAML := intermediateCertificateYAML(
			"istio-cacerts-openshift-gateway", "istio-cacerts-og-cert", "openshift-ingress")
		_, err = utils.KubectlApplyStdin(gatewayCertYAML, "")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for intermediate secrets to be created")
		for _, s := range []struct{ name, ns string }{
			{"istio-cacerts-default-cert", "istio-system"},
			{"istio-cacerts-og-cert", "openshift-ingress"},
		} {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secret", s.name,
					"-n", s.ns, "-o", "jsonpath={.data.tls\\.crt}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		}
	})

	AfterAll(func() {
		By("cleaning up Certificate resources")
		for _, args := range [][]string{
			{"delete", "certificate", "istio-mesh-root-ca", "-n", "cert-manager", "--ignore-not-found"},
			{"delete", "certificate", "istio-cacerts-default", "-n", "istio-system", "--ignore-not-found"},
			{"delete", "certificate", "istio-cacerts-openshift-gateway", "-n", "openshift-ingress", "--ignore-not-found"},
			{"delete", "clusterissuer", "istio-mesh-root-ca-issuer", "--ignore-not-found"},
			{"delete", "clusterissuer", "selfsigned-issuer", "--ignore-not-found"},
		} {
			cmd := exec.Command("kubectl", args...)
			_, _ = utils.Run(cmd)
		}

		By("cleaning up secrets and namespaces")
		for _, ns := range []string{"istio-system", "openshift-ingress", "istio-ztunnel"} {
			cmd := exec.Command("kubectl", "delete", "secret", "cacerts", "-n", ns, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}

		utils.UndeployController()

		By("removing manager namespace")
		cmd := exec.Command("kubectl", "delete", "ns", controllerNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace, "--tail=200")
			logs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", logs)
			}

			for _, ns := range []string{"cert-manager", "istio-system", "openshift-ingress"} {
				cmd = exec.Command("kubectl", "get", "events", "-n", ns, "--sort-by=.lastTimestamp")
				events, err := utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Events in %s:\n%s\n", ns, events)
				}
			}
		}
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("should create cacerts Secrets in both namespaces", func() {
		for _, ns := range []string{"istio-system", "openshift-ingress"} {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secret", "cacerts", "-n", ns,
					"-o", "jsonpath={.data}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				for _, key := range []string{"ca-cert.pem", "ca-key.pem", "root-cert.pem", "cert-chain.pem"} {
					g.Expect(output).To(ContainSubstring(key),
						fmt.Sprintf("cacerts in %s missing key %s", ns, key))
				}
			}).Should(Succeed(), fmt.Sprintf("cacerts not created in %s", ns))
		}
	})

	It("should rebuild cacerts after intermediate secret deletion", func() {
		By("recording current cacerts data")
		cmd := exec.Command("kubectl", "get", "secret", "cacerts", "-n", "istio-system",
			"-o", "jsonpath={.metadata.resourceVersion}")
		origVersion, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("deleting intermediate secret to trigger re-issuance")
		cmd = exec.Command("kubectl", "delete", "secret", "istio-cacerts-default-cert", "-n", "istio-system")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for cert-manager to re-issue the intermediate")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "secret", "istio-cacerts-default-cert",
				"-n", "istio-system", "-o", "jsonpath={.data.tls\\.crt}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())
		}).Should(Succeed())

		By("verifying cacerts was rebuilt (resourceVersion changed)")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "secret", "cacerts", "-n", "istio-system",
				"-o", "jsonpath={.metadata.resourceVersion}")
			newVersion, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(newVersion)).NotTo(Equal(strings.TrimSpace(origVersion)),
				"cacerts resourceVersion should change after rebuild")
		}).Should(Succeed())
	})

	It("should apply restart annotation on istiod and ztunnel after cert change", func() {
		By("deploying a dummy istiod Deployment in istio-system")
		_, err := utils.KubectlApplyStdin(dummyIstiodDeploymentYAML(), "")
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.WaitForDeploymentReady("istiod", "istio-system", 2*time.Minute)).To(Succeed())

		By("deploying a dummy ztunnel DaemonSet in istio-ztunnel")
		_, err = utils.KubectlApplyStdin(dummyZtunnelDaemonSetYAML(), "")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "daemonset", "ztunnel", "-n", "istio-ztunnel",
				"-o", "jsonpath={.status.numberReady}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(Equal("0"), "ztunnel should have ready pods")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("recording current restart annotation values (may be empty or from prior reconcile)")
		istiodAnnotationBefore, _ := utils.KubectlGetJsonpath("deployment", "istiod", "istio-system",
			"{.spec.template.metadata.annotations.kubectl\\.kubernetes\\.io/restartedAt}")
		ztunnelAnnotationBefore, _ := utils.KubectlGetJsonpath("daemonset", "ztunnel", "istio-ztunnel",
			"{.spec.template.metadata.annotations.kubectl\\.kubernetes\\.io/restartedAt}")

		By("deleting cacerts so reconciler must recreate on next trigger")
		cmd := exec.Command("kubectl", "delete", "secret", "cacerts", "-n", "istio-system", "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "secret", "cacerts", "-n", "openshift-ingress", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("deleting intermediate TLS secret to trigger Certificate re-issuance and reconciliation")
		cmd = exec.Command("kubectl", "delete", "secret", "istio-cacerts-default-cert", "-n", "istio-system")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for cert-manager to re-issue the intermediate")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "secret", "istio-cacerts-default-cert",
				"-n", "istio-system", "-o", "jsonpath={.data.tls\\.crt}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("waiting for restart annotation to change on istiod")
		Eventually(func(g Gomega) {
			output, err := utils.KubectlGetJsonpath("deployment", "istiod", "istio-system",
				"{.spec.template.metadata.annotations.kubectl\\.kubernetes\\.io/restartedAt}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
				"istiod should have restartedAt annotation")
			g.Expect(strings.TrimSpace(output)).NotTo(Equal(strings.TrimSpace(istiodAnnotationBefore)),
				"istiod restartedAt annotation should change after cert rotation")
		}).Should(Succeed())

		By("waiting for restart annotation to change on ztunnel")
		Eventually(func(g Gomega) {
			output, err := utils.KubectlGetJsonpath("daemonset", "ztunnel", "istio-ztunnel",
				"{.spec.template.metadata.annotations.kubectl\\.kubernetes\\.io/restartedAt}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
				"ztunnel should have restartedAt annotation")
			g.Expect(strings.TrimSpace(output)).NotTo(Equal(strings.TrimSpace(ztunnelAnnotationBefore)),
				"ztunnel restartedAt annotation should change after cert rotation")
		}).Should(Succeed())

		By("cleaning up dummy workloads")
		cmd = exec.Command("kubectl", "delete", "deployment", "istiod", "-n", "istio-system", "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "daemonset", "ztunnel", "-n", "istio-ztunnel", "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("should handle missing istiod/ztunnel gracefully", func() {
		By("verifying controller is still running (no crash from missing workloads)")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace,
				"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .status.phase }}{{ end }}{{ end }}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Running"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("checking controller logs for graceful skip messages")
		cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
			"-n", controllerNamespace, "--tail=200")
		logs, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("not found, skipping restart"),
			"controller should log skip messages for missing workloads")
	})
})

func selfSignedClusterIssuerYAML() string {
	return `apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
`
}

func rootCACertificateYAML() string {
	return `apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: istio-mesh-root-ca
  namespace: cert-manager
spec:
  isCA: true
  commonName: istio-mesh-root-ca
  secretName: istio-mesh-root-ca-secret
  duration: 87600h
  renewBefore: 720h
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: selfsigned-issuer
    kind: ClusterIssuer
    group: cert-manager.io
`
}

func rootCAClusterIssuerYAML() string {
	return `apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: istio-mesh-root-ca-issuer
spec:
  ca:
    secretName: istio-mesh-root-ca-secret
`
}

func intermediateCertificateYAML(name, secretName, namespace string) string {
	return fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
  namespace: %s
spec:
  isCA: true
  commonName: %s
  secretName: %s
  duration: 8760h
  renewBefore: 720h
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: istio-mesh-root-ca-issuer
    kind: ClusterIssuer
    group: cert-manager.io
`, name, namespace, name, secretName)
}

func dummyIstiodDeploymentYAML() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: istiod
  namespace: istio-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: istiod
  template:
    metadata:
      labels:
        app: istiod
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("65534") + `
      containers:
      - name: discovery
        image: registry.k8s.io/pause:3.9
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
          seccompProfile:
            type: RuntimeDefault
`
}

func dummyZtunnelDaemonSetYAML() string {
	return `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ztunnel
  namespace: istio-ztunnel
spec:
  selector:
    matchLabels:
      app: ztunnel
  template:
    metadata:
      labels:
        app: ztunnel
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("65534") + `
      containers:
      - name: ztunnel
        image: registry.k8s.io/pause:3.9
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
          seccompProfile:
            type: RuntimeDefault
`
}
