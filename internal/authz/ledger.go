package authz

import (
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Ledger is the Control-Plane Audit Ledger (ADR-010 E7 / ADR-009 R8-pattern): an
// OWN append-only truth domain, DISJOINT from the run stream. It is keyed by
// principal/time and carries its own monotone ledger_seq (NOT run_id/seq).
// AuthN/AuthZ decisions — especially DENIALS — live ONLY here and NEVER fabricate
// a run-stream event (Invariant 2). By construction (the arkwenv1 types) it holds
// no credential field (structural exclusion, Invariant 5).
type Ledger struct {
	mu      sync.Mutex
	entries []*arkwenv1.ControlPlaneAuditLedgerEnvelope
}

// NewLedger returns an empty audit ledger.
func NewLedger() *Ledger { return &Ledger{} }

// recordAuthDecision appends an AuthDecision entry and returns its ledger_seq.
func (l *Ledger) recordAuthDecision(principal *arkwenv1.Principal, perm arkwenv1.Permission, outcome arkwenv1.AuthzOutcome, runID string, policyVersion *arkwenv1.Digest, reason string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := uint64(len(l.entries))
	l.entries = append(l.entries, &arkwenv1.ControlPlaneAuditLedgerEnvelope{
		LedgerSeq:     seq,
		SchemaVersion: 1,
		Timestamp:     timestamppb.Now(),
		Principal:     nonSecretPrincipal(principal),
		Entry: &arkwenv1.ControlPlaneAuditLedgerEnvelope_AuthDecision{AuthDecision: &arkwenv1.AuthDecision{
			Permission:         perm,
			Outcome:            outcome,
			RunId:              runID, // scope ref only; empty for run-less calls
			AuthzPolicyVersion: policyVersion,
			Reason:             reason,
		}},
	})
	return seq
}

// RecordDenial appends a DENY AuthDecision (e.g. a floor-loosening enqueue that
// policy.Compose rejected). The denial lives ONLY here — never a run event.
func (l *Ledger) RecordDenial(principal *arkwenv1.Principal, perm arkwenv1.Permission, runID, reason string, policyVersion *arkwenv1.Digest) uint64 {
	return l.recordAuthDecision(principal, perm, arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY, runID, policyVersion, reason)
}

// RecordFloorChanged appends a FloorChanged entry (policy:set_floor footprint).
func (l *Ledger) RecordFloorChanged(principal *arkwenv1.Principal, newFloorRef, policyVersion *arkwenv1.Digest) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := uint64(len(l.entries))
	l.entries = append(l.entries, &arkwenv1.ControlPlaneAuditLedgerEnvelope{
		LedgerSeq:     seq,
		SchemaVersion: 1,
		Timestamp:     timestamppb.Now(),
		Principal:     nonSecretPrincipal(principal),
		Entry: &arkwenv1.ControlPlaneAuditLedgerEnvelope_FloorChanged{FloorChanged: &arkwenv1.FloorChanged{
			NewFloorRef:        newFloorRef,
			AuthzPolicyVersion: policyVersion,
		}},
	})
	return seq
}

// Read returns a copy of the ledger from fromSeq (inclusive), ordered.
func (l *Ledger) Read(fromSeq uint64) []*arkwenv1.ControlPlaneAuditLedgerEnvelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*arkwenv1.ControlPlaneAuditLedgerEnvelope
	for _, e := range l.entries {
		if e.GetLedgerSeq() >= fromSeq {
			out = append(out, proto.Clone(e).(*arkwenv1.ControlPlaneAuditLedgerEnvelope))
		}
	}
	return out
}

// Len returns the number of ledger entries.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// nonSecretPrincipal strips everything but the non-secret identity fields. This
// is belt-and-suspenders: the Principal type has no credential field anyway.
func nonSecretPrincipal(p *arkwenv1.Principal) *arkwenv1.Principal {
	if p == nil {
		return nil
	}
	return &arkwenv1.Principal{Type: p.GetType(), PrincipalId: p.GetPrincipalId(), TenantId: p.GetTenantId()}
}
