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

package v1alpha1

import "testing"

func TestValidatePlugins(t *testing.T) {
	tests := []struct {
		name    string
		spec    AgentRuntimeSpec
		wantErr bool
	}{
		{
			name: "no preset is a no-op even with malformed plugins",
			spec: AgentRuntimeSpec{Plugins: []string{"bogus:loud"}},
		},
		{
			name: "valid tokens with preset",
			spec: AgentRuntimeSpec{PluginPreset: "full", Plugins: []string{"ibac:observe", "mcp-parser:off"}},
		},
		{
			name: "bare name defaults to enforce",
			spec: AgentRuntimeSpec{PluginPreset: "full", Plugins: []string{"ibac"}},
		},
		{
			name:    "unknown plugin name",
			spec:    AgentRuntimeSpec{PluginPreset: "full", Plugins: []string{"nope:enforce"}},
			wantErr: true,
		},
		{
			name:    "invalid policy",
			spec:    AgentRuntimeSpec{PluginPreset: "full", Plugins: []string{"ibac:loud"}},
			wantErr: true,
		},
		{
			name:    "empty token",
			spec:    AgentRuntimeSpec{PluginPreset: "full", Plugins: []string{"  "}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.ValidatePlugins()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePlugins() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
