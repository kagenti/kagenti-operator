# Feature Specification: Card Discovery Refinement Alignment

**Feature Branch**: `002-card-discovery-alignment`
**Created**: 2026-05-26
**Status**: Draft
**Input**: Brainstorm document `brainstorm/03-card-discovery-refinement-alignment.md`

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Transport Security Visibility (Priority: P1)

A platform engineer queries an AgentRuntime to understand how the agent's card was fetched and whether the transport layer provided identity verification. Today, `CardSynced=True` gives no indication whether the fetch used mTLS (transport-verified identity) or plain HTTP (no identity guarantee). The engineer must dig into operator logs to determine the security posture.

**Why this priority**: Transport security visibility is the highest-value gap in the current API. Without this visibility, platform engineers cannot assess whether their agent discovery pipeline meets security requirements. A card fetched over plain HTTP in a multi-tenant cluster could be served by a compromised pod.

**Independent Test**: Deploy an agent with and without SPIRE configured. Check `status.card.transportSecurity` and the `CardFetched` condition reason. Verify the values correctly reflect the transport used.

**Acceptance Scenarios**:

1. **Given** an AgentRuntime with card discovery enabled and the backing agent accessible over mTLS (SPIRE configured), **When** the controller fetches the card, **Then** `status.card.transportSecurity` is `"mtls"` and the `CardFetched` condition reason is `Fetched`.
2. **Given** an AgentRuntime with card discovery enabled and no SPIRE/mTLS available, **When** the controller fetches the card over plain HTTP, **Then** `status.card.transportSecurity` is `"http"` and the `CardFetched` condition reason is `FetchedInsecure`.
3. **Given** a previously populated `status.card` with `transportSecurity: mtls`, **When** SPIRE becomes unavailable and the next fetch falls back to plain HTTP, **Then** `transportSecurity` updates to `"http"` and the condition reason changes to `FetchedInsecure`.

---

### User Story 2 - Unified Condition Model (Priority: P1)

A platform engineer uses `kubectl describe agentruntime` to diagnose card discovery issues. The condition type, status, and reason should give an immediate, unambiguous picture of what happened: was the card fetched, how, and if not, why not.

**Why this priority**: The condition model is the primary diagnostic interface. The current model has a stutter (`CardSynced True CardSynced`), conflates failure reasons, and doesn't reflect transport security. Fixing this while the API is fresh avoids a breaking change later.

**Independent Test**: Create AgentRuntimes in various states (no workload, no Service, successful mTLS fetch, successful plain HTTP fetch, fetch failure, feature disabled) and verify `kubectl describe` shows the correct condition type, status, and reason for each.

**Acceptance Scenarios**:

1. **Given** an AgentRuntime whose card was fetched over mTLS, **When** an operator runs `kubectl describe agentruntime`, **Then** the output shows `CardFetched True Fetched`.
2. **Given** an AgentRuntime whose card was fetched over plain HTTP, **When** an operator runs `kubectl describe agentruntime`, **Then** the output shows `CardFetched True FetchedInsecure`.
3. **Given** an AgentRuntime whose workload has zero Ready pods, **When** the controller reconciles, **Then** the condition shows `CardFetched False WorkloadNotReady`.
4. **Given** an AgentRuntime whose workload is ready but has no matching Service, **When** the controller reconciles, **Then** the condition shows `CardFetched False ServiceNotFound`.
5. **Given** an AgentRuntime where the card fetch fails (timeout, invalid JSON, 404), **When** the controller processes the failure, **Then** the condition shows `CardFetched False FetchFailed` with error details in the message.
6. **Given** an AgentRuntime with card discovery disabled, **When** the controller reconciles, **Then** the condition shows `CardFetched False DiscoveryDisabled`.
7. **Given** an AgentRuntime whose pod template has not changed since the last successful fetch, **When** the controller reconciles, **Then** the condition shows `CardFetched True FetchSkipped`.

---

### User Story 3 - Accurate Field Names (Priority: P2)

A platform engineer or automation tool reads AgentRuntime status fields programmatically. Field names should accurately describe their content so consumers don't need to consult documentation to understand what a field holds.

**Why this priority**: Field names are part of the CRD API surface. Once consumers depend on them, renaming requires migration. The PR just merged, so this is the last window to fix naming before consumers appear.

**Independent Test**: Create an AgentRuntime with a successful card fetch. Verify `status.card.cardHash` contains a SHA-256 hash and `status.card.lastCardFetchTime` contains a timestamp.

**Acceptance Scenarios**:

1. **Given** an AgentRuntime with a successfully fetched card, **When** an operator queries `status.card.cardHash`, **Then** it contains a SHA-256 hex string representing the card content hash.
2. **Given** an AgentRuntime with a successfully fetched card, **When** an operator queries `status.card.lastCardFetchTime`, **Then** it contains an RFC 3339 timestamp of when the controller last fetched the card.

---

### User Story 4 - Protocol-Aware Port Resolution (Priority: P2)

A platform engineer deploys an agent with a multi-port Service (e.g., an admin HTTP port and an A2A protocol port). The controller should resolve the correct port for the A2A card endpoint without manual configuration, and provide an annotation override when auto-detection fails.

**Why this priority**: Multi-port Services are common in production. The current generic HTTP port resolution could pick the wrong port, causing silent fetch failures or fetching from the wrong endpoint.

**Independent Test**: Deploy a Service with ports named `admin` (port 80) and `a2a` (port 8000). Verify the controller fetches the card from port 8000, not port 80.

**Acceptance Scenarios**:

1. **Given** a Service with a port named `a2a`, **When** the controller resolves the card endpoint, **Then** it uses the `a2a` port.
2. **Given** a Service with a `rossoctl.io/port` annotation set to `9090`, **When** the controller resolves the card endpoint, **Then** it uses port 9090 regardless of port names.
3. **Given** a Service with no `a2a` port and no annotation, **When** the controller resolves the card endpoint, **Then** it falls back to the port named `http`, then to the first port.
4. **Given** a Service with the `rossoctl.io/port` annotation and a port named `a2a`, **When** the controller resolves the card endpoint, **Then** the annotation takes priority over the port name.

---

### Edge Cases

- What happens when `transportSecurity` is `"http"` and a policy engine requires mTLS? The field provides the signal; enforcement is out of scope for this feature (deferred to identity binding migration).
- What happens when a Service has the `rossoctl.io/port` annotation with an invalid value (non-numeric, port 0)? The controller falls back to port name resolution and logs a warning.
- What happens when the `cardHash` changes but the card payload fields are semantically identical (e.g., JSON key ordering difference)? The hash is computed from the serialized JSON, so ordering differences produce different hashes. This is intentional: any byte-level change triggers a re-fetch timestamp update.
- What happens when a workload is intentionally scaled to zero (KEDA, Knative, manual `replicas: 0`)? The current generation-based change detection treats a replica count change as a workload mutation, triggering a re-fetch attempt that fails with `WorkloadNotReady` even though the card data (baked into the image) is still valid. The condition self-heals when pods return, but the flapping is noisy for operators. Existing card data is always retained (FR-013). A follow-up improvement should switch change detection from `metadata.generation` (increments on any spec change including replicas) to the Kubernetes pod template hash (only changes when `spec.template` changes: image, env, volumes). Replicas are not part of `spec.template`, so scale events would be correctly ignored.

## Clarifications

### Session 2026-05-27

- Q: How should workload readiness be determined across workload kinds? → A: Check `readyReplicas > 0` for Deployments/StatefulSets, skip readiness check for Sandboxes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST add a `transportSecurity` field to `CardStatus` that records the transport layer used for the card fetch. The field MUST be a typed enum (`TransportSecurity`) with values `"mtls"` and `"http"` (enforced via `+kubebuilder:validation:Enum`). Go constants `TransportSecurityMTLS` and `TransportSecurityHTTP` MUST be used in controller code.
- **FR-002**: The system MUST rename the condition type from `CardSynced` to `CardFetched`.
- **FR-003**: The `CardFetched` condition MUST use these reasons: `Fetched` (mTLS success), `FetchedInsecure` (plain HTTP success), `FetchSkipped` (pod template unchanged), `FetchFailed` (fetch error), `ServiceNotFound` (no matching Service), `WorkloadNotReady` (no Ready pods), `DiscoveryDisabled` (feature flag off).
- **FR-004**: The system MUST rename the `cardId` field to `cardHash` in `CardStatus`.
- **FR-005**: The system MUST rename the `fetchedAt` field to `lastCardFetchTime` in `CardStatus`.
- **FR-006**: The system MUST resolve the A2A card endpoint port using this chain: `rossoctl.io/port` annotation on the Service (highest priority), then port named `a2a`, then port named `http`, then first port.
- **FR-007**: The system MUST check workload readiness before attempting service resolution. For Deployments and StatefulSets, check `readyReplicas > 0`. For Sandboxes (unstructured), skip the readiness check and proceed directly to service resolution. If the readiness check fails, set `CardFetched=False` with reason `WorkloadNotReady`.
- **FR-008**: The `CardFetched` printer column MUST replace the existing `CardSynced` printer column in the CRD.
- **FR-009**: The project constitution MUST be updated to reflect the new condition type and field names.

### Key Entities

- **CardStatus**: Extended with `transportSecurity` field. `cardId` renamed to `cardHash`. `fetchedAt` renamed to `lastCardFetchTime`.
- **CardFetched Condition**: Replaces `CardSynced`. New reasons for transport awareness and workload readiness.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Platform engineers can determine transport security posture from a single `kubectl get agentruntime -o yaml` command without consulting operator logs.
- **SC-002**: Every `CardFetched` condition reason maps to exactly one diagnostic action (wait, check config, check network, enable feature, or no action needed).
- **SC-003**: Multi-port Services with both admin and A2A ports resolve to the correct A2A port without manual annotation.
- **SC-004**: All existing card discovery unit tests pass after migration to new field names and condition model (zero test regressions).
- **SC-005**: The CRD schema contains no references to the old field names (`cardId`, `fetchedAt`, `CardSynced`).

## Assumptions

- The PR #372 merged recently enough that no external consumers depend on the current field names or condition type. Breaking changes are acceptable.
- The `transportSecurity` values `"mtls"` and `"http"` are sufficient for v1alpha1. New values (e.g., `"ztunnel"`) can be added via normal CRD schema evolution. The field uses a typed Go enum (`TransportSecurity`) with kubebuilder validation to prevent silent divergence in consumer comparisons.
- The `rossoctl.io/port` annotation is a new API surface. No existing workloads use it, so there is no migration concern.
- The ConfigMap fetch path (init-container signing) is being deprecated. It is not represented in the `transportSecurity` enum; a value can be added if needed during the coexistence period.
- Workload readiness can be determined by checking for pods matching the workload's selector with a Ready condition. This reuses existing pod listing logic.
