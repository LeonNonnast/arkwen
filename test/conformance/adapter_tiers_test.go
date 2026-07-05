package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/adapter"
	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/cas"
	"github.com/arkwen/arkwen/internal/ids"
	"github.com/arkwen/arkwen/internal/isolation"
	"github.com/arkwen/arkwen/internal/policy"
	"github.com/arkwen/arkwen/internal/secret"
	"github.com/arkwen/arkwen/internal/shim"
	"google.golang.org/protobuf/proto"
)

// driveRun runs one Mission to terminal with the given worker kind and returns
// the full event stream + the run's provisioning isolation contract.
func driveRun(t *testing.T, workerKind string) ([]*arkwenv1.EventEnvelope, *arkwenv1.IsolationContract) {
	t.Helper()
	rt := app.New()
	ctx := context.Background()
	missionRef, err := rt.CAS.Put(ctx, "mission.md", []byte("do the thing"), "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	spec := &arkwenv1.RunSpec{MissionRef: missionRef, TenantId: "acme", MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT}
	runID, _, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts(workerKind, "", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Controller.Drive(ctx, runID); err != nil {
		t.Fatal(err)
	}
	evs, err := rt.Controller.Events(ctx, runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var iso *arkwenv1.IsolationContract
	for _, e := range evs {
		if p, ok := e.GetPayload().(*arkwenv1.EventEnvelope_RunProvisioning); ok {
			iso = p.RunProvisioning.GetIsolation()
		}
	}
	return evs, iso
}

func hasType(evs []*arkwenv1.EventEnvelope, t arkwenv1.EventType) bool {
	for _, e := range evs {
		if e.GetType() == t {
			return true
		}
	}
	return false
}

func terminalOf(evs []*arkwenv1.EventEnvelope) *arkwenv1.Termination {
	for _, e := range evs {
		if p, ok := e.GetPayload().(*arkwenv1.EventEnvelope_RunFinished); ok {
			return p.RunFinished.GetTermination()
		}
	}
	return nil
}

func manifestCount(evs []*arkwenv1.EventEnvelope) int {
	n := 0
	for _, e := range evs {
		if e.GetType() == arkwenv1.EventType_EVENT_TYPE_WORKER_ARTIFACT_WRITTEN {
			n++
		}
	}
	return n
}

// AT1 + AT4: both tiers pass the identical MUST suite (same terminal, non-empty
// manifest); observational lacks the semantic class, cooperative has it.
func TestAT1_AT4_TierParity(t *testing.T) {
	obs, _ := driveRun(t, "echo-stub")
	coop, _ := driveRun(t, "claude-code")

	if to, tc := terminalOf(obs), terminalOf(coop); to.GetState() != arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED || tc.GetState() != arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED {
		t.Fatalf("both tiers must reach COMPLETED: obs=%v coop=%v", to.GetState(), tc.GetState())
	}
	if manifestCount(obs) == 0 || manifestCount(coop) == 0 {
		t.Fatalf("both tiers must produce a non-empty manifest: obs=%d coop=%d", manifestCount(obs), manifestCount(coop))
	}
	// observational: semantic class ABSENT, worker.raw PRESENT
	for _, ty := range []arkwenv1.EventType{
		arkwenv1.EventType_EVENT_TYPE_WORKER_MESSAGE,
		arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_CALL,
		arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_RESULT,
	} {
		if hasType(obs, ty) {
			t.Fatalf("observational tier must NOT emit semantic %v", ty)
		}
	}
	if !hasType(obs, arkwenv1.EventType_EVENT_TYPE_WORKER_RAW) {
		t.Fatal("observational tier must still fold worker.raw")
	}
	// cooperative: semantic class PRESENT
	for _, ty := range []arkwenv1.EventType{
		arkwenv1.EventType_EVENT_TYPE_WORKER_MESSAGE,
		arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_CALL,
		arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_RESULT,
	} {
		if !hasType(coop, ty) {
			t.Fatalf("cooperative tier must emit semantic %v", ty)
		}
	}
}

// AT5: the security floor is byte-identical across tiers (a missing capability
// degrades observability only, never the floor).
func TestAT5_FloorIdenticalAcrossTiers(t *testing.T) {
	_, isoObs := driveRun(t, "echo-stub")
	_, isoCoop := driveRun(t, "claude-code")
	if isoObs == nil || isoCoop == nil {
		t.Fatal("both runs must carry a provisioning isolation contract")
	}
	if !proto.Equal(isoObs, isoCoop) {
		t.Fatalf("isolation floor differs across tiers:\n obs=%v\ncoop=%v", isoObs, isoCoop)
	}
	if isoObs.GetEgress().GetDefaultAction() != arkwenv1.EgressAction_EGRESS_ACTION_DENY {
		t.Fatal("egress floor must be default-deny")
	}
}

// standaloneShim builds a shim with a no-op emit for direct control-signal tests.
func standaloneShim(t *testing.T) (*shim.Shim, *arkwenv1.RunSeed, *arkwenv1.IsolationContract) {
	t.Helper()
	comp, err := policy.Compose(nil)
	if err != nil {
		t.Fatal(err)
	}
	sh := shim.New(cas.NewMem(), secret.NewMem(), isolation.NewRegistry(), adapter.NewRegistry(),
		func(ctx context.Context, ev *arkwenv1.EventEnvelope) error { return nil })
	seed := &arkwenv1.RunSeed{TenantId: "acme", MissionHash: ids.Sha256([]byte("m"))}
	return sh, seed, comp.Isolation
}

// AT2 + AT3: cancel is the unconditional MUST signal on both tiers; pause on an
// observational adapter degrades to boundary freeze (success) with the paused
// OVERLAY holding and the top-state staying non-terminal.
func TestAT2_AT3_ControlSignals(t *testing.T) {
	ctx := context.Background()

	// observational pause -> degradedToBoundary true, overlay PAUSED_BY_USER, RUNNING
	sh, seed, iso := standaloneShim(t)
	if _, err := sh.Create(ctx, "run-obs", "blocking-observational", seed, iso, shim.DefaultScopes()); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.Start(ctx, "run-obs"); err != nil {
		t.Fatal(err)
	}
	resp, err := sh.Signal(ctx, "run-obs", arkwenv1.ShimSignal_SHIM_SIGNAL_PAUSE)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetDegradedToBoundary() {
		t.Fatal("observational pause must degrade to boundary freeze")
	}
	if resp.GetStatus().GetSuspensionReason() != arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_USER {
		t.Fatalf("paused overlay must hold, got %v", resp.GetStatus().GetSuspensionReason())
	}
	if resp.GetStatus().GetState() == arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED {
		t.Fatal("pause must keep a non-terminal top-state")
	}
	// cancel is the MUST signal — works regardless of capabilities
	if _, err := sh.Signal(ctx, "run-obs", arkwenv1.ShimSignal_SHIM_SIGNAL_CANCEL); err != nil {
		t.Fatal(err)
	}
	term, err := sh.Reap(ctx, "run-obs")
	if err != nil {
		t.Fatal(err)
	}
	if term.GetState() != arkwenv1.TerminalState_TERMINAL_STATE_CANCELED {
		t.Fatalf("cancel must yield CANCELED, got %v", term.GetState())
	}

	// cooperative pause -> NOT degraded (in-worker pause capability present)
	sh2, seed2, iso2 := standaloneShim(t)
	if _, err := sh2.Create(ctx, "run-coop", "blocking-cooperative", seed2, iso2, shim.DefaultScopes()); err != nil {
		t.Fatal(err)
	}
	if _, err := sh2.Start(ctx, "run-coop"); err != nil {
		t.Fatal(err)
	}
	resp2, err := sh2.Signal(ctx, "run-coop", arkwenv1.ShimSignal_SHIM_SIGNAL_PAUSE)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.GetDegradedToBoundary() {
		t.Fatal("cooperative pause must NOT degrade to boundary")
	}
	_, _ = sh2.Signal(ctx, "run-coop", arkwenv1.ShimSignal_SHIM_SIGNAL_CANCEL)
	_, _ = sh2.Reap(ctx, "run-coop")
}

// AT6: worker_kind never drives Controller control-flow (grep-gate, Invariant 1).
func TestAT6_ControllerRuntimeAgnostic(t *testing.T) {
	dir := "../../internal/controller"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{`"echo-stub"`, `"claude-code"`, `"openhands"`, `"claude`, `"openhands"`}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range banned {
			if strings.Contains(string(b), bad) {
				t.Fatalf("controller %s special-cases a worker kind (%s) — Invariant 1 violated", e.Name(), bad)
			}
		}
	}
}
