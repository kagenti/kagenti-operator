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
	"fmt"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var agentcardlog = ctrl.Log.WithName("agentcard-webhook")

func SetupAgentCardWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &agentv1alpha1.AgentCard{}).
		WithValidator(&AgentCardValidator{Reader: mgr.GetAPIReader()}).
		Complete()
}

//+kubebuilder:webhook:path=/validate-agent-rossoctl-dev-v1alpha1-agentcard,mutating=false,failurePolicy=fail,sideEffects=None,groups=agent.rossoctl.dev,resources=agentcards,verbs=create;update,versions=v1alpha1,name=vagentcard.kb.io,admissionReviewVersions=v1

type AgentCardValidator struct {
	// Reader is an uncached client for authoritative reads from the API server.
	// Used for duplicate targetRef checks during admission. Nil-safe: the check
	// is skipped when Reader is nil (e.g., in unit tests without a real API server).
	Reader client.Reader
}

func (v *AgentCardValidator) ValidateCreate(ctx context.Context, agentcard *agentv1alpha1.AgentCard) (admission.Warnings, error) {
	agentcardlog.Info("validate create", "name", agentcard.Name)

	warnings, err := v.validateAgentCard(agentcard)
	if err != nil {
		return warnings, err
	}

	if err := v.checkDuplicateTargetRef(ctx, agentcard); err != nil {
		return warnings, err
	}

	return warnings, nil
}

func (v *AgentCardValidator) ValidateUpdate(ctx context.Context, _ *agentv1alpha1.AgentCard, agentcard *agentv1alpha1.AgentCard) (admission.Warnings, error) {
	agentcardlog.Info("validate update", "name", agentcard.Name)

	warnings, err := v.validateAgentCard(agentcard)
	if err != nil {
		return warnings, err
	}

	if err := v.checkDuplicateTargetRef(ctx, agentcard); err != nil {
		return warnings, err
	}

	return warnings, nil
}

func (v *AgentCardValidator) ValidateDelete(_ context.Context, agentcard *agentv1alpha1.AgentCard) (admission.Warnings, error) {
	agentcardlog.Info("validate delete", "name", agentcard.Name)

	return nil, nil
}

func (v *AgentCardValidator) validateAgentCard(agentcard *agentv1alpha1.AgentCard) (admission.Warnings, error) {
	var warnings admission.Warnings

	// spec.targetRef is required
	if agentcard.Spec.TargetRef == nil {
		return nil, fmt.Errorf("spec.targetRef is required: specify the workload backing this agent")
	}

	return warnings, nil
}

// checkDuplicateTargetRef rejects creation if another AgentCard already targets
// the same workload (apiVersion + kind + name) in the same namespace. This runs
// as an authoritative API server read, eliminating the informer cache-lag race
// that previously required a grace period in the Sync controller.
func (v *AgentCardValidator) checkDuplicateTargetRef(ctx context.Context, agentcard *agentv1alpha1.AgentCard) error {
	if v.Reader == nil {
		return nil
	}

	ref := agentcard.Spec.TargetRef
	if ref == nil {
		return nil
	}

	cardList := &agentv1alpha1.AgentCardList{}
	// fail-open: allow creation if we can't verify uniqueness
	if err := v.Reader.List(ctx, cardList, client.InNamespace(agentcard.Namespace)); err != nil {
		agentcardlog.Error(err, "failed to list AgentCards for duplicate check")
		return nil
	}

	for i := range cardList.Items {
		existing := &cardList.Items[i]
		if existing.Name == agentcard.Name {
			continue
		}
		if existing.Spec.TargetRef != nil &&
			existing.Spec.TargetRef.APIVersion == ref.APIVersion &&
			existing.Spec.TargetRef.Kind == ref.Kind &&
			existing.Spec.TargetRef.Name == ref.Name {
			return fmt.Errorf(
				"an AgentCard already targets %s %s in namespace %s: %s",
				ref.Kind, ref.Name, agentcard.Namespace, existing.Name,
			)
		}
	}

	return nil
}
