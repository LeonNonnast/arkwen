package warehouse

import (
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Ledger is the Warehouse provenance ledger (ADR-007 E5 / ADR-009 R8): its OWN
// append-only truth domain, DISJOINT from the run stream. Warehouse mutations
// (intake requests/resolutions, channel moves) are append-only and keyed on a
// monotone ledger_seq — NOT run_id/seq. channel->digest is a projection over this
// ledger (Invariant 2), never mutable shadow state.
type Ledger struct {
	mu      sync.Mutex
	entries []*arkwenv1.WarehouseLedgerEnvelope
}

// NewLedger returns an empty warehouse ledger.
func NewLedger() *Ledger { return &Ledger{} }

func (l *Ledger) append(by *arkwenv1.Principal, entry func(*arkwenv1.WarehouseLedgerEnvelope)) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := uint64(len(l.entries))
	env := &arkwenv1.WarehouseLedgerEnvelope{
		LedgerSeq:     seq,
		SchemaVersion: 1,
		Timestamp:     timestamppb.Now(),
		Source:        by,
	}
	entry(env)
	l.entries = append(l.entries, env)
	return seq
}

// RecordIntakeRequested records a promotion intake request (scope=promotion gate).
func (l *Ledger) RecordIntakeRequested(by *arkwenv1.Principal, rule *arkwenv1.GateRule, subject *arkwenv1.Digest, channel string) uint64 {
	return l.append(by, func(e *arkwenv1.WarehouseLedgerEnvelope) {
		e.Entry = &arkwenv1.WarehouseLedgerEnvelope_IntakeRequested{IntakeRequested: &arkwenv1.WarehouseIntakeRequested{
			Gate: rule, Subject: subject, Channel: channel,
		}}
	})
}

// RecordIntakeResolved records the promotion decision.
func (l *Ledger) RecordIntakeResolved(by *arkwenv1.Principal, gateID string, decision arkwenv1.GateDecision, subject *arkwenv1.Digest, rationale string) uint64 {
	return l.append(by, func(e *arkwenv1.WarehouseLedgerEnvelope) {
		e.Entry = &arkwenv1.WarehouseLedgerEnvelope_IntakeResolved{IntakeResolved: &arkwenv1.WarehouseIntakeResolved{
			GateId: gateID, ResolvedBy: by, Decision: decision, Subject: subject, Rationale: rationale,
		}}
	})
}

// RecordChannelMove records a movable-channel repoint.
func (l *Ledger) RecordChannelMove(by *arkwenv1.Principal, channel string, from, to *arkwenv1.Digest) uint64 {
	return l.append(by, func(e *arkwenv1.WarehouseLedgerEnvelope) {
		e.Entry = &arkwenv1.WarehouseLedgerEnvelope_ChannelPointerMoved{ChannelPointerMoved: &arkwenv1.ChannelPointerMoved{
			Channel: channel, FromDigest: from, ToDigest: to,
		}}
	})
}

// Read returns ledger entries from fromSeq (inclusive).
func (l *Ledger) Read(fromSeq uint64) []*arkwenv1.WarehouseLedgerEnvelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*arkwenv1.WarehouseLedgerEnvelope
	for _, e := range l.entries {
		if e.GetLedgerSeq() >= fromSeq {
			out = append(out, proto.Clone(e).(*arkwenv1.WarehouseLedgerEnvelope))
		}
	}
	return out
}

// Head returns the next ledger_seq (== number of entries).
func (l *Ledger) Head() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return uint64(len(l.entries))
}
