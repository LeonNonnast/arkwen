package shim

import (
	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/proto"
)

// setStarted marks the workcell as running (called by Start).
func (w *workcell) setStarted() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
}

// setTerminal records the worker's terminal outcome exactly once. If a cancel was
// requested, the outcome is forced to CANCELED regardless of what the worker
// returned (cancel is authoritative).
func (w *workcell) setTerminal(t *arkwenv1.Termination) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.doneClosed {
		return
	}
	if w.canceled {
		t = &arkwenv1.Termination{
			State:  arkwenv1.TerminalState_TERMINAL_STATE_CANCELED,
			Reason: arkwenv1.TerminalReason_TERMINAL_REASON_CANCELED_BY_SIGNAL,
			Detail: "canceled by signal",
		}
	}
	if t == nil {
		t = &arkwenv1.Termination{
			State:  arkwenv1.TerminalState_TERMINAL_STATE_FAILED,
			Reason: arkwenv1.TerminalReason_TERMINAL_REASON_WORKER_ERROR,
			Detail: "worker returned no termination",
		}
	}
	w.term = t
	w.doneClosed = true
	close(w.done)
}

// cancel requests cancellation (the universal MUST signal).
func (w *workcell) cancel() {
	w.mu.Lock()
	w.canceled = true
	cancel := w.cancelCtx
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// setOverlay sets the suspension overlay (never a top-state — Invariant 8).
func (w *workcell) setOverlay(r arkwenv1.SuspensionReason) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.overlay = r
}

// status returns the projected lifecycle status.
func (w *workcell) status() *arkwenv1.LifecycleStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.term != nil {
		return &arkwenv1.LifecycleStatus{State: terminalToLifecycle(w.term.GetState()), Termination: w.term}
	}
	state := arkwenv1.LifecycleState_LIFECYCLE_STATE_PROVISIONING
	if w.started {
		state = arkwenv1.LifecycleState_LIFECYCLE_STATE_RUNNING
	}
	return &arkwenv1.LifecycleStatus{State: state, SuspensionReason: w.overlay, ResourceLimitKind: w.resKind}
}

// terminal returns the recorded terminal outcome (nil if not terminal yet).
func (w *workcell) terminal() *arkwenv1.Termination {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.term
}

// snapshot returns a copy of the shim-local event records.
func (w *workcell) snapshot() []*arkwenv1.EventEnvelope {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*arkwenv1.EventEnvelope, len(w.records))
	for i, e := range w.records {
		out[i] = proto.Clone(e).(*arkwenv1.EventEnvelope)
	}
	return out
}

func terminalToLifecycle(t arkwenv1.TerminalState) arkwenv1.LifecycleState {
	switch t {
	case arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED:
		return arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED
	case arkwenv1.TerminalState_TERMINAL_STATE_CANCELED:
		return arkwenv1.LifecycleState_LIFECYCLE_STATE_CANCELED
	default:
		return arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED
	}
}
