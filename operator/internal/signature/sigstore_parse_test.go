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

package signature

import (
	"encoding/json"
	"testing"
)

func TestParseSignedAgentCardStructure_Absent(t *testing.T) {
	plain := []byte(`{"name":"x","version":"1"}`)
	got, err := parseSignedAgentCardStructure(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Absent {
		t.Fatalf("expected absent for plain card, got %#v", got)
	}
}

func TestParseSignedAgentCardStructure_WithAttestations(t *testing.T) {
	raw := []byte(`{
		"agentCard": {"name": "A", "version": "1"},
		"attestations": {
			"signatureBundle": {"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"},
			"provenanceBundle": {"provenance": {"predicateType": "https://slsa.dev/provenance/v1"}}
		}
	}`)
	got, err := parseSignedAgentCardStructure(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Absent {
		t.Fatal("expected bundle present")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(got.BundleRaw, &m); err != nil {
		t.Fatal(err)
	}
	if m["mediaType"] == nil {
		t.Fatalf("expected signature bundle mediaType, got %s", string(got.BundleRaw))
	}
}
