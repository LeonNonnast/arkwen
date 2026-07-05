// Package shim is the Cell-Shim (ADR-001, containerd-shim-v2 pattern) — the
// adapter that runs INSIDE the workcell next to the worker. It speaks only the
// Arkwen protocol, launches the worker, captures its output as durable SANITIZED
// worker.raw (redaction BEFORE persistence — Invariant 5/3), enforces the
// default-deny egress floor (Invariant 7), collects artifacts, and reports state.
//
// The worker touches the world only through the workcell (adapter.WorkcellAPI),
// so every invariant seam is enforced here regardless of which worker runs
// (Invariant 1).
package shim

import (
	"context"
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/adapter"
	"github.com/arkwen/arkwen/internal/ids"
	"github.com/arkwen/arkwen/internal/isolation"
	"github.com/arkwen/arkwen/internal/redaction"
	"github.com/arkwen/arkwen/internal/secret"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EmitFunc is how the shim hands an event to the run's append-only log. The
// Controller wires this to log.Append, which assigns the run-scoped monotone seq
// (the Controller owns the stream; the shim is an intra-trust emitter).
type EmitFunc func(ctx context.Context, ev *arkwenv1.EventEnvelope) error

// workcell is the per-run execution context and the adapter.WorkcellAPI the
// worker sees. It is the single choke point for redaction, egress and emission.
type workcell struct {
	runID    string
	emitter  string
	mission  string
	caps     []arkwenv1.Capability
	guard    *isolation.EgressGuard
	redactor *redaction.Engine
	cas      casPutter
	emit     EmitFunc
	leases   []secret.Lease

	mu         sync.Mutex
	secrets    map[string]string // env name -> value (transient; never persisted)
	overlay    arkwenv1.SuspensionReason
	resKind    arkwenv1.ResourceLimitKind
	term       *arkwenv1.Termination
	done       chan struct{}
	cancelCtx  context.CancelFunc
	records    []*arkwenv1.EventEnvelope // shim-local copy for Artifacts/StreamEvents
	localSeq   uint64
	started    bool
	canceled   bool
	doneClosed bool
}

// casPutter is the subset of cas.Store the shim needs (kept small for testing).
type casPutter interface {
	Put(ctx context.Context, path string, data []byte, mime string) (*arkwenv1.ContentRef, error)
}

func (w *workcell) RunID() string   { return w.runID }
func (w *workcell) Mission() string { return w.mission }

func (w *workcell) Secret(name string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	v, ok := w.secrets[name]
	return v, ok
}

// emitEnvelope stamps and records an event, then hands it to the log. Redaction
// has ALREADY happened on any content this envelope points to.
func (w *workcell) emitEnvelope(setType arkwenv1.EventType, set func(*arkwenv1.EventEnvelope)) {
	ev := &arkwenv1.EventEnvelope{
		RunId:         w.runID,
		SchemaVersion: 1,
		Timestamp:     timestamppb.Now(),
		Type:          setType,
		Emitter:       w.emitter,
	}
	set(ev)
	w.mu.Lock()
	ev.Seq = w.localSeq
	w.localSeq++
	w.records = append(w.records, ev)
	w.mu.Unlock()
	if w.emit != nil {
		_ = w.emit(context.Background(), ev)
	}
}

// putRedacted redacts data on `channel`, stores the SANITIZED bytes in the CAS,
// emits secret_leak_detected + redaction_applied when a secret was present, and
// returns the pointer to the sanitized content. The unredacted bytes never leave
// this function (they are never given to the CAS or the stream) — Invariant 5.
func (w *workcell) putRedacted(channel, path string, data []byte, mime string) *arkwenv1.ContentRef {
	res := w.redactor.Redact(data)
	if res.Total > 0 {
		w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_SECRET_LEAK_DETECTED, func(ev *arkwenv1.EventEnvelope) {
			ev.Payload = &arkwenv1.EventEnvelope_SecretLeakDetected{SecretLeakDetected: &arkwenv1.SecretLeakDetected{
				Channel: channel, MatchCount: uint32(res.Total),
			}}
		})
		for ruleID, n := range res.PerRule {
			rid, cnt := ruleID, n
			w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_REDACTION_APPLIED, func(ev *arkwenv1.EventEnvelope) {
				ev.Payload = &arkwenv1.EventEnvelope_RedactionApplied{RedactionApplied: &arkwenv1.RedactionApplied{
					Channel: channel, RedactionCount: uint32(cnt), RuleId: rid,
				}}
			})
		}
	}
	ref, err := w.cas.Put(context.Background(), path, res.Sanitized, mime)
	if err != nil {
		// a CAS failure must not leak raw content; emit an error event instead.
		w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_ERROR, func(ev *arkwenv1.EventEnvelope) {
			ev.Payload = &arkwenv1.EventEnvelope_Error{Error: &arkwenv1.ErrorEvent{
				Code: "cas_put_failed", Message: err.Error(),
			}}
		})
		return nil
	}
	return ref
}

func (w *workcell) Stdout(line string) {
	ref := w.putRedacted("stdout", "raw/stdout.log", []byte(line+"\n"), "text/plain")
	if ref == nil {
		return
	}
	w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_WORKER_RAW, func(ev *arkwenv1.EventEnvelope) {
		ev.Payload = &arkwenv1.EventEnvelope_WorkerRaw{WorkerRaw: &arkwenv1.WorkerRaw{
			Channel: "stdout", RawRef: ref,
		}}
	})
}

func (w *workcell) Egress(host string, port uint32) error {
	action, reason := w.guard.Check(host, port)
	if action == arkwenv1.EgressAction_EGRESS_ACTION_ALLOW {
		return nil
	}
	w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_EGRESS_DENIED, func(ev *arkwenv1.EventEnvelope) {
		ev.Payload = &arkwenv1.EventEnvelope_EgressDenied{EgressDenied: &arkwenv1.EgressDenied{
			HostSniRedacted: isolation.RedactHost(host), Reason: reason,
		}}
	})
	return &egressDenied{host: isolation.RedactHost(host), reason: reason}
}

func (w *workcell) WriteArtifact(path string, data []byte, mime string) error {
	ref := w.putRedacted("file-event", path, data, mime)
	if ref == nil {
		return context.Canceled
	}
	w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_WORKER_ARTIFACT_WRITTEN, func(ev *arkwenv1.EventEnvelope) {
		ev.Payload = &arkwenv1.EventEnvelope_WorkerArtifactWritten{WorkerArtifactWritten: &arkwenv1.WorkerArtifactWritten{
			ArtifactId: ids.Short("art"), Artifact: ref,
		}}
	})
	return nil
}

func (w *workcell) Message(role, text string) {
	if !adapter.HasCapability(w.caps, arkwenv1.Capability_CAPABILITY_EVENTS_STREAM) {
		return // observational degradation: semantic events absent (Invariant 9)
	}
	ref := w.putRedacted("message", "conversation/message.txt", []byte(text), "text/plain")
	if ref == nil {
		return
	}
	w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_WORKER_MESSAGE, func(ev *arkwenv1.EventEnvelope) {
		ev.Payload = &arkwenv1.EventEnvelope_WorkerMessage{WorkerMessage: &arkwenv1.WorkerMessage{
			Role: role, MessageRef: ref,
		}}
	})
}

func (w *workcell) ToolCall(callID, tool, argsJSON string) {
	if !adapter.HasCapability(w.caps, arkwenv1.Capability_CAPABILITY_TOOLS_STRUCTURED) {
		return
	}
	ref := w.putRedacted("tool_call", "toolcalls/"+callID+".args.json", []byte(argsJSON), "application/json")
	if ref == nil {
		return
	}
	w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_CALL, func(ev *arkwenv1.EventEnvelope) {
		ev.Payload = &arkwenv1.EventEnvelope_WorkerToolCall{WorkerToolCall: &arkwenv1.WorkerToolCall{
			CallId: callID, ToolName: tool, ArgumentsRef: ref,
		}}
	})
}

func (w *workcell) ToolResult(callID string, ok bool, resultJSON string) {
	if !adapter.HasCapability(w.caps, arkwenv1.Capability_CAPABILITY_TOOLS_STRUCTURED) {
		return
	}
	ref := w.putRedacted("tool_result", "toolcalls/"+callID+".result.json", []byte(resultJSON), "application/json")
	if ref == nil {
		return
	}
	w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_WORKER_TOOL_RESULT, func(ev *arkwenv1.EventEnvelope) {
		ev.Payload = &arkwenv1.EventEnvelope_WorkerToolResult{WorkerToolResult: &arkwenv1.WorkerToolResult{
			CallId: callID, Ok: ok, ResultRef: ref,
		}}
	})
}

func (w *workcell) Heartbeat() {
	w.emitEnvelope(arkwenv1.EventType_EVENT_TYPE_HEARTBEAT, func(ev *arkwenv1.EventEnvelope) {
		ev.Payload = &arkwenv1.EventEnvelope_Heartbeat{Heartbeat: &arkwenv1.Heartbeat{}}
	})
}

// egressDenied is the error a worker sees when a destination is off-allowlist.
type egressDenied struct{ host, reason string }

func (e *egressDenied) Error() string { return "egress denied to " + e.host + ": " + e.reason }
