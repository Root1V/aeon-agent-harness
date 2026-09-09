# Costura de durabilidad — `Checkpointer`

**Versión:** `0.1-draft` · **Implementación de referencia:** Aeon `INT-009`, verificada contra
Postgres y Temporal reales.

El arnés persiste el diario de un run y **no decide nada sobre él**. Dos operaciones, y
deliberadamente ninguna tercera: un endpoint que responda «¿debo reejecutar este paso?» movería una
decisión al lado de la costura que se comprometió a no tomar ninguna.

## Identidad e idempotencia

Una entrada del diario se identifica por **`(run_id, step_id, phase)`**.

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
«algún número mayor que el último».

## `load`

Devuelve el diario completo del run en orden de `seq`, más `next_seq`.

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
POST /runs/{run_id}/checkpoints    -> 201 entrada nueva · 200 duplicado · 400 entrada inválida
GET  /runs/{run_id}/checkpoints    -> 200 siempre, incluso para un run desconocido
```

Los dos códigos de éxito son ambos éxito: la distinción es informativa. Quien corre bajo
*at-least-once* nunca debe necesitar distinguirlos para ser correcto.

## Compatibilidad hacia adelante

Un campo desconocido en un **record** no es motivo de rechazo: un diario se conserva años y será
leído por versiones que hoy no existen. Los campos que falten toman su valor por defecto en vez de
reventar la lectura.

En la **petición de `append` es al revés**: ahí un campo de más se rechaza, porque `step_id` y
`phase` son la clave de idempotencia y una errata en ellos no falla — registra en silencio una
entrada de más.

## Abierto

**Qué garantía da un `append` diferido.** La tabla de durabilidad de Synaptum marca como
`DEFERRABLE` los pasos cuya repetición es segura, es decir: no tienen que bloquear antes del efecto.
Lo que no está fijado es qué pasa con un diferido que aún no llegó a disco cuando el proceso cae —
si se puede perder (y el paso se repite, que es aceptable porque es idempotente) o si «diferido»
significa asíncrono pero garantizado. La implementación de Aeon hoy es **síncrona y durable
siempre**: supera el suelo, pero no entrega la propiedad de latencia que la clase diseña.
