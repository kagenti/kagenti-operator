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

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = agentv1alpha1.AddToScheme(s)
	return s
}

func validAgentCard() *agentv1alpha1.AgentCard {
	return &agentv1alpha1.AgentCard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-card",
			Namespace: "default",
		},
		Spec: agentv1alpha1.AgentCardSpec{
			TargetRef: &agentv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test",
			},
		},
	}
}

func fakeReader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		Build()
}

func TestAgentCardValidator_ValidateCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("with targetRef succeeds", func(t *testing.T) {
		v := &AgentCardValidator{}
		_, err := v.ValidateCreate(ctx, validAgentCard())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("without targetRef returns error", func(t *testing.T) {
		v := &AgentCardValidator{}
		_, err := v.ValidateCreate(ctx, &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{Name: "no-ref", Namespace: "default"},
		})
		if err == nil {
			t.Fatal("expected error for missing targetRef")
		}
		if !strings.Contains(err.Error(), "targetRef is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate targetRef is rejected", func(t *testing.T) {
		existing := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-card",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentCardSpec{
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "test",
				},
			},
		}
		v := &AgentCardValidator{Reader: fakeReader(existing)}

		_, err := v.ValidateCreate(ctx, validAgentCard())
		if err == nil {
			t.Fatal("expected error for duplicate targetRef")
		}
		if !strings.Contains(err.Error(), "an AgentCard already targets") {
			t.Errorf("unexpected error message: %v", err)
		}
		if !strings.Contains(err.Error(), "existing-card") {
			t.Errorf("error should reference the existing card name: %v", err)
		}
	})

	t.Run("no duplicate when targeting different workload", func(t *testing.T) {
		existing := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-card",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentCardSpec{
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "other-workload",
				},
			},
		}
		v := &AgentCardValidator{Reader: fakeReader(existing)}

		_, err := v.ValidateCreate(ctx, validAgentCard())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no duplicate when targeting different kind", func(t *testing.T) {
		existing := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sts-card",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentCardSpec{
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "test",
				},
			},
		}
		v := &AgentCardValidator{Reader: fakeReader(existing)}

		_, err := v.ValidateCreate(ctx, validAgentCard())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("nil reader skips duplicate check", func(t *testing.T) {
		v := &AgentCardValidator{Reader: nil}
		_, err := v.ValidateCreate(ctx, validAgentCard())
		if err != nil {
			t.Errorf("unexpected error with nil reader: %v", err)
		}
	})
}

func TestAgentCardValidator_ValidateUpdate(t *testing.T) {
	ctx := context.Background()
	old := validAgentCard()

	t.Run("with targetRef succeeds", func(t *testing.T) {
		v := &AgentCardValidator{}
		_, err := v.ValidateUpdate(ctx, old, validAgentCard())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("without targetRef returns error", func(t *testing.T) {
		v := &AgentCardValidator{}
		_, err := v.ValidateUpdate(ctx, old, &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{Name: "no-ref", Namespace: "default"},
		})
		if err == nil {
			t.Fatal("expected error for missing targetRef")
		}
		if !strings.Contains(err.Error(), "targetRef is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("update to duplicate targetRef is rejected", func(t *testing.T) {
		existing := &agentv1alpha1.AgentCard{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-card",
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentCardSpec{
				TargetRef: &agentv1alpha1.TargetRef{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "taken-workload",
				},
			},
		}
		v := &AgentCardValidator{Reader: fakeReader(existing)}

		updated := validAgentCard()
		updated.Spec.TargetRef.Name = "taken-workload"

		_, err := v.ValidateUpdate(ctx, old, updated)
		if err == nil {
			t.Fatal("expected error for duplicate targetRef on update")
		}
		if !strings.Contains(err.Error(), "an AgentCard already targets") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("update same card same targetRef succeeds", func(t *testing.T) {
		self := validAgentCard()
		v := &AgentCardValidator{Reader: fakeReader(self)}

		_, err := v.ValidateUpdate(ctx, self, self)
		if err != nil {
			t.Errorf("unexpected error updating own targetRef: %v", err)
		}
	})
}

func TestAgentCardValidator_ValidateDelete(t *testing.T) {
	v := &AgentCardValidator{}
	ctx := context.Background()

	t.Run("with valid AgentCard succeeds", func(t *testing.T) {
		_, err := v.ValidateDelete(ctx, validAgentCard())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

}
