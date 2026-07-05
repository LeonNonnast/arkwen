package shim

import (
	"context"
	"fmt"
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/adapter"
	"github.com/arkwen/arkwen/internal/cas"
	"github.com/arkwen/arkwen/internal/ids"
	"github.com/arkwen/arkwen/internal/isolation"
	"github.com/arkwen/arkwen/internal/projection"
	"github.com/arkwen/arkwen/internal/redaction"
	"github.com/arkwen/arkwen/internal/secret"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Shim hosts workcells and implements the (conceptual) CellShimAdapter verbs.
// It is transport-agnostic: these Go methods are the contract surface; a gRPC
// server over a unix socket / vsock is an additive wrapper (ADR-006).
type Shim struct {
	cas     cas.Store
	broker  secret.Broker
	isoReg  *isolation.Registry
	workers *adapter.Registry
	emit    EmitFunc

	mu   sync.Mutex
	runs map[string]*run
}

type run struct {
	wc     *workcell
	worker adapter.Worker
}

// New builds a Shim.
func New(store cas.Store, broker secret.Broker, isoReg *isolation.Registry, workers *adapter.Registry, emit EmitFunc) *Shim {
	return &Shim{cas: store, broker: broker, isoReg: isoReg, workers: workers, emit: emit, runs: map[string]*run{}}
}

// Negotiate returns the capabilities this shim's worker actually declares,
// intersected with what the Controller desires. worker_kind is opaque.
func (s *Shim) Negotiate(workerKind string, desired []arkwenv1.Capability) (*arkwenv1.NegotiateResponse, error) {
	w, err := s.workers.Build(workerKind)
	if err != nil {
		return nil, err
	}
	var caps []arkwenv1.Capability
	for _, c := range w.Capabilities() {
		if len(desired) == 0 || adapter.HasCapability(desired, c) {
			caps = append(caps, c)
		}
	}
	return &arkwenv1.NegotiateResponse{AdapterProtocolVersion: 1, Capabilities: caps, WorkerKind: w.Kind()}, nil
}

// Create provisions the workcell: selects the isolation runtime (fail-closed, no
// downgrade), leases scoped secrets + registers them for redaction, and builds
// the egress guard. Returns PROVISIONING.
func (s *Shim) Create(ctx context.Context, runID, workerKind string, seed *arkwenv1.RunSeed, iso *arkwenv1.IsolationContract, scopes []secret.Scope) (*arkwenv1.LifecycleStatus, error) {
	worker, err := s.workers.Build(workerKind)
	if err != nil {
		return nil, err
	}
	// FAIL-CLOSED isolation selection — an unsatisfiable profile errors here and
	// the caller maps it to TERMINAL_REASON_ISOLATION_UNSATISFIABLE (no downgrade).
	if _, err := s.isoReg.Select(iso.GetProfile()); err != nil {
		return nil, err
	}

	red := redaction.New()
	wc := &workcell{
		runID:    runID,
		emitter:  "cell-shim-" + shortID(runID),
		mission:  missionFromSeed(seed),
		caps:     worker.Capabilities(),
		guard:    isolation.NewEgressGuard(iso.GetEgress()),
		redactor: red,
		cas:      s.cas,
		emit:     s.emit,
		secrets:  map[string]string{},
		done:     make(chan struct{}),
	}

	// Lease scoped secrets; inject + register for redaction; emit lease metadata.
	grant, err := s.broker.Lease(ctx, runID, seed.GetTenantId(), scopes)
	if err != nil {
		return nil, fmt.Errorf("shim: secret lease: %w", err)
	}
	for _, l := range grant.Leases {
		wc.secrets[l.EnvName] = grant.Material[l.EnvName]
		red.Register(l.RuleID, grant.Material[l.EnvName])
		wc.leases = append(wc.leases, l)
	}
	for _, l := range grant.Leases {
		lease := l
		wc.emitBroker(arkwenv1.EventType_EVENT_TYPE_SECRET_LEASED, func(ev *arkwenv1.EventEnvelope) {
			ev.Payload = &arkwenv1.EventEnvelope_SecretLeased{SecretLeased: &arkwenv1.SecretLeased{
				LeaseId: lease.ID, SecretScopeRef: lease.ScopeRef, ExpiresAt: timestamppb.New(lease.ExpiresAt),
			}}
		})
	}

	s.mu.Lock()
	s.runs[runID] = &run{wc: wc, worker: worker}
	s.mu.Unlock()
	return &arkwenv1.LifecycleStatus{
		State:            arkwenv1.LifecycleState_LIFECYCLE_STATE_PROVISIONING,
		SuspensionReason: arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE,
	}, nil
}

// Start launches the worker goroutine and returns RUNNING immediately, so
// control signals (cancel/pause/resume) can interleave.
func (s *Shim) Start(_ context.Context, runID string) (*arkwenv1.LifecycleStatus, error) {
	r, err := s.get(runID)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithCancel(context.Background())
	r.wc.mu.Lock()
	r.wc.cancelCtx = cancel
	r.wc.started = true
	r.wc.mu.Unlock()
	go func() {
		term := r.worker.Run(wctx, r.wc)
		r.wc.setTerminal(term)
	}()
	return &arkwenv1.LifecycleStatus{
		State:            arkwenv1.LifecycleState_LIFECYCLE_STATE_RUNNING,
		SuspensionReason: arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE,
	}, nil
}

// Signal handles cancel (MUST) and pause/resume (KAN, else boundary-freeze).
func (s *Shim) Signal(_ context.Context, runID string, sig arkwenv1.ShimSignal) (*arkwenv1.ShimSignalResponse, error) {
	r, err := s.get(runID)
	if err != nil {
		return nil, err
	}
	wc := r.wc
	hasPauseCap := adapter.HasCapability(wc.caps, arkwenv1.Capability_CAPABILITY_CONTROL_PAUSE_RESUME)
	switch sig {
	case arkwenv1.ShimSignal_SHIM_SIGNAL_CANCEL:
		wc.cancel()
		return &arkwenv1.ShimSignalResponse{Status: wc.status()}, nil
	case arkwenv1.ShimSignal_SHIM_SIGNAL_PAUSE:
		wc.setOverlay(arkwenv1.SuspensionReason_SUSPENSION_REASON_PAUSED_BY_USER)
		return &arkwenv1.ShimSignalResponse{Status: wc.status(), DegradedToBoundary: !hasPauseCap}, nil
	case arkwenv1.ShimSignal_SHIM_SIGNAL_RESUME:
		wc.setOverlay(arkwenv1.SuspensionReason_SUSPENSION_REASON_NONE)
		return &arkwenv1.ShimSignalResponse{Status: wc.status(), DegradedToBoundary: !hasPauseCap}, nil
	default:
		return nil, fmt.Errorf("shim: reject unspecified signal")
	}
}

// State returns the current projected lifecycle status.
func (s *Shim) State(_ context.Context, runID string) (*arkwenv1.LifecycleStatus, error) {
	r, err := s.get(runID)
	if err != nil {
		return nil, err
	}
	return r.wc.status(), nil
}

// Wait blocks until the worker reaches a terminal outcome.
func (s *Shim) Wait(ctx context.Context, runID string) (*arkwenv1.Termination, error) {
	r, err := s.get(runID)
	if err != nil {
		return nil, err
	}
	select {
	case <-r.wc.done:
		return r.wc.terminal(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Reap tears down the workcell for ANY terminal state incl. crash, revoking every
// secret lease (emitting secret_revoked metadata), and returns the termination.
func (s *Shim) Reap(ctx context.Context, runID string) (*arkwenv1.Termination, error) {
	r, err := s.get(runID)
	if err != nil {
		return nil, err
	}
	term, err := s.Wait(ctx, runID)
	if err != nil {
		return nil, err
	}
	revoked, _ := s.broker.Revoke(ctx, runID)
	byID := map[string]string{}
	for _, l := range r.wc.leases {
		byID[l.ID] = l.RuleID
	}
	for _, id := range revoked {
		leaseID := id
		r.wc.emitBroker(arkwenv1.EventType_EVENT_TYPE_SECRET_REVOKED, func(ev *arkwenv1.EventEnvelope) {
			ev.Payload = &arkwenv1.EventEnvelope_SecretRevoked{SecretRevoked: &arkwenv1.SecretRevoked{
				LeaseId: leaseID, Reason: "reap:" + term.GetState().String(),
			}}
		})
		r.wc.redactor.Unregister(byID[leaseID])
	}
	return term, nil
}

// Artifacts folds the shim's recorded events into the MUST report (manifest +
// workbench diff + terminal). This is a fold over events, never a live FS read.
func (s *Shim) Artifacts(_ context.Context, runID string, baseCommit string, snapshotRef *arkwenv1.Digest) (*arkwenv1.ArtifactManifest, *arkwenv1.WorkbenchDiff, *arkwenv1.Termination, error) {
	r, err := s.get(runID)
	if err != nil {
		return nil, nil, nil, err
	}
	recs := r.wc.snapshot()
	return projection.ArtifactManifest(recs), projection.WorkbenchDiff(recs, baseCommit, snapshotRef), r.wc.terminal(), nil
}

// StreamEvents returns the shim-recorded events from fromSeq. It is available
// ONLY if the worker declared EVENTS_STREAM; otherwise (false) the Controller
// degrades to worker.raw folding (Invariant 9).
func (s *Shim) StreamEvents(_ context.Context, runID string, fromSeq uint64) ([]*arkwenv1.EventEnvelope, bool, error) {
	r, err := s.get(runID)
	if err != nil {
		return nil, false, err
	}
	if !adapter.HasCapability(r.wc.caps, arkwenv1.Capability_CAPABILITY_EVENTS_STREAM) {
		return nil, false, nil
	}
	var out []*arkwenv1.EventEnvelope
	for _, e := range r.wc.snapshot() {
		if e.GetSeq() >= fromSeq {
			out = append(out, e)
		}
	}
	return out, true, nil
}

func (s *Shim) get(runID string) (*run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("shim: unknown run %q", runID)
	}
	return r, nil
}

// emitBroker emits an event with the secret-broker emitter (the broker is a
// separate component; the shim relays its lease/revoke metadata onto the stream).
func (w *workcell) emitBroker(t arkwenv1.EventType, set func(*arkwenv1.EventEnvelope)) {
	saved := w.emitter
	w.emitter = "secret-broker-" + shortID(w.runID)
	w.emitEnvelope(t, set)
	w.emitter = saved
}

func shortID(runID string) string {
	if len(runID) <= 8 {
		return runID
	}
	return runID[len(runID)-8:]
}

func missionFromSeed(seed *arkwenv1.RunSeed) string {
	if seed == nil {
		return ""
	}
	h := seed.GetMissionHash().GetHex()
	if len(h) > 12 {
		h = h[:12]
	}
	return "mission " + h
}

// scopesForRun returns the default secret scopes a run requests (S0 minimal path:
// the single model-API credential). S2b lets a run request arbitrary scopes.
func DefaultScopes() []secret.Scope {
	return []secret.Scope{{Name: "MODEL_API_KEY", Purpose: "worker model-API credential"}}
}

var _ = ids.Short
