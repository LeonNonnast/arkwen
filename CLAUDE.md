# CLAUDE.md — Arkwen

Guidance for any AI/human contributor working in this repo. **Read this before changing anything.**

## What Arkwen is
A **factory-style runtime for autonomous software work** — it orchestrates isolated workcells so that *any* worker (Claude Code, OpenHands, a Docker agent, …) produces software artifacts reproducibly, isolated, and observed. Goal: a standardized runtime layer for AI agents — **"OCI for agents"**.

> Not the agent is the product — the factory is.

**Arkwen is complete standalone.** An outer loop (in our product family: ELIO) is an *optional consumer of a public contract*, never a dependency. No named consumer is baked into the runtime.

**Status:** conceptual design complete (ADR-001…010, all phases closed + reconciled). **Contract-first** phase in progress (`proto/`). **No product code yet.**

One-sentence model: `Mission → Factory Controller → Cell-Shim → Workcell → Event Stream → Projections`

## Domain vocabulary — use these words
| Technical | Arkwen |
| --- | --- |
| Prompt | Mission |
| Session | Production Run |
| Agent | Worker |
| Docker Container | Workcell |
| Controller | Factory Controller |
| Web UI | Factory Floor / Control Room |
| Skills | Toolkits |
| MCP Server | Tools |
| Files | Materials |
| Output | Artifacts |
| Template | Blueprint |
| Docker Image | Worker Image |
| Queue | Production Queue |
| Workspace | Workbench |
| Approval | Quality Gate |
| Registry | Warehouse |

The **Cell-Shim** is the adapter that runs *inside* the workcell next to the worker.

## The 10 invariants — NON-NEGOTIABLE
Every change (contract, code, ADR) MUST be checked against these. Loosening any is a defect; if a change seems to require it, stop and write/update an ADR instead.

1. **Controller is runtime-agnostic** — worker/consumer specifics live in the Cell-Shim, never in the Controller.
2. **The append-only event stream IS the truth** — the Controller is a projection over it. No mutable shadow state. No update/delete semantics on the log.
3. **`worker.raw` is never dropped** — except redacted secrets (durable *sanitized* raw).
4. **Events carry only metadata + pointers** (`ContentRef`), never large payloads/contents.
5. **Secrets never enter persisted logs/artifacts/UI** — redaction happens in the Cell-Shim *before* persistence; the control-plane audit ledger uses *structural exclusion* at the AuthN boundary (a separate mechanism — do not conflate).
6. **Reproducibility seeds are recorded from day 1**, frozen at `run.created`.
7. **fail-closed by default** — security/gate defaults deny (enum 0 = most restrictive); higher policy layers may only make things *stricter*; the org-floor and the Arkwen-intrinsic floor are never removable.
8. **`paused` is an overlay, not a terminal state** — terminal states are only `completed / failed / canceled`.
9. **Tiered integration + graceful degradation** — a MUST contract + declared KANN capabilities; the *security* floor never degrades silently, only *observability* does.
10. **An outer loop may only make Arkwen stricter, never looser** — Arkwen depends on no consumer; no consumer name/field appears in the runtime.

## Contract-first — the discipline
`proto/arkwen/v1/` is the **single source of truth** for all wire contracts (protobuf; JSON-Schema is *mirrored/generated*, never hand-edited). Change the `.proto` + `docs/CONTRACTS.md` + the conformance vectors in `conformance/` **before or with** any implementation — not after. The contracts are the "spec surface" that makes Arkwen swappable; treat them like an API you can never quietly break (`schema_version` per event = forward-compat axis).

Contract surfaces: `common` (shared types: `ContentRef`, `RunSeed`, `Principal`, …), `events` (envelope + taxonomy + ledger envelopes), `adapter` (Cell-Shim ↔ Controller, tiered), `control_plane` (outer-loop Read-/Command-plane, consumer-agnostic).

## Confirmed stack
- **Go** primary; **protobuf** as the single IDL; **PostgreSQL** append-only event store behind a pluggable log interface.
- Content-addressed **Warehouse + Artifact Store** on an OCI-distribution/ORAS substrate.
- **containerd** runtime handlers: `runc` → `gVisor (runsc)` → `Firecracker` (+ jailer); transport `unix socket` / `virtio-vsock`.
- Host-side `nftables` egress.
- **strict tier (Firecracker)** is wired but **fail-closed until a capable host** (bare-metal/nested-virt) exists — `standard` + `hardened` ship first. Dev on WSL2 typically lacks nested virt.
- Rust carve-out (strict-tier in-guest shim / secret-broker) is an option later; protobuf wire makes it additive.

## Repo map
- `docs/adr/ADR-001…010.md` — the canonical (condensed) architecture decisions. **Start here.**
- `docs/adr/long-form/` — deep-dive versions (full threat-models, field schemas).
- `docs/adr/ADR-009.md` — cross-phase reconciliation (R1…R9); resolves seams between sibling ADRs.
- `docs/ARCHITECTURE-OVERVIEW.md` · `docs/PHASE-MAP-CLOSURE.md` · `docs/ELIO-reference-consumer.md`
- `docs/IMPLEMENTATION-PLAN.md` — slice-based build plan: **S0 · S1 · S2a · S2b · S3 · S4 · S5 · S6** (walking-skeleton backbone + contract-first discipline + risk-first termination).
- `proto/arkwen/v1/` — wire contracts (source of truth). `docs/CONTRACTS.md` — the guide. `conformance/` — golden vectors + conformance suite.

## Working conventions
- **Decisions are ADRs.** A new architectural decision gets a new ADR; a cross-ADR seam gets a reconciliation entry (see ADR-009). Don't silently loosen a prior ADR — supersede it explicitly.
- **Build order follows the slice plan.** S0 is a runnable walking skeleton (one mission → one run, event-stream = truth) *with the ADR-004 minimal secret path + runc hardening + default-deny egress from day 1* — never ship a runnable slice with an open network path and a live credential.
- **The 10 invariants are testable acceptance criteria** — e.g. "no secret in the persisted stream" is a conformance test, live from S0.
- **Commits:** end messages with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Commit/push only when asked. `docs:` / `feat:` / `fix:` prefixes.
