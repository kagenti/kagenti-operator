package injector

// Label constants used by the precedence evaluator.
const (
	// Per-sidecar workload labels — set value to "false" to disable injection
	LabelEnvoyProxyInject   = "rossoctl.io/envoy-proxy-inject"
	LabelSpiffeHelperInject = "rossoctl.io/spiffe-helper-inject"
)

// AuthBridge deployment modes. Selected via the namespace
// `authbridge-runtime-config` ConfigMap's `mode` field, the deprecated
// per-pod annotation, then ModeProxySidecar as the cluster-wide default.
const (
	ModeEnvoySidecar = "envoy-sidecar" // iptables + Envoy + ext_proc
	ModeProxySidecar = "proxy-sidecar" // default: HTTP_PROXY env + authbridge proxy (full plugins)
	ModeLite         = "lite"          // same shape as proxy-sidecar; uses authbridge-lite image (auth-only)
	ModeWaypoint     = "waypoint"      // standalone deployment (not injected)

	// AnnotationAuthBridgeMode is the legacy per-pod mode selector. The
	// canonical surface is the namespace authbridge-runtime-config
	// ConfigMap; this annotation is only honored as a deprecated fallback
	// so existing deployments do not silently shape-shift to a different
	// mode on first redeploy.
	//
	// Deprecated: set mode in the namespace authbridge-runtime-config ConfigMap.
	AnnotationAuthBridgeMode = "rossoctl.io/authbridge-mode"

	// Container name for proxy-sidecar mode
	AuthBridgeProxyContainerName = "authbridge-proxy"

	// Identity type constants
	IdentityTypeSpiffe         = "spiffe"
	ClientAuthTypeFederatedJWT = "federated-jwt"
)

// ProxyInitMode is the iptables strategy BuildProxyInitContainer passes to
// init-iptables.sh. It is a named type so callsites read intent-fully and the
// compiler rejects passing an unrelated typed value; BuildProxyInitContainer
// additionally fails closed on any unknown value (see its default branch),
// since an untyped string literal can still convert to this type at a callsite.
type ProxyInitMode string

// proxy-init MODE values.
const (
	// ProxyInitModeRedirect transparently REDIRECTs pod traffic to the Envoy
	// listeners (envoy-sidecar mode).
	ProxyInitModeRedirect ProxyInitMode = "redirect"
	// ProxyInitModeEnforceRedirect installs the fail-closed egress guard that
	// REDIRECTs external TCP bypassing the forward proxy to AuthBridge's
	// transparent listener (captured, not dropped) and DROPs non-TCP external
	// egress. Always-on for proxy-sidecar / lite.
	ProxyInitModeEnforceRedirect ProxyInitMode = "enforce-redirect"
)

// Egress enforcement modes for proxy-sidecar / lite paths.
// Controls whether proxy-init is injected for fail-closed egress capture.
const (
	// EgressEnforcementEnforceRedirect injects proxy-init with iptables
	// rules (default). Requires NET_ADMIN and a kernel with iptables support.
	EgressEnforcementEnforceRedirect = "enforce-redirect"

	// EgressEnforcementNone skips proxy-init injection. Egress relies on
	// HTTP_PROXY (cooperative) + inbound AuthBridge + NetworkPolicy.
	// Use on platforms where iptables is unavailable (ROSA HCP, managed OpenShift).
	EgressEnforcementNone = "none"
)

// AuthBridge's fixed (non-negotiable) listener ports in proxy-sidecar / lite.
// Unlike the reverse/forward/transparent ports these are not configurable, so
// the operator must exempt them from the transparent inbound REDIRECT by number.
// Gating AuthBridgeHealthPort in particular would put kubelet probes behind JWT
// validation and crash-loop the pod.
const (
	authBridgeHealthPort     = 9091 // /healthz
	authBridgeStatsPort      = 9093 // stats + config inspection + /reload/status
	authBridgeSessionAPIPort = 9094 // session events API (consumed by abctl)
)

// Inbound interception mechanisms for the proxy-sidecar / lite paths. These
// select HOW inbound traffic reaches AuthBridge's inbound pipeline — they are
// not two levels of the same knob, but two different deployment shapes.
const (
	// InboundInterceptionReverseProxy is the default: port stealing. AuthBridge
	// binds the agent's original port and the agent is relocated to a free one
	// via the PORT env var, so the Service needs no patching. Requires no
	// privileges, but leaves the relocated port reachable directly (a pod-to-pod
	// bypass of JWT validation), only covers the first declared container port,
	// and silently fails for agents that hardcode their listen port.
	InboundInterceptionReverseProxy = "reverse-proxy"

	// InboundInterceptionTransparent installs a PREROUTING REDIRECT via
	// proxy-init and lets AuthBridge recover each connection's real destination
	// via SO_ORIGINAL_DST. The agent keeps its own port — no relocation, no PORT
	// env var, no second port to discover — and every port it listens on is
	// covered. Costs a privileged proxy-init container (NET_ADMIN) and is
	// Linux-only, so it is opt-in.
	InboundInterceptionTransparent = "transparent"
)

// mTLS modes for the proxy-sidecar / lite paths. Selected via the
// namespace `authbridge-runtime-config` ConfigMap's `mtls.mode` field,
// then MTLSModeDisabled. envoy-sidecar mode is incompatible with mTLS
// today (Envoy SDS not configured by the rossoctl envoy-config) —
// admission rejects mtlsMode != disabled in that combination.
const (
	MTLSModeDisabled   = "disabled"
	MTLSModePermissive = "permissive"
	MTLSModeStrict     = "strict"
)

const (
	// TLSBridgeCAVolumeName is the Secret-backed volume carrying the FULL
	// per-agent cert-manager CA keypair (tls.crt + tls.key + ca.crt). It is
	// mounted ONLY into the authbridge sidecar, which needs tls.key to mint
	// leaves. It must never be mounted into the agent container — the key has
	// no Name Constraints, so an agent holding it could forge a cert for any
	// host. The mode values + secret-name suffix are the shared contract and
	// live in api/v1alpha1 (TLSBridgeMode*, TLSBridgeCASecretSuffix).
	TLSBridgeCAVolumeName = "tls-bridge-ca"
	// TLSBridgeCACertVolumeName carries ONLY ca.crt (projected from the same
	// Secret) and is mounted into agent containers so they trust the bridge's
	// minted leaves — without ever seeing the private key.
	TLSBridgeCACertVolumeName = "tls-bridge-ca-cert"
	TLSBridgeCAMountPath      = "/etc/authbridge/tls-bridge-ca"
)
