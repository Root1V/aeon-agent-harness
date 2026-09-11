# Modalidades del catálogo

La correspondencia entre el vocabulario del catálogo de Prometheus y el que use quien lo consuma,
escrita **una vez** para que nadie la deduzca por su cuenta. Aportado por Axonium a petición de Aeon.

## Los cuatro valores

`modality` vale hoy uno de **cuatro** valores. Documentados en la guía de integración §3.1 y
**observados los cuatro en un despliegue real** el 2026-09-11:

| `modality` | Endpoint que lo sirve | Ejemplo visto en producción |
|---|---|---|
| `text` | `/v1/chat/completions` | `gpt-oss-20b-mxfp4` |
| `vision` | `/v1/chat/completions` | `qwen3vl-8b-q4` |
| `embedding` | `/v1/embeddings` | `qwen3-embedding-0-6b-q8-0-local` |
| `image` | `/v1/images/generations` | `sd-turbo-test` |

## La correspondencia **no es uno a uno**

Es lo que más fácil se implementa mal, y no falla al hacerlo: **`text` y `vision` se sirven los dos
en el endpoint de chat.** Un modelo de visión se llama exactamente igual que uno de texto; lo único
que cambia es que acepta partes de contenido `image_url`.

Así que quien tenga una clase «chat» en su vocabulario, mapea **dos** valores a ella:

```
text   ─┐
        ├─→ chat
vision ─┘

embedding ──→ embedding
image     ──→ image
```

Un enum `chat | embedding` —el que Aeon tenía— **no cubre el catálogo real de hoy**: deja fuera
`vision` y `image`, y hay un modelo de cada uno desplegado ahora mismo. Bajo una regla que deniega
lo no reconocido, eso rechaza un modelo de visión perfectamente válido; bajo una que asume chat por
defecto, manda un modelo de imagen al endpoint de chat.

## Regla para un valor desconocido

El catálogo va a crecer (`audio`, `rerank`, lo que sea). **Deniega, no adivines.** Un valor nuevo
que se trate como chat produce una llamada facturable con salida degenerada — que es el fallo
silencioso que la comprobación de modalidad existe para impedir.

Axonium hace lo contrario y por una razón distinta que conviene no copiar sin pensar: su
comprobación es un *guard rail opcional del cliente*, así que ante una modalidad desconocida **se
calla y deja pasar**, porque un guard rail que empieza a rechazar peticiones válidas cuando la
plataforma añade una modalidad es peor que no tenerlo. Para una *política de plataforma* como
`MDL-011` la elección correcta es la contraria: ahí el default-deny es el punto.

## Coste de consultar el catálogo

Medido contra el despliegue real, `GET /v1/models`, 6 modelos:

```
1020 bytes · 12 ms en frío, <1 ms después · sin cabeceras de caché
```

**No requiere autenticación.** Responde `200` sin cabecera `Authorization`, así que se puede
consultar antes de tener credenciales — útil para comprobar conectividad al arrancar.

**Es barato y sirve perfectamente para consultarlo al resolver un perfil y cachearlo.** El motivo de
que Axonium lo tenga apagado por defecto **no es el coste**: es que un SDK no debe hacer ninguna
petición que quien llama no pidió. Esa razón no aplica a un gateway que resuelve perfiles al
arrancar; ahí una petición al arranque es exactamente lo correcto.

### Tres advertencias antes de cachearlo para siempre

1. **Estar en el catálogo no significa ser llamable.** Un modelo registrado pero no desplegado
   devuelve `503 model-not-loaded`, y eso no se ve en el catálogo. El catálogo responde «¿existe y
   qué es?», no «¿funciona ahora?».
2. **El catálogo cambia en caliente.** Un operador puede añadir o retirar modelos sin reiniciar a
   nadie. Una caché de por vida no ve un modelo añadido después del arranque.
3. **`/v1/models` no es `/v1/models/mine`.** El primero es el catálogo público de la plataforma; el
   segundo está filtrado a lo que un token concreto puede llamar y **requiere autenticación**. Para
   verificar la declaración contra la realidad de la plataforma, el correcto es `/v1/models`.

## Otras claves del catálogo

Observadas en el despliegue real: `id`, `object`, `owned_by`, `modality`, `context_length`,
`family`, `quantization`, `served_by`. Solo `id` y `modality` están garantizados por la guía; el
resto depende del backend, así que trátalas como opcionales.

## Fijado por un caso ejecutable

El caso `catalog-list` de `manifest.json` afirma las **cuatro** modalidades. Está comprobado que
muerde: mutar la expectativa de `vision` a `chat` —que es exactamente el mapeo ingenuo que este
documento previene— hace fallar el caso.

Antes del 2026-09-11 el fixture solo traía `text` y `embedding`, así que **el propio corpus
licenciaba el error**: una implementación con un enum de dos valores pasaba los 12 casos.
