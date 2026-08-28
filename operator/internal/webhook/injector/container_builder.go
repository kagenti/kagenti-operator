/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package injector

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/rossoctl/operator/internal/webhook/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var builderLog = logf.Log.WithName("container-builder")

const (
	// Container names for AuthBridge sidecars
	EnvoyProxyContainerName = "envoy-proxy"
	ProxyInitContainerName  = "proxy-init"

	SharedVolumesFSGroup = 0
)

// ContainerBuilder creates container specs from resolved config.
// It supports two modes:
//   - Legacy mode: constructed with NewContainerBuilder(platformConfig) — uses
//     ValueFrom refs for env vars (backward compatible)
//   - Resolved mode: constructed with NewResolvedContainerBuilder(resolvedConfig)
//     — uses literal env var values read at admission time
type ContainerBuilder struct {
	cfg      *config.PlatformConfig
	resolved *ResolvedConfig
}

// NewContainerBuilder creates a ContainerBuilder that uses ValueFrom refs
// for environment variables (legacy behavior).
func NewContainerBuilder(cfg *config.PlatformConfig) *ContainerBuilder {
	if cfg == nil {
		cfg = config.CompiledDefaults()
	}
	return &ContainerBuilder{cfg: cfg}
}

// NewResolvedContainerBuilder creates a ContainerBuilder that uses literal
// env var values from the resolved config (admission-time resolution).
func NewResolvedContainerBuilder(resolved *ResolvedConfig) *ContainerBuilder {
	if resolved == nil {
		resolved = ResolveConfig(nil, nil)
	}
	return &ContainerBuilder{
		cfg:      resolved.Platform,
		resolved: resolved,
	}
}

// BuildEnvoyProxyContainer creates the envoy-proxy sidecar container with SPIRE enabled (default).
func (b *ContainerBuilder) BuildEnvoyProxyContainer() corev1.Container {
	return b.BuildEnvoyProxyContainerWithSpireOption(true)
}

// BuildEnvoyProxyContainerWithSpireOption creates the envoy-proxy sidecar container.
// When spireEnabled is true, the svid-output volume is mounted (read-only) so the
// go-processor can read the SPIFFE JWT SVID for use as a subject token in RFC 8693
// token exchange on outbound requests.
func (b *ContainerBuilder) BuildEnvoyProxyContainerWithSpireOption(spireEnabled bool) corev1.Container {
	builderLog.Info("building EnvoyProxy Container", "spireEnabled", spireEnabled)

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "envoy-config",
			MountPath: "/etc/envoy",
			ReadOnly:  true,
		},
		{
			// Not ReadOnly: subPath mounts of /shared/client-id.txt and
			// /shared/client-secret.txt (added later by
			// ApplyKeycloakClientCredentialsSecretVolumes) need to create
			// their targets inside this mount. The combined authbridge
			// images use a read-only base (ubi9-micro), so /shared must
			// be mounted RW for runc to create the subPath mountpoints.
			Name:      "shared-data",
			MountPath: "/shared",
		},
		{
			Name:      "authproxy-routes",
			MountPath: AuthProxyMountPath,
			ReadOnly:  true,
		},
		{
			Name:      "authbridge-runtime-config",
			MountPath: "/etc/authbridge",
			ReadOnly:  true,
		},
	}
	if spireEnabled {
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				Name:      "svid-output",
				MountPath: "/opt",
				// Must be RW: the in-process spiffe Provider mirror inside the
				// authbridge-envoy combined image writes /opt/svid.pem,
				// /opt/svid_key.pem, /opt/svid_bundle.pem on every SPIRE
				// rotation. Envoy's file-based DownstreamTlsContext /
				// UpstreamTlsContext (used when mtlsMode != disabled) reads
				// these files. The proxy-sidecar mount in
				// BuildProxySidecarContainerWithPorts correctly defaults to RW;
				// this branch was historically RO from the days of an external
				// spiffe-helper writer, which no longer applies.
			},
			// authbridge-envoy bundles spiffe-helper; the entrypoint reads
			// helper.conf from this mount. Without it, the bundled
			// spiffe-helper would fail to start on SPIRE_ENABLED=true.
			corev1.VolumeMount{
				Name:      "spiffe-helper-config",
				MountPath: "/etc/spiffe-helper",
				ReadOnly:  true,
			},
			// SPIRE workload-API socket — bundled spiffe-helper dials it.
			// Path derived from SpiffeConfig.SocketPath (defaults.go).
			corev1.VolumeMount{
				Name:      "spire-agent-socket",
				MountPath: spireSocketDir(b.cfg.Spiffe.SocketPath),
				ReadOnly:  true,
			},
		)
	}

	var env []corev1.EnvVar
	if b.resolved != nil {
		env = b.buildEnvoyProxyEnvResolved()
	} else {
		env = b.buildEnvoyProxyEnvLegacy()
	}
	// SPIRE_ENABLED gates the bundled spiffe-helper inside the
	// combined image's entrypoint. Always set explicitly so the
	// container's behavior is deterministic regardless of the image's
	// own default.
	env = append(env, corev1.EnvVar{
		Name:  "SPIRE_ENABLED",
		Value: spireEnabledStr(spireEnabled),
	})

	return corev1.Container{
		Name:            EnvoyProxyContainerName,
		Image:           b.cfg.Images.EnvoyProxy,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Args:            []string{"--config", "/etc/authbridge/config.yaml"},
		Resources:       b.cfg.Resources.EnvoyProxy,
		Ports: []corev1.ContainerPort{
			{
				Name:          "envoy-outbound",
				ContainerPort: b.cfg.Proxy.Port,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-inbound",
				ContainerPort: b.cfg.Proxy.InboundProxyPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-admin",
				ContainerPort: b.cfg.Proxy.AdminPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "ext-proc",
				ContainerPort: 9090,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(b.cfg.Proxy.UID),
			RunAsGroup:               ptr.To(b.cfg.Proxy.UID),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
		},
		VolumeMounts: volumeMounts,
	}
}

func spireEnabledStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// spireSocketDir returns the directory portion of the SPIRE workload-API
// socket path (e.g. "unix:///spiffe-workload-api/spire-agent.sock" →
// "/spiffe-workload-api"), suitable for use as a container mountPath.
// Single source of truth: defaults.go's SpiffeConfig.SocketPath.
func spireSocketDir(socketPath string) string {
	stripped := strings.TrimPrefix(socketPath, "unix://")
	dir := path.Dir(stripped)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// BuildProxySidecarContainer creates a combined authbridge container for proxy-sidecar mode.
// Uses the authbridge image (authbridge-proxy + spiffe-helper bundled, no Envoy).
// The app uses HTTP_PROXY env vars to route outbound traffic through the forward proxy.
// Inbound traffic goes through the reverse proxy.
func (b *ContainerBuilder) BuildProxySidecarContainer(spireEnabled bool) corev1.Container {
	return b.BuildProxySidecarContainerWithPorts(spireEnabled, b.cfg.Images.AuthBridge, 8080, 8081)
}

// BuildProxySidecarContainerWithPorts creates a proxy-sidecar container with dynamic ports.
// image:            container image to run — Images.AuthBridge (full plugin set) or
//
//	Images.AuthBridgeLite (auth-only). Both images expose the same listeners
//	on the same ports; only the plugin set compiled into the binary differs.
//
// reverseProxyPort: where the reverse proxy listens (takes over the agent's original port)
// forwardProxyPort: where the forward proxy listens (HTTP_PROXY target)
func (b *ContainerBuilder) BuildProxySidecarContainerWithPorts(spireEnabled bool, image string, reverseProxyPort, forwardProxyPort int32) corev1.Container {
	// No agent-backend port parameter: where the agent was relocated to is
	// something the sidecar learns from its ConfigMap (reverse_proxy_backend), not
	// from a declared container port.
	return b.buildProxySidecarContainer(spireEnabled, image, "reverse-proxy", reverseProxyPort, forwardProxyPort)
}

// BuildProxySidecarContainerTransparent builds the sidecar for
// inboundInterception "transparent": AuthBridge binds its own inbound listener
// on transparentInboundPort and iptables REDIRECTs to it, so the agent keeps its
// own port and there is no relocated backend port to declare.
//
// The declared port is named "transparent-in" rather than "reverse-proxy" so
// `kubectl describe pod` reflects which inbound shape is actually running —
// otherwise the two are indistinguishable from the pod spec.
func (b *ContainerBuilder) BuildProxySidecarContainerTransparent(spireEnabled bool, image string, transparentInboundPort, forwardProxyPort int32) corev1.Container {
	// No backend port: with transparent interception the forwarding target is
	// resolved per connection from SO_ORIGINAL_DST, not configured.
	return b.buildProxySidecarContainer(spireEnabled, image, "transparent-in", transparentInboundPort, forwardProxyPort)
}

func (b *ContainerBuilder) buildProxySidecarContainer(spireEnabled bool, image, inboundPortName string, inboundPort, forwardProxyPort int32) corev1.Container {
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "shared-data",
			MountPath: "/shared",
		},
		{
			Name:      "authbridge-runtime-config",
			MountPath: "/etc/authbridge",
			ReadOnly:  true,
		},
		{
			Name:      AuthproxyRoutesConfigMapName,
			MountPath: AuthProxyMountPath,
			ReadOnly:  true,
		},
	}
	if spireEnabled {
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			// authbridge bundles spiffe-helper; the entrypoint reads
			// helper.conf from this mount. Without it, the bundled
			// spiffe-helper would fail to start on SPIRE_ENABLED=true.
			corev1.VolumeMount{
				Name:      "spiffe-helper-config",
				MountPath: "/etc/spiffe-helper",
				ReadOnly:  true,
			},
			// SPIRE workload-API socket — bundled spiffe-helper dials it.
			// Path derived from SpiffeConfig.SocketPath (defaults.go).
			corev1.VolumeMount{
				Name:      "spire-agent-socket",
				MountPath: spireSocketDir(b.cfg.Spiffe.SocketPath),
				ReadOnly:  true,
			},
		)
	}

	return corev1.Container{
		Name:            AuthBridgeProxyContainerName,
		Image:           image,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Args: []string{
			"--config", "/etc/authbridge/config.yaml",
		},
		Env: []corev1.EnvVar{
			// Gates the bundled spiffe-helper inside the combined image.
			{Name: "SPIRE_ENABLED", Value: spireEnabledStr(spireEnabled)},
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          inboundPortName,
				ContainerPort: inboundPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "forward-proxy",
				ContainerPort: forwardProxyPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: b.cfg.Resources.AuthBridge,
		SecurityContext: &corev1.SecurityContext{
			// Run as the dedicated proxy UID (default 1337), the SAME value the
			// proxy-init enforce-redirect guard exempts via `--uid-owner $PROXY_UID`.
			// Deriving both from b.cfg.Proxy.UID keeps the exempted UID in lockstep
			// with the process it exempts (a hardcoded literal could drift and
			// silently break the proxy's own egress). Matches the envoy builder.
			RunAsUser:                ptr.To(b.cfg.Proxy.UID),
			RunAsGroup:               ptr.To(b.cfg.Proxy.UID),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
		},
		VolumeMounts: volumeMounts,
	}
}

// buildEnvoyProxyEnvResolved returns literal env vars from resolved config.
func (b *ContainerBuilder) buildEnvoyProxyEnvResolved() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "KEYCLOAK_URL", Value: b.resolved.KeycloakURL},
		{Name: "KEYCLOAK_REALM", Value: b.resolved.KeycloakRealm},
		{Name: "TOKEN_URL", Value: b.resolved.TokenURL},
		{Name: "ISSUER", Value: b.resolved.Issuer},
		{Name: "EXPECTED_AUDIENCE", Value: b.resolved.ExpectedAudience},
		{Name: "TARGET_AUDIENCE", Value: b.resolved.TargetAudience},
		{Name: "TARGET_SCOPES", Value: b.resolved.TargetScopes},
		{Name: "CLIENT_ID_FILE", Value: "/shared/client-id.txt"},
		{Name: "CLIENT_SECRET_FILE", Value: "/shared/client-secret.txt"},
		{Name: "ROUTES_CONFIG_PATH", Value: AuthProxyRoutesFile},
		{Name: "DEFAULT_OUTBOUND_POLICY", Value: b.resolved.DefaultOutboundPolicy},
	}
}

// buildEnvoyProxyEnvLegacy returns ValueFrom-based env vars (backward compat).
func (b *ContainerBuilder) buildEnvoyProxyEnvLegacy() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name: "KEYCLOAK_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_URL",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "KEYCLOAK_REALM",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_REALM",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TOKEN_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TOKEN_URL",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "ISSUER",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "ISSUER",
					Optional:             ptr.To(false),
				},
			},
		},
		{
			Name: "EXPECTED_AUDIENCE",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "EXPECTED_AUDIENCE",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TARGET_AUDIENCE",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TARGET_AUDIENCE",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TARGET_SCOPES",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TARGET_SCOPES",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name:  "CLIENT_ID_FILE",
			Value: "/shared/client-id.txt",
		},
		{
			Name:  "CLIENT_SECRET_FILE",
			Value: "/shared/client-secret.txt",
		},
		{
			Name:  "ROUTES_CONFIG_PATH",
			Value: AuthProxyRoutesFile,
		},
		{
			Name: "DEFAULT_OUTBOUND_POLICY",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "DEFAULT_OUTBOUND_POLICY",
					Optional:             ptr.To(true),
				},
			},
		},
	}
}

// BuildProxyInitContainer creates the init container that sets up iptables
// to redirect outbound traffic to the Envoy proxy.
//
// SECURITY NOTE: This init container requires elevated capabilities:
//   - RunAsUser: 0 (root) - Required to modify network namespace iptables rules
//   - RunAsNonRoot: false - Explicitly allows root execution
//   - NET_ADMIN capability - Required for iptables manipulation
//   - NET_RAW capability - Required for raw socket operations used by iptables
//
// The init container does NOT require privileged mode. It uses DNAT to the pod's
// own IP instead of REDIRECT for the ztunnel inbound interception rule, which
// avoids the need for sysctl route_localnet=1 (which would require privileged
// mode to write to read-only /proc/sys). All other capabilities are dropped.
//
// Risk mitigations:
//   - This runs as an init container (not a long-running sidecar), limiting exposure window
//   - The container exits immediately after configuring iptables rules
//   - Minimal resource limits are applied (10m CPU, 10Mi memory)
//   - Only NET_ADMIN and NET_RAW capabilities are granted (all others dropped)
//   - The container image should be regularly updated and scanned for vulnerabilities
//
// mandatoryOutboundExclude is always prepended so that Keycloak traffic
// (port 8080) is never intercepted by Envoy.
const mandatoryOutboundExclude = "8080"

// BuildProxyInitContainer creates the proxy-init container.
// mode selects the interception strategy the init script runs:
//   - "redirect" (envoy-sidecar): transparently REDIRECT pod traffic to the
//     Envoy listeners. outboundPortsExclude / inboundPortsExclude (from the
//     rossoctl.io/outbound-ports-exclude and rossoctl.io/inbound-ports-exclude
//     pod annotations; mandatory 8080 always included outbound) tune it.
//   - "enforce-redirect" (proxy-sidecar): a fail-closed egress guard that
//     REDIRECTs external TCP bypassing the forward proxy to the transparent
//     listener (TRANSPARENT_PORT) and DROPs non-TCP. Driven by PROXY_UID +
//     TRANSPARENT_PORT; the exclude args do not apply. Cluster DNS is kept
//     direct by the init script reading the pod's resolv.conf nameservers.
func (b *ContainerBuilder) BuildProxyInitContainer(mode ProxyInitMode, outboundPortsExclude, inboundPortsExclude string) corev1.Container {
	return b.BuildProxyInitContainerWithInbound(mode, outboundPortsExclude, inboundPortsExclude, ProxyInitInbound{})
}

// ProxyInitInbound carries the transparent-inbound additions to proxy-init's
// enforce-redirect mode. The zero value leaves inbound interception off, which
// is what BuildProxyInitContainer passes — so existing callers keep egress-only
// behavior with no change.
type ProxyInitInbound struct {
	// Port is the PREROUTING REDIRECT target (INBOUND_TRANSPARENT_PORT). Zero
	// means inbound interception is off.
	Port int32

	// SidecarPortsExclude is the comma-separated list of AuthBridge's own
	// listeners to exempt from the inbound REDIRECT. The operator supplies the
	// real forward-proxy port here, which may not be the script's default 8081
	// when findFreePort had to move it.
	SidecarPortsExclude string

	// PortsExclude is the operator/user app-port exemption list
	// (kagenti.io/inbound-ports-exclude), for app ports that must not be
	// validated — e.g. an oauth-proxy doing its own authentication.
	PortsExclude string
}

// BuildProxyInitContainerWithInbound is BuildProxyInitContainer plus the
// transparent-inbound environment. Split out rather than folded into the
// existing signature so the redirect-mode and egress-only callers stay
// untouched.
func (b *ContainerBuilder) BuildProxyInitContainerWithInbound(mode ProxyInitMode, outboundPortsExclude, inboundPortsExclude string, inbound ProxyInitInbound) corev1.Container {
	var env []corev1.EnvVar
	switch mode {
	case ProxyInitModeEnforceRedirect:
		// PROXY_UID is exempted (its egress is not redirected) and MUST match the
		// proxy container's RunAsUser (both derive from b.cfg.Proxy.UID). External
		// TCP is REDIRECTed to TRANSPARENT_PORT (the proxy's transparent listener),
		// external non-TCP is dropped. Cluster DNS is left direct by the init script
		// itself (it reads the pod's resolv.conf nameservers — no CIDR knob needed).
		// The redirect-only exclude vars are unused in this mode.
		env = []corev1.EnvVar{
			{Name: "MODE", Value: string(ProxyInitModeEnforceRedirect)},
			{Name: "PROXY_UID", Value: fmt.Sprintf("%d", b.cfg.Proxy.UID)},
			{Name: "TRANSPARENT_PORT", Value: fmt.Sprintf("%d", b.cfg.Proxy.TransparentPort)},
		}
		// Transparent inbound (opt-in). POD_IP is REQUIRED alongside the port:
		// the init script uses it as the DNAT target for the Istio ambient inbound
		// path, which arrives through OUTPUT rather than PREROUTING. The script
		// refuses to start without it rather than install PREROUTING-only rules
		// that would wave all mesh traffic through unvalidated.
		if inbound.Port > 0 {
			env = append(env,
				corev1.EnvVar{Name: "INBOUND_TRANSPARENT_PORT", Value: fmt.Sprintf("%d", inbound.Port)},
				corev1.EnvVar{
					Name: "POD_IP",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
					},
				},
				// POD_IPS carries BOTH families on a dual-stack pod. POD_IP alone is
				// the primary address (usually v4), and the ambient DNAT target must
				// match the family of the traffic — so keying only off POD_IP leaves
				// the other family's HBONE delivery passing unvalidated while its
				// PREROUTING rules are installed. proxy-init falls back to POD_IP
				// when this is absent.
				corev1.EnvVar{
					Name: "POD_IPS",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIPs"},
					},
				},
			)
			if inbound.SidecarPortsExclude != "" {
				env = append(env, corev1.EnvVar{Name: "SIDECAR_PORTS_EXCLUDE", Value: inbound.SidecarPortsExclude})
			}
			if v := buildPortExcludeValue(inbound.PortsExclude, "inbound-ports-exclude"); v != "" {
				env = append(env, corev1.EnvVar{Name: "INBOUND_PORTS_EXCLUDE", Value: v})
			}
		}
		builderLog.Info("building ProxyInit Container",
			"mode", "enforce-redirect",
			"proxyUID", b.cfg.Proxy.UID,
			"transparentPort", b.cfg.Proxy.TransparentPort,
			"inboundTransparentPort", inbound.Port,
			"sidecarPortsExclude", inbound.SidecarPortsExclude)
	case ProxyInitModeRedirect:
		outboundValue := buildOutboundExcludeValue(outboundPortsExclude)
		inboundValue := buildPortExcludeValue(inboundPortsExclude, "inbound-ports-exclude")

		builderLog.Info("building ProxyInit Container",
			"mode", "redirect",
			"resolvedOutboundPortsExclude", outboundValue,
			"resolvedInboundPortsExclude", inboundValue)

		env = []corev1.EnvVar{
			{
				Name:  "PROXY_PORT",
				Value: fmt.Sprintf("%d", b.cfg.Proxy.Port),
			},
			{
				Name:  "INBOUND_PROXY_PORT",
				Value: fmt.Sprintf("%d", b.cfg.Proxy.InboundProxyPort),
			},
			{
				Name:  "PROXY_UID",
				Value: fmt.Sprintf("%d", b.cfg.Proxy.UID),
			},
			{
				Name:  "OUTBOUND_PORTS_EXCLUDE",
				Value: outboundValue,
			},
			{
				Name: "POD_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "status.podIP",
					},
				},
			},
		}
		if inboundValue != "" {
			env = append(env, corev1.EnvVar{
				Name:  "INBOUND_PORTS_EXCLUDE",
				Value: inboundValue,
			})
		}
	default:
		// Fail closed. An unknown mode must NOT silently degrade to redirect:
		// that would ship a proxy-init with no egress guard — fail-open, the
		// worst outcome for a fail-closed control. Return a zero-value container
		// so injection breaks visibly (empty name/image → admission/scheduler
		// rejects it) rather than passing a security-defeating container through.
		builderLog.Error(nil, "unknown proxy-init mode; refusing to build container (fail closed)", "mode", mode)
		return corev1.Container{}
	}

	// Optional explicit iptables backend override (applies to both modes). Empty
	// by default: the init script auto-detects from /proc/modules (iptable_nat
	// loaded => legacy, else nft). Set b.cfg.Proxy.IptablesCmd (e.g. "iptables"
	// on nft-only platforms) to force a backend.
	if b.cfg.Proxy.IptablesCmd != "" {
		env = append(env, corev1.EnvVar{Name: "IPTABLES_CMD", Value: b.cfg.Proxy.IptablesCmd})
	}

	return corev1.Container{
		Name:            ProxyInitContainerName,
		Image:           b.cfg.Images.ProxyInit,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.ProxyInit,
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr.To(int64(0)),
			RunAsNonRoot: ptr.To(false),
			Privileged:   ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}
}

// validateAndDeduplicatePorts parses a comma-separated port string, validates
// each token (numeric, 1-65535), deduplicates, and returns the clean list.
// initialPorts are prepended and excluded from duplicates.
func validateAndDeduplicatePorts(raw, annotationName string, initialPorts []string) []string {
	seen := map[string]bool{}
	ports := make([]string, 0, len(initialPorts)+4)
	for _, p := range initialPorts {
		seen[p] = true
		ports = append(ports, p)
	}

	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		p, err := strconv.Atoi(tok)
		if err != nil || p < 1 || p > 65535 {
			builderLog.V(0).Info("WARNING: ignoring invalid port in "+annotationName+" annotation", "value", tok)
			continue
		}
		normalized := strconv.Itoa(p)
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		ports = append(ports, normalized)
	}
	return ports
}

// buildOutboundExcludeValue merges the mandatory 8080 with validated
// user-supplied ports. Invalid tokens (non-numeric, out of range) are
// silently dropped and logged. Duplicates of 8080 are removed.
func buildOutboundExcludeValue(extra string) string {
	if extra == "" {
		return mandatoryOutboundExclude
	}
	return strings.Join(validateAndDeduplicatePorts(extra, "outbound-ports-exclude", []string{mandatoryOutboundExclude}), ",")
}

// buildPortExcludeValue validates and deduplicates a comma-separated port
// list. Returns "" when the input is empty. Used for inbound port exclusion
// where there is no mandatory port.
func buildPortExcludeValue(raw, annotationName string) string {
	if raw == "" {
		return ""
	}
	return strings.Join(validateAndDeduplicatePorts(raw, annotationName, nil), ",")
}
