package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"
)

func TestBuild_ProducesValidTarGz(t *testing.T) {
	policies := []Policy{
		{Path: "authbridge/inbound/request.rego", Content: "package authbridge.inbound.request\ndefault allow := true\n"},
	}

	data, hash, err := Build(policies)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty bundle data")
	}
	if hash == "" {
		t.Fatal("empty hash")
	}
	if len(hash) < 10 || hash[:7] != "sha256:" {
		t.Fatalf("unexpected hash format: %s", hash)
	}

	files := extractTarGz(t, data)
	if _, ok := files["authbridge/inbound/request.rego"]; !ok {
		t.Fatal("missing policy file in bundle")
	}
	if _, ok := files[".manifest"]; !ok {
		t.Fatal("missing .manifest in bundle")
	}
}

func TestBuild_ManifestHasRevisionAndRoots(t *testing.T) {
	policies := []Policy{
		{Path: "authbridge/outbound/request.rego", Content: "package authbridge.outbound.request\ndefault allow := true\n"},
	}

	data, _, err := Build(policies)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	files := extractTarGz(t, data)
	var m manifest
	if err := json.Unmarshal([]byte(files[".manifest"]), &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if m.Revision == "" {
		t.Fatal("manifest revision is empty")
	}
	if len(m.Roots) != 1 || m.Roots[0] != "authbridge" {
		t.Fatalf("unexpected roots: %v", m.Roots)
	}
}

func TestBuild_Deterministic(t *testing.T) {
	policies := []Policy{
		{Path: "authbridge/inbound/request.rego", Content: "package authbridge.inbound.request\ndefault allow := true\n"},
		{Path: "authbridge/outbound/request.rego", Content: "package authbridge.outbound.request\ndefault allow := false\n"},
	}

	data1, hash1, _ := Build(policies)
	data2, hash2, _ := Build(policies)

	if hash1 != hash2 {
		t.Fatalf("non-deterministic hashes: %s vs %s", hash1, hash2)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatal("non-deterministic bundle bytes")
	}
}

func TestBuild_SortsByPath(t *testing.T) {
	policies := []Policy{
		{Path: "authbridge/outbound/request.rego", Content: "out"},
		{Path: "authbridge/inbound/request.rego", Content: "in"},
	}

	data, _, err := Build(policies)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	names := extractTarGzOrder(t, data)
	if len(names) < 2 {
		t.Fatal("expected at least 2 entries")
	}
	if names[0] != "authbridge/inbound/request.rego" {
		t.Fatalf("expected inbound first, got %s", names[0])
	}
	if names[1] != "authbridge/outbound/request.rego" {
		t.Fatalf("expected outbound second, got %s", names[1])
	}
}

func TestBuild_RejectsPathTraversal(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"dotdot prefix", "../etc/passwd"},
		{"dotdot middle", "authbridge/../../../etc/shadow"},
		{"absolute path", "/etc/passwd"},
		{"empty path", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policies := []Policy{{Path: tc.path, Content: "package test\n"}}
			_, _, err := Build(policies)
			if err == nil {
				t.Fatalf("expected error for path %q, got nil", tc.path)
			}
		})
	}
}

func TestBuild_AcceptsValidPaths(t *testing.T) {
	cases := []string{
		"authbridge/inbound/request.rego",
		"authbridge/outbound/response.rego",
		"authbridge/tools/mcp-restrict.rego",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			policies := []Policy{{Path: path, Content: "package test\n"}}
			_, _, err := Build(policies)
			if err != nil {
				t.Fatalf("unexpected error for valid path %q: %v", path, err)
			}
		})
	}
}

func extractTarGz(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() {
		if err := gr.Close(); err != nil {
			t.Errorf("failed to close gzip reader: %v", err)
		}
	}()

	tr := tar.NewReader(gr)
	files := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		content, _ := io.ReadAll(tr)
		files[hdr.Name] = string(content)
	}
	return files
}

func extractTarGzOrder(t *testing.T, data []byte) []string {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() {
		if err := gr.Close(); err != nil {
			t.Errorf("failed to close gzip reader: %v", err)
		}
	}()

	tr := tar.NewReader(gr)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}
