#!/usr/bin/env bash
##
# Run verification commands for the SPIRE signing demo.
# Assumes setup is complete (see demo.md).
#

set -euo pipefail

NAMESPACE="${NAMESPACE:-agents}"

POD=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=weather-agent \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')

echo "=== 1. Init-Container Signing Logs ==="
kubectl logs -n "$NAMESPACE" "$POD" -c sign-agentcard
echo ""

echo "=== 2. Signed Card Verification ==="
kubectl exec -n "$NAMESPACE" "$POD" -c agent -- python3 -c "
import json
with open('/app/.well-known/agent-card.json') as f:
    d = json.load(f)
print(f'  Name:       {d.get(\"name\")}')
print(f'  Signed:     {\"signatures\" in d}')
print(f'  Signatures: {len(d.get(\"signatures\", []))}')
"
echo ""

echo "=== 3. JWS Protected Header ==="
kubectl get agentcard weather-agent-deployment-card -n "$NAMESPACE" \
  -o jsonpath='{.status.card.signatures[0].protected}' | python3 -c "
import sys, base64, json
b64 = sys.stdin.read().strip()
header = json.loads(base64.urlsafe_b64decode(b64 + '=='))
print(f'  Algorithm:  {header.get(\"alg\")}')
print(f'  Type:       {header.get(\"typ\")}')
print(f'  Key ID:     {header.get(\"kid\")}')
print(f'  x5c certs:  {len(header.get(\"x5c\", []))}')
"
echo ""

echo "=== 4. Operator Verification Status ==="
kubectl get agentcard weather-agent-deployment-card -n "$NAMESPACE" \
  -o jsonpath='{.status.conditions}' | python3 -c "
import sys, json
for c in json.loads(sys.stdin.read()):
    if c['type'] == 'SignatureVerified':
        print(f'  SignatureVerified: {c[\"status\"]}  ({c[\"reason\"]})')
    if c['type'] == 'Bound':
        print(f'  Bound:             {c[\"status\"]}  ({c[\"reason\"]})')
    if c['type'] == 'Synced':
        print(f'  Synced:            {c[\"status\"]}  ({c[\"reason\"]})')
"
echo ""

echo "=== 5. Identity Binding ==="
kubectl get agentcard weather-agent-deployment-card -n "$NAMESPACE" \
  -o jsonpath='{.status}' | python3 -c "
import sys, json
s = json.loads(sys.stdin.read())
print(f'  SPIFFE ID:      {s.get(\"signatureSpiffeId\", \"(none)\")}')
print(f'  Identity Match: {s.get(\"signatureIdentityMatch\")}')
print(f'  Bound:          {s.get(\"bindingStatus\", {}).get(\"bound\")}')
"
echo ""

echo "=== 6. Signature Label ==="
LABEL=$(kubectl get deployment weather-agent -n "$NAMESPACE" \
  -o jsonpath='{.spec.template.metadata.labels.agent\.rossoctl\.dev/signature-verified}')
echo "  agent.rossoctl.dev/signature-verified: ${LABEL:-<not set>}"
echo ""

echo "=== 7. AgentCard Summary ==="
kubectl get agentcard -n "$NAMESPACE"
