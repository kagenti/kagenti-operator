package config

// FeatureGates controls which sidecars are globally enabled/disabled.
// This is the highest-priority layer in the injection precedence chain.
//
// Spiffe-helper and client-registration are no longer separate-sidecar
// features:
//   - spiffe-helper is bundled inside the EnvoyProxy and AuthBridge
//     combined images and starts conditionally on the per-workload
//     SPIRE_ENABLED env var.
//   - client-registration is now operator-managed entirely (the in-pod
//     sidecar path is gone). See operator-managed-client-registration.md.
type FeatureGates struct {
	GlobalEnabled bool `json:"globalEnabled" yaml:"globalEnabled"`
	EnvoyProxy    bool `json:"envoyProxy" yaml:"envoyProxy"`
	// InjectTools controls whether tool workloads (rossoctl.io/type=tool) receive
	// sidecar injection. Defaults to false — tools are not injected by default.
	InjectTools bool `json:"injectTools" yaml:"injectTools"`
	// PerWorkloadConfigResolution controls the env-var injection mode:
	//   false (default) → legacy path: env vars use ValueFrom ConfigMapKeyRef/
	//                     SecretKeyRef references; kubelet resolves at container start.
	//   true            → resolved path: webhook reads namespace ConfigMaps at
	//                     admission time and injects literal env var values.
	PerWorkloadConfigResolution bool `json:"perWorkloadConfigResolution" yaml:"perWorkloadConfigResolution"`
	// SkillDiscovery controls whether the AgentRuntime controller reads the
	// rossoctl.io/skills annotation from target workloads and populates
	// status.linkedSkills. When disabled, skill discovery is skipped and
	// the SkillsDiscovered condition is not set.
	SkillDiscovery bool `json:"skillDiscovery" yaml:"skillDiscovery"`
}

// DefaultFeatureGates returns feature gates with sidecar injection enabled for
// agents and disabled for tools.
func DefaultFeatureGates() *FeatureGates {
	return &FeatureGates{
		GlobalEnabled:               true,
		EnvoyProxy:                  true,
		InjectTools:                 false,
		PerWorkloadConfigResolution: false,
		SkillDiscovery:              false,
	}
}

// DeepCopy creates a copy of the feature gates.
func (fg *FeatureGates) DeepCopy() *FeatureGates {
	if fg == nil {
		return nil
	}
	result := *fg
	return &result
}
