# Building, Running & Testing the Arkwen Reference Runtime

This is the runnable Go implementation of the Arkwen factory runtime — the
slice plan (`docs/IMPLEMENTATION-PLAN.md`, S0…S6) realized on the frozen wire
contracts in `proto/arkwen/v1/`. It builds and runs on WSL2 with zero external
services; infra-heavy backends (Postgres, containerd/runc, gVisor, Firecracker,
Vault) are additive implementations behind the same interfaces and fail closed
where the host can't support them.

## 0. Toolchain

Go is installed self-contained at `~/go-toolchain` (it is **not** on the global
`PATH`). The `Makefile` auto-detects it, so you don't need to configure anything:

```bash
make build      # compile everything
make test       # full suite: unit tests + adversarial conformance vectors
make lint       # gofmt check + go vet
make conformance# just the golden-vector conformance suite, verbose
make demo       # run the walking skeleton end-to-end and print the event stream
make serve      # start the gRPC contract plane on 127.0.0.1:7777
make test-pg    # spin up throwaway Postgres, run durable event-store tests (-race)
make docker-build # build the deployable container image (the one Railway uses)
```

**Deploying to Railway?** See `docs/DEPLOY-RAILWAY.md` — one container, gRPC over the
TCP proxy + HTTP `/healthz` over the edge, Postgres event store via `DATABASE_URL`,
sealed command plane by default.

If you prefer a raw `go`, put the toolchain on `PATH` first:

```bash
export PATH="$HOME/go-toolchain/go/bin:$HOME/go/bin:$PATH"
go build ./... && go test ./...
```

## 1. The 30-second smoke test

```bash
make demo
```

Drives one Mission → one Production Run to `completed` and prints the append-only
event stream. You should see, in order: `RUN_CREATED → RUN_PROVISIONING →
SECRET_LEASED → RUN_STARTED → HEARTBEAT → WORKER_MESSAGE → SECRET_LEAK_DETECTED →
REDACTION_APPLIED → WORKER_RAW → EGRESS_DENIED → … → SECRET_REVOKED →
RUN_FINISHED`. That single run exercises every invariant seam:

- the injected model-API credential is **printed** by the worker but **redacted
  before persistence** (`SECRET_LEAK_DETECTED`/`REDACTION_APPLIED`);
- the model endpoint is reachable (intrinsic allowlist) but an off-allowlist
  destination is blocked (`EGRESS_DENIED`, default-deny floor);
- the secret lease is **revoked at reap** (`SECRET_REVOKED`);
- state is a pure fold over the log (the controller keeps no mutable run state).

Prove no secret ever hits the stream:

```bash
go run ./cmd/arkwen run create --mission x --json | grep -c 'sk-live-'   # -> 0
```

## 2. Drive a run over the gRPC contract plane (S5/S6)

Terminal A:

```bash
make serve                       # control plane on 127.0.0.1:7777
```

Terminal B:

```bash
export PATH="$HOME/go-toolchain/go/bin:$PATH"
go run ./cmd/arkwen ctl run --addr 127.0.0.1:7777 --mission "build me a thing"
```

The client authenticates (standalone Operator token), `Enqueue`s a run (each
command becomes exactly one control event), streams the event log back over
`ReadPlane.Subscribe` (pull-based — a slow/absent consumer never blocks
execution), and fetches the `RUN_METRICS` projection. Arkwen never calls out:
there is no callback/webhook/consumer-URL anywhere in the contract.

## 3. Inspect the isolation ladder

```bash
go run ./cmd/arkwen isolation
```

`standard` is available (local, runc-class hardening); `hardened` (gVisor) and
`strict` (Firecracker) report **unsatisfiable — fail-closed, no downgrade** on a
host without `runsc` / nested-virt. Requesting them never silently runs at a
weaker profile.

## 4. What `make test` proves — invariant → test map

| # | Invariant | Where it's enforced | Where it's tested |
|---|-----------|---------------------|-------------------|
| 1 | Controller runtime-agnostic | `controller` speaks only adapter verbs; `worker_kind` opaque | `test/conformance` AT1/AT6; a 2nd (echo) adapter passes the identical suite |
| 2 | Append-only log = sole truth; controller = pure projection | `eventlog`, `projection` | `eventlog` replay-equivalence; conformance G1–G3 |
| 3 | `worker.raw` never dropped except redacted secrets | `shim` redaction seam | conformance R2; G7 |
| 4 | Events carry pointers only | no `bytes` field in the wire; `eventlog.Validate` | conformance G5/R5 (descriptor-level) |
| 5 | Secrets never persisted; redaction before persistence | `redaction`, `secret`, `authz` structural exclusion | conformance R1–R4; `redaction` fidelity; runtime no-secret check |
| 6 | Repro-seeds frozen at `run.created` | `controller` seed freeze; `warehouse.Reproduce` | conformance G4/G8; `warehouse` D1–D5 |
| 7 | Fail-closed; stricter-only; floors non-removable | `policy.Compose`, `isolation.Select`, `gate.Decide`, `authz` | conformance F1/F2/F4/F8; `policy` loosening-reject; `isolation` fail-closed |
| 8 | `paused` = overlay, terminals only completed/failed/canceled | `projection.Status` | `projection` overlay test; conformance G6/D4 |
| 9 | Tiered + graceful degradation; floor never silently degrades | `shim` capability gating; `isolation` egress-parity | conformance AT2–AT5 |
| 10 | Outer loop only stricter; Arkwen depends on no consumer | `controlplane` (one-way, no callbacks) | conformance F5/G9; control-plane no-backpressure test |

## 5. Regenerating the wire types

The contracts in `proto/arkwen/v1/` are the source of truth. To regenerate the
Go types after a `.proto` change (needs `buf` on `PATH`, installed at
`~/go/bin/buf`):

```bash
export PATH="$HOME/go-toolchain/go/bin:$HOME/go/bin:$PATH"
buf generate
```

## 6. WSL2 caveats (by design, not gaps)

- **Isolation:** only `standard` runs here; `hardened`/`strict` are wired but
  fail-closed until a host with `runsc` / nested-virt exists (ADR-006, the plan's
  §7 risk). The security floor (default-deny egress, redaction, hardening posture)
  is identical across every tier — only observability degrades, never the floor.
- **Event store:** in-memory by default; a **PostgreSQL append-only backend**
  (`eventlog.NewPostgres`) plugs in behind `eventlog.Log` with no other code change
  — selected automatically when `DATABASE_URL` is set. It enforces `UNIQUE(run_id,
  seq)`, gapless monotone seq under a per-run advisory lock (READ COMMITTED),
  append-only via a trigger that rejects UPDATE/DELETE/TRUNCATE (Invariant 2 at the
  storage layer), and cross-connection LISTEN/NOTIFY fan-out with no producer
  backpressure. Proven by `make test-pg` (concurrent-seq, durability, NOTIFY,
  append-only, worker.raw round-trip — all under `-race`).
- **CAS/Warehouse:** in-memory by default; a filesystem/OCI-distribution backend
  plugs in behind `cas.Store`.
- **Secret-Broker:** in-memory reference broker; Vault/cloud-KMS plugs in behind
  `secret.Broker`.
- **Worker:** the built-in workers run a deterministic scripted mission so the
  skeleton is reproducible offline. A real Claude Code worker shells out when
  `ARKWEN_REAL_CLAUDE=1` and credentials are present.
