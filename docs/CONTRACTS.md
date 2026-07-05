# Arkwen Wire Contracts — `proto/arkwen/v1/`

**Status:** contract-first. This package is the single source of truth for every Arkwen wire surface and is frozen *before* Slice-0 code. Go is the primary implementation language; **protobuf is the only IDL**; JSON-Schema is a *mirror* (generated, never hand-edited); the event store is append-only PostgreSQL.

> Not the agent is the product — the factory is. The contracts are the spec surface that makes Arkwen swappable ("OCI for agents"): treat them like an API you can never quietly break.

---

## 1. Package overview

Four `.proto` files, one package `arkwen.v1`, split by contract *plane*:

| File | Plane | Purpose |
| --- | --- | --- |
| `common.proto` | shared | Every type reused across planes: `Digest`, `ContentRef`, `Principal`, `RunSeed`, `LifecycleStatus`, `IsolationContract`, `GateRule`, `Permission`, `Capability`, and all projection shapes (`ArtifactManifest`, `WorkbenchDiff`, `RunSummary`, `RunMetrics`). Nothing is defined twice anywhere else. |
| `events.proto` | truth | The append-only `EventEnvelope` (`run_id`, monotone `seq`, per-event `schema_version`, `timestamp`, `type`, `payload`), the full event taxonomy (lifecycle / semantic / control / health / security / `worker.raw`), and the two **non-run-scoped** ledger envelopes (`WarehouseLedgerEnvelope`, `ControlPlaneAuditLedgerEnvelope`) with their own `ledger_seq`. |
| `adapter.proto` | inner (Cell-Shim ↔ Factory Controller) | `CellShimAdapter`: the tiered adapter. MUST verbs (`Create`/`Start`/`Signal`(cancel)/`State`/`Reap` + `Artifacts`) are unconditional; `StreamEvents` + the `Negotiate`-declared KANN capabilities degrade *observability/control* only. Transport-agnostic (unix socket ‖ vsock). |
| `control_plane.proto` | outer (consumer ↔ Factory Controller) | `ReadPlane` (`Subscribe` + `GetProjection`) and `CommandPlane` (`Enqueue`/`Signal`/`ResolveGate`/`Reprioritize`/`SetFloor`). Consumer-agnostic: no consumer name, no callback/webhook, no `*_url`. Every command becomes exactly one control event. |

**One-sentence model:** `Mission → Factory Controller → Cell-Shim → Workcell → Event Stream → Projections`.

Three structural guarantees are built into the *shape* of `common.proto` (they cannot be violated by a conforming serializer, not merely "by convention"):

- **Pointer-only (Invariant 4):** there is deliberately **no `bytes` field** anywhere in the package for content. Content lives in the content-addressed store; the wire carries `ContentRef`/`Digest` pointers only.
- **Append-only (Invariant 2):** there is **no** Update/Delete/Edit/Tombstone message and **no** `revision`/`deleted` field anywhere. The only operation over truth is *append*; state is a projection.
- **Secret-tightness (Invariant 5):** no message carries secret material. `Principal` exposes only `type`/`principal_id`/`tenant_id` — auth material is verified-and-discarded at the boundary (structural exclusion), never a field.

---

## 2. Versioning & forward-compatibility

- **Package version = `v1`** (path `proto/arkwen/v1/`). A breaking change mints a new package (`v2`), never an in-place mutation of `v1`.
- **`schema_version` is PER EVENT** (`EventEnvelope.schema_version`, `uint32`; ledgers carry their own per-envelope `schema_version`). This is *the* forward-compat axis: within `v1`, changes are **additive only** (new optional fields, new enum members with values that preserve the fail-closed ordering, new `oneof` payload cases). A consumer that sees an unknown `schema_version` or an unknown enum member reads it fail-closed (see §3), never as a looser value.
- **Enum value `0` is the fail-closed sentinel** everywhere. Where a concrete restrictive value exists it *is* `0` (`TIMEOUT_POLICY_FAIL_CLOSED`, `EGRESS_ACTION_DENY`, `AUTHZ_OUTCOME_DENY`, `MATERIALIZATION_MODE_SNAPSHOT`). Where `0` is an `*_UNSPECIFIED` proto3-hygiene sentinel, validators **reject** it and projections read it as deny/fail — never allow/approve/looser. Ascending numeric order additionally encodes strictness lattices (`ISOLATION_PROFILE` `standard < hardened < strict`) so cross-layer composition is `max()` and can never downgrade.
- **New commands ⇒ new control events** (`RunReprioritized` is the template): the Command-Plane never grows a side-channel that bypasses the append-only stream.

---

## 3. JSON-Schema mirror & the proto3-JSON convention

JSON-Schema is **generated** from these `.proto` files (buf / `protoc` plugin) and is never hand-edited — the protobuf is authoritative. Both the mirror and the conformance vectors in `conformance/` use **canonical proto3 JSON** so a Go `protojson`-based runner can load a vector directly:

| proto | JSON |
| --- | --- |
| field names | `lowerCamelCase` |
| `int64`/`uint64`/`fixed64` (`seq`, `size_bytes`, `ledger_seq`, `amount_micros`, …) | **string** |
| `int32`/`uint32` (`schema_version`, `port`, `match_count`, `priority`, …) | number |
| enums | the **value NAME** as a string (e.g. `"EVENT_TYPE_RUN_CREATED"`, `"EGRESS_ACTION_DENY"`) |
| `google.protobuf.Timestamp` | RFC3339 string (`"2026-07-05T09:00:00Z"`) |
| `google.protobuf.Duration` | string with `s` suffix (`"3600s"`) |
| `oneof` member | appears under its own field name; the sibling discriminator (`EventEnvelope.type`) must agree |

Vectors are written **expanded** (defaults shown for readability). A byte-for-byte golden comparison must first canonicalize both sides through `protojson` (which omits defaults) — the assertions below are therefore stated **semantically**, not as raw-string equality, except where a literal-substring search is the point (redaction, Invariant 5).

---

## 4. The conformance suite

Files under `conformance/`:

| Vector | File | Primary invariants |
| --- | --- | --- |
| golden-run | `conformance/golden-run.json` | 2, 4, 6, 8 (+1, 3, 10) |
| adapter-tiers | `conformance/adapter-tiers.json` | 1, 9 |
| redaction | `conformance/redaction.json` | 5 (+3) |
| reproduce-determinism | `conformance/reproduce-determinism.json` | 6, 8 |
| fail-closed | `conformance/fail-closed.json` | 7, 10 (+2, 8) |
| suite index | `conformance/README.md` | traceability |

Each vector is self-describing: `vector`, `invariants`, `description`, `conventions`, the `given` data (proto3-JSON messages) and an `assertions` list of `{id, invariant, assert, expect}` predicates. An invariant that is not encoded as at least one **adversarial** assertion is treated as unprotected.

---

## 5. Invariant → contract → vector traceability

| # | Invariant | How the wire contract encodes / enforces it | Vector(s) |
| --- | --- | --- | --- |
| 1 | **Controller runtime-agnostic** | `adapter.proto`/`CellShimAdapter` exposes only generic verbs; `NegotiateResponse.worker_kind` is explicitly an *opaque diagnostic label, NOT a controller special-case*. No worker/consumer-named field exists on any verb or on `RunSpec`. | **adapter-tiers** (both `worker_kind` profiles pass the *identical* MUST suite); golden-run (`source`/`emitter` carry no product name) |
| 2 | **Append-only stream = sole truth; Controller = projection** | No update/delete/edit/tombstone message; no `revision`/`deleted` field. `EventEnvelope{run_id, seq}` with persistence-enforced `UNIQUE(run_id, seq)` + monotone `seq`. State is only ever a projection (`LifecycleStatus`, `ArtifactManifest`, …). Ledgers use their own `ledger_seq`, disjoint from the run stream. Denials live *only* in the audit ledger and never fabricate a run-stream event. | **golden-run** (seq monotone/unique, no mutate types, one terminal event); fail-closed (denial → ledger, not run stream) |
| 3 | **`worker.raw` never dropped except redacted secrets** | `EVENT_TYPE_WORKER_RAW = 50` + `WorkerRaw{channel, raw_ref}` = durable **sanitized** raw; the raw text lives in the CAS *after* redaction, referenced by pointer. | **redaction** (worker.raw present; only the secret span sanitized); golden-run (raw event present) |
| 4 | **Events = metadata + pointers only** | **No `bytes` content field anywhere.** `ContentRef` is *the* universal pointer; every content-bearing payload (`WorkerMessage.message_ref`, `WorkerToolCall.arguments_ref`, `WorkerToolResult.result_ref`, `WorkerArtifactWritten.artifact`, `WorkerRaw.raw_ref`, `GateResolved.resolution_payload_ref`, …) is a `ContentRef`/`Digest`. | **golden-run** (every content field is a `ContentRef`; artifact carries path·hash·size·mime·ref) |
| 5 | **Secrets never persisted; redaction in Cell-Shim *before* persistence; contract-plane = structural exclusion** | Cell-Shim redaction: `security.secret_leak_detected`/`redaction_applied` carry counts/channel/`rule_id` only. `SecretLeased.secret_scope_ref` is a *scope Digest*, never the credential. `RunSeed.secret_scope_set_ref` is the audit scope, never the secrets. Contract-plane: `ControlPlaneAuditLedgerEnvelope` + `Principal` have **no** token/credential field by construction (a *separate* mechanism from Cell-Shim redaction). No `bytes` anywhere ⇒ persist-before-redact is structurally impossible. | **redaction** (injected secret on **zero** persistent surfaces; bearer token absent from ledger; structural-no-field checks) |
| 6 | **Repro-seeds from day 1, frozen at `run.created`** | `RunCreated.seed: RunSeed` captured once at the `run.created` event (the freeze point); every seed field is a resolvable `Digest`/pin (channels/ranges resolved exactly once). `RunProvisioning.isolation.contract_ref == seed.isolation_contract_ref` (provisioning re-verifies, never re-resolves). `session_seed` is the frozen non-determinism axis. | **reproduce-determinism** (same seed → byte-identical resolved inputs); **golden-run** (full seed at seq 0, never re-carried) |
| 7 | **fail-closed default; higher layers only stricter; org + intrinsic floor non-removable** | Enum-0 = most restrictive (`TIMEOUT_POLICY_FAIL_CLOSED`, `EGRESS_ACTION_DENY`, `AUTHZ_OUTCOME_DENY`, `GATE_DECISION_UNSPECIFIED`→reject, `ISOLATION_PROFILE_UNSPECIFIED`→reject). `IsolationInputs` compose `profile=max()`, `egress=∩`, `resource=min()`, `image_trust=OR` across `PolicyLayerKind` `ORG>BLUEPRINT>MISSION`. The intrinsic floor is *not expressible* as a field — enforced by enqueue-reject. `SetFloor` sets only the org floor, above the intrinsic floor. | **fail-closed** (unresolved gate→reject; loosening RunSpec→enqueue-reject; enum-0 restrictiveness) |
| 8 | **`paused` = overlay, not terminal; terminals only completed/failed/canceled** | `LifecycleState` has **no** `PAUSED` member; suspension is the orthogonal `SuspensionReason` overlay (`RunPaused` event + `LifecycleStatus.suspension_reason`). `TerminalState` = `{COMPLETED, FAILED, CANCELED}` only; `isolation_unsatisfiable`/`resource_exhausted` are `TerminalReason`s and `DEGRADED` is a *result classification* — none is a state. | **reproduce-determinism** (`DEGRADED` ∉ any state enum); **golden-run** (terminal legality); fail-closed (reasons on `failed`) |
| 9 | **Tiered + graceful degradation; security floor never silently degrades** | `Capability` enum lists **only** KANN capabilities; the MUST verbs are unconditional and not listed. `ShimSignalResponse.degraded_to_boundary` marks pause-without-`CONTROL_PAUSE_RESUME` as *success* via boundary freeze. Absent `EVENTS_STREAM` ⇒ fold `worker.raw` (observability degrades). The `IsolationContract`/egress/secret floor is identical inputs regardless of declared capabilities. | **adapter-tiers** (MUST-only vs fully-cooperative; floor byte-identical across tiers) |
| 10 | **Outer loop only stricter; Arkwen depends on no consumer** | `control_plane.proto` is consumer-agnostic: no consumer name, no callback/webhook, no `consumer_*_url`; dependency is one-way. `ReadPlane.Subscribe` is pull/replay-based (no consumer backpressure on execution). Every command = one control event; a floor-loosening `RunSpec` is rejected at `Enqueue`, never silently corrected. | **fail-closed** (enqueue-reject on loosening; no consumer field structurally); golden-run (no consumer name in envelope) |
