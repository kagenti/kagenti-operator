package workload

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// OwnerInfo describes the workload that owns a Pod.
type OwnerInfo struct {
	Name string
	Kind string // "Deployment", "StatefulSet", "Sandbox", or "" if unknown
}

// ResolveOwner determines which top-level workload (Deployment,
// StatefulSet, Sandbox) owns a Pod by inspecting ownerReferences and labels.
//
// For Deployment pods the chain is Pod → ReplicaSet → Deployment;
// the Deployment name is recovered from the ReplicaSet name by
// stripping the pod-template-hash suffix.
//
// For StatefulSet and Sandbox pods the ownerReference points directly to the
// workload (only controller ownerReferences are considered).
//
// Returns empty OwnerInfo when ownership cannot be determined.
func ResolveOwner(pod *corev1.Pod) OwnerInfo {
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "StatefulSet":
			return OwnerInfo{Name: ref.Name, Kind: "StatefulSet"}
		case "Sandbox":
			// agent-sandbox (agents.x-k8s.io) workloads. The Sandbox name is the
			// workload name an AgentRuntime targetRef points at, so key off the
			// ownerRef rather than the pod name. Mirrors IsPodOwnedBy below.
			return OwnerInfo{Name: ref.Name, Kind: "Sandbox"}
		case "ReplicaSet":
			name := deploymentNameFromReplicaSet(ref.Name, pod.Labels)
			if name != "" {
				return OwnerInfo{Name: name, Kind: "Deployment"}
			}
			return OwnerInfo{Name: ref.Name, Kind: ""}
		}
	}

	// No recognized controller ownerReference — fall back to
	// GenerateName / Name heuristics for backward compatibility.
	if pod.GenerateName != "" {
		rsName := strings.TrimRight(pod.GenerateName, "-")
		if hash, ok := pod.Labels["pod-template-hash"]; ok && hash != "" {
			suffix := "-" + hash
			if strings.HasSuffix(rsName, suffix) {
				if name := strings.TrimSuffix(rsName, suffix); name != "" {
					return OwnerInfo{Name: name, Kind: "Deployment"}
				}
			}
		}
		return OwnerInfo{Name: rsName, Kind: ""}
	}
	if pod.Name != "" {
		return OwnerInfo{Name: pod.Name, Kind: ""}
	}
	return OwnerInfo{}
}

// IsPodOwnedBy returns true if the pod is owned (directly or via a
// ReplicaSet) by the named workload. Only controller ownerReferences
// are considered, consistent with ResolveOwner. Supports Deployment
// (via ReplicaSet), StatefulSet, and Sandbox ownership chains.
func IsPodOwnedBy(pod *corev1.Pod, workloadName string) bool {
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "ReplicaSet":
			if idx := strings.LastIndex(ref.Name, "-"); idx > 0 && ref.Name[:idx] == workloadName {
				return true
			}
		case "StatefulSet", "Sandbox":
			if ref.Name == workloadName {
				return true
			}
		}
	}
	return false
}

func deploymentNameFromReplicaSet(rsName string, labels map[string]string) string {
	if hash, ok := labels["pod-template-hash"]; ok && hash != "" {
		suffix := "-" + hash
		if strings.HasSuffix(rsName, suffix) {
			return strings.TrimSuffix(rsName, suffix)
		}
	}
	return ""
}
