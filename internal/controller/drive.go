package controller

import (
	"context"
	"errors"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/isolation"
	"github.com/arkwen/arkwen/internal/projection"
	"github.com/arkwen/arkwen/internal/secret"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// allCaps is what the Controller desires from any adapter. Missing ones degrade
// observability only (Invariant 9); the Controller behaves identically otherwise.
var allCaps = []arkwenv1.Capability{
	arkwenv1.Capability_CAPABILITY_EVENTS_STREAM,
	arkwenv1.Capability_CAPABILITY_CONTROL_PAUSE_RESUME,
	arkwenv1.Capability_CAPABILITY_GATE_INTERACTIVE,
	arkwenv1.Capability_CAPABILITY_TOOLS_STRUCTURED,
}

// Drive advances a created run to a terminal state via the generic adapter verbs,
// appending lifecycle events and letting the shim fold worker events onto the
// stream. It is idempotent-safe: a run already terminal is left untouched.
func (c *Controller) Drive(ctx context.Context, runID string) error {
	evs, err := c.log.Read(ctx, runID, 0)
	if err != nil {
		return err
	}
	if len(evs) == 0 {
		return errors.New("controller: cannot drive unknown run")
	}
	if projStatus := projection.Status(evs); isTerminal(projStatus.GetState()) {
		return nil // already done
	}
	seed := projection.Seed(evs)
	workerKind, err := c.workerKindForSeed(seed)
	if err != nil {
		return c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_ADAPTER_ERROR, err.Error()))
	}

	// Negotiate (opaque worker_kind; caps only affect observability).
	if _, nerr := c.shim.Negotiate(workerKind, allCaps); nerr != nil {
		return c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_ADAPTER_ERROR, nerr.Error()))
	}

	iso, err := c.resolveIsolation(ctx, seed)
	if err != nil {
		return c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_ADAPTER_ERROR, err.Error()))
	}

	// run.provisioning — re-verifies the frozen isolation contract (never re-resolves).
	if err := c.appendLifecycle(ctx, runID, &arkwenv1.EventEnvelope{
		Type:    arkwenv1.EventType_EVENT_TYPE_RUN_PROVISIONING,
		Payload: &arkwenv1.EventEnvelope_RunProvisioning{RunProvisioning: &arkwenv1.RunProvisioning{Isolation: iso}},
	}); err != nil {
		return err
	}

	// Create the workcell — FAIL-CLOSED on an unsatisfiable isolation profile.
	if _, err := c.shim.Create(ctx, runID, workerKind, seed, iso, scopesFrom(seed)); err != nil {
		reason := arkwenv1.TerminalReason_TERMINAL_REASON_ADAPTER_ERROR
		if errors.Is(err, isolation.ErrUnsatisfiable) {
			reason = arkwenv1.TerminalReason_TERMINAL_REASON_ISOLATION_UNSATISFIABLE
		}
		return c.finish(ctx, runID, failedTerm(reason, err.Error()))
	}

	// run.started — BEFORE the worker emits, so ordering is correct.
	if err := c.appendLifecycle(ctx, runID, &arkwenv1.EventEnvelope{
		Type:    arkwenv1.EventType_EVENT_TYPE_RUN_STARTED,
		Payload: &arkwenv1.EventEnvelope_RunStarted{RunStarted: &arkwenv1.RunStarted{}},
	}); err != nil {
		return err
	}
	if _, err := c.shim.Start(ctx, runID); err != nil {
		return c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_ADAPTER_ERROR, err.Error()))
	}

	// Reap waits for the worker (all its events are folded first), then revokes
	// every secret lease for ANY terminal state incl. crash.
	term, err := c.shim.Reap(ctx, runID)
	if err != nil {
		return c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_ADAPTER_ERROR, err.Error()))
	}
	return c.finish(ctx, runID, term)
}

// Cancel requests cancellation of a running run (the universal MUST signal).
func (c *Controller) Cancel(ctx context.Context, runID string, by *arkwenv1.Principal) (uint64, error) {
	seq, err := c.appendControl(ctx, runID, by, &arkwenv1.EventEnvelope{
		Type:    arkwenv1.EventType_EVENT_TYPE_RUN_CANCEL_REQUESTED,
		Payload: &arkwenv1.EventEnvelope_RunCancelRequested{RunCancelRequested: &arkwenv1.RunCancelRequested{RequestedBy: by}},
	})
	if err != nil {
		return 0, err
	}
	_, _ = c.shim.Signal(ctx, runID, arkwenv1.ShimSignal_SHIM_SIGNAL_CANCEL)
	return seq, nil
}

// Pause/Resume drive the KAN control path; without CONTROL_PAUSE_RESUME the shim
// degrades to workcell-boundary freeze — the paused OVERLAY still holds.
func (c *Controller) Pause(ctx context.Context, runID string, by *arkwenv1.Principal, reason arkwenv1.SuspensionReason) (uint64, error) {
	if reason == arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE {
		reason = arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_USER
	}
	_, _ = c.shim.Signal(ctx, runID, arkwenv1.ShimSignal_SHIM_SIGNAL_PAUSE)
	return c.appendControl(ctx, runID, by, &arkwenv1.EventEnvelope{
		Type:    arkwenv1.EventType_EVENT_TYPE_RUN_PAUSED,
		Payload: &arkwenv1.EventEnvelope_RunPaused{RunPaused: &arkwenv1.RunPaused{Reason: reason}},
	})
}

func (c *Controller) Resume(ctx context.Context, runID string, by *arkwenv1.Principal, topup *arkwenv1.CostBudget) (uint64, error) {
	_, _ = c.shim.Signal(ctx, runID, arkwenv1.ShimSignal_SHIM_SIGNAL_RESUME)
	return c.appendControl(ctx, runID, by, &arkwenv1.EventEnvelope{
		Type:    arkwenv1.EventType_EVENT_TYPE_RUN_RESUMED,
		Payload: &arkwenv1.EventEnvelope_RunResumed{RunResumed: &arkwenv1.RunResumed{ResumedBy: by, CostBudgetTopup: topup}},
	})
}

// --- helpers ---

// appendLifecycle appends a controller-emitted lifecycle event. The caller sets
// Type + Payload; this fills the envelope invariants.
func (c *Controller) appendLifecycle(ctx context.Context, runID string, ev *arkwenv1.EventEnvelope) error {
	ev.RunId = runID
	ev.SchemaVersion = 1
	ev.Emitter = c.id
	ev.Timestamp = timestamppb.Now()
	_, err := c.log.Append(ctx, ev)
	return err
}

func (c *Controller) appendControl(ctx context.Context, runID string, by *arkwenv1.Principal, ev *arkwenv1.EventEnvelope) (uint64, error) {
	ev.RunId = runID
	ev.SchemaVersion = 1
	ev.Emitter = c.id
	ev.Source = by
	ev.Timestamp = timestamppb.Now()
	stored, err := c.log.Append(ctx, ev)
	if err != nil {
		return 0, err
	}
	return stored.GetSeq(), nil
}

func (c *Controller) finish(ctx context.Context, runID string, term *arkwenv1.Termination) error {
	return c.appendLifecycle(ctx, runID, &arkwenv1.EventEnvelope{
		Type:    arkwenv1.EventType_EVENT_TYPE_RUN_FINISHED,
		Payload: &arkwenv1.EventEnvelope_RunFinished{RunFinished: &arkwenv1.RunFinished{Termination: term}},
	})
}

func failedTerm(reason arkwenv1.TerminalReason, detail string) *arkwenv1.Termination {
	return &arkwenv1.Termination{State: arkwenv1.TerminalState_TERMINAL_STATE_FAILED, Reason: reason, Detail: detail}
}

func isTerminal(s arkwenv1.LifecycleState) bool {
	switch s {
	case arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED,
		arkwenv1.LifecycleState_LIFECYCLE_STATE_FAILED,
		arkwenv1.LifecycleState_LIFECYCLE_STATE_CANCELED:
		return true
	}
	return false
}

func scopesFrom(seed *arkwenv1.RunSeed) []secret.Scope {
	// S0: the single model-API credential. S2b lets a run request arbitrary scopes.
	return []secret.Scope{{Name: "MODEL_API_KEY", Purpose: "worker model-API credential"}}
}
