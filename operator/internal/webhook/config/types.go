package config

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// PlatformConfig represents the complete platform configuration.
//
// IMPORTANT: Instances returned by ConfigLoader.Get() are shared across
// concurrent webhook requests and MUST NOT be mutated. If you need to
// modify config values for a specific request, call DeepCopy() first.
type PlatformConfig struct {
	Images        ImageConfig           `json:"images" yaml:"images"`
	Proxy         ProxyConfig           `json:"proxy" yaml:"proxy"`
	Resources     ResourcesConfig       `json:"resources" yaml:"resources"`
	TokenExchange TokenExchangeDefaults `json:"tokenExchange" yaml:"tokenExchange"`
	Spiffe        SpiffeConfig          `json:"spiffe" yaml:"spiffe"`
	Observability ObservabilityConfig   `json:"observability" yaml:"observability"`
}

type ImageConfig struct {
	// EnvoyProxy is the combined image for envoy-sidecar mode:
	// Envoy + authbridge (ext_proc) + spiffe-helper bundled.
	// Spiffe-helper starts conditionally based on SPIRE_ENABLED.
	EnvoyProxy string `json:"envoyProxy" yaml:"envoyProxy"`

	// AuthBridge is the combined image for proxy-sidecar mode (default):
	// authbridge-proxy + spiffe-helper bundled. No Envoy, no gRPC.
	// Spiffe-helper starts conditionally based on SPIRE_ENABLED.
	AuthBridge string `json:"authbridge" yaml:"authbridge"`

	// AuthBridgeLite is the size-optimized variant of AuthBridge:
	// authbridge-lite (jwt-validation + token-exchange only, parsers
	// dropped) + spiffe-helper bundled. Same listener layout as
	// AuthBridge, used for the "lite" mode.
	AuthBridgeLite string `json:"authbridgeLite" yaml:"authbridgeLite"`

	// ProxyInit is the iptables init container, used by envoy-sidecar
	// mode only.
	ProxyInit string `json:"proxyInit" yaml:"proxyInit"`

	PullPolicy corev1.PullPolicy `json:"pullPolicy" yaml:"pullPolicy"`
}

type ProxyConfig struct {
	Port             int32 `json:"port" yaml:"port"`
	UID              int64 `json:"uid" yaml:"uid"`
	InboundProxyPort int32 `json:"inboundProxyPort" yaml:"inboundProxyPort"`
	AdminPort        int32 `json:"adminPort" yaml:"adminPort"`

	// TransparentPort is the forward proxy's transparent listener port — the
	// REDIRECT target the enforce-redirect proxy-init guard sends captured
	// external TCP egress to. It MUST match the authbridge proxy-sidecar
	// listener.transparent_proxy_addr (default :8082).
	TransparentPort int32 `json:"transparentPort" yaml:"transparentPort"`

	// IptablesCmd optionally pins the iptables backend the proxy-init script
	// uses, injected as the IPTABLES_CMD env var (omitted when empty). Empty
	// (default) lets the script auto-detect from /proc/modules (iptable_nat
	// loaded => legacy, as on Kind/kubeadm; absent => nft, as on OpenShift/
	// ROSA). Set to "iptables" (nft) or "iptables-legacy" to force a backend
	// where auto-detection is wrong or undesired.
	IptablesCmd string `json:"iptablesCmd" yaml:"iptablesCmd"`

	// AllowedEgressEnforcement restricts which egressEnforcement values
	// workloads (AgentRuntime CR / namespace ConfigMap) may select.
	// The webhook rejects resolved values not in this list, falling back
	// to the first entry. Default: ["enforce-redirect", "none"] (both
	// allowed). Set to ["enforce-redirect"] to prevent opt-out, or
	// ["none"] on platforms where iptables is unavailable.
	AllowedEgressEnforcement []string `json:"allowedEgressEnforcement,omitempty" yaml:"allowedEgressEnforcement,omitempty"`
}

type ResourcesConfig struct {
	EnvoyProxy corev1.ResourceRequirements `json:"envoyProxy" yaml:"envoyProxy"`
	ProxyInit  corev1.ResourceRequirements `json:"proxyInit" yaml:"proxyInit"`
	AuthBridge corev1.ResourceRequirements `json:"authbridge" yaml:"authbridge"`
}

type TokenExchangeDefaults struct {
	TokenURL        string   `json:"tokenUrl" yaml:"tokenUrl"`
	DefaultAudience string   `json:"defaultAudience" yaml:"defaultAudience"`
	DefaultScopes   []string `json:"defaultScopes" yaml:"defaultScopes"`
}

type SpiffeConfig struct {
	TrustDomain  string `json:"trustDomain" yaml:"trustDomain"`
	SocketPath   string `json:"socketPath" yaml:"socketPath"`
	HelperConfig string `json:"helperConfig" yaml:"helperConfig"`
}

type ObservabilityConfig struct {
	LogLevel      string `json:"logLevel" yaml:"logLevel"`
	EnableMetrics bool   `json:"enableMetrics" yaml:"enableMetrics"`
}

// DeepCopy creates a copy of the config
func (c *PlatformConfig) DeepCopy() *PlatformConfig {
	if c == nil {
		return nil
	}
	result := *c

	if c.TokenExchange.DefaultScopes != nil {
		result.TokenExchange.DefaultScopes = make([]string, len(c.TokenExchange.DefaultScopes))
		copy(result.TokenExchange.DefaultScopes, c.TokenExchange.DefaultScopes)
	}

	if c.Proxy.AllowedEgressEnforcement != nil {
		result.Proxy.AllowedEgressEnforcement = make([]string, len(c.Proxy.AllowedEgressEnforcement))
		copy(result.Proxy.AllowedEgressEnforcement, c.Proxy.AllowedEgressEnforcement)
	}

	// Deep copy ResourceRequirements — ResourceList is a map that would be shared
	result.Resources.EnvoyProxy = deepCopyResourceRequirements(c.Resources.EnvoyProxy)
	result.Resources.ProxyInit = deepCopyResourceRequirements(c.Resources.ProxyInit)
	result.Resources.AuthBridge = deepCopyResourceRequirements(c.Resources.AuthBridge)

	return &result
}

func deepCopyResourceRequirements(rr corev1.ResourceRequirements) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	if rr.Requests != nil {
		out.Requests = make(corev1.ResourceList, len(rr.Requests))
		for k, v := range rr.Requests {
			out.Requests[k] = v.DeepCopy()
		}
	}
	if rr.Limits != nil {
		out.Limits = make(corev1.ResourceList, len(rr.Limits))
		for k, v := range rr.Limits {
			out.Limits[k] = v.DeepCopy()
		}
	}
	return out
}

// Validate checks if the config is valid
func (c *PlatformConfig) Validate() error {
	if c.Proxy.Port < 1024 || c.Proxy.Port > 65535 {
		return fmt.Errorf("proxy.port must be between 1024 and 65535")
	}
	if c.Proxy.InboundProxyPort < 1024 || c.Proxy.InboundProxyPort > 65535 {
		return fmt.Errorf("proxy.inboundProxyPort must be between 1024 and 65535")
	}
	if c.Proxy.AdminPort < 1024 || c.Proxy.AdminPort > 65535 {
		return fmt.Errorf("proxy.adminPort must be between 1024 and 65535")
	}
	if c.Proxy.TransparentPort < 1024 || c.Proxy.TransparentPort > 65535 {
		return fmt.Errorf("proxy.transparentPort must be between 1024 and 65535")
	}
	// The enforce-redirect guard exempts this UID (--uid-owner) and the proxy
	// container runs as it; it must be a real non-root user.
	if c.Proxy.UID < 1 {
		return fmt.Errorf("proxy.uid must be >= 1 (got %d): the proxy must not run as root and the egress-enforcement exemption keys on this UID", c.Proxy.UID)
	}
	// IptablesCmd, when set, pins the proxy-init iptables backend (IPTABLES_CMD).
	// Restrict overrides to the binaries shipped in the proxy-init image so a
	// chart typo fails fast at operator startup rather than as a per-injected-pod
	// init crash. Empty is the default — proxy-init auto-detects from /proc/modules.
	switch c.Proxy.IptablesCmd {
	case "", "iptables", "iptables-nft", "iptables-legacy":
	default:
		return fmt.Errorf("proxy.iptablesCmd %q is not a recognized backend (want one of: \"\" (auto-detect), iptables, iptables-nft, iptables-legacy)", c.Proxy.IptablesCmd)
	}
	if len(c.Proxy.AllowedEgressEnforcement) == 0 {
		return fmt.Errorf("proxy.allowedEgressEnforcement must not be empty (set [\"enforce-redirect\"] to require enforcement, [\"none\"] to disable it, or both to allow workload choice)")
	}
	for _, mode := range c.Proxy.AllowedEgressEnforcement {
		if mode != "enforce-redirect" && mode != "none" {
			return fmt.Errorf("proxy.allowedEgressEnforcement contains invalid value %q (allowed: enforce-redirect, none)", mode)
		}
	}
	if c.Images.EnvoyProxy == "" {
		return fmt.Errorf("images.envoyProxy is required")
	}
	if c.Images.AuthBridge == "" {
		return fmt.Errorf("images.authbridge is required")
	}
	if c.Images.AuthBridgeLite == "" {
		return fmt.Errorf("images.authbridgeLite is required")
	}
	if c.Images.ProxyInit == "" {
		return fmt.Errorf("images.proxyInit is required")
	}
	return nil
}
