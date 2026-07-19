package provider

import (
	"context"
	"log/slog"

	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/bundleservice/bundle"
	"github.com/rossoctl/operator/internal/bundleservice/cache"
	"github.com/rossoctl/operator/internal/bundleservice/identity"
	"github.com/rossoctl/operator/internal/bundleservice/watcher"
)

const (
	maxBundleSize       = 5 * 1024 * 1024 // 5MB
	maxConcurrentBuilds = 10
)

type buildResult struct {
	data []byte
	hash string
}

type Provider struct {
	etagCache   *cache.ETagCache
	bundleCache *cache.BundleCache
	policyCache *cache.PolicyCache
	watcher     *watcher.Watcher
	buildGroup  singleflight.Group
	buildSem    chan struct{}
}

func New(etagCache *cache.ETagCache, bundleCache *cache.BundleCache, policyCache *cache.PolicyCache, w *watcher.Watcher) *Provider {
	return &Provider{
		etagCache:   etagCache,
		bundleCache: bundleCache,
		policyCache: policyCache,
		watcher:     w,
		buildSem:    make(chan struct{}, maxConcurrentBuilds),
	}
}

func (p *Provider) GetBundle(ctx context.Context, id identity.ClientIdentity, ifNoneMatch string) ([]byte, string, error) {
	key := id.CacheKey()

	// Fast path: check ETag cache for 304
	if cachedHash, ok := p.etagCache.Get(key); ok {
		if ifNoneMatch != "" && ifNoneMatch == cachedHash {
			return nil, cachedHash, nil
		}
	}

	// Check bundle cache (1-min TTL)
	if data, etag, ok := p.bundleCache.Get(key); ok {
		p.etagCache.Set(key, etag)
		return data, etag, nil
	}

	// Build via singleflight (dedup concurrent requests for same client)
	// Semaphore limits concurrent builds to avoid overwhelming etcd/API server
	result, err, _ := p.buildGroup.Do(key, func() (interface{}, error) {
		select {
		case p.buildSem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-p.buildSem }()
		return p.buildBundle(ctx, id)
	})
	if err != nil {
		return nil, "", err
	}

	br := result.(*buildResult)
	return br.data, br.hash, nil
}

func (p *Provider) buildBundle(ctx context.Context, id identity.ClientIdentity) (*buildResult, error) {
	_ = ctx
	key := id.CacheKey()

	// Gather policies from all three tiers
	var allPolicies []bundle.Policy

	// 1. Global tier
	globalPolicies := p.getGlobalPolicies()
	for _, pol := range globalPolicies {
		allPolicies = append(allPolicies, bundle.Policy{
			Path:    "authbridge/global/" + pol.Path,
			Content: pol.Content,
		})
	}

	// 2. Namespace tier
	nsPolicies := p.getNamespacePolicies(id.Namespace)
	for _, pol := range nsPolicies {
		allPolicies = append(allPolicies, bundle.Policy{
			Path:    "authbridge/ns/" + pol.Path,
			Content: pol.Content,
		})
	}

	// 3. Client tier — look up CR by name in the namespace
	clientPolicies, err := p.getClientPolicies(id)
	if err != nil {
		return nil, err
	}
	for _, pol := range clientPolicies {
		allPolicies = append(allPolicies, bundle.Policy{
			Path:    "authbridge/client/" + pol.Path,
			Content: pol.Content,
		})
	}

	// Compute content hash and update ETag cache
	hash := bundle.ContentHash(allPolicies)
	p.etagCache.Set(key, hash)

	// Build the tar.gz
	data, _, err := bundle.Build(allPolicies)
	if err != nil {
		return nil, err
	}

	if len(data) > maxBundleSize {
		slog.Error("bundle exceeds size limit", "ns", id.Namespace, "name", id.Name, "size", len(data), "limit", maxBundleSize)
		return nil, ErrBundleTooLarge
	}

	// Store in bundle cache (short TTL for replica bursts)
	p.bundleCache.Set(key, data, hash)
	slog.Info("bundle built", "ns", id.Namespace, "name", id.Name, "hash", hash)

	return &buildResult{data: data, hash: hash}, nil
}

func (p *Provider) getGlobalPolicies() []cache.PolicyEntry {
	if policies, _, ok := p.policyCache.GetGlobal(); ok {
		return policies
	}

	objs := p.watcher.GetGlobalPolicies()
	policies := extractPolicies(objs)
	hash := hashPolicyEntries(policies)
	p.policyCache.SetGlobal(policies, hash)
	return policies
}

func (p *Provider) getNamespacePolicies(ns string) []cache.PolicyEntry {
	if policies, _, ok := p.policyCache.GetNamespace(ns); ok {
		return policies
	}

	objs := p.watcher.GetNamespacePolicies(ns)
	policies := extractPolicies(objs)
	hash := hashPolicyEntries(policies)
	p.policyCache.SetNamespace(ns, policies, hash)
	return policies
}

func (p *Provider) getClientPolicies(id identity.ClientIdentity) ([]cache.PolicyEntry, error) {
	obj, err := p.watcher.GetPolicy(id.Name, id.Namespace)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}

	ap, err := v1alpha1.AuthorizationPolicyFromUnstructured(obj)
	if err != nil {
		slog.Error("failed to convert client policy", "ns", id.Namespace, "name", id.Name, "error", err)
		return nil, err
	}

	policies := make([]cache.PolicyEntry, len(ap.Spec.Policies))
	for i, pe := range ap.Spec.Policies {
		policies[i] = cache.PolicyEntry{Path: pe.Path, Content: pe.Content}
	}
	return policies, nil
}

func extractPolicies(objs []*unstructured.Unstructured) []cache.PolicyEntry {
	var result []cache.PolicyEntry
	for _, obj := range objs {
		ap, err := v1alpha1.AuthorizationPolicyFromUnstructured(obj)
		if err != nil {
			slog.Error("failed to convert policy CR", "name", obj.GetName(), "error", err)
			continue
		}
		for _, pe := range ap.Spec.Policies {
			result = append(result, cache.PolicyEntry{Path: pe.Path, Content: pe.Content})
		}
	}
	return result
}

func hashPolicyEntries(entries []cache.PolicyEntry) string {
	policies := make([]bundle.Policy, len(entries))
	for i, e := range entries {
		policies[i] = bundle.Policy{Path: e.Path, Content: e.Content}
	}
	return bundle.ContentHash(policies)
}
