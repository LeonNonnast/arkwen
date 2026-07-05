package conformance

import (
	"context"
	"reflect"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
)

// D1 + D2: reproduce() on a snapshot run is deterministic — two resolutions of the
// same frozen seed yield byte-identical inputs, reusing the frozen session_seed.
func TestD1_D2_ReproduceDeterministic(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	mref, _ := rt.CAS.Put(ctx, "mission.md", []byte("reproduce me"), "text/markdown")
	spec := &arkwenv1.RunSpec{MissionRef: mref, TenantId: "acme", MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT, SessionSeed: "fixed-seed-123"}
	runID, _, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts("claude-code", "", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	class1, in1, err := rt.Controller.Reproduce(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	class2, in2, err := rt.Controller.Reproduce(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if class1 != "REPRODUCED" || class2 != "REPRODUCED" {
		t.Fatalf("snapshot run must be REPRODUCED, got %q/%q", class1, class2)
	}
	if !reflect.DeepEqual(in1, in2) {
		t.Fatalf("reproduce not deterministic:\n%v\n%v", in1, in2)
	}
	if in1["sessionSeed"] != "fixed-seed-123" {
		t.Fatalf("strict reproduce must reuse the frozen session seed, got %q", in1["sessionSeed"])
	}
}

// D5: a mount-mode run is explicitly NOT bit-reproducible → DEGRADED classification
// (never a run state — Invariant 8).
func TestD5_MountRunDegraded(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	mref, _ := rt.CAS.Put(ctx, "mission.md", []byte("mount me"), "text/markdown")
	spec := &arkwenv1.RunSpec{MissionRef: mref, TenantId: "acme", MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_MOUNT}
	runID, _, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts("echo-stub", "/mnt/wb", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	class, _, err := rt.Controller.Reproduce(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if class != "DEGRADED" {
		t.Fatalf("mount run must classify DEGRADED, got %q", class)
	}
}
