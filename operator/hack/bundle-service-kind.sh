#!/usr/bin/env bash
# Build the bundle-service image, load it into a kind cluster, and deploy.
#
# Usage:
#   ./hack/bundle-service-kind.sh [kind-cluster-name] [namespace]
#
# Defaults:
#   cluster:   rossoctl
#   namespace: rossoctl-system

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER="${1:-rossoctl}"
NAMESPACE="${2:-rossoctl-system}"
IMAGE="localhost/bundle-service:latest"

echo "==> Building bundle-service image"
docker build -t "${IMAGE}" -f "${ROOT_DIR}/cmd/bundle-service/Dockerfile" "${ROOT_DIR}"

echo "==> Loading image into kind cluster '${CLUSTER}'"
kind load docker-image "${IMAGE}" --name "${CLUSTER}"

echo "==> Ensuring namespace '${NAMESPACE}' exists"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

echo "==> Ensuring CRD is installed"
if [ -f "${ROOT_DIR}/config/crd/bases/agent.rossoctl.dev_authorizationpolicies.yaml" ]; then
  kubectl apply -f "${ROOT_DIR}/config/crd/bases/agent.rossoctl.dev_authorizationpolicies.yaml"
fi

echo "==> Applying default global AuthorizationPolicy"
kubectl apply -f "${ROOT_DIR}/config/bundleservice/default-policy.yaml"

echo "==> Deploying bundle-service to namespace '${NAMESPACE}'"

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
  name: rossoctl-bundle-service
rules:
  - apiGroups:
      - agent.rossoctl.dev
    resources:
      - authorizationpolicies
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - agent.rossoctl.dev
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
  name: rossoctl-bundle-service
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: rossoctl-bundle-service
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
          image: ${IMAGE}
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

echo "==> Waiting for rollout"
kubectl rollout status deployment/bundle-service -n "${NAMESPACE}" --timeout=60s

echo "==> bundle-service deployed successfully"
echo "    URL: http://bundle-service.${NAMESPACE}.svc.cluster.local:8080"
echo "    To port-forward: kubectl port-forward -n ${NAMESPACE} svc/bundle-service 8080:8080"
