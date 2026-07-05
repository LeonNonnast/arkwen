// Package controller is the Factory Controller — a PURE PROJECTION over the
// append-only event stream (Invariant 2). It holds no authoritative mutable run
// state: every view is folded from the log on demand. It is runtime-agnostic
// (Invariant 1): it drives runs through the generic adapter verbs and never
// branches on a worker kind. It is consumer-agnostic (Invariant 10): it names no
// outer loop and never calls out.
package controller

import (
	"context"
	"errors"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/cas"
	"github.com/arkwen/arkwen/internal/eventlog"
	"github.com/arkwen/arkwen/internal/gate"
	"github.com/arkwen/arkwen/internal/ids"
	"github.com/arkwen/arkwen/internal/isolation"
	"github.com/arkwen/arkwen/internal/policy"
	"github.com/arkwen/arkwen/internal/projection"
	"github.com/arkwen/arkwen/internal/secret"
	"github.com/arkwen/arkwen/internal/shim"
	"github.com/arkwen/arkwen/internal/warehouse"
	"google.golang.org/protobuf/proto"
)

// Controller wires the runtime together. The only state it keeps is the log
// (truth), the CAS, and stateless helpers; there is no mutable run cache.
type Controller struct {
	id      string
	log     eventlog.Log
	cas     cas.Store
	broker  secret.Broker
	isoReg  *isolation.Registry
	workers interface {
		Kinds() []string
	}
	shim      *shim.Shim
	gates     *gate.Manager
	warehouse *warehouse.Warehouse

	// authzPolicyVersion is the default (standalone) authz grant-set digest,
	// frozen into every seed's authz_policy_version (S6 replaces it with the real
	// per-tenant materialized grant-set).
	authzPolicyVersion *arkwenv1.Digest

	// imageToKind reverses image_digest -> worker kind. In production the Worker
	// Image (from the Warehouse) determines the worker; here the mapping is
	// deterministic from the registered kinds, so Drive needs no per-run state
	// and never branches on the kind (Invariant 1).
	imageToKind map[string]string
}

// Deps bundles the controller's dependencies.
type Deps struct {
	Log       eventlog.Log
	CAS       cas.Store
	Broker    secret.Broker
	IsoReg    *isolation.Registry
	Workers   WorkerBuilder
	Warehouse *warehouse.Warehouse // optional; enables reproduce + promotion-gate seam
}

// WorkerBuilder is the minimal worker registry the controller needs. It is used
// only to construct/negotiate workers — never to branch on kind (Invariant 1).
type WorkerBuilder interface {
	Kinds() []string
}

// New builds a Controller and its Cell-Shim.
func New(d Deps, sh *shim.Shim) *Controller {
	img := map[string]string{}
	for _, k := range d.Workers.Kinds() {
		img[imageDigestForWorker(k).GetHex()] = k
	}
	id := "controller-" + ids.Short("c")
	c := &Controller{
		id:                 id,
		log:                d.Log,
		cas:                d.CAS,
		broker:             d.Broker,
		isoReg:             d.IsoReg,
		workers:            d.Workers,
		shim:               sh,
		authzPolicyVersion: ids.Sha256([]byte("authz-policy\x00standalone-default-v0")),
		imageToKind:        img,
		warehouse:          d.Warehouse,
	}
	// The gate spine emits control events onto the same append-only stream. When a
	// Warehouse is present, promotion-scope gates fill the Slice-3 intake seam.
	emit := func(ctx context.Context, ev *arkwenv1.EventEnvelope) error {
		_, err := d.Log.Append(ctx, ev)
		return err
	}
	opts := []gate.Option{gate.WithEmitter(id)}
	if d.Warehouse != nil {
		opts = append(opts, gate.WithPromotionHook(&warehousePromo{wh: d.Warehouse}))
	}
	c.gates = gate.NewManager(emit, opts...)
	return c
}

// Reproduce resolves a run's frozen seed to byte-identical inputs (ADR-007 E6).
// Returns the classification ("REPRODUCED" or "DEGRADED") and resolved inputs.
func (c *Controller) Reproduce(ctx context.Context, runID string) (string, map[string]string, error) {
	seed, err := c.SeedOf(ctx, runID)
	if err != nil {
		return "", nil, err
	}
	class, inputs, err := warehouse.Reproduce(seed)
	return string(class), inputs, err
}

// workerKindForSeed resolves the worker kind from the frozen image_digest. The
// Controller never special-cases the returned kind (Invariant 1); it only needs
// it to construct the correct adapter.
func (c *Controller) workerKindForSeed(seed *arkwenv1.RunSeed) (string, error) {
	if k, ok := c.imageToKind[seed.GetImageDigest().GetHex()]; ok {
		return k, nil
	}
	return "", fmt.Errorf("controller: no worker image registered for digest %s", seed.GetImageDigest().GetHex())
}

// EmitFunc returns the shim emit hook wired to the append-only log. The shim is
// an intra-trust emitter; the Controller owns the run-scoped monotone seq.
func (c *Controller) EmitFunc() shim.EmitFunc {
	return func(ctx context.Context, ev *arkwenv1.EventEnvelope) error {
		_, err := c.log.Append(ctx, ev)
		return err
	}
}

// Events returns the run's events from fromSeq (replay).
func (c *Controller) Events(ctx context.Context, runID string, fromSeq uint64) ([]*arkwenv1.EventEnvelope, error) {
	return c.log.Read(ctx, runID, fromSeq)
}

// Subscribe streams the run's events (replay + live tail), no backpressure.
func (c *Controller) Subscribe(ctx context.Context, runID string, fromSeq uint64) <-chan *arkwenv1.EventEnvelope {
	return c.log.Subscribe(ctx, runID, fromSeq)
}

// Runs lists known run ids.
func (c *Controller) Runs(ctx context.Context) []string { return c.log.Runs(ctx) }

// Status folds the current lifecycle status (pure projection).
func (c *Controller) Status(ctx context.Context, runID string) (*arkwenv1.LifecycleStatus, error) {
	evs, err := c.log.Read(ctx, runID, 0)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("controller: unknown run %q", runID)
	}
	return projection.Status(evs), nil
}

// Projection folds one of the read-side views by kind (pure projection).
func (c *Controller) Projection(ctx context.Context, runID string, kind arkwenv1.ProjectionKind) (*arkwenv1.GetProjectionResponse, error) {
	evs, err := c.log.Read(ctx, runID, 0)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("controller: unknown run %q", runID)
	}
	asOf := evs[len(evs)-1].GetSeq()
	seed := projection.Seed(evs)
	resp := &arkwenv1.GetProjectionResponse{AsOfSeq: asOf}
	switch kind {
	case arkwenv1.ProjectionKind_PROJECTION_KIND_STATUS:
		resp.Projection = &arkwenv1.GetProjectionResponse_Status{Status: projection.Status(evs)}
	case arkwenv1.ProjectionKind_PROJECTION_KIND_ARTIFACT_MANIFEST:
		resp.Projection = &arkwenv1.GetProjectionResponse_ArtifactManifest{ArtifactManifest: projection.ArtifactManifest(evs)}
	case arkwenv1.ProjectionKind_PROJECTION_KIND_WORKBENCH_DIFF:
		resp.Projection = &arkwenv1.GetProjectionResponse_WorkbenchDiff{WorkbenchDiff: projection.WorkbenchDiff(evs, seed.GetWorkbenchBaseCommit(), seed.GetWorkbenchSnapshotRef())}
	case arkwenv1.ProjectionKind_PROJECTION_KIND_RUN_SUMMARY:
		resp.Projection = &arkwenv1.GetProjectionResponse_RunSummary{RunSummary: projection.RunSummary(evs)}
	case arkwenv1.ProjectionKind_PROJECTION_KIND_RUN_METRICS:
		resp.Projection = &arkwenv1.GetProjectionResponse_RunMetrics{RunMetrics: projection.RunMetrics(evs)}
	default:
		return nil, fmt.Errorf("controller: reject projection kind UNSPECIFIED")
	}
	return resp, nil
}

// resolveIsolation retrieves the frozen isolation contract from the CAS by the
// seed's isolation_contract_ref (stored at Enqueue). This keeps provisioning a
// re-verification of the frozen contract, never a re-resolution (ADR-009 R1).
func (c *Controller) resolveIsolation(ctx context.Context, seed *arkwenv1.RunSeed) (*arkwenv1.IsolationContract, error) {
	b, err := c.cas.GetDigest(ctx, seed.GetIsolationContractRef())
	if err != nil {
		return nil, fmt.Errorf("controller: resolve isolation contract: %w", err)
	}
	iso := &arkwenv1.IsolationContract{}
	if err := proto.Unmarshal(b, iso); err != nil {
		return nil, fmt.Errorf("controller: unmarshal isolation contract: %w", err)
	}
	iso.ContractRef = seed.GetIsolationContractRef()
	return iso, nil
}

// storeIsolation persists the ref-cleared composed contract in the CAS so its
// digest IS isolation_contract_ref and it is retrievable at Drive.
func (c *Controller) storeIsolation(ctx context.Context, comp *policy.Composed) error {
	isoCopy := proto.Clone(comp.Isolation).(*arkwenv1.IsolationContract)
	isoCopy.ContractRef = nil
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(isoCopy)
	if err != nil {
		return err
	}
	ref, err := c.cas.Put(ctx, "isolation.contract", b, "application/x-protobuf")
	if err != nil {
		return err
	}
	if ref.GetContentHash().GetHex() != comp.IsolationContractRef.GetHex() {
		return fmt.Errorf("controller: isolation contract digest mismatch (%s != %s)", ref.GetContentHash().GetHex(), comp.IsolationContractRef.GetHex())
	}
	return nil
}

// ErrLoosensFloor is re-exported so callers (control plane) can classify a
// rejected enqueue as a floor violation (Invariant 7/10).
var ErrLoosensFloor = policy.ErrLoosensFloor

// isLoosening reports whether err is a floor-loosening rejection.
func isLoosening(err error) bool { return errors.Is(err, policy.ErrLoosensFloor) }
