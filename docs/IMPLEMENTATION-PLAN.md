# Arkwen — Synthesized Delivery Plan
### "Walking Skeleton on a Frozen Wire, Hard Risks Terminated Early"

**Source ADRs:** `/home/leon/workspaces/arkwen/docs/adr/ADR-001.md` … `ADR-010.md` (+ `ADR-009.md` reconciliations R1–R9). This plan implements them; it does not re-decide them.

---

## 0. How the three approaches combine (the synthesis rule)

The three inputs disagree on *what Slice 0 is*: a running product (Walking-Skeleton), a pile of frozen schemas with no runtime (Contract-First), or a Firecracker/vsock danger-spike (Risk-First). The guideline resolves this precisely, and this plan encodes that resolution:

- **Backbone = Walking-Skeleton.** Slice 0 is a *runnable product* — one Mission → one Production Run → `completed`. We get real feedback from commit one. We do **not** ship a schemas-only Slice 0, and we do **not** open with a Firecracker spike that could stall for weeks before any value is visible.
- **Contract-First becomes a per-slice discipline, not a front-loaded phase.** Every slice freezes the wire contract(s) it introduces as **versioned protobuf + mirrored JSON-Schema**, and ships **golden-vector conformance tests** as part of "done". The contract spine that Contract-First wanted all-at-once is instead grown slice-by-slice, but with the same rigor: an invariant that isn't encoded as an adversarial vector is treated as unprotected.
- **Risk-First becomes an ordering rule for the middle, not the opening.** The three *named* hard risks — **Isolation, Secret-Broker, `reproduce()`** — are pulled to Slices 2a, 2b and 3, immediately after the spine proves out. They are deliberately **not** deferred to the end (as a naive feature-value ordering would). The comparatively well-understood layers (Gates, Outer-Loop, AuthZ) follow.

**One structural payoff the synthesis buys for free:** because the real append-only event store exists from Slice 0 (Walking-Skeleton), redaction (Slice 2b) is verified against a *real persistent sink*, not a stub — which retires two of Risk-First's own worst risks (redaction-built-against-a-stub, and spike-events establishing bad precedents before the envelope is frozen).

**The "seam placement then fill" pattern (from Walking-Skeleton) is load-bearing here.** Slice 0 cuts every structural seam in the *correct architectural place*, and where a seam guards a live risk it is populated with real content from commit one: the redaction seam already protecting a real secret (the worker's minimal model-API credential); the full seed schema with `tenant_id`/`authz_policy_version`/`isolation_contract_ref` reserved as null; the versioned wire protocol; the fail-closed floor (runc-hardening + default-deny egress). Later slices **fill** these seams rather than re-cutting them — so Slice 6 lights up multi-tenancy with **no seed-schema migration**, and Slice 2b fills out full broker-fed redaction on a seam that already protects a real secret.

---

## 1. Recommended stack (one proposal)

**Primary language: Go. Single canonical IDL: protobuf (mirrored to JSON-Schema for golden vectors and non-proto consumers).**

Arkwen *is* a containerd/OCI/gRPC/event-sourcing system, and that entire ecosystem is Go-native:

- **Factory Controller, Cell-Shim, adapters, gate engine, Warehouse, gRPC contract plane → Go.** The Cell-Shim (ADR-001) *is literally* the containerd-shim-v2 pattern; building it in Go reuses real shim machinery instead of reinventing it. `runc`, `gVisor/runsc`, `firecracker-containerd` + `firecracker-go-sdk`, CNI, cgroups v2 libs, `google/nftables` (netlink), `grpc-go`, and Sigstore/`cosign`/in-toto are all Go-native. A single static binary makes the shim trivial to inject into a Worker Image.
- **One protobuf IDL for BOTH the adapter/shim wire protocol AND the outer-loop contract.** The contract is a *wire protocol, not an in-process Go interface* — this is the single most important stack decision, because it makes a polyglot worker (or a Rust strict-tier shim) additive rather than a rewrite. Per-event `schema_version` maps cleanly onto proto's additive forward-compat rules.
- **Event store: PostgreSQL append-only table** — monotone `seq` via identity/sequence, `UNIQUE(run_id, seq)` gives idempotent append + cheap replay; JSONB envelope carrying **pointers only**; LISTEN/NOTIFY or logical decoding for fan-out. Deliberately **not** Kafka/EventStoreDB at Slice 0. Kept behind a pluggable log interface so a later move to NATS JetStream/Kafka is insulated by the Envelope contract if fan-out throughput demands it.
- **Warehouse + Artifact Store: one content-addressed substrate** (ADR-007) fronted by OCI-distribution/ORAS — OCI semantics map 1:1 (immutable digests = exact aliases, movable tags = channels `dev/tested/released`) and reuse the Worker-Image provenance tooling. Blob backend behind an interface (registry / S3-compatible MinIO / filesystem).
- **Isolation: containerd with pluggable runtime handlers `runc` → `runsc` (gVisor) → Firecracker + jailer.** Transport = unix socket for runc/gVisor, `virtio-vsock` for the microVM. Host-side `nftables`/CNI per netns for default-deny egress; cgroups v2 direct.
- **Provenance/attestation: Sigstore/`cosign` + in-toto/DSSE.**
- **Secret-Broker: a separate control-plane service** (Controller never in the secret path — invariant 1), pluggable backend (Vault | cloud KMS/STS) behind an interface.
- **Selective Rust at exactly two boundaries where Go's GC is a *real* liability, never as the primary language:** (a) the **strict-tier in-guest shim fragment** on the microVM (tiny static binary, tight vsock handling, GC-pause-free deterministic redaction hot path — Firecracker itself is Rust); (b) optionally the **Secret-Broker** (reliable zeroization, no GC pinning of secret material). Start Go-only for velocity and one skill set; carve out Rust only when the strict tier's footprint/determinism or secret zeroization justifies it. The protobuf wire contract makes each carve-out additive.
- **No Kubernetes in the core.** The Controller stays runtime-agnostic; placement/scheduling (bare-metal/nested-virt for `strict`) is a separate, later concern.
- **Factory Floor / Control Room UI is deferred and off the critical path** — it is just another read-plane consumer of projections + replay (invariant 10). Any TS/React client over the gRPC read-plane works and names no privileged consumer.

---

## 2. Cross-cutting disciplines (apply to EVERY slice)

1. **Contract-first per slice.** Any wire surface a slice introduces (adapter verbs, event types, command ops, ledger envelopes, seed fields) is frozen as versioned protobuf + JSON-Schema *before* its reference implementation counts as done. Schema changes are additive; `schema_version` per event is the forward-compat axis.
2. **Conformance-as-guarantee.** Each invariant maps to ≥1 **adversarial** golden-vector (not a happy-path test). The ADR→invariant→vector traceability matrix is a living CI artifact, seeded in Slice 0.
3. **Invariants are continuous CI gates**, not slice-local checks. Once an invariant's test lands, it runs on every commit forever (see §3).
4. **Stubs are replaced, never rewritten.** A seam cut in Slice 0 (redaction, seed fields, intake gate) is *filled* later at the same architectural point.
5. **Fail-closed defaults from day 1**, with permissiveness expressed as policy *above* the floor — never by weakening the floor (invariants 7/9).

---

## 3. The 10 invariants as testable acceptance criteria

Each is a permanent CI conformance test from the slice where it first has teeth.

| # | Invariant | First enforced | Permanent testable acceptance criterion |
|---|-----------|----------------|------------------------------------------|
| 1 | Controller runtime-agnostic | Slice 0 → **confirmed Slice 1** | **grep-gate**: no worker-specific symbol (`claude*`, `openhands*`) in the controller package; **conformance**: a second, non-Claude stub adapter passes the *identical* lifecycle suite (Slice 1). |
| 2 | Append-only stream = sole truth; Controller = pure projection, no mutable shadow state | Slice 0 | **Replay-equivalence test**: drop the projection, rebuild from `seq 0`, assert byte-identical to live. Every command is a control-event; queue-ordering is a projection. |
| 3 | `worker.raw` never lost except redacted secrets | Slice 0 (minimal live credential) → **Slice 2b (full broker)** | **Fidelity-diff test**: every worker stdout byte appears in durable `worker.raw` *except* redacted spans. |
| 4 | Events carry metadata + pointers only | Slice 0 | **Adversarial vector**: an event carrying an inline blob is **rejected at append**; payload holds only `content_hash`/`artifact_ref`. |
| 5 | Secrets never in persistent logs/artifacts/UI; redaction in Cell-Shim before persistence (Contract-Plane: structural exclusion) | Slice 0 (seam + minimal live credential) → **Slice 2b (full broker)** + Slice 6 (contract plane) | **No-secret-material property test** against event store + artifact store + all projections: inject a known secret (the real injected model-API credential from Slice 0, generalized to broker-fed secrets in Slice 2b), assert it appears on **no** persistent surface; transient raw is ephemeral only. Contract-plane: AuthN material verified-and-discarded, only non-secret principal-id + decision + scope-refs are fields. |
| 6 | Repro-seeds from day 1, frozen at `run.created` | Slice 0 | **Seed-freeze-once test**: full seed schema present at `run.created`; any post-creation mutation fails; provisioning only re-verifies (R1). **reproduce-diff test** (Slice 3): a floating dependency makes it fail. |
| 7 | Security/gate default `fail_closed`; higher levels only stricter; org-floor non-removable; intrinsic floor never disable-able | Slice 0 (floor model: runc-hardening + default-deny egress) → Slice 2a (isolation ladder) → Slice 4 (gates) → Slice 6 (authz) | **Fail-closed default test** + **stricter-only merge test** + **enqueue-reject test**: a RunSpec loosening any floor (gate, isolation, or authz) is rejected at enqueue, never silently corrected. |
| 8 | `paused` = overlay, not terminal; terminals only completed/failed/canceled | Slice 1 | **Lifecycle-legality test**: `suspension_reason` is an orthogonal dimension; no transition into a terminal *from* paused; `isolation_unsatisfiable`/`resource_exhausted`/`DEGRADED` are reasons/classifications, not states. |
| 9 | Tiered + graceful degradation; security-floor degrades never silently | Slice 0 (default-deny floor) → Slice 1 (caps) → Slice 2a (isolation/egress parity) | **Degradation test**: an adapter without `control.pause_resume` still enters paused via boundary-freeze, and one without `events.stream` degrades to `worker.raw`-only folding (observability degrades, lifecycle holds). **Egress-parity test**: default-deny is identical across runc/gVisor/Firecracker (floor never diverges). |
| 10 | Outer loop only stricter, never looser; Arkwen depends on no consumer; no consumer name in the runtime | grep-enforced Slice 0 → **confirmed Slice 5** | **Consumer-agnosticism vector**: no consumer name, no outbound HTTP/webhook/callback, no consumer `*_url` config in the runtime. **No-backpressure test**: a slow/absent subscriber never blocks execution. |

---

## 4. The slice plan

### Slice 0 — First Light (Walking Skeleton + Contract Spine)
**Goal.** One Mission → one Production Run, `queued → completed`, single Claude Code Worker at the observational (MUST-only) tier. The append-only Event-Stream is the sole truth; the Factory Controller is a pure replay projection (zero mutable shadow state); Artifact-Manifest, Workbench-Diff and Run-Summary are derived; repro-seeds are frozen at `run.created` with the **full** seed schema (all future fields reserved). Because the S0 worker is a real Claude Code Worker that needs a live model-API credential, S0 ships the ADR-004 Phase-1 minimal secret path AND a fail-closed security floor from commit one — so the "no-secret-in-stream" property test protects a *real* secret and there is no open network path. Proves the whole factory spine end-to-end before any concern is thickened, and establishes the contract + conformance machinery.

**Deliverables.**
- **Event-Envelope** (protobuf + JSON-Schema): `run_id`, monotone `seq`, per-event `schema_version`, `timestamp`, `type`, `payload` (pointer-only); optional `source/emitter/correlation_id/causation_id`. `UNIQUE(run_id, seq)`; replay-from-`seq` API.
- **Event taxonomy v0**: lifecycle `run.created → provisioning → started → finished` + `worker.raw` (durable sanitized). The full enum (semantic/control/health/security) is reserved even if unused.
- **Adapter-Contract v0** as versioned protobuf over unix socket: MUST `create/start/signal(cancel)/state/reap` + `artifacts`. Observational tier only (no `events.stream` yet) — Controller polls `state` and synthesizes lifecycle events.
- **Cell-Shim v0** in the Workcell (containerd-shim-v2 pattern): speaks only the Arkwen protocol; launches Claude Code headless; captures stdout as `worker.raw`; computes the workbench-diff; collects Artifacts; reports state. **Redaction seam wired at the correct architectural point and populated with the worker's minimal live model-API credential (not an empty set)** — so the no-secret property test protects a REAL secret from commit one.
- **Minimal secret path (ADR-004 Phase-1)**: the Claude Code Worker's model-API credential is injected via **env at run-start** and **registered in the Cell-Shim redaction list** at launch. This is the Phase-1 minimal path only — the full Secret-Broker (dynamic scoped creds, rotation, revocation, auto-registration of arbitrary secrets) lands in Slice 2b.
- **Factory Controller = pure fold**: run-state + Run-Summary + Artifact-Manifest + Workbench-Diff rebuilt *only* by replaying the log; zero mutable shadow state.
- **Content-addressed Artifact Store** (sha256); events carry only `artifact_ref`/`content_hash`. (Begins the shared CAS substrate of ADR-007.)
- **Repro-seed bundle** frozen in the `run.created` payload with **all** seed fields first-class: `mission_hash, image_digest, toolkit_versions, workbench_base_commit, workbench_snapshot_ref|mount_ref, materialization_mode, adapter_version, policy_version, session_seed` — **plus reserved** `isolation_contract_ref, egress_policy_hash, resource_limits_hash, image_signature_ref, image_provenance_ref, secret_scope_set_ref, blueprint_digest, tenant_id, authz_policy_version` (default/null so later slices fill, not re-cut).
- `materialization_mode = snapshot` (canonical) as the Phase-1 default; `workbench_snapshot_ref` captured. Single `standard` isolation (runc); no profile switching.
- **S0 security floor (fail-closed from day 1).** The single `standard`/runc profile is hardened — **no-new-privileges, dropped caps, seccomp, read-only rootfs, non-root** — behind **host-side default-deny egress with a minimal static allowlist scoped to the model-API endpoint only** (the live credential's sole destination). No open network path. This makes discipline #5 (fail-closed from day 1) true for the very first runnable slice. The full policy-composed egress (toolkit∪blueprint∪mission ∩ ceilings − denies), `security.egress_denied` events, and cross-backend egress-parity land in Slice 2a; S0 ships only the minimal static floor.
- **Thin CLI** (`arkwen run create` / `arkwen events tail`) as a stand-in command+read plane, replaced by the ADR-008 gRPC contract in Slice 5.
- **Golden-vector conformance harness + CI gate**: replay-equivalence, pointer-only (inline-blob rejected), `worker.raw` preservation, seed-freeze-once, additive `schema_version`, no-secret property test (real minimal credential now, live), fail-closed floor (off-allowlist egress blocked). **ADR→invariant→vector traceability matrix** seeded.

**ADRs covered.** ADR-001 (MUST tier, Cell-Shim seat + wire protocol, seed capture), ADR-002 (event-sourcing core, Envelope, taxonomy skeleton, pointer-only), ADR-003 A+B partial (lifecycle top-state skeleton + Artifact-Manifest/Workbench-Diff/Run-Summary projections), ADR-004 (Workcell boundary, `materialization_mode` snapshot-first, content-addressed Artifact Store, redaction seam placement, **Phase-1 minimal secret path: env-injection + redaction-list registration**), ADR-006 partial (**S0 fail-closed floor: runc-hardening + minimal default-deny egress**), ADR-007 partial (shared CAS substrate begins), **ADR-009 R1** (freeze at `run.created`).

**Definition of Done / Verification.**
- `arkwen run create --mission … --worker claude-code --workbench ./src` drives a run to `completed`; the worker reaches the model API only via the allowlisted egress path; artifacts appear in the manifest; `arkwen events tail` replays the stream.
- CI **replay-equivalence** green (invariant 2). **Inline-blob rejected** at append (invariant 4). **Seed-freeze** enforced (invariant 6). **No-secret** harness live against real stores, protecting the REAL injected model-API credential — not an empty set (invariant 5 seam). **Fail-closed floor test**: default-deny egress blocks any off-allowlist destination and the runc profile is hardened (invariant 7 floor). **grep-gate**: no worker-specific symbol in controller (invariant 1 seed).

---

### Slice 1 — Runtime-Agnosticism, Full Lifecycle & Control Signals
**Goal.** Prove invariant 1 by driving a **second, non-Claude adapter** through the identical contract (contract falsified against reality before it hardens); complete the lifecycle (failed/canceled terminals + orthogonal `suspension_reason` overlay); make `signal(cancel)` work end-to-end; add `control.pause_resume` as a KAN capability with graceful degradation; and fill the **health** (`heartbeat`/`error`) and **semantic** (`events.stream`/`tools.structured`) event classes as KAN capabilities with graceful observational degradation.

**Deliverables.**
- **Full lifecycle projection**: `queued/provisioning/running/finalizing/completed/failed/canceled` + orthogonal `suspension_reason` overlay (`none/paused_by_gate/paused_by_user/paused_by_policy/paused_by_resource_limit`). `paused` is an overlay, never terminal.
- **`signal(cancel)` MUST path** end-to-end: `run.cancel_requested` → Cell-Shim → worker termination → reap → `canceled`.
- **`control.pause_resume` as a KAN capability** behind a capability-negotiation handshake; graceful degradation to **Workcell-boundary suspension** (container freeze) when absent — the paused overlay holds tier-independent, only resume-fidelity differs (ADR-008 E2).
- **`events.stream` + `tools.structured` as KAN capabilities** behind the same capability-negotiation handshake: when negotiated, the shim emits the **semantic event class** — `worker.message/tool_call/tool_result` (+ structured tool descriptors/results) — which the **Artifact-Manifest and Run-Summary projections fold**; when absent, graceful **observational degradation** to `worker.raw`-only folding (lifecycle and terminals hold, only semantic observability degrades). This exercises the semantic event class rather than leaving it merely reserved.
- **Health events + crash/timeout handling**: `heartbeat` (liveness) and `error` (crash detection) fill the health class of the S0 taxonomy — heartbeat gaps drive liveness detection; a worker crash/timeout surfaces as `error` → `failed` carrying the reap reason.
- **Stream delivery semantics**: at-least-once, seq-idempotent consumer, replay-from-`seq`, no producer backpressure.
- **Second STUB adapter** (echo / OpenHands-style) + a **Cell-Shim Conformance Kit** (test-double controller) that drives *any* shim through the full lifecycle, asserting envelope validity, `seq` monotonicity, `worker.raw` emission, capability negotiation, and graceful degradation.
- **Contract freeze**: control events (`run.paused/resumed/cancel_requested`), health events (`heartbeat/error`), the semantic event class (`worker.message/tool_call/tool_result`), and the capability-negotiation handshake (`control.pause_resume`, `events.stream`, `tools.structured`); vectors for cancel, pause-degradation, terminal-state legality, crash-via-`error`, and semantic-fold + observational degradation.

**ADRs covered.** ADR-003 (full top-states + suspension overlay + gate event-flow shape + semantic-event fold into projections), ADR-002 (control + health + semantic events + full delivery/replay semantics), ADR-001 (`signal(cancel)` MUST verified; `control.pause_resume`, `events.stream`, `tools.structured` KAN caps; tiered graceful degradation).

**Definition of Done / Verification.**
- The stub adapter passes the **identical** lifecycle conformance suite the Claude adapter passes (invariant 1, confirmed against reality).
- Cancel test → `canceled` + reap. Pause-degradation test: capability-less adapter still enters paused via boundary-freeze (invariant 9). Lifecycle-legality test: no terminal reached *from* paused; only completed/failed/canceled terminal (invariant 8). Replay-from-arbitrary-`seq`; concurrent subscribers seq-idempotent; execution never blocks on a slow consumer (invariant 2).
- **Crash-detection test**: an `error` health event / worker crash → `failed` with the reap reason, and a heartbeat gap is detected as liveness loss. **Semantic-fold test**: with `events.stream` negotiated, `worker.message/tool_call/tool_result` fold into Artifact-Manifest + Run-Summary; **observational-degradation test**: without it, the run still completes with `worker.raw`-only folding (invariant 9).

---

### Slice 2a — Isolation Ladder, Egress & Resource Limits  *(RISK-FIRST TERMINATION #1)*
**Goal.** Terminate the isolation risk and make the workload boundary real. Tiered isolation `standard(runc) < hardened(gVisor) < strict(Firecracker microVM)` by policy, **fail-closed, no auto-downgrade**; host-side policy-composed default-deny egress outside the workload trust boundary; cgroups v2 limits + budget-gate hook + hard-ceiling terminal reason; ephemeral overlay-FS. **This is where the architecture-defining bet — one shim over unix socket AND vsock — is proven, behind the already-frozen wire contract.** The S0 fail-closed floor (runc-hardening + minimal default-deny egress) grows here into the full policy-composed egress with parity across all three backends.

**Deliverables.**
- **Isolation profiles** as pluggable containerd runtime handlers: `standard`(runc; own namespaces, no-new-privileges, dropped caps, seccomp, read-only rootfs, non-root) < `hardened`(gVisor/runsc) < `strict`(Firecracker microVM + jailer). Transport = unix socket for runc/gVisor, `virtio-vsock` for the microVM — **same Arkwen protocol, proven identical over both transports.**
- **NO auto-downgrade** → fail-closed at provisioning (`reason=isolation_unsatisfiable`, a reason on `failed`, not a new state — invariant 8). Untrusted/multi-tenant → Org-Floor ≥ `hardened`, cross-tenant `strict`.
- **`isolation_contract_ref`** composed dimension-wise (profile=max, egress=∩, resource=min, image_trust=OR) at `run.created`; its **OWN** content-addressed seed (**not** `policy_version` — R2). Blueprint pins isolation-policy *inputs* stricter-only, not the finished contract.
- **`policy_bundle`** generalized to carry isolation dimensions per level (org/blueprint/mission); **enqueue-reject** on any floor-loosening or forced auto-downgrade (R3).
- **Host-side policy-composed default-deny egress** outside the workload trust boundary (netns for runc/gVisor, tap/vsock for Firecracker), growing the S0 static floor: effective = `(toolkit ∪ blueprint ∪ mission need) ∩ ceilings − denies`; raw-IP forbidden, DNS controlled; `security.egress_denied` (host/SNI redacted, length-bounded). **Broker channel = dedicated control-plane vsock path, reserved EXEMPT from the allowlist** (used by the Slice-2b broker — R4).
- **cgroups v2** limits (cpu/mem/disk/pids/wall-clock) → `resource.sample` + budget-gate hook; wall-clock counts running time only (suspension excluded). **Physical `org_cap` kept separate from consumer cost-budget** (R5). **Hard-ceiling breach → terminal reason `resource_exhausted`** (ADR-006 E3) — a reason on `failed`, not a new state (invariant 8).
- **Ephemeral overlay-FS** (read-only base + discarded writable upper); overlay-upper is *input* for `worker.artifact_written`; Workbench-Diff stays a fold over events (no FS read). `mount` only under `standard`+trusted; under hardened/strict fail-closed to snapshot (read-only via virtio-fs) — the ADR-004 mount restriction.
- **Egress-parity conformance suite** across runc/gVisor/Firecracker.
- **ADR-001 seed amendments filled**: `isolation_contract_ref`, `egress_policy_hash`, `resource_limits_hash`.

**ADRs covered.** ADR-006 (isolation profiles, egress, cgroups, overlay-FS, isolation-policy precedence, **E3 hard-ceiling `resource_exhausted`**), ADR-004 (mount restriction), ADR-001 (vsock transport, isolation/egress/resource seed amendments), **ADR-009 R2** (`isolation_contract_ref` own field) **/ R3** (`policy_bundle` isolation dims + enqueue-reject) **/ R4** (host-egress anti-exfil + broker-exempt path reserved) **/ R5** (physical `org_cap` vs cost-budget, limit side).

**Definition of Done / Verification.**
- **Egress-parity test**: off-allowlist connection blocked + `security.egress_denied` on every backend (invariant 9). **Fail-closed isolation test**: unsatisfiable profile → `failed reason=isolation_unsatisfiable`, no downgrade, no new state (invariants 7/8/9). **Floor-loosening enqueue-reject** (invariant 7). **Two-transport conformance**: the same adapter suite passes over unix socket AND vsock (strict tier is real). **Hard-ceiling test**: a cgroups breach terminates with `failed reason=resource_exhausted` (a reason, not a state — invariant 8).

---

### Slice 2b — Secret-Broker, Redaction & Image-Provenance  *(RISK-FIRST TERMINATION #2)*
**Goal.** Terminate the highest-blast-radius risk on its own verifiable increment. A control-plane **Secret-Broker** (separate from the Controller — invariant 1) that auto-feeds the Cell-Shim redaction list, completing `worker.raw` as durable sanitized raw so no secret reaches a persistent surface; worker-image provenance. **Redaction (invariant 5) lands here — decoupled from the Firecracker/vsock open decision in Slice 2a — generalizing the minimal live credential already redacted since Slice 0 to arbitrary broker-fed scoped secrets.**

**Deliverables.**
- **Secret-Broker**: control-plane service **separate from the Controller** (Controller never in the secret path — invariant 1); per-run ephemeral scoped capability handles/dynamic creds; injection env|tmpfs at start; rotation mid-run; leases survive suspension; **revocation at reap for every terminal incl. crash**; **auto-registers injected material into the Cell-Shim redaction list — generalizing the S0 single-credential path to arbitrary scoped secrets.** Uses the broker-exempt control-plane vsock channel reserved in Slice 2a (R4).
- **Redaction completed**: transient unfiltered raw only ephemeral in shim memory; persisted only after structural redaction. Events `security.secret_leased/rotated/revoked`, `security.redaction_applied/secret_leak_detected` (metadata only).
- **Worker-Image provenance** (Sigstore/`cosign` + SLSA, in-toto/DSSE) verified at provisioning; refs in seed/events. Phase-3 image source = base registry (R9), log-only enforce now.
- **ADR-001 seed amendments filled**: `image_signature_ref`, `image_provenance_ref`, `secret_scope_set_ref`.

**ADRs covered.** ADR-006 (Secret-Broker, provenance), ADR-004 (redaction complete, `worker.raw` = durable sanitized raw), ADR-001 (broker-fed redaction generalizes the S0 minimal path; image/secret seed amendments), **ADR-009 R4** (broker channel uses the reserved exempt path) **/ R9** (image source log-only).

**Definition of Done / Verification.**
- **No-secret-material property test now runs with a REAL broker-fed redaction list (multi-secret, generalizing the S0 single-credential guard) against the REAL event store + artifact store + all projections** (invariant 5, confirmed against reality — the highest-blast-radius invariant, real not stubbed). Persist-before-redact is structurally impossible/rejected.
- **Revocation test**: reap (incl. crash) revokes all leases. **Image-provenance verified** at provisioning (log-only enforce now; hard-enforce in Slice 3).

---

### Slice 3 — Warehouse & `reproduce()`  *(RISK-FIRST TERMINATION #3)*
**Goal.** Close the determinism loop. A content-addressed Warehouse (one CAS substrate, two catalogs), the Blueprint as the reproducible pin-point, and `reproduce(run_id)` resolving frozen seeds to byte-identical inputs; generic reference-based GC; its own Warehouse-Ledger envelope. Because `isolation_contract_ref` became a real seed in Slice 2a, the reproduce closure is **complete**.

**Deliverables.**
- **Warehouse = ONE CAS substrate + two catalogs**: Warehouse-Inputs (curated/signed/promoted worker images, toolkits, blueprints, Cell-Shim binary) and Artifact Store (run outputs + workbench snapshots). Shared blob substrate, separate namespaces + lifecycles.
- **Versioning**: digest (sha256) = sole identity; exact aliases immutable-by-policy; channels `dev/tested/released` = movable pointers; ranges never stored; alias/range→digest resolved exactly once at `run.created` (wired Slice 0), replay never uses a floating alias.
- **Blueprint** = reproducible run-template & sole pin-point: governance/security-floor fields (`gate_policy_set_ref`, isolation-policy inputs, `required_capabilities`) stricter-only; functional fields (`worker_image_digest, toolkits, materialization_mode`) mission-overridable only within `mission_interface` and only toward reproducible, each override re-pinned + seeded. `blueprint_digest` wired as a seed.
- **Blueprint-Manifest** self-describing content-addressed (`blueprint_digest = hash(manifest)`); provenance (in-toto/SLSA), signatures/attestations (Sigstore-style), optional SBOM; verified at intake AND re-verified at provisioning.
- **`reproduce(run_id)`**: resolve every seed against Warehouse/CAS → byte-identical inputs, incl. `adapter_version` (Cell-Shim binary as a Warehouse-Input) and `mission_hash` (content-addressed, secret-free mission body). Modes: `strict` (session_seed frozen) vs `re-run --fresh-seed`. Honest limits: input reproducibility only; `mount` not reproducible; **DEGRADED is a result classification, not a run state** (invariant 8); hosted-model best-effort seeded.
- **Generic GC**: a blob is immortal if ANY reference holds — transitive closure of ALL persisted seed-refs + channel pointers + event-stream pointers (`artifact_ref`/`worker.raw`) + artifact-manifest projection (not an enumerated subset — R7). Protects invariants 3/4/6.
- **Warehouse-Ledger envelope**: intake/promotion events are NOT run-scoped → own append-only envelope keyed on digest/channel with its own monotone `ledger_seq` (not `run_id`/`seq` — R8). Run-stream and Warehouse-ledger = two disjoint truth domains.
- **Promotion seam**: channels exist; the Intake Gate mechanism is a placeholder resolver (manual/direct promotion), **filled** by the gate spine in Slice 4 at `scope=promotion` — same discipline as Slice 0's redaction seam.

**ADRs covered.** ADR-007 (full: CAS substrate, versioning, Blueprint, manifest, `reproduce`, GC, Warehouse-Ledger; Intake-Gate seam), ADR-001 (seed resolution closes the loop; `mission_hash` + `adapter_version` resolvable), **ADR-009 R7** (generic GC), **R8** (own ledger envelope), **R9** (image source matures to Warehouse; provenance hard-enforce).

**Definition of Done / Verification.**
- **reproduce-diff test** in CI: strict mode yields byte-identical resolved inputs; a deliberately floating dependency (channel captured instead of digest, unpinned toolkit, timestamp/FS-ordering leak) makes it **fail** (invariant 6, reproduce risk terminated with a live guard).
- **GC test**: a blob referenced only transitively via a seed survives; dropping the last reference collects it (invariants 4/6). **Ledger-envelope test**: promotion events use `ledger_seq`, never `run_id`/`seq`; domains stay disjoint (invariant 2). **Mission-body secret-density** (invariant 5). **Immutability test**: exact alias can't be repointed; channel can.

---

### Slice 4 — Quality Gates (run / action + promotion intake)
**Goal.** One gate mechanism + swappable resolver, default `fail_closed`, stricter-only provenance Org>Blueprint>Mission — realized purely as control-events on the existing stream (no new state mechanism). Simultaneously **fills the Slice-3 Intake-Gate seam** by instantiating the same spine at `scope=promotion`.

**Deliverables.**
- **Gate spine**: one mechanism; resolver interface = Evaluator/auto (check-fn / second-worker / LLM-judge) and Human (routes via Controller to the Factory Floor). Flow `gate.requested → [run.paused] → gate.resolved → [run.resumed]`; Controller stays runtime-agnostic.
- **Gate object** `{gate_id, trigger, scope, source, mandatory, resolver/resolution_mode, timeout_policy, applies_to}`; trigger→`suspension_reason` mapping (auto→`paused_by_gate`, human milestone→`paused_by_user`, org rule→`paused_by_policy`, budget/resource→`paused_by_resource_limit`).
- **Scopes**: `action` (single tool-call/permission, PreToolUse-like, instantiated at runtime when an event matches `applies_to`) + `run` (milestone: deploy/merge/export/final write-back) + **`promotion`** (Warehouse Intake Gate — fills the Slice-3 seam, writes to the Warehouse-Ledger).
- **`timeout_policy` default `fail_closed`**; `fail_open` explicit per-gate opt-in; external resolver (`escalate`) MUST carry `max_wait` + inherits `fail_closed` (timeout→reject) — never unbounded hang on a consumer (ADR-008 E3).
- **Resolutions**: `approve/reject/approve_with_modification/escalate`; `gate.resolved` carries `resolved_by/decision/rationale/resolution_payload`.
- **Human-resolution channel (CLI stand-in)**: a minimal `arkwen gate resolve <gate_id> --decision approve|reject|approve_with_modification|escalate [--rationale …]` on the **S0 command-plane CLI stand-in**, writing `gate.resolved` as a control-event → `run.resumed`. This gives `paused_by_user` (human-milestone) gates a real resolution channel **in S4**; the full gRPC `resolve_gate` Command-Plane op lands in Slice 5 and replaces this stand-in (same seam→fill discipline as `arkwen run create` → gRPC).
- **Provenance**: Org-policy (non-removable floor) > Blueprint > Mission, stricter-only. `policy_version` = content-addressed digest of the materialized set (seed reserved since Slice 0). Rule-set deduplicated pre-start by `gate_id` with Org-floor; action-gates instantiated at runtime.
- **Intrinsic Arkwen floor** beneath the org-floor (redaction, `fail_closed` default, seed-capture) not disable-able even by the governance owner — a violating RunSpec is rejected at enqueue.

**ADRs covered.** ADR-005 (full: mechanism, resolver, scope, timeout, resolutions, provenance, materialization), ADR-007 E5 (Intake Gate filled at `scope=promotion`), ADR-003 (gate event-flow/overlay confirmed), ADR-008 E3 partial (external-resolver `max_wait` + `fail_closed` inheritance) + `resolve_gate` CLI stand-in (full Command-Plane op in Slice 5).

**Definition of Done / Verification.**
- **timeout→reject** under default `fail_closed` (invariant 7). **Stricter-only merge**: a Mission/Blueprint loosening an Org-floor gate is rejected (invariant 7). **Resolution replay-consistency**: gate outcomes are a pure projection (invariant 2). **escalate**: an external resolver without `max_wait` is rejected at materialization; with it, timeout→reject (invariant 7). **Promotion-gate test**: the same spine gates a `dev→tested` promotion to the Warehouse-Ledger (Slice-3 seam filled). **Action-gate** materializes at runtime on `applies_to` match. **Human-gate resolution test**: a `paused_by_user` gate is resolved via `arkwen gate resolve`, driving `gate.resolved → run.resumed` — human resolution is exercised in S4, not deferred to S5 (invariant 8: paused is an overlay, resolvable back to running).

---

### Slice 5 — Outer-Loop Contract Plane (gRPC Read/Command)
**Goal.** Expose the public, versioned, **consumer-agnostic** gRPC control plane: a Read-Plane (subscribe/replay + `get_projection`) and a Command-Plane (`enqueue`, cancel MUST / pause|resume KAN, `resolve_gate`, `reprioritize`) where every command is a control-event — strictly one-way consumer→Arkwen, with a best-effort Run-Metrics projection. Replaces the Slice-0 CLI and the Slice-4 gate-resolve stand-in with a thin client over this contract.

**Deliverables.**
- **gRPC service** (versioned protobuf; server-stream Read + unary Command), HTTP/SSE fallback. Wire = ADR-002 Envelope 1:1, forward-compat via per-event `schema_version`.
- **Read-Plane**: subscribe/replay from arbitrary `from_seq` (at-least-once, seq-idempotent, durable + fan-out, no consumer backpressure on execution) + `get_projection(run_id, kind)`; **projection snapshots for consumer cold-start**.
- **Command-Plane**: `enqueue`(idempotency_key)→`run.created`; cancel→`run.cancel_requested`; pause/resume (capability-aware, else boundary-freeze)→`run.paused/resumed`; `resolve_gate`→`gate.resolved` (replaces the S4 `arkwen gate resolve` CLI stand-in); `reprioritize`→`run.reprioritized` (queued-only, doesn't change inputs). **Every command IS a control-event** (invariant 2); queue-ordering stays a projection.
- **Strict unidirectional dependency** enforced structurally: Arkwen never calls out — no callback/webhook, no consumer `*_url` config; **no consumer name anywhere in the runtime** (invariant 10).
- **Governance-split**: the governance-plane owner (an Outer Loop if present, else Control Room/Operator — no consumer exclusive) sets the org-floor; Arkwen materializes & enforces; intrinsic floor via enqueue-reject, not disable-able. `version_constraint` (range) resolved against the Warehouse → concrete version/`toolkit_versions`/`image_digest` seeded at `run.created` (R1).
- **Run-Metrics projection (4th)**: `cost`, `duration` (incl. `suspended_ms_by_reason`), `gate_outcomes`, `terminal`, `artifact_signals` — pure fold, best-effort, tier-degrading; missing signals absent, never fabricated. **Arkwen measures objective signals, the consumer judges.** `artifact_signals` normalization across worker types.
- Budget exhaustion → `paused_by_resource_limit` + consumer topup via resume (**topup lifts only the consumer cost-budget, never the physical `org_cap`** — R5).
- Replace the Slice-0 CLI (`arkwen run create`/`events tail`) and the Slice-4 `arkwen gate resolve` stand-in with a thin client over this contract; ELIO stays strictly an external reference-consumer doc (`docs/ELIO-reference-consumer.md`), never a dependency.

**ADRs covered.** ADR-008 (full: unidirectional dependency, Read+Command plane, governance-split, Run-Metrics, delivery semantics, gRPC transport), ADR-003 (Run-Metrics = 4th projection), ADR-002 (stream as external contract surface; command→control-event incl. `run.reprioritized`), ADR-005/008 E3/E4 (governance-plane org-floor source), **ADR-009 R5** (org_cap vs cost-budget on topup).

**Definition of Done / Verification.**
- **Consumer-agnosticism vector**: grep the runtime — no consumer name, no outbound HTTP/callback/webhook, no consumer `*_url` (invariant 10). **One-way dependency**: with no consumer connected, execution reaches terminal; a slow/absent subscriber never blocks (invariant 10, no backpressure). **Command-as-event**: every command yields a control-event with `source/emitter`; queue-ordering reconstructable by replay (invariant 2). **Run-Metrics best-effort**: a degraded tier yields absent (not fabricated) signals (invariant 9). **Idempotent enqueue**: duplicate key → one run. **Topup**: lifts cost-budget only, never `org_cap` (R5, invariant 7).

---

### Slice 6 — Contract-Plane AuthZ & Multi-Tenancy
**Goal.** Lock down the contract plane: principals + pluggable AuthN, tenants (`tenant_id` seed-fest, cross-tenant default-deny **forcing `isolation_profile ≥ strict`**), least-privilege permissions, run-scoping, and a control-plane audit ledger — AuthZ **AND-composed ABOVE** the intrinsic floor. Because `tenant_id` + `authz_policy_version` were reserved seeds since Slice 0, this lights up with **no seed-schema migration**.

**Deliverables.**
- **Principals**: external Outer-Loop-Consumer (machine) / Operator (human via Control Room) / CI automation; pluggable AuthN (mTLS/OIDC/signed tokens) required on **every** read/command op. Internal Cell-Shim/Secret-Broker are intra-trust, not part of external AuthZ. Consumer-agnostic.
- **Tenancy**: tenant = isolation unit; each run belongs to exactly ONE tenant; `tenant_id` in RunSpec, frozen in seed at `run.created`; cross-tenant **default-deny**; **bridge to ADR-006 — cross-tenant co-residency forces `isolation_profile ≥ strict`** (separate mechanism; outer AuthZ informs inner isolation).
- **Permission-set** (least-privilege, scoped per `(principal, tenant, action, run-selector)`): `runs:enqueue`, `runs:read`/`stream:subscribe` (covers `get_projection` + Run-Metrics), `runs:signal:cancel|pause|resume`, `gates:resolve`, `runs:reprioritize`, `policy:set_floor`, `warehouse:promote`. **read ≠ write**; `gates:resolve` and `policy:set_floor` are separate high-trust grants, never implicitly bundled. `policy:set_floor` sets ONLY the org-floor, strictly ABOVE the non-removable intrinsic floor.
- **Run-scoping** via selectors: tenant / blueprint / own-runs / label. A subscriber can get `runs:read` without a command grant (pure observer). Queue-ordering stays projection.
- **Default-deny/fail-closed**: any request without a matching grant → deny; ambiguous → deny; a missing/degraded AuthN backend NEVER opens access (invariant 7).
- **AuthZ orthogonal to + ABOVE the intrinsic floor** (AND-composed): a fully authorized principal whose RunSpec loosens the floor or forces auto-downgrade is still **rejected at enqueue** (invariants 7/10).
- **`authz_policy_version`**: declarative + content-addressed versioned (own field, no `policy_version` overload — R2); changes = ledger entries → projection (no mutable shadow state); effective version seeded at `run.created`.
- **Control-Plane Audit Ledger** (non-run-scoped): AuthN/AuthZ decisions (esp. denials) auditable; own envelope (R8 pattern), keyed principal/time, own `ledger_seq`. **Denials never fabricate run-stream events** (invariant 2). Secret-density = **structural exclusion at the AuthN boundary** (AuthN material verified-and-immediately-discarded; only non-secret principal-id + decision + content-addressed scope-refs are ever fields) — **its own persistence guard, separate from Cell-Shim redaction** (invariants 5 + 1).
- **Standalone mode**: Operator-principal + default-single-tenant, minimally configured (invariant 10). Multi-outer-loop: tenancy is the isolation boundary (loop A can't touch loop B's runs).

**ADRs covered.** ADR-010 (full: principal model, tenancy, permission-set, run-scoping, default-deny, AuthZ-above-floor, `authz_policy_version`, Control-Plane Audit Ledger, standalone/multi-loop), **ADR-009 R6** (AuthZ as its own ADR — closed), ADR-008 E7 (minimal floor → full model), ADR-001 (`tenant_id` + `authz_policy_version` as frozen seeds — reserved since Slice 0), ADR-006 (cross-tenant → `isolation ≥ strict` bridge).

**Definition of Done / Verification.**
- **Default-deny**: no matching grant → deny; ambiguous → deny; degraded AuthN backend denies all (invariant 7). **AuthZ-above-floor**: authorized principal loosening the floor still rejected at enqueue (invariants 7/10). **Cross-tenant isolation**: co-residency forces `isolation ≥ strict`; cross-tenant read denied by default (ADR-006 bridge). **Audit-ledger secret-density**: AuthN material never persists; denials appear ONLY in the ledger, never as run-stream events (invariants 5/2/1). **Seed-continuity**: `tenant_id`+`authz_policy_version` populate the *same* fields reserved in Slice 0 — no migration (invariant 6). **Least-privilege**: `gates:resolve` / `policy:set_floor` not implicit with `runs:read` (read≠write).

---

## 5. Complete ADR → Slice traceability (nothing falls through)

| ADR / Reconciliation | Slice(s) | Landing |
|---|---|---|
| **ADR-001** Adapter-Contract, Cell-Shim, seeds | **0** (MUST tier + seat + seed capture + Phase-1 minimal secret path) · 1 (cancel/pause + `events.stream`/`tools.structured` KAN caps, degradation) · 2a (vsock transport, isolation/egress/resource seeds) · 2b (broker-fed redaction, image/secret seeds) · 3 (seed resolution) · 6 (`tenant_id`/`authz_policy_version` seeds) | frozen S0, filled through S6 |
| **ADR-002** Event-Sourcing, Envelope, taxonomy, delivery | **0** (core, pointer-only) · 1 (control + health + semantic events, full delivery) · 5 (external contract surface) | S0 |
| **ADR-003** Lifecycle + suspension overlay + projections | **0** (skeleton + 3 projections) · 1 (full states + overlay + semantic-event fold) · 5 (4th Run-Metrics) | S0→S5 |
| **ADR-004** Workcell boundary, materialization, CAS artifacts, redaction | **0** (boundary, CAS, redaction seam + minimal live credential) · **2a** (mount restriction) · **2b** (redaction complete) | S2b |
| **ADR-005** Quality-Gate policy model | **4** (full incl. human-resolve CLI stand-in) | S4 |
| **ADR-006** Isolation, egress, cgroups, Secret-Broker, provenance | **0** (fail-closed floor: runc-hardening + minimal default-deny egress) · **2a** (isolation, egress, cgroups, overlay-FS, `resource_exhausted`) · **2b** (Secret-Broker, provenance) | S2a/S2b |
| **ADR-007** Warehouse, Blueprint, `reproduce`, GC, Intake Gate | **0** (CAS substrate begins) · **3** (full) · 4 (Intake Gate filled) | S3 |
| **ADR-008** Outer-Loop Contract, Run-Metrics, governance-split | 4 (E3 external-resolver partial + `resolve_gate` CLI stand-in) · **5** (full) | S5 |
| **ADR-009 R1** freeze at `run.created` | **0** | S0 |
| **ADR-009 R2** `isolation_contract_ref` own field / `authz_policy_version` own field | **2a** (isolation) · 6 (authz) | S2a/S6 |
| **ADR-009 R3** `policy_bundle` isolation dims + enqueue-reject | **2a** | S2a |
| **ADR-009 R4** in-guest hygiene vs host-egress anti-exfil; broker channel exempt | **2a** (egress anti-exfil, broker-exempt path reserved) · **2b** (broker channel uses it) | S2a/S2b |
| **ADR-009 R5** physical `org_cap` vs consumer cost-budget | **2a** (limit) · 5 (topup) | S2a/S5 |
| **ADR-009 R6** AuthZ as its own ADR (→010) | **6** | S6 |
| **ADR-009 R7** generic reference-based GC | **3** | S3 |
| **ADR-009 R8** Warehouse-Ledger + Audit-Ledger own envelopes | **3** (warehouse) · 6 (audit) | S3/S6 |
| **ADR-009 R9** Phase-3 image source = base registry → matures to Warehouse | **2b** (log-only) · 3 (hard-enforce) | S2b/S3 |
| **ADR-010** Contract-Plane AuthZ & Multi-Tenancy | **6** (full) | S6 |

All ten ADRs and all nine R-reconciliations map to a landing slice.

---

## 6. Sequencing rationale

- **S0 before everything**: a running product + the contract/conformance machinery + the replay-equivalence guard is the cheapest possible insurance against invariant-2 erosion under later performance pressure. Because S0's real Claude Code Worker carries a live model-API credential, S0 also ships the Phase-1 minimal secret path (env-injection + redaction registration) and a fail-closed floor (runc-hardening + default-deny egress) so the first runnable slice honors discipline #5 and the no-secret test guards a real secret.
- **S1 immediately after**: locking invariant 1 with a *second real adapter* is a cheap forcing function; deferring it risks Claude-specific assumptions bleeding into the Controller before the second runtime is even attempted. S1 also fills the health + semantic event classes so they get real exercise rather than sitting merely reserved.
- **S2a + S2b + S3 = risk-first termination.** Isolation (S2a), Secret-Broker/redaction (S2b) and `reproduce()` (S3) are the design's genuine unknowns and its highest-blast-radius invariant (5). Terminating them *here* — against a real event store, not stubs — is the core of the synthesis. **The S2 split puts redaction (invariant 5) on its own verifiable increment (S2b) so it no longer hangs on the Firecracker/vsock open decision (S2a).** S2a→S2b→S3 so `isolation_contract_ref` is a real seed and the reproduce closure is complete.
- **S4→S6 = the well-understood layers**, in dependency order: Gates (S4) fill the Warehouse Intake seam and depend on the lifecycle/overlay (S1) + Warehouse-Ledger (S3); the Outer-Loop plane (S5) exposes the now-stable stream + projections; AuthZ (S6) sits above a fully-formed floor and lights up seeds reserved since S0.

---

## 7. Key risks & mitigations (carried from all three approaches)

1. **Strict-tier (Firecracker/vsock) front-loaded in S2a is skill- and environment-sensitive** (needs bare-metal/nested-virt; dev/CI may be WSL2 without nested virt). *Mitigation*: ship `standard(runc)` + the tiered fail-closed negotiation machinery first *within S2a*; treat Firecracker as an additive containerd runtime handler behind the already-frozen wire contract — a plug-in, not a rewrite. Build the placement/scheduling + gVisor-compat catalog as explicit deliverables so fail-closed rejection is predictable. **This is why `strict`-in-S2a vs `strict`-deferred is an open decision (§8).**
2. **Redaction is the highest-blast-radius invariant.** *Mitigation* (synthesis advantage): the real event store exists from S0, so S2b's redaction is verified against a real sink, not a stub; the no-secret property test is live from S0 — protecting a *real* minimal credential, not an empty set — and gates every commit.
3. **Controller-as-pure-projection erodes under performance pressure** (caches drift into authoritative state; non-deterministic payloads). *Mitigation*: replay-equivalence test from S0 (permanent), pointer-only payloads, seed-freeze at `run.created`.
4. **Reproducibility is only as strong as its weakest unpinned input**, and a late-discovered nondeterminism source forces a schema change on an immutable seed envelope. *Mitigation*: the **full** seed schema is reserved in S0; seeds are digests only (never channel names); reproduce-diff test guards S3; generic GC (R7) keeps the transitive closure immortal.
5. **Transport-abstraction leak**: proving socket+vsock with MUST verbs only (S2a) can hide protocol assumptions that break when KAN tiers (`events.stream`, `gate.interactive`) arrive. *Mitigation*: the capability-negotiation handshake is frozen in S1 (and exercises `events.stream`/`tools.structured`) and vectored; vsock framing/flow-control is exercised by the conformance kit, not just happy-path.
6. **Fail-closed-everywhere vs. a runnable skeleton.** *Mitigation*: model the intrinsic + org floor explicitly from S0 with permissive choices expressed as policy *above* the floor, never by weakening defaults — the S0 runc-hardening + default-deny egress floor makes this concrete from commit one.
7. **Go GC vs. secret zeroization** (invariant 5 at the memory level). *Mitigation*: Secret-Broker behind an interface; carve it (and the strict-tier in-guest shim) into Rust when the strict tier justifies it — additive over the protobuf wire.

---

## 8. Open decisions — see the structured `open_decisions` list.

These are implementation/operational choices (per ADR-009 closure, no core concern is missing); the biggest lever is the `strict`-tier timing in Slice 2a.
