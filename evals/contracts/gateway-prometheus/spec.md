# Contrato del gateway de Prometheus

Lo que un cliente del gateway de inferencia tiene que interpretar igual, sea cual sea su lenguaje.
Aportado por Axonium, que mantiene tres implementaciones contra él (Python, Go y, más adelante, Rust).

**`version: 1`** en `manifest.json`. Es el único handle para detectar que dos copias vendorizadas
divergieron: no hay nada automático que lo compruebe.

## Advertencia sobre la calidad de estos fixtures

**Están escritos a partir de la guía de integración, no grabados de un despliegue real.**

Eso los hace útiles para lo que se pusieron aquí: fijar las implementaciones **entre sí**, de modo
que Python, Go y Rust no puedan divergir en silencio. No sirven para fijarlas **al gateway**: hasta
que se regraben contra un despliegue vivo (Axonium `AXO-47`), un fixture que pasa demuestra que las
tres implementaciones coinciden, no que Prometheus se comporte así.

Es una distinción que conviene tener presente antes de usar un fixture verde como evidencia en un
incidente.

## Qué hay

| Fichero | Contiene |
|---|---|
| `manifest.json` | 12 casos en formato neutro de lenguaje |
| `fixtures/*.json` | 7 cuerpos de respuesta |
| `fixtures/*.sse` | 4 capturas literales de cable SSE |
| `errors.json` | 15 errores del gateway y 4 de OAuth, con status, sufijo y si es reintentable |
| `modalidades.md` | Los 4 valores de `modality`, su correspondencia con endpoints y el coste del catálogo |

Los `.sse` son bytes de cable literales, separadores de línea en blanco incluidos. **No los
reformatees**: un fixture reindentado prueba un stream que este gateway no envía nunca.

## Los casos describen resultados observables

Igual que los de la costura de durabilidad: un caso dice qué debe ver quien llama, nunca cómo se
llega. Las rutas de campo se resuelven **contra los accesores del SDK** donde los expone — un caso
que afirma `content` comprueba lo que un consumidor leería de verdad, no dónde cae el valor dentro
del payload.

Cada implementación trae su runner. Hoy hay dos, sin compartir una línea:

- Python: `python/tests/contract/test_manifest.py`
- Go: `go/axonium/contract_test.go`

## Tres cosas del extremo Prometheus que los casos codifican

**`Usage` tiene tres estados, no dos.** Con backend llama.cpp no hay chunk de `usage` en absoluto,
ni pidiéndolo: los contadores se derivan de los `timings` del chunk final (`prompt_n + cache_n` →
prompt, `predicted_n` → completion) y se marcan `estimated`. El caso `stream-basic` fija el camino
derivado y `stream-usage-chunk` el reportado. Un consumidor que no distinga medido de derivado
factura sobre una estimación creyéndola exacta, y eso no falla: solo cuadra mal.

**Prometheus solo puede alimentar tres de los cinco contadores del vocabulario acordado.**
Disponibles: `input`, `output`, `cache_read`. Por este camino `reasoning` y `cache_write` **no
existen en la fuente**, así que tienen que viajar como nulos explícitos y nunca como cero.

**El fallo de un stream llega en banda.** Cuando el backend muere a media generación, el `200` y el
`text/event-stream` ya están comprometidos, así que no hay status que lo comunique: llega un chunk
con clave `error`. El caso `stream-interrupted` lo fija, junto con la conservación del texto parcial
ya recibido. Se detecta **por la presencia de la clave, nunca por el texto del mensaje**: hoy la
guía documenta una sola cadena, pero eso es implementación, no contrato.

## Un hueco de la plataforma que estos fixtures no cubren

Los errores `422` de validación de request **no siguen RFC 9457**: salen con la forma por defecto de
FastAPI y **sin `request_id` ni `trace_id`**, así que no hay nada con que correlacionar contra los
logs de plataforma. No están en `errors.json` porque no están en el catálogo publicado — se
observaron.

La consecuencia para cualquier implementación: **hay que caer por status, no dar por hecho que todo
error trae `type`.** Preguntado al equipo de plataforma en `AXO-45`.
