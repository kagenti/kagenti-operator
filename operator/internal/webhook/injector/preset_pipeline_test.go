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

	"github.com/rossoctl/operator/internal/webhook/config"
)

// chainNames returns the ordered plugin names of one chain ("inbound"/"outbound").
func chainNames(t *testing.T, pipeline map[string]interface{}, chain string) []string {
	t.Helper()
	section, ok := pipeline[chain].(map[string]interface{})
	if !ok {
		t.Fatalf("pipeline[%q] is not a map: %T", chain, pipeline[chain])
	}
	plugins, ok := section["plugins"].([]interface{})
	if !ok {
		t.Fatalf("pipeline[%q][plugins] is not a slice: %T", chain, section["plugins"])
	}
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		entry := p.(map[string]interface{})
		names = append(names, entry["name"].(string))
	}
	return names
}

// entryByName returns the plugin entry for name in the given chain, or nil.
func entryByName(t *testing.T, pipeline map[string]interface{}, chain, name string) map[string]interface{} {
	t.Helper()
	section := pipeline[chain].(map[string]interface{})
	for _, p := range section["plugins"].([]interface{}) {
		entry := p.(map[string]interface{})
		if entry["name"] == name {
			return entry
		}
	}
	return nil
}

// TestSynthesizePresetPipeline_EmitsAllPluginsInCanonicalOrder verifies every
// supported plugin is present in both chains in the fixed canonical order,
// regardless of preset membership.
func TestSynthesizePresetPipeline_EmitsAllPluginsInCanonicalOrder(t *testing.T) {
	for _, preset := range []string{"auth-only", "ibac-only", "full"} {
		pipeline, _, err := synthesizePresetPipeline(nil, preset, nil, "", config.IBACConfig{})
		if err != nil {
			t.Fatalf("preset %q: unexpected error: %v", preset, err)
		}
		gotIn := chainNames(t, pipeline, "inbound")
		wantIn := []string{"a2a-parser", "jwt-validation"}
		if !equalStrings(gotIn, wantIn) {
			t.Errorf("preset %q inbound order = %v, want %v", preset, gotIn, wantIn)
		}
		gotOut := chainNames(t, pipeline, "outbound")
		wantOut := []string{"token-exchange", "token-broker", "inference-parser", "mcp-parser", "ibac"}
		if !equalStrings(gotOut, wantOut) {
			t.Errorf("preset %q outbound order = %v, want %v", preset, gotOut, wantOut)
		}
	}
}

// TestSynthesizePresetPipeline_MembershipPolicies verifies preset members are
// emitted at enforce (no on_error) and non-members at on_error: off.
func TestSynthesizePresetPipeline_MembershipPolicies(t *testing.T) {
	// ibac-only: members = a2a-parser (in), inference-parser/mcp-parser/ibac (out).
	pipeline, _, err := synthesizePresetPipeline(nil, "ibac-only", nil, "", config.IBACConfig{JudgeEndpoint: "http://judge:4000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Member: enforce => on_error omitted.
	if e := entryByName(t, pipeline, "inbound", "a2a-parser"); e["on_error"] != nil {
		t.Errorf("a2a-parser (member, enforce) should omit on_error, got %v", e["on_error"])
	}
	if e := entryByName(t, pipeline, "outbound", "ibac"); e["on_error"] != nil {
		t.Errorf("ibac (member, enforce) should omit on_error, got %v", e["on_error"])
	}
	// Non-member: on_error: off.
	if e := entryByName(t, pipeline, "inbound", "jwt-validation"); e["on_error"] != "off" {
		t.Errorf("jwt-validation (non-member) on_error = %v, want off", e["on_error"])
	}
	if e := entryByName(t, pipeline, "outbound", "token-exchange"); e["on_error"] != "off" {
		t.Errorf("token-exchange (non-member) on_error = %v, want off", e["on_error"])
	}
}

// TestSynthesizePresetPipeline_ChainDefaultAndOverrides verifies spec.onError
// sets the member default and per-plugin overrides win.
func TestSynthesizePresetPipeline_ChainDefaultAndOverrides(t *testing.T) {
	// onError=observe makes every member observe; override ibac back to enforce.
	pipeline, _, err := synthesizePresetPipeline(
		nil, "full", []string{"ibac:enforce"}, "observe",
		config.IBACConfig{JudgeEndpoint: "http://judge:4000"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// jwt-validation is a full member => chain default observe.
	if e := entryByName(t, pipeline, "inbound", "jwt-validation"); e["on_error"] != "observe" {
		t.Errorf("jwt-validation on_error = %v, want observe (chain default)", e["on_error"])
	}
	// ibac overridden to enforce => on_error omitted.
	if e := entryByName(t, pipeline, "outbound", "ibac"); e["on_error"] != nil {
		t.Errorf("ibac (overridden enforce) should omit on_error, got %v", e["on_error"])
	}
}

// TestSynthesizePresetPipeline_TokenExchangeBrokerMutex verifies activating both
// token-exchange and token-broker is rejected.
func TestSynthesizePresetPipeline_TokenExchangeBrokerMutex(t *testing.T) {
	// full includes token-exchange; add token-broker via override => conflict.
	_, _, err := synthesizePresetPipeline(nil, "full", []string{"token-broker:enforce"}, "", config.IBACConfig{})
	if err == nil {
		t.Fatal("expected mutex error when both token-exchange and token-broker are active, got nil")
	}
}

// TestSynthesizePresetPipeline_UnknownPresetAndPolicy verifies input validation.
func TestSynthesizePresetPipeline_UnknownPresetAndPolicy(t *testing.T) {
	if _, _, err := synthesizePresetPipeline(nil, "bogus", nil, "", config.IBACConfig{}); err == nil {
		t.Error("expected error for unknown preset")
	}
	if _, _, err := synthesizePresetPipeline(nil, "full", nil, "loud", config.IBACConfig{}); err == nil {
		t.Error("expected error for invalid onError policy")
	}
	if _, _, err := synthesizePresetPipeline(nil, "full", []string{"nope:enforce"}, "", config.IBACConfig{}); err == nil {
		t.Error("expected error for unknown plugin in overrides")
	}
}

// TestSynthesizePresetPipeline_SeedsBaseConfig verifies per-plugin config from
// the base pipeline (e.g. jwt-validation issuer) survives synthesis.
func TestSynthesizePresetPipeline_SeedsBaseConfig(t *testing.T) {
	base := map[string]interface{}{
		"inbound": map[string]interface{}{
			"plugins": []interface{}{
				map[string]interface{}{
					"name":   "jwt-validation",
					"config": map[string]interface{}{"issuer": "https://kc/realms/rossoctl"},
				},
			},
		},
		"outbound": map[string]interface{}{
			"plugins": []interface{}{
				map[string]interface{}{
					"name":   "token-exchange",
					"config": map[string]interface{}{"identity": "spiffe://x"},
				},
			},
		},
	}
	pipeline, _, err := synthesizePresetPipeline(base, "full", nil, "", config.IBACConfig{JudgeEndpoint: "http://judge:4000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	jwt := entryByName(t, pipeline, "inbound", "jwt-validation")
	cfg, _ := jwt["config"].(map[string]interface{})
	if cfg == nil || cfg["issuer"] != "https://kc/realms/rossoctl" {
		t.Errorf("jwt-validation issuer not seeded from base: %v", jwt["config"])
	}
	te := entryByName(t, pipeline, "outbound", "token-exchange")
	tecfg, _ := te["config"].(map[string]interface{})
	if tecfg == nil || tecfg["identity"] != "spiffe://x" {
		t.Errorf("token-exchange identity not seeded from base: %v", te["config"])
	}
}

// TestSynthesizePresetPipeline_IBACJudgeConfig verifies the judge config and
// baked system prompt are stamped onto an active ibac plugin, and that an
// inactive ibac plugin (auth-only) gets neither.
func TestSynthesizePresetPipeline_IBACJudgeConfig(t *testing.T) {
	ibac := config.IBACConfig{
		JudgeEndpoint: "http://litellm:4000",
		JudgeModel:    "llama3.2:3b",
		TimeoutMS:     15000,
	}
	// full: ibac active.
	pipeline, warnings, err := synthesizePresetPipeline(nil, "full", nil, "", ibac)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings with judge endpoint set, got %v", warnings)
	}
	e := entryByName(t, pipeline, "outbound", "ibac")
	cfg := e["config"].(map[string]interface{})
	if cfg["judge_endpoint"] != "http://litellm:4000" {
		t.Errorf("judge_endpoint = %v", cfg["judge_endpoint"])
	}
	if cfg["judge_model"] != "llama3.2:3b" {
		t.Errorf("judge_model = %v", cfg["judge_model"])
	}
	if cfg["timeout_ms"] != 15000 {
		t.Errorf("timeout_ms = %v", cfg["timeout_ms"])
	}
	if _, ok := cfg["system_prompt"].(string); !ok {
		t.Error("system_prompt not baked into active ibac plugin")
	}

	// auth-only: ibac inactive => no judge config, no system prompt.
	authOnly, _, err := synthesizePresetPipeline(nil, "auth-only", nil, "", ibac)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inactive := entryByName(t, authOnly, "outbound", "ibac")
	if inactive["on_error"] != "off" {
		t.Errorf("auth-only ibac on_error = %v, want off", inactive["on_error"])
	}
	if cfg2, ok := inactive["config"].(map[string]interface{}); ok {
		if _, has := cfg2["system_prompt"]; has {
			t.Error("inactive ibac plugin should not carry a system_prompt")
		}
	}
}

// TestSynthesizePresetPipeline_IBACWarningWhenJudgeUnset verifies a warning is
// emitted (but no error) when an ibac-bearing preset has no judge endpoint.
func TestSynthesizePresetPipeline_IBACWarningWhenJudgeUnset(t *testing.T) {
	_, warnings, err := synthesizePresetPipeline(nil, "ibac-only", nil, "", config.IBACConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning when ibac is selected but judgeEndpoint is unset")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
