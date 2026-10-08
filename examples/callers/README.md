# `AEON_CALLERS_DIR` — un fichero por tenant

Alternativa a `AEON_CALLERS_PATH` para un despliegue **compartido entre equipos** (`GOV-001h`).
Apunta `AEON_CALLERS_DIR` a un directorio con un `<tenant>.yaml` por tenant; **declarar las dos
variables se rechaza al arrancar**, porque la que perdiera sería un fichero que alguien cree cargado.

## La regla que lo hace una frontera

**El nombre del fichero ES el tenant, y toda entrada dentro debe declarar ese mismo tenant.** Es más
estricto de lo que necesita el bundle de políticas, y es el motivo entero de que exista el
directorio: un bundle de políticas gobierna un tenant y no nombra ninguno dentro, pero un caller
**declara** el suyo — así que sin esta regla `veritium.yaml` podría declarar un caller en el tenant
`otro` y nada lo impediría. El nombre del fichero es lo que un revisor comprueba de un vistazo.

Lo demás se valida **sobre el directorio entero y no fichero a fichero**, porque dos equipos no ven
el fichero del otro: un `id` repetido o **un mismo `tokenSHA256` en dos ficheros** se rechazan
nombrando los dos ficheros. Comprobado por fichero, eso pasaría dos veces y qué identidad recibe una
petición lo decidiría el orden de lectura del directorio.

Un fichero mal nombrado —incluido un `README.md` o un `veritium.yaml.bak`— **se rechaza, no se
salta**: un fichero que alguien cree cargado y no lo está es peor que uno que falta. (Este README
está aquí y no dentro del directorio que se monta, por eso mismo.)

## Qué entregar para incorporar un tenant

Un solo fichero, que no toca lo de nadie:

```yaml
# veritium.yaml — el nombre es el tenant
apiVersion: harness.ai/v1
kind: CallerBundle
callers:
  - id: veritium-api
    kind: service          # service | human | external
    tenant: veritium       # tiene que coincidir con el nombre del fichero
    tokenSHA256: "<sha256 hex del bearer, nunca el token>"
    mayActAs:
      - veritium-case-run@1.0.0
    mayApprove: false
```

Y, si un worker compartido ejecuta runs de este tenant, `veritium` en el `mayActForTenants` de la
entrada de ese worker (`GOV-001f`) — eso sí vive en el fichero del tenant del worker, porque es un
privilegio **suyo** y no del tenant al que sirve.

`tokenSHA256` es el hash, nunca el secreto: el bundle que cargan los gateways no contiene ninguna
credencial. `sha256` del token en hex minúscula, 64 caracteres.
