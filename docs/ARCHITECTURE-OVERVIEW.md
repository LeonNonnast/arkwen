# Arkwen — Architecture Overview

Konsolidiert ADR-001…009 zu einer erzählbaren Runtime-Story + der **consumer-agnostischen Outer-Loop-Grenze**.

Arkwen is a factory-style runtime for autonomous software work. It orchestrates isolated workcells, observes the full event stream, and materializes projections such as artifacts, gate audits, and reproducibility seeds. **It is complete and useful standalone**; any outer loop is an optional consumer of a public contract, never a dependency.

## One-sentence model
**Mission → Controller → Cell-Shim → Workcell → Event Stream → Projections**

## Arkwen is responsible for
- Starting and supervising production runs
- Bridging missions into isolated workcells
- Capturing events, artifacts, gate decisions, and seeds
- Preserving reproducibility and auditability
- Exposing a stable run/session model independent of the inner worker runtime
- Exposing a **public, versioned outer-loop contract** (read-plane + command-plane)

## Arkwen is NOT responsible for
- Designing the worker's inner loop
- Owning org-wide business-level governance
- Versioning strategic mission portfolios
- Being (or privileging) any specific outer loop / higher-level orchestrator

---
## Outer-loop seam (consumer-agnostic)
Arkwen exposes a public contract that *any* outer loop consumes. No named consumer is baked into the runtime.

- **Outer loop → Arkwen (down):** missions to execute · policies & gate requirements · blueprint refs + version constraints · priority/ordering · org constraints & compliance floors.
- **Arkwen → outer loop (up):** append-only event stream · artifacts + artifact refs · gate audit trail · reproducibility seeds + materialization refs · run terminal state + projections (incl. run-metrics).
- **Boundary rule:** the outer loop may make Arkwen **stricter, never looser**; Arkwen stays the execution boundary and does not absorb governance (Arkwen measures objective signals, the consumer judges). Dependency is strictly one-way: the consumer subscribes + enqueues; Arkwen never calls out.

*(Concrete filling in our product family: ELIO = outer/inter-session loop, Vela = inner/intra-session — see `ELIO-reference-consumer.md`. The runtime names neither.)*

---
## Factory pipeline
```
Mission → Factory Controller → Cell-Shim (adapter/boundary) → Workcell (isolated worker runtime)
  → Append-only Event Stream → Projections {status · artifact manifest · workbench diff · gate audit · reproducibility record · run metrics}
```
- **Controller** owns run lifecycle, projections, policies, orchestration state — runtime-agnostic.
- **Cell-Shim** at the edge of the workcell: translates runtime behavior into the Arkwen protocol; normalizes events, gates, artifacts, redaction.
- **Workcell** isolated execution env (isolation profiles standard/hardened/strict); hosts Claude Code / OpenHands / Docker Agent / future adapters.
- **Event Stream** IS the run — append-only source of truth, and the public read-plane.
- **Projections** derived views for UI / any consumer.

---
## Commitments (Phases 1–5, all closed)
- **Integration:** tiered · cooperative where supported · observational fallback · `worker.raw` preserved.
- **Lifecycle:** canonical states derived from stream · control overlays for pause/resume + gates · append-only control events.
- **Reproducibility seed:** mission_hash · image_digest · toolkit_versions · workbench_base_commit · workbench_snapshot_ref/mount_ref · materialization_mode · adapter_version · policy_version · session_seed · blueprint_digest · isolation_contract_ref · image_signature/provenance refs.
- **Isolation & secrets:** tiered isolation profiles · default-deny egress · secret broker with ephemeral scoped handles · redaction as persistence guard.
- **Warehouse & reproducibility:** content-addressed store · blueprint as pin-point · reproduce(run_id).
- **Outer-loop contract:** public read-plane + command-plane · one-way dependency · intrinsic floor enforced at enqueue.

---
## Design principles
- Keep the controller runtime-agnostic.
- Preserve fidelity before interpretation.
- Prefer append-only truth over mutable shadow state.
- Make gates explicit and auditable.
- Store artifact payloads separately from events.
- Treat secrets as boundary data, not log data.
- The security floor never degrades silently (only observability does).
- **The outer-loop seam is a public contract — no named consumer is privileged, and Arkwen is complete standalone.**

---
## Stakeholder readout
Arkwen is the execution layer that runs work safely and reproducibly inside isolated workcells. An outer loop (in our product family: ELIO) decides what runs, under which policies, and with which blueprint constraints — but Arkwen depends on none, and the event stream is the durable truth from which every view is projected.
