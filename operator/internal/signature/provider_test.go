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
	"testing"
)

func TestNewProvider_NilConfig(t *testing.T) {
	_, err := NewProvider(nil)
	if err == nil {
		t.Error("Expected error for nil config")
	}
}

func TestNewProvider_UnknownType(t *testing.T) {
	_, err := NewProvider(&Config{Type: "unknown"})
	if err == nil {
		t.Error("Expected error for unknown provider type")
	}
}

func TestNewProvider_X5C_MissingConfigMapName(t *testing.T) {
	_, err := NewProvider(&Config{
		Type:                   ProviderTypeX5C,
		TrustBundleConfigMapNS: "spire-system",
	})
	if err == nil {
		t.Error("Expected error when TrustBundleConfigMapName is empty")
	}
}

func TestNewProvider_X5C_MissingConfigMapNamespace(t *testing.T) {
	_, err := NewProvider(&Config{
		Type:                     ProviderTypeX5C,
		TrustBundleConfigMapName: "spire-bundle",
	})
	if err == nil {
		t.Error("Expected error when TrustBundleConfigMapNS is empty")
	}
}

func TestNewProvider_X5C_MissingClient(t *testing.T) {
	_, err := NewProvider(&Config{
		Type:                     ProviderTypeX5C,
		TrustBundleConfigMapName: "spire-bundle",
		TrustBundleConfigMapNS:   "spire-system",
	})
	if err == nil {
		t.Error("Expected error when Client is nil")
	}
}
