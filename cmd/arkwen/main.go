// Command arkwen is the thin CLI stand-in for the command + read planes (S0). It
// drives one Mission -> one Production Run to a terminal state and replays the
// append-only stream. In S5 this is replaced by a thin client over the gRPC
// contract plane; the CLI remains a convenience wrapper.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/ids"
	"google.golang.org/protobuf/encoding/protojson"
)

const version = "arkwen 0.1.0 (slices S0–S6)"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version)
	case "run":
		runCmd(os.Args[2:])
	case "events":
		eventsCmd(os.Args[2:])
	case "workers":
		rt := app.New()
		fmt.Println(strings.Join(rt.Workers.Kinds(), "\n"))
	case "isolation":
		isolationCmd()
	case "demo":
		demo()
	case "serve":
		serveCmd(os.Args[2:])
	case "ctl":
		ctlCmd(os.Args[2:])
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `arkwen — factory runtime for autonomous software work

Usage:
  arkwen run create --mission <text> [--worker claude-code|echo-stub|openhands] [--workbench DIR] [--tenant acme]
  arkwen events tail <run_id>          replay the append-only stream for a run (same process)
  arkwen workers                        list registered worker kinds
  arkwen isolation                      show the isolation ladder + host availability
  arkwen demo                           run a full walking-skeleton demo and print the stream
  arkwen serve [--addr 127.0.0.1:7777]  start the gRPC contract plane (Read + Command)
  arkwen ctl run --addr H:P --mission … drive a run over the gRPC contract plane (client)
  arkwen version
`)
}

func runCmd(args []string) {
	if len(args) < 1 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, "usage: arkwen run create --mission <text> [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("run create", flag.ExitOnError)
	mission := fs.String("mission", "Improve the project.", "mission text")
	worker := fs.String("worker", "claude-code", "worker kind")
	workbench := fs.String("workbench", "", "workbench directory to snapshot")
	tenant := fs.String("tenant", "acme", "tenant id")
	asJSON := fs.Bool("json", false, "print events as canonical proto3 JSON")
	_ = fs.Parse(args[1:])

	rt := app.New()
	ctx := context.Background()

	missionRef, err := rt.CAS.Put(ctx, "mission.md", []byte(*mission), "text/markdown")
	must(err)

	spec := &arkwenv1.RunSpec{
		MissionRef:          missionRef,
		TenantId:            *tenant,
		MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT,
	}
	runID, seq, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts(*worker, *workbench, *tenant))
	must(err)
	fmt.Printf("run.created %s at seq %d (worker=%s tenant=%s)\n", runID, seq, *worker, *tenant)

	must(rt.Controller.Drive(ctx, runID))

	st, err := rt.Controller.Status(ctx, runID)
	must(err)
	fmt.Printf("terminal: %s (%s) — %s\n\n", st.GetState(), st.GetTermination().GetReason(), st.GetTermination().GetDetail())

	printStream(ctx, rt, runID, *asJSON)
}

func eventsCmd(args []string) {
	if len(args) < 2 || args[0] != "tail" {
		fmt.Fprintln(os.Stderr, "usage: arkwen events tail <run_id>  (note: shares process state; use `arkwen run create` for the demo)")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "note: the in-memory store is per-process; run `arkwen demo` for an end-to-end stream")
}

func isolationCmd() {
	rt := app.New()
	profiles := []arkwenv1.IsolationProfile{
		arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD,
		arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED,
		arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT,
	}
	for _, p := range profiles {
		_, err := rt.IsoReg.Select(p)
		status := "AVAILABLE"
		if err != nil {
			status = "unsatisfiable (fail-closed): " + err.Error()
		}
		fmt.Printf("%-28s %s\n", p.String(), status)
	}
}

func demo() {
	rt := app.New()
	ctx := context.Background()
	missionRef, err := rt.CAS.Put(ctx, "mission.md", []byte("Write a hello-world web app."), "text/markdown")
	must(err)
	spec := &arkwenv1.RunSpec{MissionRef: missionRef, TenantId: "acme", MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT}
	runID, _, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts("claude-code", "", "acme"))
	must(err)
	must(rt.Controller.Drive(ctx, runID))
	st, _ := rt.Controller.Status(ctx, runID)
	fmt.Printf("=== Arkwen walking skeleton ===\nrun %s -> %s\n\n", runID, st.GetState())
	printStream(ctx, rt, runID, false)
}

func printStream(ctx context.Context, rt *app.Runtime, runID string, asJSON bool) {
	evs, err := rt.Controller.Events(ctx, runID, 0)
	must(err)
	fmt.Printf("append-only event stream (%d events):\n", len(evs))
	mo := protojson.MarshalOptions{Indent: "  "}
	for _, e := range evs {
		if asJSON {
			b, _ := mo.Marshal(e)
			fmt.Println(string(b))
			continue
		}
		fmt.Printf("  seq %-2d  %-32s emitter=%s\n", e.GetSeq(), shortType(e.GetType()), e.GetEmitter())
	}
}

func shortType(t arkwenv1.EventType) string {
	return strings.TrimPrefix(t.String(), "EVENT_TYPE_")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var _ = ids.Short
