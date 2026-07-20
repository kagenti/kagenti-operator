# AI Gateway Operator 

## Status

Proposal 

## Summary

Add AI Gateway capabilities to the Rossoctl operator using the Gateway API
policy attachment pattern. Users create a standard `Gateway` resource
(managed by Envoy Gateway), then attach Rossoctl policy CRDs to control
LLM routing and access control. Each policy generates the downstream
Envoy AI Gateway resources needed to implement the declared intent.

Two CRDs cover the initial scope:

- **AIRoutingPolicy** — providers, models, per-model failover,
  credentials, and per-model token rate limiting.
- **AIAccessPolicy** — gateway-level mTLS using SPIFFE trust bundles.

Providers and models are separate concepts within AIRoutingPolicy.
Providers define shared connection configuration (endpoint, schema,
credentials). Models define what clients request, which provider
backends serve them, and failover order — similar to
[LiteLLM's model group pattern][LiteLLM Router]. Failover happens
within a model's backend list, never across unrelated models.

`AIAccessPolicy` is a novel feature intended to leverage our platform's 
deep SPIFFE integration to provide tokenless inference access and governance to
agent workloads.

## Background

### Gateway API 

[Gateway API] is the Kubernetes-native standard for configuring network
gateways. Because [Envoy Gateway] is an implementation that manages Envoy proxy
data planes for Gateway API resources we are targeting it as our initial dataplane.

The Envoy [AI Gateway extension] adds AI-aware capabilities on top of
Envoy Gateway: protocol translation between LLM schemas, model-based
routing, credential injection, and token accounting. It defines its own
CRDs (`AIGatewayRoute`, `AIServiceBackend`, `BackendSecurityPolicy`)
that the extension server translates into xDS configuration.

These CRDs are powerful but low-level. A user wanting to route to Ollama
with mTLS and token rate limiting needs to create and coordinate six or
more resources. Our policy CRDs provide a higher-level abstraction.

As additional proxies are supported, we will likewise translate as needed
in order to program them.

### WG AI Gateway

The Kubernetes [WG AI Gateway] is defining standards for AI-aware
networking in Gateway API. Two proposals are directly relevant:

- [Proposal 10: Egress Gateways] — introduces a `Backend` resource
  (`gateway.networking.k8s.io/v1alpha1`) as a first-class representation
  of external destinations with inline TLS, credential injection
  extensions, and MCP protocol support. Defines three-tier policy scoping
  (Route > Backend > Gateway) with oldest-wins conflict resolution.
  When this proposal matures, it could collapse the per-provider
  Backend + AIServiceBackend + BackendSecurityPolicy into a single
  resource, reducing the generated resource count without changing the
  user-facing API.

- [Proposal 7: Payload Processing] — introduces
  `PayloadProcessingPipeline` for ordered, sequential body/header
  processors (prompt validation, PII redaction, semantic routing) as
  HTTPRoute filters. This is the emerging standard for guardrails.
  How we integrate it with our abstraction is discussed in
  [Future: PayloadProcessingPipeline](#future-payloadprocessingpipeline).

Our design generates the Envoy AI Gateway CRDs that exist today, but is
structured so that when the WG proposals mature into accepted APIs, we
can adopt them as generation targets without changing the user-facing
policy CRDs. Translation from policy intent to data-plane resources is
isolated in a renderer package to support this migration.

[Gateway API]: https://gateway-api.sigs.k8s.io/
[Envoy Gateway]: https://gateway.envoyproxy.io/
[AI Gateway extension]: https://aigateway.envoyproxy.io/
[WG AI Gateway]: https://github.com/kubernetes-sigs/wg-ai-gateway
[Proposal 10: Egress Gateways]: https://github.com/kubernetes-sigs/wg-ai-gateway/blob/main/proposals/10-egress-gateways.md
[Proposal 7: Payload Processing]: https://github.com/kubernetes-sigs/wg-ai-gateway/blob/main/proposals/7-payload-processing.md

## Architecture

```
User creates                         Controller generates
────────────                         ────────────────────

┌──────────────┐
│   Gateway    │ ◄── Envoy Gateway manages the Envoy proxy
│  class: eg   │
└──────┬───────┘
       │ targetRef
       │
 ┌─────┴──────────────────────────────────────────────┐
 │                                                     │
 │ ┌────────────────┐          ┌───────────────┐       │
 │ │AIRoutingPolicy │          │AIAccessPolicy │       │
 │ │  (providers,   │          │  (mTLS)       │       │
 │ │   models,      │          │               │       │
 │ │   rate limits) │          │               │       │
 │ └───────┬────────┘          └──────┬────────┘       │
 │         │                         │                 │
 │         ▼                         ▼                 │
 │  Backend                     CA Secret              │
 │  AIServiceBackend            Server cert            │
 │  AIGatewayRoute              ClientTrafficPolicy    │
 │  BackendSecurityPolicy                              │
 │  BackendTrafficPolicy                               │
 │   (retry + rate limits)                             │
 └─────────────────────────────────────────────────────┘
```

Two controllers watch the same Gateway for different concerns:

- **Envoy Gateway** — reconciles the Gateway, deploys the Envoy proxy,
  processes BackendTrafficPolicy and ClientTrafficPolicy
- **Rossoctl operator** — reconciles the two AI policy CRDs and generates
  downstream resources

No conflict: our controller never modifies the Gateway. It creates
sibling resources that reference it. The two Rossoctl policy controllers
are independent — they generate disjoint sets of resources and never
write to each other's objects.

## API group

```
aigateway.rossoctl.dev/v1alpha1
```

Separate from the operator's agent CRDs (`agent.rossoctl.dev`) and from
Envoy AI Gateway's CRDs (`aigateway.envoyproxy.io`).

## CRDs

### AIRoutingPolicy

The core policy — without it, the Gateway has no AI routing.

Providers and models are separate concepts:

- **Providers** define shared connection configuration: endpoint,
  protocol, API schema, and credentials. A provider is referenced by
  name from model backend entries. Each provider generates one
  Backend, one AIServiceBackend, and (if credentials are set) one
  BackendSecurityPolicy.

  Providers distinguish two layers:

  | Field | Layer | Examples |
  |-------|-------|----------|
  | `protocol` | Transport | `HTTP`, `HTTP2`, `gRPC` |
  | `schema` | Application format | `OpenAI`, `AWSBedrock`, `MCP`, `A2A` |

  The WG AI Gateway's [Proposal 10: Egress Gateways] combines both
  into a single `BackendProtocol` enum. We separate them
  intentionally — MCP and A2A are application-layer schemas
  (JSON-RPC over HTTP/SSE, HTTP respectively), not transport
  protocols. A provider's protocol determines the generated
  `Backend` transport configuration; its schema determines the
  `AIServiceBackend` API translation.

- **Models** define the client-facing routing unit. Each model has a
  `name` (the virtual name clients request), a list of `backends`
  (provider references with the actual model name and failover
  priority), and optional rate limiting. Each model generates one rule
  in the AIGatewayRoute.

A single controller owns all generated resources, eliminating
cross-controller write conflicts.

```yaml
apiVersion: aigateway.rossoctl.dev/v1alpha1
kind: AIRoutingPolicy
metadata:
  name: my-routing
  namespace: team1
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: ai-gateway

  # Providers: shared endpoint + credential definitions
  providers:
  - name: ollama-local
    endpoint: http://ollama.team1.svc:11434
    schema: OpenAI

  - name: openai
    endpoint: https://api.openai.com/v1
    schema: OpenAI
    credentials:
      type: APIKey
      secretRef:
        name: openai-secret
        key: api-key

  - name: azure-openai
    endpoint: https://my-resource.openai.azure.com
    schema: OpenAI
    credentials:
      type: AzureCredentials
      tenantId: "..."
      clientId: "..."
      clientSecretRef:
        name: azure-secret
        key: client-secret

  - name: bedrock
    endpoint: https://bedrock-runtime.us-east-1.amazonaws.com
    schema: AWSBedrock
    credentials:
      type: AWSCredentials
      region: us-east-1

  # Models: what clients request, with per-model failover
  models:
  - name: gpt-4o                       # virtual name (client-facing)
    backends:
    - provider: openai
      model: gpt-4o                    # actual model name — always explicit
      priority: 0
    - provider: azure-openai
      model: gpt-4o-2024-05-13        # Azure uses a different name
      priority: 1
    rateLimit:
      tokensPerHour: 100000
      tokenCountMode: TotalToken       # InputToken | OutputToken | TotalToken
    failover:
      retryOn: [502, 503, 429]
      maxRetries: 2

  - name: gpt-4o-mini
    backends:
    - provider: openai
      model: gpt-4o-mini
      priority: 0
    rateLimit:
      tokensPerHour: 1000000

  - name: qwen2.5:3b
    backends:
    - provider: ollama-local
      model: qwen2.5:3b               # single backend, no failover

  - name: claude-sonnet
    backends:
    - provider: bedrock
      model: anthropic.claude-sonnet-4-20250514-v1:0
      priority: 0
    rateLimit:
      tokensPerHour: 100000
```

**What it generates:**

| Generated resource | API group | Count |
|----|----|----|
| Backend | gateway.envoyproxy.io | one per provider |
| AIServiceBackend | aigateway.envoyproxy.io | one per provider |
| BackendSecurityPolicy | aigateway.envoyproxy.io | one per provider with credentials |
| AIGatewayRoute | aigateway.envoyproxy.io | one (one rule per model + llmRequestCosts) |
| BackendTrafficPolicy | gateway.envoyproxy.io | one (per-model failover + rate limit rules) |

**Provider credential types:**

| `type` | Generated BackendSecurityPolicy | Use case |
|--------|--------------------------------|----------|
| `APIKey` | `spec.apiKey.secretRef` | OpenAI, Anthropic, generic |
| `AWSCredentials` | `spec.awsCredentials` with SigV4 | AWS Bedrock |
| `AzureCredentials` | `spec.azureCredentials` | Azure OpenAI |
| `GCPCredentials` | `spec.gcpCredentials` | Vertex AI |

Protocol translation between schemas (e.g., OpenAI-format request routed
to an Anthropic backend) is handled automatically by the AI Gateway
extension server based on the provider's `schema` field. No code in our
controller.

**Failover behavior:**

Failover is per model. Each model's `backends` list defines the failover
order via the `priority` field (lowest = highest priority). If the
primary backend fails (matching `retryOn` conditions), Envoy retries at
the next priority level. Within the same priority, `weight` (default 1)
distributes traffic for active-active scenarios.

Each model can specify its own `failover` configuration (retryOn,
maxRetries, backoff). A top-level `defaultFailover` field can set
defaults to avoid repetition. Models with a single backend need no
failover configuration.

In the generated AIGatewayRoute, each model becomes one rule matching
the `x-ai-eg-model` header, with multiple `backendRefs` when the model
has multiple backends.

**Rate limiting:**

Rate limits are defined per model. Each model with a `rateLimit` field
generates one rate limit rule in the BackendTrafficPolicy, matched by
the `x-ai-eg-model` header that the AI Gateway extension sets during
routing. This maps directly to Envoy AI Gateway's rate limiting
mechanism — one rule, one model header selector, one Redis counter —
with no ambiguity in descriptor grouping.

A model with no `rateLimit` field has no token quota enforced. The
`tokenCountMode` defaults to `TotalToken` and can be set to
`InputToken` or `OutputToken` for finer-grained accounting.

When any model specifies a `rateLimit`, the controller adds
`llmRequestCosts` entries to the AIGatewayRoute (telling the AI Gateway
extension to extract token counts into Envoy dynamic metadata under the
`io.envoy.ai_gateway` namespace) and rate limit rules with
`cost.response.from: Metadata` to the BackendTrafficPolicy. Both
resources are already owned by this controller for routing and failover,
so no coordination is needed.

Requires Envoy Gateway's global rate limit service (Redis-backed) for
cross-instance quota enforcement.

The initial design treats the namespace as a single tenant — rate
limit counters are scoped per model, not per client. Per-user or
per-tenant rate limiting is a separate concern. Multi-tenancy could
be addressed by extracting rate limiting into a separate policy CRD
with precedence rules akin to [BackendTrafficPolicy][] — where
more specific policies override broader defaults.

[BackendTrafficPolicy]: https://gateway.envoyproxy.io/docs/concepts/gateway_api_extensions/backend-traffic-policy/

### AIAccessPolicy

Controls which clients can reach the Gateway. Phase 1 implements mTLS
with SPIFFE trust bundles. Future phases could add JWT validation or
API key authentication.

This is a gateway-level concern — it applies uniformly to all traffic
entering the gateway, independent of which provider handles the request.

#### Why: tokenless inference access

Today, giving an agent access to an LLM provider means provisioning
an API key, storing it in a Secret, mounting it into the workload,
and building rotation and revocation processes around it. API keys
are bearer tokens — if leaked, anyone can use them. In multi-team
environments, shared keys make attribution and cost accounting
difficult.

With AIAccessPolicy, inference access is tied to workload identity.
A platform admin deploys an AI Gateway with mTLS. Agent workloads
in the namespace have SPIFFE identities provisioned by SPIRE. When
an agent calls the gateway, its X.509 SVID is validated against the
trust bundle — no API keys to provision, rotate, or leak. Workloads
without a SPIFFE identity in the trust domain are rejected at the
TLS handshake. The agent code is unchanged; it calls an
OpenAI-compatible endpoint. The credential management is pushed from
application teams to infrastructure.

Provider-level API keys (for external providers like OpenAI) still
exist, but they live in one place — the AIRoutingPolicy's credential
configuration — managed by the platform team. Individual agents
never see them.

This mechanism also opens the door to per-workload policy. After
mTLS validation, the proxy sets the `x-forwarded-client-cert`
header with the client's SPIFFE ID (e.g.
`spiffe://localtest.me/ns/team1/sa/weather-agent`). This header
cannot be spoofed — the proxy strips any client-supplied XFCC and
replaces it with data extracted from the validated certificate. Our
initial data plane (Envoy) supports this natively; the mechanism
may be replicated in future supported proxies. A future policy CRD
could match on XFCC to apply per-workload rate limits, model access
lists, or processing pipelines.

```yaml
apiVersion: aigateway.rossoctl.dev/v1alpha1
kind: AIAccessPolicy
metadata:
  name: my-access
  namespace: team1
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: ai-gateway

  mtls:
    trustDomain: localtest.me
    trustBundleConfigMap:
      name: spire-bundle
      namespace: spire-system
      key: bundle.spiffe               # default
    serverCertRef:                      # optional — auto-generated if omitted
      name: my-server-cert
```

**What it generates:**

| Generated resource | Purpose |
|----|-----|
| Secret `<name>-mtls-ca` | PEM-encoded CA certs extracted from the SPIFFE JSON trust bundle |
| Secret `<name>-mtls-server` | Self-signed ECDSA server cert (only if `serverCertRef` is omitted) |
| ClientTrafficPolicy | Requires client certs, validated against the CA Secret |

The controller reads the SPIRE trust bundle ConfigMap (which contains a
SPIFFE-format JWK set), extracts the `x509-svid` certificates, converts
them to PEM, and writes a Secret. It skips expired certificates (SPIRE
bundles retain rotated CAs that Envoy rejects).

When an AIAccessPolicy targets a Gateway, the controller also ensures
the Gateway's listeners use HTTPS with TLS termination. If the user's
AIRoutingPolicy specifies HTTP listeners, the AIAccessPolicy overrides
them to HTTPS — access policy takes precedence over routing config for
the listener protocol.

### Status

Both CRDs report their state through standard `metav1.Condition`
entries plus per-component status maps. The conditions give aggregate
signals (suitable for `kubectl wait --for=condition=...`); the maps
give per-provider or per-component detail for debugging.

#### AIRoutingPolicy

```yaml
status:
  conditions:
  - type: Accepted          # spec is syntactically valid
    status: "True"
    reason: Valid
  - type: GatewayBound      # targetRef Gateway exists
    status: "True"
    reason: Bound
  - type: ProvidersConfigured  # all provider resources created
    status: "False"
    reason: PartialFailure
    message: "2/3 providers configured"
  - type: RoutingActive     # AIGatewayRoute + BTP created and accepted
    status: "True"
    reason: Applied
  - type: Programmed        # aggregate: all sub-conditions True
    status: "False"
    reason: ProvidersNotReady

  providers:
  - name: ollama
    ready: true
  - name: openai
    ready: true
  - name: bedrock
    ready: false
    error: "invalid endpoint URL: missing scheme"

  models:
  - name: gpt-4o
    ready: true
  - name: qwen2.5:3b
    ready: true
```

| Condition | True when | False reasons |
|-----------|-----------|---------------|
| `Accepted` | Spec passes validation (endpoints parse, names unique) | `InvalidSpec` |
| `GatewayBound` | Target Gateway exists in namespace | `GatewayNotFound` |
| `ProvidersConfigured` | All Backend + AIServiceBackend + BSP resources created | `PartialFailure`, `ApplyFailed`, `CredentialSecretNotFound` |
| `RoutingActive` | AIGatewayRoute + BackendTrafficPolicy created and accepted | `ApplyFailed` |
| `Programmed` | All of the above are True | Mirrors the failing sub-condition's reason |

`Programmed` is an aggregate signal per [GEP-713][]. It goes True
when `Accepted`, `GatewayBound`, `ProvidersConfigured`, and
`RoutingActive` are all True, giving a single condition suitable for
`kubectl wait --for=condition=Programmed`.

[GEP-713]: https://gateway-api.sigs.k8s.io/geps/gep-713/

The `status.providers[]` list mirrors `spec.providers[]` and reports
per-provider readiness. When `ProvidersConfigured` is False, the
provider entries show exactly which providers failed and why —
eliminating guesswork in multi-provider configurations.

The `status.models[]` list tracks per-model route rule creation. In
the common case all models are ready; a model becomes not-ready if its
referenced provider is missing from the spec.

#### AIAccessPolicy

```yaml
status:
  conditions:
  - type: Accepted
    status: "True"
    reason: Valid
  - type: GatewayBound
    status: "True"
    reason: Bound
  - type: BundleReady       # trust bundle parsed with ≥1 valid cert
    status: "True"
    reason: Loaded
    message: "2 certificates from SPIFFE JSON bundle"
  - type: MTLSActive        # CA Secret + server cert + CTP created
    status: "True"
    reason: Applied
  - type: Programmed        # aggregate: all sub-conditions True
    status: "True"
    reason: Programmed
```

| Condition | True when | False reasons |
|-----------|-----------|---------------|
| `Accepted` | Spec passes validation | `InvalidSpec` |
| `GatewayBound` | Target Gateway exists in namespace | `GatewayNotFound` |
| `BundleReady` | Trust bundle ConfigMap read and parsed with ≥1 valid cert | `BundleNotFound`, `BundleEmpty`, `BundleParseError` |
| `MTLSActive` | CA Secret, server cert, and ClientTrafficPolicy created | `ApplyFailed`, `CertGenerationFailed` |
| `Programmed` | All of the above are True | Mirrors the failing sub-condition's reason |

`BundleReady` is re-evaluated on every requeue (default 5 minutes),
so it reflects trust bundle rotations. The message includes the
certificate count for visibility into how many CAs are trusted.

### Future: guardrails

Content filtering, prompt injection detection, PII redaction, and
semantic caching are payload processing concerns that could apply at
a per-provider or per-model level — trust boundaries differ between
a local Ollama instance and an external API endpoint. The WG AI
Gateway's [Proposal 7: Payload Processing] defines
`PayloadProcessingPipeline` as the standard for this capability.
How we attach processing pipelines to our abstraction is discussed
in [Future: PayloadProcessingPipeline](#future-payloadprocessingpipeline)
below.

Not in scope for the initial implementation.

### Future: protocol extensibility

The provider `schema` field declares the protocol a backend speaks.
The initial implementation supports inference schemas (OpenAI,
AWSBedrock, etc.), but this maps directly to the [Backend protocol
model][Proposal 10] emerging in WG AI Gateway — where protocol is a
property of the destination, not the route.

The `models` list is inference-specific: it defines virtual model
names, maps them to provider backends, and configures per-model
failover and rate limits. Non-inference protocols like MCP don't have
a model selection concept. Extending AIRoutingPolicy to support MCP
providers would add protocol-appropriate routing entries alongside
`models`, driven by the provider's schema:

```yaml
providers:
- name: ollama
  endpoint: http://ollama.svc:11434
  schema: OpenAI           # inference — uses models[]
- name: tool-server
  endpoint: http://tools.svc:8080
  schema: MCP              # MCP — no model mapping needed

models:                    # inference-specific
- name: qwen2.5:3b
  backends:
  - provider: ollama
    model: qwen2.5:3b
```

The renderer would generate `AIGatewayRoute` for inference providers
and `MCPRoute` for MCP providers. Credential injection, mTLS, and
rate limiting apply uniformly regardless of protocol. The exact shape
of non-inference routing entries is left to a future proposal once
the WG AI Gateway Backend specification matures.

### Future: PayloadProcessingPipeline

The WG AI Gateway's [Proposal 7: Payload Processing] defines
[`PayloadProcessingPipeline`][PPP] — a resource for
well-ordered payload processing policies such as semantic caching,
model selection, guardrails, PII redaction, and prompt injection
detection. These policies could apply at a per-provider or
per-model level.

In the upstream proposal, processing pipelines attach to `Backend`
resources and `Route` resources. In this proposal, we've abstracted
networking-specific concepts behind terms of art associated with
Generative AI. `Backend` maps onto "provider" — where a provider is
a backend that communicates via an AI protocol (e.g. Responses API,
MCP, A2A). `Route` is implicit in the model-to-provider mapping.
The user never sees the generated HTTPRoute or Backend resources
directly.

This presents a tension. Consider a user who wants PII redaction on
requests to OpenAI but not to Ollama (different trust boundaries).
They need a PayloadProcessingPipeline that targets only the OpenAI
provider's traffic. Three approaches:

**Target by name string.** The user references a provider or model
by name within the AIRoutingPolicy:

```yaml
kind: PayloadProcessingPipeline
spec:
  targetRef:
    kind: AIRoutingPolicy
    name: my-routing
    provider: openai          # string reference into the spec
```

The controller looks up the AIRoutingPolicy, finds the provider
named `openai`, determines which HTTPRoute rules correspond to
models using that provider, and attaches the processing pipeline to
those rules. This works, but the controller does significant
indirection, and the targeting vocabulary grows with every new
concept (provider, model, etc.).

**Expose providers and models as resources.** Instead of one
AIRoutingPolicy containing everything, the user creates individual
resources that map to the networking primitives:

```yaml
kind: AIProvider               # generates Backend + AIServiceBackend
spec:
  name: openai
  endpoint: https://api.openai.com/v1
  schema: OpenAI
---
kind: AIModel                  # generates a rule in AIGatewayRoute
spec:
  name: gpt-4o
  providerRef: openai
  model: gpt-4o
```

PayloadProcessingPipeline can now target `AIProvider` or `AIModel`
directly using standard `targetRef` — no string indirection. This is
more composable but the user manages many resources instead of one
for a simple setup.

**Hybrid: compact CR with optional break-out.** AIRoutingPolicy
stays as-is for simple deployments. When a user needs per-provider
processing, they extract that provider into a standalone `AIProvider`
CR and reference it by name from the AIRoutingPolicy. The standalone
resource is targetable by PayloadProcessingPipeline. This is similar
to how Kubernetes handles inline vs referenced specs — like a pod
template inline in a Deployment vs a standalone Pod.

The right answer likely depends on real usage patterns. This
proposal establishes the routing and access control foundation;
payload processing attachment semantics will be addressed in a
follow-up once the WG specification matures.

[PPP]: https://github.com/kubernetes-sigs/wg-ai-gateway/blob/main/proposals/7-payload-processing.md

## Example: complete deployment

A platform admin sets up a Gateway with mTLS. A team lead configures
the LLM providers and models with per-model failover and rate limits.

```yaml
# Platform admin creates the Gateway
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ai-gateway
  namespace: team1
spec:
  gatewayClassName: eg
  listeners:
  - name: https
    port: 8443
    protocol: HTTPS
---
# Platform admin sets access policy
apiVersion: aigateway.rossoctl.dev/v1alpha1
kind: AIAccessPolicy
metadata:
  name: ai-gateway-access
  namespace: team1
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: ai-gateway
  mtls:
    trustDomain: localtest.me
    trustBundleConfigMap:
      name: spire-bundle
      namespace: spire-system
---
# Team lead configures providers and models
apiVersion: aigateway.rossoctl.dev/v1alpha1
kind: AIRoutingPolicy
metadata:
  name: ai-gateway-routing
  namespace: team1
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: ai-gateway

  providers:
  - name: ollama
    endpoint: http://ollama.team1.svc:11434
    schema: OpenAI
  - name: openai
    endpoint: https://api.openai.com/v1
    schema: OpenAI
    credentials:
      type: APIKey
      secretRef:
        name: openai-secret
        key: api-key
  - name: azure-openai
    endpoint: https://my-resource.openai.azure.com
    schema: OpenAI
    credentials:
      type: AzureCredentials
      tenantId: "..."
      clientId: "..."
      clientSecretRef:
        name: azure-secret
        key: client-secret

  models:
  - name: gpt-4o
    backends:
    - provider: openai
      model: gpt-4o
      priority: 0
    - provider: azure-openai
      model: gpt-4o-2024-05-13
      priority: 1
    rateLimit:
      tokensPerHour: 100000
    failover:
      retryOn: [502, 503, connect-failure]
      maxRetries: 2
  - name: qwen2.5:3b
    backends:
    - provider: ollama
      model: qwen2.5:3b
```

### Client integration

This proposal covers the **gateway side**: programming the proxy to
require mTLS, route to LLM backends, and enforce access control. The
gateway validates client certificates against the SPIFFE trust bundle
CA and either accepts or rejects the TLS handshake. It does not care
how the client obtained its certificate.

From the client's perspective, consuming the gateway requires two
things:

1. **Endpoint** — the gateway's in-cluster Service address
   (e.g. `https://<gateway-svc>.<ns>.svc:8443`)
2. **Client certificate** — a valid X.509 SVID from the same trust
   domain, presented during the TLS handshake

Any workload with a SPIFFE identity can call the gateway. The agent
code itself doesn't change — it uses the standard OpenAI-compatible
`/v1/chat/completions` endpoint. How the workload acquires and
presents its SPIFFE certificate (CSI driver, spiffe-helper sidecar,
AuthBridge integration, or native go-spiffe) is the client-side
concern and is out of scope for this proposal.

AuthBridge (agent-to-agent OAuth/OIDC) is orthogonal to AI Gateway
(model routing and inference access control). They target different
traffic flows and do not conflict.

Client-side configuration for connecting workloads to AI Gateways
with SPIFFE identity may be addressed in a separate proposal.

## Reconciliation

Each policy has its own controller. They run independently, generate
disjoint resource sets, and can be applied in any order. A missing
policy simply means that capability isn't configured.

```
AIRoutingPolicyReconciler
  ├── validate spec → set Accepted condition
  ├── verify target Gateway exists → set GatewayBound condition
  ├── parse provider endpoints (URL → host + port)
  ├── for each provider:
  │     ├── create/update Backend (gateway.envoyproxy.io)
  │     ├── create/update AIServiceBackend (aigateway.envoyproxy.io)
  │     ├── create/update BackendSecurityPolicy (if credentials set)
  │     └── record result in status.providers[]
  ├── set ProvidersConfigured condition (aggregate)
  ├── create/update AIGatewayRoute
  │     ├── one rule per model (x-ai-eg-model match → backendRefs)
  │     ├── llmRequestCosts (if any model has rateLimit)
  │     └── record result in status.models[]
  ├── create/update BackendTrafficPolicy
  │     ├── per-model failover/retry config
  │     └── per-model rate limit rules
  ├── set RoutingActive condition
  └── clean up orphaned resources for removed providers

AIAccessPolicyReconciler
  ├── validate spec → set Accepted condition
  ├── verify target Gateway exists → set GatewayBound condition
  ├── read SPIRE trust bundle ConfigMap
  ├── parse SPIFFE JSON → extract x509-svid certs → PEM
  ├── set BundleReady condition (with cert count)
  ├── create/update CA Secret
  ├── create/update self-signed server cert (if no serverCertRef)
  ├── create/update ClientTrafficPolicy
  └── set MTLSActive condition
```

All generated resources carry an owner reference to their policy CR.
Deleting a policy garbage-collects its generated resources.

## Code structure

Translation from policy intent to data-plane-specific resources is
isolated in a renderer package. Phase 1 ships with an Envoy AI Gateway
renderer. This boundary exists so that alternative data planes (or
future WG AI Gateway standard resources) can be supported by adding a
renderer without modifying reconciliation logic.

```
internal/aigateway/
  intent.go              # data-plane-agnostic representation of routing intent
  reconciler.go          # shared reconciliation orchestration
  envoy/
    renderer.go          # intent → Envoy AI Gateway CRDs
    renderer_test.go
```

## RBAC

The policy attachment model maps naturally to organizational roles:

| Role | Creates | Why |
|------|---------|-----|
| Platform admin | Gateway, AIAccessPolicy | Controls infra and security |
| Team lead | AIRoutingPolicy (providers, models, rate limits) | Decides which providers, models, and quotas the team uses |
| Developer | nothing — calls the gateway endpoint | Consumes inference through the gateway |

The operator service account needs:

| API group | Resources | Verbs |
|-----------|-----------|-------|
| aigateway.rossoctl.dev | airoutingpolicies, aiaccesspolicies + /status + /finalizers | all |
| gateway.networking.k8s.io | gateways | get, list, watch |
| gateway.envoyproxy.io | backends, clienttrafficpolicies, backendtrafficpolicies | all |
| aigateway.envoyproxy.io | aiservicebackends, aigatewayroutes, backendsecuritypolicies | all |
| (core) | secrets | all |
| (core) | configmaps | get, list, watch |

## Infrastructure prerequisites

The Ansible installer deploys these before the operator runs:

| Component | Version | Helm chart | Purpose |
|-----------|---------|------------|---------|
| Envoy Gateway | v1.7.0 | `oci://docker.io/envoyproxy/gateway-helm` | Data plane management, must have `extensionManager` configured |
| AI Gateway controller | v0.6.0 | `oci://docker.io/envoyproxy/ai-gateway-helm` | Extension server for AIGatewayRoute/AIServiceBackend |
| AI Gateway CRDs | v0.6.0 | `oci://docker.io/envoyproxy/ai-gateway-crds-helm` | CRD definitions |
| GatewayClass `eg` | — | kubectl apply | Links Gateways to Envoy Gateway's controller |
| SPIRE | — | spire-crds + spire charts | Trust bundle for mTLS (optional) |

Envoy Gateway must be configured with the `extensionManager` pointing
to the AI Gateway controller's gRPC service:

```yaml
config:
  envoyGateway:
    extensionApis:
      enableBackend: true
    extensionManager:
      hooks:
        xdsTranslator:
          post: [Translation, Cluster, Route]
          translation:
            cluster:
              includeAll: true
            listener:
              includeAll: true
            route:
              includeAll: true
            secret:
              includeAll: true
      service:
        fqdn:
          hostname: ai-gateway-controller.<ns>.svc.cluster.local
          port: 1063
```

The `translation` block is required. Without it, Envoy Gateway
defaults to excluding listeners and routes from the
`PostTranslateModify` hook, and the AI Gateway extension server
cannot inject the ext_proc filter into the listener filter chain.

## Implementation plan

### Phase 1: AIRoutingPolicy

The minimum viable feature. A user creates a Gateway and an
AIRoutingPolicy; the controller generates the Envoy AI Gateway
resources to make inference routing work. Per-model rate limits and
per-model failover are included from the start since they share
generated resources with routing (AIGatewayRoute and
BackendTrafficPolicy) and map directly to the Envoy data plane model.

Files:
- `api/aigateway/v1alpha1/types.go` — shared types (targetRef, provider, model, backend, credentials, rateLimit)
- `api/aigateway/v1alpha1/airoutingpolicy_types.go`
- `internal/aigateway/intent.go` — data-plane-agnostic routing intent
- `internal/aigateway/envoy/renderer.go` — Envoy AI Gateway renderer
- `internal/aigateway/envoy/renderer_test.go`
- `internal/controller/airoutingpolicy_controller.go`
- `internal/controller/airoutingpolicy_controller_test.go`

### Phase 2: AIAccessPolicy

mTLS enforcement using SPIRE trust bundles. Adds the gateway-level
access control layer on top of routing.

Files:
- `api/aigateway/v1alpha1/aiaccesspolicy_types.go`
- `internal/controller/aiaccesspolicy_controller.go`
- `internal/spiffe/bundle.go` — SPIFFE JSON → PEM conversion

### Future: AIProvider extraction

If the provider spec grows significantly (credentials, processing
pipelines, additional metadata), it may warrant extraction into its own
CRD (`AIProvider`) referenced by name from AIRoutingPolicy. This would
enable provider reuse across gateways and finer-grained RBAC. The
internal `RoutingIntent` model should be structured so that this
extraction is a mechanical refactor.

### Future: payload processing

Per-provider and per-model guardrails via
[PayloadProcessingPipeline](#future-payloadprocessingpipeline).
Depends on the WG AI Gateway [Proposal 7: Payload Processing]
maturing.

## Compatibility notes

- AI Gateway v0.6.0 requires `AIServiceBackend.backendRef` to point to
  an Envoy Gateway `Backend` resource, not a raw Service.
  See: https://github.com/envoyproxy/ai-gateway/issues/902

- `AIGatewayRoute.backendRefs` must omit `group` and `kind` to default
  to `AIServiceBackend`. Explicit values are rejected unless they
  specify `InferencePool`.

- The AI Gateway CRDs emit deprecation warnings for `v1alpha1` in favor
  of `v1beta1`. The controller should target `v1alpha1` initially and
  migrate when the beta API stabilizes.

## References

- [Gateway API Policy Attachment (GEP-713)](https://gateway-api.sigs.k8s.io/geps/gep-713/)
- [WG AI Gateway](https://github.com/kubernetes-sigs/wg-ai-gateway)
- [WG AI Gateway Charter](https://github.com/kubernetes/community/blob/master/wg-ai-gateway/charter.md)
- [Proposal 10: Egress Gateways](https://github.com/kubernetes-sigs/wg-ai-gateway/blob/main/proposals/10-egress-gateways.md)
- [Proposal 7: Payload Processing](https://github.com/kubernetes-sigs/wg-ai-gateway/blob/main/proposals/7-payload-processing.md)
- [LiteLLM Router](https://docs.litellm.ai/docs/routing)
- [Envoy AI Gateway v0.6.0](https://aigateway.envoyproxy.io/)
- [Envoy Gateway extensionManager](https://gateway.envoyproxy.io/docs/tasks/extensibility/extension-server/)
