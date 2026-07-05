// Package redaction is the Cell-Shim redaction seam (ADR-004 / ADR-006 E5).
//
// Invariant 5 (secrets never persisted) + Invariant 3 (worker.raw never dropped
// except redacted secrets): raw worker output is transient in shim memory only;
// it is structurally redacted BEFORE it is ever handed to the CAS or the event
// stream. Every non-secret byte is preserved; each secret span is replaced by a
// stable token "<redacted:RULE_ID>". The redaction list starts (S0) with the
// worker's single live model-API credential and is generalized (S2b) to arbitrary
// broker-fed scoped secrets via Register.
package redaction

import (
	"sort"
	"strings"
	"sync"
)

// Rule is a single registered secret literal + the rule id reported in
// security.redaction_applied (the id is safe to persist; the secret never is).
type Rule struct {
	ID     string
	Secret string
}

// Result is the accounting emitted alongside sanitized output. It carries counts
// and channel/rule ids only — never the matched values (Invariant 5).
type Result struct {
	Sanitized   []byte
	Total       int            // total redactions applied across all rules
	PerRule     map[string]int // rule_id -> count
	LeakChannel string         // set by the caller (stdout|stderr|file-event)
}

// Engine holds the active redaction list. Safe for concurrent use.
type Engine struct {
	mu    sync.RWMutex
	rules map[string]string // ruleID -> secret
}

// New returns an empty redaction engine.
func New() *Engine { return &Engine{rules: map[string]string{}} }

// Register adds (or updates) a secret under ruleID. Empty secrets are ignored
// (an empty match would redact the whole stream). Idempotent.
func (e *Engine) Register(ruleID, secret string) {
	if secret == "" || ruleID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules[ruleID] = secret
}

// Unregister removes a rule (e.g. on lease revocation).
func (e *Engine) Unregister(ruleID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.rules, ruleID)
}

// RuleIDs returns the currently registered rule ids (sorted), for diagnostics.
func (e *Engine) RuleIDs() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.rules))
	for id := range e.rules {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// token is the replacement written in place of a secret span.
func token(ruleID string) string { return "<redacted:" + ruleID + ">" }

// Redact returns sanitized bytes with every registered secret span replaced by
// its token, plus per-rule accounting. Longer secrets are applied first so a
// secret that is a substring of another does not corrupt the longer redaction.
func (e *Engine) Redact(data []byte) Result {
	e.mu.RLock()
	rules := make([]Rule, 0, len(e.rules))
	for id, sec := range e.rules {
		rules = append(rules, Rule{ID: id, Secret: sec})
	}
	e.mu.RUnlock()

	// Deterministic order: longest secret first, ties broken by rule id.
	sort.Slice(rules, func(i, j int) bool {
		if len(rules[i].Secret) != len(rules[j].Secret) {
			return len(rules[i].Secret) > len(rules[j].Secret)
		}
		return rules[i].ID < rules[j].ID
	})

	s := string(data)
	per := map[string]int{}
	total := 0
	for _, r := range rules {
		n := strings.Count(s, r.Secret)
		if n == 0 {
			continue
		}
		s = strings.ReplaceAll(s, r.Secret, token(r.ID))
		per[r.ID] += n
		total += n
	}
	return Result{Sanitized: []byte(s), Total: total, PerRule: per}
}

// Contains reports whether data contains any registered secret (used by leak
// detection to emit security.secret_leak_detected before redaction).
func (e *Engine) Contains(data []byte) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := string(data)
	for _, sec := range e.rules {
		if strings.Contains(s, sec) {
			return true
		}
	}
	return false
}
