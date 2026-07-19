/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package clientreg holds the naming and eligibility logic shared by the AuthBridge mutating
// webhook and the ClientRegistration controller. Both sides must agree on (a) the Secret name
// and (b) whether a workload is eligible for operator-managed Keycloak client registration,
// so the webhook can pre-populate the pod annotation at admission without waiting for the
// controller to run. Centralizing these prevents the two sides from drifting.
package clientreg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// LabelAgentType distinguishes agents from tools; operator-managed registration runs for
	// agents unconditionally and for tools only when the injectTools feature gate is on.
	LabelAgentType  = "rossoctl.io/type"
	LabelValueAgent = "agent"
	// LabelValueTool matches agentv1alpha1.RuntimeTypeTool — kept here as a string to avoid
	// importing the API package from this leaf utility.
	LabelValueTool = "tool"

	// AnnotationKeycloakClientSecretName is set on workload pod templates (by the controller)
	// and pre-populated on new pods (by the webhook) to signal the name of the Secret holding
	// Keycloak client credentials. The webhook mounts that Secret into /shared/client-id.txt
	// and /shared/client-secret.txt for any container that already mounts the shared-data volume.
	AnnotationKeycloakClientSecretName = "rossoctl.io/keycloak-client-credentials-secret-name"
)

// KeycloakClientCredentialsSecretName returns the deterministic name of the Secret the
// ClientRegistration controller produces for (namespace, workload). It is a pure function of
// those inputs only — the webhook can compute it at admission time without consulting the
// API server, so a Secret volume can be declared before the controller has run. Kubelet will
// retry the mount until the Secret appears.
func KeycloakClientCredentialsSecretName(namespace, workload string) string {
	sum := sha256.Sum256([]byte(namespace + "\000" + workload + "\000rossoctl-keycloak-client-credentials"))
	return "rossoctl-keycloak-client-credentials-" + hex.EncodeToString(sum[:8])
}

// SkipReason returns a non-empty human-readable reason when operator-managed client registration
// should not run for the workload. Empty string means "proceed". Both the controller's
// reconcileOne and the webhook's admission handler use this to stay in lockstep.
func SkipReason(labels map[string]string, injectTools bool) string {
	if labels == nil {
		return "pod template has no labels"
	}
	switch labels[LabelAgentType] {
	case LabelValueAgent:
		return ""
	case LabelValueTool:
		if !injectTools {
			return "rossoctl.io/type is tool but cluster injectTools feature gate is disabled"
		}
		return ""
	default:
		t := labels[LabelAgentType]
		if t == "" {
			return "rossoctl.io/type label is missing or not agent/tool"
		}
		return fmt.Sprintf("rossoctl.io/type=%q is not agent or tool", t)
	}
}

// WorkloadWantsOperatorClientReg returns true when the workload's labels and the cluster
// injectTools gate both permit operator-managed client registration.
func WorkloadWantsOperatorClientReg(labels map[string]string, injectTools bool) bool {
	return SkipReason(labels, injectTools) == ""
}
