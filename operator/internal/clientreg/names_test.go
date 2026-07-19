/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package clientreg

import "testing"

func TestKeycloakClientCredentialsSecretName_Deterministic(t *testing.T) {
	a := KeycloakClientCredentialsSecretName("team1", "weather-agent")
	b := KeycloakClientCredentialsSecretName("team1", "weather-agent")
	if a != b {
		t.Fatalf("expected deterministic output, got %q and %q", a, b)
	}
	if a == "" {
		t.Fatalf("expected non-empty secret name")
	}
}

func TestKeycloakClientCredentialsSecretName_DistinctByInputs(t *testing.T) {
	cases := []struct{ ns, w string }{
		{"team1", "a"},
		{"team1", "b"},
		{"team2", "a"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		got := KeycloakClientCredentialsSecretName(c.ns, c.w)
		key := c.ns + "/" + c.w
		if prev, ok := seen[got]; ok {
			t.Fatalf("collision: %s and %s both produced %s", prev, key, got)
		}
		seen[got] = key
	}
}

func TestSkipReason(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		injectTools bool
		wantSkip    bool
	}{
		{"nil labels", nil, true, true},
		{"empty labels", map[string]string{}, true, true},
		{"agent proceeds", map[string]string{LabelAgentType: LabelValueAgent}, false, false},
		{"tool with gate on proceeds", map[string]string{LabelAgentType: LabelValueTool}, true, false},
		{"tool with gate off skipped", map[string]string{LabelAgentType: LabelValueTool}, false, true},
		{"unknown type skipped", map[string]string{LabelAgentType: "other"}, true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason := SkipReason(tc.labels, tc.injectTools)
			gotSkip := reason != ""
			if gotSkip != tc.wantSkip {
				t.Fatalf("SkipReason(%v, %v) = %q; wantSkip=%v", tc.labels, tc.injectTools, reason, tc.wantSkip)
			}
			wants := WorkloadWantsOperatorClientReg(tc.labels, tc.injectTools)
			if wants == tc.wantSkip {
				t.Fatalf("WorkloadWantsOperatorClientReg disagrees with SkipReason for %v", tc)
			}
		})
	}
}
