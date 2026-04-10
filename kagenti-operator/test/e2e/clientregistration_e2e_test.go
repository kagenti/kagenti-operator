/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kagenti/operator/test/utils"
)

const clientRegNamespace = "e2e-clientreg-test"

var _ = Describe("ClientRegistration E2E", Ordered, func() {
	var origArgs []string

	BeforeAll(func() {
		By("deploying controller")
		Expect(utils.DeployController(namespace, projectImage)).To(Succeed(), "Failed to deploy controller")

		By("waiting for controller-manager to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", namespace,
				"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .status.phase }}{{ end }}{{ end }}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Running"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("patching operator to enable ClientRegistration controller")
		var err error
		origArgs, err = utils.PatchControllerArgs(namespace, "kagenti-operator-controller-manager", []string{
			"--enable-operator-client-registration=true",
			"--spire-trust-domain=example.org",
		})
		Expect(err).NotTo(HaveOccurred(), "Failed to patch operator deployment")

		By("creating clientreg test namespace")
		cmd := exec.Command("kubectl", "create", "ns", clientRegNamespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create clientreg test namespace")

		By("labeling the clientreg namespace for restricted PSA and agentcard")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", clientRegNamespace,
			"pod-security.kubernetes.io/enforce=restricted",
			"agentcard=true")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label clientreg namespace")

		By("deploying mock Keycloak server")
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(mockKeycloakFixture())
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy mock Keycloak")

		By("waiting for mock Keycloak to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "deployment", "mock-keycloak",
				"-n", clientRegNamespace, "-o", "jsonpath={.status.readyReplicas}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("1"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up clientreg test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", clientRegNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("restoring operator to default args")
		if origArgs != nil {
			_ = utils.RestoreControllerArgs(namespace, "kagenti-operator-controller-manager", origArgs)
		}

		utils.UndeployController()

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching deployment details on failure")
			cmd := exec.Command("kubectl", "get", "deployments", "-n", clientRegNamespace, "-o", "wide")
			output, _ := utils.Run(cmd)
			_, _ = fmt.Fprintf(GinkgoWriter, "Deployments:\n%s\n", output)

			By("Fetching secrets on failure")
			cmd = exec.Command("kubectl", "get", "secrets", "-n", clientRegNamespace)
			output, _ = utils.Run(cmd)
			_, _ = fmt.Fprintf(GinkgoWriter, "Secrets:\n%s\n", output)

			By("Fetching configmaps on failure")
			cmd = exec.Command("kubectl", "get", "configmaps", "-n", clientRegNamespace)
			output, _ = utils.Run(cmd)
			_, _ = fmt.Fprintf(GinkgoWriter, "ConfigMaps:\n%s\n", output)

			By("Fetching operator logs on failure")
			cmd = exec.Command("kubectl", "logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail=100")
			output, _ = utils.Run(cmd)
			_, _ = fmt.Fprintf(GinkgoWriter, "Operator logs:\n%s\n", output)
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	Context("Non-SPIRE mode", func() {
		BeforeAll(func() {
			By("deploying authbridge-config (SPIRE_ENABLED=false)")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(authbridgeConfigFixture(false))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("deploying keycloak-admin-secret")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(keycloakAdminSecretFixture())
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should create client credentials Secret for agent workload", func() {
			By("deploying clientreg-agent")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(clientRegAgentFixture())
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Deployment to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "clientreg-agent",
					"-n", clientRegNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}).Should(Succeed())

			By("verifying client credentials Secret was created")
			var secretName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "-n", clientRegNamespace,
					"-l", "app.kubernetes.io/managed-by=kagenti-operator",
					"-o", "jsonpath={.items[0].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "Expected client credentials Secret to be created")
				secretName = output
			}).Should(Succeed())

			By("verifying Secret contains client-id.txt and client-secret.txt")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secret", secretName, "-n", clientRegNamespace,
					"-o", "jsonpath={.data}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("client-id.txt"))
				g.Expect(output).To(ContainSubstring("client-secret.txt"))
			}).Should(Succeed())

			By("verifying client ID format is namespace/workloadName")
			cmd = exec.Command("kubectl", "get", "secret", secretName, "-n", clientRegNamespace,
				"-o", "jsonpath={.data.client-id\\.txt}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Decode base64
			cmd = exec.Command("base64", "-d")
			cmd.Stdin = strings.NewReader(output)
			clientID, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(clientID)).To(Equal(clientRegNamespace + "/clientreg-agent"))

			By("verifying pod template has keycloak-client-credentials-secret-name annotation")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "clientreg-agent",
					"-n", clientRegNamespace,
					"-o", "jsonpath={.spec.template.metadata.annotations.kagenti\\.io/keycloak-client-credentials-secret-name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(secretName))
			}).Should(Succeed())
		})
	})

	Context("SPIRE mode with default template", func() {
		BeforeAll(func() {
			By("deploying authbridge-config (SPIRE_ENABLED=true)")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(authbridgeConfigFixture(true))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should create SPIFFE ID using default template", func() {
			By("deploying clientreg-spire-agent with dedicated ServiceAccount")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(clientRegAgentSpireFixture())
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Deployment to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "clientreg-spire-agent",
					"-n", clientRegNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}).Should(Succeed())

			By("verifying client credentials Secret was created")
			var secretName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "-n", clientRegNamespace,
					"--selector=app.kubernetes.io/managed-by=kagenti-operator",
					"-o", "jsonpath={.items[?(@.metadata.ownerReferences[0].name=='clientreg-spire-agent')].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "Expected client credentials Secret for SPIRE agent")
				secretName = strings.TrimSpace(output)
			}).Should(Succeed())

			By("verifying client ID uses SPIFFE ID format with default template")
			cmd = exec.Command("kubectl", "get", "secret", secretName, "-n", clientRegNamespace,
				"-o", "jsonpath={.data.client-id\\.txt}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Decode base64
			cmd = exec.Command("base64", "-d")
			cmd.Stdin = strings.NewReader(output)
			clientID, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Default template: spiffe://{{.TrustDomain}}/ns/{{.Namespace}}/sa/{{.ServiceAccount}}
			expectedClientID := fmt.Sprintf("spiffe://example.org/ns/%s/sa/clientreg-spire-sa", clientRegNamespace)
			Expect(strings.TrimSpace(clientID)).To(Equal(expectedClientID))
		})
	})

	Context("SPIRE mode with custom template", func() {
		It("should create SPIFFE ID using custom template from env var", func() {
			By("patching operator deployment with custom SPIRE_ID_TEMPLATE")
			customTemplate := "spiffe://{{.TrustDomain}}/workload/{{.Namespace}}/{{.ServiceAccount}}"
			cmd := exec.Command("kubectl", "set", "env", "deployment/kagenti-operator-controller-manager",
				"-n", namespace,
				fmt.Sprintf("SPIRE_ID_TEMPLATE=%s", customTemplate))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for operator rollout after env var change")
			err = utils.WaitForRollout("kagenti-operator-controller-manager", namespace, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("deploying a new agent to test custom template")
			customAgentYAML := `apiVersion: v1
kind: ServiceAccount
metadata:
  name: clientreg-custom-sa
  namespace: ` + clientRegNamespace + `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: clientreg-custom-agent
  namespace: ` + clientRegNamespace + `
  labels:
    kagenti.io/type: agent
    protocol.kagenti.io/a2a: ""
spec:
  replicas: 1
  selector:
    matchLabels:
      app: clientreg-custom-agent
  template:
    metadata:
      labels:
        app: clientreg-custom-agent
        kagenti.io/type: agent
        protocol.kagenti.io/a2a: ""
    spec:
      serviceAccountName: clientreg-custom-sa
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(customAgentYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for custom agent Deployment to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "clientreg-custom-agent",
					"-n", clientRegNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}).Should(Succeed())

			By("verifying client ID uses custom SPIFFE ID template")
			var secretName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "-n", clientRegNamespace,
					"--selector=app.kubernetes.io/managed-by=kagenti-operator",
					"-o", "jsonpath={.items[?(@.metadata.ownerReferences[0].name=='clientreg-custom-agent')].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				secretName = strings.TrimSpace(output)
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			cmd = exec.Command("kubectl", "get", "secret", secretName, "-n", clientRegNamespace,
				"-o", "jsonpath={.data.client-id\\.txt}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Decode base64
			cmd = exec.Command("base64", "-d")
			cmd.Stdin = strings.NewReader(output)
			clientID, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Custom template: spiffe://{{.TrustDomain}}/workload/{{.Namespace}}/{{.ServiceAccount}}
			expectedClientID := fmt.Sprintf("spiffe://example.org/workload/%s/clientreg-custom-sa", clientRegNamespace)
			Expect(strings.TrimSpace(clientID)).To(Equal(expectedClientID))

			By("restoring operator to default template")
			cmd = exec.Command("kubectl", "set", "env", "deployment/kagenti-operator-controller-manager",
				"-n", namespace, "SPIRE_ID_TEMPLATE-")
			_, _ = utils.Run(cmd)
		})
	})
})
