#!/usr/bin/env bash
#
# Teardown script for the SPIRE signing demo.
# Deletes k8s resources created by the demo.
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K8S_DIR="${SCRIPT_DIR}/k8s"

NAMESPACE="${NAMESPACE:-agents}"

echo "=== SPIRE Signing Demo Teardown ==="
echo ""

echo "Deleting Kubernetes resources..."
kubectl delete -f "${K8S_DIR}/agent-deployment.yaml" --ignore-not-found=true 2>/dev/null || true
kubectl delete -f "${K8S_DIR}/clusterspiffeid.yaml" --ignore-not-found=true 2>/dev/null || true
echo "Kubernetes resources deleted."
echo ""

echo "Deleting namespace '${NAMESPACE}'..."
kubectl delete namespace "${NAMESPACE}" --wait=false --ignore-not-found=true 2>/dev/null || true

# On shared OpenShift clusters, namespaces can get stuck in Terminating
# due to stale API groups (e.g. kubevirt). Force-finalize if needed.
sleep 5
if kubectl get namespace "${NAMESPACE}" 2>/dev/null | grep -q Terminating; then
  echo "Namespace stuck in Terminating — force-finalizing..."
  kubectl get namespace "${NAMESPACE}" -o json | \
    python3 -c "import sys,json; ns=json.load(sys.stdin); ns['spec']['finalizers']=[]; print(json.dumps(ns))" | \
    kubectl replace --raw "/api/v1/namespaces/${NAMESPACE}/finalize" -f - >/dev/null 2>&1 || true
fi
echo "Namespace deleted."
echo ""

echo "=== Teardown Complete ==="
