package watcher

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	bundlecache "github.com/rossoctl/operator/internal/bundleservice/cache"
)

const testServiceNamespace = "rossoctl-system"

func newFakeClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			{Group: "agent.rossoctl.dev", Version: "v1alpha1", Resource: "authorizationpolicies"}: "AuthorizationPolicyList",
		},
		objects...,
	)
}

func newPolicy(name, ns, scope, clientID string) *unstructured.Unstructured {
	spec := map[string]any{
		"scope": scope,
		"policies": []any{
			map[string]any{
				"path":    "inbound/request.rego",
				"content": "package test\ndefault allow := true\n",
			},
		},
	}
	if clientID != "" {
		spec["clientID"] = clientID
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agent.rossoctl.dev/v1alpha1",
			"kind":       "AuthorizationPolicy",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
			},
			"spec": spec,
		},
	}
}

func waitForSync(t *testing.T, w *Watcher) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !w.HasSynced() {
		select {
		case <-deadline:
			t.Fatal("informer never synced")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestWatcher_GetPolicy(t *testing.T) {
	obj := newPolicy("my-policy", "default", "client", "")
	client := newFakeClient(obj)

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	result, err := w.GetPolicy("my-policy", "default")
	if err != nil {
		t.Fatalf("GetPolicy failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestWatcher_GetPolicy_NotFound(t *testing.T) {
	client := newFakeClient()

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	result, err := w.GetPolicy("nonexistent", "default")
	if err != nil {
		t.Fatalf("GetPolicy failed: %v", err)
	}
	if result != nil {
		t.Fatal("expected nil result")
	}
}

func TestWatcher_GetGlobalPolicies(t *testing.T) {
	obj := newPolicy("global-policy", testServiceNamespace, "global", "")
	client := newFakeClient(obj)

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	result := w.GetGlobalPolicies()
	if len(result) != 1 {
		t.Fatalf("expected 1 global policy, got %d", len(result))
	}
}

func TestWatcher_GetGlobalPolicies_IgnoresWrongNamespace(t *testing.T) {
	obj := newPolicy("rogue-global", "other-namespace", "global", "")
	client := newFakeClient(obj)

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	result := w.GetGlobalPolicies()
	if len(result) != 0 {
		t.Fatalf("expected 0 global policies (wrong namespace), got %d", len(result))
	}
}

func TestWatcher_GetNamespacePolicies(t *testing.T) {
	obj1 := newPolicy("ns-policy", "test-ns", "namespace", "")
	obj2 := newPolicy("other-ns-policy", "other-ns", "namespace", "")
	client := newFakeClient(obj1, obj2)

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	result := w.GetNamespacePolicies("test-ns")
	if len(result) != 1 {
		t.Fatalf("expected 1 namespace policy, got %d", len(result))
	}
}

func TestWatcher_GlobalChangeInvalidatesAll(t *testing.T) {
	obj := newPolicy("global-policy", testServiceNamespace, "global", "")
	client := newFakeClient(obj)

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	ec.Set("default/agent1", "hash1")
	ec.Set("default/agent2", "hash2")
	bc.Set("default/agent1", []byte("data"), "hash1")
	pc.SetGlobal([]bundlecache.PolicyEntry{{Path: "old.rego", Content: "old"}}, "oldhash")

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	if ec.Len() != 0 {
		t.Fatalf("expected etag cache cleared, got %d entries", ec.Len())
	}
	if bc.Len() != 0 {
		t.Fatalf("expected bundle cache cleared, got %d entries", bc.Len())
	}
	if _, _, ok := pc.GetGlobal(); ok {
		t.Fatal("expected policy cache global to be invalidated")
	}
}

func TestWatcher_GlobalChangeInWrongNamespaceIgnored(t *testing.T) {
	obj := newPolicy("rogue-global", "wrong-namespace", "global", "")
	client := newFakeClient(obj)

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	ec.Set("default/agent1", "hash1")

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	if _, ok := ec.Get("default/agent1"); !ok {
		t.Fatal("expected agent1 etag to survive — global CR is in wrong namespace")
	}
}

func TestWatcher_ClientChangeInvalidatesOnlyClient(t *testing.T) {
	obj := newPolicy("agent1", "default", "client", "")
	client := newFakeClient(obj)

	ec := bundlecache.NewETagCache()
	bc := bundlecache.NewBundleCache(10, time.Minute)
	pc := bundlecache.NewPolicyCache()

	ec.Set("default/agent1", "hash1")
	ec.Set("default/agent2", "hash2")
	bc.Set("default/agent1", []byte("data1"), "hash1")
	bc.Set("default/agent2", []byte("data2"), "hash2")

	w := New(client, testServiceNamespace, ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitForSync(t, w)

	if _, ok := ec.Get("default/agent1"); ok {
		t.Fatal("expected default/agent1 etag to be invalidated")
	}
	if _, ok := ec.Get("default/agent2"); !ok {
		t.Fatal("expected default/agent2 etag to survive")
	}
}
