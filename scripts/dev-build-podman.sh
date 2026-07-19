#!/bin/bash

set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-rossoctl}
NAMESPACE=${NAMESPACE:-rossoctl-system}

cd "$(dirname "$0")/../operator"

TAG=$(date +%Y%m%d%H%M%S)
echo "Building rossoctl-operator:${TAG}..."
podman build . --tag "rossoctl-operator:${TAG}"

echo "Loading image into kind cluster ${CLUSTER_NAME}..."
kind load --name "${CLUSTER_NAME}" docker-image "localhost/operator:${TAG}"

if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
  echo "Creating namespace ${NAMESPACE}..."
  kubectl create namespace "${NAMESPACE}"
fi

echo "Updating deployment in namespace ${NAMESPACE}..."
kubectl -n "${NAMESPACE}" set image deployment/rossoctl-controller-manager manager="localhost/operator:${TAG}"

echo "Waiting for rollout..."
kubectl rollout status -n "${NAMESPACE}" deployment/rossoctl-controller-manager

echo "Current pods:"
kubectl get pods -n "${NAMESPACE}" -l control-plane=controller-manager
