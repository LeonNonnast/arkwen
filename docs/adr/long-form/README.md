# Long-form ADRs

Vertiefte Fassungen der Phase-3–5-ADRs mit vollständigen Threat-Models, Feld-Schemata,
Begründungen und Invarianten-Checks. **Consumer-agnostisch** (ELIO nur als Referenz-Consumer,
siehe `../../ELIO-reference-consumer.md`) und an die ADR-009-Reconciliations + ADR-010 angeglichen.

Die verdichteten kanonischen Fassungen liegen in `docs/adr/ADR-00X.md` — diese Langform-Dateien
sind die detaillierten Begleiter, nicht ein zweiter Wahrheits-Speicher. Bei Widerspruch gilt die
verdichtete kanonische Fassung + die Reconciliation in `docs/adr/ADR-009.md`.

| ADR | Thema |
| --- | --- |
| ADR-006 | Workcell Isolation & Secrets (Isolation-Profile, Egress, Secret-Broker) |
| ADR-007 | Warehouse & Blueprints (CAS, Blueprint-Pin, reproduce()) |
| ADR-008 | Outer-Loop Contract (Control Plane, consumer-agnostisch) |
