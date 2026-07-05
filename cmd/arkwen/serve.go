package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arkwen/arkwen/internal/app"
	"github.com/arkwen/arkwen/internal/config"
	"github.com/arkwen/arkwen/internal/controlplane"
	"github.com/soheilhy/cmux"
	"google.golang.org/grpc"
)

// serveCmd starts the outer-loop Contract Plane. On one socket ($PORT) cmux
// multiplexes two protocols:
//   - gRPC (HTTP/2)  — the frozen ReadPlane + CommandPlane; THE consumer contract.
//   - HTTP/1.1       — a thin operational envelope (/healthz, /readyz, and a
//     zero-run-data landing page). NOT a consumer contract (contract-first): it
//     carries no run/tenant data and no credentials, so it needs no AuthZ.
//
// Railway's HTTP edge terminates TLS and speaks HTTP/1.1 to the container (so the
// healthcheck reaches /healthz), while a gRPC client reaches the contract plane
// over Railway's TCP proxy (raw passthrough preserves HTTP/2). Both hit $PORT.
func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	// --addr overrides the env-derived bind (local convenience). Empty => use the
	// config-resolved address (PORT/ARKWEN_BIND_HOST). Kept so the Dockerfile CMD
	// never has to pass a flag (it doesn't) and local docs keep working.
	addrOverride := fs.String("addr", "", "override listen address host:port (default: derived from PORT/env)")
	_ = fs.Parse(args)

	// The override is folded into Load so the fail-closed seal decision is made on
	// the effective bind address, not a stale env default (a public --addr seals).
	cfg, err := config.Load(os.Getenv, *addrOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, cleanup, err := app.NewFromConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	defer cleanup()

	srv := controlplane.New(rt.Controller, rt.Authz, rt.Authn, controlplane.Options{
		DefaultWorker: cfg.DefaultWorker,
		AutoDrive:     cfg.AutoDrive,
	})

	lis, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	// ---- one socket, two protocols (cmux) ----------------------------------
	m := cmux.New(lis)
	m.SetReadTimeout(5 * time.Second) // a silent/bare-TCP connection can't stall a matcher
	// Match gRPC FIRST (HTTP/2 with the gRPC content-type), then HTTP/1.1 fallback.
	grpcL := m.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	httpL := m.Match(cmux.HTTP1Fast(), cmux.Any())

	// Native gRPC server → full server-streaming for Subscribe. TLS is layered
	// here via grpc.Creds(...) in production (the existing ServerOption seam).
	gs := grpc.NewServer()
	srv.Register(gs)

	hs := &http.Server{Handler: opsMux(cfg, rt), ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = gs.Serve(grpcL) }()
	go func() {
		if err := hs.Serve(httpL); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, cmux.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "http:", err)
		}
	}()
	go func() {
		if err := m.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintln(os.Stderr, "cmux:", err)
		}
	}()

	fmt.Printf("arkwen listening on %s — gRPC contract plane + HTTP ops via cmux\n", cfg.BindAddr)
	fmt.Printf("  store=%s worker=%s autodrive=%v command-plane=%s\n",
		config.StoreName(cfg.DatabaseURL), cfg.DefaultWorker, cfg.AutoDrive, cfg.TokenMode)
	// Print the credential VALUE only on the loopback dev-fallback path — never on
	// a public bind (Invariant 5: no secret into a persisted log stream).
	switch cfg.TokenMode {
	case config.TokenDevFallback:
		fmt.Printf("  [dev] operator token: %s\n", app.OperatorToken)
		fmt.Printf("  try: arkwen ctl run --addr %s --mission \"build me a thing\"\n", cfg.BindAddr)
	case config.TokenSealed:
		fmt.Println("  command plane SEALED (set ARKWEN_OPERATOR_TOKEN to open it)")
	case config.TokenProvisioned:
		fmt.Println("  command plane open (operator token provisioned from env)")
	}

	<-ctx.Done() // SIGTERM (Railway) / SIGINT (local)
	fmt.Println("shutdown: draining…")

	// Bound the drain so it fits inside RAILWAY_DEPLOYMENT_DRAINING_SECONDS.
	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = hs.Shutdown(shCtx)

	done := make(chan struct{})
	go func() { gs.GracefulStop(); close(done) }() // drains unary + Subscribe streams
	select {
	case <-done:
	case <-shCtx.Done():
		gs.Stop() // force-close long-lived Subscribe tails before SIGKILL
	}
	_ = lis.Close() // unblock m.Serve()
}

// opsMux is the operational HTTP envelope. It exposes ZERO run/tenant data and no
// credentials — only liveness/readiness and a static identity page. This is why
// it needs no authentication and cannot become a second consumer contract.
func opsMux(cfg *config.Config, rt *app.Runtime) http.Handler {
	mux := http.NewServeMux()
	plain := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	// Liveness: the process is up and serving.
	mux.HandleFunc("/healthz", plain(http.StatusOK, "ok\n"))
	// Readiness: for the in-memory store, up == ready. With a store that exposes a
	// health probe (Postgres), readiness reflects real DB reachability — 503 when
	// the pool is unreachable, so Railway can hold traffic off a broken instance.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if p, ok := rt.Log.(interface {
			Ping(context.Context) error
		}); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := p.Ping(ctx); err != nil {
				plain(http.StatusServiceUnavailable, "not ready\n")(w, r)
				return
			}
		}
		plain(http.StatusOK, "ready\n")(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "Arkwen — factory runtime for autonomous software work.\n%s\n\n", version)
		fmt.Fprintf(w, "event-store:    %s\n", config.StoreName(cfg.DatabaseURL))
		fmt.Fprintf(w, "command-plane:  %s\n", cfg.TokenMode)
		fmt.Fprintf(w, "default-worker: %s\n\n", cfg.DefaultWorker)
		fmt.Fprint(w, "The consumer contract is gRPC (ReadPlane + CommandPlane) over the\n"+
			"Railway TCP proxy. This HTTP surface is operational only — no run data.\n")
	})
	return mux
}
