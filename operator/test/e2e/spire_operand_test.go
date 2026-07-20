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

// SPIRE operand CRD kinds and their plural resource names.
var spireOperandResources = []struct {
	kind     string
	plural   string
	hasSpec  bool
	specKeys []string
}{
	{"ZeroTrustWorkloadIdentityManager", "zerotrustworkloadidentitymanagers", true,
		[]string{"trustDomain", "clusterName", "bundleConfigMap"}},
	{"SpiffeCSIDriver", "spiffecsidrivers", true,
		[]string{"agentSocketPath", "pluginName"}},
	{"SpireServer", "spireservers", true,
		[]string{"caSubject", "datastore", "jwtIssuer"}},
	{"SpireAgent", "spireagents", true,
		[]string{"nodeAttestor", "workloadAttestors"}},
	{"SpireOIDCDiscoveryProvider", "spireoidcdiscoveryproviders", true,
		[]string{"csiDriverName", "jwtIssuer"}},
}

var _ = Describe("SPIRE Operand Controller E2E", Ordered, func() {
	const controllerNamespace = "rossoctl-operator-system"

	BeforeAll(func() {
		By("installing mock ZTWIM CRDs into Kind cluster")
		_, err := utils.KubectlApplyStdin(ztwimMockCRDsYAML(), "")
		Expect(err).NotTo(HaveOccurred(), "Failed to install mock ZTWIM CRDs")

		By("waiting for CRDs to be established")
		for _, res := range spireOperandResources {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "crd",
					fmt.Sprintf("%s.operator.openshift.io", res.plural))
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 30*time.Second, 2*time.Second).Should(Succeed(),
				fmt.Sprintf("CRD %s not established", res.plural))
		}

		By("deploying controller with SPIRE operand support")
		Expect(utils.DeployController(controllerNamespace, projectImage)).To(Succeed(),
			"Failed to deploy controller")

		By("setting SPIRE trust domain for SPIRE operand controller (no ZTWIM CR to discover from in Kind)")
		setEnvCmd := exec.Command("kubectl", "set", "env",
			"deployment/rossoctl-operator-controller-manager",
			"-n", controllerNamespace,
			"ROSSOCTL_SPIRE_TRUST_DOMAIN=example.org")
		_, err = utils.Run(setEnvCmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for controller-manager to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace,
				"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .status.phase }}{{ end }}{{ end }}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("Running"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up SPIRE operand CRs")
		for _, res := range spireOperandResources {
			cmd := exec.Command("kubectl", "delete",
				fmt.Sprintf("%s.operator.openshift.io", res.plural),
				"cluster", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}

		By("cleaning up mock CRDs")
		for _, res := range spireOperandResources {
			cmd := exec.Command("kubectl", "delete", "crd",
				fmt.Sprintf("%s.operator.openshift.io", res.plural),
				"--ignore-not-found")
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
		}
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("should create all 5 SPIRE operand CRs when CRDs are present", func() {
		for _, res := range spireOperandResources {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					fmt.Sprintf("%s.operator.openshift.io", res.plural),
					"cluster", "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("cluster"))
			}).Should(Succeed(), fmt.Sprintf("%s CR not created", res.kind))
		}
	})

	It("should label all CRs with managed-by rossoctl-operator", func() {
		for _, res := range spireOperandResources {
			Eventually(func(g Gomega) {
				output, err := utils.KubectlGetJsonpath(
					fmt.Sprintf("%s.operator.openshift.io", res.plural),
					"cluster", "",
					"{.metadata.labels.app\\.kubernetes\\.io/managed-by}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("rossoctl-operator"))
			}).Should(Succeed(), fmt.Sprintf("%s missing managed-by label", res.kind))
		}
	})

	It("should set correct ZTWIM spec", func() {
		Eventually(func(g Gomega) {
			clusterName, err := utils.KubectlGetJsonpath(
				"zerotrustworkloadidentitymanagers.operator.openshift.io",
				"cluster", "", "{.spec.clusterName}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(clusterName)).To(Equal("agent-platform"))

			bundleCM, err := utils.KubectlGetJsonpath(
				"zerotrustworkloadidentitymanagers.operator.openshift.io",
				"cluster", "", "{.spec.bundleConfigMap}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(bundleCM)).To(Equal("spire-bundle"))

			td, err := utils.KubectlGetJsonpath(
				"zerotrustworkloadidentitymanagers.operator.openshift.io",
				"cluster", "", "{.spec.trustDomain}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(td)).NotTo(BeEmpty())
		}).Should(Succeed())
	})

	It("should set correct SpiffeCSIDriver spec", func() {
		Eventually(func(g Gomega) {
			socketPath, err := utils.KubectlGetJsonpath(
				"spiffecsidrivers.operator.openshift.io",
				"cluster", "", "{.spec.agentSocketPath}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(socketPath)).To(Equal("/run/spire/agent-sockets"))

			pluginName, err := utils.KubectlGetJsonpath(
				"spiffecsidrivers.operator.openshift.io",
				"cluster", "", "{.spec.pluginName}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(pluginName)).To(Equal("csi.spiffe.io"))
		}).Should(Succeed())
	})

	It("should not set trustDomain on child CRs", func() {
		childResources := []string{
			"spiffecsidrivers.operator.openshift.io",
			"spireservers.operator.openshift.io",
			"spireagents.operator.openshift.io",
			"spireoidcdiscoveryproviders.operator.openshift.io",
		}
		for _, res := range childResources {
			Eventually(func(g Gomega) {
				td, err := utils.KubectlGetJsonpath(res, "cluster", "", "{.spec.trustDomain}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(td)).To(BeEmpty(),
					fmt.Sprintf("%s should not have trustDomain in spec", res))
			}).Should(Succeed())
		}
	})

	It("should reconcile drift when child CR is deleted", func() {
		By("deleting SpireAgent CR")
		cmd := exec.Command("kubectl", "delete",
			"spireagents.operator.openshift.io", "cluster")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for SpireAgent CR to be recreated")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get",
				"spireagents.operator.openshift.io", "cluster",
				"-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("cluster"))
		}).Should(Succeed(), "SpireAgent CR was not recreated after deletion")

		By("verifying recreated CR has managed-by label")
		Eventually(func(g Gomega) {
			output, err := utils.KubectlGetJsonpath(
				"spireagents.operator.openshift.io",
				"cluster", "",
				"{.metadata.labels.app\\.kubernetes\\.io/managed-by}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("rossoctl-operator"))
		}).Should(Succeed())
	})

	It("should reconcile drift when ZTWIM spec is modified", func() {
		By("patching ZTWIM bundleConfigMap to wrong value")
		cmd := exec.Command("kubectl", "patch",
			"zerotrustworkloadidentitymanagers.operator.openshift.io", "cluster",
			"--type=merge", "-p", `{"spec":{"bundleConfigMap":"wrong-bundle"}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for bundleConfigMap to be restored")
		Eventually(func(g Gomega) {
			output, err := utils.KubectlGetJsonpath(
				"zerotrustworkloadidentitymanagers.operator.openshift.io",
				"cluster", "", "{.spec.bundleConfigMap}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("spire-bundle"))
		}).Should(Succeed(), "ZTWIM bundleConfigMap was not restored after drift")
	})

	It("should reconcile drift when child spec is modified", func() {
		By("patching SpiffeCSIDriver pluginName to wrong value")
		cmd := exec.Command("kubectl", "patch",
			"spiffecsidrivers.operator.openshift.io", "cluster",
			"--type=merge", "-p", `{"spec":{"pluginName":"wrong.plugin"}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for pluginName to be restored")
		Eventually(func(g Gomega) {
			output, err := utils.KubectlGetJsonpath(
				"spiffecsidrivers.operator.openshift.io",
				"cluster", "", "{.spec.pluginName}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("csi.spiffe.io"))
		}).Should(Succeed(), "SpiffeCSIDriver pluginName was not restored after drift")
	})

	It("should verify controller logs show SPIRE operand activation", func() {
		cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
			"-n", controllerNamespace, "--tail=500")
		logs, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("SPIRE operand controller enabled"),
			"controller should log SPIRE operand activation")
	})
})

// ztwimMockCRDsYAML returns minimal CRD definitions for all 5 SPIRE operand
// types. These use x-kubernetes-preserve-unknown-fields to accept any spec
// without requiring full schema validation — sufficient for Kind-based e2e
// testing where the real ZTWIM operator is absent.
func ztwimMockCRDsYAML() string {
	crds := []struct {
		plural   string
		singular string
		kind     string
		listKind string
	}{
		{"zerotrustworkloadidentitymanagers", "zerotrustworkloadidentitymanager",
			"ZeroTrustWorkloadIdentityManager", "ZeroTrustWorkloadIdentityManagerList"},
		{"spiffecsidrivers", "spiffecsidriver",
			"SpiffeCSIDriver", "SpiffeCSIDriverList"},
		{"spireservers", "spireserver",
			"SpireServer", "SpireServerList"},
		{"spireagents", "spireagent",
			"SpireAgent", "SpireAgentList"},
		{"spireoidcdiscoveryproviders", "spireoidcdiscoveryprovider",
			"SpireOIDCDiscoveryProvider", "SpireOIDCDiscoveryProviderList"},
	}

	var sb strings.Builder
	for i, crd := range crds {
		if i > 0 {
			sb.WriteString("---\n")
		}
		fmt.Fprintf(&sb, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: %s.operator.openshift.io
spec:
  group: operator.openshift.io
  names:
    kind: %s
    listKind: %s
    plural: %s
    singular: %s
  scope: Cluster
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        x-kubernetes-preserve-unknown-fields: true
`, crd.plural, crd.kind, crd.listKind, crd.plural, crd.singular)
	}
	return sb.String()
}
