package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/controlplane"
	"github.com/arkwen/arkwen/internal/ids"
	"google.golang.org/protobuf/encoding/protojson"
)

// ctlCmd is a thin client over the gRPC contract plane (replaces the in-process
// CLI path for S5). It enqueues a run, subscribes to the stream, and reads the
// Run-Metrics projection — the full one-way consumer->Arkwen contract.
func ctlCmd(args []string) {
	if len(args) < 1 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "usage: arkwen ctl run --addr HOST:PORT --mission <text> [--worker kind] [--tenant id]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("ctl run", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "control-plane address")
	mission := fs.String("mission", "Improve the project.", "mission text")
	worker := fs.String("worker", "claude-code", "worker kind")
	tenant := fs.String("tenant", "default", "tenant id")
	token := fs.String("token", app.OperatorToken, "auth token")
	_ = fs.Parse(args[1:])

	conn, err := controlplane.Dial(*addr)
	must(err)
	defer conn.Close()
	cmd := arkwenv1.NewCommandPlaneClient(conn)
	read := arkwenv1.NewReadPlaneClient(conn)

	ctx := controlplane.WithToken(context.Background(), *token)

	// mission_ref is content-addressed (secret-free mission body).
	missionHash := ids.Sha256([]byte(*mission))
	spec := &arkwenv1.RunSpec{
		MissionRef:          &arkwenv1.ContentRef{Path: "mission.md", ContentHash: missionHash, SizeBytes: uint64(len(*mission)), MimeType: "text/markdown", ArtifactRef: "cas://sha256/" + missionHash.GetHex()},
		TenantId:            *tenant,
		MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT,
		Labels:              map[string]string{"worker_kind": *worker},
	}
	resp, err := cmd.Enqueue(ctx, &arkwenv1.EnqueueRequest{RunSpec: spec, IdempotencyKey: ids.Short("idem")})
	must(err)
	fmt.Printf("enqueued %s (created seq %d)\n\n", resp.GetRunId(), resp.GetCreatedSeq())

	// Subscribe replays + tails until the terminal run.finished.
	stream, err := read.Subscribe(ctx, &arkwenv1.SubscribeRequest{RunId: resp.GetRunId(), FromSeq: 0})
	must(err)
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		fmt.Printf("  seq %-2d  %s\n", ev.GetSeq(), shortType(ev.GetType()))
		if ev.GetType() == arkwenv1.EventType_EVENT_TYPE_RUN_FINISHED {
			break
		}
	}

	pr, err := read.GetProjection(ctx, &arkwenv1.GetProjectionRequest{RunId: resp.GetRunId(), Kind: arkwenv1.ProjectionKind_PROJECTION_KIND_RUN_METRICS})
	must(err)
	b, _ := protojson.MarshalOptions{Indent: "  "}.Marshal(pr)
	fmt.Printf("\nrun-metrics:\n%s\n", string(b))
}
