# AgentCard Proactive Restart Demo

This demo shows how the operator detects upcoming SVID expiry and triggers a rolling restart so workloads always have fresh signatures.

## Prerequisites

- The `agentcard-spire-signing` demo must be deployed and passing (Verified=true, Bound=true).
- SPIRE is issuing SVIDs with a finite TTL (typically ~4h).

## What This Demonstrates

| Phase | What happens |
|-------|-------------|
| Baseline | Record current pod name, key ID, and annotations |
| Trigger | Set `--svid-expiry-grace-period=999h` so the check always fires |
| Verify | New pod running, new key ID, `resign-trigger` annotation set, `ResignTriggered` events |
| Restore | Return operator to normal `30m` grace period |

## How the Trick Works

The operator checks `time.Until(leafNotAfter) < gracePeriod` on every reconciliation. SPIRE issues SVIDs with a ~4h TTL. By temporarily setting `--svid-expiry-grace-period=999h`, the check always evaluates to true (4h < 999h), forcing an immediate restart. This proves the restart logic end-to-end without waiting hours for real expiry.

## Run the Demo

```bash
./demos/agentcard-proactive-restart/run-demo-commands.sh
```

Expected output:

```
=== Part A: Baseline ===
  Pod:            weather-agent-abc123
  Baseline KeyId: a1b2c3d4e5f6g7h8
  resign-trigger: (not set)

=== Part B: Triggering SVID expiry restart ===
  Patching operator with --svid-expiry-grace-period=999h...
  Waiting for operator rollout...
  Waiting 30s for reconciliation...

=== Part C: Verify Restart ===
  Operator logs:  "Triggering proactive workload restart for re-signing"
  resign-trigger: 2026-02-20T12:00:00Z
  Events:         ResignTriggered
  Current pods:   weather-agent-xyz789 (new)

=== Part D: Restore & Verify ===
  Restoring --svid-expiry-grace-period=30m...
  validSignature: True
  signatureKeyId: (different from baseline)
  Bound:          True
```

## Why This Proves Both SVID and CA Rotation Work

The SVID expiry restart and CA rotation restart share the same code path (`maybeRestartForResign`). SVID expiry checks `time.Until(leafNotAfter) < grace`, while CA rotation checks `workloadBundleHash != currentBundleHash`. Both trigger `triggerRolloutRestart`, which sets the `resign-trigger` annotation and updates the `bundle-hash`. This demo exercises the full path end-to-end.

## Cleanup

```bash
./demos/agentcard-proactive-restart/teardown-demo.sh
```
