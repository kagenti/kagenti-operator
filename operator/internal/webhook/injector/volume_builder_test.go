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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildResolvedVolumes_SpireDisabled(t *testing.T) {
	volumes := BuildResolvedVolumes(false, "")

	// Should have: shared-data, envoy-config, authproxy-routes, authbridge-runtime-config
	if len(volumes) != 4 {
		t.Fatalf("expected 4 volumes, got %d", len(volumes))
	}

	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}

	for _, expected := range []string{"shared-data", "envoy-config", "authproxy-routes", "authbridge-runtime-config"} {
		if !names[expected] {
			t.Errorf("missing volume %q", expected)
		}
	}

	// Should NOT have SPIRE volumes
	for _, absent := range []string{"spire-agent-socket", "spiffe-helper-config", "svid-output"} {
		if names[absent] {
			t.Errorf("unexpected SPIRE volume %q when spireEnabled=false", absent)
		}
	}
}

func TestBuildResolvedVolumes_SpireEnabled(t *testing.T) {
	volumes := BuildResolvedVolumes(true, "")

	// Should have: shared-data, spire-agent-socket, spiffe-helper-config, svid-output, envoy-config, authproxy-routes, authbridge-runtime-config
	if len(volumes) != 7 {
		t.Fatalf("expected 7 volumes, got %d", len(volumes))
	}

	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}

	for _, expected := range []string{"shared-data", "spire-agent-socket", "spiffe-helper-config", "svid-output", "envoy-config", "authproxy-routes", "authbridge-runtime-config"} {
		if !names[expected] {
			t.Errorf("missing volume %q", expected)
		}
	}
}

func TestEnvoyConfigVolume_Optional(t *testing.T) {
	check := func(t *testing.T, volumes []corev1.Volume) {
		t.Helper()
		for _, v := range volumes {
			if v.Name == "envoy-config" && v.ConfigMap != nil {
				if v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
					t.Fatalf("envoy-config volume must be Optional, got %v", v.ConfigMap.Optional)
				}
				return
			}
		}
		t.Fatal("envoy-config volume not found")
	}

	t.Run("BuildRequiredVolumes", func(t *testing.T) {
		check(t, BuildRequiredVolumes())
	})
	t.Run("BuildRequiredVolumesNoSpire", func(t *testing.T) {
		check(t, BuildRequiredVolumesNoSpire())
	})
	t.Run("BuildResolvedVolumes_spireOff", func(t *testing.T) {
		check(t, BuildResolvedVolumes(false, ""))
	})
	t.Run("BuildResolvedVolumes_spireOn", func(t *testing.T) {
		check(t, BuildResolvedVolumes(true, ""))
	})
}

func TestBuildResolvedVolumes_CustomEnvoyConfigMapName(t *testing.T) {
	volumes := BuildResolvedVolumes(false, "my-custom-envoy")

	var envoyVolume *string
	for _, v := range volumes {
		if v.Name == "envoy-config" {
			name := v.VolumeSource.ConfigMap.LocalObjectReference.Name
			envoyVolume = &name
		}
	}

	if envoyVolume == nil {
		t.Fatal("envoy-config volume not found")
	}
	if *envoyVolume != "my-custom-envoy" {
		t.Errorf("envoy-config ConfigMap name = %q, want %q", *envoyVolume, "my-custom-envoy")
	}
}

func TestBuildResolvedVolumes_DefaultEnvoyConfigMapName(t *testing.T) {
	volumes := BuildResolvedVolumes(false, "")

	for _, v := range volumes {
		if v.Name == "envoy-config" {
			name := v.VolumeSource.ConfigMap.LocalObjectReference.Name
			if name != EnvoyConfigMapName {
				t.Errorf("envoy-config ConfigMap name = %q, want %q", name, EnvoyConfigMapName)
			}
			return
		}
	}
	t.Fatal("envoy-config volume not found")
}

func TestBuildResolvedVolumes_AuthBridgeDefaultsToSharedCM(t *testing.T) {
	volumes := BuildResolvedVolumes(false, "")

	for _, v := range volumes {
		if v.Name == AuthBridgeRuntimeConfigMapName {
			name := v.ConfigMap.Name
			if name != AuthBridgeRuntimeConfigMapName {
				t.Errorf("authbridge-runtime-config ConfigMap name = %q, want %q", name, AuthBridgeRuntimeConfigMapName)
			}
			return
		}
	}
	t.Fatal("authbridge-runtime-config volume not found")
}

func TestOverrideAuthBridgeConfigMapInVolumes(t *testing.T) {
	original := BuildRequiredVolumes()
	overridden := overrideAuthBridgeConfigMapInVolumes(original, "authbridge-config-my-agent")

	// Original should be unchanged
	for _, v := range original {
		if v.Name == AuthBridgeRuntimeConfigMapName && v.ConfigMap != nil {
			if v.ConfigMap.Name != AuthBridgeRuntimeConfigMapName {
				t.Errorf("original was mutated: got %q", v.ConfigMap.Name)
			}
		}
	}

	// Overridden should have the new name
	for _, v := range overridden {
		if v.Name == AuthBridgeRuntimeConfigMapName && v.ConfigMap != nil {
			if v.ConfigMap.Name != "authbridge-config-my-agent" {
				t.Errorf("override failed: got %q, want %q",
					v.ConfigMap.Name, "authbridge-config-my-agent")
			}
			return
		}
	}
	t.Fatal("authbridge-runtime-config volume not found in overridden volumes")
}

// TestOverrideEnvoyConfigMapInVolumes locks the contract used by the
// envoy-sidecar mtls path: when a per-agent envoy-config CM has been
// rendered, the volume reference at the EnvoyConfigMapName slot must
// be redirected to it without mutating the input slice or touching
// any other volume.
func TestOverrideEnvoyConfigMapInVolumes(t *testing.T) {
	tests := []struct {
		name    string
		volumes func() []corev1.Volume
		newCM   string
		// found: true means the function should locate the envoy-config
		// volume and update it; false means the input has no such
		// volume and the result must equal the input element-for-element.
		found bool
	}{
		{
			name: "volume found, name swapped",
			volumes: func() []corev1.Volume {
				return BuildRequiredVolumes()
			},
			newCM: "envoy-config-my-agent",
			found: true,
		},
		{
			name: "no envoy-config volume, list unchanged",
			volumes: func() []corev1.Volume {
				return []corev1.Volume{{
					Name:         "shared-data",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				}}
			},
			newCM: "envoy-config-my-agent",
			found: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.volumes()
			overridden := overrideEnvoyConfigMapInVolumes(original, tt.newCM)

			// Original must not be mutated regardless of branch.
			for _, v := range original {
				if v.Name == EnvoyConfigMapName && v.ConfigMap != nil {
					if v.ConfigMap.Name != EnvoyConfigMapName {
						t.Errorf("original was mutated: got %q", v.ConfigMap.Name)
					}
				}
			}

			// Output length matches input length (no add/drop).
			if len(overridden) != len(original) {
				t.Fatalf("overridden length = %d, want %d", len(overridden), len(original))
			}

			// Find-and-swap behavior.
			swappedFound := false
			for _, v := range overridden {
				if v.Name == EnvoyConfigMapName && v.ConfigMap != nil {
					swappedFound = true
					if v.ConfigMap.Name != tt.newCM {
						t.Errorf("envoy-config CM name = %q, want %q", v.ConfigMap.Name, tt.newCM)
					}
				}
			}
			if tt.found && !swappedFound {
				t.Fatal("expected envoy-config volume in overridden but didn't find it")
			}
			if !tt.found && swappedFound {
				t.Fatal("envoy-config volume should not have been added")
			}
		})
	}
}
