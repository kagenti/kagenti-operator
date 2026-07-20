#!/usr/bin/env bash
##
# Proactive restart demo: SVID expiry detection and automatic re-signing.
# Prerequisite: agentcard-spire-signing demo must be deployed.
#
# The operator args below must match your deployment. Adjust if your
# operator uses different flags.
#

set -euo pipefail

NAMESPACE="${NAMESPACE:-agents}"
AGENTCARD="${AGENTCARD:-weather-agent-deployment-card}"
DEPLOYMENT="${DEPLOYMENT:-weather-agent}"
OPERATOR_NS="${OPERATOR_NS:-rossoctl-system}"
OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-rossoctl-controller-manager}"

# Capture the live operator args so we can restore them exactly after the demo.
ORIGINAL_ARGS_JSON=$(kubectl get deployment "$OPERATOR_DEPLOY" -n "$OPERATOR_NS" \
  -o jsonpath='{.spec.template.spec.containers[0].args}')

patch_operator_grace() {
  local grace="$1"
  local new_args
  new_args=$(echo "$ORIGINAL_ARGS_JSON" | python3 -c "
import sys, json, re
args = json.loads(sys.stdin.read())
patched = [re.sub(r'^--svid-expiry-grace-period=.*', '--svid-expiry-grace-period=${grace}', a) for a in args]
if not any(a.startswith('--svid-expiry-grace-period=') for a in patched):
    patched.append('--svid-expiry-grace-period=${grace}')
print(json.dumps(patched))
")
  kubectl patch deployment "$OPERATOR_DEPLOY" -n "$OPERATOR_NS" --type=json \
    -p "[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args\", \"value\": ${new_args}}]"
}

# ── Part A: Baseline ─────────────────────────────────────────────────────────
echo "=== Part A: Baseline ==="
BASELINE_POD=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name="$DEPLOYMENT" \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
echo "  Pod:            $BASELINE_POD"

BASELINE_KEYID=$(kubectl get agentcard "$AGENTCARD" -n "$NAMESPACE" \
  -o jsonpath='{.status.signatureKeyId}')
echo "  Baseline KeyId: ${BASELINE_KEYID:-(none)}"

RESIGN=$(kubectl get deployment "$DEPLOYMENT" -n "$NAMESPACE" \
  -o jsonpath='{.spec.template.metadata.annotations.agentcard\.rossoctl\.dev/resign-trigger}' 2>/dev/null || true)
echo "  resign-trigger: ${RESIGN:-(not set)}"
echo ""

# ── Part B: Trigger ──────────────────────────────────────────────────────────
echo "=== Part B: Triggering SVID expiry restart ==="
echo "  Patching operator with --svid-expiry-grace-period=999h..."
patch_operator_grace "999h"
echo "  Waiting for operator rollout..."
kubectl rollout status deployment/"$OPERATOR_DEPLOY" -n "$OPERATOR_NS" --timeout=120s
echo "  Waiting 30s for reconciliation..."
sleep 30
echo ""

# ── Part C: Verify ───────────────────────────────────────────────────────────
echo "=== Part C: Verify Restart ==="
echo "  Operator logs (restart-related):"
kubectl logs -n "$OPERATOR_NS" deployment/"$OPERATOR_DEPLOY" 2>&1 | \
  grep -i -E "proactive|resign|restart|expir" | tail -5 || echo "  (no matching log lines)"
echo ""

RESIGN_AFTER=$(kubectl get deployment "$DEPLOYMENT" -n "$NAMESPACE" \
  -o jsonpath='{.spec.template.metadata.annotations.agentcard\.rossoctl\.dev/resign-trigger}' 2>/dev/null || true)
echo "  resign-trigger: ${RESIGN_AFTER:-(not set)}"

echo ""
echo "  ResignTriggered events:"
kubectl get events -n "$NAMESPACE" --field-selector reason=ResignTriggered --no-headers 2>/dev/null || echo "  (none)"

echo ""
echo "  Current pods:"
kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name="$DEPLOYMENT" --no-headers
echo ""

# ── Part D: Restore ──────────────────────────────────────────────────────────
echo "=== Part D: Restore & Verify ==="
echo "  Restoring original operator args..."
kubectl patch deployment "$OPERATOR_DEPLOY" -n "$OPERATOR_NS" --type=json \
  -p "[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args\", \"value\": ${ORIGINAL_ARGS_JSON}}]"
kubectl rollout status deployment/"$OPERATOR_DEPLOY" -n "$OPERATOR_NS" --timeout=120s

echo "  Waiting for weather-agent rollout to complete..."
kubectl rollout status deployment/"$DEPLOYMENT" -n "$NAMESPACE" --timeout=180s
echo "  Waiting 45s for operator reconciliation..."
sleep 45

echo ""
echo "  AgentCard status after restart cycle:"
kubectl get agentcard "$AGENTCARD" -n "$NAMESPACE" -o jsonpath='{.status}' | python3 -c "
import sys, json
s = json.loads(sys.stdin.read())
print(f'  validSignature:  {s.get(\"validSignature\")}')
print(f'  signatureKeyId:  {s.get(\"signatureKeyId\")}')
print(f'  identityMatch:   {s.get(\"signatureIdentityMatch\")}')
print(f'  bound:           {s.get(\"bindingStatus\", {}).get(\"bound\")}')
"

NEW_KEYID=$(kubectl get agentcard "$AGENTCARD" -n "$NAMESPACE" \
  -o jsonpath='{.status.signatureKeyId}')
echo ""
if [ "$BASELINE_KEYID" != "$NEW_KEYID" ]; then
  echo "  Key rotated: ${BASELINE_KEYID} -> ${NEW_KEYID}"
else
  echo "  WARNING: Key ID unchanged (${BASELINE_KEYID}). The restart may not have completed yet."
fi
