package conformance

import (
	"context"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/projection"
	"google.golang.org/protobuf/proto"
)

// Invariant 2 flagship: the Controller is a PURE PROJECTION over the append-only
// stream — no mutable shadow state. Dropping the projection and rebuilding it from
// seq 0 must be byte-identical to the controller's live view. We drive a real run,
// read the controller's projections, then independently re-fold the raw event
// stream and assert equality for every projection kind.
func TestReplayEquivalence(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	missionRef, err := rt.CAS.Put(ctx, "mission.md", []byte("replay me"), "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	spec := &arkwenv1.RunSpec{MissionRef: missionRef, TenantId: "acme", MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT}
	runID, _, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts("claude-code", "", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Controller.Drive(ctx, runID); err != nil {
		t.Fatal(err)
	}

	// live projections (folded by the controller)
	live := map[arkwenv1.ProjectionKind]proto.Message{}
	for _, k := range []arkwenv1.ProjectionKind{
		arkwenv1.ProjectionKind_PROJECTION_KIND_STATUS,
		arkwenv1.ProjectionKind_PROJECTION_KIND_ARTIFACT_MANIFEST,
		arkwenv1.ProjectionKind_PROJECTION_KIND_WORKBENCH_DIFF,
		arkwenv1.ProjectionKind_PROJECTION_KIND_RUN_SUMMARY,
	} {
		resp, err := rt.Controller.Projection(ctx, runID, k)
		if err != nil {
			t.Fatalf("projection %v: %v", k, err)
		}
		live[k] = extractProjection(resp)
	}

	// rebuild by replaying the raw stream from seq 0 (a fresh, independent fold)
	evs, err := rt.Controller.Events(ctx, runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	seed := projection.Seed(evs)
	rebuilt := map[arkwenv1.ProjectionKind]proto.Message{
		arkwenv1.ProjectionKind_PROJECTION_KIND_STATUS:            projection.Status(evs),
		arkwenv1.ProjectionKind_PROJECTION_KIND_ARTIFACT_MANIFEST: projection.ArtifactManifest(evs),
		arkwenv1.ProjectionKind_PROJECTION_KIND_WORKBENCH_DIFF:    projection.WorkbenchDiff(evs, seed.GetWorkbenchBaseCommit(), seed.GetWorkbenchSnapshotRef()),
		arkwenv1.ProjectionKind_PROJECTION_KIND_RUN_SUMMARY:       projection.RunSummary(evs),
	}

	for k, l := range live {
		if !proto.Equal(l, rebuilt[k]) {
			t.Fatalf("replay-equivalence violated for %v:\n live=%v\n rebuilt=%v", k, l, rebuilt[k])
		}
	}

	// and the run reached a legal terminal (Invariant 8)
	st := projection.Status(evs)
	if st.GetState() != arkwenv1.LifecycleState_LIFECYCLE_STATE_COMPLETED {
		t.Fatalf("expected COMPLETED, got %v", st.GetState())
	}
}

func extractProjection(resp *arkwenv1.GetProjectionResponse) proto.Message {
	switch p := resp.GetProjection().(type) {
	case *arkwenv1.GetProjectionResponse_Status:
		return p.Status
	case *arkwenv1.GetProjectionResponse_ArtifactManifest:
		return p.ArtifactManifest
	case *arkwenv1.GetProjectionResponse_WorkbenchDiff:
		return p.WorkbenchDiff
	case *arkwenv1.GetProjectionResponse_RunSummary:
		return p.RunSummary
	case *arkwenv1.GetProjectionResponse_RunMetrics:
		return p.RunMetrics
	}
	return nil
}
