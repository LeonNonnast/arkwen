package controller

import (
	"context"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/eventlog"
	"github.com/arkwen/arkwen/internal/ids"
	"github.com/arkwen/arkwen/internal/policy"
	"github.com/arkwen/arkwen/internal/projection"
	"github.com/arkwen/arkwen/internal/secret"
)

// EnqueueOptions carry the operational inputs that are NOT part of the
// consumer-agnostic wire RunSpec (Invariant 10): the worker kind (determined by
// the Worker Image in production), the workbench directory to snapshot, the
// enqueue principal, and the secret scopes to lease.
type EnqueueOptions struct {
	IdempotencyKey string
	WorkerKind     string
	WorkbenchDir   string
	CreatedBy      *arkwenv1.Principal
	Scopes         []secret.Scope
}

// Enqueue composes the policy bundle stricter-only (REJECTING any loosening —
// Invariant 7/10), resolves + freezes the full RunSeed, and appends run.created
// (the freeze point, ADR-009 R1). Idempotent by IdempotencyKey: a duplicate key
// returns the existing run without a second run.created.
func (c *Controller) Enqueue(ctx context.Context, spec *arkwenv1.RunSpec, opts EnqueueOptions) (string, uint64, error) {
	if spec.GetTenantId() == "" {
		return "", 0, fmt.Errorf("controller: RunSpec.tenant_id is mandatory (ADR-010 E2)")
	}
	if opts.WorkerKind == "" {
		return "", 0, fmt.Errorf("controller: worker kind required")
	}
	if spec.GetMissionRef().GetContentHash() == nil {
		return "", 0, fmt.Errorf("controller: RunSpec.mission_ref must be content-addressed")
	}

	// stricter-only composition — a loosening RunSpec is rejected here, never
	// silently corrected. The caller records the denial (S6 audit ledger).
	comp, err := policy.Compose(spec.GetPolicyBundle())
	if err != nil {
		return "", 0, err
	}
	if err := c.storeIsolation(ctx, comp); err != nil {
		return "", 0, err
	}

	scopes := opts.Scopes
	if scopes == nil {
		scopes = shimDefaultScopes()
	}
	scopeNames := make([]string, 0, len(scopes))
	for _, s := range scopes {
		scopeNames = append(scopeNames, s.Name)
	}

	// resolve the workbench baseline
	mode := spec.GetMaterializationMode()
	seed := &arkwenv1.RunSeed{
		MissionHash:          spec.GetMissionRef().GetContentHash(),
		ImageDigest:          imageDigestForWorker(opts.WorkerKind),
		WorkbenchBaseCommit:  "",
		MaterializationMode:  mode,
		AdapterVersion:       adapterVersion,
		PolicyVersion:        comp.PolicyVersion,
		SessionSeed:          orDefault(spec.GetSessionSeed(), ids.Short("seed")),
		BlueprintDigest:      blueprintOrDefault(spec.GetBlueprintRef()),
		IsolationContractRef: comp.IsolationContractRef,
		TenantId:             spec.GetTenantId(),
		AuthzPolicyVersion:   c.authzPolicyVersion,
		EgressPolicyHash:     comp.EgressPolicyHash,
		ResourceLimitsHash:   comp.ResourceLimitsHash,
		SecretScopeSetRef:    scopeSetRef(scopeNames),
		// Image-provenance refs (S2b, log-only enforcement now / hard-enforce in S3):
		// Sigstore/cosign signature + SLSA/in-toto provenance, pointers only.
		ImageSignatureRef:  provenanceRef("sig", opts.WorkerKind, "application/vnd.dev.sigstore.bundle+json"),
		ImageProvenanceRef: provenanceRef("slsa", opts.WorkerKind, "application/vnd.in-toto+json"),
	}
	if mode == arkwenv1.MaterializationMode_MATERIALIZATION_MODE_MOUNT || mode == arkwenv1.MaterializationMode_MATERIALIZATION_MODE_LIVE_BIND {
		// mount runs are explicitly NOT bit-reproducible (ADR-007 E6)
		seed.WorkbenchBaseline = &arkwenv1.RunSeed_WorkbenchMountRef{WorkbenchMountRef: orDefault(opts.WorkbenchDir, "/mnt/workbench")}
	} else {
		snap, err := snapshotWorkbench(ctx, c.cas, opts.WorkbenchDir)
		if err != nil {
			return "", 0, err
		}
		seed.WorkbenchBaseline = &arkwenv1.RunSeed_WorkbenchSnapshotRef{WorkbenchSnapshotRef: snap}
	}

	if err := validateSeed(seed); err != nil {
		return "", 0, err
	}

	runID := ids.RunIDFromKey(opts.IdempotencyKey)
	createdBy := opts.CreatedBy
	if createdBy == nil {
		createdBy = &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "operator", TenantId: spec.GetTenantId()}
	}

	ev := &arkwenv1.EventEnvelope{
		RunId:         runID,
		SchemaVersion: 1,
		Type:          arkwenv1.EventType_EVENT_TYPE_RUN_CREATED,
		Source:        createdBy,
		Emitter:       c.id,
		Payload: &arkwenv1.EventEnvelope_RunCreated{RunCreated: &arkwenv1.RunCreated{
			Seed:      seed,
			CreatedBy: createdBy,
		}},
	}
	stored, err := c.log.Create(ctx, ev)
	if err != nil {
		if err == eventlog.ErrRunExists {
			// idempotent: return the existing run.created seq
			existing, rerr := c.log.Read(ctx, runID, 0)
			if rerr == nil && len(existing) > 0 {
				return runID, existing[0].GetSeq(), nil
			}
		}
		return "", 0, err
	}
	return runID, stored.GetSeq(), nil
}

// SeedOf returns the frozen seed for a run (from run.created).
func (c *Controller) SeedOf(ctx context.Context, runID string) (*arkwenv1.RunSeed, error) {
	evs, err := c.log.Read(ctx, runID, 0)
	if err != nil {
		return nil, err
	}
	seed := projection.Seed(evs)
	if seed == nil {
		return nil, fmt.Errorf("controller: no seed for run %q", runID)
	}
	return seed, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func blueprintOrDefault(d *arkwenv1.Digest) *arkwenv1.Digest {
	if d != nil && d.GetHex() != "" {
		return d
	}
	return ids.Sha256([]byte("blueprint\x00default"))
}

// shimDefaultScopes mirrors shim.DefaultScopes without importing a cycle risk.
func shimDefaultScopes() []secret.Scope {
	return []secret.Scope{{Name: "MODEL_API_KEY", Purpose: "worker model-API credential"}}
}
