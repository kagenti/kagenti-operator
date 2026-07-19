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

// Run with: go test -v -count=1 ./cmd/ -run TestBuildConfigMapCacheNamespaces

package main

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/rossoctl/operator/internal/controller"
)

func TestAuthBridgeWebhooksEnabled(t *testing.T) {
	t.Run("enables when unset", func(t *testing.T) {
		t.Setenv("ENABLE_WEBHOOKS", "")
		if !authBridgeWebhooksEnabled() {
			t.Fatal("expected webhooks enabled when ENABLE_WEBHOOKS is unset")
		}
	})
	t.Run("enables when not false", func(t *testing.T) {
		t.Setenv("ENABLE_WEBHOOKS", "true")
		if !authBridgeWebhooksEnabled() {
			t.Fatal("expected webhooks enabled for non-false value")
		}
	})
	t.Run("disables when false", func(t *testing.T) {
		t.Setenv("ENABLE_WEBHOOKS", "false")
		if authBridgeWebhooksEnabled() {
			t.Fatal("expected webhooks disabled when ENABLE_WEBHOOKS=false")
		}
	})
}

func TestBuildConfigMapCacheNamespaces(t *testing.T) {
	t.Run("base config always includes cluster defaults and namespace defaults", func(t *testing.T) {
		result := buildConfigMapCacheNamespaces(false, "", "", false, "", "")

		if _, ok := result[controller.ClusterDefaultsNamespace]; !ok {
			t.Fatalf("expected entry for %s", controller.ClusterDefaultsNamespace)
		}
		if _, ok := result[cache.AllNamespaces]; !ok {
			t.Fatal("expected entry for AllNamespaces (namespace-level defaults)")
		}
		if len(result) != 2 {
			t.Fatalf("expected exactly 2 entries, got %d", len(result))
		}
	})

	t.Run("adds SPIRE trust bundle namespace when signature verification enabled", func(t *testing.T) {
		const spireNS = "zero-trust-workload-identity-manager"
		result := buildConfigMapCacheNamespaces(true, "spire-bundle", spireNS, false, "", "")

		spireCfg, ok := result[spireNS]
		if !ok {
			t.Fatalf("expected cache entry for %s namespace", spireNS)
		}
		if spireCfg.FieldSelector == nil {
			t.Fatal("expected FieldSelector on SPIRE cache entry")
		}
		if !spireCfg.FieldSelector.Matches(fieldSet("spire-bundle")) {
			t.Fatal("expected FieldSelector to match ConfigMap named spire-bundle")
		}
		if spireCfg.FieldSelector.Matches(fieldSet("other-configmap")) {
			t.Fatal("expected FieldSelector to NOT match other ConfigMap names")
		}
		if len(result) != 3 {
			t.Fatalf("expected 3 entries (cluster + namespace + SPIRE), got %d", len(result))
		}
	})

	t.Run("does not add SPIRE entry when flag is false", func(t *testing.T) {
		const spireNS = "zero-trust-workload-identity-manager"
		result := buildConfigMapCacheNamespaces(false, "spire-bundle", spireNS, false, "", "")

		if _, ok := result[spireNS]; ok {
			t.Fatal("expected no SPIRE entry when requireA2ASignature is false")
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(result))
		}
	})

	t.Run("does not add SPIRE entry when namespace is empty", func(t *testing.T) {
		result := buildConfigMapCacheNamespaces(true, "spire-bundle", "", false, "", "")

		if len(result) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(result))
		}
	})

	t.Run("namespace collision preserves existing label selector", func(t *testing.T) {
		result := buildConfigMapCacheNamespaces(true, "spire-bundle", controller.ClusterDefaultsNamespace, false, "", "")

		cfg := result[controller.ClusterDefaultsNamespace]
		if cfg.LabelSelector == nil {
			t.Fatal("expected original LabelSelector to be preserved when namespaces collide")
		}
		if cfg.FieldSelector != nil {
			t.Fatal("expected SPIRE FieldSelector to NOT be added when namespaces collide")
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 entries (no SPIRE entry added), got %d", len(result))
		}
	})
}

// fieldSet is a minimal fields.Fields implementation for test assertions.
type fieldSet string

func (f fieldSet) Has(field string) bool { return field == "metadata.name" }
func (f fieldSet) Get(field string) string {
	if field == "metadata.name" {
		return string(f)
	}
	return ""
}
