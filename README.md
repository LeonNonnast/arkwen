# Arkwen

**Arkwen ist eine Factory Runtime für autonome Softwareproduktion.** Sie orchestriert Worker, Workcells und Toolkits, sodass beliebige Agenten reproduzierbar, isoliert und überwacht Software-Artefakte erzeugen können.

> Nicht der Agent ist das Produkt, sondern die Fabrik.

Langfristiges Ziel: eine standardisierte Runtime-Schicht für KI-Agenten — vergleichbar mit **OCI für Container** —, sodass Agenten unterschiedlicher Hersteller austauschbar orchestriert und überwacht werden können. **Arkwen ist standalone vollständig**; ein Outer Loop (z. B. ELIO) ist ein optionaler Consumer eines öffentlichen Contracts, keine Abhängigkeit.

## Modell in einem Satz

```
Mission → Factory Controller → Cell-Shim → Workcell → Event Stream → Projections
```

## Domänensprache

| Technischer Begriff | Arkwen-Begriff |
| --- | --- |
| Prompt | **Mission** |
| Session | **Production Run** |
| Agent | **Worker** |
| Docker Container | **Workcell** |
| Runtime | **Factory Runtime** |
| Controller | **Factory Controller** |
| Web UI | **Factory Floor** / **Control Room** |
| Skills | **Toolkits** |
| MCP Server | **Tools** |
| Files | **Materials** |
| Output | **Artifacts** |
| Template | **Blueprint** |
| Docker Image | **Worker Image** |
| Queue | **Production Queue** |
| Workspace | **Workbench** |
| Approval | **Quality Gate** |
| Registry | **Warehouse** |

## CLI (Zielbild)

```
arkwen run create \
  --mission mission.md \
  --worker claude-code \
  --toolkit webapp \
  --workbench ./src
```

## Architecture Decision Records

Der Entwurf ist vollständig in ADRs festgehalten (`docs/adr/`). Alle fünf Phasen sind konzeptionell abgeschlossen, adversarial verifiziert und cross-phase reconciled.

| Phase | Thema | ADR(s) |
| --- | --- | --- |
| 1 | Runtime-Adapter + Session-Modell | [ADR-001](docs/adr/ADR-001.md), [002](docs/adr/ADR-002.md), [003](docs/adr/ADR-003.md), [004](docs/adr/ADR-004.md) |
| 2 | Lifecycle + Events + Quality Gates | [ADR-003](docs/adr/ADR-003.md), [005](docs/adr/ADR-005.md) |
| 3 | Workcell-Isolation + Contract-Plane-AuthZ + Secrets | [ADR-006](docs/adr/ADR-006.md), [ADR-010](docs/adr/ADR-010.md) |
| 4 | Warehouse + Blueprints | [ADR-007](docs/adr/ADR-007.md) |
| 5 | Outer-Loop-Contract (Control Plane) | [ADR-008](docs/adr/ADR-008.md) |
| — | Cross-Phase Reconciliation & Closure | [ADR-009](docs/adr/ADR-009.md) |

Siehe auch [`docs/ARCHITECTURE-OVERVIEW.md`](docs/ARCHITECTURE-OVERVIEW.md), [`docs/PHASE-MAP-CLOSURE.md`](docs/PHASE-MAP-CLOSURE.md) und [`docs/ELIO-reference-consumer.md`](docs/ELIO-reference-consumer.md).

**Implementierung:** [`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md) — slice-basierter Build-Plan (S0…S6). Vertiefte ADR-Fassungen mit vollen Threat-Models: [`docs/adr/long-form/`](docs/adr/long-form/).

## Die zehn Invarianten

Als durchgängiger Prüfmaßstab über alle ADRs:

1. Controller bleibt runtime-agnostisch (Worker-Spezifika leben im Cell-Shim).
2. Der append-only Event-Stream ist die einzige Wahrheit; Controller = Projektion.
3. `worker.raw` geht nie verloren — außer redigierte Secrets.
4. Events tragen nur Metadaten + Pointer, nie große Payloads.
5. Secrets nie in persistenten Logs/Artifacts/UI; Redaction im Cell-Shim vor Persistenz.
6. Reproduzierbarkeits-Seeds ab Tag 1.
7. Quality-Gate-Default fail_closed; höhere Policy-Ebenen nur strenger, nie lockerer.
8. `paused` ist ein Overlay, kein Terminalzustand; Terminale sind nur completed/failed/canceled.
9. Tiered Integration mit graceful degradation (nur Beobachtbarkeit degradiert, nie der Security-Floor).
10. Ein Outer Loop darf Arkwen strenger machen, nie lockerer; Arkwen hängt von keinem Consumer ab.

## Status

Konzeptioneller Architektur-Entwurf (ADR-Phase). Noch keine Implementierung.
