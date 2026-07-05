// Package eventlog is the append-only event store — THE truth of Arkwen
// (Invariant 2). The Factory Controller is a pure projection over it; there is no
// mutable shadow state. Persistence enforces UNIQUE(run_id, seq) and monotone seq
// and supports replay-from-seq and durable fan-out with NO producer backpressure
// (a slow/absent subscriber never blocks execution — Invariants 2/10).
package eventlog

import (
	"errors"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

var (
	// ErrRunExists is returned by Create when the run already has events (a
	// duplicate enqueue is idempotent, not an error the caller must fear).
	ErrRunExists = errors.New("eventlog: run already exists")
	// ErrInvalidEnvelope marks a structurally invalid event (fail-closed reject).
	ErrInvalidEnvelope = errors.New("eventlog: invalid envelope")
)

// typeForPayload returns the EventType that MUST accompany the populated payload
// oneof case, and whether a payload was set at all. This is how we enforce that
// the discriminator (EventEnvelope.type) agrees with the payload (golden-run G3).
func typeForPayload(ev *arkwenv1.EventEnvelope) (arkwenv1.EventType, bool) {
	switch ev.GetPayload().(type) {
	case *arkwenv1.EventEnvelope_RunCreated:
		return arkwenv1.EventType_EVENT_TYPE_RUN_CREATED, true
	case *arkwenv1.EventEnvelope_RunProvisioning:
		return arkwenv1.EventType_EVENT_TYPE_RUN_PROVISIONING, true
	case *arkwenv1.EventEnvelope_RunStarted:
		return arkwenv1.EventType_EVENT_TYPE_RUN_STARTED, true
	case *arkwenv1.EventEnvelope_RunFinished:
		return arkwenv1.EventType_EVENT_TYPE_RUN_FINISHED, true
	case *arkwenv1.EventEnvelope_WorkerMessage:
		return arkwenv1.EventType_EVENT_TYPE_WORKER_MESSAGE, true
	case *arkwenv1.EventEnvelope_WorkerToolCall:
		return arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_CALL, true
	case *arkwenv1.EventEnvelope_WorkerToolResult:
		return arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_RESULT, true
	case *arkwenv1.EventEnvelope_WorkerArtifactWritten:
		return arkwenv1.EventType_EVENT_TYPE_WORKER_ARTIFACT_WRITTEN, true
	case *arkwenv1.EventEnvelope_GateRequested:
		return arkwenv1.EventType_EVENT_TYPE_GATE_REQUESTED, true
	case *arkwenv1.EventEnvelope_GateResolved:
		return arkwenv1.EventType_EVENT_TYPE_GATE_RESOLVED, true
	case *arkwenv1.EventEnvelope_RunPaused:
		return arkwenv1.EventType_EVENT_TYPE_RUN_PAUSED, true
	case *arkwenv1.EventEnvelope_RunResumed:
		return arkwenv1.EventType_EVENT_TYPE_RUN_RESUMED, true
	case *arkwenv1.EventEnvelope_RunCancelRequested:
		return arkwenv1.EventType_EVENT_TYPE_RUN_CANCEL_REQUESTED, true
	case *arkwenv1.EventEnvelope_RunReprioritized:
		return arkwenv1.EventType_EVENT_TYPE_RUN_REPRIORITIZED, true
	case *arkwenv1.EventEnvelope_Heartbeat:
		return arkwenv1.EventType_EVENT_TYPE_HEARTBEAT, true
	case *arkwenv1.EventEnvelope_ResourceSample:
		return arkwenv1.EventType_EVENT_TYPE_RESOURCE_SAMPLE, true
	case *arkwenv1.EventEnvelope_Error:
		return arkwenv1.EventType_EVENT_TYPE_ERROR, true
	case *arkwenv1.EventEnvelope_SecretLeakDetected:
		return arkwenv1.EventType_EVENT_TYPE_SECRET_LEAK_DETECTED, true
	case *arkwenv1.EventEnvelope_RedactionApplied:
		return arkwenv1.EventType_EVENT_TYPE_REDACTION_APPLIED, true
	case *arkwenv1.EventEnvelope_SecretLeased:
		return arkwenv1.EventType_EVENT_TYPE_SECRET_LEASED, true
	case *arkwenv1.EventEnvelope_SecretRotated:
		return arkwenv1.EventType_EVENT_TYPE_SECRET_ROTATED, true
	case *arkwenv1.EventEnvelope_SecretRevoked:
		return arkwenv1.EventType_EVENT_TYPE_SECRET_REVOKED, true
	case *arkwenv1.EventEnvelope_EgressDenied:
		return arkwenv1.EventType_EVENT_TYPE_EGRESS_DENIED, true
	case *arkwenv1.EventEnvelope_WorkerRaw:
		return arkwenv1.EventType_EVENT_TYPE_WORKER_RAW, true
	default:
		return arkwenv1.EventType_EVENT_TYPE_UNSPECIFIED, false
	}
}

// contentRefs returns every ContentRef carried by ev's payload, so the pointer
// integrity guard can reject an event whose "pointer" carries no content hash
// (Invariant 4: events carry real pointers into the CAS, never inline content).
func contentRefs(ev *arkwenv1.EventEnvelope) []*arkwenv1.ContentRef {
	var refs []*arkwenv1.ContentRef
	add := func(r *arkwenv1.ContentRef) {
		if r != nil {
			refs = append(refs, r)
		}
	}
	switch p := ev.GetPayload().(type) {
	case *arkwenv1.EventEnvelope_WorkerMessage:
		add(p.WorkerMessage.GetMessageRef())
	case *arkwenv1.EventEnvelope_WorkerToolCall:
		add(p.WorkerToolCall.GetArgumentsRef())
	case *arkwenv1.EventEnvelope_WorkerToolResult:
		add(p.WorkerToolResult.GetResultRef())
	case *arkwenv1.EventEnvelope_WorkerArtifactWritten:
		add(p.WorkerArtifactWritten.GetArtifact())
	case *arkwenv1.EventEnvelope_WorkerRaw:
		add(p.WorkerRaw.GetRawRef())
	case *arkwenv1.EventEnvelope_GateResolved:
		add(p.GateResolved.GetResolutionPayloadRef())
	case *arkwenv1.EventEnvelope_Error:
		add(p.Error.GetDetailRef())
	case *arkwenv1.EventEnvelope_GateRequested:
		add(p.GateRequested.GetContextRef())
	}
	return refs
}

// Validate enforces the structural rules every persisted event must satisfy
// (fail-closed). It is applied at append time so the store can never hold an
// event that violates the envelope contract.
func Validate(ev *arkwenv1.EventEnvelope) error {
	if ev == nil {
		return fmt.Errorf("%w: nil", ErrInvalidEnvelope)
	}
	if ev.GetRunId() == "" {
		return fmt.Errorf("%w: empty run_id", ErrInvalidEnvelope)
	}
	if ev.GetSchemaVersion() < 1 {
		return fmt.Errorf("%w: schema_version must be >= 1", ErrInvalidEnvelope)
	}
	if ev.GetTimestamp() == nil {
		return fmt.Errorf("%w: missing timestamp", ErrInvalidEnvelope)
	}
	if ev.GetType() == arkwenv1.EventType_EVENT_TYPE_UNSPECIFIED {
		return fmt.Errorf("%w: EVENT_TYPE_UNSPECIFIED", ErrInvalidEnvelope)
	}
	want, ok := typeForPayload(ev)
	if !ok {
		return fmt.Errorf("%w: no payload set", ErrInvalidEnvelope)
	}
	if want != ev.GetType() {
		return fmt.Errorf("%w: type %v != payload %v", ErrInvalidEnvelope, ev.GetType(), want)
	}
	// Pointer integrity: any ContentRef present must be a real CAS pointer.
	for _, r := range contentRefs(ev) {
		d := r.GetContentHash()
		if d == nil || d.GetHex() == "" {
			return fmt.Errorf("%w: content ref without content_hash (no inline content allowed)", ErrInvalidEnvelope)
		}
		if d.GetAlgorithm() != arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 {
			return fmt.Errorf("%w: content ref with unverifiable digest algorithm", ErrInvalidEnvelope)
		}
	}
	return nil
}
