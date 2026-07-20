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

package agentcard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testAgentCardJSON = `{"name":"test-agent","version":"1.0","url":"http://example.com"}`

func TestDefaultFetcher_SuccessfulA2ACardFetch(t *testing.T) {
	g := NewGomegaWithT(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.URL.Path).To(Equal(A2AAgentCardPath))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testAgentCardJSON))
	}))
	defer server.Close()

	result, err := NewFetcher().Fetch(context.Background(), A2AProtocol, server.URL, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.CardData.Name).To(Equal("test-agent"))
	g.Expect(result.CardData.Version).To(Equal("1.0"))
}

func TestFetchA2ACard_LegacyFallback(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == A2ALegacyAgentCardPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(testAgentCardJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	result, err := NewFetcher().Fetch(context.Background(), A2AProtocol, srv.URL, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.CardData.Name).To(Equal("test-agent"))
}

func TestFetchA2ACard_BothNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := NewFetcher().Fetch(context.Background(), A2AProtocol, srv.URL, "", "")
	g.Expect(err).To(HaveOccurred())
}

func TestDefaultFetcher_UnsupportedProtocol(t *testing.T) {
	g := NewGomegaWithT(t)
	_, err := NewFetcher().Fetch(context.Background(), "unsupported", "http://example.com", "", "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("unsupported protocol"))
}

func TestDefaultFetcher_HTTPError500(t *testing.T) {
	g := NewGomegaWithT(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("error body"))
	}))
	defer server.Close()

	_, err := NewFetcher().Fetch(context.Background(), A2AProtocol, server.URL, "", "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("unexpected status code"))
}

func TestDefaultFetcher_InvalidJSON(t *testing.T) {
	g := NewGomegaWithT(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not valid json"))
	}))
	defer server.Close()

	_, err := NewFetcher().Fetch(context.Background(), A2AProtocol, server.URL, "", "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("failed to parse agent card JSON"))
}

func TestGetServiceURL(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(GetServiceURL("my-agent", "default", 8080)).To(Equal("http://my-agent.default.svc.cluster.local:8080"))
}

func newFakeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func TestFetchA2ACard_WithProviderField(t *testing.T) {
	g := NewGomegaWithT(t)
	cardJSON := `{
		"name": "provider-agent",
		"version": "2.0",
		"url": "http://example.com",
		"provider": {
			"organization": "ACME Corp",
			"url": "https://acme.example.com"
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cardJSON))
	}))
	defer server.Close()

	result, err := NewFetcher().Fetch(context.Background(), A2AProtocol, server.URL, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.CardData.Provider).NotTo(BeNil())
	g.Expect(result.CardData.Provider.Organization).To(Equal("ACME Corp"))
	g.Expect(result.CardData.Provider.URL).To(Equal("https://acme.example.com"))
}

func TestFetchA2ACard_WithDocAndIconURLs(t *testing.T) {
	g := NewGomegaWithT(t)
	cardJSON := `{
		"name": "docs-agent",
		"version": "1.0",
		"url": "http://example.com",
		"documentationUrl": "https://docs.example.com",
		"iconUrl": "https://example.com/icon.png"
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cardJSON))
	}))
	defer server.Close()

	result, err := NewFetcher().Fetch(context.Background(), A2AProtocol, server.URL, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.CardData.DocumentationURL).To(Equal("https://docs.example.com"))
	g.Expect(result.CardData.IconURL).To(Equal("https://example.com/icon.png"))
}

func TestFetchA2ACard_WithExtensions(t *testing.T) {
	g := NewGomegaWithT(t)
	cardJSON := `{
		"name": "ext-agent",
		"version": "1.0",
		"url": "http://example.com",
		"capabilities": {
			"streaming": true,
			"extensions": [
				{
					"uri": "urn:ext:logging",
					"description": "Logging extension",
					"required": true,
					"params": {"level": "debug"}
				}
			]
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cardJSON))
	}))
	defer server.Close()

	result, err := NewFetcher().Fetch(context.Background(), A2AProtocol, server.URL, "", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.CardData.Capabilities).NotTo(BeNil())
	g.Expect(result.CardData.Capabilities.Extensions).To(HaveLen(1))
	ext := result.CardData.Capabilities.Extensions[0]
	g.Expect(ext.URI).To(Equal("urn:ext:logging"))
	g.Expect(ext.Description).To(Equal("Logging extension"))
	g.Expect(*ext.Required).To(BeTrue())
	g.Expect(ext.Params).To(HaveKeyWithValue("level", apiextensionsv1.JSON{Raw: json.RawMessage(`"debug"`)}))
}

func TestFetchA2ACard_FullA2ACompatibility(t *testing.T) {
	g := NewGomegaWithT(t)
	cardJSON := `{
		"name": "full-agent",
		"description": "A fully compatible A2A agent",
		"version": "3.0",
		"url": "http://example.com/agent",
		"documentationUrl": "https://docs.example.com/full-agent",
		"iconUrl": "https://example.com/full-agent/icon.png",
		"provider": {
			"organization": "Full Corp",
			"url": "https://fullcorp.example.com"
		},
		"capabilities": {
			"streaming": true,
			"pushNotifications": false,
			"extensions": [
				{
					"uri": "urn:ext:audit",
					"description": "Audit trail",
					"required": false,
					"params": {"retention": "30d"}
				},
				{
					"uri": "urn:ext:metrics",
					"description": "Metrics collection"
				}
			]
		},
		"defaultInputModes": ["text", "application/json"],
		"defaultOutputModes": ["text"],
		"skills": [
			{
				"name": "summarize",
				"description": "Summarizes text"
			}
		],
		"supportsAuthenticatedExtendedCard": true
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cardJSON))
	}))
	defer server.Close()

	result, err := NewFetcher().Fetch(context.Background(), A2AProtocol, server.URL, "", "")
	g.Expect(err).NotTo(HaveOccurred())

	// Core fields
	g.Expect(result.CardData.Name).To(Equal("full-agent"))
	g.Expect(result.CardData.Description).To(Equal("A fully compatible A2A agent"))
	g.Expect(result.CardData.Version).To(Equal("3.0"))
	g.Expect(result.CardData.URL).To(Equal("http://example.com/agent"))

	// New fields
	g.Expect(result.CardData.DocumentationURL).To(Equal("https://docs.example.com/full-agent"))
	g.Expect(result.CardData.IconURL).To(Equal("https://example.com/full-agent/icon.png"))

	// Provider
	g.Expect(result.CardData.Provider).NotTo(BeNil())
	g.Expect(result.CardData.Provider.Organization).To(Equal("Full Corp"))
	g.Expect(result.CardData.Provider.URL).To(Equal("https://fullcorp.example.com"))

	// Capabilities + extensions
	g.Expect(result.CardData.Capabilities).NotTo(BeNil())
	g.Expect(*result.CardData.Capabilities.Streaming).To(BeTrue())
	g.Expect(*result.CardData.Capabilities.PushNotifications).To(BeFalse())
	g.Expect(result.CardData.Capabilities.Extensions).To(HaveLen(2))

	audit := result.CardData.Capabilities.Extensions[0]
	g.Expect(audit.URI).To(Equal("urn:ext:audit"))
	g.Expect(audit.Description).To(Equal("Audit trail"))
	g.Expect(*audit.Required).To(BeFalse())
	g.Expect(audit.Params).To(HaveKeyWithValue("retention", apiextensionsv1.JSON{Raw: json.RawMessage(`"30d"`)}))

	metrics := result.CardData.Capabilities.Extensions[1]
	g.Expect(metrics.URI).To(Equal("urn:ext:metrics"))
	g.Expect(metrics.Description).To(Equal("Metrics collection"))
	g.Expect(metrics.Required).To(BeNil())
	g.Expect(metrics.Params).To(BeEmpty())

	// Existing fields still work
	g.Expect(result.CardData.DefaultInputModes).To(Equal([]string{"text", "application/json"}))
	g.Expect(result.CardData.DefaultOutputModes).To(Equal([]string{"text"}))
	g.Expect(result.CardData.Skills).To(HaveLen(1))
	g.Expect(result.CardData.Skills[0].Name).To(Equal("summarize"))
	g.Expect(*result.CardData.SupportsAuthenticatedExtendedCard).To(BeTrue())
}

func TestConfigMapFetcher_ConfigMapFound(t *testing.T) {
	g := NewGomegaWithT(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent" + SignedCardConfigMapSuffix,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			SignedCardLegacyConfigMapKey: testAgentCardJSON,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(newFakeScheme()).
		WithObjects(cm).
		Build()
	fetcher := NewConfigMapFetcher(fakeClient)

	res, err := fetcher.Fetch(context.Background(), A2AProtocol, "", "my-agent", "test-ns")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.CardData.Name).To(Equal("test-agent"))
	g.Expect(res.CardData.Version).To(Equal("1.0"))
}

func TestConfigMapFetcher_ConfigMapNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == A2AAgentCardPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(testAgentCardJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newFakeScheme()).
		Build()
	fetcher := NewConfigMapFetcher(fakeClient)

	res, err := fetcher.Fetch(context.Background(), A2AProtocol, srv.URL, "no-such-agent", "test-ns")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.CardData.Name).To(Equal("test-agent"))
}

func TestConfigMapFetcher_MissingKey(t *testing.T) {
	g := NewGomegaWithT(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-agent" + SignedCardConfigMapSuffix,
			Namespace: "test-ns",
		},
		Data: map[string]string{},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == A2AAgentCardPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(testAgentCardJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newFakeScheme()).
		WithObjects(cm).
		Build()
	fetcher := NewConfigMapFetcher(fakeClient)

	res, err := fetcher.Fetch(context.Background(), A2AProtocol, srv.URL, "empty-agent", "test-ns")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.CardData.Name).To(Equal("test-agent"))
}

func TestConfigMapFetcher_InvalidJSON(t *testing.T) {
	g := NewGomegaWithT(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-agent" + SignedCardConfigMapSuffix,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			SignedCardLegacyConfigMapKey: "not valid json{{{",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == A2AAgentCardPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(testAgentCardJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newFakeScheme()).
		WithObjects(cm).
		Build()
	fetcher := NewConfigMapFetcher(fakeClient)

	res, err := fetcher.Fetch(context.Background(), A2AProtocol, srv.URL, "bad-agent", "test-ns")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.CardData.Name).To(Equal("test-agent"))
}
