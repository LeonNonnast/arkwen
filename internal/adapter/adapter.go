// Package adapter defines the pluggable Worker contract and the WorkcellAPI the
// Cell-Shim exposes to a worker. This is where Invariant 1 lives at the code
// level: a Worker is an OPAQUE plug-in (Claude Code, an echo stub, OpenHands,
// ...) identified only by a diagnostic `worker_kind`; the Controller never
// branches on it. A worker touches the outside world ONLY through WorkcellAPI, so
// the shim structurally enforces redaction, default-deny egress, artifact capture
// and event emission regardless of which worker runs.
package adapter

import (
	"context"
	"fmt"
	"sort"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// WorkcellAPI is the only surface a worker has to the world. Every method is
// mediated by the shim: Stdout is redacted before persistence; Egress is
// policy-checked (default-deny); WriteArtifact goes to the CAS; the semantic
// methods are emitted only if the worker declared the matching KAN capability.
type WorkcellAPI interface {
	RunID() string
	Mission() string
	// Secret returns a broker-injected scoped secret (already registered for
	// redaction). Returns ("", false) if the run holds no such secret.
	Secret(name string) (string, bool)
	// Stdout emits one line of raw worker output (durable sanitized worker.raw).
	Stdout(line string)
	// Egress requests a network destination; returns an error if denied by policy.
	Egress(host string, port uint32) error
	// WriteArtifact records a workbench artifact (worker.artifact_written).
	WriteArtifact(path string, data []byte, mime string) error
	// Semantic (KAN) — folded only if the worker declared EVENTS_STREAM/TOOLS_STRUCTURED.
	Message(role, text string)
	ToolCall(callID, tool, argsJSON string)
	ToolResult(callID string, ok bool, resultJSON string)
	// Heartbeat emits a liveness beat (health class).
	Heartbeat()
}

// Worker is a pluggable worker. Run executes the mission and returns the terminal
// outcome. A returned error is folded into a FAILED termination by the shim.
type Worker interface {
	Kind() string                        // opaque diagnostic label (NegotiateResponse.worker_kind)
	Capabilities() []arkwenv1.Capability // declared KAN capabilities (MUST verbs are unconditional)
	Run(ctx context.Context, wc WorkcellAPI) *arkwenv1.Termination
}

// Factory builds a fresh worker instance.
type Factory func() Worker

// Registry maps worker kind -> factory. The Controller selects by kind; it never
// special-cases behaviour on the kind (Invariant 1).
type Registry struct{ m map[string]Factory }

// NewRegistry returns a registry preloaded with the built-in workers.
func NewRegistry() *Registry {
	r := &Registry{m: map[string]Factory{}}
	r.Register("echo-stub", func() Worker { return NewEcho() })
	r.Register("claude-code", func() Worker { return NewClaudeCode() })
	r.Register("openhands", func() Worker { return NewOpenHands() })
	r.Register("blocking-observational", func() Worker { return NewBlocking("blocking-observational", nil) })
	r.Register("blocking-cooperative", func() Worker {
		return NewBlocking("blocking-cooperative", []arkwenv1.Capability{
			arkwenv1.Capability_CAPABILITY_EVENTS_STREAM,
			arkwenv1.Capability_CAPABILITY_CONTROL_PAUSE_RESUME,
			arkwenv1.Capability_CAPABILITY_GATE_INTERACTIVE,
			arkwenv1.Capability_CAPABILITY_TOOLS_STRUCTURED,
		})
	})
	return r
}

// Register adds a worker factory under kind.
func (r *Registry) Register(kind string, f Factory) { r.m[kind] = f }

// Build instantiates a worker by kind.
func (r *Registry) Build(kind string) (Worker, error) {
	f, ok := r.m[kind]
	if !ok {
		return nil, fmt.Errorf("adapter: unknown worker kind %q", kind)
	}
	return f(), nil
}

// Kinds lists registered worker kinds (sorted).
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HasCapability reports whether caps contains c.
func HasCapability(caps []arkwenv1.Capability, c arkwenv1.Capability) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}
