package provider

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/rossoctl/operator/internal/bundleservice/cache"
	"github.com/rossoctl/operator/internal/bundleservice/identity"
	"github.com/rossoctl/operator/internal/bundleservice/watcher"
)

func newFakeClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			{Group: "agent.rossoctl.dev", Version: "v1alpha1", Resource: "authorizationpolicies"}: "AuthorizationPolicyList",
		},
		objects...,
	)
}

func newClientPolicy(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agent.rossoctl.dev/v1alpha1",
			"kind":       "AuthorizationPolicy",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]any{
				"scope": "client",
				"policies": []any{
					map[string]any{
						"path":    "inbound/request.rego",
						"content": "package authbridge.client\ndefault allow := true\n",
					},
				},
			},
		},
	}
}

func newGlobalPolicy(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agent.rossoctl.dev/v1alpha1",
			"kind":       "AuthorizationPolicy",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]any{
				"scope": "global",
				"policies": []any{
					map[string]any{
						"path":    "base/allow-all.rego",
						"content": "package authbridge.global\ndefault allow := true\n",
					},
				},
			},
		},
	}
}

func setupWatcher(t *testing.T, objects ...runtime.Object) (*watcher.Watcher, *cache.ETagCache, *cache.BundleCache, *cache.PolicyCache) {
	t.Helper()
	client := newFakeClient(objects...)
	ec := cache.NewETagCache()
	bc := cache.NewBundleCache(100, time.Minute)
	pc := cache.NewPolicyCache()

	w := watcher.New(client, "rossoctl-system", ec, bc, pc)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)

	deadline := time.After(5 * time.Second)
	for !w.HasSynced() {
		select {
		case <-deadline:
			t.Fatal("informer never synced")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	return w, ec, bc, pc
}

func TestProvider_GetBundle_BuildsFromCR(t *testing.T) {
	obj := newClientPolicy("test-agent", "default")
	w, ec, bc, pc := setupWatcher(t, obj)

	p := New(ec, bc, pc, w)
	id := identity.ClientIdentity{Namespace: "default", Name: "test-agent"}
	data, etag, err := p.GetBundle(context.Background(), id, "")
	if err != nil {
		t.Fatalf("GetBundle failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty bundle data")
	}
	if etag == "" {
		t.Fatal("expected non-empty etag")
	}
}

func TestProvider_GetBundle_304WhenETagMatches(t *testing.T) {
	obj := newClientPolicy("test-agent", "default")
	w, ec, bc, pc := setupWatcher(t, obj)

	p := New(ec, bc, pc, w)
	id := identity.ClientIdentity{Namespace: "default", Name: "test-agent"}

	_, etag, err := p.GetBundle(context.Background(), id, "")
	if err != nil {
		t.Fatalf("first GetBundle failed: %v", err)
	}

	data, returnedEtag, err := p.GetBundle(context.Background(), id, etag)
	if err != nil {
		t.Fatalf("second GetBundle failed: %v", err)
	}
	if data != nil {
		t.Fatal("expected nil data for 304")
	}
	if returnedEtag != etag {
		t.Fatalf("expected same etag, got %s vs %s", returnedEtag, etag)
	}
}

func TestProvider_GetBundle_NoCRsReturnsEmptyBundle(t *testing.T) {
	w, ec, bc, pc := setupWatcher(t)

	p := New(ec, bc, pc, w)
	id := identity.ClientIdentity{Namespace: "default", Name: "no-cr-agent"}
	data, _, err := p.GetBundle(context.Background(), id, "")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty bundle (at least decision policy)")
	}
}

func TestProvider_GetBundle_IncludesGlobalPolicies(t *testing.T) {
	global := newGlobalPolicy("global", "rossoctl-system")
	clientPol := newClientPolicy("my-agent", "default")
	w, ec, bc, pc := setupWatcher(t, global, clientPol)

	p := New(ec, bc, pc, w)
	id := identity.ClientIdentity{Namespace: "default", Name: "my-agent"}
	data, _, err := p.GetBundle(context.Background(), id, "")
	if err != nil {
		t.Fatalf("GetBundle failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty bundle")
	}
}

func TestProvider_GetBundle_SingleflightDedup(t *testing.T) {
	obj := newClientPolicy("test-agent", "default")
	w, ec, bc, pc := setupWatcher(t, obj)

	p := New(ec, bc, pc, w)

	bc.InvalidateAll()
	ec.InvalidateAll()

	var buildCount atomic.Int32
	var wg sync.WaitGroup
	const concurrency = 50

	id := identity.ClientIdentity{Namespace: "default", Name: "test-agent"}

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			data, _, err := p.GetBundle(context.Background(), id, "")
			if err != nil {
				t.Errorf("GetBundle failed: %v", err)
				return
			}
			if len(data) > 0 {
				buildCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if buildCount.Load() != concurrency {
		t.Fatalf("expected %d successful results, got %d", concurrency, buildCount.Load())
	}
}
