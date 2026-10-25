# Normalización entre proveedores

**Versión:** `0.2-draft` · **Estado:** **la dirección response, implementada y ejecutada por los dos
lados; la dirección request, especificada aquí y ejecutada solo por uno.**

| | |
|---|---|
| Synaptum (Python) | `SYN-18` cerrado el 2026-09-11 · adaptador `openai-compatible`. Las **dos** direcciones corren: 14 casos de response y 3 de request, añadidos el 2026-10-10 y el 2026-10-13 (`SYN-93`, `SYN-94`) |
| Aeon (Go) | response corrida contra el gateway el 2026-09-11 · 1 pasa, 8 chocan con un hueco de alcance, 3 divergencias — dos arregladas, la tercera es `MDL-014` (`A-41`). **La dirección request no la ejecuta su runner todavía**: anotada con coste S el 2026-10-13, esperando esta declaración |

> **La versión la subió Synaptum a `0.2-draft` el 2026-10-14 para que coincida con lo que ya decía
> `fixtures/openai-compatible.json`**, que llevaba `"version": "0.2-draft"` mientras esta cabecera
> decía `0.1`. Dos sitios que dicen la versión y uno se quedó atrás: si no era la intención, decidlo
> y se baja.
>
> **Y cómo se entera una copia vendorizada, que es el caso real.** Aeon midió que esta carpeta **no
> es alcanzable desde su máquina**, así que la huella `spec_sha256` del fixture no les sirve: no
> pueden traer el fichero para compararlo. Lo que sí funciona lo pidieron ellos y queda como
> protocolo: **quien cambie este fichero publica en el canal la versión y la huella**. Un número que
> los dos ven basta para saber que hay que resincronizar, sin alcanzar nada.

> **Esta cabecera dijo durante dieciocho días «sin implementación que la corra todavía por el lado de
> Synaptum», y dejó de ser cierto dos días después de escribirla.** Se quedó atrás y mantuvo una fila
> de otro equipo en `BLOCKED` esperando un documento que ya existía. Si vuelves a editar este fichero
> sin tocar esta tabla, la siguiente persona pagará lo mismo.

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

La dirección **request** tiene un solo esquema de entrada —el del vocabulario compartido— y durante
la `0.1` este documento dedujo de eso que no había nada que especificar. **Era falso, y costó un run
perdido por turno en un consumidor** (`VRT-SYN-004`): el esquema de entrada es uno, pero el **cuerpo
que sale al cable** lo construye cada adaptador, y las dos direcciones no son independientes — la
response **produce** la parte (`thinking`) que la request tiene que saber mandar, o no mandar. Ningún
caso del corpus podía ver eso, en ninguno de los dos lenguajes. La dirección request está
especificada abajo.

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

**Una cadena que no parsea no se convierte en objeto vacío.** Un `{}` silencioso ejecutaría la
herramienta sin argumentos, y entonces el modelo recibe un resultado donde debería recibir su error.
Eso no ha cambiado y es lo que esta sección fija.

**Lo que decía la `0.1` y era falso: «es un error del proveedor y se levanta como tal».** Lo escribió
Synaptum y lo cumplió su propio adaptador hasta la `1.0.0rc6`, y costó un run entero por turno en un
consumidor (`VRT-SYN-005`) — el modelo escribe un JSON cortado, que es una muestra mala, y el run
muere por un argumento mal escrito. Aeon midió la otra mitad del mismo error en su transporte, donde
levantarlo convertía la respuesta en un `502` y el turno se perdía antes de llegar a nadie
(`VRT-AEON-008`). La regla no estaba equivocada en el qué: **no decía dónde aplica**, y es la
propuesta que Aeon dejó declarada como divergencia en su copia en vez de reescribir este fichero.

**La regla, por capas.** El adaptador **no levanta nada**: deja los argumentos sin leer donde quien
decide pueda verlos —el texto crudo en la llamada, `arguments` vacío— y no ejecuta nada con ellos.
Quién decide depende de la capa, y las dos decisiones son correctas en su sitio:

| Capa | Qué hace con una cadena que no parsea |
|---|---|
| Adaptador / normalización | la conserva tal cual junto a la llamada; `arguments` queda vacío. **No levanta** |
| Transporte o gateway | la **pasa tal cual** con `200`. Levantarla pierde el turno de quien ni lo ha visto |
| Bucle de agente | la devuelve **al modelo** como resultado de error de esa llamada, con el texto dentro. Cuesta un turno, no un run |

Y el nombre del test que la cubre dice la propiedad y no el mecanismo, porque la propiedad sobrevive:
*nunca se convierte en un objeto vacío*.

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

## La dirección request

Lo que esta dirección fija no es el esquema de entrada: es **el cuerpo que sale al cable** a partir de
un `Request` del vocabulario compartido. Tres reglas, las tres medidas contra el mismo servidor por
dos implementaciones distintas.

### Un turno del asistente sin nada enviable no se envuelve

Medido (Aeon contra su despliegue, 2026-10-10; Synaptum contra el mismo dialecto):

```
content: "texto"                        -> 200
content: ""                             -> 200    un string vacío explícito SÍ es enviable
sin content, con tool_calls             -> 200    el turno normal de tool calling
content: null y sin tool_calls          -> 400    "Assistant message must contain either
                                                   'content' or 'tool_calls'"
el turno quitado del todo               -> 200
```

Así que un turno del asistente cuyo contenido no produce ni `content` ni `tool_calls` —el caso real
es el que solo lleva `thinking`— **se omite del cuerpo**. No se manda `content: ""` en su lugar: ese
string es enviable, pero afirma que el asistente dijo la cadena vacía, y no la dijo.

**Y `content: ""` no se colapsa con la ausencia de contenido.** Son dos afirmaciones distintas y el
servidor las distingue; un adaptador que escriba `content = texto || null` las colapsa sin enterarse.
Las dos implementaciones lo escribieron así y las dos lo arreglaron.

### El razonamiento del asistente vuelve al proveedor

En `reasoning_content`, en todos los turnos del asistente que viajan. Medido en
[`llama.cpp#27720`](https://github.com/ggml-org/llama.cpp/issues/27720): un modelo de razonamiento al
que se le pide continuar **desde su propia tool call** sin su canal de análisis emite cabeceras mal
formadas y el turno entero se pierde — **7 turnos fallidos de 120** sin devolverlo, **0 de 120**
devolviéndolo. Cerrado como no arreglable en el parser.

Dos precisiones que son parte de la regla:

- **La clave se omite cuando no hubo razonamiento.** Nunca `""`: ausente y vacío son afirmaciones
  distintas, igual que en los contadores de consumo.
- **El razonamiento no sustituye a `content` ni a `tool_calls`.** Mandarlo solo da el mismo 400 de
  arriba, así que la primera regla manda sobre esta: un turno que solo razona sigue sin viajar.

El razonamiento **cifrado** no tiene campo en este dialecto. No se descarta en silencio: se rechaza.

### Cómo se declara un caso de esta dirección

Formato acordado el 2026-10-14, a petición de Aeon.

| Campo | Qué es |
|---|---|
| `direction` | `"request"`. **Ausente significa `"response"`**, así que ningún caso existente cambia |
| `body_file` | un **`Request` del vocabulario compartido**, no un cuerpo del cable. Lleva `authored: true` por obligación: esta dirección no se graba de un proveedor, la construye el cliente |
| `expect` | describe el **cuerpo que sale al cable** |

Las claves de `expect`:

| Clave | Qué afirma |
|---|---|
| `wire_roles` | la lista de `role` de `messages`, en orden. Es lo que delata un sobre que ya no está |
| `wire_assistant_contents` | el `content` de cada mensaje del asistente, en orden. `""` y `null` son valores distintos y comparan distinto |
| `wire_assistant_reasoning` | el `reasoning_content` de cada mensaje del asistente, en orden, con `null` donde la clave no esté |
| `assistant_messages_sendable` | `true`: para **cada** mensaje del asistente, `content` es una cadena —vacía vale— **o** hay `tool_calls`. Es la regla de arriba escrita como invariante, y se comprueba sobre el cuerpo entero y no sobre el mensaje del caso: el 400 lo provoca cualquier mensaje que salga sin ninguna de las dos claves, y el turno culpable ya pasó |

**Las claves son nuevas a propósito.** Un runner que solo conozca la dirección response las verá como
claves que no sabe comprobar, y el guard de «el corpus afirma algo que este código no lee» debe
saltar. No hacen falta cambios para que **falle a gritos**; hacen falta para que **pase**.

**Y un caso sin `expect` no es un caso.** Pasa siempre y no es evidencia de nada. Aeon lo encontró en
su runner —vuelve temprano y no comprueba nada— y Synaptum tenía lo mismo por `KeyError`, que es
fallar por suerte. Las dos implementaciones lo comprueban ahora como regla.

## Conformidad

Los casos de `fixtures/` referencian los cuerpos de `../gateway-prometheus/fixtures`, que Axonium
publicó y aquí no se duplican. Cada caso describe la **proyección unificada observable** de un cuerpo
concreto.

**Aviso que acota lo que un verde significa, y hay dos clases de cuerpo.** Los de
`../gateway-prometheus/fixtures` **sí son grabaciones** desde el manifest v8 de Axonium. Los de
`bodies/` están escritos a mano y cada uno dice en su propio `$authored` por qué no puede ser una
grabación — y ninguna de esas razones es «me venía bien»: o la propiedad ya no se puede grabar (un
consumo sin medir, que desde el v8 no ocurre) o la dirección no se graba de un proveedor (todos los
de `request`, que los construye el cliente).

Un caso que pasa demuestra que las implementaciones coinciden entre sí sobre ese cuerpo. Que el
cuerpo sea representativo lo dice de dónde salió, y por eso cada uno lo dice.

> Esta sección afirmó hasta el 2026-10-14 que **ninguno** de los cuerpos era una grabación, mientras
> la cabecera decía que casi todos lo eran. Las dos frases llevaban un mes conviviendo.

El runner lo trae cada proyecto. Aquí no entra implementación.
