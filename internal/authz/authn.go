// Package authz is the Contract-Plane AuthZ & Multi-Tenancy layer (ADR-010).
//
// It sits ABOVE the intrinsic Arkwen floor and is AND-composed with it: a fully
// authorized principal whose RunSpec would loosen the floor is STILL rejected at
// enqueue (that rejection is internal/policy.Compose's job — authz only decides
// whether the caller may perform an op at all). Everything here is FAIL-CLOSED
// (Invariant 7): the default is DENY; a missing/ambiguous grant, an intra-trust
// or unspecified principal, or a degraded AuthN backend never opens access.
//
// Secret-tightness here is STRUCTURAL EXCLUSION at the AuthN boundary (ADR-010 E7,
// Invariant 5), a mechanism SEPARATE from Cell-Shim redaction: AuthN material is
// verified-and-immediately-discarded; only the non-secret Principal (type +
// principal_id + tenant) survives, and neither Principal nor the audit ledger has
// any field that could hold a credential.
package authz

import (
	"errors"
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// ErrUnauthenticated is returned when a credential cannot be verified. A missing
// or degraded AuthN backend NEVER opens access (fail-closed, Invariant 7).
var ErrUnauthenticated = errors.New("authz: unauthenticated (fail-closed)")

// Authenticator verifies external credentials (mTLS / OIDC / signed token) and
// returns the authenticated Principal. The credential is verified-and-DISCARDED:
// it is never stored and never travels onward — the returned Principal carries
// only non-secret identity (structural exclusion, ADR-010 E7).
type Authenticator interface {
	Authenticate(credential string) (*arkwenv1.Principal, error)
}

// tokenAuthenticator is a reference signed-token backend. It maps opaque bearer
// tokens to principals; the token itself is compared and then dropped — it is
// never copied into the returned Principal (which has no field for it).
type tokenAuthenticator struct {
	mu     sync.RWMutex
	tokens map[string]*arkwenv1.Principal // token -> principal (verification table only)
}

// NewTokenAuthenticator builds a token-based Authenticator.
func NewTokenAuthenticator() *tokenAuthenticator {
	return &tokenAuthenticator{tokens: map[string]*arkwenv1.Principal{}}
}

// Bind registers a token for a principal (test/bootstrap helper). Only external
// principal types are accepted; intra-trust components are never AuthZ subjects.
func (t *tokenAuthenticator) Bind(token string, p *arkwenv1.Principal) {
	if !IsExternal(p) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tokens[token] = p
}

// Authenticate verifies the bearer token and returns ONLY the non-secret
// Principal. The token is never embedded in the result (structural exclusion).
func (t *tokenAuthenticator) Authenticate(credential string) (*arkwenv1.Principal, error) {
	t.mu.RLock()
	p, ok := t.tokens[credential]
	t.mu.RUnlock()
	if !ok {
		return nil, ErrUnauthenticated
	}
	// Return a fresh copy carrying only id/type/tenant — the credential is dropped.
	return &arkwenv1.Principal{Type: p.GetType(), PrincipalId: p.GetPrincipalId(), TenantId: p.GetTenantId()}, nil
}

// IsExternal reports whether p is an external AuthZ subject. Intra-trust
// components (Cell-Shim, Secret-Broker) and the UNSPECIFIED type are never
// external subjects (fail-closed).
func IsExternal(p *arkwenv1.Principal) bool {
	switch p.GetType() {
	case arkwenv1.PrincipalType_PRINCIPAL_TYPE_OUTER_LOOP_CONSUMER,
		arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR,
		arkwenv1.PrincipalType_PRINCIPAL_TYPE_AUTOMATION:
		return true
	}
	return false
}
