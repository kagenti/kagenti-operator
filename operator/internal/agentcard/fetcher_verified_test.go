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
	"crypto/x509"
	"net/url"
	"testing"

	"github.com/onsi/gomega"
)

func TestGetSecureServiceURL(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	g.Expect(GetSecureServiceURL("my-agent", "default", 8443)).
		To(gomega.Equal("https://my-agent.default.svc.cluster.local:8443"))
}

func TestSpiffeIDFromCert_ValidSPIFFE(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	spiffeURI, _ := url.Parse("spiffe://example.org/ns/team1/sa/weather-agent")
	cert := &x509.Certificate{
		URIs: []*url.URL{spiffeURI},
	}
	g.Expect(spiffeIDFromCert(cert)).To(gomega.Equal("spiffe://example.org/ns/team1/sa/weather-agent"))
}

func TestSpiffeIDFromCert_NoURISANs(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	cert := &x509.Certificate{}
	g.Expect(spiffeIDFromCert(cert)).To(gomega.BeEmpty())
}

func TestSpiffeIDFromCert_NonSpiffeURI(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	httpURI, _ := url.Parse("https://example.com/agent")
	cert := &x509.Certificate{
		URIs: []*url.URL{httpURI},
	}
	g.Expect(spiffeIDFromCert(cert)).To(gomega.BeEmpty())
}

func TestSpiffeIDFromCert_MultipleURIs(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	httpURI, _ := url.Parse("https://example.com/agent")
	spiffeURI, _ := url.Parse("spiffe://trust.domain/workload")
	cert := &x509.Certificate{
		URIs: []*url.URL{httpURI, spiffeURI},
	}
	g.Expect(spiffeIDFromCert(cert)).To(gomega.Equal("spiffe://trust.domain/workload"))
}

func TestNewSpiffeFetcher(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	fetcher, err := NewSpiffeFetcher(nil, "example.org")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(fetcher).NotTo(gomega.BeNil())
	g.Expect(fetcher.trustDomain).To(gomega.Equal("example.org"))
}

func TestNewSpiffeFetcher_InvalidTrustDomain(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	_, err := NewSpiffeFetcher(nil, "")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid SPIFFE trust domain"))
}

func TestSpiffeFetcher_UnsupportedProtocol(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	fetcher, err := NewSpiffeFetcher(nil, "example.org")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = fetcher.FetchAuthenticated(t.Context(), "unsupported", "https://example.com")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("unsupported protocol"))
}
