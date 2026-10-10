#!/usr/bin/env python3
"""Una puerta que sabe ejecutar un tool tiene que saber registrar un rechazo (INT-014).

Por qué un guard de texto y no una propiedad del tipo: para la puerta MCP SÍ está en el tipo
—`mcp.ToolExecutor` declara `RecordRefusal`, así que un ejecutor que solo sepa ejecutar no
compila—, pero la puerta HTTP tiene el `*toolexec.Executor` concreto, donde no hay interfaz que
obligue a nada. Y el chokepoint que lo habría hecho imposible de olvidar se rechazó a propósito:
habría metido en el ejecutor una rama «¿debo ejecutar?», y hoy es imposible que ejecute un tool
denegado porque nunca ve uno. Así que esto es «olvidarse se detecta», y se dice así.

Aquí y no en un test de Go porque es una propiedad del CÓDIGO FUENTE, no del comportamiento: una
puerta nueva que ejecute y no registre rechazos pasaría todos los tests existentes —son verdes para
las puertas que sí existen— y el hueco solo se vería auditando a mano. Mismo instrumento y mismo
motivo que scripts/compose_ports_check.py.
"""
from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
GO = ROOT / "go"

# El marcador de «esto es una puerta de ejecución de tools»: usa el tipo con el que se ejecuta.
MARKER = "toolexec.Invocation"
EXECUTES = re.compile(r"\.Execute\(")
RECORDS_REFUSAL = re.compile(r"\.RecordRefusal\(")


def main() -> int:
    if not GO.is_dir():
        print("tool-audit-sites: go/ not found", file=sys.stderr)
        return 1

    doors: list[pathlib.Path] = []
    failures: list[str] = []

    for path in sorted(GO.rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        text = path.read_text(encoding="utf-8")
        if MARKER not in text or not EXECUTES.search(text):
            continue
        doors.append(path)
        if not RECORDS_REFUSAL.search(text):
            failures.append(
                f"{path.relative_to(ROOT)}: ejecuta tools ({MARKER} + .Execute(...)) y no llama "
                f".RecordRefusal(...) en ninguna parte. Una puerta que ejecuta y no registra sus "
                f"rechazos deja sin respuesta «qué agente fue rechazado en qué tool» justo para los "
                f"llamadores que no son un run nuestro (INT-014)."
            )

    if not doors:
        # Que no haya puertas significa que el marcador dejó de ser el marcador, no que todo esté
        # bien: un guard que no encuentra nada que vigilar es un guard que pasa por el motivo
        # equivocado, y eso ya pasó una vez en este repo con un grep que no casaba nada.
        print(
            f"tool-audit-sites: no se encontró NINGUNA puerta de ejecución (marcador {MARKER!r}). "
            "O el marcador cambió o se movió el camino de ejecución: revísalo en vez de confiar en "
            "este OK.",
            file=sys.stderr,
        )
        return 1

    if failures:
        print(f"tool-audit-sites: {len(failures)} problema(s):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    names = ", ".join(str(p.relative_to(ROOT)) for p in doors)
    print(f"tool-audit-sites: OK ({len(doors)} puerta(s) ejecutan y registran rechazos: {names})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
