package gate

import (
	"context"
	"sync"
	"testing"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

func ruleRunGate(id string, mandatory bool, tp arkwenv1.TimeoutPolicy, resolver arkwenv1.ResolverKind) *arkwenv1.GateRule {
	return &arkwenv1.GateRule{
		GateId:        id,
		Scope:         arkwenv1.GateScope_GATE_SCOPE_RUN,
		Resolver:      resolver,
		TimeoutPolicy: tp,
		Mandatory:     mandatory,
		MaxWait:       durationpb.New(time.Hour),
		Source:        arkwenv1.PolicyLayerKind_POLICY_LAYER_KIND_ORG,
	}
}

// F1: an unresolved mandatory gate under FAIL_CLOSED terminates FAILED (reject).
func TestDecide_TimeoutFailClosedMandatory(t *testing.T) {
	r := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	dec, terminate := Decide(r, nil, true)
	if dec != arkwenv1.GateDecision_GATE_DECISION_REJECT || !terminate {
		t.Fatalf("want REJECT+terminate, got %v terminate=%v", dec, terminate)
	}
}

// F2: GATE_DECISION_UNSPECIFIED (value 0) is read as REJECT.
func TestDecide_UnspecifiedIsReject(t *testing.T) {
	r := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	dec, terminate := Decide(r, &Resolution{Decision: arkwenv1.GateDecision_GATE_DECISION_UNSPECIFIED}, false)
	if dec != arkwenv1.GateDecision_GATE_DECISION_REJECT || !terminate {
		t.Fatalf("UNSPECIFIED must read as REJECT+terminate(mandatory), got %v terminate=%v", dec, terminate)
	}
}

func TestDecide_TimeoutFailOpenApproves(t *testing.T) {
	r := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_OPEN, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	dec, terminate := Decide(r, nil, true)
	if dec != arkwenv1.GateDecision_GATE_DECISION_APPROVE || terminate {
		t.Fatalf("fail_open timeout must APPROVE without terminate, got %v terminate=%v", dec, terminate)
	}
}

func TestDecide_ApproveDoesNotTerminate(t *testing.T) {
	r := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	dec, terminate := Decide(r, &Resolution{Decision: arkwenv1.GateDecision_GATE_DECISION_APPROVE}, false)
	if dec != arkwenv1.GateDecision_GATE_DECISION_APPROVE || terminate {
		t.Fatalf("approve must not terminate, got %v terminate=%v", dec, terminate)
	}
}

func TestValidateRule(t *testing.T) {
	// external (Human) resolver without max_wait -> reject
	bad := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	bad.MaxWait = nil
	if err := ValidateRule(bad); err == nil {
		t.Fatal("external resolver without max_wait must be rejected (ADR-008 E3)")
	}
	// UNSPECIFIED resolver -> reject
	un := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_UNSPECIFIED)
	if err := ValidateRule(un); err == nil {
		t.Fatal("UNSPECIFIED resolver must be rejected")
	}
	// well-formed -> ok
	if err := ValidateRule(ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
}

func TestSuspensionReasonFor(t *testing.T) {
	auto := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_AUTO)
	if got := SuspensionReasonFor(auto); got != arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_GATE {
		t.Fatalf("auto -> %v", got)
	}
	human := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	if got := SuspensionReasonFor(human); got != arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_USER {
		t.Fatalf("human -> %v", got)
	}
	budget := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_AUTO)
	budget.ResourceLimitKind = arkwenv1.ResourceLimitKind_RESOURCE_LIMIT_KIND_COST_BUDGET_EXHAUSTED
	if got := SuspensionReasonFor(budget); got != arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_RESOURCE_LIMIT {
		t.Fatalf("budget -> %v", got)
	}
}

func TestMatches(t *testing.T) {
	action := func(applies string) *arkwenv1.GateRule {
		return &arkwenv1.GateRule{GateId: "a", Scope: arkwenv1.GateScope_GATE_SCOPE_ACTION, AppliesTo: applies}
	}
	if !Matches(action("fs.write"), "fs.write") {
		t.Fatal("exact match failed")
	}
	if Matches(action("fs.write"), "fs.read") {
		t.Fatal("non-match matched")
	}
	if !Matches(action("*"), "anything") {
		t.Fatal("wildcard failed")
	}
	if !Matches(action("fs.*"), "fs.delete") {
		t.Fatal("prefix glob failed")
	}
	// RUN scope never matches at runtime
	run := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	if Matches(run, "deploy") {
		t.Fatal("RUN-scope rule must not match action descriptors")
	}
}

// collector is a fake Emit that records events in order.
type collector struct {
	mu sync.Mutex
	ev []*arkwenv1.EventEnvelope
}

func (c *collector) emit(_ context.Context, e *arkwenv1.EventEnvelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ev = append(c.ev, e)
	return nil
}

func (c *collector) types() []arkwenv1.EventType {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]arkwenv1.EventType, len(c.ev))
	for i, e := range c.ev {
		out[i] = e.GetType()
	}
	return out
}

func TestManager_HumanResolveApprove(t *testing.T) {
	c := &collector{}
	m := NewManager(c.emit)
	rule := ruleRunGate("g-deploy", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	term, err := m.Request(context.Background(), "run-1", rule, nil)
	if err != nil || term {
		t.Fatalf("request: term=%v err=%v", term, err)
	}
	if !m.Pending("run-1", "g-deploy") {
		t.Fatal("human gate should be pending")
	}
	by := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "op", TenantId: "acme"}
	term, err = m.Resolve(context.Background(), "run-1", "g-deploy", &Resolution{By: by, Decision: arkwenv1.GateDecision_GATE_DECISION_APPROVE, Rationale: "ok"})
	if err != nil || term {
		t.Fatalf("resolve: term=%v err=%v", term, err)
	}
	want := []arkwenv1.EventType{
		arkwenv1.EventType_EVENT_TYPE_GATE_REQUESTED,
		arkwenv1.EventType_EVENT_TYPE_RUN_PAUSED,
		arkwenv1.EventType_EVENT_TYPE_GATE_RESOLVED,
		arkwenv1.EventType_EVENT_TYPE_RUN_RESUMED,
	}
	got := c.types()
	if len(got) != len(want) {
		t.Fatalf("event count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: got %v want %v", i, got[i], want[i])
		}
	}
	if m.Pending("run-1", "g-deploy") {
		t.Fatal("gate should be cleared after resolution")
	}
}

func TestManager_AutoRejectMandatoryTerminates(t *testing.T) {
	c := &collector{}
	auto := AutoResolver{Check: func(*arkwenv1.GateRule) (arkwenv1.GateDecision, string) {
		return arkwenv1.GateDecision_GATE_DECISION_REJECT, "policy violation"
	}}
	m := NewManager(c.emit, WithResolver(auto))
	rule := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_AUTO)
	term, err := m.Request(context.Background(), "run-2", rule, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !term {
		t.Fatal("mandatory auto-reject must terminate the run")
	}
	// no run.resumed after a terminating reject
	for _, ty := range c.types() {
		if ty == arkwenv1.EventType_EVENT_TYPE_RUN_RESUMED {
			t.Fatal("terminating reject must NOT emit run.resumed")
		}
	}
}

type fakePromotion struct {
	called bool
	dec    arkwenv1.GateDecision
}

func (f *fakePromotion) OnPromotionResolved(_ context.Context, _ string, d arkwenv1.GateDecision, _ *arkwenv1.Principal) error {
	f.called = true
	f.dec = d
	return nil
}

func TestManager_PromotionHook(t *testing.T) {
	c := &collector{}
	hook := &fakePromotion{}
	auto := AutoResolver{Check: func(*arkwenv1.GateRule) (arkwenv1.GateDecision, string) {
		return arkwenv1.GateDecision_GATE_DECISION_APPROVE, "promote"
	}}
	m := NewManager(c.emit, WithResolver(auto), WithPromotionHook(hook))
	rule := &arkwenv1.GateRule{
		GateId: "promote-tested", Scope: arkwenv1.GateScope_GATE_SCOPE_PROMOTION,
		Resolver: arkwenv1.ResolverKind_RESOLVER_KIND_AUTO, TimeoutPolicy: arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED,
	}
	if _, err := m.Request(context.Background(), "warehouse", rule, nil); err != nil {
		t.Fatal(err)
	}
	if !hook.called || hook.dec != arkwenv1.GateDecision_GATE_DECISION_APPROVE {
		t.Fatalf("promotion hook not invoked correctly: %+v", hook)
	}
}

func TestManager_TimeoutRejectsFailClosed(t *testing.T) {
	c := &collector{}
	m := NewManager(c.emit)
	rule := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	if _, err := m.Request(context.Background(), "run-3", rule, nil); err != nil {
		t.Fatal(err)
	}
	term, err := m.Timeout(context.Background(), "run-3", "g")
	if err != nil || !term {
		t.Fatalf("timeout under fail_closed must terminate: term=%v err=%v", term, err)
	}
	// last resolved event carries REJECT
	var lastResolved *arkwenv1.GateResolved
	for _, e := range c.ev {
		if r, ok := e.GetPayload().(*arkwenv1.EventEnvelope_GateResolved); ok {
			lastResolved = r.GateResolved
		}
	}
	if lastResolved == nil || lastResolved.GetDecision() != arkwenv1.GateDecision_GATE_DECISION_REJECT {
		t.Fatalf("timeout must emit gate.resolved REJECT, got %+v", lastResolved)
	}
}

func TestManager_SweepExpired(t *testing.T) {
	c := &collector{}
	m := NewManager(c.emit)
	rule := ruleRunGate("g", true, arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED, arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN)
	rule.MaxWait = durationpb.New(time.Millisecond)
	if _, err := m.Request(context.Background(), "run-4", rule, nil); err != nil {
		t.Fatal(err)
	}
	terminated, err := m.SweepExpired(context.Background(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(terminated) != 1 || terminated[0] != "g" {
		t.Fatalf("expected g to be swept+terminated, got %v", terminated)
	}
}
