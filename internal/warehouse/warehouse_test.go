package warehouse

import (
	"context"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/cas"
	"github.com/arkwen/arkwen/internal/ids"
)

func snapSeed() *arkwenv1.RunSeed {
	return &arkwenv1.RunSeed{
		MissionHash:         ids.Sha256([]byte("mission")),
		ImageDigest:         ids.Sha256([]byte("image")),
		ToolkitVersions:     []*arkwenv1.ToolkitPin{{Name: "webapp", Digest: ids.Sha256([]byte("webapp"))}},
		MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT,
		WorkbenchBaseline:   &arkwenv1.RunSeed_WorkbenchSnapshotRef{WorkbenchSnapshotRef: ids.Sha256([]byte("wb"))},
		SessionSeed:         "frozen-seed-123",
	}
}

// D1: same seed -> byte-identical resolvedInputs on two passes.
func TestReproduce_D1_Deterministic(t *testing.T) {
	seed := snapSeed()
	c1, in1, err1 := Reproduce(seed)
	c2, in2, err2 := Reproduce(seed)
	if err1 != nil || err2 != nil {
		t.Fatalf("errs: %v %v", err1, err2)
	}
	if c1 != Reproduced || c2 != Reproduced {
		t.Fatalf("want REPRODUCED, got %s %s", c1, c2)
	}
	if len(in1) != len(in2) {
		t.Fatalf("len mismatch")
	}
	for k, v := range in1 {
		if in2[k] != v {
			t.Fatalf("field %s differs: %q vs %q", k, v, in2[k])
		}
	}
}

// D2: strict reproduce reuses the FROZEN session_seed (does not draw a fresh one).
func TestReproduce_D2_FrozenSessionSeed(t *testing.T) {
	seed := snapSeed()
	_, in, err := Reproduce(seed)
	if err != nil {
		t.Fatal(err)
	}
	if in["sessionSeed"] != "frozen-seed-123" {
		t.Fatalf("session seed not reused: %q", in["sessionSeed"])
	}
}

// D3: a floating channel alias in the snapshot ref is rejected.
func TestReproduce_D3_FloatingAliasRejected(t *testing.T) {
	seed := snapSeed()
	seed.WorkbenchBaseline = &arkwenv1.RunSeed_WorkbenchSnapshotRef{
		WorkbenchSnapshotRef: &arkwenv1.Digest{Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: "channel:tested"},
	}
	if _, _, err := Reproduce(seed); err == nil {
		t.Fatal("expected rejection of floating alias")
	}
}

// D4/D5: a mount-mode seed is classified DEGRADED (a result classification only).
func TestReproduce_D5_MountIsDegraded(t *testing.T) {
	seed := snapSeed()
	seed.MaterializationMode = arkwenv1.MaterializationMode_MATERIALIZATION_MODE_MOUNT
	seed.WorkbenchBaseline = &arkwenv1.RunSeed_WorkbenchMountRef{WorkbenchMountRef: "/mnt/live/wb"}
	c, _, err := Reproduce(seed)
	if err != nil {
		t.Fatal(err)
	}
	if c != Degraded {
		t.Fatalf("want DEGRADED, got %s", c)
	}
}

// GC: a blob referenced only transitively via a seed survives; dropping the last
// reference collects it (ADR-009 R7, generic reference-based GC).
func TestGC_ReferenceBased(t *testing.T) {
	ctx := context.Background()
	w := New(cas.NewMem())
	// store a blob and reference it only via a seed's workbench snapshot
	d, err := w.Put(ctx, CatalogArtifacts, "wb", []byte("workbench-bytes"), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	orphan, _ := w.Put(ctx, CatalogArtifacts, "orphan", []byte("orphan-bytes"), "application/octet-stream")

	seed := snapSeed()
	seed.WorkbenchBaseline = &arkwenv1.RunSeed_WorkbenchSnapshotRef{WorkbenchSnapshotRef: d}
	roots := DigestsFromSeed(seed)

	collectable := w.GC(roots...)
	// d is reachable (referenced by seed) -> survives; orphan -> collectable
	if containsDigest(collectable, d) {
		t.Fatal("referenced blob must survive GC")
	}
	if !containsDigest(collectable, orphan) {
		t.Fatal("orphan blob must be collectable")
	}
	// drop the last reference (empty roots) -> d becomes collectable
	if !containsDigest(w.GC(), d) {
		t.Fatal("blob with no references must be collectable")
	}
}

// Channel immutability: an exact alias cannot repoint; a channel can.
func TestChannelVsAliasImmutability(t *testing.T) {
	ctx := context.Background()
	w := New(cas.NewMem())
	d1, _ := w.Put(ctx, CatalogInputs, "img-v1", []byte("v1"), "application/octet-stream")
	d2, _ := w.Put(ctx, CatalogInputs, "img-v2", []byte("v2"), "application/octet-stream")

	if err := w.SetAlias("img:1.0.0", d1); err != nil {
		t.Fatal(err)
	}
	if err := w.SetAlias("img:1.0.0", d1); err != nil {
		t.Fatalf("re-set same digest should be a no-op: %v", err)
	}
	if err := w.SetAlias("img:1.0.0", d2); err == nil {
		t.Fatal("exact alias must be immutable")
	}
	// channel can move, and each move is a ledger entry
	if _, err := w.MoveChannel("tested", d1, op()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.MoveChannel("tested", d2, op()); err != nil {
		t.Fatal(err)
	}
	got, err := w.Resolve("tested")
	if err != nil || got.GetHex() != d2.GetHex() {
		t.Fatalf("channel should resolve to latest: %v %v", got, err)
	}
}

// Ledger uses ledger_seq and is disjoint from run seq.
func TestLedgerSeqMonotone(t *testing.T) {
	w := New(cas.NewMem())
	d := ids.Sha256([]byte("x"))
	s0, _ := w.MoveChannel("dev", d, op())
	s1 := w.Ledger().RecordIntakeRequested(op(), &arkwenv1.GateRule{GateId: "promote", Scope: arkwenv1.GateScope_GATE_SCOPE_PROMOTION}, d, "tested")
	if s0 != 0 || s1 != 1 {
		t.Fatalf("ledger_seq must be monotone from 0: got %d %d", s0, s1)
	}
	if w.Ledger().Head() != 2 {
		t.Fatalf("head should be 2, got %d", w.Ledger().Head())
	}
	for _, e := range w.Ledger().Read(0) {
		if e.GetLedgerSeq() > 1 {
			t.Fatal("unexpected ledger_seq")
		}
	}
}

func op() *arkwenv1.Principal {
	return &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "op", TenantId: "acme"}
}

func containsDigest(ds []*arkwenv1.Digest, target *arkwenv1.Digest) bool {
	for _, d := range ds {
		if d.GetHex() == target.GetHex() {
			return true
		}
	}
	return false
}
