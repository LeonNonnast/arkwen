// Package isolation is the workload boundary (ADR-006). Isolation is a linearly
// ordered ladder standard(runc) < hardened(gVisor) < strict(Firecracker), chosen
// by policy and FAIL-CLOSED with NO auto-downgrade (Invariant 7): an unsatisfiable
// profile fails provisioning with reason=isolation_unsatisfiable, it is never
// silently run at a weaker profile.
//
// The security FLOOR never degrades (Invariant 9): default-deny egress + the
// hardening posture are identical across every backend. Only observability
// degrades when a capability is absent — never the floor.
//
// S0 ships the `standard`/local runtime (WSL2-friendly, in-process worker behind
// the WorkcellAPI boundary). runc(containerd), gVisor and Firecracker handlers
// are additive backends behind this same interface (S2a) — a plug-in, not a
// rewrite, because the wire contract is already frozen.
package isolation

import (
	"errors"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// ErrUnsatisfiable is the fail-closed result of requesting a profile the host
// cannot provide. It maps to TERMINAL_REASON_ISOLATION_UNSATISFIABLE — never an
// auto-downgrade (ADR-006 E1).
var ErrUnsatisfiable = errors.New("isolation: profile unsatisfiable on this host (fail-closed, no downgrade)")

// Runtime provisions and describes a workcell for one isolation profile.
type Runtime interface {
	Profile() arkwenv1.IsolationProfile
	// Available reports nil if this runtime can run on the current host, else a
	// fail-closed reason. NEVER downgrades.
	Available() error
	// Describe returns the human/diagnostic hardening posture (for logs/events).
	Describe() string
}

// Registry holds the available runtime handlers keyed by profile.
type Registry struct {
	handlers map[arkwenv1.IsolationProfile]Runtime
}

// NewRegistry builds the default registry. `standard` (local) is always present;
// stronger tiers are registered but self-report availability (fail-closed).
func NewRegistry() *Registry {
	r := &Registry{handlers: map[arkwenv1.IsolationProfile]Runtime{}}
	r.Register(newLocal())       // STANDARD (runc-class hardening; local backing on WSL2)
	r.Register(newGVisor())      // HARDENED (gVisor/runsc)
	r.Register(newFirecracker()) // STRICT (Firecracker microVM + jailer)
	return r
}

// Register adds/overrides a handler for its profile.
func (r *Registry) Register(rt Runtime) { r.handlers[rt.Profile()] = rt }

// Select returns the runtime for exactly the requested profile. It NEVER returns
// a weaker profile than requested: an unsatisfiable request is an error
// (Invariant 7, no auto-downgrade). UNSPECIFIED is rejected fail-closed.
func (r *Registry) Select(profile arkwenv1.IsolationProfile) (Runtime, error) {
	if profile == arkwenv1.IsolationProfile_ISOLATION_PROFILE_UNSPECIFIED {
		return nil, fmt.Errorf("%w: profile UNSPECIFIED", ErrUnsatisfiable)
	}
	rt, ok := r.handlers[profile]
	if !ok {
		return nil, fmt.Errorf("%w: no handler for %v", ErrUnsatisfiable, profile)
	}
	if err := rt.Available(); err != nil {
		return nil, fmt.Errorf("%w: %v: %v", ErrUnsatisfiable, profile, err)
	}
	return rt, nil
}
