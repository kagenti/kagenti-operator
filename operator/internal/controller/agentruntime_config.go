/*
Copyright 2026.

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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// ClusterDefaultsConfigMapName is the ConfigMap containing platform-wide webhook defaults.
	ClusterDefaultsConfigMapName = "rossoctl-platform-config"

	// ClusterFeatureGatesConfigMapName is the ConfigMap containing feature gate settings.
	ClusterFeatureGatesConfigMapName = "rossoctl-feature-gates"

	// clusterDefaultsNamespaceDefault is the fallback namespace when POD_NAMESPACE is not set.
	clusterDefaultsNamespaceDefault = "rossoctl-system"

	// LabelNamespaceDefaults identifies namespace-level defaults ConfigMaps.
	LabelNamespaceDefaults = "rossoctl.io/defaults"

	// AuthBridgeRuntimeConfigMapName is the namespace-scoped ConfigMap that
	// holds the authbridge runtime config (config.yaml). Edits to this
	// ConfigMap are watched by AgentRuntimeReconciler so the resolved-config
	// hash picks them up and rolls affected workloads.
	AuthBridgeRuntimeConfigMapName = "authbridge-runtime-config"

	// SpiffeHelperConfigMapName is the namespace-scoped ConfigMap holding
	// the spiffe-helper helper.conf. Derived from PlatformConfig by the
	// controller; included in the config hash for rolling updates.
	SpiffeHelperConfigMapName = "spiffe-helper-config"
)

// ClusterDefaultsNamespace is the namespace where cluster-level ConfigMaps
// and template ConfigMaps live. Defaults to "rossoctl-system"; set once to the
// operator's own namespace by main() via SetClusterDefaultsNamespace.
//
// Write-once semantics: SetClusterDefaultsNamespace must be called exactly once
// from main() before the manager starts. Subsequent calls are no-ops.
var (
	ClusterDefaultsNamespace     = clusterDefaultsNamespaceDefault
	clusterDefaultsNamespaceOnce sync.Once
)

func SetClusterDefaultsNamespace(ns string) {
	clusterDefaultsNamespaceOnce.Do(func() {
		if ns != "" {
			ClusterDefaultsNamespace = ns
		}
	})
}

// resolvedConfig is the canonical representation used for hash computation.
// It captures the 2-layer merge of cluster defaults → namespace defaults.
// CR-level fields (type, authBridgeMode, mtlsMode, etc.) are NOT included — the
// webhook reads those at pod CREATE time (RHAIENG-4936).
type resolvedConfig struct {
	FeatureGates map[string]string `json:"featureGates,omitempty"`
	Defaults     map[string]string `json:"defaults,omitempty"`

	// AuthBridgeRuntime captures the namespace authbridge-runtime-config
	// ConfigMap's config.yaml content so namespace-level edits flow into
	// the hash. Stored as the raw string (not parsed) because authbridge
	// pipelines/listener/mtls config drift through here in any shape and
	// we want any byte change to roll the workload. Empty string when
	// the ConfigMap doesn't exist in the namespace.
	AuthBridgeRuntime string `json:"authBridgeRuntime,omitempty"`

	// SpiffeHelperConfig captures the spiffe-helper-config CM content so
	// changes to PlatformConfig's spiffe.helperConfig trigger rolling updates.
	SpiffeHelperConfig string `json:"spiffeHelperConfig,omitempty"`
}

// ConfigResult holds the computed hash and any warnings from the config resolution.
type ConfigResult struct {
	Hash     string
	Warnings []string
}

// ComputeConfigHash computes a deterministic SHA256 hash from the 2-layer
// merged configuration: cluster defaults → namespace defaults.
// CR-level fields are excluded — the webhook reads those at pod CREATE time.
func ComputeConfigHash(ctx context.Context, c client.Reader, namespace string) (ConfigResult, error) {
	resolved, warnings := resolveConfig(ctx, c, namespace)
	hash, err := hashResolvedConfig(resolved)
	if err != nil {
		return ConfigResult{}, err
	}
	return ConfigResult{Hash: hash, Warnings: warnings}, nil
}

// resolveConfig merges the two platform configuration layers:
// 1. Cluster defaults (ConfigMaps in rossoctl-system)
// 2. Namespace defaults (ConfigMap with rossoctl.io/defaults=true label)
func resolveConfig(ctx context.Context, c client.Reader, namespace string) (resolvedConfig, []string) {
	var warnings []string

	// Layer 1: cluster defaults
	clusterDefaults := readConfigMapData(ctx, c, ClusterDefaultsNamespace, ClusterDefaultsConfigMapName)
	featureGates := readConfigMapData(ctx, c, ClusterDefaultsNamespace, ClusterFeatureGatesConfigMapName)

	// Layer 2: namespace defaults (override cluster)
	nsDefaults, nsWarning := readNamespaceDefaults(ctx, c, namespace)
	if nsWarning != "" {
		warnings = append(warnings, nsWarning)
	}
	merged := mergeMaps(clusterDefaults, nsDefaults)

	// Layer 2b: namespace authbridge-runtime-config (config.yaml).
	// Captured raw so any byte change rolls the workload. The CM may
	// not exist in every agent namespace; absence is normal and the
	// admission webhook falls back to its own defaults.
	abRuntime := ""
	if data := readConfigMapData(ctx, c, namespace, AuthBridgeRuntimeConfigMapName); len(data) > 0 {
		abRuntime = data["config.yaml"]
	}

	// Layer 2c: spiffe-helper-config (helper.conf).
	// Derived from PlatformConfig by the controller; included in hash
	// so changes to spiffe.helperConfig trigger rolling updates.
	spiffeHelper := ""
	if data := readConfigMapData(ctx, c, namespace, SpiffeHelperConfigMapName); len(data) > 0 {
		spiffeHelper = data["helper.conf"]
	}

	return resolvedConfig{
		FeatureGates:       featureGates,
		Defaults:           merged,
		AuthBridgeRuntime:  abRuntime,
		SpiffeHelperConfig: spiffeHelper,
	}, warnings
}

// readConfigMapData reads a specific ConfigMap by name and namespace.
// Returns an empty map if the ConfigMap does not exist.
func readConfigMapData(ctx context.Context, c client.Reader, namespace, name string) map[string]string {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cm); err != nil {
		log.FromContext(ctx).V(2).Info("ConfigMap not found, using empty defaults",
			"namespace", namespace, "name", name, "error", err)
		return map[string]string{}
	}
	if cm.Data == nil {
		return map[string]string{}
	}
	return cm.Data
}

// readNamespaceDefaults reads the namespace-level defaults ConfigMap.
// Expects at most one ConfigMap with the rossoctl.io/defaults=true label per namespace.
// Returns the ConfigMap data and a warning if multiple ConfigMaps are found.
func readNamespaceDefaults(ctx context.Context, c client.Reader, namespace string) (map[string]string, string) {
	logger := log.FromContext(ctx)

	cmList := &corev1.ConfigMapList{}
	if err := c.List(ctx, cmList,
		client.InNamespace(namespace),
		client.MatchingLabels{LabelNamespaceDefaults: "true"},
	); err != nil {
		logger.V(2).Info("Failed to list namespace defaults ConfigMaps", "namespace", namespace, "error", err)
		return map[string]string{}, ""
	}

	if len(cmList.Items) == 0 {
		return map[string]string{}, ""
	}

	var warning string
	if len(cmList.Items) > 1 {
		names := make([]string, len(cmList.Items))
		for i := range cmList.Items {
			names[i] = cmList.Items[i].Name
		}
		warning = fmt.Sprintf(
			"multiple namespace defaults ConfigMaps found in %s (expected at most one): %v; using %s",
			namespace, names, cmList.Items[0].Name,
		)
		logger.Error(fmt.Errorf("%s", warning), "Ambiguous namespace defaults")
	}

	if cmList.Items[0].Data == nil {
		return map[string]string{}, warning
	}
	return cmList.Items[0].Data, warning
}

// mergeMaps merges two maps. Values in override take precedence over base.
func mergeMaps(base, override map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		result[k] = v
	}
	return result
}

// hashResolvedConfig produces a deterministic SHA256 hex string from the resolved config.
// encoding/json sorts map keys, ensuring deterministic output.
func hashResolvedConfig(resolved resolvedConfig) (string, error) {
	b, err := json.Marshal(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to marshal resolved config: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
