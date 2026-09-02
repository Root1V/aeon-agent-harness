# Backlog — Aeon Agent Harness Platform

> Todo lo que queda deliberadamente fuera del alcance actual del [roadmap](roadmap.md), para no
> perderlo ni dejar que se cuele por deriva de alcance. Mover una entrada de aquí al roadmap es una
> decisión explícita en un PR (edita ambos ficheros en el mismo commit), nunca un añadido silencioso
> a mitad de una fase.

## Cómo usar este fichero

Cada entrada lleva: descripción, fase objetivo sugerida, **criterio de entrada** (qué debe ser
cierto para promoverla) y coste estimado (T-shirt: S/M/L/XL). Si una entrada se descarta
definitivamente, se borra con una nota en el mensaje de commit — no se acumulan entradas muertas.

---

### Ningún perfil de agente usa todavía `shell.exec` real (`TOOL-003`)

- **Descripción:** `TOOL-003` construyó el motor de ejecución real (`go/internal/sandbox`), pero el
  perfil Deep Research sigue siendo read-only y `policy_bundle.yaml` sigue prohibiendo `shell.*` para
  todo agente — nada en el despliegue de referencia invoca `shell.exec` de verdad todavía.
- **Fase objetivo:** cuando un segundo perfil de agente (acción) lo requiera explícitamente.
- **Criterio de entrada:** existe un caso de uso real que necesita ejecutar shell/código arbitrario,
  no sólo herramientas read-only.
- **Coste:** M (perfil + política; el motor ya existe).

### Allowlist de egress configurable para el Sandbox (`TOOL-003`)

- **Descripción:** `go/internal/sandbox` sólo implementa deniega-por-defecto
  (`NetworkMode("none")`, sin stack de red en absoluto) — la redacción original de la arquitectura
  también pedía un *allowlist* de hosts permitidos por llamada, que no se implementó. Un mecanismo
  real (probado en este mismo pase, no elegido a ciegas) sería una red Docker `--internal` (sin ruta
  a internet, una garantía nativa de Docker) compartida sólo entre el contenedor sandboxed y un
  contenedor-proxy con salida real, que aplique el allowlist por hostname antes de reenviar.
- **Fase objetivo:** cuando un tool real necesite alcanzar un host externo específico (no cero, no
  todos).
- **Criterio de entrada:** un `ToolDescriptor` real declara una lista de hosts permitidos.
- **Coste:** M.

### Aislamiento microVM/gVisor real para el Sandbox (`TOOL-003`)

- **Descripción:** `go/internal/sandbox` usa aislamiento de contenedores Docker (namespaces +
  cgroups + capabilities eliminadas), no microVMs — gVisor (`runsc`) necesita ptrace/KVM sobre un
  host Linux y Firecracker necesita KVM directamente, ninguno disponible a través del backend
  virtualizado de Docker Desktop en el Mac de desarrollo de este proyecto. El aislamiento de
  namespaces de Docker es real y con enforcement del kernel, pero no tiene la superficie de ataque
  reducida de una microVM (comparte el kernel del host).
- **Fase objetivo:** antes de ejecutar código no confiable de un tercero real en producción (no sólo
  `shell.exec` gobernado por Cedar con argumentos controlados).
- **Criterio de entrada:** un despliegue real corre sobre un host Linux con KVM/gVisor disponible —
  probarlo primero ahí, no en este Mac.
- **Coste:** L.

### `aeon-toolgw` monta el socket de Docker del host (`TOOL-003`)

- **Descripción:** `deploy/compose/docker-compose.yml` monta `/var/run/docker.sock` dentro de
  `toolgw` para que `go/internal/sandbox` pueda lanzar contenedores hermanos reales — esto le da a
  `toolgw` acceso equivalente a root sobre el host Docker. `SEC-002` (Secret Broker) ya existe, pero
  no cubre este socket — no hay ningún lease/credencial de por medio para acceder a él, es acceso
  directo de proceso. Real, no un descuido: es el tradeoff exacto de "contenedores Docker como motor
  de sandbox" (ver la nota de diseño de `TOOL-003` en `roadmap.md`).
- **Fase objetivo:** antes de exponer `aeon-toolgw` fuera de un entorno de confianza single-tenant.
- **Criterio de entrada:** se adopta un runtime rootless/daemonless (ej. Podman sin socket
  compartido) para el sandbox, o el propio socket se pone detrás de algún control de acceso real.
- **Coste:** M.

### A2A Gateway completo (más allá del Agent Card mínimo)

- **Descripción:** ciclo completo de 8 estados de Task, push notifications, discovery multi-agente.
- **Fase objetivo:** F4 (`A2A-001`).
- **Criterio de entrada:** existe al menos un consumidor A2A externo real que lo necesite.
- **Coste:** L.

### Memoria persistente (Memory Store activo, más allá de candidatos)

- **Descripción:** todo F3 — el MVP no persiste memoria entre runs.
- **Fase objetivo:** F3.
- **Criterio de entrada:** el MVP de Deep Research está en producción y hay demanda de reuso
  cross-run.
- **Coste:** XL.

### Multi-tenancy real (aislamiento fuerte por tenant)

- **Descripción:** hoy `tenant_id` está en los contratos pero el despliegue es mono-tenant. Aislamiento
  de datos/policy/secretos por tenant es F5 (`GOV-001`).
- **Fase objetivo:** F5.
- **Criterio de entrada:** un segundo equipo/organización necesita desplegar sobre la misma
  instancia sin ver datos de otros.
- **Coste:** XL.

### Consola web (Agent Console)

- **Descripción:** `OBS-002`. El MVP se inspecciona con `aeon trace` (CLI). Una UI completa
  (trace explorer, context inspector, evidence graph) es F4.
- **Fase objetivo:** F4.
- **Criterio de entrada:** el CLI resulta insuficiente para onboarding de usuarios no técnicos.
- **Coste:** XL.

### Quality-aware routing (MDL-002)

- **Descripción:** rutear por score de eval, no sólo por profile/coste/disponibilidad.
- **Fase objetivo:** F4.
- **Criterio de entrada:** existen suficientes runs históricos con eval scores por proveedor para
  que el routing tenga señal (no antes de EVAL-002 estable).
- **Coste:** M.

### Abstracción de experiencia (MEM-004) y Learning Lab (F5 completo)

- **Descripción:** clustering/generalización de memorias procedurales, auto-mejora offline.
- **Fase objetivo:** F5.
- **Criterio de entrada:** F3 (memoria gobernada) lleva ≥1 ciclo de release en producción sin
  incidentes de seguridad de memoria.
- **Coste:** XL.

### ABOM firmado (FND-002)

- **Descripción:** bill of materials reproducible y firmado por versión/deployment.
- **Fase objetivo:** F4.
- **Criterio de entrada:** hay más de un entorno de despliegue (staging/prod) y se necesita
  trazabilidad de qué versión de qué corre dónde.
- **Coste:** M.

### Identidad de workload real (SPIFFE/SVID) para el Secret Broker (`SEC-002`)

- **Descripción:** `go/internal/secrets.Broker` (`SEC-002`) emite leases de corta vida, pero no hay
  identidad de workload real detrás de quién puede pedir uno — cualquier llamador con acceso a
  `POST /secrets/issue` puede emitir un lease para cualquier secreto configurado. SPIFFE/SVID
  necesitaría un servidor SPIRE real y infraestructura de attestation de workload, un requisito
  previo propio, no algo que se pueda añadir incrementalmente sobre el Broker actual.
- **Fase objetivo:** cuando el despliegue salga de un entorno de desarrollo/demo hacia algo
  multi-tenant o con datos reales sensibles.
- **Criterio de entrada:** existe un servidor SPIRE real (o equivalente) para atestiguar la
  identidad de los llamadores.
- **Coste:** L.

### Los proveedores del Model Gateway no pasan por el Secret Broker (`SEC-002`)

- **Descripción:** `go/cmd/aeon-modelgw/main.go` sigue leyendo `ANTHROPIC_API_KEY`/`OPENAI_API_KEY`/
  etc. directamente de variables de entorno estáticas al arrancar — `SEC-002` no los retroadapta;
  sólo cubre tools nuevas que decidan usar el Broker (hoy, sólo `secrets.whoami`, una tool de
  demostración).
- **Fase objetivo:** cuando rotar credenciales de proveedor sin reiniciar `aeon-modelgw` sea un
  requisito real.
- **Criterio de entrada:** un incidente o una política de rotación real lo exige.
- **Coste:** M.

### Circuit breaker + kill switch por agente (A5)

- **Descripción:** cuarentena automática de versión por tasa de fallo/coste anómalo, revocación de
  credenciales en caliente.
- **Fase objetivo:** F4.
- **Criterio de entrada:** hay más de un agente `Released` corriendo simultáneamente en el mismo
  entorno (antes de eso el radio de explosión de un fallo es trivial).
- **Coste:** M.

### Adaptadores de proveedor adicionales (Bedrock, Azure AI Foundry nativo, Mistral, Cohere)

- **Descripción:** el Model Gateway del MVP cubre Anthropic/OpenAI/Gemini/Prometheus-local/
  OpenAI-compatible genérico. Proveedores adicionales entran bajo demanda.
- **Fase objetivo:** post-F0, sin fase fija.
- **Criterio de entrada:** un proyecto concreto requiere ese proveedor y no lo cubre el adaptador
  `openai_compatible`.
- **Coste:** S cada uno (la interfaz `Provider` ya existe).

### GOV-002 Release management (canary/rainbow/rollback)

- **Descripción:** estrategias de despliegue progresivo más allá de Released/Retired simple.
- **Fase objetivo:** F5.
- **Criterio de entrada:** hay tráfico de producción real que justifique despliegue progresivo.
- **Coste:** L.

### Servidor local-llm compatible con CPU (reemplazo o complemento de vLLM en `deploy/compose`)

- **Descripción:** el servicio `vllm` del perfil `local-llm` (`deploy/compose/docker-compose.yml`)
  usa la imagen oficial `vllm/vllm-openai`, que requiere GPU/CUDA y falla al arrancar en cualquier
  host sin GPU — incluyendo el Mac Apple Silicon de este proyecto (verificado al levantar
  `--profile full`; ver la nota de la fila `deploy/compose completo` en `roadmap.md` F0). No
  bloquea el desarrollo actual porque el equipo usa Prometheus (externo) como inferencia local
  real, no este servicio de desarrollo/CI.
- **Fase objetivo:** sin fase fija — infraestructura de desarrollo, no una feature de producto.
- **Criterio de entrada:** alguien necesita `PROFILE=local-llm`/`full` funcionando en un host sin
  GPU (p. ej. CI, o desarrollo sin acceso a un endpoint local-inference externo). La solución es
  sustituir o complementar `vllm` por un servidor CPU-compatible que hable el mismo wire format
  OpenAI Chat Completions (p. ej. Ollama con su endpoint `/v1` experimental, o llama.cpp server) —
  o, más simple, excluir `vllm` del criterio de "todos los servicios sanos" de `make dev` y
  documentar que `local-llm` requiere GPU explícitamente.
- **Coste:** S si sólo se documenta/excluye el criterio; M si se sustituye por un servidor real
  CPU-compatible (implica validar que el adaptador `openai_compatible` sigue funcionando contra él).

### Eval Runner: provider matrix, trace graders, e `injection_suite` real (EVAL-002)

- **Descripción:** `aeon_evalops/runner.py` (EVAL-002) es real para "offline" y "repeated trials",
  y `coverage_grader`/`citation_integrity_grader` llaman al pipeline real `DR-001`..`DR-005`. Tres
  piezas de la descripción original de EVAL-002 quedan fuera de este primer Runner: (1) **provider
  matrix** — correr el mismo suite contra cada adaptador real (`MDL-003..007`) en vez de fixtures
  scripted; (2) **trace graders** — calificar en base a spans OTel reales (`OBS-001`) en vez de sólo
  el resultado final; (3) un harness offline real para `injection_suite` (hoy reporta `SKIPPED`
  honestamente, sin inventar un puntaje).
- **Fase objetivo:** F2 (antes del cierre del MVP) o F4 si se decide diferir.
- **Criterio de entrada:** provider matrix necesita el Model Gateway real (`aeon-modelgw`)
  alcanzable en el entorno donde corre el Runner, no sólo fixtures — natural una vez `INT-002`
  (endpoint OpenAI-compatible) o el propio `aeon-modelgw` esté siempre arriba en CI. Trace graders
  necesita un Tempo real alcanzable (igual que `TestDistributedTracingSpansReachTempo`).
  `injection_suite` necesita decidir qué constituye "resistencia a inyección" verificable offline
  (p. ej. un documento con instrucciones embebidas → ningún `tool_call` no solicitado en el
  resultado) antes de escribir su harness.
- **Coste:** M cada uno; los tres son independientes entre sí.

### Motor del Eval Runner como servicio (reemplazar el `exec.Command` de `aeon eval run`)

- **Descripción:** `aeon eval run` (Go) hoy shell-ea directamente a
  `python -m aeon_evalops.cli` (o falla con un mensaje claro si no hay Python en el `PATH`,
  documentado como fallback vía `make eval-run`). Esto es una solución puente, no la arquitectura
  final: rompe la premisa de `aeon/cli` como binario Go estático sin dependencias, y no funciona
  si el CLI corre en una máquina distinta a donde vive `python/`.
- **Fase objetivo:** F2, cuando el resto de EVAL-002 (provider matrix) ya necesite que el motor sea
  un servicio de todos modos.
- **Criterio de entrada:** el mismo patrón que ya resolvió esto para el Model Gateway
  (`DR-001`): exponer `aeon_evalops.runner.run_suite` sobre HTTP (p. ej. un endpoint en un nuevo
  `aeon-evalrunner`, o añadido a `aeon-modelgw`/`aeon-worker`) y hacer que `aeon eval run` llame a
  ese endpoint en vez de invocar un proceso Python local.
- **Coste:** M.

### Replanning automático en `DeepResearchWorkflow` (DX-001)

- **Descripción:** `DeepResearchWorkflow` corre una sola pasada — si la Sufficiency Gate (`DR-003`)
  encuentra subtareas sin cubrir o impugnadas, el run igual produce un reporte con lo que sí se
  permitió y devuelve `sufficient=False` más `topics_to_replan`, pero nunca vuelve a planificar ni
  a investigar esos temas automáticamente. `DR-003` ya calcula exactamente qué haría falta; falta
  actuar sobre ello.
- **Fase objetivo:** F2, antes de cerrar el MVP (el criterio de MVP del roadmap no exige
  replanning automático explícitamente, pero es la brecha más visible entre "corre" y "es
  realmente Deep Research").
- **Criterio de entrada:** decidir el límite de reintentos (¿1 replan? ¿N acotado por presupuesto?)
  y si el replanning re-invoca al Planner con las `topics_to_replan` como pistas, o genera
  subtareas de reemplazo directamente a partir de ellas.
- **Coste:** M.

### `aeon_sdk` genérico: `start_run(manifest)` y superficie tools/context/memory/traces/approvals

- **Descripción:** `DX-001` entregó `aeon_sdk.deep_research.start_deep_research_run` — específico
  de Deep Research, no el `start_run` genérico que la spec original describe ("API pública:
  start_run, tools, contexts, memory, traces, approvals"). No hay todavía una forma de resolver un
  `AgentManifest` arbitrario (grafo + budgets + tools permitidas) a una ejecución de workflow sin
  escribir un workflow Temporal dedicado por perfil, como se hizo aquí para Deep Research.
- **Fase objetivo:** F2 tardío o F4, según cuántos perfiles de agente además de Deep Research
  necesite soportar la plataforma antes de que valga la pena generalizar.
- **Criterio de entrada:** un segundo perfil de agente real (más allá de Deep Research) que
  necesite `aeon_sdk` — generalizar antes de eso es adivinar la abstracción correcta sin un
  segundo caso real que la valide.
- **Coste:** L.

### `aeon replay --assert-identical` (diff/reejecución real, no sólo historial)

- **Descripción:** `aeon replay <run_id>` (DX-002) hoy imprime el historial real de eventos de
  Temporal — prueba que el CLI puede conectarse y consultar de verdad, pero no reejecuta el
  workflow ni compara resultados. El MVP final del roadmap describe `aeon replay <run_id>
  --assert-identical`: reejecutar contra el historial grabado y confirmar que el resultado es
  bit-a-bit idéntico (la prueba real de que ADR-001 se cumple).
- **Fase objetivo:** cierre del MVP de F2.
- **Criterio de entrada:** ninguno especial — es trabajo directo, usar
  `temporalio`'s replay tooling (Go o Python) contra el historial ya obtenible.
- **Coste:** M.

### `aeon publish` con promoción Candidate→Released gateada por eval real

- **Descripción:** `aeon publish` (DX-002) registra y promueve Draft→Candidate contra el control
  plane real, pero deliberadamente NO intenta Candidate→Released — ese paso necesita un
  `ReleaseGateDecision` (EVAL-003) real, comparando el `SuiteReport` del candidato contra un
  baseline, y hoy no hay de dónde sacar ese baseline automáticamente (ni un flag `--release` en el
  CLI, ni una noción de "cuál es el Released actual para comparar").
- **Fase objetivo:** F2 tardío, una vez que `EVAL-002`'s "provider matrix"/motor-como-servicio
  (ver entradas de arriba) esté resuelto — sin eso, calcular el `SuiteReport` del candidato desde
  el CLI Go implica el mismo puente `exec.Command` a Python que `eval run` ya usa.
- **Criterio de entrada:** decidir de dónde sale el "baseline": ¿el último agente `Released` con
  el mismo `name`? ¿un `SuiteReport` guardado explícitamente en el publish anterior?
- **Coste:** M.

### Enforcement de budgets en el endpoint OpenAI-compatible (INT-002)

- **Descripción:** `POST /v1/chat/completions` (INT-002) resuelve routing real contra un
  `ModelPolicyBundle` real, pero no aplica ningún límite de coste/tokens por-agente — no hay hoy
  ningún sitio en el Model Gateway que contabilice presupuesto consumido por llamada. La
  descripción original de INT-002 en la spec incluye "routing, budgets, redaction y FinOps
  cambiando una base_url"; sólo "routing" es real hoy.
- **Fase objetivo:** F4, junto con `OBS-003` (FinOps) y `MDL-002` (quality-aware routing) — los
  tres comparten la necesidad de que el Model Gateway sepa qué agente/run está haciendo la
  llamada, algo que este endpoint no recibe hoy (un cliente OpenAI genérico no manda ese contexto).
- **Criterio de entrada:** decidir cómo un cliente externo identifica el run/agente que hace la
  llamada — ¿un header custom? ¿parte del `model` string (`profile:run_id`)? — antes de poder
  contabilizar nada contra un presupuesto real.
- **Coste:** M.

### Tracing real de punta a punta para runs Python (`DeepResearchWorkflow`, `LangGraphInteropWorkflow`)

- **Descripción:** `OBS-001` instrumentó los servicios Go (`invoke_agent`/`chat`/`execute_tool`)
  pero nunca se extendió al lado Python — `DeepResearchWorkflow` (`DX-001`) y
  `LangGraphInteropWorkflow` (`INT-001`) no emiten ningún span propio. `aeon trace <run_id>`
  (`DX-002`) por tanto no encuentra nada para un run de Deep Research real, sólo para runs que
  pasan por el Run Controller Go. Esto es el hueco más visible entre "las 13 features de F2 están
  DONE" y "el MVP como narrativa compuesta (informe + traza navegable + coste + replay) es 100%
  cierto" — ver la nota al final de la sección F2 en `roadmap.md`.
- **Fase objetivo:** cierre real del MVP, antes o durante F3.
- **Criterio de entrada:** ninguno especial — es la extensión directa de `OBS-001` al lado Python
  (usar el SDK de OTel para Python dentro de las Activities de `aeon_worker`, con los mismos
  atributos `gen_ai.*`).
- **Coste:** M.

### Conectar Reflection (MEM-003) a un workflow real

- **Descripción:** `python/aeon_memory/reflection.py` es un módulo puro, testeado con un `decide`
  falso (`test_reflection_extracts_candidates`), pero nada lo llama todavía desde un run real:
  `DeepResearchWorkflow` (`DX-001`) no invoca `Reflector.reflect` al terminar, y sus candidatos
  (si los hubiera) no se escriben vía `POST /memory/candidates` (la superficie HTTP que este mismo
  PR expuso en `go/internal/api/memory_handlers.go`). Mismo patrón que DR-001..004 antes de
  `DX-001`: la lógica es real y testeada, la integración end-to-end todavía no.
- **Fase objetivo:** F3, una vez que el patrón de Activity Python→HTTP Go que usan
  `model_activities.py`/`tool_activities.py` se replique para memoria (una
  `write_memory_candidate_activity` + una llamada a `Reflector.reflect` al final de
  `DeepResearchWorkflow.run`).
- **Criterio de entrada:** ninguno especial — es la extensión directa de `DX-001`.
- **Coste:** M.

### Conectar `aeon_evalops.learning_eval` al pipeline vivo de MEM-002

- **Descripción:** `EVAL-004` ya calcula `ValidationDecision`/`PromotionDecision` reales
  (`compute_validation_decision`/`compute_promotion_decision`, con forward/negative transfer y
  staleness real, no construidos a mano) — eso cierra la mitad de este hueco. La mitad que queda:
  nada llama a `evaluate_learning` desde un run real ni pasa su resultado a
  `POST /memory/{id}/validate`/`promote`; hoy es una librería lista para usar, sin ningún
  Activity/workflow que la invoque. Tampoco hay de dónde sacar `decayed_utility` automáticamente —
  el llamador debe calcularlo (vía `MemoryStore.DecayedUtility`, Go) y pasarlo.
- **Fase objetivo:** junto con la entrada de arriba sobre conectar Reflection (`MEM-003`) a un
  workflow real — ambos son la misma clase de trabajo (Python↔Go wiring para memoria) y compartirían
  la mismas Activities nuevas.
- **Criterio de entrada:** un consumidor real (`aeon_worker`) que necesite promover una memoria
  automáticamente en vez de a mano.
- **Coste:** M.

### `Prune` (MEM-005) no tiene wiring ni política propia todavía

- **Descripción:** `MemoryStore.Prune`/`RecordUsage`/`Supersede`/`Revoke` son reales y probados
  contra Postgres real, pero nada los llama en producción todavía: ningún cron/job periódico
  ejecuta `Prune`, ningún run llama a `RecordUsage` tras consultar memoria, y `halfLifeDays`/
  `threshold` se pasan como argumentos directos a la llamada — no hay ningún
  `AgentManifest.spec.memoryPolicy` ni config-as-code que los declare por scope/tenant.
- **Fase objetivo:** F4 (Agent Console/FinOps) para el job periódico; el config-as-code de
  decaimiento podría vivir en el mismo `ModelPolicyBundle`-style YAML o uno propio, a decidir junto
  con `EVAL-004` (F3, todavía pendiente) ya que también toca cómo se gobierna una memoria `ACTIVE`.
- **Criterio de entrada:** un consumidor real (`aeon_worker` leyendo memoria vía
  `GET /memory/active` y llamando `RecordUsage`) que haga evidente qué parámetros de decaimiento
  hacen falta.
- **Coste:** M.

### Aislamiento por tenant en el lado de escritura del pipeline (`SEC-004`)

- **Descripción:** `SEC-004` cerró el aislamiento cruzado de tenant en lectura (`GET
  /memory/{id}`) y en `revoke`, pero `quarantine`/`validate`/`promote`/`reject` (MEM-002) siguen
  sin ninguna comprobación de `tenant_id` — hoy son operaciones de operador/pipeline, no
  expuestas a llamadas de un tenant concreto, pero si `aeon_worker` empieza a invocarlas
  directamente (ver la entrada de arriba sobre `Prune`) haría falta la misma comprobación que ya
  tienen `GET`/`revoke`.
- **Fase objetivo:** junto con el primer consumidor real del pipeline vía HTTP (mismo criterio de
  entrada que la entrada de `Prune` de arriba), o F4 si para entonces ya existe un modelo de
  identidad/autorización más general (Secret Broker, SPIFFE) que lo cubra de forma unificada.
- **Criterio de entrada:** un caller real no confiable de estas rutas.
- **Coste:** S.

### Conectar el MCP Adapter cliente (`TOOL-002`) al Tool Gateway real

- **Descripción:** `go/internal/mcp/adapter.go` (cliente — Aeon consumiendo servidores MCP
  externos) es real y probado, pero ningún `ToolDescriptor` lo invoca todavía — el Tool Gateway
  (`go/internal/toolexec`) no tiene un backend "mcp" que abra una `Session` y llame
  `ListTools`/`CallTool` contra un servidor de terceros. Un tool servido por un servidor MCP
  externo no puede registrarse ni ejecutarse en Aeon todavía. Nótese que esto es distinto de
  `INT-003` (ya `DONE`), que resolvió el sentido contrario — Aeon como *servidor* MCP exponiendo su
  propio catálogo — no este.
- **Fase objetivo:** cuando exista un caso de uso real que necesite un tool respaldado por un
  servidor MCP externo concreto (LangGraph/CrewAI/Claude Code ya expone algunos vía MCP).
- **Criterio de entrada:** decidir cómo se declara un tool respaldado por MCP en
  `tool_descriptor.schema.json` (`mcp_origin: {server, spec_version}` ya existe en el schema desde
  F0, pendiente de usarse) y cómo se asigna `side_effect`/`risk` a algo que Aeon no implementó — un
  tool descubierto vía `ListTools` no trae esa clasificación consigo.
- **Coste:** M.

### Catálogo MCP de salida (`INT-003`) es estático por proceso y sin identidad real de cliente

- **Descripción:** `aeon-toolgw` lee el Tool Registry (Postgres) una sola vez al arrancar para
  construir el servidor MCP (`/mcp`) — un tool registrado o modificado en `aeon-controlplane`
  después no aparece hasta reiniciar `aeon-toolgw` (verificado a mano: hizo falta un `docker compose
  restart toolgw` real para ver un tool recién registrado). Además, todo llamador MCP externo se
  autoriza hoy como un único principal Cedar compartido (`McpClient::"external-mcp-client"`) porque
  no existe autenticación real de cliente MCP — ver la nota de diseño en `roadmap.md` INT-003.
- **Fase objetivo:** el refresco en vivo del catálogo encaja con `OBS-002`/Agent Console (F4, ya
  que ambos necesitan una vista actualizada del registro); la identidad real de cliente MCP
  necesita identidad de workload real primero — `SEC-002` (Secret Broker, ya `DONE`) no la cubre,
  es un broker de credenciales de *tools*, no de identidad de *caller*; ver la entrada de
  SPIFFE/SVID en este mismo fichero.
- **Criterio de entrada:** para el refresco en vivo, ninguno especial (extensión directa: recargar
  `ToolRegistry.List()` periódicamente o en `tools/list_changed`). Para identidad real, que exista
  un mecanismo real de identidad de workload (SPIFFE/SVID u otro).
- **Coste:** S (refresco) / M (identidad real, depende de identidad de workload).

### A2A Gateway (`A2A-001`) no está montado en ningún binario real todavía

- **Descripción:** `go/internal/a2a` (`BuildAgentCard`/`AeonAgentExecutor`) es real y probado
  contra Temporal real, pero ningún `cmd/aeon-*` lo expone sobre HTTP — a diferencia de INT-003
  (donde extender `aeon-toolgw` fue directo), montar esto en `aeon-runcontroller` exige antes
  decidir qué agente(s)/grafo(s) concretos publica un gateway A2A en vivo y cómo el contenido de un
  `Message` A2A entrante se traduce en el input de un grafo — hoy `AeonAgentExecutor` siempre
  ejecuta el mismo grafo fijado en construcción, ignorando el contenido real del mensaje.
- **Fase objetivo:** cuando exista un caso de uso real que necesite invocar un agente Aeon
  concreto desde una red A2A externa.
- **Criterio de entrada:** decidir el mapeo Message→grafo (¿un grafo por skill anunciada en el
  `AgentCard`? ¿el texto del mensaje como único input de un nodo `tool_call`?) y qué
  agente(s) expone cada instancia del gateway.
- **Coste:** M.

### A2A: sin identidad/autorización real de caller (`A2A-001`)

- **Descripción:** el `AgentCard` puede declarar `securitySchemes`, pero `AeonAgentExecutor` no
  aplica ninguno — cualquier caller que alcance el endpoint puede enviar un mensaje. Mismo hueco
  que `INT-003` con MCP: hace falta autenticación real de caller antes de poder autorizar por
  identidad real en vez de tratar a todo el mundo igual.
- **Fase objetivo:** junto con la identidad de workload real (ver la entrada de SPIFFE/SVID) —
  `SEC-002` (Secret Broker, ya `DONE`) no resuelve esto por sí solo; es un broker de credenciales de
  *tools*, no un mecanismo de identidad de *caller*.
- **Criterio de entrada:** que exista un mecanismo real de identidad de workload (SPIFFE/SVID u
  otro).
- **Coste:** M.

### CrewAI: el tool-calling nativo del framework no está integrado (`INT-004`)

- **Descripción:** `AeonLLM` (Modo B) es real, pero el ejemplo deliberadamente evita el mecanismo
  de tool-calling propio de CrewAI (`crewai.tools.BaseTool`) — la llamada a `search.web` ocurre
  fuera del razonamiento del crew, como un paso `execute_tool` directo tras `kickoff()`, para no
  depender del formato exacto de parseo (ReAct u otro) que CrewAI usa internamente para decidir
  llamar a una tool, que no es un contrato público estable entre versiones. Un Agent CrewAI real no
  puede hoy decidir por sí mismo invocar un tool gobernado por Aeon durante su propio razonamiento.
- **Fase objetivo:** cuando un caso de uso real necesite que el Agent decida cuándo llamar una
  tool, no sólo que la ejecute en un orden fijo.
- **Criterio de entrada:** verificar el formato exacto que la versión de CrewAI en uso espera de
  `BaseLLM.call` para expresar una tool-call, y construir `AeonTool(BaseTool)` en consecuencia.
- **Coste:** M.

### Huella de dependencias real de `crewai` (`INT-004`)

- **Descripción:** `crewai>=1.15` trae ~110 paquetes transitivos reales (`chromadb`, `onnxruntime`,
  `lancedb`, `kubernetes`, `pyarrow`, ...) pensados para sus propias features de memoria/RAG que
  Aeon no usa — el worker Python ahora instala e importa todo eso sólo para tener acceso a
  `crewai.BaseLLM`/`Agent`/`Task`/`Crew`. Real, no un problema de Aeon, pero infla la imagen del
  worker de forma medible.
- **Fase objetivo:** si el tamaño de imagen se vuelve un problema real (build/deploy más lentos).
- **Criterio de entrada:** medir el impacto real en `deploy/compose/Dockerfile.python` una vez
  construida con esta dependencia, y decidir si vale la pena un extra/optional-dependency separado
  para Modo B en vez de instalarlo siempre.
- **Coste:** S.

### Conflicto real de versiones entre `crewai` y `openai-agents` (`INT-004`/`INT-005`)

- **Descripción:** `crewai>=1.15` fija `openai<3,>=2.30.0`; `openai-agents` saltó a exigir
  `openai>=3.0.0,<4` a partir de su propia versión `0.21.0` (19 de agosto de 2026). Ambos
  frameworks conviven hoy en el mismo `pyproject.toml` sólo porque `openai-agents` está fijado por
  debajo de ese salto (`>=0.20,<0.21`, ver `python/pyproject.toml`) — una solución real pero
  temporal: cuando `crewai` migre su propio pin a `openai>=3` (cosa que hará, tarde o temprano), este
  techo podrá levantarse; hasta entonces, cualquier fix/feature de `openai-agents` posterior a
  `0.20.x` queda fuera de alcance sin romper `INT-004`.
- **Fase objetivo:** cuando `crewai` publique una versión con `openai>=3` como dependencia, o cuando
  un caso de uso real necesite una versión de `openai-agents` posterior a `0.20.x`.
- **Criterio de entrada:** revisar el `requires_dist` de la versión de `crewai` en uso — si ya acepta
  `openai>=3`, levantar el techo de `openai-agents` en el mismo cambio.
- **Coste:** S.

### Endpoint compatible con la API Messages de Anthropic en `aeon-modelgw` (`INT-007`)

- **Descripción:** el Claude Agent SDK (`INT-007`) no tiene ningún punto de extensión de "cliente de
  modelo personalizado" — envuelve el CLI real `claude` (Claude Code), que llama directamente a
  `POST /v1/messages` de Anthropic (o a lo que `ANTHROPIC_BASE_URL`/Bedrock/Vertex tenga configurado
  el host). Para que las llamadas de modelo de esta integración pasen de verdad por el Model Gateway
  de Aeon, `aeon-modelgw` necesitaría un endpoint nuevo que hable el wire format de la API Messages
  de Anthropic (bloques de contenido, `tool_use`/`tool_result`, etc.) — un `INT-002` equivalente para
  ese formato, ya que `INT-002` sólo implementó OpenAI Chat Completions. Hasta que exista, `INT-007`
  sólo gobierna el lado de las tools (ver `roadmap.md`), no el del modelo.
- **Fase objetivo:** si un caso de uso real necesita que las llamadas de modelo de Claude Agent SDK
  pasen por el routing/budgets/redaction de Aeon, no sólo sus tool calls.
- **Criterio de entrada:** diseñar la traducción `NormalizedChatResponse` (o equivalente) ↔ forma de
  la API Messages de Anthropic, incluyendo bloques `tool_use`/`tool_result`, y decidir si vive en
  `aeon-modelgw` como una ruta nueva (`POST /v1/messages`) o en un servicio separado.
- **Coste:** M.
