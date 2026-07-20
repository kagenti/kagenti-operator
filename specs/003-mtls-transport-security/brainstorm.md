# Brainstorm: mTLS Transport Security for Agent Communication

**Date**: 2026-06-03
**Branch**: `mtls-spec`
**Jira**: RHAIENG-4944 — Agent Discovery via mTLS
**Parent Epic**: RHAIENG-4931 — Minimal AgentRuntime CRD Rework
**Parent Feature**: RHAISTRAT-1599 — Productize & Downstream the Agent Operator
**ADR**: ODH-ADR-AgentOps-0002 — Agent Network Policy and mTLS Identity
**Target Release**: rhoai-3.5.EA2 (deadline June 15-19, 2026)

## Scope Decisions

### What this spec covers

**mTLS transport security for two communication paths:**

1. **Controller-to-agent** (control plane → data plane) — the operator controller fetching agent cards and communicating with agent workloads over mTLS
2. **Agent-to-agent** (data plane ↔ data plane) — inter-agent calls where both sides prove identity via mutual TLS certificates

### What this spec does NOT cover

- Card discovery / `status.card` population (covered by spec 001)
- `spec.policy` enforcement (NetworkPolicy, AuthorizationPolicy) — separate spec
- AgentCard CRD deprecation and removal — separate migration spec
- AgentMesh CRD design — future work
- Cross-cluster agent federation — future work
- Bearer token / OAuth2 authorization (handled by authbridge, orthogonal to mTLS)

## Clarification Answers

### Q1: What sidecar mode should mTLS target?

**Answer: All sidecar modes.** mTLS must work across all authbridge sidecar modes:
- **Envoy sidecar** (full proxy with iptables interception)
- **Proxy sidecar** (lightweight HTTP_PROXY-based)
- **Lite mode** (minimal footprint)
- **Waypoint** (standalone deployment, not injected)

The mTLS implementation must be sidecar-agnostic — the certificate presentation and verification happens at whatever proxy layer is present.

### Q2: What certificate sources should be supported?

**Answer: SPIRE only.** Single certificate provider for the initial implementation:

1. **SPIRE (default and only)** — SPIRE-issued X.509 SVIDs via the Workload API. Already deployed for JWT SVIDs in Rossoctl. The spiffe-helper sidecar or go-spiffe SDK provides certificates.

Istio, user-supplied certificates, and cert-manager are explicitly out of scope. No Istio dependency — Istio support can be added in a future iteration if needed.

### Q3: How should the controller obtain its SPIFFE identity?

**Answer: go-spiffe SDK directly in the controller binary (option b).** The controller talks to the SPIRE Workload API directly. This is already implemented and working via `SpiffeFetcher` using `go-spiffe` X509Source. No authbridge sidecar on the controller pod.

### Q4: How should mTLS be enforced?

**Answer: Enabled by default, disabled is opt-in.** mTLS is on by default when SPIRE is available. Operators explicitly opt out per-AgentRuntime with `mTLSMode: disabled` if needed. No global feature flag for enforcement — it's the default behavior.

### Q6: How should certificates reach the data-plane sidecar?

**Answer: spiffe-helper sidecar (file-based, option a).** A spiffe-helper container fetches SVIDs from SPIRE and writes them as PEM files (`svid.pem`, `svid_key.pem`, `bundle.pem`) to a shared volume. The proxy reads these files and reloads on change. This is already implemented and deployed.

**Why not go-spiffe SDK directly in the proxy (option b)?** Option (b) eliminates the spiffe-helper container and keeps certs in memory, but it ties the proxy to SPIRE's Workload API. Option (a) keeps the proxy certificate-source-agnostic — it reads PEM files regardless of where they came from. This preserves the sidecar-agnostic constraint and works across all authbridge modes. Option (b) is the long-term direction per `cortex#332` but is deferred.

### Q7: What happens when mTLS is enabled but SPIRE is not deployed?

**Answer: Fail clearly (option b).** If mTLS is the default and SPIRE isn't available, set the AgentRuntime to an error state with a clear condition. The operator must either deploy SPIRE or explicitly set `mTLSMode: disabled`. No silent fallback to plain HTTP — that would mask a real security gap.

### Q8: Should this spec cover authbridge code changes in cortex?

**Answer: Yes — cover both repos.** This spec defines the work required in both `rossoctl-operator` (controller, CRD, webhook) and `cortex` (authbridge proxy TLS contexts). The spec captures what needs to change in authbridge to enable mTLS (DownstreamTlsContext, UpstreamTlsContext, certificate loading). Downstreaming logistics (how to bring authbridge code into the product build) are explicitly out of scope — that's a separate spike.

### Q5: Istio integration?

**Answer: Not supported in this iteration.** Istio is explicitly out of scope for this spec. No dependency on Istio. The platform uses SPIRE as the sole mTLS provider. Note: Istio service mesh with L4 mTLS is being worked on separately (PR #383, Issue #399, RHAIENG-5467) — that work is complementary, not competing. See spec.md "Coexistence with Istio mTLS" section.

## Source Material

### ADR Key Points (ODH-ADR-AgentOps-0002)

- **mTLS is mandatory** — all agent-to-agent and controller-to-agent communication must use mutual TLS. This replaces card signing: both sides prove identity at transport layer, on every connection.
- **Istio is not a hard dependency** — SPIRE with authbridge is the default. Istio can replace or complement when configured. Platform must work without Istio.
- **Two layers, two concerns** — mTLS handles transport-level identity (both sides verify via certificates). Authbridge handles application-level authorization via bearer tokens (OAuth2 flow). These are complementary, not replacements.
- **Why mTLS replaces card signing** — card signing proves a card was signed at deploy time, not that the agent serving it now is who it claims to be. mTLS proves the latter on every connection. Eliminates skeleton-card problem (#292).
- **Certificate source pluggable** — SPIRE (default, recommended), user-supplied, cert-manager. From authbridge's perspective, it needs a key and a certificate; where they come from is a deployment decision.
- **Event-driven, not polling** — card content is static for Pod lifetime. Verification triggers on workload rollouts.

### Meeting Decisions (May 21, 2026 — Sync with Kevin)

- **Kevin's PR is the foundation** — SpiffeFetcher, go-spiffe integration, mTLS client for controller-to-agent card fetching. Reuse library helpers.
- **Agent-to-agent mTLS lives on the sidecar** — data plane concern, not control plane. Implementation goes into authbridge, not the controller.
- **Ingress + egress** — agent needs mTLS on both slots: outgoing calls (to tools/other agents) and incoming calls (from other agents/controller).
- **Same SPIRE server for control + data plane** — team discussed security implications, no significant concerns identified. Identities are explicitly reflected in client/server certificates.
- **Customer Istio environments** — if customer uses Istio for mTLS, default to their Istio. Don't enforce a custom solution that conflicts with cluster-level security policies.
- **Envoy resource concern** — 100-200MB per pod is heavy for one-agent-per-pod model. Exploring lightweight proxy alternatives. Not blocking mTLS work, but inform design.
- **Agent Runtime Contract** — document requirements for agent behavior (respect HTTP_PROXY, propagate bearer tokens). Contract mounted in agent container. Not in scope for this spec but informs sidecar design.
- **VAP over webhooks** — team chose Validating Admission Policies for label validation (lighter weight than validating webhooks).

### Jira Context (RHAIENG-4944)

- **Summary**: Replace AgentCard polling loop with on-demand mTLS fetches
- **Assignee**: Ian Miller
- **Status**: In Progress
- **Description**: Card data comes from the live agent, not a cached CRD. Controller fetches `/.well-known/agent-card.json` over mTLS on workload rollout events, stores result in `status.card` on AgentRuntime.

## What Already Exists in the Codebase

### API Types (`api/v1alpha1/agentruntime_types.go`)

- **`MTLSMode` field** on `AgentRuntimeSpec`: enum with values `disabled`, `permissive`, `strict`
  - Auto-enables SPIRE when non-disabled
- **`CardStatus` struct**: holds fetched A2A agent card with mTLS metadata
  - `TransportSecurity` (mtls | http)
  - `AttestedAgentSpiffeID` — SPIFFE ID from peer cert
  - `ValidSignature`, `SignatureKeyID`, `SignatureVerificationDetails`
- **`SPIFFEIdentity` struct**: per-workload SPIFFE identity overrides with trust domain

### Fetcher Layer (`internal/agentcard/fetcher.go`)

- **`Fetcher` interface**: plain HTTP card fetch
- **`AuthenticatedFetcher` interface**: mTLS-authenticated fetch
- **`SpiffeFetcher`**: uses go-spiffe X509Source, verifies peer cert SPIFFE ID against trust domain
- **`ConfigMapFetcher`**: reads signed cards from ConfigMap, falls back to HTTP

### Signature Verification (`internal/signature/x5c.go`)

- **`X5CProvider`**: verifies JWS signatures via x5c certificate chains against trust bundle
- Trust bundle supports PEM and SPIFFE JSON formats
- 5-minute auto-refresh interval
- Change detection with hash comparison

### Feature Flags (`cmd/main.go`)

- `--enable-card-discovery` — activates card discovery into `status.card`
- `--enable-verified-fetch` — activates mTLS-authenticated fetch via SPIFFE
- `--spire-trust-domain` — required when verified fetch enabled
- `--spire-trust-bundle-configmap` — ConfigMap/NS/key for trust bundle
- `--verified-fetch-spiffe-socket` — SPIFFE Workload API socket path
- `--require-a2a-signature` — enforce JWS signature validation (to be deprecated)
- `--signature-audit-mode` — log failures without blocking (to be deprecated)
- `--enforce-network-policies` — create NetworkPolicies (to be deprecated)

### Controllers

- **AgentRuntimeReconciler**: has card fetch phase gated by `EnableCardDiscovery`, uses `SpiffeFetcher` when available
- **AgentCardReconciler**: has `EnableVerifiedFetch`, `AuthenticatedFetcher`, SVID expiry grace period, signature verification
- **AgentCardNetworkPolicyReconciler**: creates NetworkPolicies based on signature verification (to be replaced by policy enforcement from AgentRuntime)

## Authbridge (cortex) Current State

The authbridge in `cortex` already has significant mTLS infrastructure:

### Existing mTLS Code

- **`authlib/tls/server.go`** — `ServerConfig()` builds mTLS `*tls.Config` for reverse-proxy listener. Presents local SVID, requires client cert, verifies against SPIRE trust bundle. Hot-rotation via per-handshake callbacks (re-reads cert+key from disk).
- **`authlib/tls/client.go`** — `ClientConfig()` builds mTLS `*tls.Config` for forward-proxy dialer. Presents local SVID, verifies server cert against trust bundle. Same rotation-aware pattern.
- **`authlib/spiffe/source.go`** — `X509Source` interface: `Certificate()` returns local SVID, `TrustBundle()` returns trust bundle CertPool. Abstraction over spiffe-helper file-based certs.
- **`authlib/spiffe/provider.go`** — File-based X509Source implementation reading from spiffe-helper written PEM files.

### Proxy Binaries

Three proxy variants in `authbridge/cmd/`:
- **`authbridge-proxy`** — full proxy-sidecar (HTTP_PROXY based)
- **`authbridge-lite`** — lightweight variant
- **`authbridge-envoy`** — Envoy-based sidecar

Each has its own `main.go`, `Dockerfile`, and `entrypoint.sh`.

### Key Observation — mTLS is ALREADY IMPLEMENTED in authbridge

**All three proxy modes (proxy, lite, envoy) already have full mTLS wiring on main.** This significantly changes the scope of work:

- `authbridge-proxy/main.go` and `authbridge-lite/main.go` both have identical mTLS wiring:
  - Read `cfg.MTLS` config block, require SPIFFE provider when set
  - Build `reverseproxy.MTLSOptions` (inbound) and `forwardproxy.MTLSOptions` (outbound)
  - Permissive mode: inbound TLS-sniffing (peek-and-route), outbound plaintext
  - Strict mode: inbound rejects non-TLS, outbound TLS-or-fail
  - Shared `X509Source` across both listeners for consistent SVID + trust bundle
  - `authtls.Metrics` for TLS handshake observability
- `reverseproxy.Server.Listen()` uses a byte-peek TLS-sniffing listener when mTLS is enabled
- The `authlib/tls` package provides `ServerConfig()` (mTLS reverse-proxy) and `ClientConfig()` (mTLS forward-proxy)
- The `authlib/spiffe` package provides `X509Source` interface and `Provider` implementation
- Certificate rotation is handled per-handshake (re-reads from spiffe-helper files)

**The remaining authbridge work is likely:**
- Ensuring the operator passes the right MTLS config to the sidecar ConfigMap
- Testing mTLS across all three modes in integration
- Potentially: Envoy-specific DownstreamTlsContext/UpstreamTlsContext (if not already done)

**The main remaining work is on the operator side:**
- Setting `rossoctl.io/mtls-mode` annotation on pod template + webhook sets `MTLS_MODE` env var on authbridge
- Making mTLS enabled by default
- Error conditions when SPIRE is unavailable
- Controller-to-agent mTLS (SpiffeFetcher integration)
- Deprecating/removing JWS signing pipeline

## Key Design Constraints

1. **One agent per pod** — pod identity = agent identity (SPIFFE ID)
2. **Sidecar-agnostic** — mTLS must work across all authbridge modes
3. **SPIRE as default, Istio when present** — detect Istio enrollment per namespace
4. **No breaking changes** — existing deployments without mTLS must continue working
5. **Feature-gated** — mTLS enforcement behind flags, opt-in
6. **Authbridge prerequisite** — authbridge needs TLS contexts (DownstreamTlsContext, UpstreamTlsContext) added to envoy config
7. **EA2 deadline** — June 15-19, 2026
