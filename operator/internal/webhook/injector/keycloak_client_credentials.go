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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var keycloakClientRegistrationLog = logf.Log.WithName("keycloak-client-registration")

// AnnotationKeycloakClientSecretName is set on the pod template by rossoctl-operator when it manages
// Keycloak OAuth2/OIDC client registration. The value is the name of a Secret in the pod namespace containing
// keys client-id.txt and client-secret.txt. The webhook mounts them for every container that uses
// the shared-data volume (same paths as the client-registration sidecar: /shared/client-id.txt,
// /shared/client-secret.txt).
const AnnotationKeycloakClientSecretName = "rossoctl.io/keycloak-client-credentials-secret-name"

// keycloakClientCredentialsVolumeName is the in-pod Volume name for the Secret that holds Keycloak
// client credentials.
func keycloakClientCredentialsVolumeName(secretName string) string {
	return secretName
}

// NeedsKeycloakClientCredentialsVolumePatch reports whether the pod still needs Secret volume mounts for
// operator-managed Keycloak client credentials (e.g. after webhook reinvocation when sidecars were injected first).
func NeedsKeycloakClientCredentialsVolumePatch(podSpec *corev1.PodSpec, annotations map[string]string) bool {
	secretName := strings.TrimSpace(annotations[AnnotationKeycloakClientSecretName])
	if secretName == "" {
		return false
	}
	volName := keycloakClientCredentialsVolumeName(secretName)
	if !volumeExists(podSpec.Volumes, volName) {
		return true
	}
	for i := range podSpec.Containers {
		if !containerVolumeMountExists(podSpec.Containers[i].VolumeMounts, "shared-data") {
			continue
		}
		if !containerHasKeycloakCredentialsMount(podSpec.Containers[i], volName, "/shared/client-secret.txt") ||
			!containerHasKeycloakCredentialsMount(podSpec.Containers[i], volName, "/shared/client-id.txt") {
			return true
		}
	}
	return false
}

func containerHasKeycloakCredentialsMount(c corev1.Container, volumeName, mountPath string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == volumeName && m.MountPath == mountPath {
			return true
		}
	}
	return false
}

// ApplyKeycloakClientCredentialsSecretVolumes mounts operator-provisioned Keycloak client credentials into any
// container that already mounts shared-data (injected sidecars and any user container that shares it).
func ApplyKeycloakClientCredentialsSecretVolumes(podSpec *corev1.PodSpec, annotations map[string]string) {
	secretName := strings.TrimSpace(annotations[AnnotationKeycloakClientSecretName])
	if secretName == "" {
		return
	}

	volName := keycloakClientCredentialsVolumeName(secretName)

	keycloakClientRegistrationLog.Info("mounting Keycloak OAuth2 client credentials Secret for shared-data containers",
		"secretName", secretName,
		"volumeName", volName)

	if !volumeExists(podSpec.Volumes, volName) {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
					Optional:   ptr.To(false),
				},
			},
		})
	}

	for i := range podSpec.Containers {
		if !containerVolumeMountExists(podSpec.Containers[i].VolumeMounts, "shared-data") {
			continue
		}
		appendSubPathMount(&podSpec.Containers[i], volName, "client-secret.txt", "/shared/client-secret.txt")
		appendSubPathMount(&podSpec.Containers[i], volName, "client-id.txt", "/shared/client-id.txt")
	}
}

func containerVolumeMountExists(mounts []corev1.VolumeMount, volumeName string) bool {
	for _, m := range mounts {
		if m.Name == volumeName {
			return true
		}
	}
	return false
}

func appendSubPathMount(c *corev1.Container, vol, subPath, mountPath string) {
	for _, m := range c.VolumeMounts {
		if m.Name == vol && m.MountPath == mountPath {
			return
		}
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      vol,
		MountPath: mountPath,
		SubPath:   subPath,
		ReadOnly:  true,
	})
}
