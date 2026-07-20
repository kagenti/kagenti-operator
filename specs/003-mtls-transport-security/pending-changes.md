# Pending Spec Changes (awaiting team alignment)

**Date**: 2026-06-05
**Blocked on**: Team review of PR #401 (spec) + PR #405 review comment (mTLS annotation approach)

## Changes needed after alignment

1. **FR-009**: Change from "rolling restart via ConfigMap hash change" to annotation-based restart (`rossoctl.io/mtls-mode` annotation on pod template)
2. **Remove ConfigMap mtls: block injection**: Controller does NOT inject `mtls:` block into authbridge ConfigMap. The webhook reads `mTLSMode` from the AgentRuntime CR and configures authbridge at pod CREATE time.
3. **Update tasks T005 and contracts/authbridge-mtls-config.md** to reflect webhook-driven config delivery instead of controller-driven ConfigMap injection.
4. **Add**: Controller sets `rossoctl.io/mtls-mode` annotation on the pod template. When `mTLSMode` changes on the CR, annotation changes → pod template changes → rolling restart.
5. **Add**: Webhook reads `mTLSMode` from AgentRuntime CR at pod CREATE time, sets `MTLS_MODE` env var on authbridge sidecar container.
6. **Clarify controller/webhook boundary**: Controller owns labels, annotations, status. Webhook owns sidecar injection and container configuration.
