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
	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// tlsBridgeTrustEnvVars are the CA-bundle environment variables honored by the
// TLS stacks common to agent workloads (Node.js, OpenSSL, Python, curl, git,
// AWS SDKs, gRPC C-core). When the TLS bridge is on, the agent must trust the
// per-origin leaves the bridge forges; pointing every one of these at the
// mounted CA covers the agent regardless of which client library it uses.
// Go's crypto/tls also honors SSL_CERT_FILE.
var tlsBridgeTrustEnvVars = []string{
	"NODE_EXTRA_CA_CERTS",              // Node.js
	"SSL_CERT_FILE",                    // OpenSSL (curl, Python ssl, Go)
	"REQUESTS_CA_BUNDLE",               // Python requests / httpx
	"CURL_CA_BUNDLE",                   // curl
	"GIT_SSL_CAINFO",                   // git over HTTPS
	"AWS_CA_BUNDLE",                    // AWS SDKs / CLI
	"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", // gRPC C-core
}

// addVolumeMountIfMissing appends a VolumeMount to the container unless one
// with the same name is already present (idempotent re-injection).
func addVolumeMountIfMissing(c *corev1.Container, vm corev1.VolumeMount) {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == vm.Name {
			return
		}
	}
	c.VolumeMounts = append(c.VolumeMounts, vm)
}

// applyTLSBridgeMounts wires the per-agent CA that backs the AuthBridge TLS
// bridge into the pod. cert-manager publishes the CA keypair into the Secret
// <workloadName>-tls-bridge-ca (see TLSBridgeCAReconciler). The CA has NO Name
// Constraints, so its private key must stay confined to the sidecar — anything
// holding tls.key could forge a cert for any host. This function therefore uses
// TWO volumes from the same Secret:
//
//   - keypair volume (tls.crt + tls.key + ca.crt), mounted ONLY into the
//     authbridge sidecar, which reads tls.crt + tls.key to mint per-origin
//     leaves. The agent container never mounts it.
//   - ca.crt-only volume (Secret items: ca.crt), mounted into every agent
//     container, with the common CA-bundle env vars pointed at it so the agent
//     trusts the minted leaves — without ever seeing the private key.
//
// Both are hard mounts (NOT Optional), so the kubelet blocks pod start until
// cert-manager has issued the Secret — closing the controller↔pod startup race.
//
// Mode is 0444 on both. The sidecar runs non-root (Proxy.UID, gid != 0), so a
// root-owned 0440 file would be unreadable without forcing a fixed pod fsGroup —
// and a fixed fsGroup (we use 0) is rejected by OpenShift restricted-v2 SCC,
// whose supplemental-group range is namespace-assigned. 0444 lets the non-root
// sidecar read its key with no fsGroup at all; the key is still confined to the
// sidecar container (never mounted into the agent), and ca.crt is a public cert.
//
// workloadName MUST equal the AgentRuntime's spec.targetRef.name (== crName in
// the webhook) so the Secret name matches what the reconciler provisions.
func applyTLSBridgeMounts(podSpec *corev1.PodSpec, workloadName string) {
	secretName := workloadName + agentv1alpha1.TLSBridgeCASecretSuffix

	// Sidecar keypair volume (full Secret) + agent ca.crt-only volume.
	if !volumeExists(podSpec.Volumes, TLSBridgeCAVolumeName) {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: TLSBridgeCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  secretName,
					DefaultMode: ptr.To(int32(0o444)),
				},
			},
		})
	}
	if !volumeExists(podSpec.Volumes, TLSBridgeCACertVolumeName) {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: TLSBridgeCACertVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  secretName,
					DefaultMode: ptr.To(int32(0o444)),
					// Project ONLY ca.crt — tls.key/tls.crt never reach the agent.
					Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		})
	}

	caCrtPath := TLSBridgeCAMountPath + "/ca.crt"
	for i := range podSpec.Containers {
		c := &podSpec.Containers[i]
		if c.Name == AuthBridgeProxyContainerName {
			// Sidecar: full keypair (reads tls.crt + tls.key to mint leaves).
			addVolumeMountIfMissing(c, corev1.VolumeMount{
				Name:      TLSBridgeCAVolumeName,
				MountPath: TLSBridgeCAMountPath,
				ReadOnly:  true,
			})
			continue
		}
		// Agent containers: ca.crt only + trust env vars pointing at it.
		addVolumeMountIfMissing(c, corev1.VolumeMount{
			Name:      TLSBridgeCACertVolumeName,
			MountPath: TLSBridgeCAMountPath,
			ReadOnly:  true,
		})
		for _, env := range tlsBridgeTrustEnvVars {
			setOrAddEnv(c, env, caCrtPath)
		}
	}
}

// BuildRequiredVolumes creates all volumes required for sidecar containers (with SPIRE)
func BuildRequiredVolumes() []corev1.Volume {
	// Helper for pointer to bool
	isReadOnly := true

	return []corev1.Volume{
		{
			Name: "shared-data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			// Updated from HostPath to CSI
			Name: "spire-agent-socket",
			VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{
					Driver:   "csi.spiffe.io",
					ReadOnly: &isReadOnly,
				},
			},
		},
		{
			Name: "spiffe-helper-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "spiffe-helper-config",
					},
				},
			},
		},
		{
			Name: "svid-output",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "envoy-config",
					},
					Optional: ptr.To(true),
				},
			},
		},
		{
			Name: "authproxy-routes",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "authproxy-routes",
					},
					Optional: ptr.To(true),
				},
			},
		},
		{
			Name: "authbridge-runtime-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "authbridge-runtime-config",
					},
					Optional: ptr.To(true),
				},
			},
		},
	}
}

// BuildRequiredVolumesNoSpire creates volumes required for sidecar containers without SPIRE
// This excludes spire-agent-socket, spiffe-helper-config, and svid-output volumes
func BuildRequiredVolumesNoSpire() []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "shared-data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "envoy-config",
					},
					Optional: ptr.To(true),
				},
			},
		},
		{
			Name: "authproxy-routes",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "authproxy-routes",
					},
					Optional: ptr.To(true),
				},
			},
		},
		{
			Name: "authbridge-runtime-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "authbridge-runtime-config",
					},
					Optional: ptr.To(true),
				},
			},
		},
	}
}

// BuildResolvedVolumes creates volumes using resolved config values.
// When a resolved envoy config name is provided, the envoy-config volume
// references that ConfigMap instead of the default "envoy-config" one.
// The authbridge-runtime-config volume always references the shared namespace
// ConfigMap; use overrideAuthBridgeConfigMapInVolumes to point it at a
// per-agent ConfigMap after volume creation.
func BuildResolvedVolumes(spireEnabled bool, envoyConfigMapName string) []corev1.Volume {
	if envoyConfigMapName == "" {
		envoyConfigMapName = EnvoyConfigMapName
	}

	volumes := []corev1.Volume{
		{
			Name: "shared-data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if spireEnabled {
		isReadOnly := true
		volumes = append(volumes,
			corev1.Volume{
				Name: "spire-agent-socket",
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:   "csi.spiffe.io",
						ReadOnly: &isReadOnly,
					},
				},
			},
			corev1.Volume{
				Name: "spiffe-helper-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: SpiffeHelperConfigMapName,
						},
					},
				},
			},
			corev1.Volume{
				Name: "svid-output",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		)
	}

	volumes = append(volumes,
		corev1.Volume{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: envoyConfigMapName,
					},
					Optional: ptr.To(true),
				},
			},
		},
		corev1.Volume{
			Name: "authproxy-routes",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: AuthproxyRoutesConfigMapName,
					},
					Optional: ptr.To(true),
				},
			},
		},
		corev1.Volume{
			Name: "authbridge-runtime-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: AuthBridgeRuntimeConfigMapName,
					},
					Optional: ptr.To(true),
				},
			},
		},
	)

	return volumes
}

// overrideAuthBridgeConfigMapInVolumes returns a copy of the volume list with
// the authbridge-runtime-config volume pointing at the given ConfigMap name.
// This is used to redirect the volume mount to a per-agent ConfigMap.
func overrideAuthBridgeConfigMapInVolumes(volumes []corev1.Volume, cmName string) []corev1.Volume {
	result := make([]corev1.Volume, len(volumes))
	copy(result, volumes)
	for i := range result {
		if result[i].Name == AuthBridgeRuntimeConfigMapName && result[i].ConfigMap != nil {
			// Deep copy the ConfigMapVolumeSource to avoid mutating the original
			cmCopy := *result[i].ConfigMap
			cmCopy.Name = cmName
			result[i].ConfigMap = &cmCopy
		}
	}
	return result
}

// overrideRoutesConfigMapInVolumes returns a copy of the volume list with
// the authproxy-routes volume pointing at the given ConfigMap name. Used when
// AgentRuntime has spec.auth.outbound configured: the per-agent routes live
// in authbridge-routes-<crName>, replacing the namespace-level "authproxy-routes".
func overrideRoutesConfigMapInVolumes(volumes []corev1.Volume, routesCMName string) []corev1.Volume {
	result := make([]corev1.Volume, len(volumes))
	copy(result, volumes)
	for i := range result {
		if result[i].Name == AuthproxyRoutesConfigMapName && result[i].ConfigMap != nil {
			cmCopy := *result[i].ConfigMap
			cmCopy.Name = routesCMName
			cmCopy.Optional = ptr.To(false) // Routes are required when specified
			result[i].ConfigMap = &cmCopy
		}
	}
	return result
}

// overrideEnvoyConfigMapInVolumes returns a copy of the volume list with
// the envoy-config volume pointing at the given ConfigMap name. Used by
// the envoy-sidecar mTLS path: the rendered per-agent envoy.yaml lives
// in envoy-config-<crName>, replacing the namespace-level "envoy-config"
// for that workload's Envoy. The volume name itself stays "envoy-config"
// (matches the container's volumeMount); only the underlying ConfigMap
// reference changes.
func overrideEnvoyConfigMapInVolumes(volumes []corev1.Volume, cmName string) []corev1.Volume {
	result := make([]corev1.Volume, len(volumes))
	copy(result, volumes)
	for i := range result {
		if result[i].Name == EnvoyConfigMapName && result[i].ConfigMap != nil {
			cmCopy := *result[i].ConfigMap
			cmCopy.Name = cmName
			result[i].ConfigMap = &cmCopy
		}
	}
	return result
}
