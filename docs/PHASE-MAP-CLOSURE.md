# Arkwen — 5-Phasen-Closure & Phase-Map

Statusüberblick des Arkwen-Architektur-Entwurfs. Alle fünf ursprünglich geschnittenen Phasen sind konzeptionell abgeschlossen (ADR-001…010), adversarial verifiziert und cross-phase reconciled. Die Runtime-ADRs sind **consumer-agnostisch** — kein benannter Outer Loop ist privilegiert (ELIO nur als Referenz-Consumer in `ELIO-reference-consumer.md`). Der einzige benannte offene Owner (ADR-009 R6, AuthZ) ist durch **ADR-010** geschlossen.

## Phase-Map
| Phase | Thema | ADR(s) | Status |
| --- | --- | --- | --- |
| 1 | Runtime-Adapter + Session-Modell | ADR-001, 002, 003A, 004 | closed |
| 2 | Lifecycle + Events + Quality Gates | ADR-003B, 005 | closed |
| 3 | Workcell-Isolation + Contract-Plane-AuthZ + Secrets | ADR-006, ADR-010 | closed |
| 4 | Warehouse + Blueprints (Reproduzierbarkeit) | ADR-007 | closed |
| 5 | Outer-Loop-Contract (Referenz-Consumer: ELIO) | ADR-008 | closed |
| — | Cross-Phase Reconciliation | ADR-009 | closed |

## Wie das entstanden ist
Phase 1–2 kollaborativ Fork-für-Fork entschieden. Phase 3–5 per Multi-Agent-Workflow: pro Phase ein Design-Agent → adversarialer Verify-Agent (fand & behob 5 echte Loosening-Verstöße gegen die Invarianten) → phasenübergreifender Abschluss-Kritiker (fand 9 Cross-Phase-Nähte → ADR-009). Nachträglich wurde Phase 5 von „ELIO Integration" zu einem **consumer-agnostischen Outer-Loop-Contract** umgerahmt, damit Arkwen unabhängig als Standard positioniert bleibt.

## Die zehn Invarianten (durchgängiger Prüfmaßstab)
Controller-agnostisch · Stream-als-Wahrheit · worker.raw-Erhalt · Pointer-statt-Payload · Secret-Dichtheit · Repro-Seeds · fail_closed/stricter-only · Overlay-statt-Terminal · tiered-Degradation · Outer-Loop-stricter-nie-lockerer.

## Verbleibende offene Punkte (Implementierung/Betrieb — kein Kern-Anliegen offen)
- **AuthZ-Modell** der externen Read-/Command-Plane: **geschlossen durch ADR-010** (Principals, Tenants, least-privilege Permissions, Run-Scoping, Control-Plane Audit Ledger fixiert). Nur noch deferred: konkretes **AuthN-Backend** (OIDC/mTLS-CA, Key-Rotation), volle **RBAC-Policy-Sprache**, **Tenant-Lebenszyklus**, kontrollierte **cross-tenant-Delegation**.
- **Scheduling/Placement** für `strict`/Firecracker (nested-virt/bare-metal + Host-Label + Scheduler-Constraint).
- **gVisor-Kompatibilitäts-Katalog**, damit fail-closed-Ablehnung vorhersagbar ist.
- **Secret-Broker-Backend** (Vault vs Cloud-KMS/STS, pluggable) + HA/Failover.
- **Mid-Tool-Call-Rotation-Semantik** (kooperative KANN-Capability; sonst Injection-at-start).
- **Hosted-Modell-Attestation** (image_digest pinnt nur self-hosted Bytes).
- **Warehouse-Retention/GC** gemirrorter Images + aufgelöster Versionen.
- **Trust-Root/PKI** + Attestation-Format (in-toto/DSSE) + Key-Rotation.
- **Multi-Outer-Loop-Fairness/Fencing**; **Projektions-Snapshots** für Consumer-Kaltstart.
- **Extern-Resolver-Timeout-Default**; **artifact_signals-Normalisierung**; **Idempotency-Key-Konvention**.
- Blueprint-Komposition/Vererbung; Cell-Shim-Promotion-Lifecycle; Cross-Warehouse-Federation.
