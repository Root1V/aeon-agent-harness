#!/usr/bin/env python3
"""El compose de referencia no monta el socket de Docker (TOOL-011).

POR QUÉ UN GUARD Y NO UN COMENTARIO. Esa línea estuvo ahí desde `TOOL-003` con un comentario al
lado que explicaba correctamente lo que concedía —acceso equivalente a root sobre el host— y aun así
siguió montada por defecto en todos los despliegues, para un tool que **los dos** bundles de
política prohíben. Un comentario que describe un riesgo no lo quita, y el camino más probable para
que vuelva es que alguien lo necesite en local, lo monte, y lo comitee sin darse cuenta.

Aquí y no en un test de Go porque es una propiedad del fichero de compose, igual que
`compose_ports_check.py`: un montaje de más levanta perfectamente y no rompe nada visible.
"""
from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
COMPOSE = ROOT / "deploy" / "compose" / "docker-compose.yml"

# Una línea de montaje real: un elemento de lista, no un comentario.
MOUNT = re.compile(r"^\s*-\s*[^#\s]*docker\.sock")


def main() -> int:
    if not COMPOSE.is_file():
        print(f"compose-sandbox: {COMPOSE} not found", file=sys.stderr)
        return 1

    offenders = [
        (n, line.strip())
        for n, line in enumerate(COMPOSE.read_text(encoding="utf-8").splitlines(), start=1)
        if MOUNT.match(line)
    ]
    if offenders:
        print(f"compose-sandbox: {len(offenders)} montaje(s) del socket de Docker:", file=sys.stderr)
        for n, line in offenders:
            print(f"  - {COMPOSE.relative_to(ROOT)}:{n}: {line}", file=sys.stderr)
        print(
            "\nMontar /var/run/docker.sock concede acceso EQUIVALENTE A ROOT sobre el host: cualquier\n"
            "cosa que alcance la API de Docker puede arrancar un contenedor privilegiado con el sistema\n"
            "de ficheros del host dentro. Si lo necesitas, ponlo en un docker-compose.override.yml, que\n"
            "es tu fichero y no el de referencia (TOOL-011).",
            file=sys.stderr,
        )
        return 1

    # Que el centinela siga existiendo: si el comentario que explica la decisión desaparece, este
    # guard pasa sin vigilar nada y el siguiente que lea el fichero no sabrá por qué no está montado.
    text = COMPOSE.read_text(encoding="utf-8")
    if "TOOL-011" not in text:
        print(
            "compose-sandbox: no hay montaje, pero tampoco queda la nota que explica por qué "
            "(TOOL-011). Sin ella, el siguiente que necesite el sandbox lo monta y nadie sabe qué se "
            "decidió.",
            file=sys.stderr,
        )
        return 1

    print("compose-sandbox: OK (el compose de referencia no monta /var/run/docker.sock)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
