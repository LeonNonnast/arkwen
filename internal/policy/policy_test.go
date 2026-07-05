package policy

import (
	"errors"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

func layer(kind arkwenv1.PolicyLayerKind, profile arkwenv1.IsolationProfile, egress arkwenv1.EgressAction) *arkwenv1.PolicyLayer {
	return &arkwenv1.PolicyLayer{
		Layer: kind,
		Isolation: &arkwenv1.IsolationInputs{
			ProfileFloor: profile,
			Egress:       &arkwenv1.EgressPolicy{DefaultAction: egress},
		},
	}
}

// Invariant 7: a lower layer weakening the profile is REJECTED (not corrected).
func TestCompose_RejectsProfileLoosening(t *testing.T) {
	b := &arkwenv1.PolicyBundle{
		Org:     layer(arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_ORG, arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED, arkwenv1.EgressAction_EGRESS_ACTION_DENY),
		Mission: layer(arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_MISSION, arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD, arkwenv1.EgressAction_EGRESS_ACTION_DENY),
	}
	if _, err := Compose(b); !errors.Is(err, ErrLoosensFloor) {
		t.Fatalf("want ErrLoosensFloor, got %v", err)
	}
}

// Invariant 7: a lower layer flipping egress DENY->ALLOW is REJECTED.
func TestCompose_RejectsEgressLoosening(t *testing.T) {
	b := &arkwenv1.PolicyBundle{
		Org:     layer(arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_ORG, arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD, arkwenv1.EgressAction_EGRESS_ACTION_DENY),
		Mission: layer(arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_MISSION, arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD, arkwenv1.EgressAction_EGRESS_ACTION_ALLOW),
	}
	if _, err := Compose(b); !errors.Is(err, ErrLoosensFloor) {
		t.Fatalf("want ErrLoosensFloor, got %v", err)
	}
}

// Invariant 7: a gate flipped fail_closed->fail_open, or mandatory dropped, is REJECTED.
func TestCompose_RejectsGateLoosening(t *testing.T) {
	org := &arkwenv1.PolicyLayer{Layer: arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_ORG, Gates: []*arkwenv1.GateRule{{
		GateId: "g", Scope: arkwenv1.GateScope_GATE_SCOPE_RUN, Resolver: arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN,
		TimeoutPolicy: arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, Mandatory: true,
	}}}
	mission := &arkwenv1.PolicyLayer{Layer: arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_MISSION, Gates: []*arkwenv1.GateRule{{
		GateId: "g", Scope: arkwenv1.GateScope_GATE_SCOPE_RUN, Resolver: arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN,
		TimeoutPolicy: arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_OPEN, Mandatory: false,
	}}}
	if _, err := Compose(&arkwenv1.PolicyBundle{Org: org, Mission: mission}); !errors.Is(err, ErrLoosensFloor) {
		t.Fatalf("want ErrLoosensFloor, got %v", err)
	}
}

// A valid stricter-only bundle composes; digests are deterministic; the model-API
// endpoint is in the intrinsic minimal allowlist; default is DENY.
func TestCompose_ValidDeterministicFloor(t *testing.T) {
	b := &arkwenv1.PolicyBundle{
		Org: layer(arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_ORG, arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED, arkwenv1.EgressAction_EGRESS_ACTION_DENY),
	}
	c1, err := Compose(b)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := Compose(b)
	if c1.IsolationContractRef.GetHex() != c2.IsolationContractRef.GetHex() {
		t.Fatal("isolation_contract_ref not deterministic")
	}
	if c1.EgressPolicyHash.GetHex() != c2.EgressPolicyHash.GetHex() {
		t.Fatal("egress_policy_hash not deterministic")
	}
	if c1.Isolation.GetProfile() != arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED {
		t.Fatal("profile should compose to HARDENED (max)")
	}
	if c1.Isolation.GetEgress().GetDefaultAction() != arkwenv1.EgressAction_EGRESS_ACTION_DENY {
		t.Fatal("egress default must be DENY (floor)")
	}
	found := false
	for _, r := range c1.Isolation.GetEgress().GetAllow() {
		if r.GetHost() == "api.anthropic.com" && r.GetPort() == 443 {
			found = true
		}
	}
	if !found {
		t.Fatal("intrinsic minimal allowlist must include the model-API endpoint")
	}
}
