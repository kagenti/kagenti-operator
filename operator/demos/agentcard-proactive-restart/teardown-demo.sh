#!/usr/bin/env bash
#
# Teardown for the proactive restart demo.
# Restores the operator grace period to 30m without overwriting other args.
#

set -euo pipefail

OPERATOR_NS="${OPERATOR_NS:-rossoctl-system}"
OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-rossoctl-controller-manager}"

echo "=== AgentCard Proactive Restart Demo Teardown ==="
echo ""

CURRENT_ARGS=$(kubectl get deployment "$OPERATOR_DEPLOY" -n "$OPERATOR_NS" \
  -o jsonpath='{.spec.template.spec.containers[0].args}')

RESTORED_ARGS=$(echo "$CURRENT_ARGS" | python3 -c "
import sys, json, re
args = json.loads(sys.stdin.read())
patched = [re.sub(r'^--svid-expiry-grace-period=\S+', '--svid-expiry-grace-period=30m', a) for a in args]
print(json.dumps(patched))
")

echo "Restoring operator to --svid-expiry-grace-period=30m..."
kubectl patch deployment "$OPERATOR_DEPLOY" -n "$OPERATOR_NS" --type=json \
  -p "[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args\", \"value\": ${RESTORED_ARGS}}]"
kubectl rollout status deployment/"$OPERATOR_DEPLOY" -n "$OPERATOR_NS" --timeout=120s

echo ""
echo "=== Teardown Complete ==="
