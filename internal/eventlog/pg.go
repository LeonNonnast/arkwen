package eventlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const notifyChannel = "arkwen_events"

// schemaDDL creates the append-only event table idempotently. Invariant 2 is
// enforced structurally by a trigger that rejects any UPDATE/DELETE/TRUNCATE —
// defence-in-depth against a buggy caller, and a storage-layer conformance point.
//
// (We do NOT rely on REVOKE UPDATE/DELETE: on a managed Postgres the app connects
// as the table owner, and owners bypass table privileges — the trigger is the
// only enforcement that actually fires for an owner.)
const schemaDDL = `
CREATE TABLE IF NOT EXISTS event_log (
    run_id         text        NOT NULL,
    seq            bigint      NOT NULL,               -- server-assigned, monotone, gapless per run
    type           integer     NOT NULL,               -- EventType enum (denormalized for filtering/obs)
    schema_version integer     NOT NULL,               -- per-event forward-compat axis
    envelope       bytea       NOT NULL,               -- marshaled proto EventEnvelope (already sanitized upstream)
    created_at     timestamptz NOT NULL DEFAULT now(), -- STORE ingest time; NOT the event's own Timestamp
    PRIMARY KEY (run_id, seq)                          -- UNIQUE(run_id,seq) + the range index Read/Subscribe use
);

CREATE INDEX IF NOT EXISTS event_log_runs ON event_log (run_id) WHERE seq = 0;

CREATE OR REPLACE FUNCTION event_log_no_mutate() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'event_log is append-only (Invariant 2): % rejected', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS event_log_append_only ON event_log;
CREATE TRIGGER event_log_append_only
    BEFORE UPDATE OR DELETE OR TRUNCATE ON event_log
    FOR EACH STATEMENT EXECUTE FUNCTION event_log_no_mutate();
`

// pgLog is the PostgreSQL-backed append-only Log — a pure drop-in behind the
// eventlog.Log interface (same append-only truth, same seq-idempotent fan-out, no
// producer backpressure). Selected by the composition root when DATABASE_URL is
// set. Redaction happens upstream in the shim, so the store only ever persists
// already-sanitized envelopes (Invariant 5) as marshaled proto bytes (Invariant
// 4: envelopes are metadata + ContentRef pointers, so they stay small).
//
// ISOLATION: the seq-assignment correctness proof (see Append) assumes READ
// COMMITTED — the Postgres/pgx default. Under REPEATABLE READ/SERIALIZABLE the
// transaction snapshot freezes at the advisory-lock statement (before the prior
// writer commits), which would break gapless seq assignment. Do not run this
// store under a higher default_transaction_isolation without adding a retry loop.
type pgLog struct {
	pool   *pgxpool.Pool
	lisCfg *pgx.ConnConfig // dedicated connection config for LISTEN

	// In-process broadcast hub driven by LISTEN/NOTIFY — byte-for-byte the memLog
	// pattern: a single channel closed+replaced on every wake.
	mu     sync.Mutex
	notify chan struct{}
}

// NewPostgres connects, ensures the schema, starts the LISTEN fan-out goroutine,
// and returns the Log plus a closer that releases the pool + listener.
func NewPostgres(ctx context.Context, dsn string) (Log, func(), error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("eventlog: pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("eventlog: ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaDDL); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("eventlog: schema: %w", err)
	}
	lisCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("eventlog: parse dsn: %w", err)
	}

	l := &pgLog{pool: pool, lisCfg: lisCfg, notify: make(chan struct{})}
	lctx, cancel := context.WithCancel(context.Background())
	go l.runListener(lctx)

	closer := func() { cancel(); pool.Close() }
	return l, closer, nil
}

// ---- broadcast hub (identical semantics to memLog) ----

func (l *pgLog) snapshotNotify() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.notify
}

func (l *pgLog) broadcast() {
	l.mu.Lock()
	defer l.mu.Unlock()
	close(l.notify)
	l.notify = make(chan struct{})
}

// runListener holds one dedicated connection on LISTEN and turns every NOTIFY
// into an in-process broadcast. It reconnects with backoff and broadcasts on
// (re)connect so any NOTIFY missed during an outage is recovered by subscribers'
// next Read (at-least-once + seq-idempotent tolerate the duplicate wake).
func (l *pgLog) runListener(ctx context.Context) {
	const baseBackoff = 100 * time.Millisecond
	backoff := baseBackoff
	// retryDelay throttles the NEXT (re)connect attempt. Called on every failure
	// path AND after a mid-session drop, so a connection that dies immediately
	// after LISTEN can't spin a reconnect+broadcast storm. Backoff is reset to base
	// only after a proven-stable connection (a delivered NOTIFY), below.
	retryDelay := func() {
		l.sleep(ctx, backoff)
		backoff = min(backoff*2, 5*time.Second)
	}
	for ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(ctx, l.lisCfg)
		if err != nil {
			retryDelay()
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
			_ = conn.Close(ctx)
			retryDelay()
			continue
		}
		l.broadcast() // (re)connected: wake everyone to re-Read (recover missed NOTIFYs)
		for ctx.Err() == nil {
			if _, err := conn.WaitForNotification(ctx); err != nil {
				break // conn dropped or ctx canceled -> reconnect / exit
			}
			backoff = baseBackoff // proven-stable: a real NOTIFY arrived
			l.broadcast()         // payload carries run_id; the global broadcast ignores it (mem parity)
		}
		_ = conn.Close(context.Background())
		if ctx.Err() == nil {
			retryDelay() // throttle reconnect after a drop
		}
	}
}

func (l *pgLog) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// ---- writes ----

// Create appends run.created as seq 0 of a NEW run. A duplicate maps to
// ErrRunExists via the PK conflict on (run_id, 0). Validated fail-closed first.
func (l *pgLog) Create(ctx context.Context, ev *arkwenv1.EventEnvelope) (*arkwenv1.EventEnvelope, error) {
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

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Same advisory key as Append: all writers to a run serialize on one lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, stored.GetRunId()); err != nil {
		return nil, err
	}
	if err := insertTx(ctx, tx, stored); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrRunExists
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrRunExists
		}
		return nil, err
	}
	return proto.Clone(stored).(*arkwenv1.EventEnvelope), nil
}

// Append assigns the next monotone seq under a per-run advisory lock, then inserts.
//
// Correctness (READ COMMITTED): each writer's first statement takes
// pg_advisory_xact_lock(run_id), held until COMMIT/ROLLBACK, so writers to a run
// are totally ordered. Postgres makes a committing txn visible before releasing
// its locks, and READ COMMITTED re-snapshots per statement, so the subsequent
// SELECT max(seq) — taken AFTER the lock is granted — observes the prior writer's
// committed row. Thus max+1 is unique across committers; a rolled-back writer
// inserts nothing and leaves max unchanged (gapless). The PK is a redundant
// backstop that would abort a hypothetical bug rather than corrupt the log.
func (l *pgLog) Append(ctx context.Context, ev *arkwenv1.EventEnvelope) (*arkwenv1.EventEnvelope, error) {
	if ev.GetType() == arkwenv1.EventType_EVENT_TYPE_RUN_CREATED {
		return nil, fmt.Errorf("%w: use Create for run.created", ErrInvalidEnvelope)
	}
	stored := proto.Clone(ev).(*arkwenv1.EventEnvelope)
	if stored.GetTimestamp() == nil {
		stored.Timestamp = timestamppb.Now()
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, stored.GetRunId()); err != nil {
		return nil, err
	}

	var maxSeq *int64
	if err := tx.QueryRow(ctx,
		`SELECT max(seq) FROM event_log WHERE run_id = $1`, stored.GetRunId(),
	).Scan(&maxSeq); err != nil {
		return nil, err
	}
	if maxSeq == nil { // no rows => run does not exist (mirrors memLog)
		return nil, fmt.Errorf("%w: append to non-existent run %q", ErrInvalidEnvelope, stored.GetRunId())
	}
	stored.Seq = uint64(*maxSeq) + 1

	if err := Validate(stored); err != nil {
		return nil, err
	}
	if err := insertTx(ctx, tx, stored); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return proto.Clone(stored).(*arkwenv1.EventEnvelope), nil
}

// insertTx writes one row and issues a TRANSACTIONAL notify (delivered iff the
// txn commits — a rolled-back append never wakes a subscriber).
func insertTx(ctx context.Context, tx pgx.Tx, ev *arkwenv1.EventEnvelope) error {
	raw, err := proto.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO event_log (run_id, seq, type, schema_version, envelope) VALUES ($1,$2,$3,$4,$5)`,
		ev.GetRunId(), int64(ev.GetSeq()), int32(ev.GetType()), int32(ev.GetSchemaVersion()), raw,
	); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, notifyChannel, ev.GetRunId())
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- reads ----

func (l *pgLog) Read(ctx context.Context, runID string, fromSeq uint64) ([]*arkwenv1.EventEnvelope, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT envelope FROM event_log WHERE run_id = $1 AND seq >= $2 ORDER BY seq`,
		runID, int64(fromSeq))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*arkwenv1.EventEnvelope{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		ev := &arkwenv1.EventEnvelope{}
		if err := proto.Unmarshal(raw, ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (l *pgLog) Head(ctx context.Context, runID string) (uint64, bool) {
	var maxSeq *int64
	if err := l.pool.QueryRow(ctx,
		`SELECT max(seq) FROM event_log WHERE run_id = $1`, runID,
	).Scan(&maxSeq); err != nil {
		// The interface has no error return here; a transient DB error must not be
		// silently indistinguishable from "run absent" without a trace (parity gap
		// vs memLog, which never errors).
		slog.Warn("eventlog: pg Head query failed", "run_id", runID, "err", err)
		return 0, false
	}
	if maxSeq == nil {
		return 0, false
	}
	return uint64(*maxSeq), true
}

func (l *pgLog) Runs(ctx context.Context) []string {
	rows, err := l.pool.Query(ctx, `SELECT run_id FROM event_log WHERE seq = 0 ORDER BY run_id`)
	if err != nil {
		slog.Warn("eventlog: pg Runs query failed", "err", err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.Warn("eventlog: pg Runs scan failed", "err", err)
			return out
		}
		out = append(out, id)
	}
	// A mid-iteration connection error surfaces only here; without this check the
	// list would be silently truncated and read as complete.
	if err := rows.Err(); err != nil {
		slog.Warn("eventlog: pg Runs iteration failed (list may be truncated)", "err", err)
	}
	return out
}

// Ping reports whether the store's connection pool is reachable — used by the
// operational /readyz probe so readiness reflects real DB health.
func (l *pgLog) Ping(ctx context.Context) error { return l.pool.Ping(ctx) }

// Subscribe replays from fromSeq then live-tails. Structure mirrors memLog
// (snapshot the wake channel BEFORE Read so an append between Read and wait is
// never missed), plus a safety-net poll (recovers a NOTIFY lost during a listener
// reconnect) and transient-DB-error backoff. No backpressure: the producer already
// committed; a slow reader only lags this goroutine, never Append.
func (l *pgLog) Subscribe(ctx context.Context, runID string, fromSeq uint64) <-chan *arkwenv1.EventEnvelope {
	out := make(chan *arkwenv1.EventEnvelope)
	go func() {
		defer close(out)
		poll := time.NewTicker(2 * time.Second)
		defer poll.Stop()
		cur := fromSeq
		for {
			n := l.snapshotNotify()
			evs, err := l.Read(ctx, runID, cur)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				select { // transient DB error: back off, don't spin
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					return
				}
				continue
			}
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
			case <-poll.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
