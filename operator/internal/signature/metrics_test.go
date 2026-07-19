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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func getCounterValue(cv *prometheus.CounterVec, labels ...string) float64 {
	m := &dto.Metric{}
	c, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		return 0
	}
	_ = c.Write(m)
	return m.GetCounter().GetValue()
}

func TestRecordVerification_Success(t *testing.T) {
	before := getCounterValue(SignatureVerificationTotal, "x5c", "success", "false")
	RecordVerification("x5c", true, false)
	after := getCounterValue(SignatureVerificationTotal, "x5c", "success", "false")
	if after-before != 1 {
		t.Errorf("expected counter to increment by 1, got delta %f", after-before)
	}
}

func TestRecordVerification_Failed(t *testing.T) {
	before := getCounterValue(SignatureVerificationTotal, "x5c", "failed", "false")
	RecordVerification("x5c", false, false)
	after := getCounterValue(SignatureVerificationTotal, "x5c", "failed", "false")
	if after-before != 1 {
		t.Errorf("expected counter to increment by 1, got delta %f", after-before)
	}
}

func TestRecordVerification_AuditMode(t *testing.T) {
	before := getCounterValue(SignatureVerificationTotal, "x5c", "success", "true")
	RecordVerification("x5c", true, true)
	after := getCounterValue(SignatureVerificationTotal, "x5c", "success", "true")
	if after-before != 1 {
		t.Errorf("expected counter to increment by 1, got delta %f", after-before)
	}
}

func TestRecordError(t *testing.T) {
	before := getCounterValue(SignatureVerificationErrors, "x5c", "parse_error")
	RecordError("x5c", "parse_error")
	after := getCounterValue(SignatureVerificationErrors, "x5c", "parse_error")
	if after-before != 1 {
		t.Errorf("expected counter to increment by 1, got delta %f", after-before)
	}
}
