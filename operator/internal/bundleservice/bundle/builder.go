package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type Policy struct {
	Path    string
	Content string
}

type manifest struct {
	Revision string   `json:"revision"`
	Roots    []string `json:"roots"`
}

func validatePath(path string) error {
	if path == "" {
		return fmt.Errorf("empty policy path")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("absolute path not allowed: %s", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal not allowed: %s", path)
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("path escapes bundle root: %s", path)
	}
	return nil
}

func Build(policies []Policy) ([]byte, string, error) {
	for _, p := range policies {
		if err := validatePath(p.Path); err != nil {
			return nil, "", err
		}
	}

	sorted := make([]Policy, len(policies))
	copy(sorted, policies)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, p := range sorted {
		content := []byte(p.Content)
		hdr := &tar.Header{
			Name: p.Path,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", fmt.Errorf("writing tar header for %s: %w", p.Path, err)
		}
		if _, err := tw.Write(content); err != nil {
			return nil, "", fmt.Errorf("writing tar content for %s: %w", p.Path, err)
		}
	}

	h := sha256.Sum256(buf.Bytes())
	hash := fmt.Sprintf("sha256:%x", h)

	m := manifest{
		Revision: hash,
		Roots:    []string{"authbridge"},
	}
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, "", fmt.Errorf("marshaling manifest: %w", err)
	}

	hdr := &tar.Header{
		Name: ".manifest",
		Mode: 0644,
		Size: int64(len(manifestJSON)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, "", fmt.Errorf("writing manifest header: %w", err)
	}
	if _, err := tw.Write(manifestJSON); err != nil {
		return nil, "", fmt.Errorf("writing manifest content: %w", err)
	}

	if err := tw.Close(); err != nil {
		return nil, "", fmt.Errorf("closing tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, "", fmt.Errorf("closing gzip writer: %w", err)
	}

	data := buf.Bytes()
	finalHash := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	return data, finalHash, nil
}
