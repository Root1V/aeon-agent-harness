# Normalización entre proveedores

**Versión:** `0.1-draft` · **Estado:** especificación escrita, **sin implementación que la corra
todavía** por el lado de Synaptum (`SYN-18`). El gateway de Aeon sí puede correrla hoy.

Bajo `H1 = D` la normalización se **especifica una vez y se implementa por lenguaje**. Este
documento es esa especificación. Lo que impide que las implementaciones diverjan no es la confianza:
son los casos de `fixtures/`, que ambas ejecutan contra los mismos cuerpos.

## Qué normaliza quién

Cada proveedor tiene **exactamente una** implementación de normalización por lenguaje, alojada donde
vive el conocimiento de ese proveedor. Nadie normaliza lo de otro.

| Clase de inferencia | Quién |
|---|---|
| Local — todo Prometheus | Axonium, en cada uno de sus tres sabores |
| Cloud — Anthropic, OpenAI, Gemini | adaptadores del gateway (Go) y adaptadores dev de Synaptum (Python) |

La dirección **request** no se normaliza: hay un solo esquema, el del vocabulario compartido, y todos
lo consumen. Este documento trata la dirección **response**.

## El mensaje de sistema no es un mensaje

Viaja en `Request.system`. Ningún proveedor lo trata como turno: OpenAI lo extrae a `instructions`,
Anthropic a `system` y Gemini a `systemInstruction`. Meterlo en la lista de mensajes obliga a cada
adaptador a volver a sacarlo, y a equivocarse de forma distinta al hacerlo.

## Los argumentos de una tool call llegan decodificados

El cable los entrega como **cadena JSON**:

```json
"function": { "name": "get_weather", "arguments": "{\"city\":\"Lima\"}" }
```

El adaptador la parsea. `ToolCall.arguments` es siempre un objeto, nunca su serialización: el bucle
no debería tener que adivinar qué recibió.

**Una cadena que no parsea no se convierte en objeto vacío.** Es un error del proveedor y se levanta
como tal — un `{}` silencioso ejecutaría la herramienta sin argumentos.

## `content` nulo con tool calls no es una respuesta vacía

Cuando el modelo devuelve llamadas en vez de prosa, `content` es `null`. El mensaje unificado lleva
**solo** las partes `tool_call`, sin una parte de texto vacía: una parte de texto vacía y la ausencia
de texto no son lo mismo cuando alguien las concatena.

## Razonamiento y respuesta son dos flujos

El chain-of-thought viaja aparte (`reasoning_content` en el cable) y llega **antes** de cualquier
token de respuesta. Se normaliza como parte `thinking`, nunca concatenado al texto.

**Ninguno se infiere del otro.** Hay un caso real: con `max_tokens` corto el modelo nunca sale de la
fase de razonamiento — `content` queda vacío, `finish_reason` es `length`, y hay consumo que pagar.
Un consumidor que solo mire el texto ve una respuesta vacía sin explicación; con el razonamiento
conservado, hay algo que registrar.

Los proveedores que exigen devolver el bloque intacto en el turno siguiente traen una firma. Se
transporta sin interpretarla.

## Consumo: tres estados, y `input` es inclusivo

`Usage` tiene cinco contadores y ninguno vale cero por defecto:

- **`null`** — nadie lo midió.
- **`0`** — se midió y fue cero.
- **`estimated: true`** — el valor es **derivado**, no reportado.

La diferencia no es purismo. Si `cache_write` llega como cero cuando la fuente no lo reporta, un
bucle concluye que escribir en caché es gratis y decide mal en cada compactación. No falla: cuadra
mal.

**`input` incluye los tokens servidos desde caché**, y `cache_read` dice cuántos de ellos lo fueron.
Inclusivo y no disjunto, a propósito: es lo que los proveedores reportan tal cual, así que el
adaptador **copia en vez de restar**. Copiar no se puede hacer mal; restar sí, y olvidar la resta
contaría dos veces lo cacheado sin producir ningún error. `cache_write` no forma parte de `input`.

### Cuando no hay objeto de consumo

Prometheus con backend llama.cpp **no emite un chunk de `usage`**, ni pidiéndolo. Los contadores se
derivan de `timings` del chunk final:

| Campo unificado | Origen | |
|---|---|---|
| `input` | `prompt_n + cache_n` | total de entrada, incluida la caché |
| `cache_read` | `cache_n` | subconjunto cacheado |
| `output` | `predicted_n` | |
| `reasoning` | — | `null`: no existe en la fuente |
| `cache_write` | — | `null`: no existe en la fuente |

Todo lo derivado así lleva `estimated: true`.

**Si hay objeto de `usage`, manda él** y `timings` se ignora, aunque venga. Lo reportado no se
sustituye por una derivación.

## Motivo de fin

| Cable | Unificado |
|---|---|
| `stop` | `stop` |
| `length` | `length` |
| `tool_calls` | `tool_calls` |
| `content_filter` | `content_filter` |
| ausente o desconocido | `stop` |

Un motivo ausente se trata como parada normal: es lo que hacen los proveedores que lo omiten en la
respuesta no-streaming.

## Streaming

Ciclo `start` / `delta` / `end` uniforme para texto, razonamiento y tool calls.

**Los argumentos de una tool call llegan troceados y no son JSON válido hasta el final.** Quien
consume acumula los `delta` y parsea **solo** al recibir el `end` correspondiente. Un adaptador que
intente parsear cada fragmento producirá errores en el camino feliz.

**El último evento es siempre `finish`**, con la respuesta acumulada completa y su consumo. Quien
consumió los deltas no debería tener que reconstruirla.

**El consumo es obligatorio en `finish`.** El upstream OpenAI-compatible solo lo emite con
`stream_options: {include_usage: true}`, así que ese flag lo pone el gateway **siempre**; el evento
no lo lleva opcional. Si no se pone, el coste de todas las llamadas en streaming es invisible hasta
la factura.

**Un stream vacío no es un error.** `[DONE]` sin ningún chunk produce una respuesta con mensaje
vacío, motivo `stop` y consumo sin medir.

### Interrupción a mitad

Un error a mitad de stream levanta el error de la taxonomía que corresponda. **Los `delta` ya
emitidos no se retiran**: se generaron y se pagaron. Quien consume conserva lo parcial y sabe que el
turno no terminó.

No hay evento de cancelación y no lo habrá. Cerrar el canal *es* la señal; un evento que viaja por el
mismo canal que intenta detener llega tarde por construcción.

## Conformidad

Los casos de `fixtures/` referencian los cuerpos de `../gateway-prometheus/fixtures`, que Axonium
publicó y aquí no se duplican. Cada caso describe la **proyección unificada observable** de un cuerpo
concreto.

**Aviso que acota lo que un verde significa:** esos cuerpos están escritos a partir de la guía, **no
grabados de un despliegue real**. Un caso que pasa demuestra que las implementaciones coinciden entre
sí, no que Prometheus se comporte así. Eso llega con `AXO-47`.

El runner lo trae cada proyecto. Aquí no entra implementación.
