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

// Run with: make setup-envtest && go test -v -count=1 -timeout 120s ./cmd/ -run TestConfigMapCacheVisibility

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rossoctl/operator/internal/controller"
)

func TestConfigMapCacheVisibility(t *testing.T) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	// 1. Start a local kube-apiserver + etcd via envtest.
	testEnv := &envtest.Environment{}
	if dir := firstEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	defer func() { _ = testEnv.Stop() }()

	// 2. Create a direct (non-cached) client for seeding test data.
	directClient, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatalf("failed to create direct client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		spireNS         = "zero-trust-workload-identity-manager"
		spireBundleName = "spire-bundle"
	)

	// 3. Pre-create the namespaces the scoped cache will watch.
	for _, ns := range []string{controller.ClusterDefaultsNamespace, spireNS, "agent-team-a"} {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := directClient.Create(ctx, nsObj); err != nil {
			t.Fatalf("failed to create namespace %s: %v", ns, err)
		}
	}

	// 4. Build the scoped cache config (function under test) and start a
	//    manager whose ConfigMap informers are restricted to those selectors.
	cmCacheNamespaces := buildConfigMapCacheNamespaces(true, spireBundleName, spireNS, false, "", "")

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         clientgoscheme.Scheme,
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {Namespaces: cmCacheNamespaces},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager exited with error: %v", err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache never synced")
	}

	// 5. Obtain the cached client — reads go through the scoped informer cache,
	//    so only ConfigMaps matching our selectors will be visible.
	cachedClient := mgr.GetClient()

	// 6. Seed ConfigMaps via the direct client (bypasses the cache) so we can
	//    then verify which ones are visible through the cached client.
	spireBundleCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: spireBundleName, Namespace: spireNS,
		},
		Data: map[string]string{"bundle.crt": "FAKE-BUNDLE"},
	}
	clusterDefaultsCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.ClusterDefaultsConfigMapName,
			Namespace: controller.ClusterDefaultsNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "operator-chart"},
		},
		Data: map[string]string{"otel-endpoint": "collector:4317"},
	}
	nsDefaultsCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-a-defaults", Namespace: "agent-team-a",
			Labels: map[string]string{controller.LabelNamespaceDefaults: "true"},
		},
		Data: map[string]string{"sampling-rate": "0.5"},
	}
	unrelatedCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-config", Namespace: spireNS,
		},
		Data: map[string]string{"key": "value"},
	}

	for _, cm := range []*corev1.ConfigMap{spireBundleCM, clusterDefaultsCM, nsDefaultsCM, unrelatedCM} {
		if err := directClient.Create(ctx, cm); err != nil {
			t.Fatalf("failed to create ConfigMap %s/%s: %v", cm.Namespace, cm.Name, err)
		}
	}

	t.Run("SPIRE trust bundle visible through cache", func(t *testing.T) {
		got := &corev1.ConfigMap{}
		err := cachedClient.Get(ctx, types.NamespacedName{Name: spireBundleName, Namespace: spireNS}, got)
		if err != nil {
			t.Fatalf("expected SPIRE trust bundle to be visible, got: %v", err)
		}
		if got.Data["bundle.crt"] != "FAKE-BUNDLE" {
			t.Fatalf("unexpected data: %v", got.Data)
		}
	})

	t.Run("cluster defaults visible through cache", func(t *testing.T) {
		got := &corev1.ConfigMap{}
		err := cachedClient.Get(ctx, types.NamespacedName{
			Name: controller.ClusterDefaultsConfigMapName, Namespace: controller.ClusterDefaultsNamespace,
		}, got)
		if err != nil {
			t.Fatalf("expected cluster defaults to be visible, got: %v", err)
		}
	})

	t.Run("namespace defaults visible through cache", func(t *testing.T) {
		got := &corev1.ConfigMap{}
		err := cachedClient.Get(ctx, types.NamespacedName{Name: "team-a-defaults", Namespace: "agent-team-a"}, got)
		if err != nil {
			t.Fatalf("expected namespace defaults to be visible, got: %v", err)
		}
	})

	t.Run("unrelated ConfigMap in SPIRE namespace is NOT visible", func(t *testing.T) {
		got := &corev1.ConfigMap{}
		err := cachedClient.Get(ctx, types.NamespacedName{Name: "app-config", Namespace: spireNS}, got)
		if err == nil {
			t.Fatal("expected unrelated ConfigMap to be filtered out by field selector, but Get succeeded")
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected NotFound error, got: %v", err)
		}
	})
}

func firstEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
