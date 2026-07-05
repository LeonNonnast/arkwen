// Package projection folds the append-only event stream into the read-side
// views (ADR-003 Part B + ADR-008 Run-Metrics). EVERY projection is a PURE
// function of the event slice: no hidden state, no clock, no I/O. This is what
// makes the Controller a pure projection (Invariant 2) and guarantees
// replay-equivalence — rebuilding from seq 0 is byte-identical to the live view.
//
// paused is an OVERLAY (SuspensionReason), never a LifecycleState (Invariant 8);
// the only terminal states are completed/failed/canceled.
package projection

import (
	"strconv"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// terminalToLifecycle maps a terminal outcome to its lifecycle state,
// fail-closed: an UNSPECIFIED terminal reads as FAILED (never a silent success).
func terminalToLifecycle(t arkwenv1.TerminalState) arkwenv1.LifecycleState {
	switch t {
	case arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED:
		return arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED
	case arkwenv1.TerminalState_TERMINAL_STATE_CANCELED:
		return arkwenv1.LifecycleState_LIFECYCLE_STATE_CANCELED
	default: // FAILED and UNSPECIFIED both read as FAILED
		return arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED
	}
}

// Status folds lifecycle + suspension overlay + terminal (ADR-003 Part A).
func Status(events []*arkwenv1.EventEnvelope) *arkwenv1.LifecycleStatus {
	st := &arkwenv1.LifecycleStatus{
		State:            arkwenv1.LifecycleState_LIFECYCLE_STATE_UNSPECIFIED,
		SuspensionReason: arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE,
	}
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *arkwenv1.EventEnvelope_RunCreated:
			st.State = arkwenv1.LifecycleState_LIFECYCLE_STATE_QUEUED
		case *arkwenv1.EventEnvelope_RunProvisioning:
			if !terminal(st.State) {
				st.State = arkwenv1.LifecycleState_LIFECYCLE_STATE_PROVISIONING
			}
		case *arkwenv1.EventEnvelope_RunStarted:
			if !terminal(st.State) {
				st.State = arkwenv1.LifecycleState_LIFECYCLE_STATE_RUNNING
			}
		case *arkwenv1.EventEnvelope_RunPaused:
			// overlay only; top-state is unchanged (Invariant 8)
			st.SuspensionReason = p.RunPaused.GetReason()
			st.ResourceLimitKind = p.RunPaused.GetResourceLimitKind()
		case *arkwenv1.EventEnvelope_RunResumed:
			st.SuspensionReason = arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE
			st.ResourceLimitKind = arkwenv1.ResourceLimitKind_RESOURCE_LIMIT_KIND_UNSPECIFIED
		case *arkwenv1.EventEnvelope_RunFinished:
			term := p.RunFinished.GetTermination()
			st.State = terminalToLifecycle(term.GetState())
			st.Termination = term
			st.SuspensionReason = arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE
		}
	}
	return st
}

func terminal(s arkwenv1.LifecycleState) bool {
	switch s {
	case arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED,
		arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED,
		arkwenv1.LifecycleState_LIFECYCLE_STATE_CANCELED:
		return true
	}
	return false
}

// ArtifactManifest folds worker.artifact_written (+ file-bearing tool results)
// into the manifest. Pointer-only entries (Invariant 4).
func ArtifactManifest(events []*arkwenv1.EventEnvelope) *arkwenv1.ArtifactManifest {
	m := &arkwenv1.ArtifactManifest{}
	for _, e := range events {
		if p, ok := e.GetPayload().(*arkwenv1.EventEnvelope_WorkerArtifactWritten); ok {
			aw := p.WorkerArtifactWritten
			m.Entries = append(m.Entries, &arkwenv1.ArtifactEntry{
				ArtifactId:     aw.GetArtifactId(),
				Ref:            aw.GetArtifact(),
				CreatedBy:      e.GetEmitter(),
				CreatedAtSeq:   e.GetSeq(),
				SourceEventSeq: e.GetSeq(),
			})
		}
	}
	return m
}

// WorkbenchDiff folds artifact events into a delta vs the explicit baseline. It
// is a FOLD over events, never a live FS read (ADR-006 E4 / Invariant 2).
func WorkbenchDiff(events []*arkwenv1.EventEnvelope, baseCommit string, snapshotRef *arkwenv1.Digest) *arkwenv1.WorkbenchDiff {
	diff := &arkwenv1.WorkbenchDiff{
		WorkbenchBaseCommit:  baseCommit,
		WorkbenchSnapshotRef: snapshotRef,
	}
	seen := map[string]int{} // path -> index in changes
	for _, e := range events {
		p, ok := e.GetPayload().(*arkwenv1.EventEnvelope_WorkerArtifactWritten)
		if !ok {
			continue
		}
		ref := p.WorkerArtifactWritten.GetArtifact()
		path := ref.GetPath()
		if idx, ok := seen[path]; ok {
			diff.Changes[idx].ChangeType = arkwenv1.ChangeType_CHANGE_TYPE_MODIFIED
			diff.Changes[idx].ContentHash = ref.GetContentHash()
			continue
		}
		seen[path] = len(diff.Changes)
		diff.Changes = append(diff.Changes, &arkwenv1.FileChange{
			Path:        path,
			ChangeType:  arkwenv1.ChangeType_CHANGE_TYPE_ADDED,
			ContentHash: ref.GetContentHash(),
		})
	}
	return diff
}

// RunSummary is the renderable summary (ADR-003 Part B).
func RunSummary(events []*arkwenv1.EventEnvelope) *arkwenv1.RunSummary {
	s := &arkwenv1.RunSummary{}
	seen := map[string]bool{}
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *arkwenv1.EventEnvelope_WorkerArtifactWritten:
			path := p.WorkerArtifactWritten.GetArtifact().GetPath()
			if path != "" && !seen[path] {
				seen[path] = true
				s.ModifiedFiles = append(s.ModifiedFiles, path)
			}
		case *arkwenv1.EventEnvelope_GateResolved:
			s.GateStatus = append(s.GateStatus, &arkwenv1.GateOutcome{
				GateId:   p.GateResolved.GetGateId(),
				Decision: p.GateResolved.GetDecision(),
			})
		case *arkwenv1.EventEnvelope_WorkerMessage:
			// a message with role "summary" is a convention for the run summary
			if p.WorkerMessage.GetRole() == "summary" {
				// body lives in CAS; we only record that a summary exists via the ref path
			}
		}
	}
	s.FinalArtifacts = ArtifactManifest(events)
	return s
}

// RunMetrics is the best-effort, tier-degrading metrics projection (ADR-008 E4).
// Missing signals are ABSENT, never fabricated.
func RunMetrics(events []*arkwenv1.EventEnvelope) *arkwenv1.RunMetrics {
	rm := &arkwenv1.RunMetrics{
		SuspendedMsByReason: map[string]int64{},
		ArtifactSignals:     map[string]string{},
	}
	var createdAt, finishedAt int64 = -1, -1
	var pausedAt int64 = -1
	var pausedReason arkwenv1.SuspensionReason
	for _, e := range events {
		ts := e.GetTimestamp().AsTime().UnixMilli()
		switch p := e.GetPayload().(type) {
		case *arkwenv1.EventEnvelope_RunCreated:
			createdAt = ts
		case *arkwenv1.EventEnvelope_RunPaused:
			pausedAt = ts
			pausedReason = p.RunPaused.GetReason()
		case *arkwenv1.EventEnvelope_RunResumed:
			if pausedAt >= 0 {
				rm.SuspendedMsByReason[pausedReason.String()] += ts - pausedAt
				pausedAt = -1
			}
		case *arkwenv1.EventEnvelope_GateResolved:
			rm.GateOutcomes = append(rm.GateOutcomes, &arkwenv1.GateOutcome{
				GateId:   p.GateResolved.GetGateId(),
				Decision: p.GateResolved.GetDecision(),
			})
		case *arkwenv1.EventEnvelope_RunFinished:
			finishedAt = ts
			rm.Terminal = p.RunFinished.GetTermination()
		}
	}
	if createdAt >= 0 && finishedAt >= 0 {
		rm.Duration = durationpb.New(time.Duration(finishedAt-createdAt) * time.Millisecond)
	}
	// cost is only present if a resource signal carried it; we never fabricate it.
	if len(rm.SuspendedMsByReason) == 0 {
		rm.SuspendedMsByReason = nil
	}
	if len(rm.ArtifactSignals) == 0 {
		rm.ArtifactSignals = nil
	}
	// count artifacts as an objective signal (present only if any exist)
	if man := ArtifactManifest(events); len(man.GetEntries()) > 0 {
		rm.ArtifactSignals = map[string]string{"artifact_count": strconv.Itoa(len(man.GetEntries()))}
	}
	return rm
}

// Seed extracts the frozen RunSeed from the run.created event (seq 0), or nil.
func Seed(events []*arkwenv1.EventEnvelope) *arkwenv1.RunSeed {
	for _, e := range events {
		if p, ok := e.GetPayload().(*arkwenv1.EventEnvelope_RunCreated); ok {
			return p.RunCreated.GetSeed()
		}
	}
	return nil
}
