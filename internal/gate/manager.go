package gate

import (
	"context"
	"fmt"
	"sync"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Emit hands a control event to the run's append-only log. The Controller wires
// this to log.Append (which assigns the run-scoped monotone seq); the gate spine
// never owns truth, it only relays generic control events (Invariant 2).
type Emit func(ctx context.Context, ev *arkwenv1.EventEnvelope) error

// PromotionHook lets the Warehouse (S3) plug into GATE_SCOPE_PROMOTION resolutions
// so a dev->tested->released promotion is gated by the SAME spine, writing to the
// Warehouse ledger rather than the run stream (ADR-007 E5).
type PromotionHook interface {
	OnPromotionResolved(ctx context.Context, gateID string, decision arkwenv1.GateDecision, by *arkwenv1.Principal) error
}

// Manager tracks pending gates per run and drives the control-event flow.
type Manager struct {
	emit      Emit
	emitter   string
	resolvers map[arkwenv1.ResolverKind]Resolver
	promotion PromotionHook

	mu      sync.Mutex
	pending map[string]*pending
}

type pending struct {
	runID    string
	rule     *arkwenv1.GateRule
	deadline time.Time // zero => no deadline
}

// Option configures a Manager.
type Option func(*Manager)

// WithResolver registers a resolver for its kind.
func WithResolver(r Resolver) Option {
	return func(m *Manager) { m.resolvers[r.Kind()] = r }
}

// WithPromotionHook wires the Warehouse intake resolver for promotion gates.
func WithPromotionHook(h PromotionHook) Option { return func(m *Manager) { m.promotion = h } }

// WithEmitter sets the emitter id stamped on gate events (default "controller").
func WithEmitter(id string) Option { return func(m *Manager) { m.emitter = id } }

// NewManager builds a gate Manager. By default a Human resolver is registered
// (gates stay pending until an external ResolveGate); register an AutoResolver to
// decide auto gates synchronously.
func NewManager(emit Emit, opts ...Option) *Manager {
	m := &Manager{
		emit:      emit,
		emitter:   "controller",
		resolvers: map[arkwenv1.ResolverKind]Resolver{arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN: HumanResolver{}},
		pending:   map[string]*pending{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

func key(runID, gateID string) string { return runID + "\x00" + gateID }

// Request instantiates a gate: it validates the rule (fail-closed), emits
// gate.requested + run.paused, then either resolves synchronously (Auto) or
// leaves the gate pending (Human/external). Returns terminate=true if a
// synchronous resolution rejected a mandatory gate (the run must end FAILED).
func (m *Manager) Request(ctx context.Context, runID string, rule *arkwenv1.GateRule, contextRef *arkwenv1.ContentRef) (terminate bool, err error) {
	if err := ValidateRule(rule); err != nil {
		return false, err
	}
	reason := SuspensionReasonFor(rule)
	if err := m.emit(ctx, m.stamp(runID, arkwenv1.EventType_EVENT_TYPE_GATE_REQUESTED, nil,
		&arkwenv1.EventEnvelope{Payload: &arkwenv1.EventEnvelope_GateRequested{GateRequested: &arkwenv1.GateRequested{
			Gate:              rule,
			SuspensionReason:  reason,
			ResourceLimitKind: rule.GetResourceLimitKind(),
			ContextRef:        contextRef,
		}}})); err != nil {
		return false, err
	}
	if err := m.emit(ctx, m.stamp(runID, arkwenv1.EventType_EVENT_TYPE_RUN_PAUSED, nil,
		&arkwenv1.EventEnvelope{Payload: &arkwenv1.EventEnvelope_RunPaused{RunPaused: &arkwenv1.RunPaused{
			Reason:            reason,
			ResourceLimitKind: rule.GetResourceLimitKind(),
		}}})); err != nil {
		return false, err
	}

	var deadline time.Time
	if d := rule.GetMaxWait().AsDuration(); d > 0 {
		deadline = time.Now().Add(d)
	}
	m.mu.Lock()
	m.pending[key(runID, rule.GetGateId())] = &pending{runID: runID, rule: rule, deadline: deadline}
	m.mu.Unlock()

	if r := m.resolvers[rule.GetResolver()]; r != nil {
		if res, ok := r.Resolve(rule); ok {
			return m.apply(ctx, runID, rule, res, false)
		}
	}
	return false, nil
}

// Resolve applies an external resolution (Human via the Control Room / command
// plane) to a pending gate. Returns terminate=true if a mandatory gate was
// rejected.
func (m *Manager) Resolve(ctx context.Context, runID, gateID string, res *Resolution) (bool, error) {
	m.mu.Lock()
	p, ok := m.pending[key(runID, gateID)]
	m.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("gate: no pending gate %q for run %q", gateID, runID)
	}
	return m.apply(ctx, runID, p.rule, res, false)
}

// Timeout resolves a pending gate whose max_wait elapsed with no resolution,
// fail-closed (REJECT under FAIL_CLOSED; APPROVE only under explicit FAIL_OPEN).
func (m *Manager) Timeout(ctx context.Context, runID, gateID string) (bool, error) {
	m.mu.Lock()
	p, ok := m.pending[key(runID, gateID)]
	m.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("gate: no pending gate %q for run %q", gateID, runID)
	}
	return m.apply(ctx, runID, p.rule, nil, true)
}

// SweepExpired times out every pending gate whose deadline has passed. Returns
// the gate ids that caused the run to terminate.
func (m *Manager) SweepExpired(ctx context.Context, now time.Time) ([]string, error) {
	m.mu.Lock()
	var due []*pending
	for _, p := range m.pending {
		if !p.deadline.IsZero() && now.After(p.deadline) {
			due = append(due, p)
		}
	}
	m.mu.Unlock()
	var terminated []string
	for _, p := range due {
		term, err := m.apply(ctx, p.runID, p.rule, nil, true)
		if err != nil {
			return terminated, err
		}
		if term {
			terminated = append(terminated, p.rule.GetGateId())
		}
	}
	return terminated, nil
}

// apply computes the decision, emits gate.resolved (+ run.resumed unless the run
// terminates), routes promotion gates to the Warehouse, and clears the pending
// entry. An ESCALATE decision keeps the gate pending (routed externally).
func (m *Manager) apply(ctx context.Context, runID string, rule *arkwenv1.GateRule, res *Resolution, timedOut bool) (bool, error) {
	decision, terminate := Decide(rule, res, timedOut)

	if decision == arkwenv1.GateDecision_GATE_DECISION_ESCALATE {
		// stays pending; an external party (with a fresh max_wait) will resolve it.
		return false, nil
	}

	by := (*arkwenv1.Principal)(nil)
	rationale := ""
	var payloadRef *arkwenv1.ContentRef
	if res != nil {
		by, rationale, payloadRef = res.By, res.Rationale, res.PayloadRef
	}
	if timedOut && rationale == "" {
		rationale = "timed out under " + rule.GetTimeoutPolicy().String()
	}

	if err := m.emit(ctx, m.stamp(runID, arkwenv1.EventType_EVENT_TYPE_GATE_RESOLVED, by,
		&arkwenv1.EventEnvelope{Payload: &arkwenv1.EventEnvelope_GateResolved{GateResolved: &arkwenv1.GateResolved{
			GateId:               rule.GetGateId(),
			ResolvedBy:           by,
			Decision:             decision,
			Rationale:            rationale,
			ResolutionPayloadRef: payloadRef,
		}}})); err != nil {
		return false, err
	}

	if rule.GetScope() == arkwenv1.GateScope_GATE_SCOPE_PROMOTION && m.promotion != nil {
		if err := m.promotion.OnPromotionResolved(ctx, rule.GetGateId(), decision, by); err != nil {
			return false, err
		}
	}

	m.mu.Lock()
	delete(m.pending, key(runID, rule.GetGateId()))
	m.mu.Unlock()

	if terminate {
		return true, nil // the run ends FAILED reason GATE_REJECTED (the controller finalizes)
	}
	// resume from the pause the gate induced
	return false, m.emit(ctx, m.stamp(runID, arkwenv1.EventType_EVENT_TYPE_RUN_RESUMED, by,
		&arkwenv1.EventEnvelope{Payload: &arkwenv1.EventEnvelope_RunResumed{RunResumed: &arkwenv1.RunResumed{ResumedBy: by}}}))
}

// Pending reports whether a gate is still awaiting resolution.
func (m *Manager) Pending(runID, gateID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pending[key(runID, gateID)]
	return ok
}

// stamp fills the envelope invariants on a control event whose Type + Payload the
// caller has already set (the payload oneof interface is unexported, so the
// envelope must be constructed at the call site).
func (m *Manager) stamp(runID string, t arkwenv1.EventType, source *arkwenv1.Principal, ev *arkwenv1.EventEnvelope) *arkwenv1.EventEnvelope {
	ev.RunId = runID
	ev.SchemaVersion = 1
	ev.Type = t
	ev.Emitter = m.emitter
	ev.Timestamp = timestamppb.Now()
	if source != nil {
		ev.Source = source
	}
	return ev
}
