package bundle

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
)

func ContentHash(policies []Policy) string {
	if len(policies) == 0 {
		return "sha256:empty"
	}
	sorted := make([]Policy, len(policies))
	copy(sorted, policies)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})

	h := sha256.New()
	for _, p := range sorted {
		_, _ = io.WriteString(h, p.Path)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, p.Content)
		_, _ = io.WriteString(h, "\x00")
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
