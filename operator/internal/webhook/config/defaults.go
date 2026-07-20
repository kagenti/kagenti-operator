package config

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultSpiffeHelperConfig is the default helper.conf content for spiffe-helper.
// Keep in sync with charts/operator/values.yaml defaults.spiffe.helperConfig.
// The jwt_audience below targets a local dev Keycloak. Production and OpenShift
// deployments MUST override this via Helm values (defaults.spiffe.helperConfig)
// to set the in-cluster Keycloak audience.
const DefaultSpiffeHelperConfig = `agent_address = "/spiffe-workload-api/spire-agent.sock"
cmd = ""
cmd_args = ""
svid_file_name = "/opt/svid.pem"
svid_key_file_name = "/opt/svid_key.pem"
svid_bundle_file_name = "/opt/svid_bundle.pem"
cert_file_mode = 0644
key_file_mode = 0640
jwt_svids = [{jwt_audience="http://keycloak.localtest.me:8080/realms/rossoctl", jwt_svid_file_name="/opt/jwt_svid.token"}]
jwt_svid_file_mode = 0644
include_federated_domains = true
`

// CompiledDefaults returns hardcoded defaults used when no config is provided
func CompiledDefaults() *PlatformConfig {
	return &PlatformConfig{
		// Compiled defaults are overridden at runtime by the platform-config
		// ConfigMap (rossoctl-platform-config). These serve as fallbacks only.
		Images: ImageConfig{
			// authbridge-envoy: combined image for envoy-sidecar mode
			// (Envoy + ext_proc authbridge + spiffe-helper bundled).
			EnvoyProxy: "ghcr.io/rossoctl/cortex/authbridge-envoy:latest",
			// authbridge: combined image for proxy-sidecar mode (default
			// deployment shape) — authbridge-proxy + spiffe-helper
			// bundled, no Envoy, no gRPC.
			AuthBridge: "ghcr.io/rossoctl/cortex/authbridge:latest",
			// authbridge-lite: size-optimized variant for the "lite"
			// mode. Same listener layout as AuthBridge but parsers
			// (a2a/mcp/inference) are dropped.
			AuthBridgeLite: "ghcr.io/rossoctl/cortex/authbridge-lite:latest",
			// proxy-init: iptables init container, used by
			// envoy-sidecar mode only.
			ProxyInit:  "ghcr.io/rossoctl/cortex/proxy-init:latest",
			PullPolicy: corev1.PullIfNotPresent,
		},
		Proxy: ProxyConfig{
			Port:             15123,
			UID:              1337,
			InboundProxyPort: 15124,
			AdminPort:        9901,
			// Transparent listener port — must match the authbridge proxy-sidecar
			// preset (listener.transparent_proxy_addr default :8082).
			TransparentPort: 8082,
			// Empty by default: proxy-init auto-detects the iptables backend from
			// /proc/modules. Set (e.g. "iptables") to force a backend per-platform.
			IptablesCmd: "",
			// Both modes allowed by default. Set to ["none"] on platforms
			// where iptables is unavailable (ROSA HCP, managed OpenShift),
			// or ["enforce-redirect"] to prevent opt-out.
			AllowedEgressEnforcement: []string{"enforce-redirect", "none"},
		},
		Resources: ResourcesConfig{
			EnvoyProxy: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			ProxyInit: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("10Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("10Mi"),
				},
			},
			AuthBridge: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("300m"),
					corev1.ResourceMemory: resource.MustParse("384Mi"),
				},
			},
		},
		TokenExchange: TokenExchangeDefaults{
			DefaultScopes: []string{"openid"},
		},
		Spiffe: SpiffeConfig{
			TrustDomain:  "cluster.local",
			SocketPath:   "unix:///spiffe-workload-api/spire-agent.sock",
			HelperConfig: DefaultSpiffeHelperConfig,
		},
		Observability: ObservabilityConfig{
			LogLevel:      "info",
			EnableMetrics: true,
		},
	}
}
