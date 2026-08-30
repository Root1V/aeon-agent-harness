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

### Sandbox de shell/browser fuera del perfil Deep Research

- **Descripción:** el perfil Deep Research del MVP es read-only (search/RAG/repo.read); ejecución de
  shell/código/browser arbitrario queda fuera hasta F4 (`TOOL-003`).
- **Fase objetivo:** F4.
- **Criterio de entrada:** un segundo perfil de agente (acción) lo requiere explícitamente.
- **Coste:** L.

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

### `FrameworkAdapter` más allá de LangGraph

- **Descripción:** CrewAI, OpenAI Agents SDK, Microsoft Agent Framework, Claude Agent SDK
  (`INT-004`..`INT-007`). El MVP sólo valida el patrón con LangGraph.
- **Fase objetivo:** F4.
- **Criterio de entrada:** un proyecto concreto ya usa ese framework y quiere adoptar Aeon sin
  reescribir su orquestación.
- **Coste:** M cada uno (una vez que el patrón `FrameworkAdapter` está probado).

### ABOM firmado (FND-002)

- **Descripción:** bill of materials reproducible y firmado por versión/deployment.
- **Fase objetivo:** F4.
- **Criterio de entrada:** hay más de un entorno de despliegue (staging/prod) y se necesita
  trazabilidad de qué versión de qué corre dónde.
- **Coste:** M.

### Secret Broker (SEC-002) con credenciales de corta vida

- **Descripción:** el MVP usa secretos de entorno/compose estáticos; credenciales efímeras e
  identidad de workload (SPIFFE/SVID) son F4.
- **Fase objetivo:** F4.
- **Criterio de entrada:** el despliegue sale de un entorno de desarrollo/demo hacia algo con datos
  reales sensibles.
- **Coste:** L.

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
