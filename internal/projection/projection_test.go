package projection

import (
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func envelope(seq uint64, t arkwenv1.EventType) *arkwenv1.EventEnvelope {
	return &arkwenv1.EventEnvelope{RunId: "r", Seq: seq, SchemaVersion: 1, Type: t, Timestamp: timestamppb.Now()}
}

// Invariant 8: paused is an OVERLAY, never a top-state; resume clears it.
func TestStatus_PausedIsOverlay(t *testing.T) {
	e0 := envelope(0, arkwenv1.EventType_EVENT_TYPE_RUN_CREATED)
	e0.Payload = &arkwenv1.EventEnvelope_RunCreated{RunCreated: &arkwenv1.RunCreated{Seed: &arkwenv1.RunSeed{}}}
	e1 := envelope(1, arkwenv1.EventType_EVENT_TYPE_RUN_STARTED)
	e1.Payload = &arkwenv1.EventEnvelope_RunStarted{RunStarted: &arkwenv1.RunStarted{}}
	e2 := envelope(2, arkwenv1.EventType_EVENT_TYPE_RUN_PAUSED)
	e2.Payload = &arkwenv1.EventEnvelope_RunPaused{RunPaused: &arkwenv1.RunPaused{Reason: arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_GATE}}
	evs := []*arkwenv1.EventEnvelope{e0, e1, e2}

	st := Status(evs)
	if st.GetState() != arkwenv1.LifecycleState_LIFECYCLE_STATE_RUNNING {
		t.Fatalf("paused must not change the top-state; got %v", st.GetState())
	}
	if st.GetSuspensionReason() != arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_GATE {
		t.Fatal("suspension overlay not set")
	}
	e3 := envelope(3, arkwenv1.EventType_EVENT_TYPE_RUN_RESUMED)
	e3.Payload = &arkwenv1.EventEnvelope_RunResumed{RunResumed: &arkwenv1.RunResumed{}}
	if Status(append(evs, e3)).GetSuspensionReason() != arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE {
		t.Fatal("resume must clear the overlay")
	}
}

// Terminal legality incl. fail-closed UNSPECIFIED->FAILED.
func TestStatus_TerminalLegality(t *testing.T) {
	cases := []struct {
		term arkwenv1.TerminalState
		want arkwenv1.LifecycleState
	}{
		{arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED, arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED},
		{arkwenv1.TerminalState_TERMINAL_STATE_FAILED, arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED},
		{arkwenv1.TerminalState_TERMINAL_STATE_CANCELED, arkwenv1.LifecycleState_LIFECYCLE_STATE_CANCELED},
		{arkwenv1.TerminalState_TERMINAL_STATE_UNSPECIFIED, arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED},
	}
	for _, c := range cases {
		e0 := envelope(0, arkwenv1.EventType_EVENT_TYPE_RUN_CREATED)
		e0.Payload = &arkwenv1.EventEnvelope_RunCreated{RunCreated: &arkwenv1.RunCreated{Seed: &arkwenv1.RunSeed{}}}
		e1 := envelope(1, arkwenv1.EventType_EVENT_TYPE_RUN_FINISHED)
		e1.Payload = &arkwenv1.EventEnvelope_RunFinished{RunFinished: &arkwenv1.RunFinished{Termination: &arkwenv1.Termination{State: c.term}}}
		if got := Status([]*arkwenv1.EventEnvelope{e0, e1}).GetState(); got != c.want {
			t.Fatalf("terminal %v -> %v, want %v", c.term, got, c.want)
		}
	}
}

func TestArtifactManifestFold(t *testing.T) {
	ref := &arkwenv1.ContentRef{Path: "a.txt", ContentHash: &arkwenv1.Digest{Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: "ab"}}
	e := envelope(0, arkwenv1.EventType_EVENT_TYPE_WORKER_ARTIFACT_WRITTEN)
	e.Payload = &arkwenv1.EventEnvelope_WorkerArtifactWritten{WorkerArtifactWritten: &arkwenv1.WorkerArtifactWritten{ArtifactId: "x", Artifact: ref}}
	if len(ArtifactManifest([]*arkwenv1.EventEnvelope{e}).GetEntries()) != 1 {
		t.Fatal("manifest should fold the artifact event")
	}
}
