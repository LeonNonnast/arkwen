package authz

import (
	"fmt"
	"sort"
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/ids"
)

// SelectorKind scopes a grant to a subset of runs (ADR-010 E4). A subscriber can
// hold runs:read scoped to a tenant without any command grant (pure observer).
type SelectorKind int

const (
	SelectorTenant    SelectorKind = iota // all runs in the grant's tenant
	SelectorBlueprint                     // runs of a specific blueprint digest
	SelectorOwnRuns                       // runs the principal created
	SelectorLabel                         // runs carrying a label "k=v"
)

// Selector is a run-scoping predicate attached to a grant.
type Selector struct {
	Kind  SelectorKind
	Value string // tenant id, blueprint hex, or "k=v" label; empty for OwnRuns
}

// Grant is a least-privilege authorization: principal P may exercise Permission
// within Tenant over the runs the Selector admits. read != write; high-trust
// grants (gates:resolve, policy:set_floor) are separate rows, never bundled.
type Grant struct {
	PrincipalID string
	Tenant      string
	Permission  arkwenv1.Permission
	Selector    Selector
	CrossTenant bool // explicit cross-tenant authorization (default: false => deny)
}

// RunAttrs describes the target run for scope evaluation (all optional).
type RunAttrs struct {
	RunID        string
	Tenant       string
	BlueprintHex string
	OwnerID      string
	Labels       map[string]string
}

// Engine is the fail-closed AuthZ decision point + audit ledger. Default-deny:
// no matching grant, an ambiguous match, an intra-trust/unspecified principal, or
// an unauthenticated caller all resolve to DENY (Invariant 7).
type Engine struct {
	mu     sync.RWMutex
	grants []Grant
	ledger *Ledger
}

// New builds an Engine writing decisions to ledger (a fresh one if nil).
func New(ledger *Ledger) *Engine {
	if ledger == nil {
		ledger = NewLedger()
	}
	return &Engine{ledger: ledger}
}

// Ledger returns the audit ledger.
func (e *Engine) Ledger() *Ledger { return e.ledger }

// AddGrant registers a grant.
func (e *Engine) AddGrant(g Grant) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.grants = append(e.grants, g)
}

// Authorize decides whether principal may exercise perm against a run in
// runTenant (identified by attrs). It ALWAYS records the decision to the audit
// ledger (denials live only there — never a run-stream event). Fail-closed.
func (e *Engine) Authorize(principal *arkwenv1.Principal, perm arkwenv1.Permission, runTenant string, attrs RunAttrs) arkwenv1.AuthzOutcome {
	outcome, reason := e.decide(principal, perm, runTenant, attrs)
	e.ledger.recordAuthDecision(principal, perm, outcome, attrs.RunID, e.PolicyVersion(callerTenant(principal)), reason)
	return outcome
}

// Allowed is a convenience returning true iff the outcome is ALLOW.
func (e *Engine) Allowed(principal *arkwenv1.Principal, perm arkwenv1.Permission, runTenant string, attrs RunAttrs) bool {
	return e.Authorize(principal, perm, runTenant, attrs) == arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_ALLOW
}

func (e *Engine) decide(principal *arkwenv1.Principal, perm arkwenv1.Permission, runTenant string, attrs RunAttrs) (arkwenv1.AuthzOutcome, string) {
	if principal == nil {
		return deny("no principal (fail-closed)")
	}
	if !IsExternal(principal) {
		return deny("principal is not an external AuthZ subject")
	}
	if perm == arkwenv1.Permission_PERMISSION_UNSPECIFIED {
		return deny("permission unspecified (fail-closed)")
	}
	crossTenant := runTenant != "" && principal.GetTenantId() != runTenant

	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, g := range e.grants {
		if g.PrincipalID != principal.GetPrincipalId() {
			continue
		}
		if g.Permission != perm {
			continue // least-privilege: exact permission match, never bundled
		}
		if crossTenant && !g.CrossTenant {
			continue // cross-tenant is default-deny unless explicitly granted
		}
		if !grantTenantMatches(g, principal, runTenant) {
			continue
		}
		if !selectorAdmits(g.Selector, principal, attrs) {
			continue
		}
		return arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_ALLOW, "grant matched"
	}
	if crossTenant {
		return deny(fmt.Sprintf("cross-tenant default-deny (caller tenant %s != run tenant %s)", principal.GetTenantId(), runTenant))
	}
	return deny("no matching grant (default-deny)")
}

func deny(reason string) (arkwenv1.AuthzOutcome, string) {
	return arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY, reason
}

func grantTenantMatches(g Grant, principal *arkwenv1.Principal, runTenant string) bool {
	if g.Tenant == "*" {
		return true
	}
	if runTenant != "" {
		return g.Tenant == runTenant
	}
	// run-less op (e.g. enqueue/set_floor): scope to the caller's tenant
	return g.Tenant == principal.GetTenantId()
}

func selectorAdmits(s Selector, principal *arkwenv1.Principal, attrs RunAttrs) bool {
	switch s.Kind {
	case SelectorTenant:
		return true // tenant already checked by grantTenantMatches
	case SelectorBlueprint:
		return attrs.BlueprintHex != "" && attrs.BlueprintHex == s.Value
	case SelectorOwnRuns:
		return attrs.OwnerID != "" && attrs.OwnerID == principal.GetPrincipalId()
	case SelectorLabel:
		if attrs.Labels == nil {
			return false
		}
		for k, v := range attrs.Labels {
			if k+"="+v == s.Value {
				return true
			}
		}
		return false
	}
	return false
}

func callerTenant(p *arkwenv1.Principal) string {
	if p == nil {
		return ""
	}
	return p.GetTenantId()
}

// CoResidencyFloor is the AuthZ->isolation bridge (ADR-006 / ADR-010): cross-tenant
// co-residency forces isolation_profile >= STRICT. Same-tenant keeps the STANDARD
// floor. This is a SEPARATE mechanism — outer AuthZ informing inner isolation.
func CoResidencyFloor(callerTenant, runTenant string) arkwenv1.IsolationProfile {
	if callerTenant != runTenant {
		return arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT
	}
	return arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD
}

// PolicyVersion is the content-addressed digest of the materialized grant-set for
// a tenant (ADR-010 E8) — its OWN field, never a policy_version overload.
// Deterministic across calls for an unchanged grant-set.
func (e *Engine) PolicyVersion(tenant string) *arkwenv1.Digest {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var rows []string
	for _, g := range e.grants {
		if tenant != "" && g.Tenant != tenant && g.Tenant != "*" {
			continue
		}
		rows = append(rows, fmt.Sprintf("%s|%s|%d|%d|%s|%t",
			g.PrincipalID, g.Tenant, g.Permission, g.Selector.Kind, g.Selector.Value, g.CrossTenant))
	}
	sort.Strings(rows)
	blob := "authz-policy\x00" + tenant + "\x00"
	for _, r := range rows {
		blob += r + "\n"
	}
	return ids.Sha256([]byte(blob))
}
