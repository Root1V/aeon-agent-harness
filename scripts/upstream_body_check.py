#!/usr/bin/env python3
"""Ningún adaptador mete el cuerpo del proveedor en un mensaje de error (SEC-007).

Ese cuerpo es texto que escribe el proveedor, puede repetir la petición —medido: un 400 de
`unknown-model` devuelve el nombre del modelo **verbatim**— y por el camino del candidato fallido
llega a `span.RecordError`, o sea al almacén de Argus y al 502 del llamante, sin que nada en medio
pueda limpiarlo.

Aquí y no en un test de Go porque es una propiedad del FUENTE: el adaptador número cinco lo
escribiría igual y ningún test existente se pondría rojo, porque son verdes para los cuatro que ya
existen. Mismo instrumento y mismo motivo que scripts/tool_audit_sites_check.py.

Lo que se vigila es `string(body)` dentro de un `Errorf`/`Error(`: el helper correcto
(`providers.UpstreamError`) recibe los bytes y decide qué sale, y un struct que no puede guardar el
texto libre no lo puede filtrar aunque alguien añada un campo al formato más tarde.
"""
from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
ADAPTERS = ROOT / "go" / "internal" / "providers"

# Un error construido con el cuerpo dentro. `string(body)` es la forma que tenían los cuatro.
LEAKS = re.compile(r"(Errorf|errors\.New|Sprintf)\([^)]*string\((body|raw|b)\)")
# El marcador de «esto habla con un proveedor por HTTP».
TALKS_HTTP = re.compile(r"resp\.StatusCode")


def main() -> int:
    if not ADAPTERS.is_dir():
        print("upstream-body: go/internal/providers not found", file=sys.stderr)
        return 1

    adapters: list[pathlib.Path] = []
    failures: list[str] = []
    for path in sorted(ADAPTERS.rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        text = path.read_text(encoding="utf-8")
        if not TALKS_HTTP.search(text):
            continue
        adapters.append(path)
        for match in LEAKS.finditer(text):
            line = text[: match.start()].count("\n") + 1
            failures.append(
                f"{path.relative_to(ROOT)}:{line}: construye un error con el cuerpo del proveedor "
                f"dentro. Ese cuerpo puede repetir la petición (medido: un `unknown-model` devuelve el "
                f"nombre del modelo verbatim) y acaba en el almacén de Argus y en el 502 del llamante. "
                f"Usa providers.UpstreamError(nombre, resp.StatusCode, body), que conserva el tipo, el "
                f"título, el `request_id` y el largo, y no el texto libre."
            )

    if not adapters:
        # Que no haya adaptadores significa que el marcador dejó de ser el marcador, no que todo esté
        # bien: un guard que no encuentra nada que vigilar pasa por el motivo equivocado.
        print("upstream-body: no se encontró NINGÚN adaptador HTTP (marcador resp.StatusCode). O cambió "
              "el marcador o se movió el camino: revísalo en vez de confiar en este OK.", file=sys.stderr)
        return 1

    if failures:
        print(f"upstream-body: {len(failures)} problema(s):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    names = ", ".join(str(p.relative_to(ROOT)) for p in adapters)
    print(f"upstream-body: OK ({len(adapters)} adaptador(es) sin el cuerpo del proveedor en sus errores: {names})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
