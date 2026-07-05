package conformance

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/authz"
	"github.com/arkwen/arkwen/internal/controlplane"
	"github.com/arkwen/arkwen/internal/ids"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// cpFixture spins up the control plane over an in-process bufconn listener with a
// principal set we fully control (operator, a read-only principal, a cross-tenant
// consumer) so S5 + S6 denials are exercised without a real port.
type cpFixture struct {
	rt   *app.Runtime
	eng  *authz.Engine
	conn *grpc.ClientConn
	cmd  arkwenv1.CommandPlaneClient
	read arkwenv1.ReadPlaneClient
}

func newCPFixture(t *testing.T) *cpFixture { return newCPFixtureAutoDrive(t, true) }

func newCPFixtureAutoDrive(t *testing.T, autoDrive bool) *cpFixture {
	t.Helper()
	rt := app.New()
	eng := authz.New(nil)
	authn := authz.NewTokenAuthenticator()

	op := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "op", TenantId: "default"}
	authn.Bind("op-token", op)
	for _, p := range []arkwenv1.Permission{
		arkwenv1.Permission_PERMISSION_RUNS_ENQUEUE,
		arkwenv1.Permission_PERMISSION_RUNS_READ,
		arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_CANCEL,
		arkwenv1.Permission_PERMISSION_GATES_RESOLVE,
	} {
		eng.AddGrant(authz.Grant{PrincipalID: "op", Tenant: "default", Permission: p, Selector: authz.Selector{Kind: authz.SelectorTenant}})
	}
	// read-only principal (least privilege: no gates:resolve) — F3
	reader := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "reader", TenantId: "default"}
	authn.Bind("reader-token", reader)
	eng.AddGrant(authz.Grant{PrincipalID: "reader", Tenant: "default", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: authz.Selector{Kind: authz.SelectorTenant}})
	// cross-tenant consumer in tenant "alpha" — F6
	alpha := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OUTER_LOOP_CONSUMER, PrincipalId: "alpha", TenantId: "alpha"}
	authn.Bind("alpha-token", alpha)
	eng.AddGrant(authz.Grant{PrincipalID: "alpha", Tenant: "alpha", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: authz.Selector{Kind: authz.SelectorTenant}})

	srv := controlplane.New(rt.Controller, eng, authn, controlplane.Options{DefaultWorker: "claude-code", AutoDrive: autoDrive})

	lis := bufconn.Listen(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = controlplane.Serve(ctx, lis, srv) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &cpFixture{rt: rt, eng: eng, conn: conn, cmd: arkwenv1.NewCommandPlaneClient(conn), read: arkwenv1.NewReadPlaneClient(conn)}
}

func missionSpec(text, tenant string) *arkwenv1.RunSpec {
	h := ids.Sha256([]byte(text))
	return &arkwenv1.RunSpec{
		MissionRef:          &arkwenv1.ContentRef{Path: "mission.md", ContentHash: h, SizeBytes: uint64(len(text)), MimeType: "text/markdown", ArtifactRef: "cas://sha256/" + h.GetHex()},
		TenantId:            tenant,
		MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT,
	}
}

// S5 happy path + late-consumer replay: enqueue over gRPC, then a LATE subscriber
// (from seq 0) still receives the whole stream incl. the terminal — proving the
// run executed independently of any consumer (no backpressure, Invariant 10).
func TestControlPlane_EnqueueSubscribeProjection(t *testing.T) {
	f := newCPFixture(t)
	ctx := controlplane.WithToken(context.Background(), "op-token")

	resp, err := f.cmd.Enqueue(ctx, &arkwenv1.EnqueueRequest{RunSpec: missionSpec("build a thing", "default"), IdempotencyKey: "idem-1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if resp.GetRunId() == "" {
		t.Fatal("empty run id")
	}

	subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := f.read.Subscribe(subCtx, &arkwenv1.SubscribeRequest{RunId: resp.GetRunId(), FromSeq: 0})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sawFinished := false
	for {
		ev, err := stream.Recv()
		if err == io.EOF || err != nil {
			break
		}
		if ev.GetType() == arkwenv1.EventType_EVENT_TYPE_RUN_FINISHED {
			sawFinished = true
			break
		}
	}
	if !sawFinished {
		t.Fatal("run did not reach RUN_FINISHED via the read plane")
	}

	pr, err := f.read.GetProjection(ctx, &arkwenv1.GetProjectionRequest{RunId: resp.GetRunId(), Kind: arkwenv1.ProjectionKind_PROJECTION_KIND_RUN_METRICS})
	if err != nil {
		t.Fatalf("get projection: %v", err)
	}
	if pr.GetRunMetrics().GetTerminal().GetState() != arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED {
		t.Fatalf("run-metrics terminal = %v, want COMPLETED", pr.GetRunMetrics().GetTerminal().GetState())
	}
}

// F3: a principal without gates:resolve is DENIED; the denial is recorded ONLY in
// the audit ledger and NO run-stream event is fabricated.
func TestControlPlane_F3_MissingGrantDeniedLedgerOnly(t *testing.T) {
	f := newCPFixture(t)
	opCtx := controlplane.WithToken(context.Background(), "op-token")
	resp, err := f.cmd.Enqueue(opCtx, &arkwenv1.EnqueueRequest{RunSpec: missionSpec("gate me", "default"), IdempotencyKey: "idem-f3"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ledgerBefore := f.eng.Ledger().Len()
	readerCtx := controlplane.WithToken(context.Background(), "reader-token")
	_, err = f.cmd.ResolveGate(readerCtx, &arkwenv1.ResolveGateRequest{RunId: resp.GetRunId(), GateId: "g-x", Decision: arkwenv1.GateDecision_GATE_DECISION_APPROVE, Rationale: "looks fine"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	// a DENY entry landed in the audit ledger
	found := false
	for _, e := range f.eng.Ledger().Read(uint64(ledgerBefore)) {
		if ad := e.GetAuthDecision(); ad != nil && ad.GetPermission() == arkwenv1.Permission_PERMISSION_GATES_RESOLVE && ad.GetOutcome() == arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY {
			found = true
		}
	}
	if !found {
		t.Fatal("F3: denial not recorded in the audit ledger")
	}
	// and NO gate.resolved event exists on the run stream
	evs, _ := f.rt.Controller.Events(context.Background(), resp.GetRunId(), 0)
	for _, e := range evs {
		if e.GetType() == arkwenv1.EventType_EVENT_TYPE_GATE_RESOLVED {
			t.Fatal("F3: a denied ResolveGate fabricated a run-stream event")
		}
	}
}

// F6: cross-tenant read is default-deny.
func TestControlPlane_F6_CrossTenantDenied(t *testing.T) {
	f := newCPFixture(t)
	opCtx := controlplane.WithToken(context.Background(), "op-token")
	resp, err := f.cmd.Enqueue(opCtx, &arkwenv1.EnqueueRequest{RunSpec: missionSpec("secret work", "default"), IdempotencyKey: "idem-f6"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	alphaCtx := controlplane.WithToken(context.Background(), "alpha-token")
	stream, err := f.read.Subscribe(alphaCtx, &arkwenv1.SubscribeRequest{RunId: resp.GetRunId(), FromSeq: 0})
	if err == nil {
		_, err = stream.Recv() // the deny surfaces on first recv for a server-stream
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("F6: cross-tenant subscribe should be PermissionDenied, got %v", err)
	}
}

// F5: enqueue with no consumer connected still drives to terminal (no backpressure).
func TestControlPlane_F5_NoConsumerNoBackpressure(t *testing.T) {
	f := newCPFixture(t)
	ctx := controlplane.WithToken(context.Background(), "op-token")
	resp, err := f.cmd.Enqueue(ctx, &arkwenv1.EnqueueRequest{RunSpec: missionSpec("no watcher", "default"), IdempotencyKey: "idem-f5"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// poll the status projection (no subscriber) until terminal — proves execution
	// does not depend on a consumer reading the stream.
	deadline := time.Now().Add(10 * time.Second)
	for {
		pr, err := f.read.GetProjection(ctx, &arkwenv1.GetProjectionRequest{RunId: resp.GetRunId(), Kind: arkwenv1.ProjectionKind_PROJECTION_KIND_STATUS})
		if err == nil {
			st := pr.GetStatus().GetState()
			if st == arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("F5: run did not complete without a consumer")
		}
	}
}

// S5 command=event: a CANCEL signal over the CommandPlane maps to EXACTLY ONE
// control event on the run stream (Invariant 2). AutoDrive is off so the run sits
// at created and the mapping is observed deterministically, free of the race with
// autonomous completion.
func TestControlPlane_S5_SignalCancelIsOneControlEvent(t *testing.T) {
	f := newCPFixtureAutoDrive(t, false)
	ctx := controlplane.WithToken(context.Background(), "op-token")

	resp, err := f.cmd.Enqueue(ctx, &arkwenv1.EnqueueRequest{RunSpec: missionSpec("cancel me", "default"), IdempotencyKey: "idem-sig-cancel"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	sig, err := f.cmd.Signal(ctx, &arkwenv1.SignalRequest{RunId: resp.GetRunId(), Signal: arkwenv1.SignalKind_SIGNAL_KIND_CANCEL})
	if err != nil {
		t.Fatalf("signal cancel: %v", err)
	}
	if sig.GetEventSeq() == 0 {
		t.Fatal("signal returned no control-event seq")
	}

	evs, _ := f.rt.Controller.Events(context.Background(), resp.GetRunId(), 0)
	cancels := 0
	for _, e := range evs {
		if e.GetType() == arkwenv1.EventType_EVENT_TYPE_RUN_CANCEL_REQUESTED {
			cancels++
			if e.GetSeq() != sig.GetEventSeq() {
				t.Fatalf("cancel event seq %d != returned seq %d", e.GetSeq(), sig.GetEventSeq())
			}
			if e.GetSource().GetPrincipalId() != "op" {
				t.Fatalf("cancel event source = %q, want op", e.GetSource().GetPrincipalId())
			}
		}
	}
	if cancels != 1 {
		t.Fatalf("Invariant 2: one command must map to exactly one control event, got %d", cancels)
	}
}

// Fail-closed: an UNSPECIFIED signal is rejected (enum 0 = most restrictive), never
// silently treated as a default action (Invariant 7). No run event is produced.
func TestControlPlane_S5_SignalUnspecifiedRejected(t *testing.T) {
	f := newCPFixtureAutoDrive(t, false)
	ctx := controlplane.WithToken(context.Background(), "op-token")

	resp, err := f.cmd.Enqueue(ctx, &arkwenv1.EnqueueRequest{RunSpec: missionSpec("bad signal", "default"), IdempotencyKey: "idem-sig-bad"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, err = f.cmd.Signal(ctx, &arkwenv1.SignalRequest{RunId: resp.GetRunId(), Signal: arkwenv1.SignalKind_SIGNAL_KIND_UNSPECIFIED})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unspecified signal must be InvalidArgument, got %v", err)
	}
	evs, _ := f.rt.Controller.Events(context.Background(), resp.GetRunId(), 0)
	for _, e := range evs {
		switch e.GetType() {
		case arkwenv1.EventType_EVENT_TYPE_RUN_CANCEL_REQUESTED,
			arkwenv1.EventType_EVENT_TYPE_RUN_PAUSED,
			arkwenv1.EventType_EVENT_TYPE_RUN_RESUMED:
			t.Fatal("a rejected signal fabricated a control event")
		}
	}
}

// F: a principal without runs:signal:cancel is DENIED; the denial is recorded ONLY
// in the audit ledger and NO run-stream event is fabricated (mirrors F3 for Signal).
func TestControlPlane_S6_SignalMissingGrantDeniedLedgerOnly(t *testing.T) {
	f := newCPFixtureAutoDrive(t, false)
	opCtx := controlplane.WithToken(context.Background(), "op-token")
	resp, err := f.cmd.Enqueue(opCtx, &arkwenv1.EnqueueRequest{RunSpec: missionSpec("cancel denied", "default"), IdempotencyKey: "idem-sig-deny"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ledgerBefore := f.eng.Ledger().Len()
	readerCtx := controlplane.WithToken(context.Background(), "reader-token") // has runs:read, NOT signal:cancel
	_, err = f.cmd.Signal(readerCtx, &arkwenv1.SignalRequest{RunId: resp.GetRunId(), Signal: arkwenv1.SignalKind_SIGNAL_KIND_CANCEL})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	found := false
	for _, e := range f.eng.Ledger().Read(uint64(ledgerBefore)) {
		if ad := e.GetAuthDecision(); ad != nil && ad.GetPermission() == arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_CANCEL && ad.GetOutcome() == arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY {
			found = true
		}
	}
	if !found {
		t.Fatal("Signal denial not recorded in the audit ledger")
	}
	evs, _ := f.rt.Controller.Events(context.Background(), resp.GetRunId(), 0)
	for _, e := range evs {
		if e.GetType() == arkwenv1.EventType_EVENT_TYPE_RUN_CANCEL_REQUESTED {
			t.Fatal("a denied Signal fabricated a run-stream event")
		}
	}
}
