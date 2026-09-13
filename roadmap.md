# Roadmap — Aeon Agent Harness Platform

> Este fichero es el estado de verdad del proyecto. Una feature sólo pasa a `DONE` cuando su test de
> aceptación nombrado existe y está en verde en CI. `make roadmap-check` (o `aeon roadmap check`
> cuando exista el CLI) valida mecánicamente esta regla — ver [docs/adr](docs/adr/).
>
> Estados: `TODO` · `IN_PROGRESS` · `BLOCKED` · `DONE` · `DEFERRED` (→ movida a [backlog.md](backlog.md))
>
> Última actualización: 2026-09-01 (F0 cerrado salvo la nota de `local-llm`; **las 13 features de
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
> paquete `agent-framework` instalado, sólo `agent-framework-core`+`agent-framework-openai`) e
> `INT-007` (`FrameworkAdapter` Claude Agent SDK, el último de la lista — estructuralmente distinto:
> el modelo no se enruta por Aeon, pero cada tool que el agente puede llamar sí). Con `INT-007` las
> **5 `FrameworkAdapter` (`INT-001`/`INT-004`/`INT-005`/`INT-006`/`INT-007`) están completas**, y
> `TOOL-003` (Sandbox) cierra el hueco de `shell.exec`: ya no es un stub falso, corre de verdad
> dentro de un contenedor Docker real sin stack de red por defecto (motor de aislamiento real, no
> gVisor/Firecracker — ver la nota de F4 abajo). `SEC-002` (Secret Broker) añade emisión real de
> leases de corta vida: un llamador nunca ve un secreto crudo, sólo una referencia opaca que una
> tool call resuelve del lado del servidor — verificado buscando el valor crudo byte a byte en todo
> el round-trip HTTP real. `FND-002` (ABOM) cierra `aeon publish`: cada publicación escribe un bill
> of materials real firmado con Ed25519 junto al manifiesto, verificable y reproducible byte a byte
> con la misma clave. `A5` (circuit breaker + kill switch) cierra el ciclo de seguridad de F4: una
> tasa de fallo real cuarentena una versión `Released` de forma durable, revoca sus leases de
> secreto reales y bloquea nuevos runs contra un Temporal real, antes de que el Run Controller lo
> toque siquiera — probado end-to-end, no simulado. `OBS-002` añade el Agent Console: una página
> HTML real servida por `aeon-runcontroller` que muestra el estado terminal y el trace real de un
> run, combinando exactamente las mismas dos fuentes reales que `aeon trace`/`aeon status` ya leían
> — acotado deliberadamente al trace explorer, sin context inspector ni evidence graph (ninguno de
> los dos tiene todavía datos reales y durables que mostrar). `OBS-003` (FinOps) cierra F4 casi del
> todo: cada `/decide` calcula un coste real en dólares desde uso de tokens real y una tabla de
> precios real config-as-code, lo registra en un ledger Postgres real, y `GET /finops/costs` lo
> agrega en un dashboard real que nunca inventa un `$0.00` para un proveedor `compute_based` sin
> precio por token. **`MDL-002` (quality-aware routing) cierra F4: 14/14, `DONE`.** Un score de eval
> real, bajo, reportado por HTTP real para un candidato concreto (proveedor+modelo) hace que el
> Model Gateway lo salte por completo en el siguiente `/decide` — nunca se llega a intentar —
> verificado tanto en test como a mano contra el binario reconstruido.
>
> **Nueva fase F4.5 — Costura Synaptum**, antepuesta a F5: nace de una negociación de arquitectura
> real con el equipo del framework Synaptum sobre dónde vive el runtime. El acuerdo estructural está
> cerrado (dos costuras: aplicación síncrona y denegable, y `Checkpointer` que persiste sin decidir);
> quedan seis decisiones de contrato, una de ellas bloqueante para ambos equipos. Destapa además un
> hueco propio y real de Aeon: **el Model Gateway no tiene streaming**, así que hoy no hay corte de
> presupuesto en caliente ni cancelación a mitad de generación para ningún cliente.

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
nada para un run de Deep Research real; `OBS-003` (F4, ya `DONE`) da coste real por modelo, pero
coste por-run específicamente sigue sin existir — ningún run_id llega todavía al Model Gateway (ver
`backlog.md`); y
`aeon replay` (`DX-002`) muestra historial real pero no reejecuta ni compara (`--assert-identical`,
en `backlog.md`). Se documentan como trabajo futuro explícito, no como huecos silenciosos.

| Fase | Nombre | % DONE | Estado |
|---|---|---|---|
| F0 | Foundation durable | ~94% (16/17) | `IN_PROGRESS` |
| F1 | Contexto y evidencia | 100% (9/9) | `DONE` |
| F2 | Deep Research + EvalOps (**MVP**) | 100% (13/13) | `DONE`* |
| F3 | Memoria gobernada | 100% (6/6) | `DONE` |
| F4 | Trust e interoperabilidad | 100% (14/14) | `DONE` |
| F4.5 | Costura Synaptum + Axonium (interop de runtime) | 38% (6/16) | `IN_PROGRESS` |
| F5 | Learning Lab | 0% | `TODO` |

Bloqueos abiertos: **uno real, y es externo** — `FND-004` y la suite de conformidad de F4.5 dependen
de una decisión conjunta con el equipo de Synaptum (H1: quién es dueño de la normalización de la capa
de modelo). Las otras cuatro features de F4.5 no dependen de esa decisión y pueden construirse ya. Nota de entorno pendiente (última fila de
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
| INT-007 | `FrameworkAdapter` Claude Agent SDK | `DONE` | `test_claude_agent_interop_agent_runs_inside_a_real_activity` en verde — un `claude_agent_sdk.ClaudeSDKClient` real (dependencia real; envuelve el CLI real `claude`/Claude Code vía npm, no una reimplementación) corre dentro de una única Activity real. A diferencia de `INT-001`/`INT-004`/`INT-005`/`INT-006`, el modelo NO se enruta por el Model Gateway de Aeon — esta SDK no tiene ese punto de extensión, ver la nota de diseño abajo — pero `ClaudeAgentOptions(tools=[])` excluye por completo cada tool nativa de Claude Code (Bash, Read, Write, WebFetch, ...) y el único tool disponible es un MCP tool propio en proceso que llama a `execute_tool` (RUN-004) real, decidido por el propio bucle de razonamiento de Claude. Verificado end-to-end contra un Temporal efímero real y un worker real separado, con el CLI real redirigido vía `ANTHROPIC_BASE_URL` a un servidor Anthropic Messages API falso (sin coste, sin credenciales reales) | python/aeon_adapters/claude_agent_sdk/adapter.py, python/aeon_adapters/claude_agent_sdk/example_agent.py, python/tests/integration/test_claude_agent_interop_workflow.py |
| TOOL-003 | Sandbox (shell/code/browser, microVM/gVisor, egress allowlist) | `DONE` | `TestSandboxEgressDeniedByDefault` en verde — un `shell.exec` real (ya no un stub falso) corre dentro de un contenedor Docker real sin stack de red (`NetworkMode("none")`), rootfs de sólo lectura, todas las capabilities eliminadas y `no-new-privileges`; una resolución DNS falla al instante, no por timeout. Motor de aislamiento real: contenedores Docker vía el cliente oficial de la Engine API (`github.com/moby/moby/client`), no gVisor/Firecracker — ver la nota de diseño abajo. Sólo deniega-por-defecto está implementado; el allowlist de egress configurable queda en `backlog.md` | go/internal/sandbox/sandbox.go, go/internal/sandbox/sandbox_test.go, go/internal/toolexec/executor.go |
| SEC-002 | Secret Broker (short-lived credentials) | `DONE` | `TestNoSecretInPrompt` en verde — un secreto real se emite como un lease de corta vida y opaco por HTTP real (`POST /secrets/issue`), y una tool call real (`secrets.whoami`) lo resuelve del lado del servidor; todo el round-trip completo (exactamente lo que volvería hacia el llamador y de ahí a un contexto de modelo renderizado) se busca byte a byte y el valor crudo del secreto no aparece nunca — verificado también que la resolución fue real (un fingerprint SHA-256 calculado del secreto conocido, no un no-op) | go/internal/secrets/broker.go, go/internal/toolexec/secrets_tool.go, go/internal/api/secret_broker_handlers_test.go |
| FND-002 | ABOM (bill of materials firmado, reproducible) | `DONE` | `TestPublishGeneratesAndSignsReproducibleABOM` en verde — `aeon publish` escribe un ABOM real firmado con Ed25519 (`go/internal/abom`) junto al manifiesto publicado; la firma verifica (`abom.Verify`), y publicar el mismo manifiesto dos veces con la misma clave (`AEON_ABOM_SIGNING_KEY`) produce el mismo fichero ABOM byte a byte — determinismo real de Ed25519 (RFC 8032), no una promesa sin comprobar. Sin clave configurada, sigue firmando con una clave efímera y avisa explícitamente que esa firma no se reproducirá | go/internal/abom/abom.go, go/cmd/aeon/fnd002.go, go/cmd/aeon/fnd002_test.go |
| A5 | Circuit breaker + kill switch por agente | `DONE` | `TestCircuitBreakerQuarantinesVersion` en verde — reportar suficientes fallos reales para una versión `Released` (por HTTP real) hace saltar el breaker, cuarentena la versión de forma durable en el Agent Registry real (Postgres), revoca un lease de secreto real emitido para ese agente, y — el punto de aplicación real — un `POST /runs` posterior que nombra esa versión se rechaza antes de que el Run Controller llegue siquiera a tocar un Temporal real; probado contra un servidor Temporal real, no simulado. `TestQuarantineHandlerIsAKillSwitchRegardlessOfBreakerState` prueba el kill switch manual, sin umbral de por medio | go/internal/circuitbreaker/breaker.go, go/internal/api/circuit_breaker_handlers.go, go/internal/api/circuit_breaker_handlers_test.go |
| OBS-002 | Agent Console (trace explorer) | `DONE` | `TestAgentConsoleShowsARunEndToEnd` en verde — un run real, arrancado por el Run Controller HTTP real contra un Temporal real y un worker real, termina de verdad; su span `invoke_agent` real llega a una Tempo real; la página HTML servida por `GET /console/runs/{run_id}` muestra el estado terminal real (`SUCCEEDED`) y el trace real que Tempo devolvió — un navegador mostrando un run real de punta a punta, no un fixture. Alcance acotado: sólo el trace explorer; el context inspector y el evidence graph no tienen todavía una fuente de datos real y durable que mostrar (ver la nota de diseño abajo) | go/internal/api/console_handlers.go, go/internal/tempoclient/tempoclient.go, go/internal/api/console_handlers_test.go |
| OBS-003 | FinOps (cost per model, ledger durable) | `DONE` | `TestFinOpsDashboardShowsRealCostPerModel` en verde — un `POST /decide` real con uso de tokens real, enrutado contra una tabla de precios real config-as-code (el propio `ModelPolicyBundle`), calcula un coste real en dólares, lo registra de forma durable en Postgres real, y `GET /finops/costs` lo agrega y lo muestra en un dashboard HTML real — cada fila con su propio `cost_model` (`token_based`/`compute_based`), nunca un `$0.00` fabricado para un proveedor sin precio configurado | go/internal/finops/finops.go, go/internal/store/finops_ledger.go, go/internal/api/finops_handlers.go, go/internal/api/finops_handlers_test.go |
| MDL-002 | Quality-aware routing (eval scores como condición de routing) | `DONE` | `TestQualityAwareRoutingChangesWithADegradedScore` en verde — antes de reportar ningún score, un `/decide` real enruta al candidato de mayor prioridad como siempre; tras reportar un score real y bajo para ese mismo candidato por HTTP real (`POST /quality-scores`, Postgres real), el siguiente `/decide` lo salta por completo — nunca se intenta — y cae al candidato de fallback; probado también a mano con `curl` contra el binario reconstruido | go/internal/modelgateway/gateway.go, go/internal/store/quality_scores.go, go/internal/api/quality_score_handlers.go, go/internal/api/quality_score_handlers_test.go |

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

INT-007 (`FrameworkAdapter` Claude Agent SDK) es el último de los cinco `FrameworkAdapter` y el
único con una forma realmente distinta — no una repetición mecánica del mismo patrón. Investigar el
paquete real (`claude-agent-sdk`, no la documentación) reveló el hecho central: es un envoltorio
delgado sobre el CLI real `claude` (Claude Code), lanzado como subproceso vía npm — no hay ningún
punto de extensión tipo "cliente de modelo personalizado" como en `INT-005`/`INT-006`. El CLI llama
directamente a la API Messages de Anthropic (o a lo que `ANTHROPIC_BASE_URL`/Bedrock/Vertex tenga
configurado el host) — enrutar esto por Aeon exigiría un endpoint nuevo en `aeon-modelgw` compatible
con la API Messages de Anthropic (un `INT-002` equivalente para ese wire format, que hoy sólo habla
OpenAI Chat Completions) — un requisito previo real, no trivial, que esta feature no intenta resolver
y deja documentado en `backlog.md` en vez de fingir que lo cumple.

La compensación real, deliberada: en vez de enrutar el modelo, esta integración enruta el *tool* —
el trade-off inverso al de `INT-004` (CrewAI enrutó el modelo, evitó el tool-calling nativo).
`ClaudeAgentOptions(tools=[])` excluye por completo cada tool nativa de Claude Code (Bash, Read,
Write, Edit, Glob, Grep, WebFetch, WebSearch, ...) — no sólo sin aprobación, directamente no se le
ofrecen al modelo — y el único tool disponible es un MCP tool propio en proceso
(`claude_agent_sdk.tool` + `create_sdk_mcp_server`) que llama a `execute_tool` (RUN-004) real.
Decida lo que decida Claude, el único efecto secundario posible es una llamada real y gobernada a un
tool de Aeon.

Dependencia de infraestructura real y más pesada que las otras tres (paquetes Python puros): esta
necesita el CLI real `@anthropic-ai/claude-code` (npm) en el `PATH` — el paquete Python es un
envoltorio, no una reimplementación. `deploy/compose/Dockerfile.python` y el target `test-python`
del `Makefile` instalan Node.js + ese CLI por esta razón exacta. Verificado a mano antes de escribir
código: el CLI real, redirigido vía `ANTHROPIC_BASE_URL` a un servidor local falso que habla el wire
format real de la API Messages (no `/decide`, no Chat Completions — un formato tercero), completa un
turno con tool-calling nativo end-to-end sin tocar la red real ni credenciales reales — la misma
técnica de "falsear la frontera de red externa" que usa cada test de este proyecto, aplicada a un
wire format distinto. El test de aceptación corrió limpio dentro de un contenedor efímero sin
credenciales de Anthropic reales montadas — nunca contra el propio entorno de este agente.

**Con `INT-007`, los cinco `FrameworkAdapter` (`INT-001` LangGraph, `INT-004` CrewAI, `INT-005`
OpenAI Agents SDK, `INT-006` Microsoft Agent Framework, `INT-007` Claude Agent SDK) están
completos** — cada uno con al menos un ejemplo real corriendo dentro de una Activity Temporal real,
verificado end-to-end.

TOOL-003 (Sandbox) reemplaza el `shell.exec` falso de `go/internal/toolexec/executor.go` — que hasta
ahora sólo devolvía `{"status": "executed", ...}` sin ejecutar nada real — por una ejecución real,
aislada. Decisión de diseño honesta, no un atajo: la redacción original de la arquitectura pedía
"microVM/gVisor", pero ni gVisor (`runsc`, que necesita ptrace/KVM sobre un host Linux) ni Firecracker
(que necesita KVM directamente) están disponibles a través del backend virtualizado de Docker Desktop
en el Mac de desarrollo de este proyecto — exigir uno habría hecho esta feature imposible de probar
aquí. En su lugar, `go/internal/sandbox/sandbox.go` usa aislamiento real de contenedores Docker vía
el cliente oficial de la Engine API (`github.com/moby/moby/client` — el mismo criterio de "usar el
SDK oficial real" que ya se aplicó a MCP/A2A): namespaces, cgroups, todas las capabilities eliminadas
(`CapDrop: ["ALL"]`), `no-new-privileges`, rootfs de sólo lectura con un tmpfs pequeño en `/tmp`, y
sobre todo `NetworkMode("none")` — el contenedor no tiene ningún stack de red, no una red restringida;
una resolución DNS falla al instante ("bad address"), no por timeout. Verificado escribiendo el
código contra la API real, no asumiéndola: dos bugs reales aparecieron y se corrigieron durante la
propia implementación, no después. Primero, `ContainerWait` con la condición por defecto
(`WaitConditionNotRunning`) dispara de inmediato sobre un contenedor recién creado pero aún no
iniciado — que ya "no está corriendo" — devolviendo un `StatusCode` sin sentido (0) en vez de esperar
a que el comando real termine; cada código de salida volvía 0 sin importar el comando hasta cambiar a
`WaitConditionNextExit`, la condición que la propia documentación del cliente recomienda para
sincronizar antes de `ContainerStart`. Segundo, leer los logs con una sola llamada justo después de
la señal de `wait` puede competir con el propio volcado de logs del daemon y truncar la salida
silenciosamente; se corrigió leyendo con `Follow: true` hasta EOF, que es en sí mismo el punto de
sincronización real (el stream sólo se cierra cuando el contenedor terminó de verdad). Sólo
deniega-por-defecto está implementado — un allowlist de egress configurable (mencionado también en la
redacción original) queda documentado como hueco real en `backlog.md`, no oculto. `shell.exec` sigue
prohibido por política en el despliegue de referencia (`policy_bundle.yaml` sigue negando
`shell.*` para todo agente) — TOOL-003 construye el motor de ejecución real que un futuro perfil de
acción usaría, no cambia qué agentes pueden invocarlo hoy.

SEC-002 (Secret Broker) reemplaza, para tools nuevas que lo necesiten, el modelo de "secreto
estático de entorno" que el resto de la plataforma sigue usando (las API keys de proveedor de
`aeon-modelgw`, por ejemplo, siguen leyéndose directamente de variables de entorno — eso no cambia
aquí). `go/internal/secrets.Broker` emite referencias de lease opacas y de corta vida
(`POST /secrets/issue`) — nunca el valor crudo — y sólo la propia ejecución de una tool del lado del
servidor puede resolver una (`Broker.Resolve`, usado por `secrets.whoami` en
`go/internal/toolexec/secrets_tool.go`). El test de aceptación no se conforma con probar que
`Resolve` funciona en aislamiento: hace el round-trip completo por HTTP real (emitir el lease,
llamar a la tool con la referencia, capturar la respuesta cruda de `/execute`) y busca el valor
crudo del secreto byte a byte en esa respuesta — exactamente el contenido que en un caller
descuidado terminaría en un contexto de modelo renderizado. Que la tool devuelva además un
fingerprint SHA-256 calculado del secreto real (no un valor fijo) prueba que la resolución
efectivamente ocurrió, no que el test pasa por no hacer nada. Identidad de workload real
(SPIFFE/SVID, mencionada junto a este feature en la redacción original de la arquitectura) no está
implementada — necesitaría un servidor SPIRE real y infraestructura de attestation, un requisito
previo propio que queda en `backlog.md`, igual que el hecho de que montar el socket de Docker de
`toolgw` (`TOOL-003`) le da acceso equivalente a root sobre el host sin que este Broker lo cubra.

FND-002 (ABOM) cierra `aeon publish` (DX-002), que hasta ahora sólo registraba el manifiesto en el
control plane y lo promovía Draft → Candidate. `go/internal/abom.Document` extrae del propio
manifiesto ya parseado (identidad del agente, `spec.modelPolicy`, `spec.tools.allow/deny`,
`spec.evalGates`) más el hash SHA-256 de los bytes crudos realmente publicados — nunca de una
re-serialización, así que verifica contra el fichero literal, no contra una reconstrucción que
podría divergir. "Reproducible" es una propiedad real y comprobada, no una etiqueta: `Document` no
lleva ningún campo no determinista (sin timestamp), y las firmas Ed25519 son deterministas por
diseño (RFC 8032: misma clave + mismo mensaje siempre produce la misma firma, a diferencia de ECDSA)
— el test de aceptación publica el mismo manifiesto dos veces con la misma clave
(`AEON_ABOM_SIGNING_KEY`) y exige que el fichero `.abom.json` completo salga byte a byte idéntico,
no sólo que ambas firmas verifiquen por separado. Sin esa variable configurada, `aeon publish` sigue
firmando con una clave Ed25519 generada al vuelo — el ABOM resultante verifica igual de bien, pero el
CLI imprime una advertencia explícita de que esa firma en concreto no se reproducirá en una
publicación futura. Alcance real y acotado, documentado en `backlog.md`: las entradas `tools`/`deny`
del ABOM son los nombres que el propio manifiesto declara, no `ToolDescriptor`s completos resueltos
contra el Tool Registry — `aeon`, el CLI, no tiene todavía un cliente del Tool Registry — así que la
firma real por-tool/por-skill que menciona la arquitectura (ASI04) queda pendiente de esa resolución
previa.

A5 (circuit breaker + kill switch) añade `go/internal/circuitbreaker.Breaker` — lógica pura, sin
dependencia de Postgres/HTTP, deliberadamente separada de `store.AgentRegistry.Quarantine`
(aplicación durable) la misma separación entre Decision y quien la aplica que ADR-001 ya establece
en todos lados. `Quarantine`/`Unquarantine` son deliberadamente **independientes** del mapa
`validTransitions` forward-only que gobierna Draft→Candidate→Released→Retired: una versión
cuarentenada sigue siendo `Released` en todo momento — cuarentena es un flag ortogonal, no un
movimiento de lifecycle, la misma solución de diseño que MEM-005 ya usó (un segundo mapa de
transiciones separado del pipeline pre-active) para el mismo problema estructural. El breaker
mantiene una ventana móvil por versión de agente y hace saltar la cuarentena automáticamente cuando
la tasa de fallo la supera; el kill switch manual (`POST /agents/{name}/{version}/quarantine`) es
literalmente la misma llamada a `Quarantine`, sin pasar por ningún umbral. SEC-002 (Secret Broker)
se extiende con `IssueForOwner`/`RevokeAllForOwner` para que cuarentenar una versión revoque también,
de verdad, cualquier lease de secreto que esa versión tuviera emitido — la "revocación de
credenciales en caliente" que el backlog prometía. El punto de aplicación real es
`go/internal/api`'s `checkNotQuarantined`, llamado desde `RunControllerHandlers.start` antes de que
`runcontroller.Controller.Start` toque Temporal — el test de aceptación lo prueba contra un servidor
Temporal real: un run arranca con normalidad antes de la cuarentena, y es rechazado con 403 después,
sin que se cree ningún workflow. Hueco real y documentado, no oculto: `aeon-controlplane` (donde vive
el Registry) y `aeon-toolgw` (donde vive el Secret Broker en producción) son procesos distintos —
`RevokeAllForOwner` funciona de verdad en el mismo proceso (como prueba el test), pero nada todavía
hace la llamada HTTP cruzada de controlplane a toolgw en el despliegue real de compose; queda en
`backlog.md`. Tampoco existe todavía nada que llame a `POST /outcomes` automáticamente cuando un run
real termina — el mismo hueco de wiring que MEM-003/EVAL-004 dejaron documentado antes de que
Reflection/Learning Eval tuvieran su propio enganche a un workflow real.

OBS-002 (Agent Console) es deliberadamente sólo un tercio de su propia descripción original ("trace
explorer, context inspector, evidence graph"). Antes de escribir HTML se comprobó qué de esos tres
tiene de verdad datos durables y consultables después de que un run termine: `aeon_context`'s
`LaneState` vive únicamente dentro del proceso Python de un run mientras se ejecuta, y
`aeon_evidence.EvidenceLedger` es un objeto Python en memoria pura, sin persistencia alguna — ninguno
de los dos existe todavía en ningún sitio que una página pudiera consultar después del hecho.
Construir un panel de "contexto" o "evidencia" sobre datos de muestra habría roto el mismo estándar
de "nada simulado" que cada feature anterior de esta sesión ha respetado, así que ambos quedan fuera,
documentados en `backlog.md`, en vez de rellenados con algo falso. El trace explorer sí tiene una
fuente real: `go/internal/tempoclient` (nuevo, compartido) extrae la consulta TraceQL que `aeon
trace` ya hacía contra una Tempo real, y `ConsoleHandlers` (montado en `aeon-runcontroller`, junto al
propio Run Controller) la combina con `Controller.Status` para servir una página HTML real en
`GET /console/runs/{run_id}`. El test de aceptación no se conforma con golpear el endpoint con un
`run_id` inventado: arranca un run real contra un Temporal real y un worker real, espera a que
termine de verdad (`SUCCEEDED`), espera a que su span `invoke_agent` real llegue a una Tempo real, y
sólo entonces pide la página — comprobando que el HTML devuelto contiene el estado terminal real y
el trace real, no una cadena fija. De paso, `aeon trace` (DX-002) se refactorizó para compartir esta
misma consulta a Tempo en vez de mantener una segunda implementación divergente.

Bug real encontrado y arreglado al añadir el segundo test de este paquete que necesita tracing de
verdad: `TestDistributedTracingSpansReachTempo` (OBS-001) y el nuevo test de OBS-002 corriendo juntos
en el mismo binario de test hacían que uno de los dos perdiera sus spans de forma intermitente.
Causa real: cada test llamaba a `tracing.Init` (que reemplaza el `TracerProvider` global) y luego a
su `Shutdown` propio para forzar un flush inmediato antes de consultar Tempo — pero `Shutdown` es
terminal, así que un tracer de paquete obtenido en otro sitio de este código (p. ej. el de
`modelgateway`, creado una vez al arrancar el paquete) que ya se hubiera resuelto contra el
`TracerProvider` que el primer test acababa de cerrar se quedaba apuntando a un exportador cerrado
durante el resto del proceso. Arreglado con un `TracerProvider` compartido, inicializado una sola vez
por binario de test (`go/internal/api/tracing_test_setup_test.go`) y sólo `ForceFlush`eado (nunca
`Shutdown`) entre tests — verificado corriendo ambos tests juntos, repetidamente, tras el arreglo.

OBS-003 (FinOps) empieza por lo mismo que motivó el alcance recortado de OBS-002: comprobar qué dato
de coste ya es real antes de escribir nada. `BudgetsConsumed.cost_usd` (RUN-003, Python) existe desde
antes en el esquema pero nunca se incrementa — el propio comentario en `graph.py` lo admitía
("model_calls/tokens/cost_usd aren't enforced yet"), y de hecho ese comentario ya estaba desactualizado:
el Model Gateway (`MDL-001`) lleva tiempo `DONE`, sólo que el Graph Runtime genérico (`RUN-002`) nunca
tuvo un nodo `model_call` que lo invocara — únicamente DR-001 llama al Model Gateway, por sus propias
Activities, fuera del Graph Runtime genérico. Dado que el Model Gateway (`go/internal/api/
model_gateway_handlers.go`) es el único punto por el que pasa TODA llamada a modelo, sin importar qué
workflow la origine, ahí es donde OBS-003 calcula el coste real: `go/internal/finops.PricingTable`
(paquete puro, sin dependencia de `modelgateway`) multiplica el uso de tokens real que ya devuelve
cada `NormalizedChatResponse` por una tarifa real, config-as-code — reutilizando el mismo
`ModelPolicyBundle` que ya declaraba `cost_model` por candidato (campo del schema ya existente,
nunca antes leído en Go) y sumándole dos campos nuevos, `cost_per_million_{input,output}_tokens`.
Cuando no hay tarifa configurada, o el `cost_model` del proveedor es `compute_based` (facturado por
segundo de GPU, no por token), `CostUSD` devuelve `ok=false` — el llamador debe tratar eso como
"coste desconocido", nunca como un `$0.00` silencioso, y `FinOpsHandlers`'s dashboard respeta esa
distinción mostrando el `cost_model` de cada fila explícitamente. Un test de aceptación real prueba
todo el camino: una llamada real con uso de tokens conocido produce un `cost_usd` real en la
respuesta de `/decide`, ese coste queda grabado en una tabla Postgres real
(`model_gateway_costs`, migración aditiva igual que MEM-005/A5), y `GET /finops/costs` lo agrega con
SQL real (`SUM`/`COUNT` agrupados por proveedor+modelo) — verificado también a mano, con `curl`
contra el binario reconstruido. Hueco real y documentado, no oculto: `decideRequest` acepta ya
`run_id`/`agent_manifest_ref` opcionales para etiquetar el coste, pero ningún llamador real
(Python) los rellena todavía — el coste hoy se agrega por modelo, no por run/agente, hasta que esa
integración exista (ver `backlog.md`, mismo patrón de hueco que A5 dejó con `POST /outcomes`).

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

MDL-002 (quality-aware routing) cierra F4. `modelgateway.Gateway` gana un campo `Quality
QualityGate` opcional y nil-safe — el mismo patrón que `finops.PricingTable`/`FinOpsLedger` en
`ModelGatewayHandlers` (OBS-003): sin configurar, el comportamiento es idéntico al de antes de que
el campo existiera. `QualityGate` es una interfaz de una sola función
(`IsDegraded(ctx, provider, model) bool`) definida en el propio paquete `modelgateway` (el
consumidor define la interfaz que necesita, no el productor) — `go/internal/store.
QualityScoreStore` la satisface estructuralmente, sin que `modelgateway` importe `store` en ningún
momento. Dentro de `Decide`, un candidato degradado se trata exactamente igual que un proveedor no
registrado: se registra el intento con su motivo y se pasa al siguiente, sin haberlo llamado nunca.
Una ausencia de score (un par proveedor+modelo que ningún suite evaluó todavía) deliberadamente NO
cuenta como degradado — sólo un score real y bajo bloquea, nunca la falta de datos, que es el caso
común para la mayoría de candidatos la mayor parte del tiempo. El test de aceptación prueba el
cambio de comportamiento real, no sólo la lógica en aislado: un `/decide` real enruta al candidato
de mayor prioridad antes de reportar nada, y tras un `POST /quality-scores` real con un score bajo
para ese mismo candidato, el siguiente `/decide` lo salta — verificado además a mano reconstruyendo
`aeon-modelgw` y forzando exactamente ese escenario con `curl`. Hueco real, documentado en
`backlog.md`: ningún suite de eval real (`provider_conformance` u otro) llama todavía a
`POST /quality-scores` automáticamente tras correr — el mecanismo de reporte y el de enrutamiento
están completos y probados; conectar un suite real a él es la misma clase de hueco de wiring que
A5/OBS-003/MEM-003 dejaron documentado antes de sus propias integraciones.

**Con esto, F4 (Trust e interoperabilidad) está completa: 14/14 features `DONE`.** Cada una con su
test de aceptación real en verde, contra infraestructura real (Postgres, Temporal, un worker real,
Docker, un OTel Collector + Tempo reales) — nunca un mock ni un fixture aislado del sistema que dice
probar. Los huecos que quedan (wiring automático entre features ya reales, identidad de workload,
persistencia de contexto/evidencia) están documentados explícitamente en `backlog.md`, no ocultos.

## F4.5 — Costura Synaptum + Axonium (interop de runtime)

Fase nueva, no prevista en el plan original. Surge de una discusión de arquitectura con el equipo de
Synaptum (el framework de agentes que se está reescribiendo en paralelo) sobre dónde vive el runtime:
tres notas cruzadas —«Dónde vive el runtime» (Synaptum), «Frontera del runtime» (Aeon), «Acta de
convergencia» (conjunta)— que cierran el corte **semántica de ejecución (framework) / sustrato de
ejecución (harness)** y definen dos costuras entre ambos: una de **aplicación** (síncrona, denegable,
fuera del proceso del bucle) y una de **durabilidad** (`Checkpointer`: `append`/`load`, persiste pero
no decide).

Se antepone a F5 por dos razones: hay compromisos con otro equipo, y una de las features destapa un
hueco real de Aeon que existiría igual sin Synaptum — el Model Gateway no tiene streaming.

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| INT-008 | Streaming con cancelación en el Model Gateway | `DONE` | `TestModelGatewayStreamsAndCancelsMidStream` en verde — `/v1/chat/completions` con `"stream": true` transmite deltas reales por SSE desde un servidor upstream real, y una cancelación a mitad de stream **corta la generación aguas arriba de verdad**: el upstream de prueba registra que observó la desconexión y se detuvo tras 2 de 50 chunks, en vez de completar y ser descartado localmente. Es la respuesta empírica a `P5` del acuerdo tripartito para el salto que Aeon controla. Más `TestGatewayDecideStream` (7 subtests) sobre las reglas de routing en streaming | go/internal/providers/streaming.go, go/internal/providers/openai_compatible/streaming.go, go/internal/modelgateway/streaming.go, go/internal/api/openai_compatible_streaming.go |
| INT-009 | `Checkpointer` — costura de durabilidad | `DONE` | `TestCheckpointerDeduplicatesByStepIdentity` en verde contra Postgres real **y Temporal real**: un worker Go de prueba ejecuta una Activity que registra el paso y muere después, y Temporal la reintenta de verdad — *«real Temporal ran the Activity 3 times; the journal holds 1 entry»*. Cubre además que la fase es parte de la identidad, que un duplicado con resultado distinto conserva el primero y **reporta la divergencia**, que reordenar las claves del payload no es divergencia (comparación `jsonb` semántica, no de bytes), y que appends concurrentes del mismo run obtienen `seq` contiguo sin huecos. Más `TestRunStateAnswersTheThreeReadings` (sin infraestructura) sobre las tres lecturas de las dos fases, `TestCheckpointSeamOverHTTP` porque una costura que solo existe en Go no es una frontera (el consumidor es un proceso Python), y `TestCheckpointerPassesSharedSeamFixtures`, que ejecuta contra esta implementación el corpus dorado publicado a los otros dos equipos — es lo que convierte «somos la implementación de referencia» de afirmación en hecho comprobado | go/internal/checkpoint/checkpoint.go, go/internal/store/checkpointer.go, go/internal/api/checkpoint_handlers.go, evals/contracts/ |
| INT-010 | Costura de aplicación — forma y medición | `TODO` | `TestEnforcementSeamDeniesWithDisposition` en verde — la costura devuelve un tipo con disposición (`deny_step`/`terminate_run`/`require_approval`), no un booleano; más medición real gateway HTTP remoto vs proxy de egress local, con streaming | — |
| OBS-004 | Nivel de durabilidad como campo consultable del run | `TODO` | `TestRunReportsDurabilityLevel` en verde — el nivel (paso vs Activity) es campo del run y atributo de traza, consultable durante un incidente sin leer documentación | — |
| MDL-008 | Clase de inferencia (`local`/`cloud`) en el `ModelPolicyBundle` | `DONE` | `TestLocalInferenceOutsidePrometheusIsDenied` en verde (6 subtests) — `inference_class` es **obligatorio** y **no declarado se deniega**, no se asume: si fuera default-allow con una regla de deny encima, la regla solo dispararía sobre los candidatos que alguien recordó anotar, y los que nadie anotó son exactamente donde viven los errores. Un candidato `local` solo lo sirve `prometheus_inference` —la puerta— salvo que una **excepción nominal** nombre al proveedor *y al entorno* al que pertenece. La excepción está **acotada al perfil**, no al bundle: una concedida al perfil que la necesita no puede ensancharse en silencio a los demás del mismo fichero. Denegación observable: `403` + `aeon_policy_denied` + un mensaje que nombra la excepción que existe y no cubrió el caso (más útil en un incidente que reportar su ausencia), y el test comprueba que el proveedor **no se llama ni una vez**. **Lo que a propósito NO se fusiona:** `data_sensitivity: restricted` sigue filtrando por nombre de proveedor. Se parecen y no son la misma regla — restricted es una frontera de red y tiene que seguir significando «`prometheus_inference` y nada más», incluso en un entorno donde una excepción permita otro proveedor local. Fusionarlas entregaría datos restringidos a lo que esa excepción nombrase | proto/schemas/model_profile.schema.json, go/internal/modelgateway/bundle.go, go/cmd/aeon/dx002.go |
| INT-011 | Un desenlace denegado es un hecho registrado, no un silencio | `TODO` | `TestDeniedStepIsJournalledAsKnownOutcome` en verde — dos caminos, el mismo bug: (a) la decisión de una aprobación (`RUN-005`, ya ligada al hash de los parámetros y con expiración) se escribe en el journal como `ApprovalStep` en fase `result`, porque la toma una persona cuando el bucle no está corriendo y solo el arnés sabe que ocurrió; (b) una denegación por política del Tool Gateway (Cedar, default-deny) se registra igual. Sin esto, un paso denegado *antes* de ejecutar es indistinguible de uno que se intentó y no se sabe cómo acabó — y un run suspendido esperando a una persona no se puede reanudar. Hallazgo de Synaptum al persistir su journal de verdad; solo aparece cuando la durabilidad es real. **Consecuencia de contrato aceptada:** `Checkpointer.append` no lo llama solo el bucle, también el arnés — registrar un hecho no es decidir | — |
| MDL-011 | Verificación de modalidad del candidato antes de enrutar | `DONE` | `TestEmbeddingModelRoutedAsChatIsRejected` en verde (7 subtests) — un candidato no servible por el endpoint de chat hace **fallar el perfil al resolverlo** (`403`, `aeon_policy_denied`), y el test comprueba que **el proveedor no se llama ni una vez**: el fallo que previene no es una respuesta mala, es una *facturable*. Hallazgo original del equipo Axonium contra Prometheus real: `/v1/chat/completions` con un modelo de *embeddings* devuelve `200` con salida degenerada que se factura, y como la cascada de fallback solo avanza cuando un candidato *falla*, ese `200` se da por bueno. `modality` es **obligatorio** (el silencio se deniega) y un candidato malo **hace fallar el perfil entero** en vez de saltarse. **Corregido el 2026-09-11, y era un defecto real de la primera versión:** el enum era `chat`/`embedding`, inventado por nosotros. El catálogo de Prometheus tiene **cuatro** valores —`text`, `vision`, `embedding`, `image`, los cuatro desplegados hoy— y la correspondencia **no es uno a uno**: `text` y `vision` se sirven **ambos** en el endpoint de chat. Con el enum viejo habríamos **rechazado un modelo de visión válido**, haciendo fallar el perfil entero — la consecuencia elegida a propósito para un candidato malo, aplicada a uno bueno. Ahora se adopta el vocabulario del catálogo en vez de traducirlo (ver `contratos/gateway-prometheus/modalidades.md`), porque el mapeo privado es justo la pieza que se implementa mal sin fallar. Un valor desconocido se **deniega, no se adivina**. **Límite honesto:** verificamos la *declaración*, no la realidad — ver `MDL-013` | proto/schemas/model_profile.schema.json, go/internal/modelgateway/bundle.go, go/internal/api/openai_compatible_handlers.go |
| MDL-010 | Robustez del `TokenSource` frente al reloj | `TODO` | `TestTokenSourceSurvivesLocalClockJump` en verde — la expiración se mide como **tiempo transcurrido** (reloj monótono, que `time.Now().Add` conserva y `Before` usa), no como instante de reloj de pared, así que ni el desfase constante con el auth-service ni un salto de NTP la afectan; más el camino suspensión → 401 → `Invalidate()` → reacuñación, que es el único residuo real (el reloj monótono no avanza durante la suspensión del sistema). **Premisa corregida:** este ítem nació como «anclar al header `Date`» a partir de la cesión de Axonium; al ir al código resultó que anclar al `Date` sería cambiar una medida de tiempo transcurrido por una de reloj de pared — un retroceso. El `Date` queda como cota de sanidad ante un `expires_in` absurdo, no como ancla. Ver el canal de coordinación tripartito | — |
| MDL-013 | Contrastar la modalidad declarada con el catálogo real | `TODO` | `TestDeclaredModalityIsCheckedAgainstTheCatalog` en verde — al arrancar (una vez, no por llamada) se consulta `GET /v1/models` y una declaración que contradiga al catálogo hace fallar el perfil. Cierra el límite asumido de `MDL-011`: hoy verificamos lo que el operador escribió, no lo que el modelo es. Viable según medida de Axonium contra el despliegue real: **1020 bytes, 12 ms en frío, <1 ms después, sin autenticación**; y su `verify_modality` está apagado por defecto por una razón que **no nos aplica** —un SDK no debe hacer peticiones que quien llama no pidió—, mientras que un gateway que resuelve perfiles al arrancar está justo en el caso contrario. **Dos límites que no cierra, y conviene no prometerlos:** estar en el catálogo **no significa ser llamable** (un modelo registrado y no desplegado da `503 model-not-loaded`, invisible en el catálogo), y el catálogo **cambia en caliente**, así que una copia cacheada al arrancar envejece | — |
| MDL-012 | Los tokens servidos desde caché existen en el ledger | `DONE` | `TestAnthropicCacheTokensReachTheLedger` en verde (4 subtests) contra un servidor Anthropic falso real, el adaptador real, el Gateway real y Postgres real — deliberadamente a través del adaptador y no de un proveedor falso, porque **el bug vivía en el mapeo del adaptador**, y un falso que ya devuelve usage normalizado habría probado todo menos lo que estaba roto. Antes, `anthropic.go` mapeaba `usage.input_tokens` a `prompt_tokens` y no parseaba los contadores de caché: cada token servido desde la caché de Anthropic era invisible para `OBS-003`. Ahora `100 + 900 = 1000` de entrada total con `cache_read_tokens: 900`, y el coste sube en consecuencia. **Anthropic es el único adaptador donde el mapeo es aritmética y no una copia** — reporta los contadores disjuntos, así que la convención inclusiva acordada con Synaptum y Axonium obliga a sumar; el comentario del código dice cómo falsificarlo (una petición con prefijo cacheado) por si Anthropic cambiara a inclusivo, en cuyo caso la suma pasaría a contar doble. `cache_write` se reporta pero **no** se suma a la entrada, por el mismo acuerdo. Los contadores viajan como punteros: **nulo no es cero** — «este backend no mide caché» y «la caché estaba fría» son hechos distintos, y colapsarlos mete un dato inventado en el ledger (mismo fallo que Axonium encontró un nivel más abajo). **Lo que NO cierra:** las escrituras de caché se facturan con recargo y el `ModelPolicyBundle` no tiene tarifas de caché, así que esos tokens quedan visibles pero sin precio. Visible y sin precio es mejor que invisible | go/internal/providers/anthropic/anthropic.go, go/internal/providers/provider.go, go/internal/store/finops_ledger.go |
| OBS-005 | Qué despliegue atendió de verdad, registrado | `TODO` | `TestServedModelIsRecordedWhenItDiffers` en verde — el ledger imputa por el modelo del `ModelPolicyBundle` (correcto: paga el perfil, no el despliegue), pero la respuesta normalizada devuelve `response.model`, que **puede ser otro**. Hallazgo de Axonium contra Prometheus real, reproducible: pedir `qwen3-0-6b-...-local-2` devuelve `...-local-1` dos veces seguidas. Hoy conviven **dos respuestas distintas a «qué modelo fue»** en la misma llamada y nada las reconcilia; peor, no queda registro de qué despliegue sirvió, que es justo la pregunta de un incidente. Registrar ambos y hacer la discrepancia visible en vez de deducible. **No cambia la imputación.** Si además resultara que el despliegue servido **varía entre llamadas** del mismo perfil —lo que Axonium explícitamente no supone— entonces `MDL-013` también envejece y el problema pasa de ser de registro a ser de enrutado | — |
| MDL-014 | Un contador sin medir no es un cero | `TODO` | `TestUnreportedUsageIsNotRecordedAsZero` en verde — una respuesta sin objeto `usage` se normaliza hoy como `prompt_tokens: 0`, y el ledger registra una llamada de 0 tokens y $0 de coste. Es el mismo hecho fabricado que `MDL-012` corrigió para los contadores de caché, en los dos contadores base. **Encontrado ejecutando el corpus de normalización de Synaptum**, no leyendo el código: es la única divergencia real que queda de los 10 casos (`TestNormalizationCorpusAgainstThisAdapter`, donde figura como divergencia **conocida y nombrada** para que el corpus siga fallando ante una nueva). El arreglo obliga a que `input`/`output` sean nullables de punta a punta —adaptador, `NormalizedChatResponse`, `recordCost`— y a que el ledger **omita** en vez de registrar cero, así que va con la forma de cinco contadores de `FND-004` y no suelto | — |
| MDL-009 | Sustituir `prometheus_inference` nativo por Axonium-Go | `TODO` | **Desbloqueado el 2026-09-13**: `AXO-27` publicado como `go get github.com/Root1V/axonium-sdk/go@v0.1.0`, verificado por ellos instalando esa copia descargada contra el despliegue real. Cumplen la condición de aceptación que pusimos, **medida y no afirmada** — `upstream stopped after 3 chunks; the client read 3`, y funciona por las dos puertas (`ctx` cancelado y `stream.Close()`). Cero dependencias de terceros, incluido el trazado: definen una interfaz `Tracer` de dos métodos en vez de importar OTel, así que no entra en el árbol de nadie. 24/24 casos del corpus compartido reproduciendo los mismos bytes que el SDK de Python. **Criterio de DONE:** `TestPrometheusInferenceViaAxoniumGo` en verde, con el adaptador nativo retirado sólo cuando el reemplazo pase el corpus **y** conserve la cancelación medida de `INT-008`. Es `0.1.0` a propósito y la superficie puede moverse antes del `1.0.0` | — |
| FND-004 | Vocabulario de normalización como contrato compartido | `BLOCKED` | Bloqueado por la decisión conjunta H1 (opción D: especificación compartida versionada, implementada en Go por el gateway y en Python por Synaptum, con suite de conformidad como garantía de equivalencia) | — |
| — | Extracción del repo de contratos (`proto/` fuera de Aeon) | `DEFERRED` | **Decidido no hacerlo por ahora.** Los contratos compartidos viven en la carpeta de coordinación de los tres equipos, junto al `cordinacion.md` donde se anuncian los cambios; cada proyecto vendoriza su copia (la de Aeon está en `evals/contracts/`, con la advertencia de duplicación escrita en su README). El coste real y asumido: en la carpeta no hay CODEOWNERS ni check de CI, así que la regla de «sin implementación» y el proceso de cambio son convención, no control — y fui yo quien argumentó que las convenciones se erosionan. El disparador para revisarlo es que empiece a erosionarse, no antes | evals/contracts/README.md |
| INT-012 | Corpus de normalización compartido ejecutado contra el gateway | `DONE` | `TestNormalizationCorpusAgainstThisAdapter` en verde — corre el corpus publicado por Synaptum (`contratos/normalizacion/`, con los cuerpos de Axonium) contra el adaptador `openai_compatible` real detrás de un servidor HTTP real. **Contra el corpus `0.2-draft` y los cuerpos del manifest v8: 5/14 pasan, 9 con hueco, 2 divergencias conocidas.** La distinción es el entregable: un **hueco** es un concepto que este adaptador no tiene (partes `thinking`, `tool_call`, `estimated`) y es alcance de `FND-004`; una **divergencia** es un desacuerdo sobre algo que sí implementa. De las tres divergencias originales, dos se arreglaron aquí —motivo de fin ausente que no se mapeaba a `stop`, y `[DONE]` que retornaba sin declarar desenlace— y la tercera es `MDL-014`. El test **falla ante cualquier divergencia no registrada**; las conocidas se listan con nombre en vez de silenciarse. **Guarda de versión:** el runner rechaza correr si los cuerpos vendorizados no son el `body_manifest_version` que el corpus declara — sin ella, la primera ejecución emparejó expectativas 0.1 con cuerpos v2 y nadie lo detectó, tampoco el runner. Los casos `authored: true` se leen de `authored_root` y no son grabaciones: existen porque tras el v8 toda respuesta de Prometheus trae `usage`, así que «nadie lo midió» se quedó sin grabación real | go/internal/providers/openai_compatible/normalization_corpus_test.go, evals/contracts/ |
| — | Suite de conformidad de la costura (artefacto conjunto) | `BLOCKED` | Bloqueado por el congelado del contrato en v0.1 — hasta entonces no hay contra qué escribirla. **Anticipo entregado, y no cuenta como la suite:** el corpus dorado de la costura de durabilidad (8 casos) está publicado en la carpeta compartida de los tres equipos y Aeon lo ejecuta en CI (`TestCheckpointerPassesSharedSeamFixtures`). Cubre una sola costura de las dos y una sola implementación de las tres, que es exactamente lo que una suite de conformidad *no* es | evals/contracts/ |

Estado de la negociación al abrir esta fase: **acuerdo cerrado en lo estructural** (corte
semántica/sustrato, dos costuras, dos niveles de durabilidad conviviendo, observabilidad por
operación, presupuestos partidos entre corrección y política). Quedan seis decisiones sobre la forma
del contrato, de las cuales **H1 es la única que bloquea a ambos lados**: Synaptum propuso que Aeon
importara sus adaptadores de proveedor como librería, lo cual no es implementable cruzando la
frontera Go/Python —los cinco adaptadores viven en `go/internal/providers/`, y el lado Python está
diseñado explícitamente para no tener credenciales— además de invertir el principio de que Aeon no
puede depender de un framework concreto. La contrapropuesta de Aeon (opción D) es que la
normalización se **especifique una vez y se implemente dos**, acotada por la suite de conformidad.

Consecuencia sobre F0-F4: **casi ninguna, con una excepción.** Nada del diseño se invalida — el acta
confirma el lado harness — pero se suma un tercer equipo, **Axonium**, que construye el SDK de acceso
a la plataforma de inferencia Prometheus, con una regla fijada por el dueño del proyecto: *toda la
inferencia local se resuelve en Prometheus, y la única puerta es el SDK Axonium.* Eso convierte
`MDL-006` (adaptador `prometheus_inference` nativo en Go) en **superseded-pending**: sigue `DONE` y
en producción porque es código real, probado y verificado en vivo, y se retira el día que
Axonium-Go (**`AXO-27`**, aún no construido) exista y pase el corpus de fixtures. No antes — sustituir
392 líneas de producción funcionando por una dependencia que todavía no se puede instalar sería
cambiar riesgo conocido por riesgo desconocido.

De la misma regla sale `MDL-008`: la política de Prometheus tiene que poder **denegar**, no solo
enrutar. El mecanismo de fallo en cerrado ya existe (`data_sensitivity: restricted` filtra y devuelve
`ErrNoRestrictedCandidate` en vez de degradar a cloud) y la detección también (el ledger de FinOps
registra proveedor y modelo por llamada); lo que falta es que el `ModelPolicyBundle` sepa expresar
*«este modelo es local»*. `MDL-007` (`openai_compatible`) **no se borra**: el adaptador es capacidad,
que se use o no es política — decidido conjuntamente con Axonium.

Consecuencia sobre Modo A: se congela. Si Synaptum es la capa de autoría, el DSL de grafos nativo de
Aeon (`RUN-002`) deja de crecer y se queda como el caso mínimo sin framework — compromiso adquirido
por escrito con el otro equipo, no una decisión reversible unilateralmente.

`INT-008` (streaming) cierra el primer elemento de la fase y responde `P5` para el salto que Aeon
controla. Tres decisiones de diseño que merecen quedar escritas, porque ninguna era obvia:

- **`StreamingProvider` es una interfaz opcional**, no un método más en `Provider`. Un adaptador que
  no la implementa se sirve igual —una sola llamada entregada como un único chunk— así que el
  endpoint funciona contra los cinco proveedores desde el primer día, al coste honesto de que esa
  llamada no se puede cancelar a mitad de generación. `StreamResult.Streamed` lo reporta para que un
  aplicador de presupuesto sepa cuál de las dos garantías tiene, en vez de deducirlo.
- **La forma de la costura es `yield func(Chunk) error`**, no un canal ni un iterador. Devolver error
  desde `yield` significa «para ahora», y es lo que desenrolla toda la cadena hasta cerrar la
  conexión upstream. Un canal habría requerido una goroutine por llamada y una disciplina de cierre
  que se rompe justo en el camino de error.
- **El fallback se detiene en cuanto se entrega el primer chunk.** Reintentar otro candidato después
  empalmaría la salida de dos modelos distintos en una sola respuesta. Un fallo posterior se propaga
  tal cual; verificado con un test dedicado, porque es exactamente el tipo de regresión que un
  refactor introduce sin darse cuenta.

Lo que `INT-008` **no** resuelve, y queda anotado: el `Usage` que viaja en el stream conserva la
forma actual de dos contadores (`prompt_tokens`/`completion_tokens`), no los cinco campos que fija
`H3` del acuerdo tripartito. Cambiar esa forma toca lo que el ledger de FinOps (`OBS-003`) lee en
cada llamada, así que pertenece al vocabulario compartido (`FND-004`), no a esta feature.

`INT-009` (`Checkpointer`) cierra el segundo elemento y es el que Synaptum necesita para integrar.
Cuatro decisiones que no eran obvias:

- **La clave de idempotencia la aplica Postgres, no el código.** `PRIMARY KEY (run_id, step_id,
  phase)` *es* el contrato. La comprobación de duplicado que hace `Append` existe para poder
  *informar* del duplicado, no para evitarlo: un llamante que se saltara esa comprobación seguiría
  sin poder escribir dos veces.
- **Un duplicado con resultado distinto conserva el primero y reporta `PayloadDiverged`.** Gana el
  primero porque es el que otros lectores pueden haber usado ya; pero dos intentos del mismo paso
  produciendo resultados distintos es no-determinismo real, y un diario que lo traga en silencio es
  peor que no tener diario. La comparación es semántica sobre `jsonb`, no de bytes: al otro lado de
  la costura hay Python, y el orden de las claves de un dict entre dos intentos no es algo que
  podamos dar por supuesto — compararlo por bytes convertiría cada reintento en una falsa alarma.
- **`seq` es contiguo por run**, asignado bajo un advisory lock por run. No es cosmética: es lo que
  hace que `NextSeq` signifique «por dónde continúa el diario» y no «algún número mayor que el
  último». Con nodos `parallel` hay appends concurrentes de verdad, y sin el lock los dos leerían el
  mismo `MAX(seq)`. Serializar por run convierte un bucle de reintentos en una espera.
- **`Attempted` es excluyente de `Completed`.** Un paso terminado no está «intentado», está hecho, y
  quien pregunta «¿qué hago ahora?» necesita una respuesta, no dos.

La costura se expone por HTTP en `aeon-controlplane` (`POST`/`GET /runs/{run_id}/checkpoints`) con
exactamente las dos operaciones acordadas y ninguna más. Se dejó fuera a propósito un endpoint que
responda «¿debo reejecutar este paso?»: esa decisión es del bucle, y ponerla aquí movería una
decisión al lado de la costura que se comprometió a no tomar ninguna.

**La pregunta que esto dejó abierta ya está resuelta, y la respuesta corrigió a los dos lados.** Se
planteó a Synaptum en el canal de coordinación: distinguir `attempted` de `completed` obliga a dos
escrituras durables por paso, el doble de latencia en el camino caliente, para una distinción que
solo cambia la decisión cuando el efecto no es idempotente. La implementación soporta las tres
lecturas —dos fases siempre, solo `completed`, o dos fases solo para pasos no idempotentes— y
`TestRunStateAnswersTheThreeReadings` prueba las tres, así que la elección se pudo tomar con el
código delante.

Synaptum eligió la tercera, y el campo ya existía (`ToolDefinition.idempotent` / `ToolStep.idempotent`,
aditivos y con `False` por defecto: quien calla paga durabilidad). Pero además corrigieron su propia
implementación con un argumento mejor que el mío: **el coste no es el número de `append`, es cuántos
bloquean antes del efecto.** Una llamada al modelo no tiene efecto externo más allá de su coste, así
que saber que *se intentó* no cambia ninguna decisión — si el registro se pierde, se vuelve a
inferir, caro pero correcto. Resultado: un agente con todas sus tools idempotentes hace **cero
escrituras bloqueantes antes de un efecto**, y uno con una tool no idempotente hace exactamente una.

**Consecuencia para Aeon, anotada y no cumplida todavía:** hoy `Append` es síncrono y durable
siempre. Por la regla acordada («`durability` es un suelo, no un techo») eso es correcto, y para un
régimen de auditoría bancario probablemente sea lo que se quiere de todas formas. Pero no entrega la
propiedad de latencia que la tabla de Synaptum diseña: si todas las escrituras bloquean, `DEFERRABLE`
no significa nada en el camino real. El append diferido es una feature aparte y tiene su propia
pregunta abierta en el canal —qué garantía se espera de un append diferido que no llegó a disco
cuando el proceso cae— cuya respuesta decide si basta con agrupar escrituras o hace falta un journal
de escritura por delante.

## F5 — Learning Lab (semanas 24+)

| ID | Feature | Estado | Criterio de DONE | PR |
|---|---|---|---|---|
| MEM-004 | Experience Abstraction (cluster/generalize procedural memories) | `TODO` | — | — |
| GOV-001 | Multi-tenancy (tenant isolation + per-scope policy/data residency) | `TODO` | — | — |
| GOV-002 | Release management (canary/rainbow/rollback/version pinning) | `TODO` | — | — |

---

Ver también: [backlog.md](backlog.md) para todo lo diferido con su criterio de entrada, y
[docs/adr/](docs/adr/) para las decisiones de arquitectura referenciadas aquí.
