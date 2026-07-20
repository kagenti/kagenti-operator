package cache

import "testing"

func TestPolicyCache_Global(t *testing.T) {
	c := NewPolicyCache()

	_, _, ok := c.GetGlobal()
	if ok {
		t.Fatal("expected miss before set")
	}

	policies := []PolicyEntry{{Path: "inbound/request.rego", Content: "package test"}}
	c.SetGlobal(policies, "hash1")

	got, hash, ok := c.GetGlobal()
	if !ok {
		t.Fatal("expected hit")
	}
	if hash != "hash1" {
		t.Fatalf("unexpected hash: %s", hash)
	}
	if len(got) != 1 || got[0].Path != "inbound/request.rego" {
		t.Fatal("unexpected policies")
	}

	c.InvalidateGlobal()
	_, _, ok = c.GetGlobal()
	if ok {
		t.Fatal("expected miss after invalidation")
	}
}

func TestPolicyCache_Namespace(t *testing.T) {
	c := NewPolicyCache()

	_, _, ok := c.GetNamespace("test-ns")
	if ok {
		t.Fatal("expected miss before set")
	}

	policies := []PolicyEntry{{Path: "outbound/request.rego", Content: "package ns"}}
	c.SetNamespace("test-ns", policies, "hash2")

	got, hash, ok := c.GetNamespace("test-ns")
	if !ok {
		t.Fatal("expected hit")
	}
	if hash != "hash2" {
		t.Fatalf("unexpected hash: %s", hash)
	}
	if len(got) != 1 || got[0].Path != "outbound/request.rego" {
		t.Fatal("unexpected policies")
	}

	c.InvalidateNamespace("test-ns")
	_, _, ok = c.GetNamespace("test-ns")
	if ok {
		t.Fatal("expected miss after invalidation")
	}
}

func TestPolicyCache_MultipleNamespaces(t *testing.T) {
	c := NewPolicyCache()
	c.SetNamespace("ns1", []PolicyEntry{{Path: "a.rego", Content: "a"}}, "h1")
	c.SetNamespace("ns2", []PolicyEntry{{Path: "b.rego", Content: "b"}}, "h2")

	c.InvalidateNamespace("ns1")

	_, _, ok := c.GetNamespace("ns1")
	if ok {
		t.Fatal("expected miss for ns1")
	}
	_, _, ok = c.GetNamespace("ns2")
	if !ok {
		t.Fatal("expected hit for ns2")
	}
}
