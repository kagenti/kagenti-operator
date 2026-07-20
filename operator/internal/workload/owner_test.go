package workload

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolPtr(b bool) *bool { return &b }

func TestResolveOwner_StatefulSet(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-sts-2",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "my-sts", Controller: boolPtr(true)},
			},
		},
	}
	info := ResolveOwner(pod)
	if info.Name != "my-sts" || info.Kind != "StatefulSet" {
		t.Errorf("expected (my-sts, StatefulSet), got (%s, %s)", info.Name, info.Kind)
	}
}

func TestResolveOwner_Sandbox(t *testing.T) {
	// A Sandbox pod's controller ownerRef carries the Sandbox (workload) name,
	// which is what an AgentRuntime targetRef points at. Resolving Kind="Sandbox"
	// makes override matching exact instead of relying on the pod-name fallback.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "weather-agent",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Sandbox", Name: "weather-agent", Controller: boolPtr(true)},
			},
		},
	}
	info := ResolveOwner(pod)
	if info.Name != "weather-agent" || info.Kind != "Sandbox" {
		t.Errorf("expected (weather-agent, Sandbox), got (%s, %s)", info.Name, info.Kind)
	}
}

func TestResolveOwner_StatefulSet_NonController_Ignored(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-sts-0",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "my-sts", Controller: boolPtr(false)},
			},
		},
	}
	info := ResolveOwner(pod)
	if info.Kind == "StatefulSet" {
		t.Error("non-controller StatefulSet ownerRef should be ignored")
	}
}

func TestResolveOwner_Deployment(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "my-deploy-7d4f8b9c5-",
			Labels:       map[string]string{"pod-template-hash": "7d4f8b9c5"},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-deploy-7d4f8b9c5", Controller: boolPtr(true)},
			},
		},
	}
	info := ResolveOwner(pod)
	if info.Name != "my-deploy" || info.Kind != "Deployment" {
		t.Errorf("expected (my-deploy, Deployment), got (%s, %s)", info.Name, info.Kind)
	}
}

func TestResolveOwner_BarePod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "standalone-pod",
		},
	}
	info := ResolveOwner(pod)
	if info.Name != "standalone-pod" || info.Kind != "" {
		t.Errorf("expected (standalone-pod, ''), got (%s, %s)", info.Name, info.Kind)
	}
}

func TestResolveOwner_GenerateName_NoHash(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "myapp-",
		},
	}
	info := ResolveOwner(pod)
	if info.Name != "myapp" || info.Kind != "" {
		t.Errorf("expected (myapp, ''), got (%s, %s)", info.Name, info.Kind)
	}
}

func TestIsPodOwnedBy_Deployment(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-deploy-abc123", Controller: boolPtr(true)},
			},
		},
	}
	if !IsPodOwnedBy(pod, "my-deploy") {
		t.Error("expected pod to be owned by my-deploy")
	}
	if IsPodOwnedBy(pod, "other-deploy") {
		t.Error("expected pod NOT to be owned by other-deploy")
	}
}

func TestIsPodOwnedBy_StatefulSet(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "my-sts", Controller: boolPtr(true)},
			},
		},
	}
	if !IsPodOwnedBy(pod, "my-sts") {
		t.Error("expected pod to be owned by my-sts")
	}
}

func TestIsPodOwnedBy_Sandbox(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Sandbox", Name: "my-sandbox", Controller: boolPtr(true)},
			},
		},
	}
	if !IsPodOwnedBy(pod, "my-sandbox") {
		t.Error("expected pod to be owned by my-sandbox")
	}
}
