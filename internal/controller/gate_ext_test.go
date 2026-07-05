package controller_test

import (
	"context"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"google.golang.org/protobuf/types/known/durationpb"
)

func enqueue(t *testing.T, rt *app.Runtime, tenant string) string {
	t.Helper()
	ctx := context.Background()
	mref, err := rt.CAS.Put(ctx, "mission.md", []byte("m"), "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	spec := &arkwenv1.RunSpec{MissionRef: mref, TenantId: tenant, MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT}
	runID, _, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts("echo-stub", "", tenant))
	if err != nil {
		t.Fatal(err)
	}
	return runID
}

func mandatoryHumanGate() *arkwenv1.GateRule {
	return &arkwenv1.GateRule{
		GateId:        "g-deploy",
		Scope:         arkwenv1.GateScope_GATE_SCOPE_RUN,
		Resolver:      arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN,
		TimeoutPolicy: arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED,
		MaxWait:       durationpb.New(3600e9),
		Mandatory:     true,
		Trigger:       "pre-deploy",
	}
}

func statusOf(t *testing.T, rt *app.Runtime, runID string) *arkwenv1.LifecycleStatus {
	t.Helper()
	st, err := rt.Controller.Status(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// F1: an unresolved mandatory gate under fail_closed times out → run FAILED with
// reason GATE_REJECTED (never a silent approve).
func TestGateF1_TimeoutFailClosed(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	runID := enqueue(t, rt, "acme")
	if err := rt.Controller.RequestGate(ctx, runID, mandatoryHumanGate(), nil); err != nil {
		t.Fatal(err)
	}
	// paused overlay holds
	if statusOf(t, rt, runID).GetSuspensionReason() == arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE {
		t.Fatal("gate.requested must pause the run")
	}
	if err := rt.Controller.GateTimeout(ctx, runID, "g-deploy"); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, rt, runID)
	if st.GetState() != arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED ||
		st.GetTermination().GetReason() != arkwenv1.TerminalReason_TERMINAL_REASON_GATE_REJECTED {
		t.Fatalf("timeout under fail_closed must FAIL with GATE_REJECTED, got %v/%v", st.GetState(), st.GetTermination().GetReason())
	}
}

// F2: a gate.resolved carrying GATE_DECISION_UNSPECIFIED is read as REJECT.
func TestGateF2_UnspecifiedIsReject(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	runID := enqueue(t, rt, "acme")
	if err := rt.Controller.RequestGate(ctx, runID, mandatoryHumanGate(), nil); err != nil {
		t.Fatal(err)
	}
	by := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "op", TenantId: "acme"}
	if _, err := rt.Controller.ResolveGate(ctx, runID, "g-deploy", arkwenv1.GateDecision_GATE_DECISION_UNSPECIFIED, "", nil, by); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, rt, runID)
	if st.GetState() != arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED ||
		st.GetTermination().GetReason() != arkwenv1.TerminalReason_TERMINAL_REASON_GATE_REJECTED {
		t.Fatalf("UNSPECIFIED decision on a mandatory gate must FAIL with GATE_REJECTED, got %v/%v", st.GetState(), st.GetTermination().GetReason())
	}
}

// A human gate resolved APPROVE resumes the run (paused is an overlay, resolvable
// back to running — Invariant 8).
func TestGateApproveResumes(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	runID := enqueue(t, rt, "acme")
	if err := rt.Controller.RequestGate(ctx, runID, mandatoryHumanGate(), nil); err != nil {
		t.Fatal(err)
	}
	by := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "op", TenantId: "acme"}
	if _, err := rt.Controller.ResolveGate(ctx, runID, "g-deploy", arkwenv1.GateDecision_GATE_DECISION_APPROVE, "ok", nil, by); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, rt, runID)
	if st.GetSuspensionReason() != arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE {
		t.Fatalf("approve must resume (clear the overlay), got %v", st.GetSuspensionReason())
	}
	if st.GetState() == arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED {
		t.Fatal("approve must not fail the run")
	}
}
