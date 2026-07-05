package eventlog

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These tests exercise the PostgreSQL Log against a real database. They are the
// behavioral-parity proof for the drop-in (same assertions as the mem tests) plus
// the Postgres-specific concurrency / durability / append-only / fan-out cases.
//
// They SKIP unless ARKWEN_TEST_DATABASE_URL is set, so `go test ./...` and the
// conformance suite stay Docker-free. Run them via `make test-pg`.

var pgRunSeq uint64

// uniqueRun yields a fresh run_id per call. The append-only trigger forbids
// TRUNCATE/DELETE, so tests must never collide on a run_id rather than clean up.
func uniqueRun(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, atomic.AddUint64(&pgRunSeq, 1))
}

func newPG(t *testing.T) (Log, func()) {
	t.Helper()
	dsn := os.Getenv("ARKWEN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKWEN_TEST_DATABASE_URL to run Postgres event-store tests")
	}
	log, closer, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(closer)
	return log, closer
}

func workerRaw(runID string) *arkwenv1.EventEnvelope {
	return &arkwenv1.EventEnvelope{
		RunId: runID, SchemaVersion: 1, Type: arkwenv1.EventType_EVENT_TYPE_WORKER_RAW,
		Timestamp: timestamppb.Now(),
		Payload: &arkwenv1.EventEnvelope_WorkerRaw{WorkerRaw: &arkwenv1.WorkerRaw{
			Channel: "stdout",
			RawRef: &arkwenv1.ContentRef{
				Path:        "raw/0.txt",
				ContentHash: &arkwenv1.Digest{Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: "deadbeef00000000000000000000000000000000000000000000000000000000"},
				ArtifactRef: "cas://sha256/deadbeef",
			},
		}},
	}
}

func createRun(runID string) *arkwenv1.EventEnvelope { return created(runID) }

// Parity: replay-equivalence + contiguous seq 0..N (same as TestReplayEquivalence).
func TestPG_ReplayEquivalence(t *testing.T) {
	l, _ := newPG(t)
	ctx := context.Background()
	r := uniqueRun("replay")
	if _, err := l.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := l.Append(ctx, started(r)); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := l.Read(ctx, r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 6 {
		t.Fatalf("want 6 events, got %d", len(evs))
	}
	for i, e := range evs {
		if e.GetSeq() != uint64(i) {
			t.Fatalf("seq not contiguous at %d: got %d", i, e.GetSeq())
		}
	}
}

// Parity: Create is idempotency-guarding (ErrRunExists on the second).
func TestPG_CreateIdempotentGuard(t *testing.T) {
	l, _ := newPG(t)
	ctx := context.Background()
	r := uniqueRun("idem")
	if _, err := l.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Create(ctx, createRun(r)); err != ErrRunExists {
		t.Fatalf("want ErrRunExists, got %v", err)
	}
}

// Parity: fail-closed append validation, incl. append to a non-existent run.
func TestPG_AppendRejectsInvalid(t *testing.T) {
	l, _ := newPG(t)
	ctx := context.Background()
	r := uniqueRun("invalid")
	if _, err := l.Append(ctx, started(r)); err == nil {
		t.Fatal("append to non-existent run must be rejected")
	}
	_, _ = l.Create(ctx, createRun(r))
	if _, err := l.Append(ctx, createRun(r)); err == nil {
		t.Fatal("run.created via Append must be rejected")
	}
}

// Parity: a slow subscriber never blocks the producer (no backpressure) and
// eventually receives every event in order over real LISTEN/NOTIFY.
func TestPG_SubscribeDeliversAndNoBackpressure(t *testing.T) {
	l, _ := newPG(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := uniqueRun("sub")
	if _, err := l.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}
	ch := l.Subscribe(ctx, r, 0)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 30; i++ {
			if _, err := l.Append(ctx, started(r)); err != nil {
				t.Error(err)
				return
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked on a slow subscriber (backpressure!)")
	}

	got, prev := 0, int64(-1)
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e := <-ch:
			if int64(e.GetSeq()) <= prev {
				t.Fatalf("out of order: %d after %d", e.GetSeq(), prev)
			}
			prev = int64(e.GetSeq())
			got++
			if got == 31 { // created + 30
				return
			}
		case <-timeout:
			t.Fatalf("only received %d/31 events", got)
		}
	}
}

// Concurrency (the crux): N concurrent Appends to one run yield a unique, gapless,
// monotone seq 0..N with no UNIQUE violation. Run under -race.
func TestPG_ConcurrentAppendGaplessMonotone(t *testing.T) {
	l, _ := newPG(t)
	ctx := context.Background()
	r := uniqueRun("concurrent")
	if _, err := l.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}
	const N = 200
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Append(ctx, started(r)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append failed: %v", err)
	}

	evs, err := l.Read(ctx, r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != N+1 {
		t.Fatalf("want %d events, got %d", N+1, len(evs))
	}
	for i, e := range evs {
		if e.GetSeq() != uint64(i) {
			t.Fatalf("seq gap/dup at index %d: got %d", i, e.GetSeq())
		}
	}
	if h, ok := l.Head(ctx, r); !ok || h != N {
		t.Fatalf("Head=%d ok=%v, want %d", h, ok, N)
	}
}

// Concurrency: M concurrent Creates of the same run_id => exactly one success,
// M-1 ErrRunExists, and a single seq-0 row.
func TestPG_ConcurrentCreateExactlyOne(t *testing.T) {
	l, _ := newPG(t)
	ctx := context.Background()
	r := uniqueRun("race-create")
	const M = 32
	var wg sync.WaitGroup
	var ok, exists int64
	for i := 0; i < M; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.Create(ctx, createRun(r))
			switch err {
			case nil:
				atomic.AddInt64(&ok, 1)
			case ErrRunExists:
				atomic.AddInt64(&exists, 1)
			default:
				t.Errorf("unexpected Create error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || exists != M-1 {
		t.Fatalf("want 1 success + %d ErrRunExists, got ok=%d exists=%d", M-1, ok, exists)
	}
}

// Durability: events survive a full close + reconnect (seq is DB state, not memory).
func TestPG_DurabilityAcrossRestart(t *testing.T) {
	dsn := os.Getenv("ARKWEN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKWEN_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	r := uniqueRun("durable")

	l1, closer1, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := l1.Append(ctx, started(r)); err != nil {
			t.Fatal(err)
		}
	}
	closer1()

	l2, closer2, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closer2()
	evs, err := l2.Read(ctx, r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("durability: want 4 events after restart, got %d", len(evs))
	}
	for i, e := range evs {
		if e.GetSeq() != uint64(i) {
			t.Fatalf("durability: seq %d != %d", e.GetSeq(), i)
		}
	}
}

// Fan-out is real cross-connection LISTEN/NOTIFY: a subscriber on one pgLog sees
// an append made through a SEPARATE pgLog/pool.
func TestPG_CrossConnectionNotify(t *testing.T) {
	dsn := os.Getenv("ARKWEN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ARKWEN_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := uniqueRun("xconn")

	reader, cr, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cr()
	writer, cw, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cw()

	if _, err := writer.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}
	ch := reader.Subscribe(ctx, r, 0)
	<-ch // consume seq 0 (created)

	if _, err := writer.Append(ctx, started(r)); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ch:
		if e.GetSeq() != 1 {
			t.Fatalf("want seq 1 via cross-connection notify, got %d", e.GetSeq())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cross-connection NOTIFY not delivered (LISTEN/NOTIFY broken)")
	}
}

// Invariant 2 at the storage layer: raw UPDATE/DELETE are rejected by the trigger.
func TestPG_AppendOnlyTriggerBlocksMutation(t *testing.T) {
	l, _ := newPG(t)
	ctx := context.Background()
	r := uniqueRun("append-only")
	if _, err := l.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, os.Getenv("ARKWEN_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `UPDATE event_log SET seq = seq WHERE run_id = $1`, r); err == nil {
		t.Fatal("Invariant 2: UPDATE on event_log must be rejected by the append-only trigger")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM event_log WHERE run_id = $1`, r); err == nil {
		t.Fatal("Invariant 2: DELETE on event_log must be rejected by the append-only trigger")
	}
	// the row is still there
	if h, ok := l.Head(ctx, r); !ok || h != 0 {
		t.Fatalf("run should be intact after blocked mutation, Head=%d ok=%v", h, ok)
	}
}

// Invariant 3: worker.raw round-trips through bytea unchanged (never dropped).
func TestPG_WorkerRawRoundTrip(t *testing.T) {
	l, _ := newPG(t)
	ctx := context.Background()
	r := uniqueRun("workerraw")
	if _, err := l.Create(ctx, createRun(r)); err != nil {
		t.Fatal(err)
	}
	in := workerRaw(r)
	stored, err := l.Append(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := l.Read(ctx, r, stored.GetSeq())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event at seq %d, got %d", stored.GetSeq(), len(evs))
	}
	// The RawRef pointer must survive the marshal/store/unmarshal round-trip.
	got := evs[0].GetWorkerRaw().GetRawRef()
	if got.GetContentHash().GetHex() != in.GetWorkerRaw().GetRawRef().GetContentHash().GetHex() {
		t.Fatal("Invariant 3: worker.raw RawRef content hash was dropped/altered")
	}
	if !proto.Equal(evs[0].GetWorkerRaw(), in.GetWorkerRaw()) {
		t.Fatal("Invariant 3: worker.raw payload not preserved through bytea round-trip")
	}
}
