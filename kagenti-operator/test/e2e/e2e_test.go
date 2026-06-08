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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kagenti/operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "kagenti-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "kagenti-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "kagenti-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "kagenti-operator-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		Expect(utils.DeployController(namespace, projectImage)).To(Succeed(), "Failed to deploy controller")
	})

	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("cleaning up metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		utils.UndeployController()

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=kagenti-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("validating that the ServiceMonitor for Prometheus is applied in the namespace")
			cmd = exec.Command("kubectl", "get", "ServiceMonitor", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "ServiceMonitor should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("waiting for the webhook endpoint to be ready")
			verifyWebhookEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints",
					"kagenti-operator-webhook-service", "-n", namespace,
					"-o", "jsonpath={.subsets[0].addresses[0].ip}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "webhook endpoint not yet populated")
			}
			Eventually(verifyWebhookEndpointReady).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			Eventually(func() error {
				cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
					"--namespace", namespace,
					"--image=curlimages/curl:latest",
					"--overrides",
					fmt.Sprintf(`{
						"spec": {
							"containers": [{
								"name": "curl",
								"image": "curlimages/curl:latest",
								"command": ["/bin/sh", "-c"],
								"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
								"securityContext": {
									"allowPrivilegeEscalation": false,
									"capabilities": {
										"drop": ["ALL"]
									},
									"runAsNonRoot": true,
									"runAsUser": 1000,
									"seccompProfile": {
										"type": "RuntimeDefault"
									}
								}
							}],
							"serviceAccountName": "%s"
						}
					}`, token, metricsServiceName, namespace, serviceAccountName))
				_, runErr := utils.Run(cmd)
				if runErr != nil && strings.Contains(runErr.Error(), "already exists") {
					return nil
				}
				return runErr
			}, 1*time.Minute, 5*time.Second).Should(Succeed(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

var _ = Describe("AuthBridge Injection E2E", Ordered, func() {
	const controllerNamespace = "kagenti-operator-system"

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

		By("waiting for webhook endpoint to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "endpoints",
				"kagenti-operator-webhook-service", "-n", controllerNamespace,
				"-o", "jsonpath={.subsets[0].addresses[0].ip}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "webhook endpoint not yet populated")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("creating auth bridge test namespace")
		cmd := exec.Command("kubectl", "create", "ns", authBridgeTestNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", authBridgeTestNamespace,
			"kagenti-enabled=true",
			"pod-security.kubernetes.io/enforce=privileged",
			"pod-security.kubernetes.io/warn=baseline")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating ClusterSPIFFEID for auth bridge test namespace")
		_, err = utils.KubectlApplyStdin(authBridgeClusterSPIFFEIDFixture(), "")
		Expect(err).NotTo(HaveOccurred())

		By("applying auth bridge ConfigMaps")
		_, err = utils.KubectlApplyStdin(authBridgeConfigMapFixture(), authBridgeTestNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("pre-creating Keycloak client credentials Secret (no real Keycloak in e2e)")
		_, err = utils.KubectlApplyStdin(
			keycloakClientCredentialsSecretFixture(authBridgeTestNamespace, "authbridge-agent"),
			authBridgeTestNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("creating AgentRuntime CR for authbridge-agent (with retry for webhook readiness)")
		Eventually(func() error {
			_, err := utils.KubectlApplyStdin(authBridgeAgentRuntimeFixture(), authBridgeTestNamespace)
			return err
		}, 1*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("deleting auth bridge test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", authBridgeTestNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up ClusterSPIFFEID")
		cmd = exec.Command("kubectl", "delete", "clusterspiffeid", "e2e-authbridge-test", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		utils.UndeployController()
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace, "--tail=100")
			logs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", logs)
			}

			cmd = exec.Command("kubectl", "get", "events", "-n", authBridgeTestNamespace, "--sort-by=.lastTimestamp")
			events, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Events:\n%s\n", events)
			}

			cmd = exec.Command("kubectl", "describe", "pods", "-n", authBridgeTestNamespace)
			desc, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Pod descriptions:\n%s\n", desc)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Sidecar injection", Ordered, func() {
		It("should inject envoy-proxy, proxy-init, and spiffe-helper", func() {
			By("deploying authbridge-agent")
			_, err := utils.KubectlApplyStdin(authBridgeAgentFixture(), authBridgeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			Expect(utils.WaitForDeploymentReady("authbridge-agent", authBridgeTestNamespace, 3*time.Minute)).To(Succeed())

			By("verifying injected sidecar containers")
			Eventually(func(g Gomega) {
				containers, err := utils.KubectlGetJsonpath("pod", "",
					authBridgeTestNamespace,
					"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='authbridge-agent')].spec.containers[*].name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(containers).To(ContainSubstring("envoy-proxy"))
				g.Expect(containers).To(ContainSubstring("spiffe-helper"))
				g.Expect(containers).NotTo(ContainSubstring("kagenti-client-registration"))
			}).Should(Succeed())

			By("verifying injected init containers")
			Eventually(func(g Gomega) {
				initContainers, err := utils.KubectlGetJsonpath("pod", "",
					authBridgeTestNamespace,
					"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='authbridge-agent')].spec.initContainers[*].name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(initContainers).To(ContainSubstring("proxy-init"))
			}).Should(Succeed())

			By("verifying injected volumes")
			Eventually(func(g Gomega) {
				volumes, err := utils.KubectlGetJsonpath("pod", "",
					authBridgeTestNamespace,
					"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='authbridge-agent')].spec.volumes[*].name}")
				g.Expect(err).NotTo(HaveOccurred())
				expectedVolumes := []string{
					"shared-data", "spire-agent-socket", "spiffe-helper-config",
					"svid-output", "envoy-config", "authproxy-routes",
					"authbridge-runtime-config",
				}
				for _, vol := range expectedVolumes {
					g.Expect(volumes).To(ContainSubstring(vol), "expected volume %s", vol)
				}
			}).Should(Succeed())
		})

		It("should not duplicate sidecars on pod recreation (idempotency)", func() {
			By("getting current pod name")
			var oldPodName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=authbridge-agent",
					"-n", authBridgeTestNamespace,
					"-o", "jsonpath={.items[0].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				oldPodName = output
			}).Should(Succeed())

			By("deleting the pod")
			cmd := exec.Command("kubectl", "delete", "pod", oldPodName, "-n", authBridgeTestNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for new pod to be running with a different name")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=authbridge-agent",
					"-n", authBridgeTestNamespace,
					"-o", "jsonpath={.items[0].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				g.Expect(output).NotTo(Equal(oldPodName), "new pod should have a different name")

				phase, err := utils.KubectlGetJsonpath("pod", output, authBridgeTestNamespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Running"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying exactly 1 envoy-proxy, 1 spiffe-helper, 1 proxy-init")
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", "app.kubernetes.io/name=authbridge-agent",
				"-n", authBridgeTestNamespace,
				"-o", "jsonpath={.items[0].spec.containers[*].name}")
			containers, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Count(containers, "envoy-proxy")).To(Equal(1), "expected exactly 1 envoy-proxy")
			Expect(strings.Count(containers, "spiffe-helper")).To(Equal(1), "expected exactly 1 spiffe-helper")

			cmd = exec.Command("kubectl", "get", "pods",
				"-l", "app.kubernetes.io/name=authbridge-agent",
				"-n", authBridgeTestNamespace,
				"-o", "jsonpath={.items[0].spec.initContainers[*].name}")
			initContainers, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Count(initContainers, "proxy-init")).To(Equal(1), "expected exactly 1 proxy-init")
		})
	})

	Context("Injection opt-out", func() {
		It("should not inject when kagenti.io/inject=disabled", func() {
			By("creating AgentRuntime for disabled agent")
			_, err := utils.KubectlApplyStdin(authBridgeDisabledAgentRuntimeFixture(), authBridgeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("verifying AgentRuntime CR exists")
			_, err = utils.KubectlGetJsonpath("agentruntime", "authbridge-disabled-agent",
				authBridgeTestNamespace, "{.metadata.name}")
			Expect(err).NotTo(HaveOccurred(), "AgentRuntime CR must exist before deploying disabled agent")

			By("deploying disabled agent")
			_, err = utils.KubectlApplyStdin(authBridgeDisabledAgentFixture(), authBridgeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			Expect(utils.WaitForDeploymentReady(
				"authbridge-disabled-agent", authBridgeTestNamespace, 2*time.Minute,
			)).To(Succeed())

			By("verifying only pause container, no sidecars")
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "app.kubernetes.io/name=authbridge-disabled-agent",
				"-n", authBridgeTestNamespace,
				"-o", "jsonpath={.items[0].spec.containers[*].name}")
			containers, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(containers).To(Equal("pause"), "expected only pause container")

			By("verifying no init containers")
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", "app.kubernetes.io/name=authbridge-disabled-agent",
				"-n", authBridgeTestNamespace,
				"-o", "jsonpath={.items[0].spec.initContainers[*].name}")
			initContainers, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(initContainers)).To(BeEmpty(), "expected no init containers")
		})
	})

	Context("HTTP validation", func() {
		It("should route HTTP traffic through the injected envoy proxy", func() {
			By("waiting for envoy-proxy container to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/name=authbridge-agent",
					"-n", authBridgeTestNamespace,
					"-o", "jsonpath={.items[0].status.containerStatuses[?(@.name=='envoy-proxy')].ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"), "envoy-proxy container not ready")
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("getting pod name")
			var podName string
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "app.kubernetes.io/name=authbridge-agent",
				"-n", authBridgeTestNamespace,
				"-o", "jsonpath={.items[0].metadata.name}")
			podName, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			podName = strings.TrimSpace(podName)

			By("collecting diagnostic logs before envoy admin test")
			diagCmd := exec.Command("kubectl", "logs", podName,
				"-n", authBridgeTestNamespace,
				"-c", "envoy-proxy", "--tail=30")
			if envoyLogs, diagErr := utils.Run(diagCmd); diagErr == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "=== envoy-proxy logs (last 30) ===\n%s\n", envoyLogs)
			}
			diagCmd = exec.Command("kubectl", "get", "configmap",
				authBridgeAgentCMName,
				"-n", authBridgeTestNamespace,
				"-o", "jsonpath={.data.config\\.yaml}")
			if cmData, diagErr := utils.Run(diagCmd); diagErr == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "=== per-agent ConfigMap ===\n%s\n", cmData)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "=== per-agent ConfigMap not found: %v ===\n", diagErr)
			}
			statusJSONPath := "{range .status.containerStatuses[*]}" +
				"{.name}: ready={.ready} restarts={.restartCount} state={.state}{\"\\n\"}{end}"
			diagCmd = exec.Command("kubectl", "get", "pod", podName,
				"-n", authBridgeTestNamespace,
				"-o", "jsonpath="+statusJSONPath)
			if statuses, diagErr := utils.Run(diagCmd); diagErr == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "=== container statuses ===\n%s\n", statuses)
			}

			// Verify envoy admin is responding. The request originates from the echo
			// container (non-proxy UID) so proxy-init iptables redirect it through
			// envoy's outbound listener, which forwards to the original destination
			// 127.0.0.1:9901 (envoy admin). The response is genuinely from envoy admin.
			By("hitting envoy admin interface from the echo container")
			cmd = exec.Command("kubectl", "exec", podName,
				"-n", authBridgeTestNamespace,
				"-c", "echo",
				"--", "python3", "-c",
				"import urllib.request; r = urllib.request.urlopen('http://127.0.0.1:9901/server_info'); print(r.status)")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("200"), "envoy admin should return 200")

			By("running a curl pod to hit authbridge-agent service")
			curlPodName := "curl-authbridge-test"
			cmd = exec.Command("kubectl", "run", curlPodName,
				"--restart=Never",
				"--namespace", authBridgeTestNamespace,
				"--image=curlimages/curl:latest",
				"--command", "--",
				"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
				fmt.Sprintf("http://authbridge-agent.%s.svc:8080/", authBridgeTestNamespace))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for curl pod to complete")
			Eventually(func(g Gomega) {
				phase, err := utils.KubectlGetJsonpath("pod", curlPodName, authBridgeTestNamespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Succeeded"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying HTTP 200 response")
			cmd = exec.Command("kubectl", "logs", curlPodName, "-n", authBridgeTestNamespace)
			curlOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(curlOutput)).To(Equal("200"), "expected HTTP 200 through envoy proxy")

			By("cleaning up curl pod")
			cmd = exec.Command("kubectl", "delete", "pod", curlPodName, "-n", authBridgeTestNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})
})

var _ = Describe("AgentCard E2E", Ordered, func() {
	const controllerNamespace = "kagenti-operator-system"
	const controllerDeployment = "kagenti-operator-controller-manager"

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

		By("waiting for webhook endpoint to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "endpoints",
				"kagenti-operator-webhook-service", "-n", controllerNamespace,
				"-o", "jsonpath={.subsets[0].addresses[0].ip}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "webhook endpoint not yet populated")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("creating test namespace with labels")
		cmd := exec.Command("kubectl", "create", "ns", testNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", testNamespace,
			"agentcard=true",
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("deleting test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up ClusterSPIFFEID")
		cmd = exec.Command("kubectl", "delete", "clusterspiffeid", "e2e-agentcard-test", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		utils.UndeployController()
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			// Dump controller logs
			cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace, "--tail=100")
			logs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", logs)
			}

			// Dump events in test namespace
			cmd = exec.Command("kubectl", "get", "events", "-n", testNamespace, "--sort-by=.lastTimestamp")
			events, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Events:\n%s\n", events)
			}

			// Dump AgentCards
			cmd = exec.Command("kubectl", "get", "agentcards", "-n", testNamespace, "-o", "yaml")
			cards, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "AgentCards:\n%s\n", cards)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Without signature verification", Ordered, func() {
		It("should reject AgentCard without targetRef", func() {
			By("attempting to apply AgentCard without targetRef")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "apply", "-f", "-", "-n", testNamespace)
				cmd.Stdin = strings.NewReader(invalidAgentCardFixture())
				output, err := cmd.CombinedOutput()
				g.Expect(err).To(HaveOccurred(), "kubectl apply should fail")
				g.Expect(string(output)).To(ContainSubstring("spec.targetRef is required"))
			}, 1*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should not create AgentCard for workload without protocol label", func() {
			By("deploying noproto-agent without protocol label")
			_, err := utils.KubectlApplyStdin(noProtocolAgentFixture(), testNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			Expect(utils.WaitForDeploymentReady("noproto-agent", testNamespace, 2*time.Minute)).To(Succeed())

			By("verifying no AgentCard is created")
			Consistently(func() string {
				cmd := exec.Command("kubectl", "get", "agentcards", "-n", testNamespace,
					"-o", "jsonpath={.items[*].metadata.name}")
				output, _ := utils.Run(cmd)
				return output
			}, 30*time.Second, 5*time.Second).ShouldNot(ContainSubstring("noproto-agent"))
		})

		It("should auto-create AgentCard for labelled workload", func() {
			By("deploying echo-agent with agent and protocol labels")
			_, err := utils.KubectlApplyStdin(echoAgentFixture(), testNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			Expect(utils.WaitForDeploymentReady("echo-agent", testNamespace, 2*time.Minute)).To(Succeed())

			cardName := "echo-agent-deployment-card"

			By("verifying AgentCard is auto-created")
			Eventually(func(g Gomega) {
				managedBy, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(managedBy).To(Equal("kagenti-operator"))
			}).Should(Succeed())

			By("verifying targetRef")
			Eventually(func(g Gomega) {
				apiVersion, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.spec.targetRef.apiVersion}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(apiVersion).To(Equal("apps/v1"))

				kind, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.spec.targetRef.kind}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(kind).To(Equal("Deployment"))

				name, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.spec.targetRef.name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(name).To(Equal("echo-agent"))
			}).Should(Succeed())

			By("verifying protocol and Synced condition")
			Eventually(func(g Gomega) {
				protocol, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.status.protocol}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(protocol).To(Equal("a2a"))

				syncedStatus, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.status.conditions[?(@.type=='Synced')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(syncedStatus).To(Equal("True"))
			}).Should(Succeed())
		})

		It("should reject duplicate AgentCard targeting same workload", func() {
			By("attempting to create manual AgentCard targeting echo-agent")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "apply", "-f", "-", "-n", testNamespace)
				cmd.Stdin = strings.NewReader(manualAgentCardFixture())
				output, err := cmd.CombinedOutput()
				g.Expect(err).To(HaveOccurred(), "kubectl apply should fail for duplicate")
				g.Expect(string(output)).To(ContainSubstring("an AgentCard already targets"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	Context("With signature verification", Ordered, func() {
		var origArgs []string

		BeforeAll(func() {
			By("patching controller with signature verification flags")
			var err error
			origArgs, err = utils.PatchControllerArgs(controllerNamespace, controllerDeployment, []string{
				"--require-a2a-signature=true",
				"--spire-trust-domain=example.org",
				"--spire-trust-bundle-configmap=spire-bundle",
				"--spire-trust-bundle-configmap-namespace=spire-system",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("restoring original controller args")
			if origArgs != nil {
				err := utils.RestoreControllerArgs(controllerNamespace, controllerDeployment, origArgs)
				Expect(err).NotTo(HaveOccurred())

				By("verifying controller args were restored")
				currentArgs, readErr := utils.KubectlGetJsonpath("deployment", controllerDeployment,
					controllerNamespace, "{.spec.template.spec.containers[0].args}")
				Expect(readErr).NotTo(HaveOccurred())
				for _, arg := range []string{"--require-a2a-signature", "--spire-trust-domain"} {
					Expect(currentArgs).NotTo(ContainSubstring(arg),
						"controller args not fully restored: still contains "+arg)
				}
			}
		})

		Context("Audit mode", Ordered, func() {
			var auditOrigArgs []string

			BeforeAll(func() {
				By("adding audit mode flag")
				var err error
				auditOrigArgs, err = utils.PatchControllerArgs(controllerNamespace, controllerDeployment, []string{
					"--signature-audit-mode=true",
				})
				Expect(err).NotTo(HaveOccurred())
			})

			AfterAll(func() {
				By("removing audit mode flag")
				if auditOrigArgs != nil {
					err := utils.RestoreControllerArgs(controllerNamespace, controllerDeployment, auditOrigArgs)
					Expect(err).NotTo(HaveOccurred())

					By("verifying audit mode flag was removed")
					currentArgs, readErr := utils.KubectlGetJsonpath("deployment", controllerDeployment,
						controllerNamespace, "{.spec.template.spec.containers[0].args}")
					Expect(readErr).NotTo(HaveOccurred())
					Expect(currentArgs).NotTo(ContainSubstring("--signature-audit-mode"),
						"controller args not restored: still contains --signature-audit-mode")
				}
			})

			It("should allow sync but report SignatureInvalidAudit", func() {
				By("deploying audit-agent (unsigned)")
				_, err := utils.KubectlApplyStdin(auditAgentFixture(), testNamespace)
				Expect(err).NotTo(HaveOccurred())
				Expect(utils.WaitForDeploymentReady("audit-agent", testNamespace, 2*time.Minute)).To(Succeed())

				By("updating auto-created AgentCard for audit-agent")
				Eventually(func(g Gomega) {
					_, applyErr := utils.KubectlApplyStdin(auditModeAgentCardFixture(), testNamespace)
					g.Expect(applyErr).NotTo(HaveOccurred())
				}, 30*time.Second, 2*time.Second).Should(Succeed())

				cardName := "audit-agent-deployment-card"

				By("verifying Synced=True (audit mode allows sync)")
				Eventually(func(g Gomega) {
					syncedStatus, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
						"{.status.conditions[?(@.type=='Synced')].status}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(syncedStatus).To(Equal("True"))
				}).Should(Succeed())

				By("verifying SignatureVerified=False with reason SignatureInvalidAudit")
				Eventually(func(g Gomega) {
					sigStatus, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
						"{.status.conditions[?(@.type=='SignatureVerified')].status}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(sigStatus).To(Equal("False"))

					sigReason, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
						"{.status.conditions[?(@.type=='SignatureVerified')].reason}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(sigReason).To(Equal("SignatureInvalidAudit"))
				}).Should(Succeed())
			})
		})

		It("should verify signed agent card", func() {
			By("creating ClusterSPIFFEID")
			_, err := utils.KubectlApplyStdin(clusterSPIFFEIDFixture(), "")
			Expect(err).NotTo(HaveOccurred())

			By("deploying signed-agent stack")
			_, err = utils.KubectlApplyStdin(signedAgentFixture(), testNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(utils.WaitForDeploymentReady("signed-agent", testNamespace, 3*time.Minute)).To(Succeed())

			By("updating auto-created AgentCard with identityBinding")
			Eventually(func(g Gomega) {
				_, applyErr := utils.KubectlApplyStdin(signedAgentCardFixture(), testNamespace)
				g.Expect(applyErr).NotTo(HaveOccurred())
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			cardName := "signed-agent-deployment-card"

			By("verifying SignatureVerified=True")
			Eventually(func(g Gomega) {
				sigStatus, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.status.conditions[?(@.type=='SignatureVerified')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sigStatus).To(Equal("True"))

				sigReason, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.status.conditions[?(@.type=='SignatureVerified')].reason}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sigReason).To(Equal("SignatureValid"))
			}, 3*time.Minute).Should(Succeed())

			By("verifying signatureSpiffeId")
			Eventually(func(g Gomega) {
				spiffeId, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.status.signatureSpiffeId}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(spiffeId).To(Equal("spiffe://example.org/ns/e2e-agentcard-test/sa/signed-agent-sa"))
			}).Should(Succeed())

			By("verifying Synced=True")
			Eventually(func(g Gomega) {
				syncedStatus, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.status.conditions[?(@.type=='Synced')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(syncedStatus).To(Equal("True"))
			}).Should(Succeed())

			By("verifying Bound=True")
			Eventually(func(g Gomega) {
				boundStatus, err := utils.KubectlGetJsonpath("agentcard", cardName, testNamespace,
					"{.status.conditions[?(@.type=='Bound')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(boundStatus).To(Equal("True"))
			}).Should(Succeed())
		})
	})
})

var _ = Describe("AgentRuntime E2E", Ordered, func() {
	const controllerNamespace = "kagenti-operator-system"

	BeforeAll(func() {
		By("ensuring mlflow-operator ClusterRole exists for ServiceAccount informer")
		clusterRoleCmd := exec.Command("kubectl", "apply", "-f", "-")
		clusterRoleCmd.Stdin = strings.NewReader(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: mlflow-operator-mlflow-integration
rules:
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["list", "watch"]
`)
		_, _ = utils.Run(clusterRoleCmd)

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

		By("waiting for webhook endpoint to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "endpoints",
				"kagenti-operator-webhook-service", "-n", controllerNamespace,
				"-o", "jsonpath={.subsets[0].addresses[0].ip}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "webhook endpoint not yet populated")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("creating test namespace")
		cmd := exec.Command("kubectl", "create", "ns", agentRuntimeTestNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", agentRuntimeTestNamespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("ensuring kagenti-system namespace exists")
		cmd = exec.Command("kubectl", "create", "ns", "kagenti-system")
		_, _ = utils.Run(cmd) // ignore error; namespace may already exist

		By("creating cluster defaults ConfigMap")
		_, err = utils.KubectlApplyStdin(runtimeClusterDefaultsConfigMapFixture(), "kagenti-system")
		Expect(err).NotTo(HaveOccurred())

		By("creating namespace defaults ConfigMap")
		_, err = utils.KubectlApplyStdin(runtimeNamespaceDefaultsConfigMapFixture(), agentRuntimeTestNamespace)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("deleting test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", agentRuntimeTestNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up cluster defaults ConfigMap")
		cmd = exec.Command("kubectl", "delete", "configmap", "kagenti-platform-config",
			"-n", "kagenti-system", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up mlflow-operator ClusterRole")
		cmd = exec.Command("kubectl", "delete", "clusterrole",
			"mlflow-operator-mlflow-integration", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		utils.UndeployController()
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			// Dump controller logs
			cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace, "--tail=100")
			logs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", logs)
			}

			// Dump events in test namespace
			cmd = exec.Command("kubectl", "get", "events", "-n", agentRuntimeTestNamespace, "--sort-by=.lastTimestamp")
			events, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Events:\n%s\n", events)
			}

			// Dump AgentRuntimes
			cmd = exec.Command("kubectl", "get", "agentruntimes", "-n", agentRuntimeTestNamespace, "-o", "yaml")
			runtimes, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "AgentRuntimes:\n%s\n", runtimes)
			}

			// Dump Deployments
			cmd = exec.Command("kubectl", "get", "deployments", "-n", agentRuntimeTestNamespace, "-o", "yaml")
			deploys, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Deployments:\n%s\n", deploys)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Agent lifecycle", Ordered, func() {
		It("should apply labels and config-hash to target Deployment", func() {
			By("deploying the agent target workload")
			_, err := utils.KubectlApplyStdin(runtimeTargetDeploymentFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(utils.WaitForDeploymentReady("runtime-agent-target", agentRuntimeTestNamespace, 2*time.Minute)).To(Succeed())

			By("creating AgentRuntime CR")
			_, err = utils.KubectlApplyStdin(runtimeAgentCRFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("verifying kagenti.io/type=agent on workload metadata")
			Eventually(func(g Gomega) {
				typeLabel, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace, "{.metadata.labels['kagenti\\.io/type']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(typeLabel).To(Equal("agent"))
			}).Should(Succeed())

			By("verifying app.kubernetes.io/managed-by on workload metadata")
			Eventually(func(g Gomega) {
				managedBy, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace, "{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(managedBy).To(Equal("kagenti-operator"))
			}).Should(Succeed())

			By("verifying kagenti.io/type=agent on pod template")
			Eventually(func(g Gomega) {
				podTypeLabel, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace,
					"{.spec.template.metadata.labels['kagenti\\.io/type']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(podTypeLabel).To(Equal("agent"))
			}).Should(Succeed())

			By("verifying config-hash annotation on pod template")
			Eventually(func(g Gomega) {
				hash, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace,
					"{.spec.template.metadata.annotations['kagenti\\.io/config-hash']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hash).NotTo(BeEmpty())
				g.Expect(hash).To(HaveLen(64))
			}).Should(Succeed())

			By("verifying AgentCard is auto-created by AgentCardSync")
			cardName := "runtime-agent-target-deployment-card"
			Eventually(func(g Gomega) {
				managedBy, err := utils.KubectlGetJsonpath("agentcard", cardName,
					agentRuntimeTestNamespace,
					"{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(managedBy).To(Equal("kagenti-operator"))
			}).Should(Succeed())

			By("verifying AgentCard targetRef points to the runtime target")
			Eventually(func(g Gomega) {
				kind, err := utils.KubectlGetJsonpath("agentcard", cardName,
					agentRuntimeTestNamespace, "{.spec.targetRef.kind}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(kind).To(Equal("Deployment"))

				name, err := utils.KubectlGetJsonpath("agentcard", cardName,
					agentRuntimeTestNamespace, "{.spec.targetRef.name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(name).To(Equal("runtime-agent-target"))
			}).Should(Succeed())
		})

		It("should set Phase=Active and Ready=True", func() {
			By("verifying phase is Active")
			Eventually(func(g Gomega) {
				phase, err := utils.KubectlGetJsonpath("agentruntime", "test-agent-runtime",
					agentRuntimeTestNamespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Active"))
			}).Should(Succeed())

			By("verifying Ready condition is True")
			Eventually(func(g Gomega) {
				readyStatus, err := utils.KubectlGetJsonpath("agentruntime", "test-agent-runtime",
					agentRuntimeTestNamespace,
					"{.status.conditions[?(@.type=='Ready')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(readyStatus).To(Equal("True"))
			}).Should(Succeed())
		})

		It("should not change Deployment generation on re-reconcile", func() {
			By("recording current deployment generation")
			gen, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
				agentRuntimeTestNamespace, "{.metadata.generation}")
			Expect(err).NotTo(HaveOccurred())
			Expect(gen).NotTo(BeEmpty())

			By("verifying generation stays stable for 30s")
			Consistently(func(g Gomega) {
				currentGen, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace, "{.metadata.generation}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(currentGen).To(Equal(gen))
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should clean up on deletion", func() {
			By("deleting the AgentRuntime CR")
			cmd := exec.Command("kubectl", "delete", "agentruntime", "test-agent-runtime",
				"-n", agentRuntimeTestNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying AgentRuntime CR is gone")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "agentruntime", "test-agent-runtime",
					"-n", agentRuntimeTestNamespace)
				_, err := cmd.CombinedOutput()
				g.Expect(err).To(HaveOccurred(), "AgentRuntime should be deleted")
			}).Should(Succeed())

			By("verifying target deployment still exists")
			cmd = exec.Command("kubectl", "get", "deployment", "runtime-agent-target",
				"-n", agentRuntimeTestNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying kagenti.io/type label is removed from workload metadata")
			Eventually(func(g Gomega) {
				typeLabel, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace, "{.metadata.labels['kagenti\\.io/type']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(typeLabel).To(BeEmpty())
			}).Should(Succeed())

			By("verifying kagenti.io/type label is removed from PodTemplateSpec")
			Eventually(func(g Gomega) {
				typeLabel, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace, "{.spec.template.metadata.labels['kagenti\\.io/type']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(typeLabel).To(BeEmpty())
			}).Should(Succeed())

			By("verifying managed-by label is removed")
			Eventually(func(g Gomega) {
				managedBy, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace, "{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(managedBy).To(BeEmpty())
			}).Should(Succeed())

			By("verifying config-hash annotation is removed from PodTemplateSpec")
			Eventually(func(g Gomega) {
				hash, err := utils.KubectlGetJsonpath("deployment", "runtime-agent-target",
					agentRuntimeTestNamespace,
					"{.spec.template.metadata.annotations['kagenti\\.io/config-hash']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hash).To(BeEmpty())
			}).Should(Succeed())
		})
	})

	Context("Error cases", func() {
		It("should set Phase=Error for missing target", func() {
			By("creating AgentRuntime targeting non-existent deployment")
			_, err := utils.KubectlApplyStdin(runtimeMissingTargetCRFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("verifying phase is Error")
			Eventually(func(g Gomega) {
				phase, err := utils.KubectlGetJsonpath("agentruntime", "test-missing-target",
					agentRuntimeTestNamespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Error"))
			}).Should(Succeed())

			By("verifying TargetResolved condition mentions the target")
			Eventually(func(g Gomega) {
				msg, err := utils.KubectlGetJsonpath("agentruntime", "test-missing-target",
					agentRuntimeTestNamespace,
					"{.status.conditions[?(@.type=='TargetResolved')].message}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(msg).To(ContainSubstring("nonexistent-deployment"))
			}).Should(Succeed())

			By("cleaning up")
			cmd := exec.Command("kubectl", "delete", "agentruntime", "test-missing-target",
				"-n", agentRuntimeTestNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	Context("Tool type", func() {
		It("should apply kagenti.io/type=tool label", func() {
			By("deploying the tool target workload")
			_, err := utils.KubectlApplyStdin(runtimeToolTargetDeploymentFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(utils.WaitForDeploymentReady("runtime-tool-target", agentRuntimeTestNamespace, 2*time.Minute)).To(Succeed())

			By("creating tool AgentRuntime CR")
			_, err = utils.KubectlApplyStdin(runtimeToolCRFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("verifying kagenti.io/type=tool on workload metadata")
			Eventually(func(g Gomega) {
				typeLabel, err := utils.KubectlGetJsonpath("deployment", "runtime-tool-target",
					agentRuntimeTestNamespace, "{.metadata.labels['kagenti\\.io/type']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(typeLabel).To(Equal("tool"))
			}).Should(Succeed())

			By("verifying no AgentCard is created for tool-type workload")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "agentcards", "-n", agentRuntimeTestNamespace,
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(ContainSubstring("runtime-tool-target"))
			}, 15*time.Second, 3*time.Second).Should(Succeed())

			By("cleaning up")
			cmd := exec.Command("kubectl", "delete", "agentruntime", "test-tool-runtime",
				"-n", agentRuntimeTestNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	Context("StatefulSet target", func() {
		It("should apply labels and config-hash to a StatefulSet workload", func() {
			By("deploying the StatefulSet target workload")
			_, err := utils.KubectlApplyStdin(runtimeStatefulSetTargetFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for StatefulSet to be ready")
			Eventually(func(g Gomega) {
				ready, err := utils.KubectlGetJsonpath("statefulset", "runtime-sts-target",
					agentRuntimeTestNamespace, "{.status.readyReplicas}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(Equal("1"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("creating AgentRuntime CR targeting StatefulSet")
			_, err = utils.KubectlApplyStdin(runtimeStatefulSetCRFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("verifying kagenti.io/type=agent on StatefulSet metadata")
			Eventually(func(g Gomega) {
				typeLabel, err := utils.KubectlGetJsonpath("statefulset", "runtime-sts-target",
					agentRuntimeTestNamespace, "{.metadata.labels['kagenti\\.io/type']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(typeLabel).To(Equal("agent"))
			}).Should(Succeed())

			By("verifying app.kubernetes.io/managed-by on StatefulSet metadata")
			Eventually(func(g Gomega) {
				managedBy, err := utils.KubectlGetJsonpath("statefulset", "runtime-sts-target",
					agentRuntimeTestNamespace, "{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(managedBy).To(Equal("kagenti-operator"))
			}).Should(Succeed())

			By("verifying Phase=Active")
			Eventually(func(g Gomega) {
				phase, err := utils.KubectlGetJsonpath("agentruntime", "test-sts-runtime",
					agentRuntimeTestNamespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Active"))
			}).Should(Succeed())

			By("verifying config-hash on pod template")
			Eventually(func(g Gomega) {
				hash, err := utils.KubectlGetJsonpath("statefulset", "runtime-sts-target",
					agentRuntimeTestNamespace,
					"{.spec.template.metadata.annotations['kagenti\\.io/config-hash']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hash).To(HaveLen(64))
			}).Should(Succeed())

			By("cleaning up")
			cmd := exec.Command("kubectl", "delete", "agentruntime", "test-sts-runtime",
				"-n", agentRuntimeTestNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	Context("Identity and trace overrides", func() {
		It("should produce a different config-hash than a minimal CR", func() {
			By("deploying two target workloads")
			_, err := utils.KubectlApplyStdin(runtimeMinimalTargetDeploymentFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.KubectlApplyStdin(runtimeOverridesTargetDeploymentFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			Expect(utils.WaitForDeploymentReady(
				"runtime-minimal-target", agentRuntimeTestNamespace, 2*time.Minute)).To(Succeed())
			Expect(utils.WaitForDeploymentReady(
				"runtime-overrides-target", agentRuntimeTestNamespace, 2*time.Minute)).To(Succeed())

			By("creating minimal AgentRuntime CR (no overrides)")
			_, err = utils.KubectlApplyStdin(runtimeMinimalCRFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("creating AgentRuntime CR with identity and trace overrides")
			_, err = utils.KubectlApplyStdin(runtimeOverridesCRFixture(), agentRuntimeTestNamespace)
			Expect(err).NotTo(HaveOccurred())

			var minimalHash, overridesHash string

			By("waiting for minimal CR to reach Active")
			Eventually(func(g Gomega) {
				phase, err := utils.KubectlGetJsonpath("agentruntime", "test-minimal-runtime",
					agentRuntimeTestNamespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Active"))
			}).Should(Succeed())

			By("recording minimal config-hash")
			Eventually(func(g Gomega) {
				hash, err := utils.KubectlGetJsonpath("deployment", "runtime-minimal-target",
					agentRuntimeTestNamespace,
					"{.spec.template.metadata.annotations['kagenti\\.io/config-hash']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hash).To(HaveLen(64))
				minimalHash = hash
			}).Should(Succeed())

			By("waiting for overrides CR to reach Active")
			Eventually(func(g Gomega) {
				phase, err := utils.KubectlGetJsonpath("agentruntime", "test-overrides-runtime",
					agentRuntimeTestNamespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Active"))
			}).Should(Succeed())

			By("recording overrides config-hash")
			Eventually(func(g Gomega) {
				hash, err := utils.KubectlGetJsonpath("deployment", "runtime-overrides-target",
					agentRuntimeTestNamespace,
					"{.spec.template.metadata.annotations['kagenti\\.io/config-hash']}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hash).To(HaveLen(64))
				overridesHash = hash
			}).Should(Succeed())

			By("verifying config-hashes differ")
			Expect(overridesHash).NotTo(Equal(minimalHash),
				"identity/trace overrides should produce a different config-hash")

			By("cleaning up")
			cmd := exec.Command("kubectl", "delete", "agentruntime", "test-minimal-runtime", "test-overrides-runtime",
				"-n", agentRuntimeTestNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})
})

var _ = Describe("Combined AgentRuntime + AgentCard + Auth Bridge E2E", Ordered, func() {
	const controllerNamespace = "kagenti-operator-system"
	const controllerDeployment = "kagenti-operator-controller-manager"

	var origArgs []string

	BeforeAll(func() {
		By("ensuring mlflow-operator ClusterRole exists for ServiceAccount informer")
		clusterRoleCmd := exec.Command("kubectl", "apply", "-f", "-")
		clusterRoleCmd.Stdin = strings.NewReader(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: mlflow-operator-mlflow-integration
rules:
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["list", "watch"]
`)
		_, _ = utils.Run(clusterRoleCmd)

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

		By("waiting for webhook endpoint to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "endpoints",
				"kagenti-operator-webhook-service", "-n", controllerNamespace,
				"-o", "jsonpath={.subsets[0].addresses[0].ip}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "webhook endpoint not yet populated")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("patching controller with --spire-trust-domain=example.org")
		var err error
		origArgs, err = utils.PatchControllerArgs(controllerNamespace, controllerDeployment, []string{
			"--spire-trust-domain=example.org",
		})
		Expect(err).NotTo(HaveOccurred())

		By("creating combined test namespace")
		cmd := exec.Command("kubectl", "create", "ns", combinedTestNamespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", combinedTestNamespace,
			"kagenti-enabled=true",
			"pod-security.kubernetes.io/enforce=privileged",
			"pod-security.kubernetes.io/warn=baseline")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating ClusterSPIFFEID for combined test namespace")
		_, err = utils.KubectlApplyStdin(combinedClusterSPIFFEIDFixture(), "")
		Expect(err).NotTo(HaveOccurred())

		By("applying auth bridge ConfigMaps")
		_, err = utils.KubectlApplyStdin(combinedConfigMapFixture(), combinedTestNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("pre-creating Keycloak client credentials Secret (no real Keycloak in e2e)")
		_, err = utils.KubectlApplyStdin(
			keycloakClientCredentialsSecretFixture(combinedTestNamespace, "combined-agent"),
			combinedTestNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("ensuring kagenti-system namespace exists")
		cmd = exec.Command("kubectl", "create", "ns", "kagenti-system")
		_, _ = utils.Run(cmd)

		By("creating cluster defaults ConfigMap")
		_, err = utils.KubectlApplyStdin(runtimeClusterDefaultsConfigMapFixture(), "kagenti-system")
		Expect(err).NotTo(HaveOccurred())

		By("deploying combined agent")
		_, err = utils.KubectlApplyStdin(combinedAgentFixture(), combinedTestNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for deployment to be ready")
		Expect(utils.WaitForDeploymentReady("combined-agent", combinedTestNamespace, 5*time.Minute)).To(Succeed())

		By("creating AgentRuntime CR (with retry for webhook readiness)")
		Eventually(func() error {
			_, err := utils.KubectlApplyStdin(combinedAgentRuntimeFixture(), combinedTestNamespace)
			return err
		}, 1*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("restoring original controller args")
		if origArgs != nil {
			err := utils.RestoreControllerArgs(controllerNamespace, controllerDeployment, origArgs)
			Expect(err).NotTo(HaveOccurred())
		}

		By("deleting combined test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", combinedTestNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up ClusterSPIFFEID")
		cmd = exec.Command("kubectl", "delete", "clusterspiffeid", "e2e-combined-test", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up cluster defaults ConfigMap")
		cmd = exec.Command("kubectl", "delete", "configmap", "kagenti-platform-config",
			"-n", "kagenti-system", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up mlflow-operator ClusterRole")
		cmd = exec.Command("kubectl", "delete", "clusterrole",
			"mlflow-operator-mlflow-integration", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		utils.UndeployController()
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
				"-n", controllerNamespace, "--tail=100")
			logs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", logs)
			}

			cmd = exec.Command("kubectl", "get", "events", "-n", combinedTestNamespace, "--sort-by=.lastTimestamp")
			events, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Events:\n%s\n", events)
			}

			cmd = exec.Command("kubectl", "describe", "pods", "-n", combinedTestNamespace)
			desc, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Pod descriptions:\n%s\n", desc)
			}

			cmd = exec.Command("kubectl", "get", "agentcards", "-n", combinedTestNamespace, "-o", "yaml")
			cards, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "AgentCards:\n%s\n", cards)
			}

			cmd = exec.Command("kubectl", "get", "agentruntimes", "-n", combinedTestNamespace, "-o", "yaml")
			runtimes, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "AgentRuntimes:\n%s\n", runtimes)
			}

			cmd = exec.Command("kubectl", "get", "deployments", "-n", combinedTestNamespace, "-o", "yaml")
			deploys, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Deployments:\n%s\n", deploys)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should apply labels to workload when AgentRuntime is created", func() {
		By("waiting for AgentRuntime phase=Active")
		Eventually(func(g Gomega) {
			phase, err := utils.KubectlGetJsonpath("agentruntime", "combined-agent",
				combinedTestNamespace, "{.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Active"))
		}).Should(Succeed())

		By("verifying Ready condition is True")
		Eventually(func(g Gomega) {
			readyStatus, err := utils.KubectlGetJsonpath("agentruntime", "combined-agent",
				combinedTestNamespace,
				"{.status.conditions[?(@.type=='Ready')].status}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyStatus).To(Equal("True"))
		}).Should(Succeed())

		By("verifying kagenti.io/type=agent on deployment metadata")
		Eventually(func(g Gomega) {
			typeLabel, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace, "{.metadata.labels['kagenti\\.io/type']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(typeLabel).To(Equal("agent"))
		}).Should(Succeed())

		By("verifying app.kubernetes.io/managed-by on deployment metadata")
		Eventually(func(g Gomega) {
			managedBy, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace, "{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(managedBy).To(Equal("kagenti-operator"))
		}).Should(Succeed())

		By("verifying kagenti.io/type=agent on pod template")
		Eventually(func(g Gomega) {
			podTypeLabel, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace,
				"{.spec.template.metadata.labels['kagenti\\.io/type']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podTypeLabel).To(Equal("agent"))
		}).Should(Succeed())

		By("verifying protocol.kagenti.io/a2a label preserved")
		Eventually(func(g Gomega) {
			labelsJSON, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace, "{.metadata.labels}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(labelsJSON).To(ContainSubstring("protocol.kagenti.io/a2a"))
		}).Should(Succeed())

		By("verifying config-hash annotation is 64-char hex")
		Eventually(func(g Gomega) {
			hash, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace,
				"{.spec.template.metadata.annotations['kagenti\\.io/config-hash']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(hash).To(HaveLen(64))
		}).Should(Succeed())
	})

	It("should auto-create AgentCard with Synced=True", func() {
		cardName := "combined-agent-deployment-card"

		By("waiting for AgentCard to exist with managed-by label")
		Eventually(func(g Gomega) {
			managedBy, err := utils.KubectlGetJsonpath("agentcard", cardName,
				combinedTestNamespace,
				"{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(managedBy).To(Equal("kagenti-operator"))
		}).Should(Succeed())

		By("verifying targetRef")
		Eventually(func(g Gomega) {
			apiVersion, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.spec.targetRef.apiVersion}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(apiVersion).To(Equal("apps/v1"))

			kind, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.spec.targetRef.kind}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(kind).To(Equal("Deployment"))

			name, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.spec.targetRef.name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(name).To(Equal("combined-agent"))
		}).Should(Succeed())

		By("verifying protocol=a2a and Synced=True")
		Eventually(func(g Gomega) {
			protocol, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.status.protocol}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(protocol).To(Equal("a2a"))

			syncedStatus, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.status.conditions[?(@.type=='Synced')].status}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(syncedStatus).To(Equal("True"))
		}).Should(Succeed())

		By("verifying identityBinding is non-empty")
		Eventually(func(g Gomega) {
			ib, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.spec.identityBinding}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ib).NotTo(BeEmpty())
		}).Should(Succeed())
	})

	It("should inject Auth Bridge sidecars into workload pods", func() {
		By("verifying injected sidecar containers")
		Eventually(func(g Gomega) {
			containers, err := utils.KubectlGetJsonpath("pod", "",
				combinedTestNamespace,
				"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='combined-agent')].spec.containers[*].name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(containers).To(ContainSubstring("envoy-proxy"))
			g.Expect(containers).To(ContainSubstring("spiffe-helper"))
		}).Should(Succeed())

		By("verifying injected init containers")
		Eventually(func(g Gomega) {
			initContainers, err := utils.KubectlGetJsonpath("pod", "",
				combinedTestNamespace,
				"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='combined-agent')].spec.initContainers[*].name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(initContainers).To(ContainSubstring("proxy-init"))
		}).Should(Succeed())

		By("verifying injected volumes")
		Eventually(func(g Gomega) {
			volumes, err := utils.KubectlGetJsonpath("pod", "",
				combinedTestNamespace,
				"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='combined-agent')].spec.volumes[*].name}")
			g.Expect(err).NotTo(HaveOccurred())
			expectedVolumes := []string{
				"shared-data", "spire-agent-socket", "spiffe-helper-config",
				"svid-output", "envoy-config", "authproxy-routes",
				"authbridge-runtime-config",
			}
			for _, vol := range expectedVolumes {
				g.Expect(volumes).To(ContainSubstring(vol), "expected volume %s", vol)
			}
		}).Should(Succeed())
	})

	It("should reflect identity binding on AgentCard", func() {
		cardName := "combined-agent-deployment-card"

		By("verifying identityBinding is non-nil")
		Eventually(func(g Gomega) {
			ib, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.spec.identityBinding}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ib).NotTo(BeEmpty())
		}).Should(Succeed())

		By("verifying card name is Combined Agent")
		Eventually(func(g Gomega) {
			cardNameField, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.status.card.name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cardNameField).To(Equal("Combined Agent"))
		}, 3*time.Minute).Should(Succeed())

		By("verifying card URL contains combined-agent")
		Eventually(func(g Gomega) {
			cardURL, err := utils.KubectlGetJsonpath("agentcard", cardName, combinedTestNamespace,
				"{.status.card.url}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cardURL).To(ContainSubstring("combined-agent"))
		}).Should(Succeed())
	})

	It("should clean up on AgentRuntime deletion and stop injection", func() {
		By("deleting the AgentRuntime CR")
		cmd := exec.Command("kubectl", "delete", "agentruntime", "combined-agent",
			"-n", combinedTestNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("verifying AgentRuntime CR is gone")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "agentruntime", "combined-agent",
				"-n", combinedTestNamespace)
			_, err := cmd.CombinedOutput()
			g.Expect(err).To(HaveOccurred(), "AgentRuntime should be deleted")
		}).Should(Succeed())

		By("verifying deployment still exists")
		cmd = exec.Command("kubectl", "get", "deployment", "combined-agent",
			"-n", combinedTestNamespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("verifying kagenti.io/type label removed from workload metadata")
		Eventually(func(g Gomega) {
			typeLabel, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace, "{.metadata.labels['kagenti\\.io/type']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(typeLabel).To(BeEmpty())
		}).Should(Succeed())

		By("verifying kagenti.io/type label removed from PodTemplateSpec")
		Eventually(func(g Gomega) {
			typeLabel, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace, "{.spec.template.metadata.labels['kagenti\\.io/type']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(typeLabel).To(BeEmpty())
		}).Should(Succeed())

		By("verifying managed-by label removed")
		Eventually(func(g Gomega) {
			managedBy, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace, "{.metadata.labels['app\\.kubernetes\\.io/managed-by']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(managedBy).To(BeEmpty())
		}).Should(Succeed())

		By("verifying config-hash annotation removed from PodTemplateSpec")
		Eventually(func(g Gomega) {
			hash, err := utils.KubectlGetJsonpath("deployment", "combined-agent",
				combinedTestNamespace,
				"{.spec.template.metadata.annotations['kagenti\\.io/config-hash']}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(hash).To(BeEmpty())
		}).Should(Succeed())

		By("verifying AgentCard still exists")
		cardName := "combined-agent-deployment-card"
		Eventually(func(g Gomega) {
			name, err := utils.KubectlGetJsonpath("agentcard", cardName,
				combinedTestNamespace, "{.metadata.name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(name).To(Equal(cardName))
		}).Should(Succeed())

		By("waiting for rolling update to complete after label removal")
		Expect(utils.WaitForDeploymentReady("combined-agent", combinedTestNamespace, 3*time.Minute)).To(Succeed())

		By("verifying replacement pods have no sidecars")
		Eventually(func(g Gomega) {
			containers, err := utils.KubectlGetJsonpath("pod", "",
				combinedTestNamespace,
				"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='combined-agent')].spec.containers[*].name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(containers).NotTo(ContainSubstring("envoy-proxy"),
				"envoy-proxy should not be injected after AR deletion")
		}).Should(Succeed())

		By("verifying replacement pods have no init containers")
		Eventually(func(g Gomega) {
			initContainers, err := utils.KubectlGetJsonpath("pod", "",
				combinedTestNamespace,
				"{.items[?(@.metadata.labels.app\\.kubernetes\\.io/name=='combined-agent')].spec.initContainers[*].name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(initContainers).NotTo(ContainSubstring("proxy-init"),
				"proxy-init should not be injected after AR deletion")
		}).Should(Succeed())
	})
})
