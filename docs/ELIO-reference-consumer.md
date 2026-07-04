# ELIO als Referenz-Outer-Loop-Consumer

**Kontext:** Arkwen definiert eine **consumer-agnostische** Outer-Loop-Contract-Fläche (ADR-008: Read-Plane, Command-Plane, Run-Metrics-Projektion, Delivery-Semantik). Kein benannter Outer Loop ist in der Runtime privilegiert; Arkwen ist standalone vollständig. Dieses Dokument beschreibt, wie **ELIO** — der Outer/inter-session-Loop unserer Produktfamilie (Vela = Inner/intra-session) — die generischen Rollen konkret besetzt. **Arkwen kennt keinen ELIO-Typ**; dies ist ELIOs Sicht + ELIOs Adapter.

## Rollen-Besetzung (generische Rolle → ELIO)
| Generische Rolle (ADR-008) | ELIO |
| --- | --- |
| **Governance-Plane-Inhaber** | liefert das `policy_bundle` (Org-Floor: Gate-Set + Isolationsdimensionen, ADR-009 R3) in der RunSpec |
| **Queue-Producer** | enqueued Missionen (mit Blueprint-Ref + `version_constraint`) in die Production Queue; idempotent via `idempotency_key` |
| **Read-Plane-Consumer** | subscribed den Event-Stream + zieht Projektionen (inkl. Run-Metrics), um den inter-session-Lern-Loop zu schließen |
| **Extern-Resolver (escalate)** | optional per-Gate als Resolver vorkonfiguriert (`max_wait` Pflicht, `fail_closed` bei Timeout, ADR-008 E3) |
| **arkwen-adapter** | ELIO-seitig; fährt einen Arkwen-Run als opaken Node in ELIOs DAG und gibt Terminal-State/Artifacts als NodeResult zurück |

*(Ohne ELIO stellt der Governance-Plane-Inhaber ein Control Room / Operator.)*

## Der geschlossene Outer Loop
```
ELIO entscheidet → enqueue → Arkwen produziert (misst objektiv)
   → Stream + Metrics → ELIO bewertet/lernt → justiert Policies/Blueprints/Portfolio → enqueue ↺
```
**Vela** deckt den inneren (intra-session) Loop, **ELIO** den äußeren (inter-session).

## Was ausdrücklich NICHT gilt
- Arkwen callt ELIO nie; keine ELIO-Abhängigkeit im Runtime-Code (Invariant 10, ADR-008 E1).
- ELIO kann Arkwen nur **strenger** machen; der Arkwen-intrinsische Floor (Redaction, fail_closed, Seed-Capture) ist auch für ELIO nicht abschaltbar (Enqueue-Reject, ADR-008 E3).
- **Ersetzbarkeit:** Jeder andere Orchestrator (oder ein Operator via Control Room, oder kein Outer Loop) kann dieselben Rollen füllen. ELIO ist der erste/Referenz-Consumer, nicht die Definition.

Verweise: [ADR-008](adr/ADR-008.md) (Outer-Loop Contract), [ADR-009](adr/ADR-009.md) (Reconciliation, R3/R5/R6).
