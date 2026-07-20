package injector

import (
	"github.com/rossoctl/operator/internal/webhook/config"
)

const (
	labelValueTrue  = "true"
	labelValueFalse = "false"
)

// PrecedenceEvaluator determines which sidecars should be injected for a workload
// by evaluating a per-sidecar precedence chain. Each layer can short-circuit with "no".
//
// Precedence order (highest to lowest):
//  1. Per-sidecar feature gate (featureGates.<sidecar>=false disables cluster-wide)
//  2. Workload label (rossoctl.io/<sidecar>-inject=false) — per-sidecar opt-out
//
// Global and workload-level pre-filtering (globalEnabled kill switch, type check,
// injectTools gate, rossoctl.io/inject=disabled opt-out) is handled upstream in
// PodMutator before this evaluator is reached.
type PrecedenceEvaluator struct {
	featureGates *config.FeatureGates
}

// NewPrecedenceEvaluator creates a new evaluator with the given feature gates.
func NewPrecedenceEvaluator(fg *config.FeatureGates) *PrecedenceEvaluator {
	if fg == nil {
		fg = config.DefaultFeatureGates()
	}
	return &PrecedenceEvaluator{
		featureGates: fg,
	}
}

// Evaluate determines which sidecars should be injected for a given workload.
//
// Parameters:
//   - workloadLabels: labels from the pod template or workload metadata
func (e *PrecedenceEvaluator) Evaluate(
	workloadLabels map[string]string,
) InjectionDecision {
	decision := InjectionDecision{
		EnvoyProxy: e.evaluateSidecar(
			"envoy-proxy",
			e.featureGates.EnvoyProxy,
			workloadLabels[LabelEnvoyProxyInject],
		),
		SpiffeHelper: evaluateSpiffeHelper(workloadLabels[LabelSpiffeHelperInject]),
	}

	// proxy-init always follows envoy-proxy
	decision.ProxyInit = SidecarDecision{
		Inject: decision.EnvoyProxy.Inject,
		Reason: "follows envoy-proxy decision",
		Layer:  decision.EnvoyProxy.Layer,
	}

	return decision
}

// evaluateSpiffeHelper resolves the per-workload SPIRE-enabled flag from
// the rossoctl.io/spiffe-helper-inject label. Spiffe-helper is bundled
// inside the combined authbridge images and gated by the SPIRE_ENABLED
// env var rather than by feature-gate, so this skips the feature-gate
// layer that evaluateSidecar applies.
//
// TODO: rename SpiffeHelper -> SpireEnabled (decision field + label)
// once the in-pod helper truly retires; left as-is here to keep this
// PR's blast radius contained.
func evaluateSpiffeHelper(workloadLabelValue string) SidecarDecision {
	if workloadLabelValue == labelValueFalse {
		return SidecarDecision{
			Inject: false,
			Reason: "workload label disabled spiffe-helper",
			Layer:  "workload-label",
		}
	}
	return SidecarDecision{
		Inject: true,
		Reason: "all gates passed",
		Layer:  "default",
	}
}

// evaluateSidecar evaluates the two-layer precedence chain for a single sidecar.
func (e *PrecedenceEvaluator) evaluateSidecar(
	sidecarName string,
	featureGateEnabled bool,
	workloadLabelValue string, // "" or "false"
) SidecarDecision {
	// Layer 1: Per-sidecar feature gate
	if !featureGateEnabled {
		return SidecarDecision{
			Inject: false,
			Reason: sidecarName + " feature gate disabled",
			Layer:  "feature-gate",
		}
	}

	// Layer 2: Per-sidecar workload opt-out label
	if workloadLabelValue == labelValueFalse {
		return SidecarDecision{
			Inject: false,
			Reason: "workload label disabled " + sidecarName,
			Layer:  "workload-label",
		}
	}

	return SidecarDecision{
		Inject: true,
		Reason: "all gates passed",
		Layer:  "default",
	}
}
