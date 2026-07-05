// Package secret is the control-plane Secret-Broker (ADR-006 E5) — a component
// SEPARATE from the Factory Controller: the Controller is never in the secret
// path (Invariant 1). The broker mints per-run ephemeral scoped credentials,
// injects them (env/tmpfs) at start, auto-registers them into the Cell-Shim
// redaction list, rotates mid-run, and REVOKES every lease at reap for ANY
// terminal state including crash. Events carry lease/scope metadata only — never
// the credential (Invariant 5).
//
// S0 ships the minimal path (one live model-API credential injected at start +
// registered for redaction). S2b generalizes the SAME interface to arbitrary
// broker-fed scoped secrets with rotation + revocation. A production backend
// (Vault / cloud KMS-STS) is an additive drop-in behind this interface.
package secret

import (
	"context"
	"fmt"
	"sync"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/ids"
)

// Scope is a request for a scoped secret. Purpose is audit metadata; Name is the
// env var the material is injected as (and the redaction rule id).
type Scope struct {
	Name    string // e.g. "MODEL_API_KEY" — becomes env name + redaction rule id
	Purpose string // audit-only description
}

// Lease is the non-secret record of a minted credential (safe to persist).
type Lease struct {
	ID        string
	RuleID    string // == Scope.Name; reported in redaction_applied
	EnvName   string // == Scope.Name; how the material is injected
	ScopeRef  *arkwenv1.Digest
	ExpiresAt time.Time
}

// Grant bundles the leases (metadata) with the actual material to inject. The
// material is handed to the shim, which injects it + registers it for redaction,
// then never lets it reach a persistent surface.
type Grant struct {
	Leases   []Lease
	Material map[string]string // env name -> secret value (transient)
}

// Broker is the Secret-Broker contract.
type Broker interface {
	Lease(ctx context.Context, runID, tenantID string, scopes []Scope) (Grant, error)
	Rotate(ctx context.Context, runID, leaseID string) (Lease, string, error)
	Revoke(ctx context.Context, runID string) ([]string, error)
}

// memBroker is the in-memory reference broker. It sources secret values from a
// per-scope resolver (default: a fixed store) so tests + the walking skeleton can
// inject a REAL secret that redaction must protect from S0.
type memBroker struct {
	mu       sync.Mutex
	ttl      time.Duration
	resolve  func(tenantID, name string) (string, bool)
	byRun    map[string][]Lease // runID -> active leases
	rotation int
}

// Option configures the broker.
type Option func(*memBroker)

// WithTTL sets the lease TTL.
func WithTTL(d time.Duration) Option { return func(b *memBroker) { b.ttl = d } }

// WithResolver sets the secret-value resolver (name -> value).
func WithResolver(fn func(tenantID, name string) (string, bool)) Option {
	return func(b *memBroker) { b.resolve = fn }
}

// NewMem returns an in-memory broker. Without a resolver it mints deterministic
// placeholder secrets ("brokered:<tenant>:<name>") so redaction always has a real
// value to protect.
func NewMem(opts ...Option) Broker {
	b := &memBroker{
		ttl:   time.Hour,
		byRun: map[string][]Lease{},
		resolve: func(tenant, name string) (string, bool) {
			return fmt.Sprintf("brokered-secret:%s:%s", tenant, name), true
		},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

func (b *memBroker) Lease(_ context.Context, runID, tenantID string, scopes []Scope) (Grant, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := Grant{Material: map[string]string{}}
	for _, s := range scopes {
		val, ok := b.resolve(tenantID, s.Name)
		if !ok || val == "" {
			continue // no such secret; skip (the run may not need it)
		}
		lease := Lease{
			ID:        ids.Short("lease"),
			RuleID:    s.Name,
			EnvName:   s.Name,
			ScopeRef:  ids.Sha256([]byte("scope\x00" + s.Name + "\x00" + s.Purpose)),
			ExpiresAt: time.Now().Add(b.ttl),
		}
		g.Leases = append(g.Leases, lease)
		g.Material[s.Name] = val
		b.byRun[runID] = append(b.byRun[runID], lease)
	}
	return g, nil
}

func (b *memBroker) Rotate(_ context.Context, runID, leaseID string) (Lease, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rotation++
	for i, l := range b.byRun[runID] {
		if l.ID == leaseID {
			l.ExpiresAt = time.Now().Add(b.ttl)
			b.byRun[runID][i] = l
			return l, fmt.Sprintf("rotated-secret:%s:%d", l.EnvName, b.rotation), nil
		}
	}
	return Lease{}, "", fmt.Errorf("secret: lease %q not found for run %q", leaseID, runID)
}

// Revoke revokes every lease for the run (called at reap for any terminal state,
// including crash). Returns the revoked lease ids.
func (b *memBroker) Revoke(_ context.Context, runID string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for _, l := range b.byRun[runID] {
		ids = append(ids, l.ID)
	}
	delete(b.byRun, runID)
	return ids, nil
}
