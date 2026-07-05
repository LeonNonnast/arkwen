# Arkwen conformance suite (`conformance/`)

Adversarial golden vectors that pin the ten Arkwen invariants to the frozen wire
contract in `proto/arkwen/v1/`. An invariant that is not covered by at least one
vector here is treated as **unprotected** and is a CI failure.

## Vectors

| File | Vector | Invariants |
| --- | --- | --- |
| `golden-run.json` | full `queued → completed` event stream | 2, 4, 6, 8 (+1, 3, 10) |
| `adapter-tiers.json` | MUST-only (observational) vs fully-cooperative + graceful degradation | 1, 9 |
| `redaction.json` | injected secret never persisted; audit-ledger structural exclusion | 5 (+3) |
| `reproduce-determinism.json` | same seed → byte-identical inputs; DEGRADED classification | 6, 8 |
| `fail-closed.json` | unresolved gate / missing grant / cross-tenant → deny | 7, 10 (+2, 8) |

## Encoding — canonical proto3 JSON

Vectors are authored so a Go `protojson`-based runner can `Unmarshal` the `given`
messages directly against the generated types:

- field names `lowerCamelCase`;
- 64-bit ints (`seq`, `sizeBytes`, `ledgerSeq`, `amountMicros`, …) are **strings**;
- 32-bit ints (`schemaVersion`, `port`, `matchCount`, `priority`) are numbers;
- enums are their **value NAME** string (`"EGRESS_ACTION_DENY"`);
- `Timestamp` = RFC3339 string; `Duration` = `"<n>s"`;
- a `oneof` member appears under its own field name and the discriminator
  (`EventEnvelope.type`) must agree.

Vectors are written **expanded** (defaults shown). Golden byte-comparison must
first canonicalize both sides through `protojson` (which omits defaults); the
`assertions` are stated **semantically** except where a literal substring search
is the whole point (redaction / Invariant 5).

All `Digest.hex` values are illustrative 64-char sha256 placeholders — a runner
treats them as opaque identities (content-addressing: equal hex ⇒ equal bytes).

## Runner contract

For each vector the runner:
1. `Unmarshal`s every message in `given` against `arkwen.v1` (must succeed).
2. Evaluates every `assertions[].assert` predicate and compares to `expect`.
3. Fails the build if any assertion fails, or if any message that the vector
   marks `mustReject` unmarshals-and-validates as accepted.

Structural assertions ("field X does not exist on message Y") are checked against
the compiled descriptor set, not the instance — they guard the *shape*, which is
where Invariants 4 and 5 actually live.
