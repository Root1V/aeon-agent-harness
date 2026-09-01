# Roadmap — Aeon Agent Harness Platform

> Este fichero es el estado de verdad del proyecto. Una feature sólo pasa a `DONE` cuando su test de
> aceptación nombrado existe y está en verde en CI. `make roadmap-check` (o `aeon roadmap check`
> cuando exista el CLI) valida mecánicamente esta regla — ver [docs/adr](docs/adr/).
>
> Estados: `TODO` · `IN_PROGRESS` · `BLOCKED` · `DONE` · `DEFERRED` (→ movida a [backlog.md](backlog.md))
>
> Última actualización: 2026-08-31 (F0 cerrado salvo la nota de `local-llm`; **las 13 features de
> F2 están `DONE`** — `INT-002` cierra el Model Gateway como endpoint OpenAI-compatible real,
> verificado con el paquete `openai` de verdad. El MVP como narrativa compuesta ("traza navegable"
> para runs Python, "coste por run", `replay --assert-identical`) todavía tiene huecos reales, ver
> la nota de F2 abajo y `backlog.md` — no bloquean pasar a F3, pero son honestos de nombrar.
> **F3 (Memoria gobernada) está `DONE`, 6/6**: `MEM-001`/`MEM-002` (Memory Store + Candidate
> Pipeline, Postgres-backed), `MEM-003` (Reflection + superficie HTTP), `MEM-005`
> (Utility/Forgetting), `SEC-004` (Memory security) y `EVAL-004` (Learning Eval, que además cierra
> el hueco de `ValidationDecision`/`PromotionDecision` que MEM-002 había dejado abierto). F4 (Trust
> e interoperabilidad) arrancó con `TOOL-002` (MCP Adapter cliente, real sobre el SDK Go oficial de
> MCP, conformidad probada contra la spec 2026-07-28 y contra un fallback legacy 2025-11-25 real),
> `INT-003` (servidor MCP de salida exponiendo el catálogo real de `aeon-toolgw`, verificado también
> con el paquete oficial `mcp` de Python, un cliente independiente del SDK Go), `A2A-001` (A2A
> Gateway, real sobre el SDK Go oficial de A2A, task lifecycle respaldado por un run Temporal real),
> `INT-004` (`FrameworkAdapter` CrewAI, segundo Modo B real, con un puente sync/async real hacia
> `crewai.BaseLLM`), `INT-005` (`FrameworkAdapter` OpenAI Agents SDK, tercer Modo B real — sin
> puente sync/async, y primera vez que el propio bucle de razonamiento del framework externo decide
> cuándo llamar a una tool de Aeon, en vez de que la Activity conduzca una secuencia fija) e
> `INT-006` (`FrameworkAdapter` Microsoft Agent Framework, mismo patrón que `INT-005` — sin meta-
> paquete `agent-framework` instalado, sólo `agent-framework-core`+`agent-framework-openai`). F4 va
> 6/14 (~43%).

## Resumen ejecutivo

F1 (Contexto y evidencia) se completó en su totalidad. Por decisión del usuario, antes de avanzar a
F2 se terminó lo pendiente de F0: Model Gateway con los 5 adaptadores
(`MDL-001/003/004/005/006/007`), la conformidad cruzada mínima, y `OBS-001` (tracing), con spans
reales verificados en un OTel Collector + Tempo reales. Sólo queda una nota de infraestructura de
desarrollo sobre el perfil `local-llm` de `deploy/compose` (ver la fila `—` de F0 abajo), no una
feature con ID propio.

F2 arrancó con `DR-001` (Research Planner). Esto exigió construir la primera integración real
Python↔Go de la plataforma: el Model Gateway (Go) ahora expone `POST /decide` sobre HTTP
(`go/internal/api/model_gateway_handlers.go`, montado en `aeon-modelgw`, adaptadores registrados
desde variables de entorno), y el worker Python lo llama vía una Activity real
(`aeon_worker/activities/model_activities.py::decide_activity`) — sin esto, ningún feature de F2 que
necesite un LLM real podría funcionar. El Planner mismo (`aeon_profiles/deep_research/planner.py`)
es puro y se testea con un `decide` falso; el bound de 3-5 subtareas vive en
`research_plan.schema.json`, no en código de aplicación. De paso se encontraron y arreglaron dos
bugs reales preexistentes: `make test-python` sólo montaba `python/` (rompía cualquier test que
leyera `proto/`/`examples/`, incluido el ya existente `test_contracts.py`) y le faltaba instalar
`pytest` (`--with-editable '.[dev]']` en vez de `.`); y `OPENAI_COMPATIBLE_BASE_URL` en `.env.example`
tenía un `/v1` final que el adaptador ya añade, duplicando la ruta.

`DR-002` (Isolated Researchers) añadió `aeon_worker/decision.py` (parsea y valida la salida del
modelo contra `decision.schema.json`, ya existente en `proto/` desde F0 pero sin ningún consumidor
hasta ahora) y `aeon_profiles/deep_research/researcher.py`: un bucle ReAct acotado por subtarea
(para en `FINISH`/`REQUEST_REPLAN` o al agotar `max_model_calls`/`max_tool_calls` del plan de
`DR-001`), con `run_researchers_in_parallel` corriendo todas las subtareas concurrentemente vía
`asyncio.gather`. Igual que el Planner, es puro (`decide`/`execute_tool`/`recall` inyectados) y se
testea con fakes — `test_researcher_isolation` corre dos subtareas a la vez y verifica que ningún
transcript o tool-call de una menciona el topic de la otra.

`DR-003` (Sufficiency Gate) añadió `aeon_profiles/deep_research/sufficiency_gate.py`: cruza el plan
de `DR-001` con los `ResearchResult` de `DR-002` y el Evidence Ledger de F1 (`aeon_evidence.ledger`,
enlazando por `EvidencePacket.subtopic_id == Subtask.id`) para decidir, por subtarea, si está
cubierta (terminó con evidencia real, no sólo un `FINISH` vacío) y si está impugnada por un
`contradiction_group` sin resolver — nunca resuelve una contradicción, sólo la señala. Cuando
cualquier subtarea queda sin cubrir o impugnada, la Gate pide replanning de exactamente esos
`coverage_topic`, no de todo el plan. Sigue el mismo patrón: puro, sin Temporal, testeado con fakes.

`DR-004` (Tool-less Reporter) añadió `aeon_profiles/deep_research/reporter.py`: `select_allowed_claims`
toma sólo las claims de subtareas que `DR-003` marcó cubiertas y no impugnadas, y
`build_reporter_request` arma la petición al modelo sin la clave `tools`/`tool_choice` en absoluto
— una garantía estructural, no una instrucción que el modelo podría ignorar. El Reporter rechaza
(`ReporterError`, sin reparar) cualquier reporte que cite un `claim_id` fuera de ese conjunto
permitido; reparar una cita real pero mal formada es trabajo de `DR-005`.

`DR-005` (Citation Verifier) cierra el pipeline de Deep Research: `aeon_profiles/deep_research/
citation_verifier.py` es un segundo chequeo, independiente y de sólo lectura, sobre un
`ReportDraft` — no confía en que `DR-004` ya lo haya filtrado (defensa en profundidad, igual que
RUN-003 verifica presupuestos en dos sitios). Para cada cita que no resuelve contra
`select_allowed_claims`, intenta reparación determinista: si el propio texto del draft ya contiene
literalmente el `quote` de alguna claim permitida (y aún no usada por otra reparación en el mismo
draft), sustituye la cita por esa claim real; si ninguna coincide, la marca `unrepairable` y
`verified=False` — nunca inventa evidencia para tapar el hueco. Con esto, `DR-001`→`DR-002`→
`DR-003`→`DR-004`→`DR-005` forma un pipeline real y encadenado (aunque cada pieza sigue siendo pura
y sin Temporal): Planner → Researchers aislados → Sufficiency Gate → Reporter sin tools → Citation
Verifier.

`EVAL-001` (Eval Registry) creó `evals/suites/*.yaml` — 4 `EvalSuite` reales (config-as-code, igual
que `AgentManifest`): `deep_research_core`, `citation_integrity`, `injection_suite`,
`provider_conformance` (exactamente los 4 que `examples/deep-research/agent.yaml` nombra en
`evalGates`, con datasets reales en `evals/datasets/*.jsonl`), y `aeon eval list` (`go/cmd/aeon/main.go`)
los lee, valida cada uno contra `eval_suite.schema.json` (un suite inválido es un error duro, no se
salta en silencio) e imprime nombre/versión/dataset/graders/thresholds/gateOn. Reutiliza la misma
infraestructura de `aeon validate` (FND-003) para cargar y resolver `$ref` entre schemas.

`EVAL-002` (Eval Runner) hizo real el motor: `aeon_evalops/runner.py` corre el pipeline
`DR-001`..`DR-005` completo por cada caso del dataset contra fixtures deterministas de
`decide`/`execute_tool` (ninguna llamada a un proveedor real, sin coste) y califica
`coverage_grader`/`citation_integrity_grader` con el resultado REAL de `evaluate_sufficiency`/
`verify_and_repair` — no un doble. Un suite sin harness offline registrado (`injection_suite`, hoy)
se reporta `SKIPPED` explícitamente, nunca con un puntaje inventado; el resumen `overall` distingue
`SKIPPED` de `PASS` para que eso no se lea como un éxito. `aeon eval run` (Go) delega al motor vía
`python -m aeon_evalops.cli` (interprete configurable vía `AEON_EVAL_PYTHON_BIN`, con `make eval-run`
como ruta contenedora) — es un puente real, aunque temporal: cuando el motor tenga su propio
servicio (como el Model Gateway tras `DR-001`), este `exec.Command` deja de ser necesario. "Provider
matrix" y "trace graders" quedan fuera de este primer Runner — ver `backlog.md`.

`EVAL-003` (Release Gates) añadió `aeon_evalops/release_gate.py::evaluate_release_gate`: compara
dos `SuiteReport` (el baseline del `Released` actual y el del candidato) y bloquea la promoción si
algún grader empeoró frente al baseline — **aunque el candidato siga pasando el umbral estático de
`EVAL-002`** (ese es exactamente el caso que prueba `test_release_gate_blocks_regression_even_when_
still_above_threshold`: 1.0→0.85 con umbral 0.8). Un grader `SKIPPED` en cualquiera de los dos lados,
o sin contraparte en el baseline, nunca bloquea ni desbloquea por sí solo. El Agent Registry en Go
(`go/internal/store/agent_registry.go`) ahora APLICA de verdad esa decisión: `TransitionLifecycle`
exige `ReleaseGateDecision.Allowed` para el paso Candidate→Released específicamente (ignorado en
cualquier otra transición) — probado contra Postgres real en `TestAgentRegistryReleaseGateBlocksPromotion`.
El registry nunca calcula el veredicto, sólo lo aplica — la misma separación Decision/workflow de
ADR-001.

`DX-001` (SDK Python) dio el salto que faltaba: hasta ahora `DR-001`..`DR-005` eran módulos puros
probados sólo con fakes, nunca ejecutados dentro de un workflow Temporal real. Ahora
`aeon_worker/workflows/deep_research_run.py::DeepResearchWorkflow` corre el pipeline completo
—Planner → Researchers en paralelo (`asyncio.gather` sobre Activities, igual que
`aeon_worker.graph._execute_parallel`) → Sufficiency Gate → Reporter → Citation Verifier— con cada
llamada a modelo/tool pasando por una Activity real (`aeon_worker/activities/
deep_research_activities.py`, más `decide`/`execute_tool` refactorizados en `model_activities.py`/
`tool_activities.py` para exponer su lógica como funciones planas invocables desde otra Activity, no
sólo vía `workflow.execute_activity`). El workflow en sí sólo toca lo determinista (`evaluate_
sufficiency`, `verify_and_repair`, ensamblar el Evidence Ledger) — nunca genera IDs aleatorios ni
hace I/O directo (ADR-001). Deliberadamente una sola pasada: si la Sufficiency Gate encuentra huecos,
el run igual produce reporte con lo permitido y devuelve `sufficient=False` + `topics_to_replan`, sin
replanificar automáticamente todavía (ver `backlog.md`).

`aeon_sdk.deep_research.start_deep_research_run` conecta a un Temporal real y arranca ese workflow;
`aeon_sdk.model_policy.resolve_candidates` resuelve `agent.yaml`'s `modelPolicy.profile` contra el
`model_policy_bundle.yaml` real (nunca un modelo hardcodeado). `examples/deep-research/run.py` es el
script real y ejecutable que los junta. La prueba de aceptación
(`test_deep_research_workflow_produces_a_verified_report_end_to_end`) corre contra un Temporal
efímero real y un worker en un proceso separado real — sólo el Model Gateway es un doble HTTP (nada
de esto pasa por fakes en memoria como los tests puros de `DR-001`..`DR-005`).

`DX-002` (CLI) completó `init` (scaffolding real de `agent.yaml`/`policy_bundle.yaml`/
`model_policy_bundle.yaml`, cada uno válido tal cual contra `aeon validate`), `run` (delega a
`<directorio>/run.py`, mismo patrón de `aeon eval run`), `trace` (consulta TraceQL real contra
Tempo, reutilizando el atributo `gen_ai.agent.name` de `OBS-001`), `replay` (historial real de
Temporal vía el cliente Go, sin diff/reejecución todavía — ver `backlog.md`) y `publish` (valida,
registra en el Agent Registry real y promueve Draft→Candidate; re-ejecutable de forma idempotente).
Verificado a mano contra el stack real (`--profile core --profile obs`): `init`→`validate`→
`publish` contra Postgres real, `trace` contra Tempo real, `replay` contra Temporal real — cada uno
con datos genuinos, no fixtures.

**Bug real encontrado y arreglado durante esta verificación:** el servicio `worker` de
`deploy/compose` llevaba roto desde `DX-001` — `aeon_worker/__main__.py` ahora importa
`deep_research_activities`, que a su vez importa `planner.py`/`decision.py`, y ambos cargan un
JSON Schema con una ruta relativa que asume un checkout completo del repo. La imagen del `worker`
sólo empaqueta `python/` (su build context), así que `proto/` nunca estuvo ahí — el worker
crasheaba al arrancar, antes incluso de conectar a Temporal, y nadie lo había notado porque ningún
test lo ejercitaba contra la imagen real de compose. Arreglado con la misma convención que ya usa
la CLI Go (`AEON_SCHEMAS_DIR`): ambos módulos Python la consultan primero, y `docker-compose.yml`
monta `proto/` de sólo lectura en el `worker` y la define. Verificado arrancando el `worker` real y
completando un run real de punta a punta.

`DX-003` (Template Deep Research) añadió `python/tests/unit/test_deep_research_template.py`: los 4
ficheros de config-as-code del template (`agent.yaml`, `policy_bundle.yaml`,
`model_policy_bundle.yaml`, `evals/suites/*.yaml`) se validan entre sí, no sólo cada uno contra su
propio schema — cada tool en `tools.allow` tiene un permit Cedar real que lo cubre, cada perfil de
`modelPolicy` (incluyendo fallbacks) existe en el bundle, y cada `evalGates` nombra un suite
realmente registrado. "Válido" y "ejecutable" ya estaban probados por `FND-003`/`DX-002`; esto
cierra el hueco de cohesión ENTRE ficheros que ninguno de los dos cubría. Se añadió también
`examples/deep-research/README.md`.

`INT-001` (`FrameworkAdapter` LangGraph) añadió `langgraph` como dependencia real (no un stand-in)
y `python/aeon_adapters/langgraph/adapter.py`: `run_langgraph_graph` construye un
`ModelGatewayChatClient`/`ToolGatewayCaller` — los únicos clientes que un nodo LangGraph recibe —
y corre el grafo compilado dentro de una única Activity (`run_langgraph_interop_activity`), nunca
en código de workflow, porque el bucle interno de LangGraph no es determinista (ADR-001, la misma
limitación documentada de Modo B: se replica el input/output de la Activity, no la trayectoria
interna del framework). `examples/langgraph-interop/` trae un grafo real de 2 nodos (`plan`→
`research`) que llama a esos clientes, nunca a un proveedor o tool directamente. Verificado con
Temporal efímero real + worker en proceso separado real, sólo el Model Gateway como doble HTTP
(mismo patrón que `DX-001`); también se reconstruyó y arrancó el `worker` real de compose con la
nueva dependencia para confirmar que no rompe nada.

`INT-002` (Endpoint OpenAI-compatible) añadió `POST /v1/chat/completions` a `aeon-modelgw`
(`go/internal/api/openai_compatible_handlers.go`): "model" en el request se interpreta como un
*capability profile* (nunca un modelo concreto, ADR-004), resuelto contra un `ModelPolicyBundle`
real cargado en Go (`modelgateway.ModelPolicyBundleDoc`, mismo patrón de carga config-as-code que
`aeon-toolgw`). La traducción de vuelta es casi trivial porque `NormalizedChatResponse` ya tiene
forma OpenAI por diseño — sólo hace falta envolver `id`/`object`/`created`. Verificado a mano con el
paquete **real** `openai` de Python apuntando a un `aeon-modelgw` real (`OpenAI(base_url=".../v1")`
→ `client.chat.completions.create(model="reasoning-test", ...)`), con un proveedor local falso
detrás — la respuesta se parseó como un `ChatCompletion` real, sin ningún cambio de código del lado
del cliente. "Routing" (incluyendo el filtro `data_sensitivity=restricted`) es real y probado;
"budgets" (aplicar límites de coste/tokens en este endpoint específico) queda pendiente — no hay
hoy ningún sitio en el Model Gateway que contabilice presupuesto por-agente, ver `backlog.md`.

**Con esto, las 13 features de F2 están `DONE`.** Pero el MVP como narrativa compuesta (roadmap
§7 original: informe con citas + traza navegable + coste por run + replay idéntico) todavía tiene
huecos honestos, ninguno bloqueante para pasar a F3: `DeepResearchWorkflow` (Python, `DX-001`) no
emite spans todavía — sólo los servicios Go (`OBS-001`) lo hacen, así que `aeon trace` no encuentra
nada para un run de Deep Research real; no hay coste-por-run (eso es `OBS-003`/FinOps, F4); y
`aeon replay` (`DX-002`) muestra historial real pero no reejecuta ni compara (`--assert-identical`,
en `backlog.md`). Se documentan como trabajo futuro explícito, no como huecos silenciosos.

| Fase | Nombre | % DONE | Estado |
|---|---|---|---|
| F0 | Foundation durable | ~94% (16/17) | `IN_PROGRESS` |
| F1 | Contexto y evidencia | 100% (9/9) | `DONE` |
| F2 | Deep Research + EvalOps (**MVP**) | 100% (13/13) | `DONE`* |
| F3 | Memoria gobernada | 100% (6/6) | `DONE` |
| F4 | Trust e interoperabilidad | ~43% (6/14) | `IN_PROGRESS` |
| F5 | Learning Lab | 0% | `TODO` |

Bloqueos abiertos: ninguno para el roadmap de features. Nota de entorno pendiente (última fila de
F0): la imagen oficial `vllm/vllm-openai` (perfil `local-llm`) requiere GPU/CUDA y falla en hosts
sin GPU — incluyendo el Mac Apple Silicon de este proyecto, verificado al levantar `--profile full`.
`make dev` con `PROFILE=core` o `PROFILE=obs` (core+obs, sin `local-llm`) está completamente
verificado y sano — es lo que este equipo usa día a día, con Prometheus como inferencia local real,
no vLLM. El supuesto sobre la API de Prometheus ya no está pendiente — resuelto con los hechos
reales, ver ADR-004 y la nota de `MDL-006` en F0 abajo.

`*` en F2: las 13 features con ID propio están `DONE` con test real. El MVP como narrativa
*compuesta* (informe + traza navegable + coste por run + replay idéntico, contra proveedor cloud o
local indistintamente) todavía no es 100% cierto en ese sentido más amplio — ver la nota al final
de la sección F2 abajo para el detalle exacto de qué falta y por qué no bloquea F3.

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
  uno de los 5 stubs.
  **Verificado en vivo** contra la instancia real del usuario (`127.0.0.1:8020`/`9000`, cliente
  `aeon-ai` creado vía `POST /admin/clients`): token OAuth2 real, `GET /v1/models/mine` real, y una
  llamada real de `chat/completions` contra `gpt-oss-20b-mxfp4` que devolvió una respuesta válida —
  las tres a través del cliente Go real (`go/internal/providers/prometheus_inference`), no `curl`.
  En el camino se encontró (y no era un bug de Prometheus, sino una confusión propia sobre el
  contrato) que **el scope `model:<id>` no se concede automáticamente** aunque el cliente esté
  autorizado para ese modelo — hay que pedirlo explícitamente en el `scope` de la petición de
  token, o `GET /v1/models/mine` devuelve una lista vacía aunque el modelo exista y el cliente
  tenga acceso. Documentado en `.env.example` y en el ADR. **Pendiente para pasar a `DONE`:** solo
  la suite `provider_conformance` (llega con `EVAL-002`/F2) — la verificación en vivo con un modelo
  real ya no es un pendiente. Ver [ADR-004](docs/adr/0004-model-gateway-provider-abstraction.md).

El resto de F0 (Model Gateway con el resto de proveedores, routing/fallback, tabla de dedupe del
Tool Gateway) siguen siendo stubs de scaffolding o TODO — ver filas abajo.

**F1 — arrancada:**
- `CTX-001` (Typed Context Lanes) — [test_context_lanes.py](python/tests/unit/test_context_lanes.py)
  prueba que la fidelidad de cada lane se **aplica**, no sólo se declara: `L0_POLICY`
  (`PINNED_EXACT`) sobrevive byte a byte sin reescritura ni truncado; `L2_EVIDENCE`
  (`EVIDENCE_ATOMIC`) rechaza cualquier entrada sin `claim_id`/`source_id`; `L3_EPISODIC`
  (`ADDRESSABLE`) acepta texto completo o un puntero `recall_id`, pero rechaza cualquier otra
  forma. El mapeo lane→fidelidad es fijo por diseño (`LANE_FIDELITY`, no configurable por llamada)
  para que sea estructuralmente imposible renderizar `L0` como si fuera resumible. La combinación
  completa de las 7 lanes se probó determinista: `assemble()` con el mismo input produce salida
  byte-idéntica, siempre en orden `L0..L6` fijo, sin importar el orden de inserción del dict de
  entrada — la propiedad de "función pura" que el plan exige para que el context assembly sea
  reproducible bajo replay. Fuera de alcance de `CTX-001` (llegan con `CTX-002..005`, `RAG-001`,
  `MEM-*`): budgeting/orden por cache-hit, offload de tool I/O grande, compactación tipada, y
  addressable recall — el módulo deja el punto de extensión (`_RENDERERS`) listo para cuando
  lleguen, sin necesidad de tocar el enforcement ya existente.
- `CTX-002` (Context Budgeter) — [test_context_budgeter.py](python/tests/unit/test_context_budgeter.py)
  prueba la propiedad económica real, no sólo el orden: el prefijo estable (`L0_POLICY` →
  `L6_SKILLS` → `L1_STATE`) sale **byte-idéntico** entre dos "turnos" que sólo difieren en las
  lanes volátiles (`L2_EVIDENCE`/`L3_EPISODIC`) — eso es justo lo que hace que un proveedor con
  prompt caching detecte el mismo prefijo y lo cobre más barato. El orden `CACHE_STABLE_ORDER` es
  deliberadamente distinto del orden `L0..L6` de `CTX-001` (ese es por fidelidad, éste es por
  economía de caché — ambos fijos, cada uno por su propia razón). Bajo presión de presupuesto se
  descarta primero lo más volátil (`L2`/`L3`), nunca `L0_POLICY`/`L1_STATE`. **Bug real encontrado
  y corregido en el camino:** el algoritmo greedy original podía exceder el presupuesto total si
  una lane opcional aparecía en el orden ANTES que una lane obligatoria (p. ej. `L6_SKILLS` antes
  que `L1_STATE`) — se colaba "creyendo" que sobraba espacio, y luego la lane obligatoria se
  añadía igual sin mirar el presupuesto. Se corrigió reservando el coste de las lanes obligatorias
  por adelantado, para que el orden de aparición nunca afecte si el presupuesto se respeta de
  verdad.
- `CTX-003` (Offload) — [test_context_offload.py](python/tests/unit/test_context_offload.py)
  prueba el criterio de aceptación literal de la spec §9: un documento de >50k tokens (`>50k×4`
  caracteres, mismo ratio que usa la estimación de tokens de `CTX-001`) nunca aparece completo en
  ningún sitio — ni en la entrada de la lane, ni (de punta a punta) en el texto ensamblado por
  `ContextAssembler`, sólo un puntero `{recall_id, summary}`. El contenido original se recupera
  byte a byte desde el `ObservationStore` por su `recall_id`. La implementación de storage para
  dev/test es un `FilesystemObservationStore` real (no un mock) sobre disco local, detrás de un
  `Protocol` — producción apuntará esto a MinIO/S3 sin cambiar la interfaz. `Addressable Recall`
  (`CTX-005`, `TODO`) es lo que falta para que una tool pueda pedir de vuelta el contenido completo
  por `recall_id` durante un run; `CTX-003` sólo resuelve la mitad de "guardar y apuntar".
- `CTX-004` (Typed Compaction) — [test_context_compaction.py](python/tests/unit/test_context_compaction.py)
  prueba el criterio de aceptación literal de la spec §9: `L0_POLICY` (`PINNED_EXACT`) sobrevive
  **50 ciclos** de compactación byte a byte, incluso con un target de compactación absurdamente
  ajustado (1 carácter) en cada ciclo, y aunque otras lanes en el mismo `dict` sí se compacten de
  verdad junto a ella. No hay "resumen uniforme": cada fidelidad tiene su propia regla —
  `PINNED_EXACT` es un no-op siempre; `EVIDENCE_ATOMIC` sólo puede descartar entradas completas
  (nunca reescribir una que sobrevive — se probó que la entrada superviviente queda byte-idéntica);
  `ADDRESSABLE` trunca el texto de entradas completas pero deja los punteros `recall_id` intactos
  (ya son mínimos, no hay nada que compactar). `compact_lane`/`compact_lanes` son funciones puras
  (no mutan su entrada), consistente con `ContextAssembler.assemble()` de `CTX-001` y
  `ContextBudgeter.budget()` de `CTX-002`.
- `CTX-005` (Addressable Recall) — [test_context_recall.py](python/tests/unit/test_context_recall.py)
  cierra el ciclo que `CTX-003` dejó abierto: `recall(recall_id, store, query=...)` recupera
  contenido real desde el `ObservationStore`, no un stub. Sin `query`, devuelve una ventana acotada
  desde el inicio (no el documento completo). Con `query`, hace una extracción real por ventana de
  palabra clave — encuentra la primera ocurrencia y devuelve sólo el entorno cercano, probado que
  es <10% del tamaño del documento original, no todo. Una `query` que no aparece en ningún sitio
  devuelve `matched=False` con contenido vacío — honesto sobre el "no encontrado" en vez de fingir
  una coincidencia. Un `recall_id` inventado falla con `RecallNotFoundError`, ruidoso a propósito.
  `recall_as_lane_entry()` reinyecta el fragmento recuperado como una entrada de lane válida:
  probado de punta a punta que el documento original de >50k tokens NUNCA aparece en el contexto
  ensamblado, sólo el fragmento pedido — la propiedad que hace útil todo el ciclo offload→recall.
  Búsqueda semántica real sobre contenido recuperado queda para `RAG-001` (`TODO`); esto es
  deliberadamente una extracción simple pero genuina, no un placeholder.
- `CTX-006` (Context Integrity Gate) — [test_context_integrity.py](python/tests/unit/test_context_integrity.py)
  cierra el subgrupo `CTX-*`: es el último chequeo antes de una llamada al modelo, sobre el
  contexto ya ensamblado por `CTX-001..005`. No es fuente de ninguna garantía por sí mismo —
  `IntegrityGate` detecta si algo aguas arriba falló silenciosamente: una constraint requerida
  (`required_constraints`) que ya no aparece en el texto ensamblado, un puntero `[recall:id]` cuyo
  `id` no está en el conjunto de `recall_id`s conocidos (referencia colgante), o un contexto que
  excede el presupuesto de tokens del modelo destino. `enforce()` lanza `IntegrityViolation` con
  las tres categorías de violación reportadas juntas si coinciden, no sólo la primera que
  encuentra — se probó explícitamente. Esto es el equivalente, del lado del contexto de ENTRADA,
  a lo que `DR-005` (Citation Verifier, `DONE`) hace del lado de la SALIDA del modelo.
- `RAG-001` (Retrieval Gateway) — [test_retrieval_gateway.py](python/tests/unit/test_retrieval_gateway.py)
  prueba el invariante que más importa: **el ACL lo aplica el gateway, nunca el connector**. Un
  documento fuera del scope del principal nunca llega, aunque sería el resultado más relevante
  (probado con un documento de baja relevancia dentro de scope ganando sobre uno de alta relevancia
  fuera de scope), y aunque el connector subyacente sí lo tenga de verdad (probado consultando el
  mismo connector con dos conjuntos de scopes distintos). Documentos sin `acl` son públicos por
  convención (nunca al revés — ausencia de ACL nunca significa "denegar por defecto" de forma
  silenciosa, sería peor que ruidosa). Búsqueda híbrida real (no un placeholder): solapamiento de
  términos + boost por frase exacta, ambos en Python puro, sin modelo de embeddings ni red — un
  connector real con pgvector (ya en el compose stack) se conecta detrás del mismo `Protocol`
  cuando haga falta. Caché probado de verdad: una consulta repetida no vuelve a invocar al
  connector, y la caché está particionada por `(query, scopes)` — dos principals con scopes
  distintos nunca comparten una entrada de caché, que sería una fuga de ACL a través de la caché.
- `RAG-002` (Evidence Extractor) — [test_evidence_extractor.py](python/tests/unit/test_evidence_extractor.py)
  prueba que los `EvidencePacket` producidos son válidos de verdad contra
  `evidence_packet.schema.json` (no sólo "tienen la forma correcta en Python"), no sólo que
  parsean. La extracción está genuinamente **condicionada a la query**: un documento sin contenido
  relevante produce cero packets, no uno relleno o inventado — se probó con un documento real sobre
  otro tema. El `quote` de cada packet es siempre texto verbatim que existe literalmente en el
  documento fuente (probado con `in`, no sólo asumido). La procedencia (`claim_id`, `retrieved_at`)
  la genera siempre este módulo, nunca la estrategia de extracción — que sólo propone texto de
  claim/quote, nunca su propia identidad o timestamp. Los scores de `RAG-001` (que pueden superar
  1.0 por el boost de frase exacta) se recortan al rango `[0,1]` que exige el schema para
  `source_quality`/`confidence` — sin este recorte, un score alto de relevancia produciría un
  packet inválido. La estrategia de extracción real usada aquí (`SentenceMatchExtractionStrategy`)
  es simple pero genuina (sentencias con términos de la query, verbatim) — una estrategia respaldada
  por modelo (compactación condicionada de verdad) es un reemplazo directo detrás del mismo
  `Protocol`, sin tocar `extract_evidence`.
- `RAG-003` (Evidence Ledger) — [test_evidence_ledger.py](python/tests/unit/test_evidence_ledger.py)
  cierra F1. El Ledger **no detecta** contradicciones (eso es trabajo de un modelo, igual que la
  extracción real de `RAG-002`) — las **preserva y agrupa** cuando el caller identifica qué
  `claim_id`s se contradicen, exactamente como pide la spec: "las contradicciones se preservan",
  nunca se resuelven en silencio. Probado el caso real: una tercera claim que nombra a dos
  miembros ya agrupados se une al MISMO grupo, no crea uno nuevo (fragmentar sería peor que no
  agrupar). Nombrar un `claim_id` inexistente falla con `UnknownClaimError`, a propósito ruidoso.
  Dedupe real por `(source_id, quote)` — el mismo hecho reportado dos veces por la misma fuente no
  duplica entrada. `source_quality` por fuente es un promedio real sobre los packets vistos, no un
  valor fijo. **F1 completa: 9/9, con `aeon_context/` (lanes, budgeter, offload, recall, integrity)
  y `aeon_evidence/` (retrieval, extractor, ledger) formando el pipeline completo de contexto y
  evidencia — desde el ensamblado con fidelidad tipada hasta la persistencia de evidencia con
  contradicciones preservadas.**

**F0 — retomada para terminar lo pendiente antes de F2:**
- `MDL-001` (Model Gateway) — [gateway_test.go](go/internal/modelgateway/gateway_test.go) prueba
  routing y fallback reales: candidatos probados en orden de `Priority` (no del orden en que
  llegan en el slice), fallback automático al siguiente candidato cuando uno falla (con el log de
  intentos completo, incluido el que falló), y un nombre de proveedor no registrado cae al
  siguiente candidato en vez de entrar en pánico. `data_sensitivity: restricted`
  (`model_profile.schema.json`) fuerza `prometheus_inference` como único candidato posible **antes**
  de empezar a rutear — un candidato cloud con mayor prioridad ni siquiera se intenta, no es sólo
  despriorizado; y si no hay ningún candidato local configurado, falla con un error claro
  (`ErrNoRestrictedCandidate`) en vez de caer silenciosamente a un proveedor cloud. El Gateway
  depende únicamente de la interfaz `providers.Provider` — nunca importa un adaptador concreto.
  De paso se añadió `providers.NormalizedChatResponse()`: la única forma de salida que cualquier
  adaptador devuelve (`choices[].message.content`, `usage.*`), sin importar el formato real del
  proveedor — es lo que hace mecánica la futura conformidad cruzada entre proveedores.
- `MDL-003` (Adaptador `anthropic`, **IN_PROGRESS** — falta la suite `provider_conformance`) —
  [anthropic_test.go](go/internal/providers/anthropic/anthropic_test.go) prueba un cliente HTTP
  real contra la Messages API documentada (`POST /v1/messages`, headers `x-api-key` +
  `anthropic-version`), no un mock superficial. Traducción de request real: un mensaje con
  `role: system` se extrae al campo `system` de nivel superior (Anthropic no tiene rol "system" en
  `messages`), y `max_tokens` (obligatorio en esta API, a diferencia de OpenAI) se rellena con un
  default si el caller no lo especifica. Traducción de response real: los content blocks de
  Anthropic se concatenan y se devuelven en la forma normalizada común
  (`providers.NormalizedChatResponse`), no en el shape nativo de Anthropic. Un API key incorrecto
  falla con el 401 real del servidor falso, no un error genérico.
- `MDL-004` (Adaptador `openai`, **IN_PROGRESS** — falta la suite `provider_conformance`) —
  [openai_test.go](go/internal/providers/openai/openai_test.go) prueba el cliente HTTP real contra
  `POST /v1/chat/completions` con `Authorization: Bearer`. El shape de OpenAI es el que ya
  inspiró la convención común del gateway, así que aquí la traducción es casi paso directo — pero
  la respuesta igual se re-envuelve explícitamente en `providers.NormalizedChatResponse`, igual que
  cualquier otro adaptador, para que ningún caller necesite saber que está hablando con OpenAI en
  vez de con otro proveedor.
- `MDL-005` (Adaptador `gemini`, **IN_PROGRESS** — falta la suite `provider_conformance`) —
  [gemini_test.go](go/internal/providers/gemini/gemini_test.go) prueba el cliente HTTP real contra
  `POST /v1beta/models/{model}:generateContent`. Tres traducciones reales verificadas: el modelo va
  en la URL, no en el body; los roles `user`/`assistant` de OpenAI se traducen a `user`/`model` (el
  vocabulario propio de Gemini); y un mensaje `system` se extrae a `systemInstruction` de nivel
  superior, igual que Anthropic pero con su propio campo. **Decisión de seguridad deliberada:** el
  contrato documentado de Gemini pasa la API key como query param (`?key=...`), pero este adaptador
  usa el header `x-goog-api-key` en su lugar (Gemini soporta ambos) — un secreto nunca debe viajar
  en una URL que termina en logs/historial. Se probó explícitamente que la key nunca aparece en la
  URL de la request real.
- `MDL-007` (Adaptador `openai_compatible`, **IN_PROGRESS** — falta la suite `provider_conformance`)
  — [openai_compatible_test.go](go/internal/providers/openai_compatible/openai_compatible_test.go)
  prueba el fallback genérico para vLLM/Ollama/TGI/LM Studio: mismo shape de request/response que
  `openai`, pero sin las suposiciones que no valen para un servidor self-hosted — sin `BaseURL` por
  defecto (falla explícitamente si no se configura, no hay "el" endpoint self-hosted), y sin API
  key obligatoria (probado que funciona sin ninguna, que es la configuración por defecto de
  vLLM/Ollama; si se configura una, se envía como Bearer).
- **`provider_conformance` (versión mínima)** — [conformance_test.go](go/internal/providers/conformance_test.go)
  corre el mismo request representativo contra los 5 adaptadores reales (cada uno contra un
  servidor falso que replica su contrato documentado exacto) y verifica que todos devuelven
  exactamente la misma forma de salida (`choices[].message.content`/`role`, `usage.*` con enteros
  reales) sin importar el formato nativo del proveedor. **Bug real encontrado y corregido en el
  camino:** `prometheus_inference` fallaba esta prueba — a diferencia de los otros 4 adaptadores
  (que decodifican en un struct tipado antes de construir `NormalizedChatResponse`),
  `prometheus_inference` reenviaba el JSON crudo decodificado genéricamente, donde
  `encoding/json` de Go decodifica todo número como `float64`, no `int` — un consumidor que
  esperara `usage.prompt_tokens` como entero real (p. ej. para sumarlo a un contador de budget)
  se habría roto en silencio. Corregido para que también decodifique en un struct tipado y pase
  por `NormalizedChatResponse` como el resto. Esto cierra el criterio de salida de fase de F0 (en
  su versión mínima) y permite marcar `MDL-003..007` como `DONE` — la versión completa de
  `provider_conformance` (tool calling real, structured output, constraint respect, contexto
  largo, rechazo de inyección) llega con `EVAL-002`/F2.

---

## F0 — Foundation durable (semanas 1-4)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| FND-001 | Agent Registry (CRUD/versionado, lifecycle Draft→Candidate→Released→Retired) | `DONE` | `TestAgentRegistryLifecycle` en verde | go/internal/store/agent_registry_test.go |
| FND-003 | Config-as-code (manifiestos en Git, UI no es source of truth) | `DONE` | `TestAeonValidateAcceptsAndRejectsExampleManifests` en verde | go/cmd/aeon/main_test.go |
| RUN-001 | Run Controller (start/cancel/pause/resume/status/stream) | `DONE` | `TestRunControllerLifecycle` en verde | go/internal/api/run_controller_handlers_test.go |
| RUN-002 | Graph Runtime (sequential/parallel/conditional/loop/subgraph/fan-in) | `DONE` | `test_graph_runtime_node_kinds` en verde | python/tests/integration/test_graph_runtime.py |
| RUN-003 | Budgets (tokens/calls/tools/cost/deadline/depth, hard stop) | `DONE` | `test_budget_hard_stop` en verde (tool_calls/depth/deadline; model_calls/tokens/cost_usd declarados, no aplicados — el Graph Runtime en Python todavía no llama al Model Gateway en Go, esa integración es trabajo futuro) | python/tests/integration/test_budget_hard_stop.py |
| RUN-004 | Checkpoint & replay (resume sin duplicar tool effects) | `DONE` | `test_crash_resume_no_duplicate_write` en verde | python/tests/integration/test_crash_resume.py |
| RUN-005 | Approvals (interrupt durable, parameter binding, expiry) | `DONE` | `test_approval_binding` en verde (aprobado, rechazado, hash no coincide, expira) | python/tests/integration/test_approval_binding.py |
| MDL-001 | Model Gateway (capability profiles, adapters, fallback, routing) | `DONE` | `TestModelGatewayRoutingFallback` en verde | go/internal/modelgateway/gateway_test.go |
| MDL-003 | Adaptador `anthropic` | `DONE` | pasa `provider_conformance` (versión mínima) | go/internal/providers/conformance_test.go, go/internal/providers/anthropic/anthropic_test.go |
| MDL-004 | Adaptador `openai` | `DONE` | pasa `provider_conformance` (versión mínima) | go/internal/providers/conformance_test.go, go/internal/providers/openai/openai_test.go |
| MDL-005 | Adaptador `gemini` | `DONE` | pasa `provider_conformance` (versión mínima) | go/internal/providers/conformance_test.go, go/internal/providers/gemini/gemini_test.go |
| MDL-006 | Adaptador `prometheus_inference` (LLM local) | `DONE` | pasa `provider_conformance` (versión mínima); verificado también en vivo contra una instancia real | go/internal/providers/conformance_test.go, go/internal/providers/prometheus_inference/prometheus_inference_test.go |
| MDL-007 | Adaptador `openai_compatible` (vLLM/Ollama/TGI genérico) | `DONE` | pasa `provider_conformance` (versión mínima) | go/internal/providers/conformance_test.go, go/internal/providers/openai_compatible/openai_compatible_test.go |
| TOOL-001 | Tool Registry/Gateway (typed schemas, risk classification, scopes) | `DONE` | `TestToolRegistryCRUDAndRiskClassification` (registry) + `TestToolPolicyDeniesOutOfManifestToolCall` (gateway ejecuta con policy check real) | go/internal/store/tool_registry_test.go, go/internal/api/tool_gateway_handlers_test.go |
| SEC-001 | Policy Engine (Cedar, authz fuera del modelo) | `DONE` | `TestToolPolicyDeniesOutOfManifestToolCall` en verde | go/internal/api/tool_gateway_handlers_test.go |
| OBS-001 | Distributed tracing (OTel GenAI semantic conventions) | `DONE` | `TestDistributedTracingSpansReachTempo` en verde — spans reales `invoke_agent`/`chat`/`execute_tool` emitidos por el Run Controller/Model Gateway/Tool Gateway, exportados por un OTel Collector real y encontrados en una Tempo real vía TraceQL | go/internal/api/tracing_integration_test.go |
| — | `deploy/compose` completo (Temporal, Postgres+pgvector, MinIO, OTel, Tempo, Grafana) | `IN_PROGRESS` | `make dev` levanta todos los servicios sanos | — |

**Nota sobre la fila `deploy/compose` completo:** perfiles `core` y `obs` (Temporal, Postgres,
MinIO, OTel Collector, Tempo, control plane y gateways) verificados sanos end-to-end durante el
trabajo de `OBS-001`. El perfil `local-llm` (`vllm/vllm-openai`) requiere GPU/CUDA y no arranca en
un host sin GPU — no es un defecto de este compose sino una limitación de esa imagen oficial; no
bloquea a F2, que puede correr con `PROFILE=core`/`PROFILE=obs` más Prometheus como inferencia
local real. Queda `IN_PROGRESS` en vez de `DONE` porque el criterio tal como está escrito ("todos
los servicios") no se puede prometer para `local-llm` en este tipo de host; requiere una decisión
explícita (¿excluir `vllm` del criterio, o sustituirlo por un servidor CPU-compatible?) antes de
cerrarse — ver `backlog.md`.

**Salida de fase — cumplida:** `test_crash_resume_no_duplicate_write` en verde ✅;
`TestProviderConformance` prueba que el mismo request representativo pasa contra los 5 adaptadores
(cloud + local) con la misma forma normalizada de salida ✅ — es la versión **mínima** de
`provider_conformance`, no la suite completa de `EVAL-002`/F2 (tool calling real, structured
output, respeto de constraints, contexto largo, rechazo de inyección), que queda para F2/EVAL-002;
`TestDistributedTracingSpansReachTempo` en verde ✅. Sólo la nota de `deploy/compose`/`local-llm`
de arriba queda abierta, sin bloquear F2.

## F1 — Contexto y evidencia (semanas 5-8)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| CTX-001 | Typed Context Lanes (L0-L6) | `DONE` | `test_lane_fidelity_policy` en verde | python/tests/unit/test_context_lanes.py |
| CTX-002 | Context Budgeter (ensamblado por prioridad + cache-hit) | `DONE` | `test_budgeter_cache_stable_ordering` en verde | python/tests/unit/test_context_budgeter.py |
| CTX-003 | Offload (tool I/O grande → observation store + puntero) | `DONE` | `test_no_full_document_injection` en verde | python/tests/unit/test_context_offload.py |
| CTX-004 | Typed Compaction (fidelity policy por lane, no resumen uniforme) | `DONE` | `test_pinned_exact_survives_stress` en verde | python/tests/unit/test_context_compaction.py |
| CTX-005 | Addressable Recall (IDs estables, `context.recall`) | `DONE` | `test_addressable_recall_roundtrip` en verde | python/tests/unit/test_context_recall.py |
| CTX-006 | Context Integrity Gate (constraint/citation/token checks pre-model-call) | `DONE` | `test_integrity_gate_blocks_missing_constraint` en verde | python/tests/unit/test_context_integrity.py |
| RAG-001 | Retrieval Gateway (connectors, ACL, hybrid search, rerank, cache) | `DONE` | `test_retrieval_acl_enforced` en verde | python/tests/unit/test_retrieval_gateway.py |
| RAG-002 | Evidence Extractor (compactación condicionada → EvidencePacket) | `DONE` | `test_evidence_packet_schema_valid` en verde | python/tests/unit/test_evidence_extractor.py |
| RAG-003 | Evidence Ledger (provenance, dedupe, contradictions, source quality) | `DONE` | `test_ledger_contradiction_grouping` en verde | python/tests/unit/test_evidence_ledger.py |

**Salida de fase — cumplida:** documento >50k tokens nunca entra completo (`test_no_full_document_injection`);
100% de `PINNED_EXACT` y locators sobreviven 50 ciclos de compactación (`test_pinned_exact_survives_stress`).
F1 completa: 9/9 features, todas con test de aceptación real en verde — `aeon_context/` (lanes,
budgeter, offload, recall, integrity) y `aeon_evidence/` (retrieval, extractor, ledger).

## F2 — Deep Research + EvalOps → MVP (semanas 9-11)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| DR-001 | Research Planner (3-5 subtareas, coverage, budgets) | `DONE` | familia `test_planner_subtask_bounds` en verde (el bound 3-5 está en `research_plan.schema.json`, no en código de aplicación) | python/tests/unit/test_planner.py |
| DR-002 | Isolated Researchers (parallel worker contexts, bounded ReAct) | `DONE` | `test_researcher_isolation` en verde | python/tests/unit/test_researcher.py |
| DR-003 | Sufficiency Gate (coverage matrix, contradiction gate, replanning) | `DONE` | familia `test_sufficiency_gate_replans` en verde | python/tests/unit/test_sufficiency_gate.py |
| DR-004 | Tool-less Reporter (output sólo desde allowed_claim_ids) | `DONE` | `test_reporter_no_tools_available` en verde | python/tests/unit/test_reporter.py |
| DR-005 | Citation Verifier (claim-to-evidence, repair-from-ledger) | `DONE` | familia `test_reporter_cannot_invent_citations` en verde | python/tests/unit/test_citation_verifier.py |
| EVAL-001 | Eval Registry (datasets, graders, thresholds, versions) | `DONE` | `TestAeonEvalListShowsSuitesFromEvalsDir` en verde — `aeon eval list` muestra las 4 suites reales de `evals/suites` (las mismas que nombra `examples/deep-research/agent.yaml`'s `evalGates`) | go/cmd/aeon/main_test.go |
| EVAL-002 | Eval Runner (offline/repeated trials/provider matrix/trace graders) | `DONE` | `aeon eval run deep_research_core` produce reporte real — ver `test_eval_run_produces_a_report_for_deep_research_core` (motor) y `TestAeonEvalRun` (CLI) en verde. Sólo "offline" y "repeated trials" son reales hoy; "provider matrix" y "trace graders" quedan en `backlog.md` | python/tests/unit/test_eval_runner.py, go/cmd/aeon/main_test.go |
| EVAL-003 | Release Gates (bloquear promoción por regresión) | `DONE` | `test_release_gate_blocks_regression_even_when_still_above_threshold` en verde (motor) + `TestAgentRegistryReleaseGateBlocksPromotion` en verde (aplicación real en el registry) | python/tests/unit/test_release_gate.py, go/internal/store/agent_registry_test.go |
| DX-001 | SDK Python (`start_deep_research_run`, primer workflow real DR-001..DR-005) | `DONE` | `test_deep_research_workflow_produces_a_verified_report_end_to_end` en verde — pipeline completo real contra Temporal + worker real, sólo el Model Gateway es un doble HTTP. Alcance: `aeon_sdk.deep_research`/`aeon_sdk.model_policy` (Deep Research únicamente); un `start_run(manifest)` genérico y tools/context/memory/traces/approvals como superficie SDK propia quedan en `backlog.md` | python/tests/integration/test_deep_research_workflow.py, examples/deep-research/run.py |
| DX-002 | CLI (init/validate/run/eval/trace/replay/publish) | `DONE` | los 7 subcomandos ejecutan sin error contra el compose real — verificado a mano (`init`/`validate`/`publish` contra Postgres real, `trace` contra Tempo real, `replay` contra Temporal real) más `TestAeonInit`/`TestAeonRun`/`TestAeonTrace`/`TestAeonReplay`/`TestAeonPublish` en verde | go/cmd/aeon/dx002.go, go/cmd/aeon/dx002_test.go |
| DX-003 | Template Deep Research | `DONE` | válido (`TestAeonValidateAcceptsAndRejectsExampleManifests`) y ejecutable (`test_examples_deep_research_run_script_produces_a_report`, DX-002) — más `test_every_allowed_tool_has_a_matching_cedar_permit` y familia: los 4 ficheros de config-as-code del template (agent/policy/model_policy/evalGates) se validan mutuamente entre sí, no sólo cada uno contra su propio schema | python/tests/unit/test_deep_research_template.py, examples/deep-research/README.md |
| INT-001 | `FrameworkAdapter` LangGraph (Modo B) | `DONE` | `test_langgraph_interop_graph_runs_inside_a_real_activity` en verde — un `langgraph.graph.StateGraph` real (dependencia real, no un stand-in) corre dentro de una única Activity real, sus dos nodos llamando a los clientes reales del Model/Tool Gateway | python/tests/integration/test_langgraph_interop_workflow.py, python/aeon_adapters/langgraph/adapter.py |
| INT-002 | Endpoint OpenAI-compatible del Model Gateway (Modo C) | `DONE` | `TestOpenAICompatibleChatCompletions` en verde — verificado también a mano con el paquete real `openai` de Python apuntando a un `aeon-modelgw` real, resuelto vía un `ModelPolicyBundle` real. "Routing" es real; "budgets" (aplicar límites en este endpoint) queda pendiente, ver `backlog.md` | go/internal/api/openai_compatible_handlers.go, go/internal/api/openai_compatible_handlers_test.go |

**MVP:** `make dev && aeon run examples/deep-research --query "…"` produce informe con citas
verificadas, traza navegable, coste por run y `aeon replay <run_id>` idéntico — contra proveedor
cloud o local.

## F3 — Memoria gobernada (semanas 12-17)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| MEM-001 | Memory Store (typed/scoped/versioned, provenance/TTL/status) | `DONE` | `TestMemoryRecordSchemaValid` en verde — un `MemoryRecord` escrito por el `MemoryStore` real (Postgres real) contra `hash`/`provenance_hmac` calculados por el propio store (nunca confiados de quien llama) valida contra `memory_record.schema.json`. Además `TestMemoryStoreCreateRejectsDirectActiveWrite` (sólo `CANDIDATE`/`QUARANTINED` son escribibles al crear — la promoción a `ACTIVE` es MEM-002), `TestMemoryStoreListActiveRespectsScopeTenantAndTTL` (lectura gobernada por scope/tenant/TTL real) y `TestMemoryStoreVerifyProvenanceDetectsTampering` (detección real de manipulación de contenido/HMAC). Sin superficie HTTP todavía — eso llega con MEM-002, igual que DR-001..004 fueron módulos puros antes de DX-001 | go/internal/store/memory_store.go, go/internal/store/memory_store_test.go |
| MEM-002 | Memory Candidate Pipeline (quarantine→validate→promote/reject) | `DONE` | `TestMemoryWriteModeCandidateOnly` en verde — `WriteCandidate` (la única vía de escritura de un agente/run) fuerza `status=CANDIDATE` sin importar qué status intente colar quien llama; `AgentManifest.spec.memoryPolicy.writeMode: candidate_only` queda aplicado, no sólo documentado. Además la máquina de estados real `quarantine→validate→promote/reject` (`TestMemoryPipelineHappyPathQuarantineValidatePromote`, `TestMemoryPipelineValidateBlockedWithoutDecision`, `TestMemoryPipelinePromoteBlockedWithoutDecision`, `TestMemoryPipelineRejectFromEachPreActiveStatus`, `TestMemoryPipelineInvalidTransitionsAreRejected`) contra Postgres real | go/internal/store/memory_pipeline.go, go/internal/store/memory_pipeline_test.go |
| MEM-003 | Reflection (post-run candidate extraction) | `DONE` | `test_reflection_extracts_candidates` en verde — un `RunSummary` (outcome + evidence_refs reales del run, nunca chain-of-thought) produce candidatos vía `decide` falso, grounding forzado (`test_reflection_rejects_a_candidate_citing_an_evidence_ref_the_run_never_produced`, mismo principio que el Citation Verifier de DR-005 aplicado a memoria). Además la superficie HTTP del Memory Store (MEM-001/002) quedó expuesta en `aeon-controlplane` (`TestMemoryHandlersFullPipelineOverHTTP`, verificado también a mano contra un contenedor real: candidates→quarantine→validate→promote→active sobre HTTP real) — cierra el hueco de `backlog.md` que decía "empezar MEM-003" | python/aeon_memory/reflection.py, python/tests/unit/test_reflection.py, go/internal/api/memory_handlers.go, go/internal/api/memory_handlers_test.go |
| MEM-005 | Utility/Forgetting (decay, prune, supersede/revoke) | `DONE` | `TestMemoryDecayPrunesStale` en verde — un `MemoryStore.Prune` real revoca una memoria `ACTIVE` real cuyo `utility_score` decaído (`DecayedUtility`, decaimiento exponencial real desde `last_used_at`) cae bajo el umbral, y deja de aparecer en `ListActive`. Además `RecordUsage` (ajusta `utility_score` con uso real, nunca negativo), `Supersede` (ACTIVE→SUPERSEDED, enlaza `superseded_by`) y `Revoke` (ACTIVE→REVOKED explícito) — una máquina de estados separada de la de MEM-002 (`memoryPostActiveTransitions`), a propósito: `Reject` de MEM-002 sigue sin poder tocar un `ACTIVE` | go/internal/store/memory_forgetting.go, go/internal/store/memory_forgetting_test.go |
| SEC-004 | Memory security (isolation, poisoning tests, repair/revocation) | `DONE` | `TestMemoryPoisoningSuite` en verde — 6 escenarios de ataque reales cargados desde `evals/datasets/memory_poisoning.jsonl` (la misma `memory_poisoning` `EvalSuite` real que `aeon eval list` ya muestra), cada uno contra Postgres real: escritura directa a `ACTIVE` forzada a `CANDIDATE`; contenido manipulado fuera del store detectado (`RepairIfTampered`) y revocado automáticamente; aislamiento cruzado de tenant en lectura por id, `ListActive` y revocación; una memoria revocada desaparece de `ListActive`. Aislamiento e integridad verificados también sobre HTTP real (`TestMemoryHandlersGetIsIsolatedByTenant`, `TestMemoryHandlersRevokeIsIsolatedByTenant`, `TestMemoryHandlersRepairDetectsTamperingAndRevokes`) y a mano contra un `aeon-controlplane` real: contenido tamperado directamente en Postgres (`UPDATE` vía `psql`, sin pasar por la API) fue detectado y revocado por `/memory/{id}/repair` | go/internal/store/memory_security.go, go/internal/store/memory_poisoning_test.go, go/internal/api/memory_handlers.go, go/internal/api/memory_handlers_test.go, evals/suites/memory_poisoning.yaml, evals/datasets/memory_poisoning.jsonl |
| EVAL-004 | Learning Eval (forward/negative transfer, usefulness, staleness) | `DONE` | `test_eval_run_produces_a_report_for_learning_eval` en verde — la suite real `learning_eval` (config-as-code, `aeon eval list` la muestra) corre offline y produce un reporte real `PASS` (`forward_transfer_grader`/`negative_transfer_grader`, ambos 1.000), verificado también a mano vía `make eval-run SUITE=learning_eval`. La lógica pura (`aeon_evalops/learning_eval.py`) cubre las 4 dimensiones del criterio: forward/negative transfer (probes con y sin memoria inyectada) y usefulness/staleness (`DecayedUtility` de MEM-005 como entrada, con un piso de staleness que anula el transfer aunque el probe "mejorara"). Además calcula `ValidationDecision`/`PromotionDecision` reales — cierra el hueco que MEM-002 dejó abierto (antes sólo los tests los construían a mano) | python/aeon_evalops/learning_eval.py, python/tests/unit/test_learning_eval.py, python/tests/unit/test_eval_runner.py, evals/suites/learning_eval.yaml, evals/datasets/learning_eval.jsonl |

MEM-001 se implementó en Go (`go/internal/store`), siguiendo el mismo patrón que el Agent/Tool
Registry (FND-001/TOOL-001): Postgres real, CHECK constraints por enum, y validación estructural en
código antes de tocar la base. Dos decisiones deliberadas: (1) `hash` (integridad del contenido) y
`provenance_hmac` (integridad de la procedencia, con una clave real vía `AEON_MEMORY_HMAC_KEY`) los
calcula siempre el propio `MemoryStore`, nunca quien llama — igual que `provenance_hmac` ya decía en
el comentario del schema desde F0 — de forma que `VerifyProvenance` puede detectar más tarde
contenido alterado fuera del store, sentando la base real de SEC-004; (2) `Create` sólo admite
`status` `CANDIDATE` o `QUARANTINED` — escribir directamente `ACTIVE`/`VALIDATED`/`SUPERSEDED`/
`REVOKED` se rechaza con `ErrDirectActiveWriteRejected`, tal como el propio
`memory_record.schema.json` prometía desde que se creó el fichero. La lectura gobernada
(`ListActive`) filtra por `scope`+`tenant_id`+`status=ACTIVE`+TTL no expirado — cierra el ciclo de
"provenance/TTL/status" del criterio, no sólo los campos existiendo en una tabla. Lo que falta
explícitamente para F3: el propio pipeline `quarantine→validate→promote/reject` (MEM-002) que es la
única vía real hacia `ACTIVE`, y una superficie HTTP/SDK para que un run Python pueda leer/escribir
memoria — ninguna de las dos bloquea llamar a MEM-001 `DONE` (mismo criterio que se aplicó a
DR-001..004 en F2).

MEM-003 (Reflection) es un módulo Python puro (`python/aeon_memory/reflection.py`), sin import de
Temporal, con el mismo patrón que Planner/Researcher/Reporter: `decide` inyectado, directamente
testeable con un doble. Su contrato de entrada (`RunSummary`) es deliberadamente estrecho — sólo
`run_id`/`query`/`outcome`/`report_text`/`evidence_refs`, nunca una transcripción completa —
cumpliendo roadmap.md §6 ("sin almacenar chain-of-thought") desde el primer campo, no como filtro
posterior. El grounding es la misma disciplina que DR-004/DR-005 aplican a citas: un candidato que
referencia un `evidence_ref` que el run nunca produjo se rechaza entero (`ReflectionError`), nunca
se repara ni se deja pasar parcialmente.

También en este PR: la superficie HTTP que MEM-001/MEM-002 habían dejado pendiente
(`go/internal/api/memory_handlers.go`, montada en `aeon-controlplane`) — `POST /memory/candidates`
(fuerza `status=CANDIDATE` sin importar qué status pida el cuerpo, mismo principio que
`WriteCandidate`), `GET /memory/active`, `GET /memory/{memory_id}`, y
`POST /memory/{memory_id}/{quarantine,validate,promote,reject}`. `aeon-controlplane` ahora requiere
`AEON_MEMORY_HMAC_KEY` (con un default inseguro sólo para `make dev`, ver `.env.example`).
Verificado con Postgres real vía `TestMemoryHandlersFullPipelineOverHTTP` y, a mano, con `curl` real
contra un contenedor `aeon-controlplane` real: un intento de colar `status: ACTIVE` en el `POST`
inicial volvió como `CANDIDATE`, y el pipeline completo (`quarantine`→`validate`→`promote` bloqueado
sin decisión→`promote` permitido→visible en `/memory/active`) funcionó de punta a punta sobre HTTP
real. Reflection en sí todavía no está conectada a ningún workflow (nadie llama todavía a
`Reflector.reflect` desde `DeepResearchWorkflow` ni escribe sus candidatos vía
`POST /memory/candidates`) — eso es la extensión natural de `DX-001` a un run real, ver
`backlog.md`.

MEM-005 (Utility/Forgetting) añade una segunda máquina de estados post-ACTIVE
(`memoryPostActiveTransitions`) deliberadamente separada de la de MEM-002 (`memoryValidTransitions`)
— `transitionStatus` ahora recibe qué mapa aplicar, en vez de ser una única tabla global. Esto
preserva intacta una garantía que MEM-002 ya probaba (`Reject` nunca toca un registro `ACTIVE`,
sigue en verde sin cambios) mientras `Supersede`/`Revoke` (nuevos, MEM-005) sólo aceptan un origen
`ACTIVE`. `DecayedUtility` es una función pura (decaimiento exponencial con vida media
configurable) que `Prune` usa sobre `last_used_at` (columna nueva, poblada por `RecordUsage`, no
por ninguna transición de pipeline) — así que "olvidar" está gobernado por uso real, no por edad
del registro en sí. `Prune` nunca borra una fila: revoca (`REVOKED`), preservando el registro para
auditoría, igual que todo lo demás en este store.

Nota técnica real encontrada durante la verificación: añadir `last_used_at`/`superseded_by` a
`memory_records` requirió una migración aditiva real (`ALTER TABLE ... ADD COLUMN IF NOT EXISTS`,
`schema.sql`) porque `CREATE TABLE IF NOT EXISTS` es un no-op sobre una tabla que ya existía en el
volumen Postgres persistente de sesiones anteriores — la primera vez que este schema necesitó
historia real, no sólo creación inicial. Verificar esto expuso un bug de concurrencia genuino:
`go test ./...` corre cada paquete como proceso separado contra el mismo Postgres, y varios
`Migrate()` concurrentes ejecutando ALTER TABLE con una FK auto-referenciada produjeron un
deadlock real (`SQLSTATE 40P01`), reproducido y confirmado. Arreglado con un
`pg_advisory_lock`/`pg_advisory_unlock` real alrededor de `Migrate()`, sobre una única conexión
tomada explícitamente del pool (`go/internal/store/postgres.go`) — verificado estable en 4
ejecuciones consecutivas de la suite completa tras el fix.

SEC-004 (Memory security) diseñó su propio suite de forma distinta a `injection_suite`
(EVAL-001/002): como la lógica de defensa real vive en Go (`go/internal/store`,
`go/internal/api`), no en Python, `evals/suites/memory_poisoning.yaml` documenta el suite como
config-as-code de la forma habitual (dataset, graders, thresholds, `gateOn`) pero su implementación
real y en verde es un test de integración Go (`memory_poisoning_test.go`) que **carga el mismo
`evals/datasets/memory_poisoning.jsonl`** como fuente de sus casos — así el dataset y su
verificación nunca pueden divergir en silencio: si el dataset nombra un `case_id` sin defensa
implementada, el test falla con un mensaje explícito en vez de saltarse el caso calladamente
(la misma regla de "no silent caps" que ya se sigue en otros sitios del roadmap). Dos hallazgos
reales durante el diseño: (1) `MemoryStore.Get`/`Revoke` no tenían ninguna verificación de
`tenant_id` — cualquiera que conociera un `memory_id` (un UUID, pero no un secreto) podía leerlo o
revocarlo sin importar el tenant; arreglado en la capa HTTP (`go/internal/api/memory_handlers.go`,
que es donde de verdad se establece la identidad de quien llama), devolviendo 404 —nunca 403— en un
cruce de tenant, para no confirmar siquiera que el registro existe; (2) "repair" para memoria
envenenada significa revocar, nunca reconstruir contenido de confianza — mismo principio que el
Citation Verifier de DR-005 (nunca inventa una cita, repara desde el ledger), aplicado aquí a
integridad de memoria: `RepairIfTampered` recalcula hash/HMAC y revoca si no coinciden, sin intentar
salvar el contenido. `emergencyRevoke` puede mover cualquier estado a `REVOKED` directamente,
deliberadamente fuera de las máquinas de estados de MEM-002/MEM-005 — una respuesta de seguridad
real no puede esperar a que un registro llegue a la etapa "correcta" de su propio pipeline.

**Con EVAL-004, las 6 features de F3 están `DONE`.** `aeon_evalops/learning_eval.py` sigue el mismo
patrón que `DR-001..005`/`aeon_evalops.release_gate`: módulo Python puro, sin import de Temporal,
`decide` inyectado, directamente testeable con dobles. Reutiliza deliberadamente el contrato
`(bool, bool)` que ya usa `CaseRunner`/`GRADER_RUNNERS` en `aeon_evalops/runner.py` (en vez de
generalizarlo a un tipo más rico) — `forward_transfer_ok`/`negative_transfer_ok` encajan
exactamente en la misma forma que `(sufficient, citation_ok)` de `deep_research_core`, así que
`EVAL-002` no necesitó ningún cambio de tipo ni se tocó ningún test existente. El dataset real
(`evals/datasets/learning_eval.jsonl`) contiene únicamente candidatos "limpios" (como
`deep_research_core`/`citation_integrity`: casos que deben pasar en producción); los escenarios
negativos (staleness, negative transfer real) se prueban directamente contra la lógica pura en
`test_learning_eval.py`, no a través del dataset de la suite — mismo patrón que
`test_release_gate.py` frente a `EVAL-003`.

Huecos honestos que quedan, ninguno bloqueante para F3: `compute_validation_decision`/
`compute_promotion_decision` son reales y probados, pero nada los conecta todavía al pipeline vivo
de MEM-002 — ningún Activity/workflow llama a `evaluate_learning` y pasa el resultado a
`POST /memory/{id}/validate`/`promote`; hoy son una librería lista para usar, no un lazo cerrado.
El "usefulness/staleness" del criterio depende de un `decayed_utility` que el llamador debe
calcular y pasar (normalmente vía `MemoryStore.DecayedUtility`, Go) — este módulo nunca toca
Postgres directamente. Ambos quedan en `backlog.md`.

MEM-002 añade la máquina de estados real sobre el store de MEM-001
(`memoryValidTransitions`, mismo patrón que `validTransitions` del Agent Registry): CANDIDATE →
QUARANTINED → VALIDATED → ACTIVE, con REVOKED alcanzable desde cualquier estado pre-ACTIVE
(`Reject`). Dos gates, no una sola transición libre: `Validate` requiere una `ValidationDecision`
externa (replay/seguridad/negative-transfer — el mismo hueco que llenará `EVAL-004`), y `Promote`
requiere una `PromotionDecision` externa — la expresión real de
`AgentManifest.spec.memoryPolicy.promotionGate: eval_required`. Ninguna transición muta el estado
si el gate no la permite (verificado explícitamente: un `Validate`/`Promote` bloqueado dejan el
`status` intacto). `WriteCandidate` es deliberadamente la única función que un agente/run puede
llamar para escribir memoria — nunca expone `Quarantine`/`Validate`/`Promote`/`Reject` como algo que
el contenido de un run pueda invocar por sí mismo; ésas son operaciones de curación/pipeline. Fuera
de alcance, igual que en MEM-001: superficie HTTP/SDK (sigue en `backlog.md`, entry point ahora es
`MEM-003` que necesitará escribir candidatos desde Reflection) y el contenido real de
`ValidationDecision`/`PromotionDecision` (hoy tipos aplicados, no calculados — ninguna suite de eval
o replay los produce todavía; eso es `EVAL-004`).

**F3 completa: 6/6 features, todas con test de aceptación real en verde.** `go/internal/store`
tiene el Memory Store + su máquina de estados completa (`MEM-001`/`MEM-002`/`MEM-005`/`SEC-004`),
expuesta sobre HTTP real en `aeon-controlplane`; `python/aeon_memory`/`aeon_evalops` tienen la
lógica de aprendizaje (`MEM-003` Reflection, `EVAL-004` Learning Eval) que produce lo que ese
pipeline necesita para decidir. Lo que falta para que sea un lazo cerrado de punta a punta —
Reflection escribiendo candidatos desde un workflow real, Learning Eval alimentando
`Validate`/`Promote` automáticamente, un job de `Prune` programado — está documentado
explícitamente en `backlog.md`, no oculto.

## F4 — Trust e interoperabilidad (semanas 18-23)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| TOOL-002 | MCP Adapter (core stateless 2026-07-28 + legacy adapter) | `DONE` | `TestAdapterStatelessConformance20260728` y `TestAdapterLegacyFallback20251125` en verde — Aeon como cliente MCP real (`github.com/modelcontextprotocol/go-sdk`, dependencia real, no reinventada) negociando contra un servidor MCP real (mismo SDK): stateless 2026-07-28 por defecto, con fallback real al handshake legacy `initialize` 2025-11-25 cuando el servidor sólo anuncia esa versión vía `server/discover`. Un único adaptador, no dos — el fallback es el `Client.Connect` real del SDK, no una rama de código separada | go/internal/mcp/adapter.go, go/internal/mcp/adapter_test.go |
| INT-003 | Servidor MCP de salida (catálogo de tools gobernado) | `DONE` | `TestToolGatewayServerEndToEndWithRealAdapter` en verde — el catálogo real del Tool Registry (Postgres real, TOOL-001) expuesto como servidor MCP real, con cada `tools/call` pasando por el mismo Cedar `Policy.IsAllowedForPrincipal` que `/execute` ya aplicaba. Verificado también a mano con el paquete **real** `mcp` de Python (independiente del SDK Go usado en el servidor) contra un `aeon-toolgw` real: listó 36 tools reales del registro y llamó `search.web` (permitido) y `shell.exec` (denegado por policy, nunca llega al executor) | go/internal/mcp/server.go, go/internal/mcp/server_test.go, go/internal/policy/cedar.go |
| A2A-001 | A2A Gateway (Agent Card, identity/authz, task exchange) | `DONE` | `TestA2ATaskLifecycle` en verde — un `AgentCard` real construido desde un `AgentManifest` real (FND-001, Postgres real), un cliente A2A real (`github.com/a2aproject/a2a-go`) resolviéndolo, enviando un mensaje y observando la tarea recorrer `submitted`→`working`→`completed`, respaldado por un run Temporal real (RUN-001) — no un estado sintético. Un segundo escenario cancela una tarea en curso y confirma tanto el estado A2A `canceled` como la cancelación real del run subyacente | go/internal/a2a/agentcard.go, go/internal/a2a/executor.go, go/internal/a2a/executor_test.go |
| INT-004 | `FrameworkAdapter` CrewAI | `DONE` | `test_crewai_interop_crew_runs_inside_a_real_activity` en verde — un `crewai.Crew` real (dependencia real, `crewai>=1.15`) corre dentro de una única Activity real, con su Agent llamando al cliente real del Model Gateway (`AeonLLM`, un `crewai.BaseLLM` real) — nunca un SDK de proveedor directamente. Mismo patrón que `INT-001` (LangGraph): `examples/crewai-interop`, verificado end-to-end contra un Temporal efímero real y un worker real separado | python/aeon_adapters/crewai/adapter.py, python/aeon_adapters/crewai/example_crew.py, python/tests/integration/test_crewai_interop_workflow.py |
| INT-005 | `FrameworkAdapter` OpenAI Agents SDK | `DONE` | `test_openai_agents_interop_agent_runs_inside_a_real_activity` en verde — un `agents.Agent`/`agents.Runner` real (dependencia real, `openai-agents`) corre dentro de una única Activity real; su modelo es el propio `OpenAIChatCompletionsModel` del SDK apuntado a `aeon-modelgw`'s `POST /v1/chat/completions` (INT-002) — nunca un SDK de proveedor directamente — y su tool-calling nativo (`FunctionTool` real, no bypaseado como en `INT-004`) dispara una llamada real a `execute_tool` (RUN-004) decidida por el propio Agent, no por la Activity. Verificado end-to-end contra un Temporal efímero real y un worker real separado | python/aeon_adapters/openai_agents/adapter.py, python/aeon_adapters/openai_agents/example_agent.py, python/tests/integration/test_openai_agents_interop_workflow.py |
| INT-006 | `FrameworkAdapter` Microsoft Agent Framework | `DONE` | `test_maf_interop_agent_runs_inside_a_real_activity` en verde — un `agent_framework.Agent` real (dependencias reales, `agent-framework-core`+`agent-framework-openai`, sin el meta-paquete `agent-framework`) corre dentro de una única Activity real; su chat client es el propio `OpenAIChatCompletionClient` del SDK (deliberadamente no el `OpenAIChatClient` por defecto, que habla la API Responses en vez de Chat Completions) apuntado a `aeon-modelgw`'s `POST /v1/chat/completions` (INT-002) — nunca un SDK de proveedor directamente — y su tool-calling nativo (`agent_framework.tool` real) dispara una llamada real a `execute_tool` (RUN-004) decidida por el propio Agent, no por la Activity. Mismo patrón que `INT-005`. Verificado end-to-end contra un Temporal efímero real y un worker real separado | python/aeon_adapters/microsoft_agent_framework/adapter.py, python/aeon_adapters/microsoft_agent_framework/example_agent.py, python/tests/integration/test_maf_interop_workflow.py |
| INT-007 | `FrameworkAdapter` Claude Agent SDK | `TODO` | ídem | — |
| TOOL-003 | Sandbox (shell/code/browser, microVM/gVisor, egress allowlist) | `TODO` | `test_sandbox_egress_denied_by_default` en verde | — |
| SEC-002 | Secret Broker (short-lived credentials) | `TODO` | `test_no_secret_in_prompt` en verde | — |
| FND-002 | ABOM (bill of materials firmado, reproducible) | `TODO` | `aeon publish` genera y firma el ABOM | — |
| — | Circuit breaker + kill switch por agente (A5) | `TODO` | `test_circuit_breaker_quarantines_version` en verde | — |
| OBS-002 | Agent Console (trace explorer, context inspector, evidence graph) | `TODO` | UI muestra un run real de punta a punta | — |
| OBS-003 | FinOps (cost per run/success/agent/model/tool) | `TODO` | dashboard con `cost_model: token_based|compute_based` | — |
| MDL-002 | Quality-aware routing (eval scores como condición de routing) | `TODO` | routing cambia con score degradado en fixture | — |

INT-003 expone el catálogo real de `store.ToolRegistry` (TOOL-001) como servidor MCP real,
montado en `aeon-toolgw` bajo `/mcp` (opcional: sin `AEON_PG_DSN` simplemente no se monta). Cada
`tools/call` reutiliza exactamente la misma comprobación de policy que `ToolGatewayHandlers.execute`
ya aplicaba (`go/internal/api/tool_gateway_handlers.go`) — nunca una ruta paralela sin política.
Decisión de diseño real, no un atajo: un cliente MCP se identifica a sí mismo vía `clientInfo`, pero
la especificación 2026-07-28 dice explícitamente que ese campo es auto-reportado y "SHOULD NOT
[be relied on] for security decisions" — así que, hasta que exista autenticación real de cliente
MCP (`SEC-002`, Secret Broker), todo llamador MCP externo se autoriza como un único principal Cedar
compartido (`McpClient::"external-mcp-client"`, `go/internal/mcp/server.go`), nunca con más
privilegio del que un operador conceda explícitamente a ese principal en el `PolicyBundle`
(`examples/deep-research/policy_bundle.yaml` ganó un permiso nuevo para ese principal, con el mismo
conjunto read-only que el agente de referencia). Esto requirió generalizar
`policy.Engine.IsAllowed` (antes fijado a `Agent::"..."`) en un método nuevo
`IsAllowedForPrincipal(principalType, principalID, toolName)` — `IsAllowed` sigue existiendo sin
cambios, delegando al nuevo método, así que ningún llamador existente ni test de SEC-001 se tocó.

Verificación real cruzada, no sólo mismo-SDK: además del test Go (`TestToolGatewayServerEndToEndWithRealAdapter`,
cliente y servidor ambos sobre `github.com/modelcontextprotocol/go-sdk`), se verificó a mano con el
paquete **oficial `mcp` de Python** (`ClientSession`/`streamable_http_client`, independiente del SDK
Go) contra un `aeon-toolgw` real: registrar un tool real vía `aeon-controlplane`, reiniciar
`aeon-toolgw`, listar 36 tools reales, llamar `search.web` (permitido, resultado real del
executor) y `shell.exec` (denegado, nunca llega a `toolexec.Executor`). Hallazgo real y honesto: el
cliente Python de esta verificación negoció protocolo `2025-11-25`, no `2026-07-28` — no es un bug
del servidor Go: el propio SDK Go documenta que el RPC legacy `initialize` (el que este cliente
Python usa) nunca negocia más allá de `2025-11-25` por diseño de la especificación; sólo
`server/discover` (SEP-2575) alcanza `2026-07-28`. Esto en realidad confirma el objetivo de
TOOL-002/INT-003 desde el otro lado: un cliente que todavía no habla el flujo nuevo interopera
igualmente bien vía el fallback legacy.

A2A-001 sigue el mismo patrón que TOOL-002/INT-003: SDK oficial real
(`github.com/a2aproject/a2a-go` v0.3.15, verificado primero contra la spec real —
`https://a2a-protocol.org/dev/specification/`, la misma `[R17]` que cita la spec original — antes
de escribir nada), nunca una reimplementación de protocolo propia. `go/internal/a2a/agentcard.go`
mapea un `AgentManifest` real del Agent Registry (FND-001) a un `AgentCard` real — los `Skills`
salen literalmente de `spec.tools.allow`, así que lo que un caller A2A ve que el agente puede hacer
nunca puede divergir de lo que su propia policy Cedar ya gobierna. `go/internal/a2a/executor.go`
implementa `a2asrv.AgentExecutor` como un puente real al Run Controller (RUN-001): `Execute` arranca
un run Temporal real vía `runcontroller.Controller.Start`, emite `working` de inmediato y luego
sondea el estado real del run, traduciendo `SUCCEEDED`/`FAILED`/`CANCELLED` a los `TaskState` de A2A
— nunca un estado sintético desconectado de lo que el run realmente hizo. `Cancel` cancela el run
Temporal real (no sólo cambia un estado A2A) y confía en el propio contrato documentado del SDK
(escribir el evento `canceled` cancela el contexto de un `Execute` todavía en curso, evitando una
doble escritura del evento terminal) — verificado de verdad, no asumido: el test de cancelación usa
un grafo con un `loop` de 50 iteraciones para que quede tiempo real de observar `working` antes de
cancelar.

Identidad/authz: mismo hueco reconocido honestamente que en INT-003 — A2A permite declarar
`securitySchemes` en el `AgentCard`, pero Aeon no aplica ninguno todavía (necesita `SEC-002`,
Secret Broker). No inventado aquí; documentado en `backlog.md`.

Hallazgo real durante la verificación (no relacionado con A2A en sí): correr la suite completa de Go
con `AEON_TEST_TEMPORAL_ADDRESS` puesto (necesario para este feature) hizo que `TestAeonReplay`
(DX-002) — que hasta ahora sólo se había *auto-saltado* en cada verificación anterior — corriera de
verdad por primera vez, y falló: `runReplay` para un `run_id` inexistente propagaba el error crudo
de Temporal (`sql: no rows in result set`, una fuga del propio almacén de persistencia de Temporal)
en vez de tratarlo como "no hay eventos de historial". Arreglado detectando
`serviceerror.NotFound` explícitamente (`go/cmd/aeon/dx002.go`) — un bug real de DX-002 que el
"auto-saltarse" había mantenido invisible en cada verificación anterior, encontrado y corregido
aquí, no ignorado, al pasar esta suite por primera vez con Temporal realmente disponible.

INT-004 (`FrameworkAdapter` CrewAI) es el segundo Modo B real, tras `INT-001` (LangGraph), y
comparte su misma estructura (`aeon_adapters/crewai/adapter.py` + `example_crew.py` +
`framework_adapter_activities.py` + `crewai_interop_run.py` + `aeon_sdk/crewai_interop.py` +
`examples/crewai-interop`) pero con una diferencia real que no existía en LangGraph: CrewAI es
síncrono por diseño (`crewai.BaseLLM.call` y `Crew.kickoff` son métodos planos, no corrutinas),
mientras que `call_model_gateway` de Aeon es `async`. Puentear eso exige una decisión de diseño
real, no un parche: `run_crewai_crew` ejecuta `Crew.kickoff` dentro de un hilo de trabajo
(`asyncio.to_thread`), de forma que `AeonLLM.call` puede arrancar su propio event loop nuevo con
`asyncio.run()` con seguridad — seguro precisamente porque ese hilo no tiene ya un event loop
corriendo, a diferencia de la Activity async que lo lanzó. La llamada a herramienta ("research")
deliberadamente NO usa el mecanismo de tool-calling propio de CrewAI (evitar depender del formato
exacto de parseo ReAct de CrewAI, que no es un contrato público estable) — en su lugar, el mismo
patrón "plan luego research" de LangGraph se reproduce llamando a `execute_tool` directamente
después de que el crew termina, no durante su razonamiento interno.

INT-005 (`FrameworkAdapter` OpenAI Agents SDK) es el tercer Modo B real, y trae dos diferencias
técnicas genuinas frente a `INT-001`/`INT-004`, no una simple repetición del patrón. Primera: no
hace falta ninguna subclase de `Model` propia — el SDK oficial ya trae `OpenAIChatCompletionsModel`,
pensado para hablar con cualquier endpoint compatible con OpenAI Chat Completions, así que
`build_aeon_model` (`aeon_adapters/openai_agents/adapter.py`) simplemente apunta el propio
`AsyncOpenAI` del SDK al `POST /v1/chat/completions` real que `INT-002` ya expone en
`aeon-modelgw` — el mismo camino de integración que el ejemplo oficial
`examples/model_providers/custom_example_provider.py` del repo upstream documenta, no un atajo, y
la primera vez que ese endpoint se ejerce contra un framework externo real (antes sólo se había
verificado a mano con el paquete `openai` puro). Segunda: a diferencia de `INT-004`, aquí el
tool-calling nativo del framework SÍ se conecta de verdad — `FunctionTool`/`@function_tool` del SDK
son un contrato tipado y estable (JSON schema de parámetros, payload de argumentos en JSON, callback
async), no el parseo ReAct interno de CrewAI que se evitó por frágil — así que
`build_tool_gateway_tool` envuelve `execute_tool` (RUN-004) como una tool real, y es el propio
`agents.Agent` quien decide cuándo llamarla; el test de aceptación lo prueba end-to-end: el modelo
falso responde primero con `tool_calls`, el SDK ejecuta la tool real, y el resultado
(`status: "written"` del ledger real, no un doble) vuelve al modelo para la respuesta final.
`agents.Runner.run` es además una corrutina nativa (a diferencia de `Crew.kickoff`), así que no hizo
falta ningún puente sync/async. Hueco real encontrado y documentado, no oculto: `crewai>=1.15`
fija `openai<3`, mientras que `openai-agents` saltó a exigir `openai>=3.0.0` en su propia versión
`0.21.0` (19 de agosto de 2026) — un conflicto de dependencias real entre ambos frameworks que obliga
a fijar `openai-agents>=0.20,<0.21` en `pyproject.toml` hasta que se resuelva (ver `backlog.md`).

INT-006 (`FrameworkAdapter` Microsoft Agent Framework) repite el mismo patrón que `INT-005` — misma
razón de fondo, no repetición mecánica: MAF también trae un chat client OpenAI-compatible listo
para usar, así que tampoco hizo falta ninguna implementación propia de su protocolo de chat client,
y su tool-calling nativo (`agent_framework.tool`) también es un contrato tipado y estable, así que
también se conectó de verdad. Una diferencia real que sólo se descubrió inspeccionando el código
fuente instalado, no la documentación (que muestra casi siempre el otro cliente): MAF trae **dos**
clientes con forma de OpenAI — `OpenAIChatClient` (su cliente por defecto, que llama
`client.responses.create`, la API Responses más nueva) y `OpenAIChatCompletionClient` (que llama
`client.chat.completions.create`, la API Chat Completions clásica). `aeon-modelgw`'s
`POST /v1/chat/completions` (INT-002) habla Chat Completions, no Responses — así que el adaptador usa
deliberadamente `OpenAIChatCompletionClient`, no el `OpenAIChatClient` que casi todos los ejemplos
oficiales muestran primero; usar el equivocado habría fallado en silencio contra el endpoint real.
Segunda decisión real, evitando repetir el error de `INT-004`: `pip install agent-framework` (el
meta-paquete) instala ~30 integraciones de proveedor (Anthropic, Bedrock, Gemini, Azure, Redis,
mem0, ...) que Aeon no usa — mismo problema de sobre-instalación que ya dejó `crewai` documentado en
`backlog.md`. Aquí se evitó desde el principio: `pyproject.toml` sólo declara
`agent-framework-core`+`agent-framework-openai`, no el meta-paquete.

F4 arrancó con `TOOL-002`. Antes de implementar nada se verificó contra la especificación real
(`https://blog.modelcontextprotocol.io/posts/2026-07-28/` y
`https://modelcontextprotocol.io/specification/2026-07-28`, citadas por la spec original en
`[R16]`) en vez de asumir el contenido de la nota A3 del plan — todos los detalles técnicos
(headers `Mcp-Method`/`Mcp-Name`, statelessness real vía `_meta`, `resultType`/MRTR, CIMD
reemplazando DCR, los nuevos códigos de error `-32020`/`-32021`/`-32022`) se confirmaron reales, no
inventados. Se encontró que existe un SDK Go oficial y real
(`github.com/modelcontextprotocol/go-sdk`, v1.7.0) que ya implementa exactamente esta versión del
protocolo — incluyendo la negociación real `server/discover` → fallback a `initialize` legacy — así
que `go/internal/mcp/adapter.go` es una capa de traducción fina sobre ese SDK (mismo criterio que
`go.temporal.io/sdk`/`go.opentelemetry.io/otel`/`github.com/jackc/pgx`: usar el SDK oficial real, no
reimplementar el wire format). "Core stateless 2026-07-28 + legacy adapter" es deliberadamente **un
solo** adaptador: el fallback a 2025-11-25 es el propio `Client.Connect` del SDK negociando con un
servidor real que sólo anuncia esa versión — verificado forzando exactamente ese escenario con el
mismo truco de `AddReceivingMiddleware` que usa la propia suite de tests del SDK
(`TestInMemory_E2E_DiscoverFallback_NoOverlap`), no una rama de código propia sin probar. Ambos
extremos de la conformidad (cliente Aeon y servidor de referencia) corren sobre HTTP real
(`httptest.Server`), nunca in-process/fake. Fuera de alcance, documentado en `backlog.md`: conectar
este adaptador al Tool Gateway real (hoy es una librería lista para usar, sin ningún
`ToolDescriptor` que la invoque todavía) — mismo patrón que Reflection (`MEM-003`) o
`aeon_evalops.learning_eval` (`EVAL-004`) antes de su propio wiring.

## F5 — Learning Lab (semanas 24+)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| MEM-004 | Experience Abstraction (cluster/generalize procedural memories) | `TODO` | — | — |
| GOV-001 | Multi-tenancy (tenant isolation + per-scope policy/data residency) | `TODO` | — | — |
| GOV-002 | Release management (canary/rainbow/rollback/version pinning) | `TODO` | — | — |

---

Ver también: [backlog.md](backlog.md) para todo lo diferido con su criterio de entrada, y
[docs/adr/](docs/adr/) para las decisiones de arquitectura referenciadas aquí.
