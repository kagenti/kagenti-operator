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

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func validAgentRuntime() *agentv1alpha1.AgentRuntime {
	return &agentv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-runtime",
			Namespace: "default",
		},
		Spec: agentv1alpha1.AgentRuntimeSpec{
			Type: agentv1alpha1.RuntimeTypeAgent,
			TargetRef: agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test",
			},
		},
	}
}

func fakeRuntimeReader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		Build()
}

func TestAgentRuntimeValidator_ValidateCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("valid runtime succeeds", func(t *testing.T) {
		v := &AgentRuntimeValidator{}
		_, err := v.ValidateCreate(ctx, validAgentRuntime())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate targetRef is rejected", func(t *testing.T) {
		existing := &agentv1alpha1.AgentRuntime{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-runtime",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentRuntimeSpec{
				Type: agentv1alpha1.RuntimeTypeTool,
				TargetRef: agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "test",
				},
			},
		}
		v := &AgentRuntimeValidator{Reader: fakeRuntimeReader(existing)}

		_, err := v.ValidateCreate(ctx, validAgentRuntime())
		if err == nil {
			t.Fatal("expected error for duplicate targetRef")
		}
		if !strings.Contains(err.Error(), "an AgentRuntime already targets") {
			t.Errorf("unexpected error message: %v", err)
		}
		if !strings.Contains(err.Error(), "existing-runtime") {
			t.Errorf("error should reference the existing runtime name: %v", err)
		}
	})

	t.Run("no duplicate when targeting different workload", func(t *testing.T) {
		existing := &agentv1alpha1.AgentRuntime{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-runtime",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentRuntimeSpec{
				Type: agentv1alpha1.RuntimeTypeAgent,
				TargetRef: agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "other-workload",
				},
			},
		}
		v := &AgentRuntimeValidator{Reader: fakeRuntimeReader(existing)}

		_, err := v.ValidateCreate(ctx, validAgentRuntime())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no duplicate when targeting different kind", func(t *testing.T) {
		existing := &agentv1alpha1.AgentRuntime{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sts-runtime",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentRuntimeSpec{
				Type: agentv1alpha1.RuntimeTypeAgent,
				TargetRef: agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "test",
				},
			},
		}
		v := &AgentRuntimeValidator{Reader: fakeRuntimeReader(existing)}

		_, err := v.ValidateCreate(ctx, validAgentRuntime())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("nil reader skips duplicate check", func(t *testing.T) {
		v := &AgentRuntimeValidator{Reader: nil}
		_, err := v.ValidateCreate(ctx, validAgentRuntime())
		if err != nil {
			t.Errorf("unexpected error with nil reader: %v", err)
		}
	})

	t.Run("list error fails open", func(t *testing.T) {
		// Reader without AgentRuntime registered in scheme causes list to fail
		emptyScheme := runtime.NewScheme()
		brokenReader := fake.NewClientBuilder().WithScheme(emptyScheme).Build()
		v := &AgentRuntimeValidator{Reader: brokenReader}

		_, err := v.ValidateCreate(ctx, validAgentRuntime())
		if err != nil {
			t.Errorf("expected fail-open on list error, got: %v", err)
		}
	})

	t.Run("no duplicate when in different namespace", func(t *testing.T) {
		existing := &agentv1alpha1.AgentRuntime{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-runtime",
				Namespace: "other-ns",
			},
			Spec: agentv1alpha1.AgentRuntimeSpec{
				Type: agentv1alpha1.RuntimeTypeAgent,
				TargetRef: agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "test",
				},
			},
		}
		v := &AgentRuntimeValidator{Reader: fakeRuntimeReader(existing)}

		_, err := v.ValidateCreate(ctx, validAgentRuntime())
		if err != nil {
			t.Errorf("unexpected error for different namespace: %v", err)
		}
	})
}

func TestAgentRuntimeValidator_ValidateUpdate(t *testing.T) {
	ctx := context.Background()
	old := validAgentRuntime()

	t.Run("valid update succeeds", func(t *testing.T) {
		v := &AgentRuntimeValidator{}
		_, err := v.ValidateUpdate(ctx, old, validAgentRuntime())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("update to duplicate targetRef is rejected", func(t *testing.T) {
		existing := &agentv1alpha1.AgentRuntime{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-runtime",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentRuntimeSpec{
				Type: agentv1alpha1.RuntimeTypeAgent,
				TargetRef: agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "taken-workload",
				},
			},
		}
		v := &AgentRuntimeValidator{Reader: fakeRuntimeReader(existing)}

		updated := validAgentRuntime()
		updated.Spec.TargetRef.Name = "taken-workload"

		_, err := v.ValidateUpdate(ctx, old, updated)
		if err == nil {
			t.Fatal("expected error for duplicate targetRef on update")
		}
		if !strings.Contains(err.Error(), "an AgentRuntime already targets") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("update same runtime same targetRef succeeds", func(t *testing.T) {
		self := validAgentRuntime()
		v := &AgentRuntimeValidator{Reader: fakeRuntimeReader(self)}

		_, err := v.ValidateUpdate(ctx, self, self)
		if err != nil {
			t.Errorf("unexpected error updating own targetRef: %v", err)
		}
	})
}

func TestAgentRuntimeValidator_ValidateDelete(t *testing.T) {
	v := &AgentRuntimeValidator{}
	ctx := context.Background()

	t.Run("with valid AgentRuntime succeeds", func(t *testing.T) {
		_, err := v.ValidateDelete(ctx, validAgentRuntime())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

}

// TestAgentRuntimeValidator_MTLSCompatWithMode covers the
// authBridgeMode + mtlsMode compatibility matrix. All combinations
// of {proxy-sidecar, lite, envoy-sidecar} × {disabled, permissive,
// strict} are now valid — envoy-sidecar mtls is supported via
// per-agent envoy-config rendering with TLS blocks
// (DownstreamTlsContext / UpstreamTlsContext). Empty authBridgeMode
// also admits across the matrix (resolution chain picks a default).
func TestAgentRuntimeValidator_MTLSCompatWithMode(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		mode    string
		mtls    string
		wantErr bool
	}{
		{"proxy-sidecar + strict allowed", "proxy-sidecar", "strict", false},
		{"proxy-sidecar + permissive allowed", "proxy-sidecar", "permissive", false},
		{"proxy-sidecar + disabled allowed", "proxy-sidecar", "disabled", false},
		{"proxy-sidecar + empty allowed", "proxy-sidecar", "", false},
		{"lite + strict allowed", "lite", "strict", false},
		{"lite + permissive allowed", "lite", "permissive", false},
		{"empty mode + strict allowed", "", "strict", false},
		{"envoy-sidecar + disabled allowed", "envoy-sidecar", "disabled", false},
		{"envoy-sidecar + empty allowed", "envoy-sidecar", "", false},
		// envoy-sidecar + permissive/strict — newly supported. Locked
		// in here so a future regression that re-introduces the gate
		// gets caught by tests instead of breaking the user-facing
		// Spec.MTLSMode contract.
		{"envoy-sidecar + permissive allowed", "envoy-sidecar", "permissive", false},
		{"envoy-sidecar + strict allowed", "envoy-sidecar", "strict", false},
	}

	for _, tt := range tests {
		t.Run("create/"+tt.name, func(t *testing.T) {
			rt := validAgentRuntime()
			rt.Spec.AuthBridgeMode = tt.mode
			rt.Spec.MTLSMode = tt.mtls

			v := &AgentRuntimeValidator{}
			_, err := v.ValidateCreate(ctx, rt)
			gotErr := err != nil
			if gotErr != tt.wantErr {
				t.Errorf("ValidateCreate(mode=%q, mtls=%q): wantErr=%v, gotErr=%v (err=%v)",
					tt.mode, tt.mtls, tt.wantErr, gotErr, err)
			}
		})

		t.Run("update/"+tt.name, func(t *testing.T) {
			old := validAgentRuntime()
			updated := validAgentRuntime()
			updated.Spec.AuthBridgeMode = tt.mode
			updated.Spec.MTLSMode = tt.mtls

			v := &AgentRuntimeValidator{}
			_, err := v.ValidateUpdate(ctx, old, updated)
			gotErr := err != nil
			if gotErr != tt.wantErr {
				t.Errorf("ValidateUpdate(mode=%q, mtls=%q): wantErr=%v, gotErr=%v (err=%v)",
					tt.mode, tt.mtls, tt.wantErr, gotErr, err)
			}
		})
	}
}

func TestCheckTLSBridgeCompatibleWithMode(t *testing.T) {
	cases := []struct {
		name        string
		bridge, abm string
		wantErr     bool
	}{
		{"enabled+envoy rejected", "enabled", "envoy-sidecar", true},
		{"enabled+waypoint rejected", "enabled", "waypoint", true},
		{"enabled+proxy-sidecar ok", "enabled", "proxy-sidecar", false},
		{"enabled+lite ok", "enabled", "lite", false},
		{"enabled+empty ok", "enabled", "", false},
		{"disabled+envoy ok", "disabled", "envoy-sidecar", false},
		{"unset bridge ok", "", "envoy-sidecar", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &agentv1alpha1.AgentRuntime{Spec: agentv1alpha1.AgentRuntimeSpec{
				TLSBridgeMode: tc.bridge, AuthBridgeMode: tc.abm,
			}}
			if err := checkTLSBridgeCompatibleWithMode(rt); (err != nil) != tc.wantErr {
				t.Errorf("got err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
