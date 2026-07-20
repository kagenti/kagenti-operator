#!/usr/bin/env bash
#
# Teardown for the enforcement demo.
# Restores the AgentCard to its correct binding state.
#

set -euo pipefail

NAMESPACE="${NAMESPACE:-agents}"
AGENTCARD="${AGENTCARD:-weather-agent-deployment-card}"

echo "=== AgentCard Enforcement Demo Teardown ==="
echo ""

echo "Restoring identity binding to default (strict: true, no trust domain override)..."
kubectl patch agentcard "$AGENTCARD" -n "$NAMESPACE" --type=json -p '[
  {"op": "remove", "path": "/spec/identityBinding"}
]' 2>/dev/null || true

kubectl patch agentcard "$AGENTCARD" -n "$NAMESPACE" --type=merge -p '{
  "spec": {
    "identityBinding": {
      "strict": true
    }
  }
}'

echo "Waiting for reconciliation..."
sleep 15

echo ""
echo "=== Teardown Complete ==="
