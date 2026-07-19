/*
Copyright 2025.

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

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/rossoctl/operator/internal/bundleservice/cache"
	"github.com/rossoctl/operator/internal/bundleservice/handler"
	"github.com/rossoctl/operator/internal/bundleservice/identity"
	"github.com/rossoctl/operator/internal/bundleservice/provider"
	"github.com/rossoctl/operator/internal/bundleservice/watcher"
)

const version = "0.3.1"

func main() {
	slog.Info("bundle-service starting", "version", version)

	level := slog.LevelInfo
	if l := os.Getenv("LOG_LEVEL"); l != "" {
		switch l {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	cfg, err := restConfig()
	if err != nil {
		slog.Error("failed to get kubernetes config", "error", err)
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		slog.Error("failed to create dynamic client", "error", err)
		os.Exit(1)
	}

	serviceNamespace := os.Getenv("POD_NAMESPACE")
	if serviceNamespace == "" {
		serviceNamespace = detectNamespace()
	}
	slog.Info("service namespace", "namespace", serviceNamespace)

	etagCache := cache.NewETagCache()
	bundleCache := cache.NewBundleCache(100, 1*time.Minute)
	policyCache := cache.NewPolicyCache()

	w := watcher.New(dynClient, serviceNamespace, etagCache, bundleCache, policyCache)
	go w.Run(ctx)

	// Block until informer syncs or context is cancelled
	slog.Info("waiting for informer sync")
	syncTimeout := time.After(60 * time.Second)
	for !w.HasSynced() {
		select {
		case <-ctx.Done():
			slog.Error("context cancelled before informer sync")
			os.Exit(1)
		case <-syncTimeout:
			slog.Error("timed out waiting for informer sync")
			os.Exit(1)
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	slog.Info("informer synced, starting HTTP server")

	prov := provider.New(etagCache, bundleCache, policyCache, w)
	h := handler.New(prov, w, identity.NoopVerifier{})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}

func restConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func detectNamespace() string {
	// In-cluster: read from the mounted service account namespace file
	if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(ns)
	}
	return "rossoctl-system"
}
