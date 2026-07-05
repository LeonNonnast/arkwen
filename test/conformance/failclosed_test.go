package conformance

import (
	"context"
	"errors"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/policy"
)

// F4: an Enqueue whose mission layer LOOSENS the org floor (weaker isolation
// profile, egress ALLOW over DENY, a gate flipped to fail_open) is REJECTED —
// never silently corrected (Invariants 7/10). The rejected enqueue has no run_id.
func TestF4_EnqueueRejectsLoosening(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	missionRef, _ := rt.CAS.Put(ctx, "mission.md", []byte("m"), "text/markdown")

	loosening := &arkwenv1.RunSpec{
		MissionRef:          missionRef,
		TenantId:            "acme",
		MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT,
		PolicyBundle: &arkwenv1.PolicyBundle{
			Org: &arkwenv1.PolicyLayer{
				Layer: arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_ORG,
				Gates: []*arkwenv1.GateRule{{GateId: "g-deploy", Scope: arkwenv1.GateScope_GATE_SCOPE_RUN, Resolver: arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN, TimeoutPolicy: arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, Mandatory: true, Source: arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_ORG}},
				Isolation: &arkwenv1.IsolationInputs{
					ProfileFloor: arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED,
					Egress:       &arkwenv1.EgressPolicy{DefaultAction: arkwenv1.EgressAction_EGRESS_ACTION_DENY},
				},
			},
			Mission: &arkwenv1.PolicyLayer{
				Layer: arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_MISSION,
				Gates: []*arkwenv1.GateRule{{GateId: "g-deploy", Scope: arkwenv1.GateScope_GATE_SCOPE_RUN, Resolver: arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN, TimeoutPolicy: arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_OPEN, Mandatory: false, Source: arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_MISSION}},
				Isolation: &arkwenv1.IsolationInputs{
					ProfileFloor: arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD,
					Egress:       &arkwenv1.EgressPolicy{DefaultAction: arkwenv1.EgressAction_EGRESS_ACTION_ALLOW},
				},
			},
		},
	}
	runID, _, err := rt.Controller.Enqueue(ctx, loosening, app.EnqueueOpts("echo-stub", "", "acme"))
	if err == nil {
		t.Fatal("enqueue must reject a floor-loosening RunSpec")
	}
	if !errors.Is(err, policy.ErrLoosensFloor) {
		t.Fatalf("expected ErrLoosensFloor, got %v", err)
	}
	if runID != "" {
		t.Fatalf("a rejected enqueue must have no run_id, got %q", runID)
	}
	// and no run was created on the stream
	if len(rt.Controller.Runs(ctx)) != 0 {
		t.Fatal("a rejected enqueue must not create a run")
	}
}

// F5: RunSpec is consumer-agnostic — no consumer name / callback / *_url field.
func TestF5_RunSpecConsumerAgnostic(t *testing.T) {
	if f := fieldsMatching("RunSpec", "url", "callback", "webhook", "consumer"); len(f) != 0 {
		t.Fatalf("RunSpec has a consumer/callout field %v (Invariant 10)", f)
	}
}

// F8: global enum-0 restrictiveness — value 0 is the most-restrictive reading.
func TestF8_EnumZeroRestrictiveness(t *testing.T) {
	cases := []struct {
		name string
		got  int32
		want int32
	}{
		{"TimeoutPolicy0=FAIL_CLOSED", int32(arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED), 0},
		{"EgressAction0=DENY", int32(arkwenv1.EgressAction_EGRESS_ACTION_DENY), 0},
		{"AuthzOutcome0=DENY", int32(arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY), 0},
		{"MaterializationMode0=SNAPSHOT", int32(arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT), 0},
		{"GateDecision0=UNSPECIFIED", int32(arkwenv1.GateDecision_GATE_DECISION_UNSPECIFIED), 0},
		{"IsolationProfile0=UNSPECIFIED", int32(arkwenv1.IsolationProfile_ISOLATION_PROFILE_UNSPECIFIED), 0},
		{"TerminalState0=UNSPECIFIED", int32(arkwenv1.TerminalState_TERMINAL_STATE_UNSPECIFIED), 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: value = %d, want %d", c.name, c.got, c.want)
		}
	}
	// The restrictive concrete values must sit at 0 where one exists.
	if enumValueName("arkwen.v1.TimeoutPolicy", 0) != "TIMEOUT_POLICY_FAIL_CLOSED" {
		t.Fatal("TimeoutPolicy value 0 must be FAIL_CLOSED")
	}
	if enumValueName("arkwen.v1.EgressAction", 0) != "EGRESS_ACTION_DENY" {
		t.Fatal("EgressAction value 0 must be DENY")
	}
	if enumValueName("arkwen.v1.AuthzOutcome", 0) != "AUTHZ_OUTCOME_DENY" {
		t.Fatal("AuthzOutcome value 0 must be DENY")
	}
}
