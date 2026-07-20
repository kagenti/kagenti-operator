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

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigLoaderOnChangeInvokedOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	var calls int
	l := NewConfigLoader(path)
	l.OnChange(func(*PlatformConfig) { calls++ })

	if err := l.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 OnChange call after first Load, got %d", calls)
	}

	yaml := `images:
  envoyProxy: test/envoy:latest
  proxyInit: test/init:latest
  spiffeHelper: test/spiffe:latest
  clientRegistration: test/reg:latest
proxy:
  port: 16000
  uid: 1337
  inboundProxyPort: 16001
  adminPort: 16002
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Load(); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 OnChange calls after second Load, got %d", calls)
	}
	got := l.Get()
	if got.Proxy.Port != 16000 {
		t.Fatalf("expected merged proxy port 16000, got %d", got.Proxy.Port)
	}
}

func TestFeatureGateLoaderOnChangeInvokedOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feature-gates.yaml")

	var calls int
	l := NewFeatureGateLoader(path)
	l.OnChange(func(*FeatureGates) { calls++ })

	if err := l.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 OnChange call after first Load, got %d", calls)
	}

	yaml := `globalEnabled: true
injectTools: true
envoyProxy: true
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Load(); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 OnChange calls after second Load, got %d", calls)
	}
	if !l.Get().InjectTools {
		t.Fatal("expected injectTools true from file")
	}
}
