package conformance

import (
	"testing"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/isolation"
	"google.golang.org/protobuf/types/known/durationpb"
)

// S2a: the isolation ladder is fail-closed with NO auto-downgrade. STANDARD is
// available (local); HARDENED/STRICT are unsatisfiable on this host and error —
// they are NEVER silently run at a weaker profile (Invariant 7/9).
func TestS2a_IsolationLadderFailClosed(t *testing.T) {
	reg := isolation.NewRegistry()
	if _, err := reg.Select(arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD); err != nil {
		t.Fatalf("standard must be available: %v", err)
	}
	if _, err := reg.Select(arkwenv1.IsolationProfile_ISOLATION_PROFILE_UNSPECIFIED); err == nil {
		t.Fatal("UNSPECIFIED profile must be rejected (fail-closed)")
	}
	// HARDENED (gVisor) + STRICT (Firecracker) are unsatisfiable on WSL2 → error,
	// never a downgrade to standard.
	for _, p := range []arkwenv1.IsolationProfile{
		arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED,
		arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT,
	} {
		if rt, err := reg.Select(p); err == nil {
			t.Fatalf("%v unexpectedly satisfiable as %v (no host support expected)", p, rt.Profile())
		}
	}
}

// S2a: default-deny egress is IDENTICAL across every backend — the floor never
// diverges by tier (egress-parity, Invariant 9). The guard is profile-independent.
func TestS2a_EgressParity(t *testing.T) {
	policy := &arkwenv1.EgressPolicy{
		DefaultAction: arkwenv1.EgressAction_EGRESS_ACTION_DENY,
		Allow:         []*arkwenv1.EgressRule{{Host: "api.anthropic.com", Port: 443}},
	}
	guard := isolation.NewEgressGuard(policy)
	cases := []struct {
		host string
		port uint32
		want arkwenv1.EgressAction
	}{
		{"api.anthropic.com", 443, arkwenv1.EgressAction_EGRESS_ACTION_ALLOW},
		{"exfil.evil.example", 443, arkwenv1.EgressAction_EGRESS_ACTION_DENY},
		{"10.0.0.1", 443, arkwenv1.EgressAction_EGRESS_ACTION_DENY}, // raw-ip forbidden
		{"api.anthropic.com", 80, arkwenv1.EgressAction_EGRESS_ACTION_DENY},
	}
	// Same guard, same decisions regardless of which profile backs the workcell.
	for _, prof := range []string{"standard", "hardened", "strict"} {
		for _, c := range cases {
			got, _ := guard.Check(c.host, c.port)
			if got != c.want {
				t.Fatalf("[%s] egress %s:%d = %v, want %v (floor must be identical across tiers)", prof, c.host, c.port, got, c.want)
			}
		}
	}
}

// S2a: a hard resource-ceiling breach maps to TERMINAL_REASON_RESOURCE_EXHAUSTED
// — a reason on FAILED, never a new state (ADR-006 E3 / Invariant 8).
func TestS2a_ResourceExhausted(t *testing.T) {
	limits := &arkwenv1.ResourceLimits{MemBytes: 1 << 30, WallClock: durationpb.New(time.Hour)}
	// within ceiling
	if b, _ := isolation.CheckCeiling(limits, isolation.ResourceSample{MemBytes: 1 << 20}); b {
		t.Fatal("usage within ceiling must not breach")
	}
	// over the memory ceiling
	b, dim := isolation.CheckCeiling(limits, isolation.ResourceSample{MemBytes: 2 << 30})
	if !b || dim != "mem_bytes" {
		t.Fatalf("mem breach not detected: b=%v dim=%q", b, dim)
	}
	term := isolation.ResourceExhaustedTermination(dim)
	if term.GetState() != arkwenv1.TerminalState_TERMINAL_STATE_FAILED ||
		term.GetReason() != arkwenv1.TerminalReason_TERMINAL_REASON_RESOURCE_EXHAUSTED {
		t.Fatalf("breach must terminate FAILED/RESOURCE_EXHAUSTED, got %v/%v", term.GetState(), term.GetReason())
	}
}
