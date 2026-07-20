# AgentCard Enforcement Demo

This demo shows how the operator enforces identity binding through trust-domain validation and NetworkPolicy.

## Prerequisites

- The `agentcard-spire-signing` demo must be deployed and passing (Verified=true, Bound=true).
- Operator running with `--enforce-network-policies=true`.

## What This Demonstrates

| Scenario | What happens |
|----------|-------------|
| Wrong trust domain | Signature stays valid, but binding fails — `Bound=false` |
| Binding failure (`strict: true`) | Label removed, restrictive NetworkPolicy applied |
| Binding failure (`strict: false`) | Label retained, permissive NetworkPolicy kept |
| Restored trust domain | Binding passes — label restored, permissive NetworkPolicy |

## Run the Demo

```bash
./demos/agentcard-enforcement/run-demo-commands.sh
```

Expected output:

```
=== 1. Baseline (correct trust domain) ===
  Verified:       True
  Bound:          True
  Identity Match: True
  Reason:         Bound
  Label:          true
  NetworkPolicy:  weather-agent-signature-policy

=== 2. Wrong Trust Domain (strict: true) ===
  Verified:       True
  Bound:          False
  Identity Match: False
  Reason:         NotBound
  Label:          <removed>
  NetworkPolicy:  weather-agent-signature-policy

=== 3. Wrong Trust Domain (strict: false) ===
  Verified:       True
  Bound:          False
  Identity Match: False
  Reason:         NotBound
  Label:          true
  NetworkPolicy:  weather-agent-signature-policy

=== 4. Restored ===
  Verified:       True
  Bound:          True
  Identity Match: True
  Reason:         Bound
  Label:          true
  NetworkPolicy:  weather-agent-signature-policy
```

## How It Works

1. The operator evaluates identity binding on every reconciliation
2. When `spec.identityBinding.trustDomain` is set, it overrides the operator-level `--spire-trust-domain`
3. If the SPIFFE ID from the x5c chain doesn't match the configured trust domain, binding fails
4. `strict: true` — binding failure removes the `signature-verified` label and triggers restrictive NetworkPolicy
5. `strict: false` (default) — binding failure is recorded in status only; the label and permissive policy are retained
6. Status always reflects the true binding result regardless of `strict`

## Cleanup

```bash
./demos/agentcard-enforcement/teardown-demo.sh
```
