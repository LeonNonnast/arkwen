package eventlog

import (
	"context"
	"fmt"
	"sort"
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Log is the append-only event store. It is the sole truth (Invariant 2): all
// run state is derived by folding Read(...) output; the log never exposes an
// update/delete operation.
type Log interface {
	// Create atomically appends ev as seq 0 of a NEW run. ev.Type must be
	// EVENT_TYPE_RUN_CREATED. Returns ErrRunExists if the run already has events
	// (a duplicate enqueue is idempotent — the caller re-reads the existing run).
	Create(ctx context.Context, ev *arkwenv1.EventEnvelope) (*arkwenv1.EventEnvelope, error)
	// Append appends ev to its run at the next monotone seq (server-assigned).
	// The incoming seq field is ignored; the store assigns it under
	// UNIQUE(run_id, seq). Validates the envelope (fail-closed).
	Append(ctx context.Context, ev *arkwenv1.EventEnvelope) (*arkwenv1.EventEnvelope, error)
	// Read returns the run's events from fromSeq (inclusive), ordered by seq.
	Read(ctx context.Context, runID string, fromSeq uint64) ([]*arkwenv1.EventEnvelope, error)
	// Subscribe replays from fromSeq then streams live appends. At-least-once +
	// seq-idempotent + no producer backpressure (a slow reader never blocks the
	// producer — it simply lags and catches up on the next read).
	Subscribe(ctx context.Context, runID string, fromSeq uint64) <-chan *arkwenv1.EventEnvelope
	// Head returns the highest seq present for the run and whether it exists.
	Head(ctx context.Context, runID string) (uint64, bool)
	// Runs lists known run ids, sorted.
	Runs(ctx context.Context) []string
}

// memLog is the in-memory reference implementation (default for the walking
// skeleton + conformance). A PostgreSQL-backed Log (append-only table, identity
// seq, UNIQUE(run_id, seq), LISTEN/NOTIFY fan-out) is an additive drop-in behind
// this same interface — see docs; the contract is what insulates the swap.
type memLog struct {
	mu      sync.RWMutex
	streams map[string][]*arkwenv1.EventEnvelope
	notify  chan struct{} // broadcast channel, closed+replaced on every append
}

// NewMem returns an in-memory append-only Log.
func NewMem() Log {
	return &memLog{
		streams: map[string][]*arkwenv1.EventEnvelope{},
		notify:  make(chan struct{}),
	}
}

// broadcast wakes all subscribers. Caller must hold l.mu.
func (l *memLog) broadcast() {
	close(l.notify)
	l.notify = make(chan struct{})
}

func (l *memLog) Create(_ context.Context, ev *arkwenv1.EventEnvelope) (*arkwenv1.EventEnvelope, error) {
	if ev.GetType() != arkwenv1.EventType_EVENT_TYPE_RUN_CREATED {
		return nil, fmt.Errorf("%w: Create requires EVENT_TYPE_RUN_CREATED", ErrInvalidEnvelope)
	}
	stored := proto.Clone(ev).(*arkwenv1.EventEnvelope)
	stored.Seq = 0
	if stored.GetTimestamp() == nil {
		stored.Timestamp = timestamppb.Now()
	}
	if err := Validate(stored); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.streams[stored.GetRunId()]) > 0 {
		return nil, ErrRunExists
	}
	l.streams[stored.GetRunId()] = []*arkwenv1.EventEnvelope{stored}
	l.broadcast()
	return proto.Clone(stored).(*arkwenv1.EventEnvelope), nil
}

func (l *memLog) Append(_ context.Context, ev *arkwenv1.EventEnvelope) (*arkwenv1.EventEnvelope, error) {
	if ev.GetType() == arkwenv1.EventType_EVENT_TYPE_RUN_CREATED {
		return nil, fmt.Errorf("%w: use Create for run.created", ErrInvalidEnvelope)
	}
	stored := proto.Clone(ev).(*arkwenv1.EventEnvelope)
	if stored.GetTimestamp() == nil {
		stored.Timestamp = timestamppb.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cur := l.streams[stored.GetRunId()]
	if len(cur) == 0 {
		return nil, fmt.Errorf("%w: append to non-existent run %q", ErrInvalidEnvelope, stored.GetRunId())
	}
	stored.Seq = cur[len(cur)-1].GetSeq() + 1
	if err := Validate(stored); err != nil {
		return nil, err
	}
	l.streams[stored.GetRunId()] = append(cur, stored)
	l.broadcast()
	return proto.Clone(stored).(*arkwenv1.EventEnvelope), nil
}

func (l *memLog) Read(_ context.Context, runID string, fromSeq uint64) ([]*arkwenv1.EventEnvelope, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cur := l.streams[runID]
	out := make([]*arkwenv1.EventEnvelope, 0, len(cur))
	for _, e := range cur {
		if e.GetSeq() >= fromSeq {
			out = append(out, proto.Clone(e).(*arkwenv1.EventEnvelope))
		}
	}
	return out, nil
}

func (l *memLog) Head(_ context.Context, runID string) (uint64, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cur := l.streams[runID]
	if len(cur) == 0 {
		return 0, false
	}
	return cur[len(cur)-1].GetSeq(), true
}

func (l *memLog) Runs(_ context.Context) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.streams))
	for id := range l.streams {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (l *memLog) snapshotNotify() chan struct{} {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.notify
}

func (l *memLog) Subscribe(ctx context.Context, runID string, fromSeq uint64) <-chan *arkwenv1.EventEnvelope {
	out := make(chan *arkwenv1.EventEnvelope)
	go func() {
		defer close(out)
		cur := fromSeq
		for {
			// grab the current notify BEFORE reading, so an append between the
			// read and the wait cannot be missed (classic broadcast pattern).
			n := l.snapshotNotify()
			evs, _ := l.Read(ctx, runID, cur)
			for _, e := range evs {
				select {
				case out <- e:
					cur = e.GetSeq() + 1
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-n:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
