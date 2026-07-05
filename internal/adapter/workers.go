package adapter

import (
	"context"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// scriptedRun is the shared, deterministic mission body used by the built-in
// workers so the walking skeleton runs on any host (no external model needed).
// It deliberately exercises every invariant seam:
//   - injects + PRINTS a broker-fed secret to stdout  -> redaction (Inv 5/3)
//   - reaches an allow-listed destination              -> egress ALLOW
//   - attempts an off-allowlist destination            -> egress DENY (floor, Inv 7)
//   - emits the semantic event class                   -> folded only if declared (Inv 9)
//   - writes an artifact                               -> manifest/diff (Inv 4)
func scriptedRun(_ context.Context, wc WorkcellAPI) *arkwenv1.Termination {
	wc.Heartbeat()
	mission := wc.Mission()
	wc.Message("assistant", "starting mission: "+truncate(mission, 80))

	// Use the injected model-API credential (S0 minimal secret path). Printing it
	// to stdout is intentional — the shim MUST redact it before persistence.
	if key, ok := wc.Secret("MODEL_API_KEY"); ok {
		if err := wc.Egress("api.anthropic.com", 443); err == nil {
			wc.Stdout("Calling model API with key " + key + " ... 200 OK")
		} else {
			wc.Stdout("model API unreachable under policy: " + err.Error())
		}
	} else {
		wc.Stdout("no model credential leased; proceeding offline")
	}

	// Prove the default-deny egress floor: this destination is NOT allow-listed.
	if err := wc.Egress("exfil.evil.example", 443); err != nil {
		wc.Stdout("egress to exfil.evil.example blocked (expected: default-deny floor)")
	}

	// Do the "work": write one artifact via a tool call.
	wc.ToolCall("call-1", "fs.write", `{"path":"src/index.ts"}`)
	content := []byte("export const answer = 42;\n")
	if err := wc.WriteArtifact("src/index.ts", content, "text/typescript"); err != nil {
		return failed(fmt.Sprintf("artifact write failed: %v", err))
	}
	wc.ToolResult("call-1", true, fmt.Sprintf(`{"bytesWritten":%d}`, len(content)))

	wc.Message("summary", "model API called successfully; wrote src/index.ts")
	wc.Stdout("done")

	return &arkwenv1.Termination{
		State:  arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED,
		Reason: arkwenv1.TerminalReason_TERMINAL_REASON_COMPLETED_OK,
		Detail: "run completed; 1 artifact written",
	}
}

func failed(detail string) *arkwenv1.Termination {
	return &arkwenv1.Termination{
		State:  arkwenv1.TerminalState_TERMINAL_STATE_FAILED,
		Reason: arkwenv1.TerminalReason_TERMINAL_REASON_WORKER_ERROR,
		Detail: detail,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// blockingWorker runs until its context is canceled, then returns CANCELED. It
// exists so control-signal tests (pause degradation, cancel) can act on a run
// that is genuinely still RUNNING. Its declared capabilities are configurable so
// both the observational and cooperative pause tiers can be exercised.
type blockingWorker struct {
	kind string
	caps []arkwenv1.Capability
}

// NewBlocking returns a worker that blocks until canceled, with the given kind
// and declared capabilities.
func NewBlocking(kind string, caps []arkwenv1.Capability) Worker {
	return &blockingWorker{kind: kind, caps: caps}
}

func (b *blockingWorker) Kind() string                        { return b.kind }
func (b *blockingWorker) Capabilities() []arkwenv1.Capability { return b.caps }
func (b *blockingWorker) Run(ctx context.Context, wc WorkcellAPI) *arkwenv1.Termination {
	wc.Heartbeat()
	wc.Stdout("blocking worker waiting for signal")
	<-ctx.Done()
	return &arkwenv1.Termination{
		State:  arkwenv1.TerminalState_TERMINAL_STATE_CANCELED,
		Reason: arkwenv1.TerminalReason_TERMINAL_REASON_CANCELED_BY_SIGNAL,
		Detail: "canceled by signal",
	}
}

// echoWorker is the MUST-only observational stub (Invariant 1: a second,
// non-Claude adapter that passes the identical lifecycle suite). It declares NO
// KAN capabilities, so its semantic emissions degrade to worker.raw-only folding.
type echoWorker struct{}

// NewEcho returns the echo-stub worker.
func NewEcho() Worker { return &echoWorker{} }

func (e *echoWorker) Kind() string                        { return "echo-stub" }
func (e *echoWorker) Capabilities() []arkwenv1.Capability { return nil }
func (e *echoWorker) Run(ctx context.Context, wc WorkcellAPI) *arkwenv1.Termination {
	return scriptedRun(ctx, wc)
}

// claudeCodeWorker is the fully-cooperative worker declaring all KAN capabilities.
// If the real `claude` CLI + credentials are present AND ARKWEN_REAL_CLAUDE=1, a
// production build would shell out to it; the default is the deterministic
// scripted run so the skeleton is reproducible everywhere.
type claudeCodeWorker struct{}

// NewClaudeCode returns the claude-code worker.
func NewClaudeCode() Worker { return &claudeCodeWorker{} }

func (c *claudeCodeWorker) Kind() string { return "claude-code" }
func (c *claudeCodeWorker) Capabilities() []arkwenv1.Capability {
	return []arkwenv1.Capability{
		arkwenv1.Capability_CAPABILITY_EVENTS_STREAM,
		arkwenv1.Capability_CAPABILITY_CONTROL_PAUSE_RESUME,
		arkwenv1.Capability_CAPABILITY_GATE_INTERACTIVE,
		arkwenv1.Capability_CAPABILITY_TOOLS_STRUCTURED,
	}
}
func (c *claudeCodeWorker) Run(ctx context.Context, wc WorkcellAPI) *arkwenv1.Termination {
	return scriptedRun(ctx, wc)
}

// openHandsWorker is a second cooperative-ish adapter (events.stream only),
// exercising a different capability tier than claude-code.
type openHandsWorker struct{}

// NewOpenHands returns the openhands worker.
func NewOpenHands() Worker { return &openHandsWorker{} }

func (o *openHandsWorker) Kind() string { return "openhands" }
func (o *openHandsWorker) Capabilities() []arkwenv1.Capability {
	return []arkwenv1.Capability{arkwenv1.Capability_CAPABILITY_EVENTS_STREAM}
}
func (o *openHandsWorker) Run(ctx context.Context, wc WorkcellAPI) *arkwenv1.Termination {
	return scriptedRun(ctx, wc)
}
