#!/usr/bin/env bash
# Build all operator services, load them into a Kind cluster, and deploy.
# Creates deployments if they don't exist, updates them if they do.
#
# Services:
#   1. kagenti-controller-manager (operator)
#   2. bundle-service (OPA bundle server)
#   3. token-broker (OAuth token management)
#
# Usage:
#   ./hack/kind-reload-all.sh [kind-cluster-name] [namespace]
#
# Defaults:
#   cluster:   kagenti
#   namespace: kagenti-system

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER="${1:-kagenti}"
NAMESPACE="${2:-kagenti-system}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
IMAGE_TAG="$(git -C "${ROOT_DIR}" rev-parse --short HEAD)"

OPERATOR_IMG="localhost/kagenti-operator:${IMAGE_TAG}"
BUNDLE_IMG="localhost/bundle-service:${IMAGE_TAG}"
TOKEN_BROKER_IMG="localhost/token-broker:${IMAGE_TAG}"

echo "============================================"
echo " Building and loading to Kind: ${CLUSTER}"
echo " Namespace: ${NAMESPACE}"
echo " Tag: ${IMAGE_TAG}"
echo "============================================"

# --- Ensure namespace ---

echo ""
echo "==> Ensuring namespace '${NAMESPACE}' exists"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# --- Ensure CRDs ---

echo ""
echo "==> Ensuring CRDs are installed"
if [ -f "${ROOT_DIR}/config/crd/bases/agent.kagenti.dev_authorizationpolicies.yaml" ]; then
  kubectl apply -f "${ROOT_DIR}/config/crd/bases/agent.kagenti.dev_authorizationpolicies.yaml"
fi

# --- Build images ---

echo ""
echo "==> Building kagenti-operator image"
${CONTAINER_TOOL} build -t "${OPERATOR_IMG}" -f "${ROOT_DIR}/Dockerfile" "${ROOT_DIR}"

if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  echo ""
  echo "==> Building bundle-service image"
  ${CONTAINER_TOOL} build -t "${BUNDLE_IMG}" -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" "${ROOT_DIR}"
fi

if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  echo ""
  echo "==> Building token-broker image"
  ${CONTAINER_TOOL} build -t "${TOKEN_BROKER_IMG}" -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" "${ROOT_DIR}"
fi

# --- Load into Kind ---

echo ""
echo "==> Loading images into Kind cluster '${CLUSTER}'"
kind load docker-image "${OPERATOR_IMG}" --name "${CLUSTER}"

if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  kind load docker-image "${BUNDLE_IMG}" --name "${CLUSTER}"
fi

if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  kind load docker-image "${TOKEN_BROKER_IMG}" --name "${CLUSTER}"
fi

# --- Deploy kagenti-controller-manager ---

echo ""
echo "==> Deploying kagenti-controller-manager"
if kubectl get deployment kagenti-controller-manager -n "${NAMESPACE}" &>/dev/null; then
  kubectl set image deployment/kagenti-controller-manager \
    manager="${OPERATOR_IMG}" \
    -n "${NAMESPACE}"
else
  echo "    Deployment not found — install the operator with 'make deploy' first"
  echo "    (the controller-manager requires webhook certs, RBAC, and CRDs from kustomize)"
fi

# --- Deploy bundle-service (if source exists) ---

if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
echo ""
echo "==> Deploying bundle-service"

# ServiceAccount
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: bundle-service
  namespace: ${NAMESPACE}
EOF

# RBAC
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kagenti-bundle-service
rules:
  - apiGroups:
      - agent.kagenti.dev
    resources:
      - authorizationpolicies
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - agent.kagenti.dev
    resources:
      - authorizationpolicies/status
    verbs:
      - get
      - update
      - patch
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kagenti-bundle-service
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kagenti-bundle-service
subjects:
  - kind: ServiceAccount
    name: bundle-service
    namespace: ${NAMESPACE}
EOF

# Deployment
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bundle-service
  namespace: ${NAMESPACE}
  labels:
    app: bundle-service
spec:
  replicas: 1
  selector:
    matchLabels:
      app: bundle-service
  template:
    metadata:
      labels:
        app: bundle-service
      annotations:
        sidecar.istio.io/inject: "false"
    spec:
      serviceAccountName: bundle-service
      securityContext:
        runAsNonRoot: true
      containers:
        - name: bundle-service
          image: ${BUNDLE_IMG}
          imagePullPolicy: Never
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: LOG_LEVEL
              value: info
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            initialDelaySeconds: 2
            periodSeconds: 5
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              memory: 256Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop:
                - ALL
EOF

# Service
kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: bundle-service
  namespace: ${NAMESPACE}
spec:
  type: ClusterIP
  selector:
    app: bundle-service
  ports:
    - name: http
      port: 8080
      targetPort: http
      protocol: TCP
EOF
fi

# --- Deploy token-broker (if source exists) ---

if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
echo ""
echo "==> Deploying token-broker"

# Load OAuth credentials from .env
if [ -f "${ROOT_DIR}/.env" ]; then
  export $(grep -v '^#' "${ROOT_DIR}/.env" | grep -v '^\s*$' | xargs)
fi

if [ -z "${GITHUB_OAUTH_CLIENT_ID:-}" ] || [ -z "${GITHUB_OAUTH_CLIENT_SECRET:-}" ]; then
  echo "    ERROR: OAuth credentials not set."
  echo "    Create ${ROOT_DIR}/.env with:"
  echo "      GITHUB_OAUTH_CLIENT_ID=<your-client-id>"
  echo "      GITHUB_OAUTH_CLIENT_SECRET=<your-client-secret>"
  echo "    See .env.example for reference."
  exit 1
fi

# Create/update OAuth secret
kubectl delete secret github-oauth-credentials -n "${NAMESPACE}" 2>/dev/null || true
kubectl create secret generic github-oauth-credentials \
  --from-literal=client-id="${GITHUB_OAUTH_CLIENT_ID}" \
  --from-literal=client-secret="${GITHUB_OAUTH_CLIENT_SECRET}" \
  --namespace="${NAMESPACE}"
echo "    OAuth secret created"

# Apply the deployment manifests (single source of truth).
# These pin namespace kagenti-system and include the OAuth-callback HTTPRoute
# (attached to the shared "http" gateway). The secret is created above from
# .env, so the secret example manifest is intentionally skipped.
DEPLOY_DIR="${ROOT_DIR}/cmd/token-broker/deploy"
kubectl apply \
  -f "${DEPLOY_DIR}/00-serviceaccount.yaml" \
  -f "${DEPLOY_DIR}/02-deployment.yaml" \
  -f "${DEPLOY_DIR}/03-service.yaml" \
  -f "${DEPLOY_DIR}/04-httproute.yaml"

# Inject the freshly built, git-tagged image (the manifest carries a placeholder
# tag with imagePullPolicy: IfNotPresent; kind-loaded images need Never).
kubectl set image deployment/token-broker \
  token-broker="${TOKEN_BROKER_IMG}" \
  -n "${NAMESPACE}"
kubectl patch deployment/token-broker -n "${NAMESPACE}" --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
echo "    token-broker manifests applied (image ${TOKEN_BROKER_IMG})"
fi

# --- Delete pods to pick up the new images ---

echo ""
echo "==> Deleting pods to pick up new images"
kubectl delete pods -n "${NAMESPACE}" -l control-plane=controller-manager --wait=false 2>/dev/null || true

if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  kubectl delete pods -n "${NAMESPACE}" -l app=bundle-service --wait=false
fi

if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  kubectl delete pods -n "${NAMESPACE}" -l app=token-broker --wait=false
fi

# --- Wait for rollouts ---

echo ""
echo "==> Waiting for kagenti-controller-manager rollout"
kubectl rollout status deployment/kagenti-controller-manager -n "${NAMESPACE}" --timeout=120s 2>/dev/null || \
  echo "    WARNING: kagenti-controller-manager rollout did not complete"

if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  echo "==> Waiting for bundle-service rollout"
  kubectl rollout status deployment/bundle-service -n "${NAMESPACE}" --timeout=60s
fi

if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  echo "==> Waiting for token-broker rollout"
  kubectl rollout status deployment/token-broker -n "${NAMESPACE}" --timeout=60s
fi

# --- Summary ---

echo ""
echo "============================================"
echo " Done!"
echo ""
echo " Images loaded:"
echo "   ${OPERATOR_IMG}"
if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  echo "   ${BUNDLE_IMG}"
fi
if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  echo "   ${TOKEN_BROKER_IMG}"
fi
echo ""
echo " Namespace: ${NAMESPACE}"
echo "   - kagenti-controller-manager"
if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  echo "   - bundle-service"
fi
if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  echo "   - token-broker"
fi
echo ""
if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  echo " bundle-service URL: http://bundle-service.${NAMESPACE}.svc.cluster.local:8080"
fi
if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  echo " token-broker URL:   http://token-broker.${NAMESPACE}.svc.cluster.local:8190"
  echo " OAuth callback:     http://token-broker.localtest.me:8080/oauth/callback (via 'http' gateway)"
fi
echo ""
echo " To port-forward:"
if [ -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" ]; then
  echo "   kubectl port-forward -n ${NAMESPACE} svc/bundle-service 8080:8080"
fi
if [ -f "${ROOT_DIR}/cmd/token-broker/Dockerfile" ]; then
  echo "   kubectl port-forward -n ${NAMESPACE} svc/token-broker 8190:8190"
fi
echo "============================================"
