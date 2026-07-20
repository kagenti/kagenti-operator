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

package controller

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const TargetRefNameIndex = ".spec.targetRef.name"

var (
	registerTargetRefIndexOnce sync.Once
	registerTargetRefIndexErr  error
)

// RegisterAgentCardTargetRefIndex registers a field indexer on .spec.targetRef.name (idempotent).
func RegisterAgentCardTargetRefIndex(mgr ctrl.Manager) error {
	registerTargetRefIndexOnce.Do(func() {
		registerTargetRefIndexErr = mgr.GetFieldIndexer().IndexField(
			context.Background(),
			&agentv1alpha1.AgentCard{},
			TargetRefNameIndex,
			func(obj client.Object) []string {
				card, ok := obj.(*agentv1alpha1.AgentCard)
				if !ok {
					return nil
				}
				if card.Spec.TargetRef != nil && card.Spec.TargetRef.Name != "" {
					return []string{card.Spec.TargetRef.Name}
				}
				return nil
			},
		)
		if registerTargetRefIndexErr != nil {
			registerTargetRefIndexErr = fmt.Errorf("failed to create field indexer for %s: %w",
				TargetRefNameIndex, registerTargetRefIndexErr)
		}
	})
	return registerTargetRefIndexErr
}

func mapWorkloadToAgentCards(lister client.Reader, apiVersion, kind string, log logr.Logger) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if !isAgentWorkload(obj.GetLabels()) {
			return nil
		}

		agentCardList := &agentv1alpha1.AgentCardList{}
		if err := lister.List(ctx, agentCardList,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{TargetRefNameIndex: obj.GetName()},
		); err != nil {
			log.Error(err, "Failed to list AgentCards for mapping")
			return nil
		}

		var requests []reconcile.Request
		for _, card := range agentCardList.Items {
			if card.Spec.TargetRef != nil &&
				card.Spec.TargetRef.Kind == kind &&
				card.Spec.TargetRef.APIVersion == apiVersion {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      card.Name,
						Namespace: card.Namespace,
					},
				})
			}
		}
		return requests
	}
}
