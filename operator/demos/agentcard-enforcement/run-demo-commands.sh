#!/usr/bin/env bash
##
# Enforcement demo: trust domain rejection, binding failure enforcement, NetworkPolicy.
# Prerequisite: agentcard-spire-signing demo must be deployed.
#

set -euo pipefail

NAMESPACE="${NAMESPACE:-agents}"
AGENTCARD="${AGENTCARD:-weather-agent-deployment-card}"
DEPLOYMENT="${DEPLOYMENT:-weather-agent}"

get_status() {
  kubectl get agentcard "$AGENTCARD" -n "$NAMESPACE" -o jsonpath='{.status}' | python3 -c "
import sys, json
s = json.loads(sys.stdin.read())
print(f'  Verified:       {s.get(\"validSignature\")}')
print(f'  Bound:          {s.get(\"bindingStatus\", {}).get(\"bound\")}')
print(f'  Identity Match: {s.get(\"signatureIdentityMatch\")}')
print(f'  Reason:         {s.get(\"bindingStatus\", {}).get(\"reason\")}')
"
}

get_label() {
  local val
  val=$(kubectl get deployment "$DEPLOYMENT" -n "$NAMESPACE" \
    -o jsonpath='{.spec.template.metadata.labels.agent\.rossoctl\.dev/signature-verified}')
  echo "  Label:          ${val:-<removed>}"
}

get_netpol() {
  local pol
  pol=$(kubectl get networkpolicy -n "$NAMESPACE" --no-headers 2>/dev/null || true)
  if [ -n "$pol" ]; then
    echo "  NetworkPolicy:  $(echo "$pol" | awk '{print $1}')"
  else
    echo "  NetworkPolicy:  <none>"
  fi
}

# ── 1. Baseline ──────────────────────────────────────────────────────────────
echo "=== 1. Baseline (correct trust domain) ==="
get_status
get_label
get_netpol
echo ""

# ── 2. Wrong trust domain with strict: true ──────────────────────────────────
echo "=== 2. Wrong Trust Domain (strict: true) ==="
kubectl patch agentcard "$AGENTCARD" -n "$NAMESPACE" --type=merge -p '{
  "spec": {
    "identityBinding": {
      "trustDomain": "wrong.example.com",
      "strict": true
    }
  }
}'
echo "Waiting for reconciliation..."
sleep 20
get_status
get_label
get_netpol
echo ""

# ── 3. Wrong trust domain with strict: false ─────────────────────────────────
echo "=== 3. Wrong Trust Domain (strict: false) ==="
kubectl patch agentcard "$AGENTCARD" -n "$NAMESPACE" --type=merge -p '{
  "spec": {
    "identityBinding": {
      "trustDomain": "wrong.example.com",
      "strict": false
    }
  }
}'
echo "Waiting for reconciliation..."
sleep 20
get_status
get_label
get_netpol
echo ""

# ── 4. Restore correct binding ──────────────────────────────────────────────
echo "=== 4. Restoring correct binding ==="
kubectl patch agentcard "$AGENTCARD" -n "$NAMESPACE" --type=json -p '[
  {"op": "remove", "path": "/spec/identityBinding"}
]'
kubectl patch agentcard "$AGENTCARD" -n "$NAMESPACE" --type=merge -p '{
  "spec": {
    "identityBinding": {
      "strict": true
    }
  }
}'
echo "Waiting for reconciliation..."
sleep 20
echo ""
echo "=== 4. Restored ==="
get_status
get_label
get_netpol
