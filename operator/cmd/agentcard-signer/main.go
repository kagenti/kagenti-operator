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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/agentcard"
	"github.com/rossoctl/operator/internal/signature"
)

const (
	defaultSocket       = "unix:///run/spire/sockets/agent.sock"
	defaultUnsignedPath = "/etc/agentcard/agent.json"
	defaultSignedPath   = "/app/.well-known/agent-card.json"
	defaultTimeout      = "30s"
)

func main() {
	if err := run(); err != nil {
		logJSON("error", "signing failed", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	socketPath := envOrDefault("SPIFFE_ENDPOINT_SOCKET", defaultSocket)
	unsignedPath := envOrDefault("UNSIGNED_CARD_PATH", defaultUnsignedPath)
	signedPath := envOrDefault("AGENT_CARD_PATH", defaultSignedPath)
	timeoutStr := envOrDefault("SIGN_TIMEOUT", defaultTimeout)

	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return fmt.Errorf("invalid SIGN_TIMEOUT %q: %w", timeoutStr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	logJSON("info", "starting agentcard signer",
		"socket", socketPath,
		"unsigned_path", unsignedPath,
		"signed_path", signedPath,
		"timeout", timeoutStr,
	)

	svid, err := fetchSVID(ctx, socketPath)
	if err != nil {
		return fmt.Errorf("failed to fetch X.509-SVID: %w", err)
	}
	defer signature.ZeroPrivateKey(svid.PrivateKey)

	spiffeID := svid.ID.String()
	logJSON("info", "fetched SVID", "spiffe_id", spiffeID)

	unsignedJSON, err := os.ReadFile(unsignedPath)
	if err != nil {
		return fmt.Errorf("failed to read unsigned card from %s: %w", unsignedPath, err)
	}

	var cardData agentv1alpha1.AgentCardData
	if err := json.Unmarshal(unsignedJSON, &cardData); err != nil {
		return fmt.Errorf("failed to parse unsigned card JSON: %w", err)
	}

	signedCard, err := signature.SignCard(&cardData, svid.PrivateKey, svid.Certificates)
	if err != nil {
		return fmt.Errorf("signing failed: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(signedPath), 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	if err := os.WriteFile(signedPath, signedCard, 0o644); err != nil {
		return fmt.Errorf("failed to write signed card to %s: %w", signedPath, err)
	}

	logJSON("info", "signed card written successfully",
		"spiffe_id", spiffeID,
		"output_path", signedPath,
	)

	if err := writeConfigMap(ctx, signedCard); err != nil {
		logJSON("warn", "ConfigMap write failed (non-fatal, operator will use HTTP fallback)", "error", err.Error())
	}

	return nil
}

func writeConfigMap(ctx context.Context, signedCard []byte) error {
	agentName := os.Getenv("AGENT_NAME")
	namespace := os.Getenv("POD_NAMESPACE")
	if agentName == "" || namespace == "" {
		return fmt.Errorf("AGENT_NAME or POD_NAMESPACE not set, skipping ConfigMap write")
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}

	clientset, err := k8sclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("k8s clientset: %w", err)
	}

	return writeConfigMapWithClient(ctx, clientset, agentName, namespace, signedCard)
}

func writeConfigMapWithClient(
	ctx context.Context, clientset k8sclient.Interface,
	agentName, namespace string, signedCard []byte,
) error {
	cmName := agentcard.ConfigMapName(agentName)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: namespace},
		Data:       map[string]string{"agent-card.json": string(signedCard)},
	}

	_, err := clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("failed to write ConfigMap %s: %w", cmName, err)
	}

	logJSON("info", "signed card written to ConfigMap", "configMap", cmName, "namespace", namespace)
	return nil
}

func fetchSVID(ctx context.Context, socketPath string) (*x509svid.SVID, error) {
	client, err := workloadapi.New(ctx, workloadapi.WithAddr(socketPath))
	if err != nil {
		return nil, fmt.Errorf("failed to create workload API client: %w", err)
	}
	defer client.Close() //nolint:errcheck // best-effort cleanup

	svid, err := client.FetchX509SVID(ctx)
	if err != nil {
		return nil, fmt.Errorf("FetchX509SVID failed: %w", err)
	}
	return svid, nil
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func logJSON(level, msg string, kvs ...string) {
	entry := map[string]string{
		"level": level,
		"msg":   msg,
		"ts":    time.Now().UTC().Format(time.RFC3339),
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		entry[kvs[i]] = kvs[i+1]
	}
	data, _ := json.Marshal(entry)
	fmt.Fprintln(os.Stderr, string(data))
}
