# Roadmap — Aeon Agent Harness Platform

> Este fichero es el estado de verdad del proyecto. Una feature sólo pasa a `DONE` cuando su test de
> aceptación nombrado existe y está en verde en CI. `make roadmap-check` (o `aeon roadmap check`
> cuando exista el CLI) valida mecánicamente esta regla — ver [docs/adr](docs/adr/).
>
> Estados: `TODO` · `IN_PROGRESS` · `BLOCKED` · `DONE` · `DEFERRED` (→ movida a [backlog.md](backlog.md))
>
> Última actualización: 2026-08-29 (`aeon validate` real: JSON Schema + $ref multi-archivo).

## Resumen ejecutivo

| Fase | Nombre | % DONE | Estado |
|---|---|---|---|
| F0 | Foundation durable | ~53% (9/17) | `IN_PROGRESS` |
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
- `SEC-001` (Cedar Policy Engine) y `TOOL-001` (Gateway, porción de ejecución) —
  [tool_gateway_handlers_test.go](go/internal/api/tool_gateway_handlers_test.go) carga el
  `policy_bundle.yaml` real del repo (no un string embebido) y prueba, contra los handlers HTTP
  reales: una tool permitida se ejecuta de verdad; una tool con `forbid` explícito (`shell.*`) es
  denegada con 403 y **nunca llega al executor**; una tool ausente de todo `permit` es denegada por
  el default-deny de Cedar; y la política es por agente, no global (otro agente no hereda los
  permisos de `deep-research-general`). Verificado también en vivo por HTTP contra
  `aeon-toolgw` real corriendo en el compose stack.
- `RUN-002` (Graph Runtime) — [test_graph_runtime_node_kinds](python/tests/integration/test_graph_runtime.py)
  ejecuta [una única fixture](python/tests/fixtures/graph_all_node_kinds.json) (también
  schema-validada en `test_contracts.py`, para que ambos tests no puedan divergir) que ejercita los
  6 tipos de nodo en un solo árbol contra un Temporal efímero real: `sequential`, `parallel` (dos
  tools corren de verdad, no se saltan), `conditional` (la rama se decide leyendo el resultado ya
  ejecutado de un hermano — `if_false` nunca se ejecuta), `loop` (una vez frena antes por
  `stop_condition`, otra agota `max_iterations`, cada iteración con su propia `idempotency_key` aun
  con los mismos `tool_args`), `subgraph` (envuelve el resultado interno, no lo sustituye —
  `graph_result`), y `fan_in` (mezcla los resultados de sus hijos en una sola lista). **Nota de
  implementación real:** este feature expuso un bug genuino de Temporal Python SDK — el sandbox
  sólo respeta `imports_passed_through()` si se declara en el fichero que define el `@workflow.defn`
  directamente, no en un módulo helper que ese fichero importa (aunque el helper también esté
  marcado passthrough). El síntoma no es un error de import: es un fallo determinista de decode
  ("`name 'Any' is not defined`") que Temporal reintenta para siempre con backoff, indistinguible de
  un cuelgue salvo leyendo el stderr del worker. Documentado en
  [ADR-001](docs/adr/0001-temporal-determinism-boundary.md).
- `RUN-001` (Run Controller) — [run_controller_handlers_test.go](go/internal/api/run_controller_handlers_test.go)
  ejercita `aeon-runcontroller` real contra un Temporal real y un worker real (`make
  test-go-integration`, que ahora también levanta `temporal` y `worker`, no sólo `postgres`), a
  través de su API HTTP: **pause** detiene un run antes de que ejecute su primer nodo (sin
  condición de carrera — `graph.py` comprueba la puerta de pausa antes de cada nodo, así que por
  rápida que sea la tool, no puede avanzar hasta el resume) y el status pasa a `PAUSED`; **resume**
  lo deja terminar en `SUCCEEDED`; **cancel** sobre un run pausado llega de forma determinista a
  `CANCELLED` (pausar primero evita la carrera de cancelar un run de un solo nodo que ya pudo haber
  terminado); **stream** (Server-Sent Events) reporta la transición hasta el estado terminal. El
  Run Controller no guarda estado propio — Temporal es la única fuente de verdad, así que el
  servicio es stateless. Verificado también en vivo por HTTP contra `aeon-runcontroller` real
  corriendo en el compose stack.
- `RUN-003` (Budgets) — [test_budget_hard_stop.py](python/tests/integration/test_budget_hard_stop.py)
  prueba tres dimensiones contra un Temporal efímero real, cada una como **hard stop real**: la
  acción que cruzaría el límite nunca se ejecuta, no es sólo que el run termine marcado como
  fallido. `max_tool_calls`: un loop dispuesto a correr 10 iteraciones con límite de 3 se detiene
  exactamente en 3 (`budgets_consumed` lo confirma). `max_depth`: dos niveles de `subgraph`
  anidado con `max_depth=1` nunca ejecuta el nodo del segundo nivel. `deadline_seconds`: un
  deadline de 0 (ya vencido al arrancar) impide que se ejecute cualquier nodo, incluso el primero.
  `model_calls`/`tokens`/`cost_usd` quedan declarados en `BudgetsConsumed` pero sin aplicar —
  no hay todavía un punto de llamada al Model Gateway en el Graph Runtime que los produzca
  (`MDL-001`, sigue `TODO`); su forma no debería cambiar cuando lleguen.
  **Nota de implementación real:** este feature expuso un segundo bug real de Temporal (distinto al
  de `RUN-002`): una excepción Python normal (`Exception`) lanzada dentro del workflow no termina
  el run — Temporal la trata como un fallo del *workflow task* y la reintenta para siempre con
  backoff, indistinguible de un cuelgue. La excepción debe heredar de
  `temporalio.exceptions.ApplicationError` para que Temporal la trate como un fallo *terminal* del
  run. Corregido en la base (`GraphError`), no sólo en `BudgetExceededError` — cualquier error
  estructural del grafo (kind desconocido, node sin id) tenía el mismo problema latente. Verificado
  también en vivo por HTTP contra `aeon-runcontroller` real: `budgets_consumed` es consultable
  incluso después de que el run termine en `FAILED`, porque Temporal responde queries sobre un
  workflow cerrado reproduciendo su historial.
- `RUN-005` (Approvals) — [test_approval_binding.py](python/tests/integration/test_approval_binding.py)
  prueba las cuatro salidas posibles de un nodo `tool_call` marcado `requires_approval: true`,
  contra un Temporal efímero real: **aprobado** con el hash exacto → la tool se ejecuta de verdad;
  **rechazado** → nunca se ejecuta; **aprobado con un hash distinto al que está pendiente**
  (parameter binding — el corazón de esta feature, matching el criterio de la spec "aprobar con
  args A, mutar a B antes de ejecutar → rechazo") → denegado, nunca se ejecuta; **expira sin
  decisión** (TTL) → denegado, nunca se ejecuta. `pending_approval` es consultable durante la
  espera y sigue la forma exacta de `RunState.pending_approval`. Verificado también en vivo por
  HTTP contra `aeon-runcontroller` real: `POST /runs/{id}/approve` sólo necesita el `tool_call_hash`
  que el cliente vio en `Status` — el controller resuelve el `approval_id` internamente consultando
  `pending_approval`, así que un dashboard nunca necesita rastrear `approval_id`s.
  **Nota de implementación real:** el diseño original de las señales `approve`/`reject` usaba dos
  argumentos posicionales (`approval_id, tool_call_hash`) — funciona en Python-a-Python, pero el
  cliente Go de Temporal (`SignalWorkflow`) sólo puede codificar **un** argumento por señal
  (`dc.ToPayloads(arg)` envuelve exactamente un valor). Enviar dos habría fallado en runtime con el
  mismo síntoma silencioso de intentos anteriores (fallo de decode → reintento infinito). Se
  corrigió pasando un único payload estructurado (`{"approval_id":..., "tool_call_hash":...}`),
  que además es la práctica correcta para cualquier señal que deba ser interoperable entre SDKs.
- `FND-003` (Config-as-code) — [main_test.go](go/cmd/aeon/main_test.go) prueba `aeon validate`
  con JSON Schema real (no comparaciones superficiales de `apiVersion`/`kind`): acepta los tres
  manifiestos reales de `examples/deep-research/` (incluido `model_policy_bundle.yaml`, cuyo
  `$ref` cruzado a `model_profile.schema.json` se resuelve **offline** por `$id`, sin red), y
  rechaza con mensajes accionables: un `kind` desconocido, un `Agent` al que le falta `spec`, y un
  `ModelPolicyBundle` cuyo perfil viola de verdad el shape de `model_profile.schema.json` a través
  del `$ref`. Los cuatro `kind` soportados (`Agent`, `EvalSuite`, `PolicyBundle`,
  `ModelPolicyBundle`) se resuelven a su schema por convención de `$id`
  (`https://aeon.dev/manifests/<archivo>`), no por lógica hardcodeada por tipo.
- `MDL-006` (`prometheus_inference`, **IN_PROGRESS**) — el adaptador es real, no un stub: OAuth2
  `client_credentials` de verdad contra el auth-service de Prometheus (confirmado con el equipo de
  la plataforma y contra una instancia local viva en `127.0.0.1:8020`/`9000`), con caché de token,
  reintento automático con token fresco tras un 401, y `POST /v1/chat/completions` en formato
  OpenAI real. `prometheus_inference_test.go` prueba todo esto contra un servidor falso que replica
  el contrato documentado exacto (no un mock superficial): confirma que `GET /v1/models` es público,
  que dos llamadas seguidas reusan el mismo token (no hay una petición redundante), y que un 401
  fuerza exactamente un token nuevo y reintenta con él. Se creó también una interfaz `Provider`
  única compartida (`go/internal/providers/provider.go`) — antes estaba duplicada idéntica en cada
  uno de los 5 stubs. **Pendiente para pasar a `DONE`:** la suite `provider_conformance` (llega con
  `EVAL-002`/F2) y una verificación en vivo con un modelo real registrado (la instancia de prueba
  no tenía ninguno dado de alta en el momento de esta implementación). Ver
  [ADR-004](docs/adr/0004-model-gateway-provider-abstraction.md), resuelto con los hechos reales.

El resto de F0 (Model Gateway con el resto de proveedores, routing/fallback, tabla de dedupe del
Tool Gateway) siguen siendo stubs de scaffolding o TODO — ver filas abajo.

---

## F0 — Foundation durable (semanas 1-4)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| FND-001 | Agent Registry (CRUD/versionado, lifecycle Draft→Candidate→Released→Retired) | `DONE` | `TestAgentRegistryLifecycle` en verde | go/internal/store/agent_registry_test.go |
| FND-003 | Config-as-code (manifiestos en Git, UI no es source of truth) | `DONE` | `TestAeonValidateAcceptsAndRejectsExampleManifests` en verde | go/cmd/aeon/main_test.go |
| RUN-001 | Run Controller (start/cancel/pause/resume/status/stream) | `DONE` | `TestRunControllerLifecycle` en verde | go/internal/api/run_controller_handlers_test.go |
| RUN-002 | Graph Runtime (sequential/parallel/conditional/loop/subgraph/fan-in) | `DONE` | `test_graph_runtime_node_kinds` en verde | python/tests/integration/test_graph_runtime.py |
| RUN-003 | Budgets (tokens/calls/tools/cost/deadline/depth, hard stop) | `DONE` | `test_budget_hard_stop` en verde (tool_calls/depth/deadline; model_calls/tokens/cost_usd declarados, no aplicados hasta MDL-001) | python/tests/integration/test_budget_hard_stop.py |
| RUN-004 | Checkpoint & replay (resume sin duplicar tool effects) | `DONE` | `test_crash_resume_no_duplicate_write` en verde | python/tests/integration/test_crash_resume.py |
| RUN-005 | Approvals (interrupt durable, parameter binding, expiry) | `DONE` | `test_approval_binding` en verde (aprobado, rechazado, hash no coincide, expira) | python/tests/integration/test_approval_binding.py |
| MDL-001 | Model Gateway (capability profiles, adapters, fallback, routing) | `TODO` | `test_model_gateway_routing_fallback` en verde | — |
| MDL-003 | Adaptador `anthropic` | `TODO` | pasa `provider_conformance` | — |
| MDL-004 | Adaptador `openai` | `TODO` | pasa `provider_conformance` | — |
| MDL-005 | Adaptador `gemini` | `TODO` | pasa `provider_conformance` | — |
| MDL-006 | Adaptador `prometheus_inference` (LLM local) | `IN_PROGRESS` | pasa `provider_conformance` (suite no existe aún, llega con EVAL-002/F2); mientras tanto: `TestPrometheusInferenceRetriesOnceWithFreshTokenAfter401` y el resto de `prometheus_inference_test.go` en verde | go/internal/providers/prometheus_inference/prometheus_inference_test.go |
| MDL-007 | Adaptador `openai_compatible` (vLLM/Ollama/TGI genérico) | `TODO` | pasa `provider_conformance` | — |
| TOOL-001 | Tool Registry/Gateway (typed schemas, risk classification, scopes) | `DONE` | `TestToolRegistryCRUDAndRiskClassification` (registry) + `TestToolPolicyDeniesOutOfManifestToolCall` (gateway ejecuta con policy check real) | go/internal/store/tool_registry_test.go, go/internal/api/tool_gateway_handlers_test.go |
| SEC-001 | Policy Engine (Cedar, authz fuera del modelo) | `DONE` | `TestToolPolicyDeniesOutOfManifestToolCall` en verde | go/internal/api/tool_gateway_handlers_test.go |
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
