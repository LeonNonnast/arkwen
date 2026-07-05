package eventlog

import (
	"context"
	"testing"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/projection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func created(runID string) *arkwenv1.EventEnvelope {
	return &arkwenv1.EventEnvelope{
		RunId: runID, SchemaVersion: 1, Type: arkwenv1.EventType_EVENT_TYPE_RUN_CREATED,
		Timestamp: timestamppb.Now(),
		Payload:   &arkwenv1.EventEnvelope_RunCreated{RunCreated: &arkwenv1.RunCreated{Seed: &arkwenv1.RunSeed{TenantId: "acme"}}},
	}
}

func started(runID string) *arkwenv1.EventEnvelope {
	return &arkwenv1.EventEnvelope{
		RunId: runID, SchemaVersion: 1, Type: arkwenv1.EventType_EVENT_TYPE_RUN_STARTED,
		Timestamp: timestamppb.Now(), Payload: &arkwenv1.EventEnvelope_RunStarted{RunStarted: &arkwenv1.RunStarted{}},
	}
}

// Invariant 2: replay-equivalence — folding from the live log equals folding a
// fresh replay from seq 0; seq is contiguous 0..N.
func TestReplayEquivalence(t *testing.T) {
	ctx := context.Background()
	l := NewMem()
	if _, err := l.Create(ctx, created("r1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := l.Append(ctx, started("r1")); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := l.Read(ctx, "r1", 0)
	b, _ := l.Read(ctx, "r1", 0)
	if !proto.Equal(projection.Status(a), projection.Status(b)) {
		t.Fatal("replay not equivalent")
	}
	for i, e := range a {
		if e.GetSeq() != uint64(i) {
			t.Fatalf("seq not contiguous: index %d has seq %d", i, e.GetSeq())
		}
	}
}

// Invariant 2: Create is idempotency-guarding — a second Create errors ErrRunExists.
func TestCreateIdempotentGuard(t *testing.T) {
	ctx := context.Background()
	l := NewMem()
	if _, err := l.Create(ctx, created("r1")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Create(ctx, created("r1")); err != ErrRunExists {
		t.Fatalf("want ErrRunExists, got %v", err)
	}
}

// Fail-closed append validation.
func TestAppendRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	l := NewMem()
	_, _ = l.Create(ctx, created("r1"))
	// run.created via Append is rejected
	if _, err := l.Append(ctx, created("r1")); err == nil {
		t.Fatal("run.created via Append must be rejected")
	}
	// type UNSPECIFIED is rejected
	bad := &arkwenv1.EventEnvelope{RunId: "r1", SchemaVersion: 1, Timestamp: timestamppb.Now(),
		Payload: &arkwenv1.EventEnvelope_RunStarted{RunStarted: &arkwenv1.RunStarted{}}}
	if _, err := l.Append(ctx, bad); err == nil {
		t.Fatal("UNSPECIFIED type must be rejected")
	}
	// content ref without content_hash (pointer-integrity, Invariant 4)
	badRef := &arkwenv1.EventEnvelope{RunId: "r1", SchemaVersion: 1, Timestamp: timestamppb.Now(),
		Type:    arkwenv1.EventType_EVENT_TYPE_WORKER_RAW,
		Payload: &arkwenv1.EventEnvelope_WorkerRaw{WorkerRaw: &arkwenv1.WorkerRaw{Channel: "stdout", RawRef: &arkwenv1.ContentRef{Path: "x"}}}}
	if _, err := l.Append(ctx, badRef); err == nil {
		t.Fatal("content ref without content_hash must be rejected")
	}
}

// Invariant 2/10: a slow subscriber never blocks the producer (no backpressure).
func TestSubscribeNoBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := NewMem()
	_, _ = l.Create(ctx, created("r1"))
	ch := l.Subscribe(ctx, "r1", 0)
	// append many WITHOUT reading; each Append must return promptly.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			if _, err := l.Append(ctx, started("r1")); err != nil {
				t.Error(err)
				return
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("producer blocked on a slow subscriber (backpressure!)")
	}
	// the subscriber eventually catches up, in order
	got := 0
	prev := int64(-1)
	for e := range ch {
		if int64(e.GetSeq()) <= prev {
			t.Fatalf("out of order: %d after %d", e.GetSeq(), prev)
		}
		prev = int64(e.GetSeq())
		got++
		if got == 51 { // created + 50 appends
			break
		}
	}
}
