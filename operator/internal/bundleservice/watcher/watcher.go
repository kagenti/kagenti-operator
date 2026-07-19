package watcher

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	bundlecache "github.com/rossoctl/operator/internal/bundleservice/cache"
)

const (
	ScopeIndex = "spec.scope"
)

type Watcher struct {
	informer         cache.SharedIndexInformer
	etagCache        *bundlecache.ETagCache
	bundleCache      *bundlecache.BundleCache
	policyCache      *bundlecache.PolicyCache
	serviceNamespace string
}

const defaultScope = "client"

func New(client dynamic.Interface, serviceNamespace string, etagCache *bundlecache.ETagCache, bundleCache *bundlecache.BundleCache, policyCache *bundlecache.PolicyCache) *Watcher {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(client, 0)
	informer := factory.ForResource(v1alpha1.AuthorizationPolicyGVR()).Informer()

	_ = informer.AddIndexers(cache.Indexers{
		ScopeIndex: func(obj interface{}) ([]string, error) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return nil, fmt.Errorf("unexpected type %T", obj)
			}
			scope, _, _ := unstructured.NestedString(u.Object, "spec", "scope")
			if scope == "" {
				scope = defaultScope
			}
			return []string{scope}, nil
		},
	})

	w := &Watcher{
		informer:         informer,
		etagCache:        etagCache,
		bundleCache:      bundleCache,
		policyCache:      policyCache,
		serviceNamespace: serviceNamespace,
	}

	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.invalidate(obj) },
		UpdateFunc: func(_, newObj interface{}) { w.invalidate(newObj) },
		DeleteFunc: func(obj interface{}) { w.invalidate(obj) },
	})

	return w
}

func (w *Watcher) Run(ctx context.Context) {
	slog.Info("starting AuthorizationPolicy watcher")
	w.informer.Run(ctx.Done())
}

func (w *Watcher) HasSynced() bool {
	return w.informer.HasSynced()
}

func (w *Watcher) Ready() bool {
	return w.informer.HasSynced()
}

// GetGlobalPolicies returns global-scope CRs that reside in the service namespace.
// Global CRs in other namespaces are ignored.
func (w *Watcher) GetGlobalPolicies() []*unstructured.Unstructured {
	items, err := w.informer.GetIndexer().ByIndex(ScopeIndex, "global")
	if err != nil {
		slog.Error("global index lookup failed", "error", err)
		return nil
	}
	var result []*unstructured.Unstructured
	for _, item := range items {
		u, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if u.GetNamespace() != w.serviceNamespace {
			slog.Warn("ignoring global CR outside service namespace",
				"name", u.GetName(), "namespace", u.GetNamespace(),
				"expected", w.serviceNamespace)
			continue
		}
		result = append(result, u)
	}
	return result
}

// GetNamespacePolicies returns namespace-scope CRs that reside in the given namespace.
func (w *Watcher) GetNamespacePolicies(ns string) []*unstructured.Unstructured {
	items, err := w.informer.GetIndexer().ByIndex(ScopeIndex, "namespace")
	if err != nil {
		slog.Error("namespace index lookup failed", "error", err)
		return nil
	}
	var result []*unstructured.Unstructured
	for _, item := range items {
		u, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if u.GetNamespace() == ns {
			result = append(result, u)
		}
	}
	return result
}

// GetPolicy looks up a client-scope AuthorizationPolicy CR by name and namespace.
func (w *Watcher) GetPolicy(name, namespace string) (*unstructured.Unstructured, error) {
	key := namespace + "/" + name
	item, exists, err := w.informer.GetIndexer().GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", key, err)
	}
	if !exists {
		return nil, nil
	}
	u, ok := item.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T for key %s", item, key)
	}
	return u, nil
}

func (w *Watcher) invalidate(obj interface{}) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		u, ok = tombstone.Obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
	}

	scope, _, _ := unstructured.NestedString(u.Object, "spec", "scope")
	if scope == "" {
		scope = "client"
	}

	switch scope {
	case "global":
		if u.GetNamespace() != w.serviceNamespace {
			return
		}
		slog.Info("global policy changed, invalidating all caches")
		w.policyCache.InvalidateGlobal()
		w.etagCache.InvalidateAll()
		w.bundleCache.InvalidateAll()

	case "namespace":
		ns := u.GetNamespace()
		slog.Info("namespace policy changed", "namespace", ns)
		w.policyCache.InvalidateNamespace(ns)
		w.etagCache.InvalidateAll()
		w.bundleCache.InvalidateAll()

	case "client":
		ns := u.GetNamespace()
		name := u.GetName()
		key := ns + "/" + name
		slog.Info("client policy changed", "key", key)
		w.etagCache.Invalidate(key)
		w.bundleCache.Invalidate(key)
	}
}
