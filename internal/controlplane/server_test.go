package controlplane

import (
	"context"
	"testing"
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/authz"
	"github.com/arkwen/arkwen/internal/ids"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func authCtx(token string) context.Context {
	md := metadata.New(map[string]string{"authorization": token})
	return metadata.NewIncomingContext(context.Background(), md)
}

func missionRef(text string) *arkwenv1.ContentRef {
	h := ids.Sha256([]byte(text))
	return &arkwenv1.ContentRef{Path: "mission.md", ContentHash: h, SizeBytes: uint64(len(text)), MimeType: "text/markdown", ArtifactRef: "cas://sha256/" + h.GetHex()}
}

// enqueueAs enqueues a run for tenant, using a principal with RUNS_ENQUEUE.
func enqueueAs(t *testing.T, s *Server, token, tenant, worker string) string {
	t.Helper()
	resp, err := s.Enqueue(authCtx(token), &arkwenv1.EnqueueRequest{
		RunSpec: &arkwenv1.RunSpec{
			MissionRef:          missionRef("m"),
			TenantId:            tenant,
			MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT,
			Labels:              map[string]string{"worker_kind": worker},
		},
		IdempotencyKey: ids.Short("idem"),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return resp.GetRunId()
}

// buildServer wires a control-plane server over a real Controller with a custom
// AuthZ engine + token authenticator (so we can model distinct principals).
func buildServer(t *testing.T) (*Server, *authz.Engine, *authz.Ledger) {
	t.Helper()
	rt := app.New()
	eng := authz.New(authz.NewLedger())
	authn := authz.NewTokenAuthenticator()

	// creator: may enqueue in tenant beta + read beta
	creator := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OUTER_LOOP_CONSUMER, PrincipalId: "creator", TenantId: "beta"}
	authn.Bind("tok-creator", creator)
	eng.AddGrant(authz.Grant{PrincipalID: "creator", Tenant: "beta", Permission: arkwenv1.Permission_PERMISSION_RUNS_ENQUEUE, Selector: authz.Selector{Kind: authz.SelectorTenant, Value: "beta"}})
	eng.AddGrant(authz.Grant{PrincipalID: "creator", Tenant: "beta", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: authz.Selector{Kind: authz.SelectorTenant, Value: "beta"}})

	// reader: only RUNS_READ in tenant beta (NOT gates:resolve) — least-privilege
	reader := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "reader", TenantId: "beta"}
	authn.Bind("tok-reader", reader)
	eng.AddGrant(authz.Grant{PrincipalID: "reader", Tenant: "beta", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: authz.Selector{Kind: authz.SelectorTenant, Value: "beta"}})

	// alpha: RUNS_READ only in tenant alpha (cross-tenant vs beta runs)
	alpha := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OUTER_LOOP_CONSUMER, PrincipalId: "consumer-alpha", TenantId: "alpha"}
	authn.Bind("tok-alpha", alpha)
	eng.AddGrant(authz.Grant{PrincipalID: "consumer-alpha", Tenant: "alpha", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: authz.Selector{Kind: authz.SelectorTenant, Value: "alpha"}})

	s := New(rt.Controller, eng, authn, Options{DefaultWorker: "echo-stub", AutoDrive: false})
	return s, eng, eng.Ledger()
}

// F3: ResolveGate by a principal without gates:resolve is DENIED, recorded ONLY
// in the audit ledger, and fabricates NO run-stream event.
func TestF3_MissingGrantDeniedLedgerOnly(t *testing.T) {
	s, _, ledger := buildServer(t)
	runID := enqueueAs(t, s, "tok-creator", "beta", "echo-stub")

	before := ledger.Len()
	_, err := s.ResolveGate(authCtx("tok-reader"), &arkwenv1.ResolveGateRequest{
		RunId: runID, GateId: "g-deploy", Decision: arkwenv1.GateDecision_GATE_DECISION_APPROVE, Rationale: "looks fine",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	if ledger.Len() <= before {
		t.Fatal("denial must be recorded in the audit ledger")
	}
	// no gate.resolved fabricated on the run stream
	evs, _ := s.ctrl.Events(context.Background(), runID, 0)
	for _, e := range evs {
		if e.GetType() == arkwenv1.EventType_EVENT_TYPE_GATE_RESOLVED {
			t.Fatal("a denied ResolveGate must NOT emit gate.resolved (Invariant 2)")
		}
	}
}

// F6: cross-tenant runs:read is default-deny (caller tenant alpha != run tenant beta).
func TestF6_CrossTenantReadDenied(t *testing.T) {
	s, _, ledger := buildServer(t)
	runID := enqueueAs(t, s, "tok-creator", "beta", "echo-stub")

	before := ledger.Len()
	_, err := s.GetProjection(authCtx("tok-alpha"), &arkwenv1.GetProjectionRequest{
		RunId: runID, Kind: arkwenv1.ProjectionKind_PROJECTION_KIND_STATUS,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-tenant read must be denied, got %v", err)
	}
	if ledger.Len() <= before {
		t.Fatal("cross-tenant denial must be recorded in the audit ledger")
	}
	// same-tenant read is allowed
	if _, err := s.GetProjection(authCtx("tok-creator"), &arkwenv1.GetProjectionRequest{RunId: runID, Kind: arkwenv1.ProjectionKind_PROJECTION_KIND_STATUS}); err != nil {
		t.Fatalf("same-tenant read must be allowed: %v", err)
	}
}

// F7: cross-tenant co-residency forces isolation_profile >= STRICT.
func TestF7_CrossTenantForcesStrict(t *testing.T) {
	if authz.CoResidencyFloor("alpha", "beta") != arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT {
		t.Fatal("cross-tenant co-residency must force STRICT")
	}
	if authz.CoResidencyFloor("acme", "acme") == arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT {
		t.Fatal("same-tenant must not be forced to STRICT by this bridge")
	}
}

// Unauthenticated calls are fail-closed.
func TestUnauthenticatedDenied(t *testing.T) {
	s, _, _ := buildServer(t)
	_, err := s.Enqueue(context.Background(), &arkwenv1.EnqueueRequest{RunSpec: &arkwenv1.RunSpec{MissionRef: missionRef("m"), TenantId: "beta"}})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing credential must be Unauthenticated, got %v", err)
	}
}

// Consumer-agnostic / no-backpressure: a run drives to terminal with NO consumer
// subscribed (execution never depends on a reader — Invariant 10).
func TestNoBackpressure_RunCompletesWithoutConsumer(t *testing.T) {
	s, _, _ := buildServer(t)
	runID := enqueueAs(t, s, "tok-creator", "beta", "echo-stub")
	// drive synchronously (AutoDrive off) — no subscriber attached at any point
	if err := s.ctrl.Drive(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := s.ctrl.Status(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if st.GetState() == arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not complete without a consumer, state=%v", st.GetState())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
