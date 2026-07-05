// Package controlplane is the outer-loop Contract Plane (ADR-008 / ADR-010): the
// public, versioned, CONSUMER-AGNOSTIC gRPC surface. ReadPlane (subscribe +
// get_projection) is pull-based so a slow/absent consumer never blocks execution
// (Invariant 10, no backpressure). CommandPlane turns every command into exactly
// one control event (Invariant 2). Every op is authenticated + authorized; a
// denial is recorded ONLY in the audit ledger and NEVER fabricates a run event.
// Arkwen never calls out — there is no callback/webhook/consumer_url anywhere.
package controlplane

import (
	"context"
	"errors"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/authz"
	"github.com/arkwen/arkwen/internal/controller"
	"github.com/arkwen/arkwen/internal/policy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Server implements ReadPlane + CommandPlane over the Controller, guarded by the
// AuthZ engine. It holds no mutable run state (the Controller is a projection).
type Server struct {
	arkwenv1.UnimplementedReadPlaneServer
	arkwenv1.UnimplementedCommandPlaneServer

	ctrl        *controller.Controller
	authz       *authz.Engine
	authn       authz.Authenticator
	defaultWork string // worker kind used when a RunSpec carries no worker label
	autoDrive   bool   // drive runs autonomously on enqueue
}

// Options configures the server.
type Options struct {
	DefaultWorker string
	AutoDrive     bool
}

// New builds a control-plane Server.
func New(ctrl *controller.Controller, eng *authz.Engine, authn authz.Authenticator, opts Options) *Server {
	if opts.DefaultWorker == "" {
		opts.DefaultWorker = "claude-code"
	}
	return &Server{ctrl: ctrl, authz: eng, authn: authn, defaultWork: opts.DefaultWorker, autoDrive: opts.AutoDrive}
}

// principal authenticates the caller from the "authorization" metadata header.
// A missing/invalid credential is fail-closed (Unauthenticated); the AuthN
// material is verified-and-discarded (structural exclusion — never stored).
func (s *Server) principal(ctx context.Context) (*arkwenv1.Principal, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	var cred string
	if vs := md.Get("authorization"); len(vs) > 0 {
		cred = vs[0]
	}
	p, err := s.authn.Authenticate(cred)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "authentication required")
	}
	return p, nil
}

// authorize checks a permission, recording the decision (incl. denials) in the
// audit ledger, and returns a PermissionDenied error on DENY (no run event).
func (s *Server) authorize(p *arkwenv1.Principal, perm arkwenv1.Permission, runTenant string, attrs authz.RunAttrs) error {
	if s.authz.Authorize(p, perm, runTenant, attrs) != arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_ALLOW {
		return status.Errorf(codes.PermissionDenied, "denied: %v", perm)
	}
	return nil
}

// ---- CommandPlane ----

// Enqueue creates + freezes a run (and autonomously drives it if configured). A
// floor-loosening RunSpec is REJECTED (no run_id); the AuthZ denial path records
// only to the ledger.
func (s *Server) Enqueue(ctx context.Context, req *arkwenv1.EnqueueRequest) (*arkwenv1.EnqueueResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	spec := req.GetRunSpec()
	if err := s.authorize(p, arkwenv1.Permission_PERMISSION_RUNS_ENQUEUE, spec.GetTenantId(), authz.RunAttrs{Tenant: spec.GetTenantId(), OwnerID: p.GetPrincipalId()}); err != nil {
		return nil, err
	}
	workerKind := s.defaultWork
	if w := spec.GetLabels()["worker_kind"]; w != "" {
		workerKind = w
	}
	runID, seq, err := s.ctrl.Enqueue(ctx, spec, controller.EnqueueOptions{
		IdempotencyKey: req.GetIdempotencyKey(),
		WorkerKind:     workerKind,
		CreatedBy:      p,
	})
	if err != nil {
		if errors.Is(err, policy.ErrLoosensFloor) {
			// floor violation: rejected at enqueue, no run created (Invariant 7/10)
			return nil, status.Errorf(codes.FailedPrecondition, "run spec loosens the floor: %v", err)
		}
		return nil, status.Errorf(codes.InvalidArgument, "enqueue: %v", err)
	}
	if s.autoDrive {
		go func() { _ = s.ctrl.Drive(context.Background(), runID) }()
	}
	return &arkwenv1.EnqueueResponse{RunId: runID, CreatedSeq: seq}, nil
}

// Signal cancels (MUST) or pauses/resumes (KAN). Every signal is a control event.
func (s *Server) Signal(ctx context.Context, req *arkwenv1.SignalRequest) (*arkwenv1.SignalResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	tenant, attrs, err := s.runScope(ctx, req.GetRunId())
	if err != nil {
		return nil, err
	}
	var seq uint64
	switch req.GetSignal() {
	case arkwenv1.SignalKind_SIGNAL_KIND_CANCEL:
		if err := s.authorize(p, arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_CANCEL, tenant, attrs); err != nil {
			return nil, err
		}
		seq, err = s.ctrl.Cancel(ctx, req.GetRunId(), p)
	case arkwenv1.SignalKind_SIGNAL_KIND_PAUSE:
		if err := s.authorize(p, arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_PAUSE, tenant, attrs); err != nil {
			return nil, err
		}
		seq, err = s.ctrl.Pause(ctx, req.GetRunId(), p, arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_USER)
	case arkwenv1.SignalKind_SIGNAL_KIND_RESUME:
		if err := s.authorize(p, arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_RESUME, tenant, attrs); err != nil {
			return nil, err
		}
		seq, err = s.ctrl.Resume(ctx, req.GetRunId(), p, req.GetCostBudgetTopup())
	default:
		return nil, status.Error(codes.InvalidArgument, "reject unspecified signal")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "signal: %v", err)
	}
	return &arkwenv1.SignalResponse{EventSeq: seq}, nil
}

// ResolveGate resolves a pending gate. A caller without GATES_RESOLVE is denied
// (recorded in the ledger only — NO gate.resolved run event is fabricated).
func (s *Server) ResolveGate(ctx context.Context, req *arkwenv1.ResolveGateRequest) (*arkwenv1.ResolveGateResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	tenant, attrs, err := s.runScope(ctx, req.GetRunId())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(p, arkwenv1.Permission_PERMISSION_GATES_RESOLVE, tenant, attrs); err != nil {
		return nil, err
	}
	seq, err := s.ctrl.ResolveGate(ctx, req.GetRunId(), req.GetGateId(), req.GetDecision(), req.GetRationale(), req.GetResolutionPayloadRef(), p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve_gate: %v", err)
	}
	return &arkwenv1.ResolveGateResponse{EventSeq: seq}, nil
}

// Reprioritize changes a queued run's priority (queue ordering stays a projection).
func (s *Server) Reprioritize(ctx context.Context, req *arkwenv1.ReprioritizeRequest) (*arkwenv1.ReprioritizeResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	tenant, attrs, err := s.runScope(ctx, req.GetRunId())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(p, arkwenv1.Permission_PERMISSION_RUNS_REPRIORITIZE, tenant, attrs); err != nil {
		return nil, err
	}
	seq, err := s.ctrl.Reprioritize(ctx, req.GetRunId(), req.GetNewPriority(), p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reprioritize: %v", err)
	}
	return &arkwenv1.ReprioritizeResponse{EventSeq: seq}, nil
}

// SetFloor sets ONLY the org floor (above the intrinsic floor). Not run-scoped —
// recorded in the Control-Plane audit ledger (FloorChanged), NOT the run stream.
func (s *Server) SetFloor(ctx context.Context, req *arkwenv1.SetFloorRequest) (*arkwenv1.SetFloorResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(p, arkwenv1.Permission_PERMISSION_POLICY_SET_FLOOR, req.GetTenantId(), authz.RunAttrs{Tenant: req.GetTenantId()}); err != nil {
		return nil, err
	}
	ledgerSeq := s.authz.Ledger().RecordFloorChanged(p, req.GetNewFloorRef(), s.authz.PolicyVersion(req.GetTenantId()))
	return &arkwenv1.SetFloorResponse{LedgerSeq: ledgerSeq}, nil
}

// ---- ReadPlane ----

// Subscribe replays + tails the append-only stream. Pull-based: a slow consumer
// lags but never blocks execution (Invariant 10).
func (s *Server) Subscribe(req *arkwenv1.SubscribeRequest, stream arkwenv1.ReadPlane_SubscribeServer) error {
	p, err := s.principal(stream.Context())
	if err != nil {
		return err
	}
	tenant, attrs, err := s.runScope(stream.Context(), req.GetRunId())
	if err != nil {
		return err
	}
	if err := s.authorize(p, arkwenv1.Permission_PERMISSION_RUNS_READ, tenant, attrs); err != nil {
		return err
	}
	ch := s.ctrl.Subscribe(stream.Context(), req.GetRunId(), req.GetFromSeq())
	for ev := range ch {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

// GetProjection returns a folded read-side view.
func (s *Server) GetProjection(ctx context.Context, req *arkwenv1.GetProjectionRequest) (*arkwenv1.GetProjectionResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	tenant, attrs, err := s.runScope(ctx, req.GetRunId())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(p, arkwenv1.Permission_PERMISSION_RUNS_READ, tenant, attrs); err != nil {
		return nil, err
	}
	resp, err := s.ctrl.Projection(ctx, req.GetRunId(), req.GetKind())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "projection: %v", err)
	}
	return resp, nil
}

// runScope resolves a run's tenant + attrs for AuthZ (cross-tenant default-deny).
func (s *Server) runScope(ctx context.Context, runID string) (string, authz.RunAttrs, error) {
	seed, err := s.ctrl.SeedOf(ctx, runID)
	if err != nil {
		return "", authz.RunAttrs{}, status.Errorf(codes.NotFound, "run %s not found", runID)
	}
	return seed.GetTenantId(), authz.RunAttrs{RunID: runID, Tenant: seed.GetTenantId()}, nil
}
