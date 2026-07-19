#!/usr/bin/env bash
#
# operator-rollout.sh — Build, load, and deploy the rossoctl-operator with
# AuthBridge webhook into a local Kind cluster.
#
# This script builds the operator, loads it into Kind, and deploys via Helm
# with webhook, cert-manager, and metrics enabled. It:
#   1. Detects container runtime (docker/podman)
#   2. Builds the operator image with ko and loads into Kind
#   3. Disables the old rossoctl-webhook MWC from rossocortex
#   4. Ensures CRDs are installed (Helm skips crds/ on upgrade)
#   5. Cleans up immutable RBAC resources (roleRef conflicts)
#   6. Deploys the operator via Helm with webhook enabled
#   7. Verifies ConfigMaps and webhook
#   8. Restarts and waits for rollout
#
# Usage:
#   ./scripts/operator-rollout.sh [--demo-ns <namespace>] [--cluster <name>]
#
# Options:
#   --demo-ns <ns>    Also set up a demo namespace with authbridge ConfigMaps
#   --cluster <name>  Kind cluster name (default: rossoctl)
#   --skip-build      Skip image build and load (use existing image)
#   --skip-disable    Skip disabling the old webhook MWC
#   --help            Show this help message

set -euo pipefail

# --- Defaults ----------------------------------------------------------------

CLUSTER="rossoctl"
DEMO_NS=""
SKIP_BUILD=false
SKIP_DISABLE=false
OPERATOR_NS="rossoctl-system"
OLD_MWC_NAME="rossoctl-webhook-authbridge-mutating-webhook-configuration"
CHART_DIR="../charts/operator"

# Resolve script location for relative paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# --- Parse Arguments ---------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --demo-ns)
            DEMO_NS="$2"
            shift 2
            ;;
        --cluster)
            CLUSTER="$2"
            shift 2
            ;;
        --skip-build)
            SKIP_BUILD=true
            shift
            ;;
        --skip-disable)
            SKIP_DISABLE=true
            shift
            ;;
        --help)
            head -25 "$0" | tail -20
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# --- Container Runtime Detection ---------------------------------------------
# Supports Docker and Podman (including podman-docker shims).
# Override with CONTAINER_TOOL=docker|podman.

detect_runtime() {
    if [[ -n "${CONTAINER_TOOL-}" ]]; then
        echo "${CONTAINER_TOOL}"
        return
    fi
    if command -v podman &>/dev/null; then
        local out
        out=$(podman info 2>/dev/null || true)
        if printf '%s' "$out" | grep -Ei 'apiversion|buildorigin|libpod|podman|version:' >/dev/null 2>&1; then
            echo "podman"; return
        fi
    fi
    if command -v docker &>/dev/null; then
        local out
        out=$(docker info 2>/dev/null || true)
        if printf '%s' "$out" | grep -Ei 'client: docker engine|docker engine - community|server:' >/dev/null 2>&1; then
            echo "docker"; return
        fi
        # docker binary might be a podman shim (common on Fedora/RHEL)
        if printf '%s' "$out" | grep -Ei 'apiversion|buildorigin|libpod|podman|version:' >/dev/null 2>&1; then
            echo "podman"; return
        fi
    fi
    echo "ERROR: Neither docker nor podman found" >&2
    exit 1
}

CONTAINER_TOOL=$(detect_runtime)
echo "==> Container runtime: ${CONTAINER_TOOL}"

# --- Build Variables ---------------------------------------------------------

IMAGE_TAG="$(git -C "${REPO_ROOT}" rev-parse --short HEAD)"
KO_DOCKER_REPO="ko.local"
CMD_NAME="rossoctl-operator"
IMG="${KO_DOCKER_REPO}/${CMD_NAME}:${IMAGE_TAG}"
ARCH="$(go env GOARCH)"

echo "==> Image: ${IMG}"
echo "==> Cluster: ${CLUSTER}"

# --- Step 1: Build and Load Image -------------------------------------------

if [[ "${SKIP_BUILD}" == "false" ]]; then
    echo ""
    echo "==> Building operator image with ko..."
    cd "${REPO_ROOT}"
    KO_DOCKER_REPO="${KO_DOCKER_REPO}/${CMD_NAME}" ko build -B ./cmd -t "${IMAGE_TAG}" --platform "linux/${ARCH}"
    ${CONTAINER_TOOL} tag "${KO_DOCKER_REPO}/${CMD_NAME}/cmd:${IMAGE_TAG}" "${IMG}"

    echo "==> Loading image into Kind cluster '${CLUSTER}'..."
    if kind load docker-image "${IMG}" --name "${CLUSTER}" 2>/dev/null; then
        echo "    Image loaded via kind load."
    else
        echo "    kind load failed (snapshotter bug?) — falling back to ctr import..."
        ${CONTAINER_TOOL} save "${IMG}" | ${CONTAINER_TOOL} exec -i "${CLUSTER}-control-plane" ctr -n k8s.io images import -
        echo "    Image loaded via ctr import."
    fi
else
    echo ""
    echo "==> Skipping build (--skip-build)"
fi

# --- Step 2: Disable Old Webhook MWC ----------------------------------------

if [[ "${SKIP_DISABLE}" == "false" ]]; then
    echo ""
    echo "==> Checking for old rossoctl-webhook MWC..."
    if kubectl get mutatingwebhookconfiguration "${OLD_MWC_NAME}" &>/dev/null 2>&1; then
        echo "    Found '${OLD_MWC_NAME}' — deleting..."
        kubectl delete mutatingwebhookconfiguration "${OLD_MWC_NAME}"
        echo "    Old MWC deleted."
    else
        echo "    Old MWC '${OLD_MWC_NAME}' not found — nothing to disable."
    fi
else
    echo ""
    echo "==> Skipping old webhook disable (--skip-disable)"
fi

# --- Step 3: Ensure CRDs are installed ----------------------------------------
# Helm only installs CRDs from crds/ on initial install, not on upgrade.
# Explicitly apply them so new CRDs (e.g., AgentRuntime) are always present.

echo ""
echo "==> Applying CRDs from chart..."
CRD_DIR="${REPO_ROOT}/${CHART_DIR}/crds"
if [[ -d "${CRD_DIR}" ]]; then
    kubectl apply -f "${CRD_DIR}/"
    echo "    CRDs applied."
else
    echo "    WARNING: No crds/ directory found at ${CRD_DIR}"
fi

# --- Step 4: Clean up immutable RBAC resources --------------------------------
# Kubernetes forbids changing roleRef on existing ClusterRoleBindings.
# This can happen when:
#   a) Upgrading from a subchart install (different release name → different roleRef)
#   b) Changing Helm release name between deploys
# Detect conflicts and delete the stale bindings so Helm can recreate them.

HELM_RELEASE="${HELM_RELEASE:-rossoctl}"

echo ""
echo "==> Checking for ClusterRoleBinding roleRef conflicts..."

# Collect all rossoctl ClusterRoleBindings that Helm will try to manage
EXPECTED_CRBS=(
    "rossoctl-operator-httproute-binding-${HELM_RELEASE}"
)

for crb_name in "${EXPECTED_CRBS[@]}"; do
    if kubectl get clusterrolebinding "${crb_name}" &>/dev/null 2>&1; then
        # Check if the roleRef matches what the chart will produce
        CURRENT_ROLE=$(kubectl get clusterrolebinding "${crb_name}" \
            -o jsonpath='{.roleRef.name}' 2>/dev/null || true)
        EXPECTED_ROLE="${crb_name/%-binding-*/}-${HELM_RELEASE}"
        if [[ "${CURRENT_ROLE}" != "${EXPECTED_ROLE}" ]]; then
            echo "    Conflict: '${crb_name}' has roleRef '${CURRENT_ROLE}', expected '${EXPECTED_ROLE}'"
            echo "    Deleting stale ClusterRoleBinding..."
            kubectl delete clusterrolebinding "${crb_name}"
            echo "    Deleted. Helm will recreate it."
        else
            echo "    '${crb_name}' roleRef OK."
        fi
    else
        echo "    '${crb_name}' not found — Helm will create it."
    fi
done

# Also clean up bindings from a prior subchart install with a different naming convention
for crb in $(kubectl get clusterrolebinding -o name 2>/dev/null | grep 'rossoctl-operator-httproute-binding' || true); do
    crb_short="${crb#clusterrolebinding.rbac.authorization.k8s.io/}"
    # Skip the one we expect (already handled above)
    if [[ "${crb_short}" == "rossoctl-operator-httproute-binding-${HELM_RELEASE}" ]]; then
        continue
    fi
    echo "    Found stale binding from prior install: '${crb_short}' — deleting..."
    kubectl delete clusterrolebinding "${crb_short}"
    echo "    Deleted."
done

# --- Step 5: Deploy Operator via Helm ----------------------------------------
# Helm chart manages all resources including:
#   - rossoctl-platform-config ConfigMap (from values.yaml .defaults)
#   - rossoctl-feature-gates ConfigMap (from values.yaml .featureGates)
#   - MutatingWebhookConfiguration, ValidatingWebhookConfiguration
#   - RBAC, ServiceAccount, CRDs

echo ""
echo "==> Deploying operator via Helm..."

HELM_ARGS=(
    upgrade --install
    --create-namespace
    -n "${OPERATOR_NS}"
    "${HELM_RELEASE}"
    "${REPO_ROOT}/${CHART_DIR}"
    --set "controllerManager.container.image.repository=${KO_DOCKER_REPO}/${CMD_NAME}"
    --set "controllerManager.container.image.tag=${IMAGE_TAG}"
    --set "controllerManager.container.image.pullPolicy=Never"
    --set "webhook.enable=true"
    --set "certmanager.enable=true"
    --set "metrics.enable=true"
    --set "controllerManager.container.env.ENABLE_WEBHOOKS=true"
)

helm "${HELM_ARGS[@]}"

# --- Step 6: Verify ConfigMaps and Webhook -----------------------------------

echo ""
echo "==> Verifying required ConfigMaps..."
MISSING_CMS=()
for cm in rossoctl-platform-config rossoctl-feature-gates; do
    if kubectl get configmap "${cm}" -n "${OPERATOR_NS}" &>/dev/null; then
        echo "    ConfigMap '${cm}' found."
    else
        echo "    ERROR: ConfigMap '${cm}' NOT found!"
        MISSING_CMS+=("${cm}")
    fi
done

if [[ ${#MISSING_CMS[@]} -gt 0 ]]; then
    echo ""
    echo "    FATAL: Missing ConfigMaps: ${MISSING_CMS[*]}"
    echo "    The Helm chart should create these. Check:"
    echo "      - templates/manager/configmap-platform-defaults.yaml"
    echo "      - templates/manager/configmap-feature-gates.yaml"
    echo "      - webhook.enable is set to true in values"
    exit 1
fi

echo ""
echo "==> Verifying webhook endpoint..."
kubectl get mutatingwebhookconfiguration -o name 2>/dev/null | grep -q "rossoctl" && \
    echo "    MWC found." || \
    echo "    WARNING: No rossoctl MWC found. The operator may need cert-manager to provision certs first."

# --- Step 7: Restart and Wait for Rollout ------------------------------------

echo ""
echo "==> Restarting operator to pick up new image..."
kubectl rollout restart deployment/rossoctl-controller-manager -n "${OPERATOR_NS}"
echo "==> Waiting for operator rollout..."
kubectl rollout status deployment/rossoctl-controller-manager -n "${OPERATOR_NS}" --timeout=120s

# --- Step 8 (Optional): Demo Namespace Setup ---------------------------------

if [[ -n "${DEMO_NS}" ]]; then
    echo ""
    echo "==> Setting up demo namespace '${DEMO_NS}'..."

    kubectl create namespace "${DEMO_NS}" 2>/dev/null || true
    kubectl label namespace "${DEMO_NS}" rossoctl-enabled=true --overwrite

    echo "==> Deploying required per-namespace ConfigMaps in '${DEMO_NS}'..."
    kubectl apply -n "${DEMO_NS}" -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config
data:
  KEYCLOAK_URL: "http://keycloak.${OPERATOR_NS}.svc.cluster.local:8080"
  KEYCLOAK_REALM: "rossoctl"
  SPIRE_ENABLED: "false"
  PLATFORM_CLIENT_IDS: ""
  TOKEN_URL: "http://keycloak.${OPERATOR_NS}.svc.cluster.local:8080/realms/rossoctl/protocol/openid-connect/token"
  ISSUER: "http://keycloak.${OPERATOR_NS}.svc.cluster.local:8080/realms/rossoctl"
  EXPECTED_AUDIENCE: "rossoctl"
  TARGET_AUDIENCE: "rossoctl"
  TARGET_SCOPES: "openid"
  DEFAULT_OUTBOUND_POLICY: "passthrough"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: spiffe-helper-config
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
  name: envoy-config
data:
  envoy.yaml: |
    admin:
      address:
        socket_address:
          protocol: TCP
          address: 127.0.0.1
          port_value: 9901
    static_resources:
      listeners:
      - name: outbound_listener
        address:
          socket_address:
            protocol: TCP
            address: 0.0.0.0
            port_value: 15123
        listener_filters:
        - name: envoy.filters.listener.original_dst
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.listener.original_dst.v3.OriginalDst
        filter_chains:
        - filters:
          - name: envoy.filters.network.http_connection_manager
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
              stat_prefix: outbound_http
              codec_type: AUTO
              route_config:
                name: outbound_routes
                virtual_hosts:
                - name: catch_all
                  domains: ["*"]
                  routes:
                  - match:
                      prefix: "/"
                    route:
                      cluster: original_destination
              http_filters:
              - name: envoy.filters.http.ext_proc
                typed_config:
                  "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
                  grpc_service:
                    envoy_grpc:
                      cluster_name: ext_proc_cluster
                    timeout: 30s
                  processing_mode:
                    request_header_mode: SEND
                    response_header_mode: SKIP
                    request_body_mode: NONE
                    response_body_mode: NONE
              - name: envoy.filters.http.router
                typed_config:
                  "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
      - name: inbound_listener
        address:
          socket_address:
            protocol: TCP
            address: 0.0.0.0
            port_value: 15124
        listener_filters:
        - name: envoy.filters.listener.original_dst
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.listener.original_dst.v3.OriginalDst
        filter_chains:
        - filters:
          - name: envoy.filters.network.http_connection_manager
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
              stat_prefix: inbound_http
              codec_type: AUTO
              route_config:
                name: inbound_routes
                virtual_hosts:
                - name: local_app
                  domains: ["*"]
                  request_headers_to_add:
                  - header:
                      key: "x-authbridge-direction"
                      value: "inbound"
                    append: false
                  routes:
                  - match:
                      prefix: "/"
                    route:
                      cluster: original_destination
              http_filters:
              - name: envoy.filters.http.ext_proc
                typed_config:
                  "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
                  grpc_service:
                    envoy_grpc:
                      cluster_name: ext_proc_cluster
                    timeout: 30s
                  processing_mode:
                    request_header_mode: SEND
                    response_header_mode: SKIP
                    request_body_mode: NONE
                    response_body_mode: NONE
              - name: envoy.filters.http.router
                typed_config:
                  "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
      clusters:
      - name: original_destination
        connect_timeout: 30s
        type: ORIGINAL_DST
        lb_policy: CLUSTER_PROVIDED
        original_dst_lb_config:
          use_http_header: false
      - name: ext_proc_cluster
        connect_timeout: 5s
        type: STATIC
        lb_policy: ROUND_ROBIN
        http2_protocol_options: {}
        load_assignment:
          cluster_name: ext_proc_cluster
          endpoints:
          - lb_endpoints:
            - endpoint:
                address:
                  socket_address:
                    address: 127.0.0.1
                    port_value: 9090
EOF

    echo "==> Ensuring keycloak-admin-secret exists in '${DEMO_NS}'..."
    kubectl create secret generic keycloak-admin-secret -n "${DEMO_NS}" \
      --from-literal=KEYCLOAK_ADMIN_USERNAME=admin \
      --from-literal=KEYCLOAK_ADMIN_PASSWORD=admin \
      --dry-run=client -o yaml | kubectl apply -f -

    echo ""
    echo "==> Demo namespace '${DEMO_NS}' ready."
    echo "    To test injection, create an AgentRuntime CR and deploy a pod with:"
    echo "      labels:"
    echo "        rossoctl.io/type: agent"
    echo "        rossoctl.io/inject: enabled"
fi

# --- Summary -----------------------------------------------------------------

echo ""
echo "============================================="
echo "  Operator rollout complete"
echo "============================================="
echo "  Namespace:  ${OPERATOR_NS}"
echo "  Image:      ${IMG}"
echo "  Cluster:    ${CLUSTER}"
if [[ -n "${DEMO_NS}" ]]; then
    echo "  Demo NS:    ${DEMO_NS}"
fi
echo ""
echo "  To verify injection:"
echo "    1. Create an AgentRuntime CR targeting your workload"
echo "    2. Deploy a pod with labels rossoctl.io/type=agent, rossoctl.io/inject=enabled"
echo "    3. Check: kubectl get pod <name> -o jsonpath='{.spec.containers[*].name}'"
echo ""
echo "  To revert to old webhook:"
echo "    1. Delete operator MWC: kubectl delete mutatingwebhookconfiguration <name>"
echo "    2. Re-deploy rossocortex chart (restores old MWC)"
echo "============================================="
