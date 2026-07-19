package bundle

import "testing"

func TestContentHash_Deterministic(t *testing.T) {
	policies := []Policy{
		{Path: "authbridge/agent/inbound.rego", Content: "package test\ndefault allow := true\n"},
		{Path: "authbridge/global/base.rego", Content: "package global\n"},
	}

	h1 := ContentHash(policies)
	h2 := ContentHash(policies)

	if h1 != h2 {
		t.Fatalf("non-deterministic: %s vs %s", h1, h2)
	}
}

func TestContentHash_OrderIndependent(t *testing.T) {
	p1 := []Policy{
		{Path: "b.rego", Content: "b"},
		{Path: "a.rego", Content: "a"},
	}
	p2 := []Policy{
		{Path: "a.rego", Content: "a"},
		{Path: "b.rego", Content: "b"},
	}

	if ContentHash(p1) != ContentHash(p2) {
		t.Fatal("hash should be order-independent")
	}
}

func TestContentHash_DifferentContent(t *testing.T) {
	p1 := []Policy{{Path: "a.rego", Content: "v1"}}
	p2 := []Policy{{Path: "a.rego", Content: "v2"}}

	if ContentHash(p1) == ContentHash(p2) {
		t.Fatal("different content should produce different hash")
	}
}

func TestContentHash_Empty(t *testing.T) {
	h := ContentHash(nil)
	if h != "sha256:empty" {
		t.Fatalf("unexpected empty hash: %s", h)
	}
}

func TestContentHash_PathContentSeparation(t *testing.T) {
	// Ensure path+content boundary doesn't cause collisions
	p1 := []Policy{{Path: "ab", Content: "cd"}}
	p2 := []Policy{{Path: "a", Content: "bcd"}}

	if ContentHash(p1) == ContentHash(p2) {
		t.Fatal("different path/content splits should not collide")
	}
}
