package controller

import (
	"context"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/gate"
)

// RequestGate instantiates a gate for a run (auto-resolves an AUTO gate, leaves a
// HUMAN gate pending). A mandatory reject terminates the run FAILED/GATE_REJECTED.
func (c *Controller) RequestGate(ctx context.Context, runID string, rule *arkwenv1.GateRule, contextRef *arkwenv1.ContentRef) error {
	terminate, err := c.gates.Request(ctx, runID, rule, contextRef)
	if err != nil {
		return err
	}
	if terminate {
		return c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_GATE_REJECTED, "gate rejected"))
	}
	return nil
}

// ResolveGate resolves a pending gate (the human/consumer resolution channel).
// gate.resolved (+ run.resumed unless a mandatory reject terminates) is a control
// event on the stream (Invariant 2). Returns the gate.resolved seq.
func (c *Controller) ResolveGate(ctx context.Context, runID, gateID string, decision arkwenv1.GateDecision, rationale string, payloadRef *arkwenv1.ContentRef, by *arkwenv1.Principal) (uint64, error) {
	before, _ := c.log.Head(ctx, runID)
	terminate, err := c.gates.Resolve(ctx, runID, gateID, &gate.Resolution{
		By: by, Decision: decision, Rationale: rationale, PayloadRef: payloadRef,
	})
	if err != nil {
		return 0, err
	}
	if terminate {
		if ferr := c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_GATE_REJECTED, "gate rejected: "+gateID)); ferr != nil {
			return 0, ferr
		}
	}
	after, _ := c.log.Head(ctx, runID)
	if after > before {
		return after, nil
	}
	return before, nil
}

// GateTimeout fires the fail-closed timeout path for an unresolved gate.
func (c *Controller) GateTimeout(ctx context.Context, runID, gateID string) error {
	terminate, err := c.gates.Timeout(ctx, runID, gateID)
	if err != nil {
		return err
	}
	if terminate {
		return c.finish(ctx, runID, failedTerm(arkwenv1.TerminalReason_TERMINAL_REASON_GATE_REJECTED, "gate timed out under fail_closed: "+gateID))
	}
	return nil
}

// Reprioritize changes a queued run's priority. Queue ordering stays a projection
// (Invariant 2); inputs are unchanged (ADR-009 R1). Returns the control-event seq.
func (c *Controller) Reprioritize(ctx context.Context, runID string, newPriority int32, by *arkwenv1.Principal) (uint64, error) {
	return c.appendControl(ctx, runID, by, &arkwenv1.EventEnvelope{
		Type:    arkwenv1.EventType_EVENT_TYPE_RUN_REPRIORITIZED,
		Payload: &arkwenv1.EventEnvelope_RunReprioritized{RunReprioritized: &arkwenv1.RunReprioritized{RequestedBy: by, NewPriority: newPriority}},
	})
}
