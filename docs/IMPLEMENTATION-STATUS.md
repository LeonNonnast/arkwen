# Arkwen Reference Runtime — Implementation Status

Maps the slice plan (`IMPLEMENTATION-PLAN.md`) to the Go implementation. Every
slice's **contract + core logic** is implemented and tested; infra backends that
need capabilities WSL2 lacks are additive behind interfaces (see BUILD-AND-RUN §6).

| Slice | Status | Key packages | Conformance / tests |
|-------|--------|--------------|---------------------|
| **S0** First Light (walking skeleton + contract spine) | ✅ | `eventlog` `projection` `cas` `redaction` `shim` `adapter` `controller` `policy` (intrinsic floor) | golden-run G1–G9, redaction R1–R5, replay-equivalence, no-secret |
| **S1** Runtime-agnosticism, full lifecycle, control signals | ✅ | `shim` (Negotiate/Signal/StreamEvents), `adapter` (echo + claude-code + openhands), `projection` overlay | adapter-tiers AT1–AT6; cancel/pause-degradation |
| **S2a** Isolation ladder, egress, resource limits | ✅ | `isolation` (local/gVisor/Firecracker, fail-closed no-downgrade), `isolation.EgressGuard`, `isolation/resource`, `policy` (egress ∩, resource min) | isolation fail-closed + egress-parity; `resource_exhausted` reason |
| **S2b** Secret-Broker, redaction, image-provenance | ✅ | `secret` (lease/rotate/revoke), `shim` redaction (multi-secret), seed provenance refs (log-only) | broker tests; redaction fidelity; no-secret property |
| **S3** Warehouse & `reproduce()` | ✅ | `warehouse` (2 catalogs, channels, blueprint, reproduce, generic GC, ledger) | reproduce-determinism D1–D5; GC; channel immutability; ledger_seq |
| **S4** Quality Gates (run/action/promotion) | ✅ | `gate` (spine, Auto/Human resolver, `Decide`, `Manager`, timeouts), controller `RequestGate`/`ResolveGate`/`GateTimeout` | fail-closed F1/F2; gate manager tests; promotion hook |
| **S5** Outer-Loop Contract Plane (gRPC) | ✅ | `controlplane` (ReadPlane + CommandPlane), `cmd/arkwen serve`/`ctl` | command=event, no-backpressure, F5 consumer-agnostic |
| **S6** Contract-Plane AuthZ & Multi-Tenancy | ✅ | `authz` (Engine default-deny, grants, tenancy, co-residency floor, audit ledger) | F3/F4/F6/F7; least-privilege; structural secret exclusion (R4) |
| **Deploy** Railway-ready container (durable) | ✅ | `config` (env + seal decision), `eventlog.NewPostgres` (durable Log), cmux serve (`$PORT`/`::`, gRPC + HTTP `/healthz`), `Dockerfile` (distroless), `railway.json` | `config` seal tests; `make test-pg` (concurrent-seq/durability/NOTIFY/append-only, -race); container drive verified (health + gRPC + persistence + seal + SIGTERM) |

## Deployment (Railway)

A first durable MVP deploys as one container (`docs/DEPLOY-RAILWAY.md`): gRPC contract
plane over Railway's **TCP proxy** + HTTP `/healthz` over the **edge**, multiplexed on
one `$PORT` via cmux; the append-only event log becomes **PostgreSQL** when
`DATABASE_URL` is set (drop-in behind `eventlog.Log`, no contract change). The command
plane is **sealed** (fail-closed) on a public bind unless `ARKWEN_OPERATOR_TOKEN` is
provisioned. CAS/Warehouse/Secret-Broker/isolation tiers remain in-memory/fail-closed
behind their interfaces (unchanged).

## The 10 invariants — all encoded as continuous CI tests

Run `make test`. See `docs/BUILD-AND-RUN.md §4` for the invariant→enforcement→test
matrix. Each invariant is pinned by at least one **adversarial** assertion, per
the contract-first discipline (an invariant not covered by an adversarial vector
is treated as unprotected).

## Architecture at a glance

```
Mission ─▶ CommandPlane.Enqueue ─▶ policy.Compose (stricter-only, enqueue-reject)
        ─▶ controller.Enqueue (freeze RunSeed) ─▶ eventlog: run.created (seq 0, THE truth)
        ─▶ controller.Drive ─▶ shim.Create (fail-closed isolation.Select + secret.Lease
             + redaction registration) ─▶ shim.Start ─▶ worker.Run(WorkcellAPI)
             │   every stdout/artifact/egress/tool call flows through the shim:
             │   redaction-before-persistence, default-deny egress, CAS pointers
        ─▶ shim.Reap (revoke leases) ─▶ run.finished
        ─▶ projections (Status/ArtifactManifest/WorkbenchDiff/RunSummary/RunMetrics)
             are PURE folds over the log — the controller keeps no mutable state.
ReadPlane.Subscribe replays + tails the log (pull-based, no backpressure).
Every command is one control event. Denials live only in the audit ledger.
```

## Notable engineering choices

- **The adapter contract is a Go interface mirroring `CellShimAdapter`**; a gRPC
  transport over unix-socket/vsock is an additive wrapper (the contract, not the
  transport, is what makes workers swappable — Invariant 1).
- **`run_id` is derived from the idempotency key**, so a duplicate `Enqueue` is
  idempotent structurally (a second `run.created` collides with `UNIQUE(run_id,
  seq=0)`) — no mutable idempotency cache (Invariant 2).
- **The composed `IsolationContract` is stored in the CAS under its own
  `isolation_contract_ref`**, so provisioning re-verifies the frozen contract
  rather than re-resolving it (ADR-009 R1).
- **The worker touches the world only through `WorkcellAPI`**, so the shim is the
  single choke point that enforces redaction, egress default-deny and artifact
  capture regardless of which worker runs.
