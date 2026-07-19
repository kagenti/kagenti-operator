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
	"encoding/base64"
	"fmt"
	"os"

	"github.com/rossoctl/operator/internal/clientreg"
)

// curlImage returns the container image used for curl-based test pods.
// Override with E2E_CURL_IMAGE for environments where Docker Hub is unavailable.
func curlImage() string {
	if v := os.Getenv("E2E_CURL_IMAGE"); v != "" {
		return v
	}
	return "curlimages/curl:latest"
}

// pythonImage returns the container image used for Python-based test workloads.
// Override with E2E_PYTHON_IMAGE for environments that need a different registry.
func pythonImage() string {
	if v := os.Getenv("E2E_PYTHON_IMAGE"); v != "" {
		return v
	}
	return "docker.io/python:3.11-slim"
}

// runAsUserYAML returns a YAML line setting runAsUser for pod security contexts.
// The default images (curlimages/curl, python:3.11-slim) need an explicit numeric
// UID because they either use a non-numeric user or run as root, which fails the
// runAsNonRoot check without it.
// Set E2E_RUN_AS_USER=none to omit the line entirely (e.g. OpenShift assigns UIDs
// via SCC), or E2E_RUN_AS_USER=<uid> to override the value.
func runAsUserYAML(defaultUID string) string {
	v := os.Getenv("E2E_RUN_AS_USER")
	if v == "none" {
		return ""
	}
	uid := defaultUID
	if v != "" {
		uid = v
	}
	return fmt.Sprintf("\n        runAsUser: %s", uid)
}

// runAsUserJSON returns a JSON fragment for runAsUser inside a securityContext.
// Returns empty string when E2E_RUN_AS_USER=none.
func runAsUserJSON(defaultUID string) string {
	v := os.Getenv("E2E_RUN_AS_USER")
	if v == "none" {
		return ""
	}
	uid := defaultUID
	if v != "" {
		uid = v
	}
	return fmt.Sprintf(`, "runAsUser": %s`, uid)
}

const testNamespace = "e2e-agentcard-test"
const authBridgeTestNamespace = "e2e-authbridge-test"
const authBridgeAgentName = "authbridge-agent"
const authBridgeAgentCMName = "authbridge-config-" + authBridgeAgentName

// keycloakClientCredentialsSecretFixture returns YAML for a Secret that matches the deterministic
// name the AuthBridge mutating webhook will pre-populate on pods of the given workload. Without it,
// the webhook's eager Secret mount (introduced to eliminate the first-deploy credentials race) would
// keep the pod in ContainerCreating forever in the e2e environment, which has no real Keycloak + no
// admin-secret for the ClientRegistration controller to successfully register clients. The values
// are dummy — these tests exercise injection shape, not OAuth flow.
func keycloakClientCredentialsSecretFixture(namespace, workload string) string {
	name := clientreg.KeycloakClientCredentialsSecretName(namespace, workload)
	clientID := base64.StdEncoding.EncodeToString([]byte("e2e-" + workload))
	clientSecret := base64.StdEncoding.EncodeToString([]byte("e2e-dummy-secret"))
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  client-id.txt: %s
  client-secret.txt: %s
`, name, namespace, clientID, clientSecret)
}

// echoAgentFixture returns YAML for echo-agent Deployment + Service (used by S1, S3).
// The rossoctl.io/type label is applied by the operator via an AgentRuntime CR.
func echoAgentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo-agent
  namespace: ` + testNamespace + `
  labels:
    protocol.rossoctl.io/a2a: ""
    app.kubernetes.io/name: echo-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: echo-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: echo-agent
        rossoctl.io/inject: disabled
        protocol.rossoctl.io/a2a: ""
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: echo
          image: ` + pythonImage() + `
          imagePullPolicy: IfNotPresent
          command:
            - python3
            - -c
            - |
              import http.server, json
              class H(http.server.BaseHTTPRequestHandler):
                  def do_GET(self):
                      if self.path == '/.well-known/agent-card.json':
                          card = {'name': 'Echo Agent', 'version': '1.0.0',
                                  'url': 'http://echo-agent.` + testNamespace + `.svc:8001'}
                          self.send_response(200)
                          self.send_header('Content-Type', 'application/json')
                          self.end_headers()
                          self.wfile.write(json.dumps(card).encode())
                      else:
                          self.send_response(404)
                          self.end_headers()
                  def log_message(self, *a): pass
              http.server.HTTPServer(('', 8001), H).serve_forever()
          ports:
            - containerPort: 8001
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
---
apiVersion: v1
kind: Service
metadata:
  name: echo-agent
  namespace: ` + testNamespace + `
spec:
  selector:
    app.kubernetes.io/name: echo-agent
  ports:
    - port: 8001
      targetPort: 8001
`
}

// noProtocolAgentFixture returns YAML for noproto-agent Deployment (S2) - receives
// rossoctl.io/type=agent via AgentRuntime but has NO protocol.rossoctl.io/* label.
// rossoctl.io/inject=disabled is set because this test validates AgentCard sync
// behaviour, not sidecar injection.
func noProtocolAgentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: noproto-agent
  namespace: ` + testNamespace + `
  labels:
    app.kubernetes.io/name: noproto-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: noproto-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: noproto-agent
        rossoctl.io/inject: disabled
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// manualAgentCardFixture returns YAML for a manual AgentCard targeting echo-agent (S3).
func manualAgentCardFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: echo-agent-manual-card
  namespace: ` + testNamespace + `
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: echo-agent
`
}

// invalidAgentCardFixture returns YAML for an AgentCard WITHOUT spec.targetRef (S6).
func invalidAgentCardFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: invalid-no-targetref
  namespace: ` + testNamespace + `
spec:
  syncPeriod: "30s"
`
}

// auditAgentFixture returns YAML for audit-agent Deployment + Service (S5).
// The rossoctl.io/type label is applied by the operator via an AgentRuntime CR.
func auditAgentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: audit-agent
  namespace: ` + testNamespace + `
  labels:
    protocol.rossoctl.io/a2a: ""
    app.kubernetes.io/name: audit-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: audit-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: audit-agent
        rossoctl.io/inject: disabled
        protocol.rossoctl.io/a2a: ""
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: echo
          image: ` + pythonImage() + `
          imagePullPolicy: IfNotPresent
          command:
            - python3
            - -c
            - |
              import http.server, json
              class H(http.server.BaseHTTPRequestHandler):
                  def do_GET(self):
                      if self.path == '/.well-known/agent-card.json':
                          card = {'name': 'Audit Agent', 'version': '1.0.0',
                                  'url': 'http://audit-agent.` + testNamespace + `.svc:8002'}
                          self.send_response(200)
                          self.send_header('Content-Type', 'application/json')
                          self.end_headers()
                          self.wfile.write(json.dumps(card).encode())
                      else:
                          self.send_response(404)
                          self.end_headers()
                  def log_message(self, *a): pass
              http.server.HTTPServer(('', 8002), H).serve_forever()
          ports:
            - containerPort: 8002
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
---
apiVersion: v1
kind: Service
metadata:
  name: audit-agent
  namespace: ` + testNamespace + `
spec:
  selector:
    app.kubernetes.io/name: audit-agent
  ports:
    - port: 8002
      targetPort: 8002
`
}

// auditModeAgentCardFixture returns YAML for AgentCard targeting audit-agent.
// Uses the auto-created card name so kubectl apply updates the existing card.
func auditModeAgentCardFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: audit-agent-deployment-card
  namespace: ` + testNamespace + `
  labels:
    app.kubernetes.io/name: audit-agent
    app.kubernetes.io/managed-by: rossoctl-operator
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: audit-agent
`
}

// signedAgentFixture returns YAML for the full signed-agent stack (S4):
// ServiceAccount, Role, RoleBinding, ConfigMap, Deployment (with agentcard-signer
// init-container + SPIRE CSI volume), Service.
func signedAgentFixture() string {
	return `apiVersion: v1
kind: ServiceAccount
metadata:
  name: signed-agent-sa
  namespace: ` + testNamespace + `
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agentcard-signer
  namespace: ` + testNamespace + `
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["create", "update", "get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: agentcard-signer
  namespace: ` + testNamespace + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: agentcard-signer
subjects:
  - kind: ServiceAccount
    name: signed-agent-sa
    namespace: ` + testNamespace + `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: signed-agent-card-unsigned
  namespace: ` + testNamespace + `
data:
  agent.json: |
    {
      "name": "Signed Agent",
      "description": "Agent with SPIRE-signed agent card",
      "url": "http://signed-agent.` + testNamespace + `.svc.cluster.local:8080",
      "version": "1.0.0",
      "capabilities": {
        "streaming": false,
        "pushNotifications": false
      },
      "defaultInputModes": ["text/plain"],
      "defaultOutputModes": ["text/plain"],
      "skills": [
        {
          "name": "echo",
          "description": "Echo back the input",
          "inputModes": ["text/plain"],
          "outputModes": ["text/plain"]
        }
      ]
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: signed-agent
  namespace: ` + testNamespace + `
  labels:
    protocol.rossoctl.io/a2a: ""
    app.kubernetes.io/name: signed-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: signed-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: signed-agent
        rossoctl.io/inject: disabled
        protocol.rossoctl.io/a2a: ""
    spec:
      serviceAccountName: signed-agent-sa
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      initContainers:
        - name: sign-agentcard
          image: ghcr.io/rossoctl/operator/agentcard-signer:e2e-test
          imagePullPolicy: IfNotPresent
          env:
            - name: SPIFFE_ENDPOINT_SOCKET
              value: unix:///run/spire/agent-sockets/spire-agent.sock
            - name: UNSIGNED_CARD_PATH
              value: /etc/agentcard/agent.json
            - name: AGENT_CARD_PATH
              value: /app/.well-known/agent-card.json
            - name: SIGN_TIMEOUT
              value: "30s"
            - name: AGENT_NAME
              value: signed-agent
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          volumeMounts:
            - name: spire-agent-socket
              mountPath: /run/spire/agent-sockets
              readOnly: true
            - name: unsigned-card
              mountPath: /etc/agentcard
              readOnly: true
            - name: signed-card
              mountPath: /app/.well-known
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
            limits:
              cpu: 100m
              memory: 32Mi
      containers:
        - name: agent
          image: ` + pythonImage() + `
          imagePullPolicy: IfNotPresent
          command: ["python3", "-m", "http.server", "8080", "--directory", "/app"]
          ports:
            - containerPort: 8080
          volumeMounts:
            - name: signed-card
              mountPath: /app/.well-known
              readOnly: true
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
      volumes:
        - name: spire-agent-socket
          csi:
            driver: csi.spiffe.io
            readOnly: true
        - name: unsigned-card
          configMap:
            name: signed-agent-card-unsigned
        - name: signed-card
          emptyDir:
            medium: Memory
            sizeLimit: 1Mi
---
apiVersion: v1
kind: Service
metadata:
  name: signed-agent
  namespace: ` + testNamespace + `
spec:
  selector:
    app.kubernetes.io/name: signed-agent
  ports:
    - port: 8080
      targetPort: 8080
`
}

// signedAgentCardFixture returns YAML for AgentCard with identityBinding for signed-agent (S4).
// Uses the auto-created card name so kubectl apply updates the existing card.
func signedAgentCardFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: signed-agent-deployment-card
  namespace: ` + testNamespace + `
  labels:
    app.kubernetes.io/name: signed-agent
    app.kubernetes.io/managed-by: rossoctl-operator
spec:
  syncPeriod: "30s"
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: signed-agent
  identityBinding:
    strict: true
`
}

// clusterSPIFFEIDFixture returns YAML for ClusterSPIFFEID (S4).
func clusterSPIFFEIDFixture() string {
	return `apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: e2e-agentcard-test
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      rossoctl.io/type: agent
  namespaceSelector:
    matchLabels:
      agentcard: "true"
`
}

// echoAgentRuntimeFixture returns YAML for an AgentRuntime CR targeting echo-agent.
func echoAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: echo-agent
  namespace: ` + testNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: echo-agent
`
}

// noProtoAgentRuntimeFixture returns YAML for an AgentRuntime CR targeting noproto-agent.
func noProtoAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: noproto-agent
  namespace: ` + testNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: noproto-agent
`
}

// auditAgentRuntimeFixture returns YAML for an AgentRuntime CR targeting audit-agent.
func auditAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: audit-agent
  namespace: ` + testNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: audit-agent
`
}

// signedAgentRuntimeFixture returns YAML for an AgentRuntime CR targeting signed-agent.
func signedAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: signed-agent
  namespace: ` + testNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: signed-agent
`
}

// --- AgentRuntime E2E fixtures ---

const agentRuntimeTestNamespace = "e2e-agentruntime-test"

// runtimeTargetDeploymentFixture returns YAML for the agent target Deployment (pause container).
// Includes protocol label to test cross-controller interaction with AgentCardSync.
func runtimeTargetDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: runtime-agent-target
  namespace: ` + agentRuntimeTestNamespace + `
  labels:
    app.kubernetes.io/name: runtime-agent-target
    protocol.rossoctl.io/a2a: ""
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: runtime-agent-target
  template:
    metadata:
      labels:
        app.kubernetes.io/name: runtime-agent-target
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// runtimeAgentCRFixture returns YAML for an AgentRuntime CR with type=agent.
func runtimeAgentCRFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: test-agent-runtime
  namespace: ` + agentRuntimeTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: runtime-agent-target
`
}

// runtimeMissingTargetCRFixture returns YAML for an AgentRuntime CR targeting a non-existent deployment.
func runtimeMissingTargetCRFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: test-missing-target
  namespace: ` + agentRuntimeTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: nonexistent-deployment
`
}

// runtimeToolTargetDeploymentFixture returns YAML for the tool target Deployment (pause container, no protocol labels).
func runtimeToolTargetDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: runtime-tool-target
  namespace: ` + agentRuntimeTestNamespace + `
  labels:
    app.kubernetes.io/name: runtime-tool-target
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: runtime-tool-target
  template:
    metadata:
      labels:
        app.kubernetes.io/name: runtime-tool-target
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// runtimeToolCRFixture returns YAML for an AgentRuntime CR with type=tool.
func runtimeToolCRFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: test-tool-runtime
  namespace: ` + agentRuntimeTestNamespace + `
spec:
  type: tool
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: runtime-tool-target
`
}

// runtimeClusterDefaultsConfigMapFixture returns YAML for the cluster-level defaults ConfigMap
// in rossoctl-system namespace (layer 1 of 3-layer config merge).
func runtimeClusterDefaultsConfigMapFixture() string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: rossoctl-platform-config
  namespace: rossoctl-system
data:
  trace.endpoint: "http://otel-collector.observability:4317"
`
}

// runtimeNamespaceDefaultsConfigMapFixture returns YAML for the namespace-level defaults ConfigMap
// (layer 2 of 3-layer config merge). Must have rossoctl.io/defaults=true label.
func runtimeNamespaceDefaultsConfigMapFixture() string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: runtime-ns-defaults
  namespace: ` + agentRuntimeTestNamespace + `
  labels:
    rossoctl.io/defaults: "true"
data:
  log.level: debug
`
}

// runtimeStatefulSetTargetFixture returns YAML for a StatefulSet target with headless Service.
func runtimeStatefulSetTargetFixture() string {
	return `apiVersion: v1
kind: Service
metadata:
  name: runtime-sts-target
  namespace: ` + agentRuntimeTestNamespace + `
spec:
  clusterIP: None
  selector:
    app.kubernetes.io/name: runtime-sts-target
  ports:
    - port: 80
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: runtime-sts-target
  namespace: ` + agentRuntimeTestNamespace + `
  labels:
    app.kubernetes.io/name: runtime-sts-target
spec:
  serviceName: runtime-sts-target
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: runtime-sts-target
  template:
    metadata:
      labels:
        app.kubernetes.io/name: runtime-sts-target
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// runtimeStatefulSetCRFixture returns YAML for an AgentRuntime CR targeting a StatefulSet.
func runtimeStatefulSetCRFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: test-sts-runtime
  namespace: ` + agentRuntimeTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: StatefulSet
    name: runtime-sts-target
`
}

// runtimeMinimalTargetDeploymentFixture returns YAML for a minimal target Deployment (baseline for hash comparison).
func runtimeMinimalTargetDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: runtime-minimal-target
  namespace: ` + agentRuntimeTestNamespace + `
  labels:
    app.kubernetes.io/name: runtime-minimal-target
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: runtime-minimal-target
  template:
    metadata:
      labels:
        app.kubernetes.io/name: runtime-minimal-target
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// runtimeMinimalCRFixture returns YAML for an AgentRuntime CR without overrides (baseline for hash comparison).
func runtimeMinimalCRFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: test-minimal-runtime
  namespace: ` + agentRuntimeTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: runtime-minimal-target
`
}

// runtimeOverridesTargetDeploymentFixture returns YAML for the overrides test target Deployment.
func runtimeOverridesTargetDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: runtime-overrides-target
  namespace: ` + agentRuntimeTestNamespace + `
  labels:
    app.kubernetes.io/name: runtime-overrides-target
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: runtime-overrides-target
  template:
    metadata:
      labels:
        app.kubernetes.io/name: runtime-overrides-target
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// runtimeOverridesCRFixture returns YAML for an AgentRuntime CR with CR-level overrides.
// The mtlsMode override is a real spec field that the controller stamps as an annotation
// but excludes from the config hash, so the hash should match the minimal CR.
func runtimeOverridesCRFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: test-overrides-runtime
  namespace: ` + agentRuntimeTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: runtime-overrides-target
  mtlsMode: strict
`
}

// --- AuthBridge Injection E2E fixtures ---

// authBridgeConfigMapFixture returns YAML for the 3 ConfigMaps required by
// the auth bridge webhook: authbridge-config, authbridge-runtime-config, spiffe-helper-config.
// Only the mandatory keys are set (ISSUER, KEYCLOAK_URL, KEYCLOAK_REALM, TOKEN_URL,
// DEFAULT_OUTBOUND_POLICY). The operator reads additional optional keys
// (EXPECTED_AUDIENCE, TARGET_AUDIENCE, etc.) which default to empty.
func authBridgeConfigMapFixture() string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config
  namespace: ` + authBridgeTestNamespace + `
data:
  ISSUER: "https://keycloak.example.com/realms/test"
  KEYCLOAK_URL: "https://keycloak.example.com"
  KEYCLOAK_REALM: "test"
  TOKEN_URL: "https://keycloak.example.com/realms/test/protocol/openid-connect/token"
  DEFAULT_OUTBOUND_POLICY: "passthrough"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: spiffe-helper-config
  namespace: ` + authBridgeTestNamespace + `
data:
  helper.conf: |
    agent_address = "/spiffe-workload-api/spire-agent.sock"
    cmd = ""
    cmd_args = ""
    cert_dir = "/opt"
    renew_signal = ""
    svid_file_name = "svid.pem"
    svid_key_file_name = "svid_key.pem"
    svid_bundle_file_name = "svid_bundle.pem"
    jwt_svids = [{jwt_audience="rossoctl", jwt_svid_file_name="jwt_svid.token"}]
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-runtime-config
  namespace: ` + authBridgeTestNamespace + `
data:
  config.yaml: |
    mode: envoy-sidecar
    mtls:
      mode: disabled
    pipeline:
      inbound:
        plugins:
          - name: jwt-validation
            config:
              issuer: "https://keycloak.example.com/realms/test"
      outbound:
        plugins:
          - name: token-exchange
            config:
              token_url: "https://keycloak.example.com/realms/test/protocol/openid-connect/token"
              default_policy: "passthrough"
              identity:
                type: client-secret
`
}

// authBridgeAgentRuntimeFixture returns YAML for an AgentRuntime CR targeting
// the authbridge-agent Deployment.
func authBridgeAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: authbridge-agent
  namespace: ` + authBridgeTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: authbridge-agent
`
}

// authBridgeAgentFixture returns YAML for the authbridge-agent Deployment,
// ServiceAccount, and Service. The rossoctl.io/type label is applied by the
// operator via the AgentRuntime CR created in BeforeAll.
func authBridgeAgentFixture() string {
	return `apiVersion: v1
kind: ServiceAccount
metadata:
  name: authbridge-agent
  namespace: ` + authBridgeTestNamespace + `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: authbridge-agent
  namespace: ` + authBridgeTestNamespace + `
  labels:
    app.kubernetes.io/name: authbridge-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: authbridge-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: authbridge-agent
    spec:
      serviceAccountName: authbridge-agent
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: echo
          image: ` + pythonImage() + `
          imagePullPolicy: IfNotPresent
          command:
            - python3
            - -c
            - |
              import http.server, json
              class H(http.server.BaseHTTPRequestHandler):
                  def do_GET(self):
                      self.send_response(200)
                      self.send_header('Content-Type', 'application/json')
                      self.end_headers()
                      self.wfile.write(json.dumps({"status":"ok"}).encode())
                  def log_message(self, *a): pass
              http.server.HTTPServer(('', 8080), H).serve_forever()
          ports:
            - containerPort: 8080
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
---
apiVersion: v1
kind: Service
metadata:
  name: authbridge-agent
  namespace: ` + authBridgeTestNamespace + `
spec:
  selector:
    app.kubernetes.io/name: authbridge-agent
  ports:
    - port: 8080
      targetPort: 8080
`
}

// authBridgeDisabledAgentFixture returns YAML for a Deployment that opts out
// of sidecar injection via the rossoctl.io/inject=disabled pod template label.
// The rossoctl.io/type label is applied by the operator via an AgentRuntime CR.
func authBridgeDisabledAgentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: authbridge-disabled-agent
  namespace: ` + authBridgeTestNamespace + `
  labels:
    app.kubernetes.io/name: authbridge-disabled-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: authbridge-disabled-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: authbridge-disabled-agent
        rossoctl.io/inject: disabled
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// authBridgeDisabledAgentRuntimeFixture returns YAML for an AgentRuntime CR
// targeting the disabled agent.
func authBridgeDisabledAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: authbridge-disabled-agent
  namespace: ` + authBridgeTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: authbridge-disabled-agent
`
}

// authBridgeClusterSPIFFEIDFixture returns YAML for a ClusterSPIFFEID matching
// the auth bridge test namespace.
func authBridgeClusterSPIFFEIDFixture() string {
	return `apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: e2e-authbridge-test
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      rossoctl.io/type: agent
  namespaceSelector:
    matchLabels:
      rossoctl-enabled: "true"
`
}

// --- Combined E2E fixtures ---

const combinedTestNamespace = "e2e-combined-test"

// combinedAgentFixture returns YAML for the combined-agent ServiceAccount,
// Deployment (Python HTTP server with agent card on port 8080), and Service.
// Unlike echoAgentFixture, injection is NOT disabled so Auth Bridge sidecars are injected.
func combinedAgentFixture() string {
	return `apiVersion: v1
kind: ServiceAccount
metadata:
  name: combined-agent
  namespace: ` + combinedTestNamespace + `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: combined-agent
  namespace: ` + combinedTestNamespace + `
  labels:
    protocol.rossoctl.io/a2a: ""
    app.kubernetes.io/name: combined-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: combined-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: combined-agent
        protocol.rossoctl.io/a2a: ""
    spec:
      serviceAccountName: combined-agent
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: echo
          image: ` + pythonImage() + `
          imagePullPolicy: IfNotPresent
          command:
            - python3
            - -c
            - |
              import http.server, json
              class H(http.server.BaseHTTPRequestHandler):
                  def do_GET(self):
                      if self.path == '/.well-known/agent-card.json':
                          card = {
                              'name': 'Combined Agent',
                              'version': '1.0.0',
                              'url': 'http://combined-agent.` + combinedTestNamespace + `.svc:8080',
                              'capabilities': {'streaming': False, 'pushNotifications': False},
                              'defaultInputModes': ['text/plain'],
                              'defaultOutputModes': ['text/plain'],
                              'skills': [{'name': 'echo', 'description': 'Echo back input',
                                          'inputModes': ['text/plain'], 'outputModes': ['text/plain']}]
                          }
                          self.send_response(200)
                          self.send_header('Content-Type', 'application/json')
                          self.end_headers()
                          self.wfile.write(json.dumps(card).encode())
                      else:
                          self.send_response(200)
                          self.send_header('Content-Type', 'application/json')
                          self.end_headers()
                          self.wfile.write(json.dumps({"status":"ok"}).encode())
                  def log_message(self, *a): pass
              http.server.HTTPServer(('', 8080), H).serve_forever()
          ports:
            - containerPort: 8080
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
---
apiVersion: v1
kind: Service
metadata:
  name: combined-agent
  namespace: ` + combinedTestNamespace + `
spec:
  selector:
    app.kubernetes.io/name: combined-agent
  ports:
    - port: 8080
      targetPort: 8080
`
}

// combinedAgentRuntimeFixture returns YAML for an AgentRuntime CR targeting
// the combined-agent Deployment.
func combinedAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: combined-agent
  namespace: ` + combinedTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: combined-agent
`
}

// combinedConfigMapFixture returns YAML for the 3 AuthBridge ConfigMaps
// (authbridge-config, spiffe-helper-config, authbridge-runtime-config)
// scoped to the combined test namespace.
func combinedConfigMapFixture() string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config
  namespace: ` + combinedTestNamespace + `
data:
  ISSUER: "https://keycloak.example.com/realms/test"
  KEYCLOAK_URL: "https://keycloak.example.com"
  KEYCLOAK_REALM: "test"
  TOKEN_URL: "https://keycloak.example.com/realms/test/protocol/openid-connect/token"
  DEFAULT_OUTBOUND_POLICY: "passthrough"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: spiffe-helper-config
  namespace: ` + combinedTestNamespace + `
data:
  helper.conf: |
    agent_address = "/spiffe-workload-api/spire-agent.sock"
    cmd = ""
    cmd_args = ""
    cert_dir = "/opt"
    renew_signal = ""
    svid_file_name = "svid.pem"
    svid_key_file_name = "svid_key.pem"
    svid_bundle_file_name = "svid_bundle.pem"
    jwt_svids = [{jwt_audience="rossoctl", jwt_svid_file_name="jwt_svid.token"}]
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-runtime-config
  namespace: ` + combinedTestNamespace + `
data:
  config.yaml: |
    mode: envoy-sidecar
    mtls:
      mode: disabled
    pipeline:
      inbound:
        plugins:
          - name: jwt-validation
            config:
              issuer: "https://keycloak.example.com/realms/test"
      outbound:
        plugins:
          - name: token-exchange
            config:
              token_url: "https://keycloak.example.com/realms/test/protocol/openid-connect/token"
              default_policy: "passthrough"
              identity:
                type: client-secret
`
}

// --- Skill Discovery E2E fixtures ---

const skillDiscoveryTestNamespace = "e2e-skill-discovery-test"

// skillDiscoveryDeploymentFixture returns YAML for a Deployment with the
// rossoctl.io/skills annotation set by the user (or rossoctl backend).
func skillDiscoveryDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: skill-discovery-agent
  namespace: ` + skillDiscoveryTestNamespace + `
  labels:
    app.kubernetes.io/name: skill-discovery-agent
  annotations:
    rossoctl.io/skills: '["summarizer","openshift-review"]'
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: skill-discovery-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: skill-discovery-agent
        rossoctl.io/inject: disabled
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: agent
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

// skillDiscoveryAgentRuntimeFixture returns YAML for an AgentRuntime CR
// targeting the skill-discovery-agent Deployment. No spec.skills — the
// operator discovers skills from the Deployment's annotation.
func skillDiscoveryAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: skill-discovery-agent
  namespace: ` + skillDiscoveryTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: skill-discovery-agent
`
}

// ociSkillDeploymentFixture returns YAML for a Deployment with two OCI
// ImageVolume skills and the rossoctl.io/skills annotation listing both.
func ociSkillDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: oci-skill-agent
  namespace: ` + skillDiscoveryTestNamespace + `
  labels:
    app.kubernetes.io/name: oci-skill-agent
  annotations:
    rossoctl.io/skills: '["summarizer","openshift-review"]'
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: oci-skill-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: oci-skill-agent
        rossoctl.io/inject: disabled
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: agent
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          volumeMounts:
            - name: skill-summarizer
              mountPath: /app/skills/summarizer
              readOnly: true
            - name: skill-openshift-review
              mountPath: /app/skills/openshift-review
              readOnly: true
      volumes:
        - name: skill-summarizer
          image:
            reference: registry.k8s.io/pause:3.9
        - name: skill-openshift-review
          image:
            reference: registry.k8s.io/pause:3.9
`
}

// ociSkillDeploymentOneSkillFixture returns the same Deployment with one skill
// removed (openshift-review), simulating an OCI skill removal.
func ociSkillDeploymentOneSkillFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: oci-skill-agent
  namespace: ` + skillDiscoveryTestNamespace + `
  labels:
    app.kubernetes.io/name: oci-skill-agent
  annotations:
    rossoctl.io/skills: '["summarizer"]'
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: oci-skill-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: oci-skill-agent
        rossoctl.io/inject: disabled
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: agent
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          volumeMounts:
            - name: skill-summarizer
              mountPath: /app/skills/summarizer
              readOnly: true
      volumes:
        - name: skill-summarizer
          image:
            reference: registry.k8s.io/pause:3.9
`
}

// ociSkillAgentRuntimeFixture returns YAML for an AgentRuntime CR
// targeting the oci-skill-agent Deployment.
func ociSkillAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: oci-skill-agent
  namespace: ` + skillDiscoveryTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: oci-skill-agent
`
}

// combinedClusterSPIFFEIDFixture returns YAML for a ClusterSPIFFEID matching
// the combined test namespace.
func combinedClusterSPIFFEIDFixture() string {
	return `apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: e2e-combined-test
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      rossoctl.io/type: agent
  namespaceSelector:
    matchLabels:
      rossoctl-enabled: "true"
`
}

// --- Istio Mesh Enrollment E2E Fixtures ---

const istioMeshTestNamespace = "e2e-istio-mesh-test"
const istioMeshOptOutTestNamespace = "e2e-istio-mesh-optout-test"

func istioMeshDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: istio-mesh-agent
  namespace: ` + istioMeshTestNamespace + `
  labels:
    app.kubernetes.io/name: istio-mesh-agent
    protocol.rossoctl.io/a2a: ""
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: istio-mesh-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: istio-mesh-agent
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

func istioMeshAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: istio-mesh-test-runtime
  namespace: ` + istioMeshTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: istio-mesh-agent
`
}

func istioMeshOptOutDeploymentFixture() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: istio-mesh-optout-agent
  namespace: ` + istioMeshOptOutTestNamespace + `
  labels:
    app.kubernetes.io/name: istio-mesh-optout-agent
    protocol.rossoctl.io/a2a: ""
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: istio-mesh-optout-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: istio-mesh-optout-agent
    spec:
      securityContext:
        runAsNonRoot: true` + runAsUserYAML("1000") + `
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
`
}

func istioMeshOptOutAgentRuntimeFixture() string {
	return `apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: istio-mesh-optout-runtime
  namespace: ` + istioMeshOptOutTestNamespace + `
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: istio-mesh-optout-agent
`
}
