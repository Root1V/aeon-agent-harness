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
- **Nota (2026-09-20):** Synaptum pidió delegación a agentes remotos, y **no cumple este criterio**
  — necesitan el sentido contrario. Esto es el lado *servidor* (recibimos tareas); lo suyo es
  *egress* (emitimos hacia un agente remoto), que se abrió como `A2A-002` en el roadmap y se
  resuelve con un proxy en el Tool Gateway, sin cliente A2A ni ciclo de vida de Task. Anotado
  porque las dos cosas se llaman «A2A» y promover ésta creyendo que desbloquea aquélla es el error
  fácil.

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

### ~~No hay CI~~ — CERRADA el 2026-10-01, promovida como `CI-001`

> Cerrada con `.github/workflows/ci.yml` (cuatro jobs) y `scripts/check_skips.py`. Lo que la entrada no
> podía anticipar: **el riesgo no era que CI no existiera, era que existiera y fuera verde sobre nada.**
> Cada test de integración se auto-salta sin su infraestructura, así que olvidar una variable en el
> workflow lo deja pasando para siempre. El guard que cuenta los saltos encontró tres huecos en los
> targets que ya teníamos — detalles en `roadmap.md`.

### ~~No existe ninguna herramienta permitida que corra sin red~~ — CERRADA como `TOOL-008` el 2026-10-02

> Cerrada implementando `repository.read`, que era la opción que la entrada recomendaba. Lo que la
> entrada no podía anticipar: **la primera versión entregaba `.env`**. Montar el repositorio entero como
> root parecía la lectura obvia de «repository.read» y una sonda devolvió el secreto de Prometheus a un
> llamante con el token público. El montaje de referencia es ahora `docs/` y el gateway se niega a
> arrancar con un root que contenga credenciales. Detalles en `roadmap.md`.
>
> **`artifact.read` cerrado como `TOOL-009` el 2026-10-02**, con su escritor: un run de Deep Research
> aparca su informe y el gateway lo lee por id. Ya no queda ninguna herramienta declarada sin
> implementación en el manifiesto de referencia.

### No existe ninguna herramienta permitida que corra sin red (contexto original)

- **Descripción:** el despliegue de referencia registra exactamente dos herramientas y las dos necesitan
  algo externo: `search.web` sale a las máquinas de búsqueda públicas (que nos limitan por tasa) y
  `search.rag` necesita las credenciales de embeddings de Prometheus. `shell.exec` existe y la política
  lo prohíbe para todo agente, a propósito. Y `repository.read` y `artifact.read` están en
  `policy_bundle.yaml` **y** en el `tools.allow` de `examples/deep-research/agent.yaml` **sin
  implementación**, así que el manifiesto declara herramientas que el gateway no puede ejecutar.
  **Consecuencia medida el 2026-10-01:** el único camino extremo-a-extremo de ejecución de herramienta
  que podemos probar depende de la web pública, así que es intermitente por construcción — dos tests
  (uno Go, uno Python) tuvieron que pasar a saltarse con motivo cuando todas las máquinas refusaron a la
  vez.
- **Fase objetivo:** F2, con lo que quede de `TOOL-006`/`TOOL-007`.
- **Criterio de entrada:** ninguno especial, pero **sí una decisión**, y no es registrar un stub:
  `TOOL-007` quitó exactamente eso para que un despliegue sin proveedor de búsqueda no pudiera contestar
  una búsqueda. Lo honesto es **implementar `repository.read`** —lee un fichero del repo montado, no
  necesita red, y ya está declarado en el manifiesto y permitido por la política, así que hoy es una
  promesa incumplida— o quitarlo del manifiesto. Lo primero cierra además el hueco de CI.
- **Coste:** S.

### MinIO corre en el stack de referencia y nadie le habla

- **Descripción:** el servicio `minio` está en los cuatro perfiles de `deploy/compose` y **ni una línea
  de Go o de Python lo usa**. El único sitio donde aparece es un comentario en
  `python/aeon_context/offload.py` diciendo que un despliegue de producción apuntaría ahí el
  `ObservationStore`. Encontrado al buscar dónde vivían los artefactos para `TOOL-009`, que acabó
  usando un directorio compartido precisamente porque no había cliente de objetos.
- **Fase objetivo:** cuando los artefactos tengan que sobrevivir al host o compartirse entre máquinas.
- **Criterio de entrada:** ninguno especial, y **una decisión primero**: o se implementa el
  `ObservationStore` sobre S3 y `artifact.read`/`write_artifact_activity` pasan por ahí, o **se quita
  MinIO del compose**. Un servicio corriendo para nadie es un recurso consumido y una capacidad
  aparente — la misma familia que `deploy/helm` vacío. Lo segundo es gratis y honesto hasta que lo
  primero haga falta.
- **Coste:** S (quitarlo) / M (cliente S3 real detrás del mismo Protocol).

### El corpus dorado de identidad de paso solo se puede verificar en este portátil

- **Descripción:** `TestStepIdentityMatchesGoldenCorpus` lee
  `../../../../../Victor/coordinacion_project/contratos/identidad-de-paso/fixtures/hashes-dorados.json`
  — **fuera del repositorio**, en la carpeta de coordinación de los tres equipos. Es una decisión
  deliberada y está en `roadmap.md` («Extracción del repo de contratos», `DEFERRED`), pero la
  consecuencia no estaba escrita en ningún sitio: **el test de aceptación del contrato que compartimos
  con Synaptum y Axonium se salta en cualquier máquina que no sea esta.** Lo destapó
  `scripts/check_skips.py` al contar qué se salta con toda la infraestructura levantada.
- **Fase objetivo:** cuando el corpus tenga un sitio que las tres partes puedan leer.
- **Criterio de entrada:** acuerdo con Synaptum y Axonium sobre dónde vive. Hay dos salidas y **ninguna
  es copiarlo aquí sin decírselo**: publicarlo donde los tres CI lo alcancen, o vendorizar una copia
  **con su hash de procedencia** y un test que falle si divergen — lo segundo es más barato y tiene su
  propio modo de fallo (una copia que se queda atrás pareciendo verificada).
- **Coste:** S.

### No hay CI: las 93 filas verificadas dependen de que alguien corra `make` a mano (contexto original)

- **Descripción:** no existe `.github/workflows` ni equivalente. Todo lo que este roadmap afirma está
  medido, y está medido **una vez, en un portátil**. Con más de una persona tocando el repo eso es la
  diferencia entre «probado» y «se probó entonces». **No es teórico y ya pasó dos veces el 2026-10-01:**
  `test_end_to_end_tracing` llevaba roto desde el cambio de nombre de servicio de Argus (afirmaba
  `aeon-toolgw` cuando el servicio es `toolgw`) y nadie lo vio porque el único target que corre ese
  fichero no se había corrido desde entonces; y `test_approval_wait_is_observable` pasaba sola y
  fallaba en la suite porque otro test cerraba el TracerProvider del proceso.
- **Fase objetivo:** antes de cualquier piloto con otro equipo.
- **Criterio de entrada:** ninguno especial. Lo que hay que decidir es qué corre en cada push: `make
  test` entero es contenedores + descargas y tarda; `test-go` + `test-python` sin infra es barato y es
  la mayoría de la cobertura. Los targets que necesitan Postgres/Temporal/Tempo son los que de verdad
  cazan estas cosas, así que al menos uno tiene que correr en algún sitio.
- **Coste:** S (un workflow) / M (si hay que hacer los targets de integración reproducibles en CI).

### `deploy/helm` es un directorio vacío

- **Descripción:** existe `deploy/helm/` y no contiene **nada**. El despliegue real es
  `deploy/compose` y solo eso. Un directorio con ese nombre es una promesa que el repo no cumple, y
  es la misma familia de defecto que este proyecto lleva semanas encontrando en sus propios
  artefactos: algo que afirma una capacidad que no existe.
- **Fase objetivo:** cuando haya un destino que no sea un portátil.
- **Criterio de entrada:** un despliegue real sobre Kubernetes. Hasta entonces la acción honesta es
  **borrar el directorio**, no llenarlo: un chart escrito contra un cluster que no existe se escribe
  dos veces.
- **Coste:** S (borrarlo) / L (un chart de verdad).

### ~~Las claves de proveedor viajan en texto plano por el entorno del proceso~~ — CERRADA como `SEC-006` el 2026-10-03

El criterio de entrada («que exista un almacén de secretos en el despliegue») se leyó al revés: no se
puede traer Vault al repositorio, pero sí dejar de ser el motivo de que no sirviera. Todo almacén real
acaba en «hay un fichero», así que `<NOMBRE>_FILE` es la costura. Ver la fila `SEC-006` del roadmap y
`docs/secrets.md`; lo que destapó de paso (que `.env` no se leía para la interpolación de compose, y
que la clave HMAC de `MEM-001`/`SEC-004` corría con el valor público commiteado aunque el operador
hiciera lo que la documentación decía) vale más que la entrada original.

**Lo que queda, dicho sin adorno:** `deploy/compose/secrets/` sigue siendo texto plano en el host
(modo 600, gitignored), igual que `.env`. Lo que cambió es que ya no está en `docker inspect`, ni en
`/proc/<pid>/environ`, ni en el entorno de los procesos hijos. Enchufar un almacén de verdad ya no
toca este código: lo único que tiene que producir es el fichero.

### El `.env` sigue siendo plano en el host, y nadie rota nada automáticamente

- **Descripción:** `SEC-006` dejó la costura (`<NOMBRE>_FILE`) y un camino real (`secrets:` de compose,
  `make dev-secrets`), pero el valor sigue naciendo de un fichero en el host que alguien escribió a
  mano. No hay emisión, ni caducidad, ni rotación: `scripts/secret_files.sh` genera lo que es nuestro
  (clave HMAC, token de caller) y para lo que no es nuestro dice que no lo es.
- **Fase objetivo:** cuando el despliegue tenga un almacén real (Vault, SOPS, el de la nube que sea).
- **Criterio de entrada:** existe ese almacén **y** alguien decide cuál. La parte que falta ya no es
  código nuestro: es una decisión de despliegue más un `sidecar`/`agent` que escriba el fichero.
- **Coste:** S por nuestro lado (nada que cambiar), L por el del entorno.

### El esquema de Postgres no tiene migraciones versionadas

- **Descripción:** `store.Connect` llama a `Migrate()`, que aplica `schema.sql` idempotentemente bajo
  un advisory lock. Para cambios aditivos funciona y está probado. Lo que no existe es historia para
  un cambio **destructivo** —renombrar una columna, estrechar un tipo— ni forma de saber qué versión
  tiene una base de datos, ni rollback.
- **Fase objetivo:** antes del primer cambio de esquema sobre datos que importen.
- **Criterio de entrada:** ninguno especial, pero **sí una decisión**: una herramienta de migraciones
  (golang-migrate, atlas) o un `schema_version` propio. Lo segundo parece más barato y es cómo se
  acaba teniendo una herramienta de migraciones peor.
- **Coste:** M.

### La deriva de `step_seq` es invisible para Temporal, y no sabemos si es inofensiva (`RUN-004`)

- **Descripción:** nuestra `idempotency_key` es `hash(run_id, node_id, step_seq, args)` y `step_seq`
  sale de `GraphExecutionState.next_step_seq()`, **un contador posicional que vive en código de
  workflow**. Synaptum describió el 2026-09-29 un defecto silencioso suyo con exactamente ese
  ingrediente (identidad de paso posicional → un bucle que emite un paso más pide claves desplazadas →
  el diario no se consulta y los efectos se repiten sin una queja), y preguntó si nos aplica.
  **Nuestro primer instinto fue «a nosotros nos sale ruidoso, Temporal levanta un
  `Nondeterminism error`». Medido, es falso:** un cambio que incrementa `step_seq` **sin** añadir
  ningún comando replaya limpio —Temporal compara *comandos*, y el contador viaja dentro del *payload
  de entrada* de la Activity, que no compara—. La clave cambia (`f7d2f48e…` → `518734ef…`) y nada
  protesta. Lo que **no** conseguimos construir es el caso en que eso repita un efecto: un retry de
  Temporal **re-entrega el input grabado** en vez de re-derivar la clave, así que la ventana de
  `RUN-004` (crash entre la escritura y su registro) usa la clave original. O sea: el ingrediente está,
  la deriva es invisible, y lo que hoy nos salva es una propiedad del runtime y no una decisión de
  nuestro diseño. No poder construir el caso dañino no es lo mismo que no existir.
- **Fase objetivo:** F1 (durabilidad), con `RUN-004`.
- **Criterio de entrada:** ninguno especial. El trabajo es un test que intente construir el caso
  dañino de verdad —crash después de la escritura, **cambio de código** entre crash y reanudación,
  reanudar y contar las escrituras— y que, si no se puede construir, quede **afirmando esa
  imposibilidad** en vez de dejarla como creencia. Si se puede, el arreglo no es un detector de
  deriva (que puede negarse a reanudar un run legítimo, el propio reparo de Synaptum): es **persistir
  el identificador derivado y leerlo al reanudar en vez de re-derivarlo**.
- **Coste:** S (el test) / M (si hay que cambiar la derivación, porque es contrato compartido con
  Synaptum y Axonium).

### ~~La traza no cruza de `aeon-runcontroller` al worker~~ — PROMOVIDA a `roadmap.md` como `OBS-010b` el 2026-09-29

> Cerrada el mismo día que se abrió. Medido: una traza real con **7 spans y 2 servicios**, del
> `traceparent` entrante al `execute_tool` del worker Python. Y un hallazgo que esta entrada no podía
> anticipar: el `TextMapPropagator` del interceptor de Temporal **no** usa el global de OpenTelemetry por
> defecto (lo dice su propia documentación), y el global de Go es un no-op — así que pasarle el global
> habría sido peor que no pasarle nada. Detalles en `roadmap.md`.

### La traza no cruza de `aeon-runcontroller` al worker (contexto original, para la trazabilidad)

- **Descripción:** `OBS-010` instaló el interceptor OTel de Temporal en el lado **Python**, así que
  los spans de todas las Activities de un run son una sola traza en vez de ocho raíces suelas. El
  salto que falta es el primero: `go/cmd/aeon-runcontroller` arranca el workflow con
  `client.Dial(client.Options{HostPort, Namespace})` — **sin `Interceptors` ni
  `ContextPropagators`** —, así que el contexto de la petición HTTP que arranca el run no entra en la
  historia del workflow y la traza del worker empieza en el worker. Verificado leyendo el `Dial`, no
  supuesto. Argus lo detectó desde su almacén el 2026-09-30 sin que se lo dijéramos: «lo que todavía
  **no** veo es una traza que cruce del `runcontroller` al `worker` pasando por la espera». El SDK Go
  de Temporal trae `contrib/opentelemetry` para esto, igual que el de Python.
- **Fase objetivo:** F4 (observabilidad), junto a lo que quede de `OBS-002`.
- **Criterio de entrada:** ninguno especial — es la mitad Go del mismo cambio, y el lado Python ya
  está. Lo que hay que medir al hacerlo es lo que se midió en el lado Python: que **no añade comandos
  a una historia** (replay de una historia anterior al cambio) y que la traza resultante contiene de
  verdad los dos servicios, no solo el mismo trace id.
- **Coste:** S.

### Agent Console: context inspector y evidence graph (`OBS-002`)

- **Descripción:** `OBS-002` (ya `DONE`) sólo entrega el trace explorer — la única de las tres
  vistas prometidas con una fuente de datos real y durable hoy. `aeon_context`'s `LaneState` vive
  sólo en el proceso Python de un run mientras se ejecuta (nunca se persiste);
  `aeon_evidence.EvidenceLedger` es un objeto Python en memoria pura, sin persistencia alguna.
  Ninguno de los dos es consultable después de que un run termine, así que un "context inspector" o
  "evidence graph" reales necesitan antes una capa de persistencia que hoy no existe.
- **Fase objetivo:** cuando exista una necesidad real de inspeccionar contexto/evidencia después de
  que un run termine (hoy sólo se pueden ver mientras el run está en curso, dentro de sus propios
  logs/trazas).
- **Criterio de entrada:** `aeon_context`/`aeon_evidence` ganan persistencia real (Postgres, o el
  `ObservationStore` apuntando de verdad a MinIO/S3 en vez de sólo disco local por proceso).
- **Coste:** L (persistencia) + M (UI sobre ella).

### Agent Console no está montado en `aeon-controlplane`, sólo en `aeon-runcontroller`

- **Descripción:** `ConsoleHandlers` (`OBS-002`) vive junto al Run Controller porque ahí es donde
  ya existía `Controller.Status`. No muestra nada del Agent/Tool Registry (lifecycle, cuarentena de
  `A5`, ABOM de `FND-002`) — sólo estado de run + trace.
- **Fase objetivo:** cuando un caso de uso real necesite ver registro y ejecución en la misma
  página.
- **Criterio de entrada:** ese caso de uso existe.
- **Coste:** M.

### ~~Coste real por run/agente, no sólo por modelo~~ — PROMOVIDA a `roadmap.md` como `OBS-003b` el 2026-09-29

> Cerrada. Lo que esta entrada no podía anticipar: **medido, 1101 filas en el ledger real y todas con
> `run_id` NULL** — no era que faltara rellenar los campos, era que *nunca se había rellenado ninguno*,
> así que la mitad del título de `OBS-003` llevaba meses sin un solo dato. Y el criterio de entrada de
> abajo decía «sólo falta añadirlos a `DecideInput` y threadearlos hasta el body HTTP», que es cierto y
> es la mitad más pequeña: sin agregación por run ni por agente en el dashboard, los datos habrían
> llegado a un sitio donde nadie los mira. Tres hallazgos más en `roadmap.md`, incluido que
> `make test-mdl-015` —el target que el propio test de MDL-015 nombra en su mensaje de skip— **no
> existía**.

### Coste real por run/agente, no sólo por modelo (contexto original, para la trazabilidad)

- **Descripción:** `decideRequest` (`OBS-003`, ya `DONE`) acepta `run_id`/`agent_manifest_ref`
  opcionales y `FinOpsLedger` los graba cuando llegan, pero ningún llamador real del Model Gateway
  (DR-001's Planner/Researcher/Reporter, `aeon_worker.activities.model_activities.
  call_model_gateway`) los rellena hoy — así que `GET /finops/costs` agrega correctamente por
  modelo, pero "coste por run" o "coste por agente" (parte del título original de `OBS-003` en la
  spec) no tiene todavía ningún dato real que mostrar.
- **Fase objetivo:** cuando un caso de uso real necesite atribuir coste a un run/agente específico,
  no sólo a un modelo en agregado.
- **Criterio de entrada:** `call_model_gateway`/`decide_activity` (Python) empiezan a pasar
  `run_id`/`agent_manifest_ref` — normalmente disponibles ya en el contexto de la Activity que
  llama, sólo falta añadirlos a `DecideInput` y threadearlos hasta el body HTTP.
- **Coste:** S.

### Coste `compute_based` nunca se calcula (`OBS-003`)

- **Descripción:** un proveedor `compute_based` (hoy sólo `prometheus_inference`) nunca obtiene un
  `cost_usd` real — `finops.PricingTable.CostUSD` devuelve `ok=false` a propósito para él, ya que se
  factura por segundo de GPU, no por token, y no existe ninguna instrumentación de tiempo de
  inferencia real todavía.
  **Corregido el 2026-09-20 (`OBS-008`):** esta entrada decía que «el dashboard lo muestra
  honestamente (fila con `cost_model: compute_based`, sin coste inventado)». Era falso, y llevaba
  aquí escrito como propiedad correcta, que es por lo que el defecto no se veía. La respuesta de
  `/decide` sí omitía el coste; el **ledger** escribía la fila con `cost_usd = 0` en una columna
  `NOT NULL DEFAULT 0`, y `TotalsByModel` la sumaba y enseñaba `$0.00`. Ya está arreglado: la
  columna es anulable y el panel no imprime cifra donde no la tiene.
  **Y el criterio de entrada de abajo también cambió**: Axonium midió que
  `GET /v1/usage/{request_id}` ya devuelve `cost_usd` para modelos locales, así que no hace falta
  instrumentar tiempo de GPU — hace falta **leerlo** (`OBS-007`).
- **Fase objetivo:** cuando el coste de inferencia local sea significativo frente al de proveedores
  cloud y valga la pena medirlo.
- **Criterio de entrada:** existe una forma real de medir tiempo de GPU por llamada (el propio
  adaptador `prometheus_inference`, o el gateway de Prometheus, tendría que exponerlo).
- **Coste:** M.

### Ningún suite de eval reporta scores automáticamente todavía (`MDL-002`)

- **Descripción:** `MDL-002` (ya `DONE`) entrega el mecanismo completo y probado: `POST
  /quality-scores` real, `go/internal/store.QualityScoreStore` real (Postgres), y
  `modelgateway.Gateway.Decide` real saltando un candidato degradado. Lo que falta es el lado que lo
  alimenta — ningún suite de eval real (`provider_conformance` u otro, `aeon_evalops.runner`) llama
  a ese endpoint tras correr. Hoy sólo un reporte manual (`curl`/un test) puede degradar un
  candidato.
- **Fase objetivo:** cuando `provider_conformance` (u otro suite por-proveedor) corra con
  regularidad real (CI nocturno, según roadmap §7) y su resultado deba alimentar routing
  automáticamente.
- **Criterio de entrada:** `aeon_evalops.runner` gana un modo "reportar a aeon-modelgw" tras
  terminar un suite cuyo `case_id`/dataset mapee a un (provider, model) concreto — hoy los suites no
  necesariamente llevan esa granularidad.
- **Coste:** M.

### Abstracción de experiencia (MEM-004) y Learning Lab (F5 completo)

- **Descripción:** clustering/generalización de memorias procedurales, auto-mejora offline.
- **Fase objetivo:** F5.
- **Criterio de entrada:** F3 (memoria gobernada) lleva ≥1 ciclo de release en producción sin
  incidentes de seguridad de memoria.
- **Coste:** XL.

### Firma por-tool/por-skill en el ABOM (ASI04, depende de `FND-002`)

- **Descripción:** `go/internal/abom.Document` (`FND-002`, ya `DONE`) sólo incluye los *nombres* de
  `spec.tools.allow/deny` tal como aparecen en el manifiesto — no `ToolDescriptor`s completos
  resueltos contra el Tool Registry (hash de esquema, clasificación de riesgo, `idempotency_key`
  requerido, etc.), porque `aeon`, el CLI, no tiene todavía un cliente HTTP del Tool Registry.
  Firmar cada tool/skill individualmente (lo que menciona ASI04 en la redacción original de la
  arquitectura) necesita esa resolución primero.
- **Fase objetivo:** F4, cuando `aeon publish` (o un comando nuevo) necesite verificar que las tools
  que un manifiesto declara existen realmente en el Registry con el `ToolDescriptor` esperado.
- **Criterio de entrada:** `aeon` gana un cliente del Tool Registry (mismo patrón que
  `controlplaneClient` ya usa para Agent Registry).
- **Coste:** M.

### `aeon` no tiene comando `abom verify` (depende de `FND-002`)

- **Descripción:** `go/internal/abom.Verify` es real y probado, pero sólo se usa hoy desde el propio
  `aeon publish` (para confirmar lo que acaba de firmar) y desde tests — no hay una forma de
  verificar un `.abom.json` ya escrito, en otra máquina, sin escribir Go a mano.
- **Fase objetivo:** cuando exista un flujo real de distribución/despliegue que necesite verificar
  la procedencia de un ABOM antes de desplegar el agente que describe.
- **Criterio de entrada:** ese flujo de despliegue existe.
- **Coste:** S.

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

### Revocación cruzada de proceso: `aeon-controlplane` → `aeon-toolgw` (depende de `A5`)

- **Descripción:** `CircuitBreakerHandlers.applyQuarantine` (`A5`, ya `DONE`) llama a
  `secrets.Broker.RevokeAllForOwner` cuando está configurado — y funciona de verdad, probado
  end-to-end en `TestCircuitBreakerQuarantinesVersion`. Pero en el despliegue real de
  `deploy/compose/docker-compose.yml`, el Registry vive en `aeon-controlplane` y el Secret Broker
  real vive en `aeon-toolgw` — procesos distintos. Hoy `aeon-controlplane` monta
  `CircuitBreakerHandlers` con `Secrets: nil`, así que la revocación de credenciales en caliente no
  ocurre todavía en el despliegue real, sólo en el mismo proceso (como en el test).
- **Fase objetivo:** cuando exista al menos un tool real que emita leases con `IssueForOwner` en
  producción (hoy sólo `secrets.whoami`, una tool de demostración, lo hace).
- **Criterio de entrada:** `aeon-toolgw` expone un endpoint HTTP para revocar por owner, y
  `aeon-controlplane` lo llama desde `applyQuarantine` con la dirección de `aeon-toolgw` configurada.
- **Coste:** S.

### Nada reporta automáticamente el resultado de un run al circuit breaker (`A5`)

- **Descripción:** `POST /agents/{name}/{version}/outcomes` es real y probado, pero nada en el
  worker Python llama a este endpoint cuando un run real termina — el mismo hueco de wiring que
  `MEM-003`/`EVAL-004` documentaron antes de que Reflection/Learning Eval tuvieran su propio enganche
  a un workflow real. Hoy sólo un kill switch manual (`POST .../quarantine`) o una llamada manual a
  `/outcomes` puede cuarentenar una versión.
- **Fase objetivo:** cuando `AgentRunWorkflow`/`GraphRunWorkflow` tengan un punto de finalización
  real desde el que reportar (éxito/fallo, coste) hacia el control plane.
- **Criterio de entrada:** existe ese punto de finalización, y se decide qué cuenta como "fallo" a
  efectos del breaker (¿cualquier excepción no capturada? ¿sólo un budget agotado?).
- **Coste:** M.
- **Orden respecto a `OBS-009` (2026-09-25):** `OBS-009` va **antes o a la vez**, no después. El
  campo de coste del endpoint es `float64` con `omitempty`, así que quien enganche este wiring pasará
  el coste de cada run — y un run sin tarifar (hoy, toda llamada a `prometheus_inference`) viajará
  como `0` y **bajará la media**, que es fail-open en el control. Hoy el defecto es inofensivo
  precisamente porque nadie llama a `/outcomes`: implementar esta entrada es lo que lo enciende.

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

### ~~`aeon replay --assert-identical`~~ — PROMOVIDA a `roadmap.md` como `DX-003` el 2026-09-27

> **Cerrada.** Su criterio de entrada era «ninguno especial: trabajo directo», y es una de las siete
> comprobaciones del §7 del plan contra el riesgo nº1 (determinismo). Tenerla aquí era lo que estaba mal:
> una entrada de backlog sin criterio de entrada no es trabajo diferido, es trabajo sin hacer. Ver la fila
> `DX-003` para lo implementado, incluido el hallazgo de que un test de replay que graba y reproduce con el
> mismo código solo prueba que el código coincide consigo mismo.

### `aeon replay --assert-identical` (contexto original, para la trazabilidad)

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
- **Fase objetivo:** F4. Requiere que el Model Gateway sepa qué agente/run está haciendo la
  llamada, algo que este endpoint no recibe hoy (un cliente OpenAI genérico no manda ese contexto).
  `OBS-003` (FinOps, ya `DONE`) resolvió esto para `POST /decide` (que sí acepta
  `run_id`/`agent_manifest_ref` opcionales) — ver la entrada de coste por-run/agente más abajo —
  pero no para este endpoint OpenAI-compatible. (`MDL-002`, ya `DONE`, no comparte este requisito:
  enruta por score de proveedor+modelo, no por identidad de agente/run.)
- **Criterio de entrada:** decidir cómo un cliente externo identifica el run/agente que hace la
  llamada — ¿un header custom? ¿parte del `model` string (`profile:run_id`)? — antes de poder
  contabilizar nada contra un presupuesto real.
- **Coste:** M.

### ~~Tracing real de punta a punta para runs Python~~ — PROMOVIDA a `roadmap.md` como `OBS-006b` el 2026-09-28

> **Cerrada.** Su criterio de entrada era «ninguno especial». Y al implementarla resultó que el problema no
> era el que esta entrada describía: no es que al lado Python le faltaran spans, es que **no había traza** —
> el propagador global de OTel en Go es no-op por defecto, así que cada servicio creaba raíces y el test de
> `OBS-001` no lo vio porque consultaba los tres spans por separado. Incluye la integración con Argus, que
> es configuración y no dependencia. Ver la fila `OBS-006b`.

### Tracing real de punta a punta para runs Python (contexto original, para la trazabilidad)

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

### ~~Conectar Reflection (MEM-003) a un workflow real~~ — PROMOVIDA como `MEM-003b` el 2026-09-29

> **Cerrada.** Criterio de entrada: «ninguno especial». Al implementarla salió lo que la entrada no podía
> anticipar: `MemoryStore.Create` asigna un UUID aleatorio y su `INSERT` no tiene `ON CONFLICT`, así que un
> reintento de la Activity duplicaba candidatos **en silencio**. El arreglo es un `memory_id` determinista
> (UUIDv5 — la primera versión usaba un hash y la columna es UUID), que convierte el duplicado silencioso
> en un 409 que ya existía. Ver la fila `MEM-003b`.

### Conectar Reflection (MEM-003) a un workflow real (contexto original, para la trazabilidad)

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
- **Fase objetivo:** F4, para el job periódico en sí (`OBS-002`/`OBS-003`, ambos ya `DONE`, no
  resolvieron esto — son observabilidad, no un scheduler); el config-as-code de decaimiento podría
  vivir en el mismo `ModelPolicyBundle`-style YAML o uno propio, a decidir junto con `EVAL-004`
  (F3, ya `DONE`) ya que también toca cómo se gobierna una memoria `ACTIVE`.
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

### Catálogo MCP de salida (`INT-003`): identidad real de cliente MCP

> **El refresco en vivo se PROMOVIÓ como `INT-003b` el 2026-09-29** y queda cerrado. Al implementarlo
> apareció lo que esta entrada no podía anticipar: el catálogo no solo era estático, **exponía la versión
> MÁS ANTIGUA** de cada tool, porque `List` devuelve todas las versiones y el bucle de arranque dejaba que
> la última iterada ganara. Refrescar sin arreglarlo habría refrescado a la versión equivocada.
>
> **Lo que sigue aquí es la otra mitad:** todo llamador MCP externo se autoriza como un único principal
> Cedar compartido (`McpClient::"external-mcp-client"`), porque el `clientInfo` que un cliente MCP declara
> no lo verifica el protocolo y usarlo como principal sería inventar una frontera de confianza que no
> existe. Necesita identidad de carga de trabajo real (SPIFFE/SVID) — ver su entrada en este fichero.
>
> - **Criterio de entrada:** que exista un mecanismo real de identidad de workload.
> - **Coste:** M.

### Catálogo MCP de salida (contexto original, para la trazabilidad)

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
