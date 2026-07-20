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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
)

const NetworkPolicyFinalizer = "agentcard.rossoctl.dev/network-policy"

var networkPolicyLogger = ctrl.Log.WithName("controller").WithName("AgentCardNetworkPolicy")

// AgentCardNetworkPolicyReconciler manages NetworkPolicies based on AgentCard verification status.
type AgentCardNetworkPolicyReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	EnforceNetworkPolicies bool
	// OperatorNamespace is the namespace the operator runs in. Used to allow
	// ingress from the operator in generated NetworkPolicies via the built-in
	// kubernetes.io/metadata.name label (no manual namespace labeling required).
	OperatorNamespace string
	// KubeAPIServerCIDRs are the /32 CIDRs of the K8s API server endpoints.
	// Populated at startup from the "kubernetes" Endpoints in the default namespace.
	// Used to allow workload egress to the API server in restrictive policies.
	KubeAPIServerCIDRs []string
}

// +kubebuilder:rbac:groups=core,resources=endpoints,verbs=get
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch

func (r *AgentCardNetworkPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	networkPolicyLogger.V(1).Info("Reconciling AgentCard NetworkPolicy", "namespacedName", req.NamespacedName)

	if !r.EnforceNetworkPolicies {
		return ctrl.Result{}, nil
	}

	agentCard := &agentv1alpha1.AgentCard{}
	err := r.Get(ctx, req.NamespacedName, agentCard)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !agentCard.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, agentCard)
	}

	if !controllerutil.ContainsFinalizer(agentCard, NetworkPolicyFinalizer) {
		controllerutil.AddFinalizer(agentCard, NetworkPolicyFinalizer)
		if err := r.Update(ctx, agentCard); err != nil {
			networkPolicyLogger.Error(err, "Failed to add finalizer to AgentCard")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	workloadName, podSelectorLabels, err := r.resolveWorkload(ctx, agentCard)
	if err != nil {
		networkPolicyLogger.Info("No workload resolved for AgentCard", "agentCard", agentCard.Name, "error", err)
		return ctrl.Result{}, nil
	}

	if err := r.manageNetworkPolicy(ctx, agentCard, workloadName, podSelectorLabels); err != nil {
		networkPolicyLogger.Error(err, "Failed to manage NetworkPolicy")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AgentCardNetworkPolicyReconciler) resolveWorkload(ctx context.Context, agentCard *agentv1alpha1.AgentCard) (string, map[string]string, error) {
	ref := agentCard.Spec.TargetRef
	if ref == nil {
		return "", nil, fmt.Errorf("spec.targetRef is required: specify the workload backing this agent")
	}
	podLabels, err := r.getPodTemplateLabels(ctx, agentCard.Namespace, ref)
	if err != nil {
		return "", nil, err
	}
	return ref.Name, podLabels, nil
}

func (r *AgentCardNetworkPolicyReconciler) getPodTemplateLabels(ctx context.Context, namespace string, ref *agentv1alpha1.TargetRef) (map[string]string, error) {
	key := types.NamespacedName{Name: ref.Name, Namespace: namespace}

	switch ref.Kind {
	case "Deployment":
		deployment := &appsv1.Deployment{}
		if err := r.Get(ctx, key, deployment); err != nil {
			return nil, err
		}
		return deployment.Spec.Template.Labels, nil

	case "StatefulSet":
		statefulset := &appsv1.StatefulSet{}
		if err := r.Get(ctx, key, statefulset); err != nil {
			return nil, err
		}
		return statefulset.Spec.Template.Labels, nil

	default:
		return map[string]string{
			LabelAgentType: LabelValueAgent,
			"app":          ref.Name,
		}, nil
	}
}

func (r *AgentCardNetworkPolicyReconciler) manageNetworkPolicy(ctx context.Context, agentCard *agentv1alpha1.AgentCard, workloadName string, podSelectorLabels map[string]string) error {
	policyName := fmt.Sprintf("%s-signature-policy", workloadName)

	isVerified := false

	verifiedCond := meta.FindStatusCondition(agentCard.Status.Conditions, ConditionVerified)
	if verifiedCond != nil {
		isVerified = verifiedCond.Status == metav1.ConditionTrue
	}

	if isVerified {
		return r.createPermissivePolicy(ctx, policyName, agentCard, podSelectorLabels)
	}
	return r.createRestrictivePolicy(ctx, policyName, agentCard, podSelectorLabels)
}

func (r *AgentCardNetworkPolicyReconciler) upsertNetworkPolicy(ctx context.Context, policyName string, agentCard *agentv1alpha1.AgentCard, spec netv1.NetworkPolicySpec) error {
	policy := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: agentCard.Namespace,
			Labels: map[string]string{
				"managed-by":               "rossoctl-operator",
				"rossoctl.dev/agentcard":   agentCard.Name,
				"rossoctl.dev/policy-type": "identity-verification",
			},
		},
		Spec: spec,
	}

	if err := controllerutil.SetControllerReference(agentCard, policy, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	existingPolicy := &netv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: policyName, Namespace: agentCard.Namespace}, existingPolicy)
	if err != nil {
		if apierrors.IsNotFound(err) {
			networkPolicyLogger.Info("Creating NetworkPolicy",
				"agentCard", agentCard.Name, "policy", policyName)
			return r.Create(ctx, policy)
		}
		return err
	}

	existingPolicy.Spec = spec
	existingPolicy.OwnerReferences = policy.OwnerReferences
	networkPolicyLogger.Info("Updating NetworkPolicy",
		"agentCard", agentCard.Name, "policy", policyName)
	return r.Update(ctx, existingPolicy)
}

func (r *AgentCardNetworkPolicyReconciler) createPermissivePolicy(
	ctx context.Context, policyName string,
	agentCard *agentv1alpha1.AgentCard, podSelectorLabels map[string]string,
) error {
	ingressRule := r.operatorIngressRule()
	ingressRule.From = append(ingressRule.From, netv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{LabelSignatureVerified: "true"},
		},
	})

	spec := netv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: podSelectorLabels},
		PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress, netv1.PolicyTypeEgress},
		Ingress:     []netv1.NetworkPolicyIngressRule{ingressRule},
		// Allow all egress for verified agents so they can reach external APIs and DNS.
		Egress: []netv1.NetworkPolicyEgressRule{{}},
	}
	return r.upsertNetworkPolicy(ctx, policyName, agentCard, spec)
}

func (r *AgentCardNetworkPolicyReconciler) operatorIngressRule() netv1.NetworkPolicyIngressRule {
	peers := []netv1.NetworkPolicyPeer{
		{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": r.OperatorNamespace},
			},
		},
	}
	if r.OperatorNamespace != ClusterDefaultsNamespace {
		peers = append(peers, netv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ClusterDefaultsNamespace},
			},
		})
	}
	return netv1.NetworkPolicyIngressRule{From: peers}
}

func (r *AgentCardNetworkPolicyReconciler) createRestrictivePolicy(
	ctx context.Context, policyName string,
	agentCard *agentv1alpha1.AgentCard,
	podSelectorLabels map[string]string,
) error {
	spec := netv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: podSelectorLabels},
		PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress, netv1.PolicyTypeEgress},
		Ingress:     []netv1.NetworkPolicyIngressRule{r.operatorIngressRule()},
		Egress:      r.kubeAPIEgressRules(),
	}
	return r.upsertNetworkPolicy(ctx, policyName, agentCard, spec)
}

// kubeAPIEgressRules returns egress rules that allow only traffic to the
// K8s API server endpoints on port 6443. This permits workloads to
// communicate with the API server while blocking all other outbound
// traffic. If no API server CIDRs are configured, returns an empty
// list (deny-all egress).
func (r *AgentCardNetworkPolicyReconciler) kubeAPIEgressRules() []netv1.NetworkPolicyEgressRule {
	if len(r.KubeAPIServerCIDRs) == 0 {
		return []netv1.NetworkPolicyEgressRule{}
	}

	apiPort := intstr.FromInt32(6443)
	tcp := corev1.ProtocolTCP
	peers := make([]netv1.NetworkPolicyPeer, 0, len(r.KubeAPIServerCIDRs))
	for _, cidr := range r.KubeAPIServerCIDRs {
		peers = append(peers, netv1.NetworkPolicyPeer{
			IPBlock: &netv1.IPBlock{CIDR: cidr},
		})
	}

	return []netv1.NetworkPolicyEgressRule{{
		Ports: []netv1.NetworkPolicyPort{{
			Protocol: &tcp,
			Port:     &apiPort,
		}},
		To: peers,
	}}
}

func (r *AgentCardNetworkPolicyReconciler) handleDeletion(ctx context.Context, agentCard *agentv1alpha1.AgentCard) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(agentCard, NetworkPolicyFinalizer) {
		networkPolicyLogger.Info("Cleaning up NetworkPolicy for AgentCard", "name", agentCard.Name)

		workloadName := agentCard.Name
		if agentCard.Spec.TargetRef != nil {
			workloadName = agentCard.Spec.TargetRef.Name
		} else if agentCard.Status.TargetRef != nil {
			workloadName = agentCard.Status.TargetRef.Name
		}

		if agentCard.Spec.TargetRef != nil && agentCard.Status.TargetRef != nil &&
			agentCard.Spec.TargetRef.Name != agentCard.Status.TargetRef.Name {
			networkPolicyLogger.Info("WARNING: spec.targetRef.name differs from status.targetRef.name; "+
				"policy for the old workload may be orphaned until owner-reference GC runs",
				"specTargetRef", agentCard.Spec.TargetRef.Name,
				"statusTargetRef", agentCard.Status.TargetRef.Name)
		}
		policyName := fmt.Sprintf("%s-signature-policy", workloadName)

		policy := &netv1.NetworkPolicy{}
		err := r.Get(ctx, types.NamespacedName{Name: policyName, Namespace: agentCard.Namespace}, policy)
		if err != nil && !apierrors.IsNotFound(err) {
			networkPolicyLogger.Error(err, "Failed to get NetworkPolicy for deletion")
			return ctrl.Result{}, err
		}
		if err == nil {
			if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
				networkPolicyLogger.Error(err, "Failed to delete NetworkPolicy")
				return ctrl.Result{}, err
			}
			networkPolicyLogger.Info("Deleted NetworkPolicy", "policy", policyName)
		}

		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &agentv1alpha1.AgentCard{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      agentCard.Name,
				Namespace: agentCard.Namespace,
			}, latest); err != nil {
				return err
			}

			controllerutil.RemoveFinalizer(latest, NetworkPolicyFinalizer)
			return r.Update(ctx, latest)
		}); err != nil {
			networkPolicyLogger.Error(err, "Failed to remove finalizer from AgentCard")
			return ctrl.Result{}, err
		}

		networkPolicyLogger.Info("Removed finalizer from AgentCard")
	}

	return ctrl.Result{}, nil
}

func (r *AgentCardNetworkPolicyReconciler) mapWorkloadToAgentCard(apiVersion, kind string) handler.MapFunc {
	return mapWorkloadToAgentCards(r.Client, apiVersion, kind, networkPolicyLogger)
}

// DiscoverKubeAPIServerCIDRs reads the "kubernetes" Endpoints in the
// default namespace and populates KubeAPIServerCIDRs with /32 entries
// for each API server address. Called once at startup.
func (r *AgentCardNetworkPolicyReconciler) DiscoverKubeAPIServerCIDRs(
	ctx context.Context, reader client.Reader,
) {
	ep := &corev1.Endpoints{}
	key := types.NamespacedName{Name: "kubernetes", Namespace: "default"}
	if err := reader.Get(ctx, key, ep); err != nil {
		networkPolicyLogger.Error(err,
			"Failed to read kubernetes Endpoints; "+
				"restrictive policies will deny all egress")
		return
	}
	var cidrs []string
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			cidrs = append(cidrs, addr.IP+"/32")
		}
	}
	r.KubeAPIServerCIDRs = cidrs
	networkPolicyLogger.Info("Discovered K8s API server endpoints",
		"cidrs", cidrs)
}

func (r *AgentCardNetworkPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := RegisterAgentCardTargetRefIndex(mgr); err != nil {
		return err
	}

	controllerBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&agentv1alpha1.AgentCard{}).
		Owns(&netv1.NetworkPolicy{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentCard("apps/v1", "Deployment")),
			builder.WithPredicates(agentLabelPredicate()),
		).
		Watches(
			&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentCard("apps/v1", "StatefulSet")),
			builder.WithPredicates(agentLabelPredicate()),
		)

	if SandboxCRDExists(mgr.GetConfig()) {
		sandboxObj := &unstructured.Unstructured{}
		sandboxObj.SetGroupVersionKind(sandboxGVK)
		controllerBuilder = controllerBuilder.Watches(
			sandboxObj,
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentCard("agents.x-k8s.io/v1alpha1", "Sandbox")),
			builder.WithPredicates(agentLabelPredicate()),
		)
	}

	return controllerBuilder.
		Named("AgentCardNetworkPolicy").
		Complete(r)
}
