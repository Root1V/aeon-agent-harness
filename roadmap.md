# Roadmap — Aeon Agent Harness Platform

> Este fichero es el estado de verdad del proyecto. Una feature sólo pasa a `DONE` cuando su test de
> aceptación nombrado existe y está en verde en CI. `make roadmap-check` (o `aeon roadmap check`
> cuando exista el CLI) valida mecánicamente esta regla — ver [docs/adr](docs/adr/).
>
> Estados: `TODO` · `IN_PROGRESS` · `BLOCKED` · `DONE` · `DEFERRED` (→ movida a [backlog.md](backlog.md))
>
> Última actualización: 2026-08-28 (Agent/Tool Registry real sobre Postgres, verificado en vivo).

## Resumen ejecutivo

| Fase | Nombre | % DONE | Estado |
|---|---|---|---|
| F0 | Foundation durable | ~18% (3/17) | `IN_PROGRESS` |
| F1 | Contexto y evidencia | 0% | `TODO` |
| F2 | Deep Research + EvalOps (**MVP**) | 0% | `TODO` |
| F3 | Memoria gobernada | 0% | `TODO` |
| F4 | Trust e interoperabilidad | 0% | `TODO` |
| F5 | Learning Lab | 0% | `TODO` |

Bloqueos abiertos: ninguno. Supuesto pendiente de confirmar: forma de la API de la plataforma de
inferencia local "Prometheus" del usuario (asumimos OpenAI-compatible — ver ADR-004).

**Progreso real verificado hoy:**
- `RUN-004` (Checkpoint & replay) — [test_crash_resume_no_duplicate_write](python/tests/integration/test_crash_resume.py)
  pasa contra un servidor Temporal efímero real y dos procesos worker separados (uno se mata a sí
  mismo con `os._exit(1)` tras confirmar la escritura). Valida [ADR-001](docs/adr/0001-temporal-determinism-boundary.md)
  de punta a punta.
- `FND-001` (Agent Registry) y `TOOL-001` (Tool Registry, porción CRUD) —
  [agent_registry_test.go](go/internal/store/agent_registry_test.go) y
  [tool_registry_test.go](go/internal/store/tool_registry_test.go) pasan contra Postgres real
  (`make test-go-integration`), y se verificó en vivo por HTTP contra `aeon-controlplane` real
  corriendo en el compose stack: crear agente, transición de lifecycle válida e inválida (422),
  creación duplicada (409), y rechazo de una tool con efectos sin `idempotency_key_fields` (400).
  El lifecycle es estrictamente hacia adelante, un paso a la vez — no se puede saltar de `Draft`
  a `Released` ni retroceder.

El resto de F0 (Cedar policy engine, Model/Tool Gateway ejecutando de verdad, aprobaciones,
budgets) siguen siendo stubs de scaffolding: compilan y sirven `/healthz`, pero no implementan
lógica de negocio todavía — ver filas `TODO` abajo. En particular, `TOOL-001` está `DONE` sólo para
su porción de registro/clasificación de riesgo; el Tool *Gateway* ejecutando llamadas reales con
policy check llega con `SEC-001`.

---

## F0 — Foundation durable (semanas 1-4)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| FND-001 | Agent Registry (CRUD/versionado, lifecycle Draft→Candidate→Released→Retired) | `DONE` | `TestAgentRegistryLifecycle` en verde | go/internal/store/agent_registry_test.go |
| FND-003 | Config-as-code (manifiestos en Git, UI no es source of truth) | `TODO` | `aeon validate` acepta/rechaza manifiestos de `examples/` | — |
| RUN-001 | Run Controller (start/cancel/pause/resume/status/stream) | `TODO` | `test_run_controller_lifecycle` en verde | — |
| RUN-002 | Graph Runtime (sequential/parallel/conditional/loop/subgraph/fan-in) | `TODO` | `test_graph_runtime_node_kinds` en verde | — |
| RUN-003 | Budgets (tokens/calls/tools/cost/deadline/depth, hard stop) | `TODO` | `test_budget_hard_stop` en verde | — |
| RUN-004 | Checkpoint & replay (resume sin duplicar tool effects) | `DONE` | `test_crash_resume_no_duplicate_write` en verde | python/tests/integration/test_crash_resume.py |
| RUN-005 | Approvals (interrupt durable, parameter binding, expiry) | `TODO` | `test_approval_binding` en verde | — |
| MDL-001 | Model Gateway (capability profiles, adapters, fallback, routing) | `TODO` | `test_model_gateway_routing_fallback` en verde | — |
| MDL-003 | Adaptador `anthropic` | `TODO` | pasa `provider_conformance` | — |
| MDL-004 | Adaptador `openai` | `TODO` | pasa `provider_conformance` | — |
| MDL-005 | Adaptador `gemini` | `TODO` | pasa `provider_conformance` | — |
| MDL-006 | Adaptador `prometheus_inference` (LLM local) | `TODO` | pasa `provider_conformance`; ver ADR-004 | — |
| MDL-007 | Adaptador `openai_compatible` (vLLM/Ollama/TGI genérico) | `TODO` | pasa `provider_conformance` | — |
| TOOL-001 | Tool Registry/Gateway (typed schemas, risk classification, scopes) | `DONE` | `TestToolRegistryCRUDAndRiskClassification` en verde (registry only — el Gateway ejecutor real vive junto a SEC-001) | go/internal/store/tool_registry_test.go |
| SEC-001 | Policy Engine (Cedar, authz fuera del modelo) | `TODO` | `test_tool_policy_denies_out_of_manifest` en verde | — |
| OBS-001 | Distributed tracing (OTel GenAI semantic conventions) | `TODO` | spans `invoke_agent`/`chat`/`execute_tool` visibles en Tempo | — |
| — | `deploy/compose` completo (Temporal, Postgres+pgvector, MinIO, OTel, Tempo, Grafana) | `IN_PROGRESS` | `make dev` levanta todos los servicios sanos | — |

**Salida de fase:** `test_crash_resume_no_duplicate_write` en verde + el mismo run pasa contra los
cuatro adaptadores cloud/local (`test_provider_parity`).

## F1 — Contexto y evidencia (semanas 5-8)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| CTX-001 | Typed Context Lanes (L0-L6) | `TODO` | `test_lane_fidelity_policy` en verde | — |
| CTX-002 | Context Budgeter (ensamblado por prioridad + cache-hit) | `TODO` | `test_budgeter_cache_stable_ordering` en verde | — |
| CTX-003 | Offload (tool I/O grande → observation store + puntero) | `TODO` | `test_no_full_document_injection` en verde | — |
| CTX-004 | Typed Compaction (fidelity policy por lane, no resumen uniforme) | `TODO` | `test_pinned_exact_survives_stress` en verde | — |
| CTX-005 | Addressable Recall (IDs estables, `context.recall`) | `TODO` | `test_addressable_recall_roundtrip` en verde | — |
| CTX-006 | Context Integrity Gate (constraint/citation/token checks pre-model-call) | `TODO` | `test_integrity_gate_blocks_missing_constraint` en verde | — |
| RAG-001 | Retrieval Gateway (connectors, ACL, hybrid search, rerank, cache) | `TODO` | `test_retrieval_acl_enforced` en verde | — |
| RAG-002 | Evidence Extractor (compactación condicionada → EvidencePacket) | `TODO` | `test_evidence_packet_schema_valid` en verde | — |
| RAG-003 | Evidence Ledger (provenance, dedupe, contradictions, source quality) | `TODO` | `test_ledger_contradiction_grouping` en verde | — |

**Salida de fase:** documento >50k tokens nunca entra completo; 100% de `PINNED_EXACT` y locators
sobreviven 50 ciclos de compactación.

## F2 — Deep Research + EvalOps → MVP (semanas 9-11)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| DR-001 | Research Planner (3-5 subtareas, coverage, budgets) | `TODO` | `test_planner_subtask_bounds` en verde | — |
| DR-002 | Isolated Researchers (parallel worker contexts, bounded ReAct) | `TODO` | `test_researcher_isolation` en verde | — |
| DR-003 | Sufficiency Gate (coverage matrix, contradiction gate, replanning) | `TODO` | `test_sufficiency_gate_replans` en verde | — |
| DR-004 | Tool-less Reporter (output sólo desde allowed_claim_ids) | `TODO` | `test_reporter_no_tools_available` en verde | — |
| DR-005 | Citation Verifier (claim-to-evidence, repair-from-ledger) | `TODO` | `test_reporter_cannot_invent_citations` en verde | — |
| EVAL-001 | Eval Registry (datasets, graders, thresholds, versions) | `TODO` | `aeon eval list` muestra las suites de `evals/suites` | — |
| EVAL-002 | Eval Runner (offline/repeated trials/provider matrix/trace graders) | `TODO` | `aeon eval run deep_research_core` produce reporte | — |
| EVAL-003 | Release Gates (bloquear promoción por regresión) | `TODO` | `test_release_gate_blocks_regression` en verde | — |
| DX-001 | SDK Python (start_run, tools, contexts, memory, traces, approvals) | `TODO` | `examples/deep-research` corre con `aeon_sdk` | — |
| DX-002 | CLI (init/validate/run/eval/trace/replay/publish) | `TODO` | los 7 subcomandos ejecutan sin error contra el compose | — |
| DX-003 | Template Deep Research | `TODO` | `examples/deep-research/agent.yaml` válido y ejecutable | — |
| INT-001 | `FrameworkAdapter` LangGraph (Modo B) | `TODO` | `examples/langgraph-interop` corre dentro de una Activity | — |
| INT-002 | Endpoint OpenAI-compatible del Model Gateway (Modo C) | `TODO` | un cliente `openai` apuntando a `base_url` local obtiene routing/budgets | — |

**MVP:** `make dev && aeon run examples/deep-research --query "…"` produce informe con citas
verificadas, traza navegable, coste por run y `aeon replay <run_id>` idéntico — contra proveedor
cloud o local.

## F3 — Memoria gobernada (semanas 12-17)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| MEM-001 | Memory Store (typed/scoped/versioned, provenance/TTL/status) | `TODO` | `test_memory_record_schema_valid` en verde | — |
| MEM-002 | Memory Candidate Pipeline (quarantine→validate→promote/reject) | `TODO` | `test_memory_write_mode_candidate_only` en verde | — |
| MEM-003 | Reflection (post-run candidate extraction) | `TODO` | `test_reflection_extracts_candidates` en verde | — |
| MEM-005 | Utility/Forgetting (decay, prune, supersede/revoke) | `TODO` | `test_memory_decay_prunes_stale` en verde | — |
| SEC-004 | Memory security (isolation, poisoning tests, repair/revocation) | `TODO` | `memory_poisoning` suite en verde | — |
| EVAL-004 | Learning Eval (forward/negative transfer, usefulness, staleness) | `TODO` | reporte de `evals/suites/learning_eval` | — |

## F4 — Trust e interoperabilidad (semanas 18-23)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| TOOL-002 | MCP Adapter (core stateless 2026-07-28 + legacy adapter) | `TODO` | conformidad contra un servidor MCP de referencia | — |
| INT-003 | Servidor MCP de salida (catálogo de tools gobernado) | `TODO` | un cliente MCP externo lista y llama tools de Aeon | — |
| A2A-001 | A2A Gateway (Agent Card, identity/authz, task exchange) | `TODO` | `test_a2a_task_lifecycle` en verde | — |
| INT-004 | `FrameworkAdapter` CrewAI | `TODO` | ejemplo equivalente a `langgraph-interop` | — |
| INT-005 | `FrameworkAdapter` OpenAI Agents SDK | `TODO` | ídem | — |
| INT-006 | `FrameworkAdapter` Microsoft Agent Framework | `TODO` | ídem | — |
| INT-007 | `FrameworkAdapter` Claude Agent SDK | `TODO` | ídem | — |
| TOOL-003 | Sandbox (shell/code/browser, microVM/gVisor, egress allowlist) | `TODO` | `test_sandbox_egress_denied_by_default` en verde | — |
| SEC-002 | Secret Broker (short-lived credentials) | `TODO` | `test_no_secret_in_prompt` en verde | — |
| FND-002 | ABOM (bill of materials firmado, reproducible) | `TODO` | `aeon publish` genera y firma el ABOM | — |
| — | Circuit breaker + kill switch por agente (A5) | `TODO` | `test_circuit_breaker_quarantines_version` en verde | — |
| OBS-002 | Agent Console (trace explorer, context inspector, evidence graph) | `TODO` | UI muestra un run real de punta a punta | — |
| OBS-003 | FinOps (cost per run/success/agent/model/tool) | `TODO` | dashboard con `cost_model: token_based|compute_based` | — |
| MDL-002 | Quality-aware routing (eval scores como condición de routing) | `TODO` | routing cambia con score degradado en fixture | — |

## F5 — Learning Lab (semanas 24+)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| MEM-004 | Experience Abstraction (cluster/generalize procedural memories) | `TODO` | — | — |
| GOV-001 | Multi-tenancy (tenant isolation + per-scope policy/data residency) | `TODO` | — | — |
| GOV-002 | Release management (canary/rainbow/rollback/version pinning) | `TODO` | — | — |

---

Ver también: [backlog.md](backlog.md) para todo lo diferido con su criterio de entrada, y
[docs/adr/](docs/adr/) para las decisiones de arquitectura referenciadas aquí.
