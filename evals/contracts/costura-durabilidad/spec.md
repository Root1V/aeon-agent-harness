# Costura de durabilidad — `Checkpointer`

**Versión:** `0.2-draft` · **Implementación de referencia:** Aeon `INT-009` + `INT-009b` + `INT-009c`, verificada
contra Postgres y Temporal reales.

**Qué cambió en `0.2` y por qué no rompe a nadie** (`VRT-AEON-004`, pedido por Veritium): la
identidad gana un cuarto componente, `sub_run_id`, **opcional en la petición**. Un cliente que lo
omite se comporta exactamente como en `0.1` — el centinela `''` lo escribe el esquema de la
implementación, no el cliente —, así que una implementación de `0.1` sigue siendo conforme para
runs sin delegación. Lo que `0.1` no podía expresar es un run con delegación anidada: ver abajo.

**Y `0.2` declara `durability` en `run-state`, que la implementación de referencia devolvía desde
`INT-011` sin que este documento lo dijera.** Eso era un defecto del contrato y no del servicio: el
schema tiene `additionalProperties: false` en la raíz, así que **toda** respuesta de la referencia era
inválida contra el documento publicado — y nada lo notaba, porque nada validaba nada contra estos
schemas. Ahora hay un test que valida la petición y las dos respuestas reales contra ellos
(`TestTheDurabilitySeamMatchesTheSharedContract`), que es lo que hace que una versión signifique
algo. `durability` **no es obligatoria**: una implementación que no la calcule es conforme.

El arnés persiste el diario de un run y **no decide nada sobre él**. Dos operaciones, y
deliberadamente ninguna tercera: un endpoint que responda «¿debo reejecutar este paso?» movería una
decisión al lado de la costura que se comprometió a no tomar ninguna.

## Identidad e idempotencia

Una entrada del diario se identifica por **`(run_id, sub_run_id, step_id, phase)`**.

`sub_run_id` es la **ruta de delegación** y es **opaca**: nadie la parsea, la parte ni le lee
profundidad. Un nieto es una cadena más larga. Omitirla significa el **run raíz**, y el centinela
(`''`) lo decide quien guarda los datos — el cliente no tiene que elegir uno.

**Por qué está en la identidad**, que es la parte que `0.1` no podía expresar: dos sub-runs
distintos de un mismo run usan legítimamente el mismo `step_id`, porque son bucles separados y cada
uno numera sus propios pasos. Sin este componente, la entrada del segundo era un **duplicado** de la
del primero: `append` no escribía nada, devolvía `duplicate: true` y **reportaba éxito**. El diario
decía entonces que un paso ya había corrido cuando no, y un resume se lo saltaba. Una implementación
puede estar entera en verde sobre esto: un test que compruebe que un duplicado **no** crea una
segunda entrada pasa en los dos casos.

**El componente no puede ser nulable.** NULL no es igual a NULL en un índice único, así que cada
entrada del run raíz sería única contra sí misma y la deduplicación dejaría de funcionar del todo.

`append` es idempotente bajo esa identidad. Un duplicado es un **no-op y nunca un error** — quien
llama corre bajo ejecución *at-least-once* y genuinamente no puede distinguir un reintento de un
primer intento; obligarle a distinguirlo devolvería la parte difícil al otro lado de la costura.

`phase` es parte de la identidad, no decoración. Un valor desconocido se rechaza al entrar: si se
aceptara, no fallaría — registraría en silencio una segunda entrada de un paso que ya se ejecutó.

## Las dos fases

| Fase | Se escribe | Significa |
|---|---|---|
| `attempted` | antes del efecto | hay intención, no hay resultado |
| `completed` | después del efecto | hay resultado registrado |

Escribir `attempted` es **opcional y es decisión de quien llama**. Un paso que solo escribe
`completed` y muere a mitad del efecto no deja rastro y se reejecuta — seguro exactamente cuando el
efecto es idempotente, que es la misma condición bajo la que saltarse la escritura previa era seguro.

Acordado con Synaptum: se escriben las dos fases **solo para pasos declarados no idempotentes**
(`ToolDefinition.idempotent` / `ToolStep.idempotent`, `False` por defecto). El coste que importa no
es el número de `append`, es cuántos **bloquean antes del efecto**.

## `append`

**Entrada:** `run_id`, `step_id`, `phase`, `payload` opcional (JSON, opaco para la costura).

**Salida:** `seq`, `duplicate`, `payload_diverged`.

- `seq` — posición en el diario. En un duplicado, la de la entrada que ya existía.
- `duplicate` — la identidad ya estaba registrada y no se escribió nada.
- `payload_diverged` — llegó un duplicado con un payload **distinto** del almacenado. **Gana el
  primero**, porque es el que otros lectores pueden haber usado ya. Pero dos intentos del mismo paso
  produciendo resultados distintos es no-determinismo real, y un diario que lo traga en silencio es
  peor que no tener diario.

**La comparación de payload es semántica, no de bytes.** Reserializar el mismo objeto con las claves
en otro orden **no** es divergencia. Comparar bytes convertiría en falsa alarma cada reintento desde
un lenguaje que no garantiza el orden de las claves.

**Ausencia de payload y payload nulo son cosas distintas.** «No se registró resultado» no es «el
resultado registrado fue nulo».

## `seq`

Contiguo por run, empezando en `0`, asignado por el arnés. No hay huecos: un duplicado no consume
número. Con nodos en paralelo hay `append` concurrentes de verdad, así que la asignación se serializa
por run.

Contiguo importa: es lo que hace que `next_seq` signifique «por dónde continúa el diario» y no
«algún número mayor que el último». Contiguo **por run**, no por sub-run: una lectura filtrada
devuelve huecos en `seq`, y eso es correcto (ver `load`).

## `load`

Devuelve el diario **de un sub-run** en orden de `seq`, más `next_seq`.

**`load` toma el `sub_run_id` y lo filtra, con la misma regla que `append`:** omitido es el run raíz
y vacío explícito también. No es opcional en la firma de la implementación de referencia: es un
argumento obligatorio, porque un llamante que no sabe que existe el ámbito es exactamente quien lee
el diario de otra delegación sin notarlo.

**Esto lo decía `0.2` a medias y es un defecto de este documento** (`INT-009c`, encontrado por
Veritium): `0.2` metió `sub_run_id` en la identidad de la escritura y no dijo nada de la lectura. De
ese silencio salió una implementación de referencia que escribía por sub-run y leía todo el run, y
**ningún test del contrato lo veía** porque el schema de `run-state` no podía expresar un ámbito que
el documento no mencionaba. Un sub-run que reanudaba adoptaba los pasos de sus hermanos como propios
y se los saltaba.

**`next_seq` sigue siendo del run entero, no del sub-run.** Es la única parte de la respuesta que no
se filtra, y es deliberado: `seq` ordena el diario **entre** sub-runs (ver `seq`), así que derivarlo
del subconjunto le daría a un sub-run que reanuda números que sus hermanos ya escribieron. Eso
convierte un defecto de lectura en uno de orden, que es peor. Consecuencia directa, y es la parte que
un cliente tiene que saber: **`next_seq` no es igual al número de entradas devueltas** cuando el run
tiene delegación. Dentro de un sub-run los `seq` tampoco son contiguos; lo contiguo es el run.

Un run **desconocido devuelve un diario vacío, no un error**. Un bucle que pregunta «¿dónde estaba?»
antes de su primer checkpoint es la llamada normal, no un fallo; responder 404 obligaría a todos los
llamantes a tratar el camino feliz como caso especial.

### Las tres consultas derivadas

- `completed(step_id)` — hay resultado. El efecto ocurrió: **no se repite**, y el resultado
  registrado es desde donde se continúa.
- `attempted(step_id)` — hay intención sin resultado. El proceso cayó en medio, así que el efecto
  **pudo** haber ocurrido. **Excluyente de `completed`**: un paso terminado no está intentado, está
  hecho, y quien pregunta «¿qué hago ahora?» necesita una respuesta, no dos.
- `next_seq` — por dónde continuar.

## Superficie HTTP de referencia

```
POST /runs/{run_id}/checkpoints                     -> 201 entrada nueva · 200 duplicado · 400 inválida
GET  /runs/{run_id}/checkpoints[?sub_run_id=<ruta>] -> 200 siempre, incluso para un run desconocido
```

`sub_run_id` va en el query y **no** en el path: la ruta de delegación es opaca y lleva barras, así
que un segmento de path la partiría en varios. Omitirlo es el run raíz, y `?sub_run_id=` vacío
también — las dos formas caen en la raíz por igualdad simple, que es lo que la escritura guarda.

Los dos códigos de éxito son ambos éxito: la distinción es informativa. Quien corre bajo
*at-least-once* nunca debe necesitar distinguirlos para ser correcto.

## Compatibilidad hacia adelante

Un campo desconocido en un **record** no es motivo de rechazo: un diario se conserva años y será
leído por versiones que hoy no existen. Los campos que falten toman su valor por defecto en vez de
reventar la lectura.

En la **petición de `append` es al revés**: ahí un campo de más se rechaza, porque `step_id` y
`phase` son la clave de idempotencia y una errata en ellos no falla — registra en silencio una
entrada de más.

## Qué garantía da un `append` diferido

**Contestado por Synaptum el 2026-10-25** (`SYN-AEON-003`), y era la única pregunta abierta de este
documento. La respuesta es la débil de las dos: **un diferido que no llegó a disco se pierde, y el
paso se repite.**

Su mecanismo, en sus palabras: lo diferido se acumula en memoria y se vacía antes de cualquier
escritura durable, al cerrar el run y ante cualquier salida — así que si el proceso muere con cosas
pendientes, se pierden. Es correcto **por una sola razón**, y conviene que esté escrita porque es la
que sostiene todo lo demás: `DEFERRABLE` se asigna **únicamente** a pasos cuya repetición es segura.

Así que la garantía que este contrato fija es **«no bloquea antes del efecto»**, y NO «asíncrono pero
garantizado». La distinción no es terminológica: **un cliente que suponga la fuerte sobre una
implementación con la débil pierde pasos sin enterarse** — no hay error, no hay fila, y el síntoma
aparece como un paso que se repitió o como uno que no está.

**La implementación de referencia (Aeon) es síncrona y durable siempre.** Supera el suelo y no
incumple nada; lo que no entrega es la propiedad de latencia que la clase diseña. Si alguna vez la
entrega, este documento tiene que decir **cuál de las dos garantías** da, porque entonces la
diferencia empieza a ser observable.

## Abierto

Nada. La pregunta anterior de esta sección quedó contestada arriba.
