package isolation

import (
	"errors"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// Invariant 7: STANDARD is always satisfiable (local); UNSPECIFIED and stronger
// tiers not available on this host are rejected fail-closed — NEVER downgraded.
func TestSelect_FailClosedNoDowngrade(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Select(arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD); err != nil {
		t.Fatalf("standard must be available: %v", err)
	}
	if _, err := r.Select(arkwenv1.IsolationProfile_ISOLATION_PROFILE_UNSPECIFIED); !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("UNSPECIFIED must be unsatisfiable, got %v", err)
	}
	// hardened/strict are unsatisfiable here (no runsc/firecracker) — and the
	// error is unsatisfiable, NOT a silent downgrade to a weaker runtime.
	for _, p := range []arkwenv1.IsolationProfile{arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED, arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT} {
		rt, err := r.Select(p)
		if err == nil {
			// if the host happens to have runsc/kvm, the returned runtime must be
			// EXACTLY the requested profile (still no downgrade).
			if rt.Profile() != p {
				t.Fatalf("no-downgrade violated: requested %v got %v", p, rt.Profile())
			}
			continue
		}
		if !errors.Is(err, ErrUnsatisfiable) {
			t.Fatalf("want ErrUnsatisfiable for %v, got %v", p, err)
		}
	}
}

// Egress floor: allow-listed passes; off-allowlist and raw-IP are denied.
func TestEgressGuard(t *testing.T) {
	g := NewEgressGuard(&arkwenv1.EgressPolicy{
		DefaultAction: arkwenv1.EgressAction_EGRESS_ACTION_DENY,
		Allow:         []*arkwenv1.EgressRule{{Host: "api.anthropic.com", Port: 443}},
		Deny:          []string{"blocked.example"},
	})
	if a, _ := g.Check("api.anthropic.com", 443); a != arkwenv1.EgressAction_EGRESS_ACTION_ALLOW {
		t.Fatal("allow-listed host must be ALLOW")
	}
	if a, _ := g.Check("evil.example", 443); a != arkwenv1.EgressAction_EGRESS_ACTION_DENY {
		t.Fatal("off-allowlist must be DENY")
	}
	if a, _ := g.Check("93.184.216.34", 443); a != arkwenv1.EgressAction_EGRESS_ACTION_DENY {
		t.Fatal("raw IP must be DENY (forbidden)")
	}
	if a, _ := g.Check("blocked.example", 443); a != arkwenv1.EgressAction_EGRESS_ACTION_DENY {
		t.Fatal("explicit deny must win")
	}
}
