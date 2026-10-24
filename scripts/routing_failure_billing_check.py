#!/usr/bin/env python3
"""Una superficie que enruta tiene que cobrar lo que se generó antes de fallar (FIN-003).

`Gateway.Decide`/`DecideStreaming` pueden descartar un candidato DESPUÉS de que el modelo
respondió (`emptyAnswer`, `MDL-015`): esos tokens se generaron y se pagaron. En el camino feliz la
fila la escribe `recordCost`, que las tres superficies ya llaman, así que ahí no hay nada que
olvidar. El camino que SÍ se olvida es el del error: el 502 contestaba sin escribir nada, y una
superficie nueva lo haría igual sin que ningún test existente se pusiera rojo — los que hay son
verdes para las superficies que ya existen.

Aquí y no en un test de Go por eso mismo: es una propiedad del CÓDIGO FUENTE. Mismo instrumento y
mismo motivo que scripts/tool_audit_sites_check.py, y la misma honestidad: es «olvidarse se
detecta», no «olvidarse es imposible».
"""
from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
API = ROOT / "go" / "internal" / "api"

# El marcador de «esto enruta»: llama al gateway y maneja su error.
# `Decide` y `DecideStream`, nombrados por el prefijo y no por una lista: una tercera variante
# —la que falte mañana— tiene que entrar en el guard por defecto y no cuando alguien se acuerde.
ROUTES = re.compile(r"h\.Gateway\.Decide\w*\(")
BILLS_THE_FAILURE = re.compile(r"recordDiscardedAttemptsFromError\(")


def main() -> int:
    if not API.is_dir():
        print("routing-failure-billing: go/internal/api not found", file=sys.stderr)
        return 1

    surfaces: list[pathlib.Path] = []
    failures: list[str] = []

    for path in sorted(API.rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        text = path.read_text(encoding="utf-8")
        if not ROUTES.search(text):
            continue
        surfaces.append(path)
        if not BILLS_THE_FAILURE.search(text):
            failures.append(
                f"{path.relative_to(ROOT)}: llama al gateway y no llama "
                f"recordDiscardedAttemptsFromError(...) en ninguna parte. Cuando todos los "
                f"candidatos fallan, puede que todos hayan respondido y que todas sus respuestas se "
                f"hayan tirado: el llamante recibe un 502 y el modelo corrió igual. Sin esa llamada "
                f"esos tokens no los ve el dashboard, ni SpendForRun, ni el techo de presupuesto — y "
                f"el error del techo queda en la dirección que no se queja (FIN-003)."
            )

    if not surfaces:
        # Que no haya superficies significa que el marcador dejó de ser el marcador, no que todo
        # esté bien: un guard que no encuentra nada que vigilar pasa por el motivo equivocado.
        print(
            "routing-failure-billing: no se encontró NINGUNA superficie que enrute (marcador "
            "h.Gateway.Decide...). O cambió el marcador o se movió el camino: revísalo en vez de "
            "confiar en este OK.",
            file=sys.stderr,
        )
        return 1

    if failures:
        print(f"routing-failure-billing: {len(failures)} problema(s):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    names = ", ".join(str(p.relative_to(ROOT)) for p in surfaces)
    print(f"routing-failure-billing: OK ({len(surfaces)} superficie(s) cobran el fallo: {names})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
