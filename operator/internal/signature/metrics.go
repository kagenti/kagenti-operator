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

package signature

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	SignatureVerificationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "a2a_signature_verification_total",
			Help: "Total A2A signature verifications",
		},
		[]string{"provider", "result", "audit_mode"},
	)

	SignatureVerificationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "a2a_signature_verification_duration_seconds",
			Help:    "Duration of A2A signature verifications",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"provider"},
	)

	SignatureVerificationErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "a2a_signature_verification_errors_total",
			Help: "Total A2A signature verification errors",
		},
		[]string{"provider", "error_type"},
	)

	SigstoreVerificationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rossoctl_sigstore_verification_total",
			Help: "Sigstore SignedAgentCard bundle verification attempts",
		},
		[]string{"result", "reason"},
	)

	SigstoreVerificationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rossoctl_sigstore_verification_duration_seconds",
			Help:    "Sigstore bundle verification latency",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"provider"},
	)

	SigstoreTrustedRootAgeSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "rossoctl_sigstore_trusted_root_age_seconds",
			Help: "Age of the cached Sigstore trusted root material",
		},
	)

	SLSAProvenanceVerifiedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rossoctl_slsa_provenance_verified_total",
			Help: "SLSA provenance bundle observations after Sigstore verify",
		},
		[]string{"result"},
	)
)

func init() {
	for _, c := range []prometheus.Collector{
		SignatureVerificationTotal,
		SignatureVerificationDuration,
		SignatureVerificationErrors,
		SigstoreVerificationTotal,
		SigstoreVerificationDuration,
		SigstoreTrustedRootAgeSeconds,
		SLSAProvenanceVerifiedTotal,
	} {
		if err := metrics.Registry.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

func RecordVerification(provider string, verified bool, auditMode bool) {
	result := "failed"
	if verified {
		result = "success"
	}
	audit := "false"
	if auditMode {
		audit = "true"
	}
	SignatureVerificationTotal.WithLabelValues(provider, result, audit).Inc()
}

func RecordError(provider string, errorType string) {
	SignatureVerificationErrors.WithLabelValues(provider, errorType).Inc()
}

func RecordSigstoreVerification(success bool, reason string) {
	res := "failure"
	if success {
		res = "success"
	}
	SigstoreVerificationTotal.WithLabelValues(res, reason).Inc()
}

func ObserveSigstoreTrustedRootAge(seconds float64) {
	SigstoreTrustedRootAgeSeconds.Set(seconds)
}

func RecordSLSAProvenance(result string) {
	SLSAProvenanceVerifiedTotal.WithLabelValues(result).Inc()
}
