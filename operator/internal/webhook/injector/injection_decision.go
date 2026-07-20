package injector

// SidecarDecision represents the injection decision for a single sidecar.
type SidecarDecision struct {
	Inject bool
	Reason string // human-readable reason for the decision
	Layer  string // which precedence layer made the decision
}

// InjectionDecision holds the per-sidecar injection decisions for a workload.
//
// SpiffeHelper here is a per-workload SPIRE-enabled flag, not a separate
// container — spiffe-helper is bundled inside the combined authbridge
// images and starts conditionally on SPIRE_ENABLED. The flag still
// controls SPIRE volume mounts, ServiceAccount provisioning, and the
// SPIRE_ENABLED env var on the combined container.
//
// Client registration is operator-managed (no in-pod sidecar) and is
// no longer represented in the decision struct.
//
// TODO: rename SpiffeHelper to SpireEnabled (and the
// rossoctl.io/spiffe-helper-inject label to rossoctl.io/spire-enabled) so
// the names match what the field actually controls now that the
// standalone helper sidecar is gone. Left for a follow-up PR to keep
// this one focused.
type InjectionDecision struct {
	EnvoyProxy   SidecarDecision
	ProxyInit    SidecarDecision // follows EnvoyProxy
	SpiffeHelper SidecarDecision
}

// AnyInjected returns true if at least one sidecar will be injected.
func (d *InjectionDecision) AnyInjected() bool {
	return d.EnvoyProxy.Inject || d.SpiffeHelper.Inject
}
