package isolation

import (
	"fmt"
	"net"
	"strings"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// EgressGuard enforces the composed host-side egress policy OUTSIDE the workload
// trust boundary (ADR-006 E2). Default-deny is the floor for ALL profiles: an
// off-allowlist destination is denied identically on standard/hardened/strict
// (egress-parity, Invariant 9). Raw IPs are forbidden and DNS is controlled — a
// broken-out worker cannot dial an arbitrary address.
//
// On WSL2 the guard is the enforcement point for the in-process worker (every
// WorkcellAPI.Egress call passes through it). On a production host the SAME
// policy is materialized into host nftables/CNI per netns (runc/gVisor) or
// tap/vsock filtering (Firecracker) — identical decisions, different mechanism.
type EgressGuard struct {
	policy *arkwenv1.EgressPolicy
	allow  map[string]bool // "host:port"
	deny   map[string]bool // host (any port)
}

// NewEgressGuard builds a guard from the composed egress policy. A nil policy is
// treated as the fail-closed floor (default-deny, empty allow-list).
func NewEgressGuard(p *arkwenv1.EgressPolicy) *EgressGuard {
	if p == nil {
		p = &arkwenv1.EgressPolicy{DefaultAction: arkwenv1.EgressAction_EGRESS_ACTION_DENY}
	}
	g := &EgressGuard{policy: p, allow: map[string]bool{}, deny: map[string]bool{}}
	for _, r := range p.GetAllow() {
		g.allow[key(r.GetHost(), r.GetPort())] = true
	}
	for _, h := range p.GetDeny() {
		g.deny[strings.ToLower(h)] = true
	}
	return g
}

func key(host string, port uint32) string { return fmt.Sprintf("%s:%d", strings.ToLower(host), port) }

// Check returns the enforced action for host:port plus a redacted, length-bounded
// reason suitable for a security.egress_denied event (never the full raw target).
func (g *EgressGuard) Check(host string, port uint32) (arkwenv1.EgressAction, string) {
	h := strings.ToLower(strings.TrimSpace(host))
	// raw IP is forbidden regardless of allow-list (ADR-006 E2)
	if net.ParseIP(h) != nil {
		return arkwenv1.EgressAction_EGRESS_ACTION_DENY, "raw-ip forbidden"
	}
	if h == "" {
		return arkwenv1.EgressAction_EGRESS_ACTION_DENY, "empty host"
	}
	// explicit denies win
	if g.deny[h] {
		return arkwenv1.EgressAction_EGRESS_ACTION_DENY, "explicit deny"
	}
	if g.allow[key(h, port)] {
		return arkwenv1.EgressAction_EGRESS_ACTION_ALLOW, "allow-listed"
	}
	// default is DENY (the floor); ALLOW default is only possible if policy set it
	if g.policy.GetDefaultAction() == arkwenv1.EgressAction_EGRESS_ACTION_ALLOW {
		return arkwenv1.EgressAction_EGRESS_ACTION_ALLOW, "default-allow"
	}
	return arkwenv1.EgressAction_EGRESS_ACTION_DENY, "off-allowlist (default-deny)"
}

// RedactHost returns a length-bounded, partially-masked host for egress_denied
// events (host/SNI redacted per ADR-006 E2).
func RedactHost(host string) string {
	h := strings.TrimSpace(host)
	if len(h) <= 4 {
		return "***"
	}
	if len(h) > 40 {
		h = h[:40]
	}
	return h[:2] + "***" + h[len(h)-2:]
}
