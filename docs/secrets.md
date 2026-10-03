# Secretos: de dónde vienen y quién los ve (SEC-006)

Hasta `SEC-006` toda credencial de este proyecto llegaba por el mismo camino: `.env` → entorno de
compose → entorno del proceso. Funciona, y es por lo que `make dev` no necesita ningún paso de
provisión. También significa que el valor está en tres sitios que nadie eligió.

Este documento dice qué cambia, qué no, y cómo se midió cada afirmación.

## Lo que un secreto en el entorno expone

Tres superficies, y no son la misma:

| superficie | entorno | fichero |
|---|---|---|
| `docker inspect <contenedor>` | lo muestra | no aparece |
| `/proc/<pid>/environ` dentro del contenedor | lo muestra | no aparece |
| entorno de cada proceso hijo | lo hereda | no lo hereda |

La tercera es la que más importa aquí, porque este repositorio **lanza procesos hijos reales**:

- el worker arranca la CLI `claude` a través de `claude_agent_sdk`, cuyo transporte construye el
  entorno del hijo como `{**os.environ, **options.env}` — leído en su código fuente, no supuesto. Antes
  de `SEC-006` eso incluía `AEON_CALLER_TOKEN`, la credencial que permite llamar al Tool Gateway
  **como este agente**. Un binario de terceros la recibía sin ninguna razón.
- `aeon eval run` y `aeon replay` hacen `exec.Command` de un intérprete de Python **sin** fijar
  `cmd.Env`, así que el hijo hereda todo lo que tenga el shell del operador.

`go/internal/secretref` y `python/aeon_worker/secretref.py` quitan la variable del entorno en cuanto
la leen, lo que cierra la tercera columna para las dos rutas.

**No cierran las dos primeras, y eso está medido, no supuesto.** `os.Unsetenv` no reescribe
`/proc/<pid>/environ`: ese fichero reporta el bloque de entorno con el que el proceso fue `exec`ado.
Medido en Linux real (`golang:1.25-alpine`, variable puesta con `docker run -e`):

```
before: os.Getenv = true     before: /proc/self/environ contains it = true
after:  os.Getenv = false    after:  /proc/self/environ contains it = true
```

Por eso existe la ruta de fichero y no sólo el borrado: lo único que cierra las tres columnas es que
el valor **nunca** entre en el entorno.

## Las dos rutas

Cada credencial acepta `<NOMBRE>` o `<NOMBRE>_FILE`. La convención es deliberadamente aburrida —es la
de las imágenes de postgres/mysql/redis— porque todo almacén de secretos real acaba en «hay un
fichero»: `secrets:` de Docker Compose, volúmenes de secretos de Kubernetes, plantillas de Vault
Agent, un paso de `sops -d`. `secretref` es la costura que hace utilizable cualquiera de ellos sin que
este proyecto tenga que saber cuál.

Eso es también, leído al revés, el **criterio de entrada** que `backlog.md` tenía anotado para esta
entrada («que exista un almacén de secretos en el entorno de despliegue»): no podemos traer Vault al
repositorio, pero sí podemos dejar de ser el motivo de que no sirviera.

```bash
make dev-secret-files   # escribe deploy/compose/secrets/* (gitignored); no imprime ningún valor
make dev-secrets        # el stack con los secretos entregados como FICHEROS
```

### Tres estados, no dos

`secretref` distingue ausente, presente y **roto**, y rechaza el tercero:

- **las dos fuentes a la vez** (`X` y `X_FILE`): rechazo. Dos fuentes para un secreto significa que
  una rotación puede aplicarse a la que pierde y no fallar nada. No hay regla de precedencia a
  propósito; cualquier regla elige un ganador en silencio.
- **`X_FILE` que no se puede leer, o vacío**: rechazo. Un almacén de secretos caído no debe parecer
  «este despliegue no tiene ese proveedor» — ese estado es indistinguible de uno que deliberadamente
  no lo tiene, así que el routing caería al siguiente candidato del bundle y el run contestaría desde
  otro modelo, a otro precio, con éxito.
- **ninguna de las dos**: ausente, y **no** es un error. Un despliegue sin cuenta de Anthropic
  simplemente no registra ese adaptador.

## Qué proceso ve qué

`env_file` con el `.env` entero estaba en tres servicios, y el resultado medido era que cada secreto
del fichero llegaba a los tres. Medido antes y después (`docker compose config`, con valores centinela):

| secreto | antes | después |
|---|---|---|
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GOOGLE_API_KEY` | controlplane, modelgw, toolgw | **modelgw** |
| `OPENAI_COMPATIBLE_API_KEY` | controlplane, modelgw, toolgw | **modelgw** |
| `PROMETHEUS_CLIENT_SECRET` | controlplane, modelgw, toolgw | **modelgw, toolgw** (los dos lo leen) |
| `AEON_MEMORY_HMAC_KEY` | controlplane (ignorado, ver abajo) | **controlplane** |
| `AEON_CALLER_TOKEN` | controlplane, modelgw, toolgw — **no el worker** | **worker** |

Ninguna línea estaba mal por separado: el radio era una propiedad del mecanismo. Así que
`TestNoServiceLoadsTheWholeEnvFile` prohíbe **el mecanismo**, y
`TestEachSecretReachesOnlyTheServicesThatReadIt` fija la tabla con un motivo por fila que es una
afirmación sobre el código. Cuando falle, la pregunta no es «actualiza la lista», es si el servicio
que ganó una credencial tiene algo que la use.

## El defecto que esto destapó: `.env` no se leía

`docker compose` busca su fichero de interpolación en el **directorio del proyecto**, que es
`deploy/compose/` (donde vive el compose), no la raíz del repositorio, donde están `.env` y
`.env.example`. Así que los 16 `${VAR:-default}` del compose se resolvían **sin leer nunca el `.env`
que la documentación te dice que edites**. El Makefile pasa ahora `--env-file .env`.

Lo que eso significaba, medido:

- **`AEON_OTEL_ENDPOINT`** puesto en `.env` se quedaba en `otel-collector:4318` en los cuatro
  servicios Go. O sea que «apuntar todo el stack a un agente de Argus son estas tres variables» era
  falso — y ese cambio a una sola variable fue precisamente el arreglo de tener que editar cuatro
  servicios a mano.
- **`AEON_CALLER_TOKEN`** puesto en `.env` llegaba a controlplane, modelgw y toolgw —los tres que
  **verifican** tokens y nunca presentan uno— y **no** llegaba al worker, el único que lo necesita,
  que se quedaba con el token de desarrollo público y commiteado. **Rotar la credencial del worker
  editando `.env` no rotaba nada.**
- **`AEON_MEMORY_HMAC_KEY`** puesto en `.env` se quedaba en `dev-only-insecure-memory-hmac-key-change-me`,
  el valor por defecto commiteado, porque el bloque `environment:` de compose tiene precedencia sobre
  `env_file:`. Esa clave es la base entera de la detección de manipulación de `MEM-001`/`SEC-004`:
  quien la conozca puede falsificar un `provenance_hmac`, y el valor público está en este repositorio.
  `.env.example` decía «set a real, random value here», y hacerlo no tenía ningún efecto.

Los tres son el mismo defecto: un artefacto que afirma algo que el código no hace.

## Lo que sigue pendiente

- **El `.env` sigue siendo texto plano en el host** mientras lo uses. La ruta de fichero lo mueve a
  `deploy/compose/secrets/` (modo 600, gitignored), que es texto plano en el host también: lo que
  cambia es que ya no está en `docker inspect`, ni en `/proc/<pid>/environ`, ni en los hijos. Para que
  deje de estar en el host hace falta un almacén de secretos de verdad, y ahora se puede enchufar uno
  sin tocar este código: lo único que tiene que producir es el fichero.
- **Los proveedores del Model Gateway no pasan por el Secret Broker** (`SEC-002`), así que rotar una
  credencial sigue necesitando reiniciar `aeon-modelgw`. Entrada propia en `backlog.md`.
- **No hay identidad de workload** (SPIFFE/SVID) detrás de quién puede pedir un lease al Broker.
  Entrada propia en `backlog.md`.
