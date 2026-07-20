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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	defaultSocket   = "unix:///run/spire/sockets/agent.sock"
	defaultTLSPort  = "8443"
	defaultHTTPPort = "8080"
)

func main() {
	socketPath := envOrDefault("SPIFFE_ENDPOINT_SOCKET", defaultSocket)
	tlsPort := envOrDefault("TLS_PORT", defaultTLSPort)
	httpPort := envOrDefault("HTTP_PORT", defaultHTTPPort)
	agentName := envOrDefault("AGENT_NAME", "tls-test-agent")
	podNamespace := envOrDefault("POD_NAMESPACE", "default")

	cardJSON := buildAgentCard(agentName, podNamespace)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cardJSON)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	var wg sync.WaitGroup

	// HTTP server (plain, for fallback testing)
	httpServer := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("Starting HTTP server on :%s", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// TLS server using SPIFFE SVID
	x509Source, err := workloadapi.NewX509Source(
		ctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socketPath)),
	)
	if err != nil {
		log.Fatalf("Failed to create X509Source: %v", err)
	}
	defer x509Source.Close() //nolint:errcheck

	svid, err := x509Source.GetX509SVID()
	if err != nil {
		log.Fatalf("Failed to get initial SVID: %v", err)
	}
	log.Printf("Got SVID: %s", svid.ID)

	tlsCfg := tlsconfig.MTLSServerConfig(x509Source, x509Source, tlsconfig.AuthorizeAny())

	tlsServer := &http.Server{
		Addr:              ":" + tlsPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("Starting TLS server on :%s (SPIFFE mTLS)", tlsPort)
		ln, err := tls.Listen("tcp", ":"+tlsPort, tlsCfg)
		if err != nil {
			log.Fatalf("TLS listen error: %v", err)
		}
		if err := tlsServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("TLS server error: %v", err)
		}
	}()

	<-sig
	log.Println("Shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = tlsServer.Shutdown(shutdownCtx)
	wg.Wait()
}

func buildAgentCard(agentName, namespace string) []byte {
	cardPath := envOrDefault("AGENT_CARD_PATH", "")
	if cardPath != "" {
		data, err := os.ReadFile(cardPath)
		if err == nil {
			return data
		}
		log.Printf("Warning: could not read %s: %v, using default card", cardPath, err)
	}

	card := map[string]interface{}{
		"name":        agentName,
		"description": "TLS test agent for Phase 1 verified fetch testing",
		"url":         fmt.Sprintf("https://%s.%s.svc.cluster.local:8443", agentName, namespace),
		"version":     "1.0.0",
		"capabilities": map[string]interface{}{
			"streaming":         false,
			"pushNotifications": false,
		},
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain"},
		"skills": []map[string]interface{}{
			{
				"name":        "echo",
				"description": "Echoes input back (test skill)",
				"inputModes":  []string{"text/plain"},
				"outputModes": []string{"text/plain"},
			},
		},
	}

	data, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal agent card: %v", err)
	}
	return data
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
