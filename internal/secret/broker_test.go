package secret

import (
	"context"
	"testing"
)

// Invariants 1/5: the broker leases scoped secrets (material out-of-band), reports
// only a scope digest on the Lease record (never the value), and revokes at reap.
func TestLeaseRevokeRotate(t *testing.T) {
	ctx := context.Background()
	b := NewMem(WithResolver(func(tenant, name string) (string, bool) {
		if name == "MODEL_API_KEY" {
			return "sk-live-XYZ", true
		}
		return "", false
	}))
	grant, err := b.Lease(ctx, "run-1", "acme", []Scope{{Name: "MODEL_API_KEY", Purpose: "model"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(grant.Leases) != 1 {
		t.Fatalf("want 1 lease, got %d", len(grant.Leases))
	}
	l := grant.Leases[0]
	if grant.Material["MODEL_API_KEY"] != "sk-live-XYZ" {
		t.Fatal("material must carry the secret value out-of-band")
	}
	// the Lease record itself carries only a scope DIGEST, never the value.
	if l.ScopeRef == nil || l.ScopeRef.GetHex() == "" {
		t.Fatal("lease must carry a scope digest")
	}
	if l.ScopeRef.GetHex() == "sk-live-XYZ" || l.RuleID == "sk-live-XYZ" {
		t.Fatal("lease record must not contain the secret value")
	}
	// rotate yields fresh material
	_, fresh, err := b.Rotate(ctx, "run-1", l.ID)
	if err != nil || fresh == "" || fresh == "sk-live-XYZ" {
		t.Fatalf("rotate should yield fresh material: %q %v", fresh, err)
	}
	// revoke returns the lease ids and clears them
	revoked, err := b.Revoke(ctx, "run-1")
	if err != nil || len(revoked) != 1 || revoked[0] != l.ID {
		t.Fatalf("revoke should return the lease id: %v %v", revoked, err)
	}
	if again, _ := b.Revoke(ctx, "run-1"); len(again) != 0 {
		t.Fatal("second revoke should find no leases")
	}
}

// A run that requests a scope with no resolvable secret simply gets no lease.
func TestLeaseUnknownScope(t *testing.T) {
	ctx := context.Background()
	b := NewMem(WithResolver(func(tenant, name string) (string, bool) { return "", false }))
	grant, err := b.Lease(ctx, "run-1", "acme", []Scope{{Name: "NOPE"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(grant.Leases) != 0 {
		t.Fatal("unresolvable scope must yield no lease")
	}
}
