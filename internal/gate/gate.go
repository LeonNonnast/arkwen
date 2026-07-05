// Package gate is the Quality-Gate spine (ADR-005). It is ONE mechanism with a
// swappable resolver, realized purely as control-events on the existing
// append-only stream (no new state machine): gate.requested -> [run.paused] ->
// gate.resolved -> [run.resumed]. The Controller stays runtime-agnostic — it only
// relays these generic events (Invariant 1).
//
// Fail-closed is the law (Invariant 7): timeout_policy defaults to FAIL_CLOSED
// (enum value 0), an unresolved mandatory gate never silently passes, and an
// UNSPECIFIED decision is read as REJECT. The intrinsic Arkwen floor (redaction,
// fail_closed default, seed-capture) sits BELOW the org floor and is not
// disable-able even by the governance owner — a violating RunSpec is rejected at
// enqueue by policy.Compose (policy.ErrLoosensFloor), never by this package.
package gate

import (
	"errors"
	"strings"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// ErrInvalidRule marks a gate rule that cannot be materialized fail-closed.
var ErrInvalidRule = errors.New("gate: invalid rule")

// Resolution is a decision submitted for a pending gate (from an Auto evaluator,
// a Human via the Control Room, or a timeout-synthesized reject).
type Resolution struct {
	By         *arkwenv1.Principal
	Decision   arkwenv1.GateDecision
	Rationale  string
	PayloadRef *arkwenv1.ContentRef // approve_with_modification diff / injected guidance, pointer-only
}

// Resolver is the swappable gate resolver (ADR-005 spine). Auto resolvers decide
// synchronously; Human resolvers stay pending until an external Resolve call.
type Resolver interface {
	Kind() arkwenv1.ResolverKind
	// Resolve returns (resolution, true) if it can decide now, or (nil, false) if
	// the gate must remain pending (Human / escalate to an external party).
	Resolve(rule *arkwenv1.GateRule) (*Resolution, bool)
}

// AutoResolver decides synchronously via a check function (evaluator / check-fn /
// LLM-judge). A nil check fails closed (REJECT).
type AutoResolver struct {
	Check func(rule *arkwenv1.GateRule) (arkwenv1.GateDecision, string)
}

// Kind reports RESOLVER_KIND_AUTO.
func (a AutoResolver) Kind() arkwenv1.ResolverKind { return arkwenv1.ResolverKind_RESOLVER_KIND_AUTO }

// Resolve runs the check function (fail-closed if absent).
func (a AutoResolver) Resolve(rule *arkwenv1.GateRule) (*Resolution, bool) {
	if a.Check == nil {
		return &Resolution{Decision: arkwenv1.GateDecision_GATE_DECISION_REJECT, Rationale: "no evaluator (fail-closed)"}, true
	}
	dec, why := a.Check(rule)
	return &Resolution{Decision: dec, Rationale: why}, true
}

// HumanResolver always stays pending — it travels to the Control Room and is
// resolved via an external ResolveGate call (or times out fail-closed).
type HumanResolver struct{}

// Kind reports RESOLVER_KIND_HUMAN.
func (HumanResolver) Kind() arkwenv1.ResolverKind { return arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN }

// Resolve never decides synchronously (pending until external resolution).
func (HumanResolver) Resolve(*arkwenv1.GateRule) (*Resolution, bool) { return nil, false }

// IsExternal reports whether a rule's resolution travels outside Arkwen (a Human
// resolver routed to the Control Room / an external escalation). Such resolvers
// MUST carry max_wait and inherit fail_closed (ADR-008 E3).
func IsExternal(rule *arkwenv1.GateRule) bool {
	return rule.GetResolver() == arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN
}

// ValidateRule enforces fail-closed materialization constraints:
//   - the resolver must be specified (UNSPECIFIED => reject);
//   - an external resolver (Human/escalate) MUST carry max_wait, so a stalled
//     external party can never hang the run unbounded (timeout -> reject).
func ValidateRule(rule *arkwenv1.GateRule) error {
	if rule == nil {
		return ErrInvalidRule
	}
	if rule.GetResolver() == arkwenv1.ResolverKind_RESOLVER_KIND_UNSPECIFIED {
		return errors.New("gate: resolver UNSPECIFIED (fail-closed reject)")
	}
	if IsExternal(rule) && rule.GetMaxWait().AsDuration() <= 0 {
		return errors.New("gate: external resolver requires max_wait (ADR-008 E3)")
	}
	return nil
}

// Decide is the PURE fail-closed decision function. Given a rule and either a
// submitted resolution or a timeout, it returns the effective decision and
// whether the run must terminate (a mandatory gate that ends in REJECT).
//
//   - timeout + FAIL_CLOSED + mandatory  -> (REJECT, terminate)
//   - timeout + FAIL_CLOSED + !mandatory -> (REJECT, no-terminate)
//   - timeout + FAIL_OPEN                -> (APPROVE, no-terminate)  [explicit opt-in]
//   - resolution UNSPECIFIED             -> read as REJECT
//   - resolution REJECT + mandatory      -> (REJECT, terminate)
//   - resolution APPROVE[/w-mod]         -> (that, no-terminate)
//   - resolution ESCALATE                -> (ESCALATE, no-terminate) [re-pended by the Manager]
func Decide(rule *arkwenv1.GateRule, res *Resolution, timedOut bool) (arkwenv1.GateDecision, bool) {
	if timedOut {
		if rule.GetTimeoutPolicy() == arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_OPEN {
			return arkwenv1.GateDecision_GATE_DECISION_APPROVE, false
		}
		// default FAIL_CLOSED (enum value 0): never silently pass
		return arkwenv1.GateDecision_GATE_DECISION_REJECT, rule.GetMandatory()
	}
	dec := arkwenv1.GateDecision_GATE_DECISION_REJECT
	if res != nil {
		dec = res.Decision
	}
	if dec == arkwenv1.GateDecision_GATE_DECISION_UNSPECIFIED {
		dec = arkwenv1.GateDecision_GATE_DECISION_REJECT // fail-closed (F2)
	}
	switch dec {
	case arkwenv1.GateDecision_GATE_DECISION_APPROVE,
		arkwenv1.GateDecision_GATE_DECISION_APPROVE_WITH_MODIFICATION:
		return dec, false
	case arkwenv1.GateDecision_GATE_DECISION_ESCALATE:
		return dec, false
	default: // REJECT
		return arkwenv1.GateDecision_GATE_DECISION_REJECT, rule.GetMandatory()
	}
}

// SuspensionReasonFor maps a gate rule to the suspension overlay its pause induces
// (ADR-003 mapping). Priority: a budget/resource gate is a resource-limit pause;
// a Human gate is a user milestone; otherwise an auto gate is a gate pause. A
// policy/org-sourced pause can be requested explicitly via Manager.Request.
func SuspensionReasonFor(rule *arkwenv1.GateRule) arkwenv1.SuspensionReason {
	if rule.GetResourceLimitKind() != arkwenv1.ResourceLimitKind_RESOURCE_LIMIT_KIND_UNSPECIFIED {
		return arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_RESOURCE_LIMIT
	}
	if rule.GetResolver() == arkwenv1.ResolverKind_RESOLVER_KIND_HUMAN {
		return arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_USER
	}
	return arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_GATE
}

// Matches reports whether an ACTION-scope rule's applies_to matches a runtime
// event descriptor (tool name / permission). Supports exact match, "*" wildcard,
// and a trailing-"*" prefix glob (e.g. "fs.*"). Non-action scopes never match at
// runtime (they are milestone/promotion gates, not per-action).
func Matches(rule *arkwenv1.GateRule, descriptor string) bool {
	if rule.GetScope() != arkwenv1.GateScope_GATE_SCOPE_ACTION {
		return false
	}
	pat := rule.GetAppliesTo()
	switch {
	case pat == "":
		return false
	case pat == "*":
		return true
	case strings.HasSuffix(pat, "*"):
		return strings.HasPrefix(descriptor, strings.TrimSuffix(pat, "*"))
	default:
		return pat == descriptor
	}
}
