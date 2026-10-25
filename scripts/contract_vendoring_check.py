#!/usr/bin/env python3
"""La copia vendorizada del contrato compartido, comprobada en sus dos mitades (INT-012c).

POR QUÉ EXISTE, y es un defecto nuestro y no higiene: el 2026-10-15 publicamos en el canal que la
carpeta compartida «no es alcanzable desde esta máquina», y **Synaptum cambió su protocolo por eso**
— se comprometieron a publicar versión y huella en cada edición porque supuestamente no podíamos
traer el fichero para comparar. Sí podemos. La ruta la habían declarado ellos el 14 y su `spec.md`
hashea exactamente lo que publicaron. Lo que medimos mal fue **dónde mirar**, que es la misma forma
que el `&&` de TraceQL y el guard que vigilaba 2 de 3 superficies.

Las dos mitades hacen cosas distintas y las dos hacen falta:

  · la HUELLA FIJADA detecta que nuestra copia cambió sin querer, y funciona aunque la carpeta
    compartida no esté montada — que es el caso de CI;
  · la COMPARACIÓN contra el canónico detecta que ELLOS lo editaron, que es lo que ninguna huella
    propia puede ver. Se salta, diciéndolo, cuando la carpeta no está.

Un contrato que otro equipo implementa y que nosotros copiamos tiene dos copias por construcción.
Esto no quita la copia: la hace ruidosa cuando divergen.
"""
from __future__ import annotations

import hashlib
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
# Los dos contratos con copia compartida, y la DIRECCIÓN importa:
#   · normalizacion lo escribe Synaptum — el canónico es la carpeta compartida y nosotros copiamos;
#   · costura-durabilidad lo escribimos nosotros — el canónico es este repositorio y la carpeta
#     compartida es la copia publicada.
# El mismo guard sirve para las dos porque la comprobación es la misma: los bytes de los dos lados
# tienen que coincidir con la huella fijada. Lo que cambia es a quién hay que avisar cuando no.
CONTRACTS = ["normalizacion", "costura-durabilidad"]


def sha256(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def check(contract: str, failures: list[str]) -> tuple[int, str]:
    vendored = ROOT / "evals" / "contracts" / contract
    provenance = vendored / "PROCEDENCIA.json"
    if not provenance.is_file():
        failures.append(f"{contract}: falta {provenance.relative_to(ROOT)}")
        return 0, "sin procedencia"
    record = json.loads(provenance.read_text(encoding="utf-8"))
    files = record.get("files") or {}
    if not files:
        failures.append(f"{contract}: la procedencia no fija ninguna huella, así que no comprueba nada")
        return 0, "sin huellas"

    VENDORED = vendored

    # Mitad 1: la copia vendorizada contra la huella fijada.
    for name, want in files.items():
        local = VENDORED / name
        if not local.is_file():
            failures.append(f"{name}: fijado en la procedencia y ausente de la copia vendorizada")
            continue
        got = sha256(local)
        if got != want:
            failures.append(
                f"{name}: la copia vendorizada hashea {got[:12]} y la procedencia fija {want[:12]}. "
                f"Si el cambio es nuestro, no debería serlo —este fichero es de otro equipo—; si "
                f"viene de re-vendorizar, actualiza PROCEDENCIA.json en el mismo cambio."
            )

    # La huella del spec tiene que seguir siendo la que SYN publicó en el canal: es el único número
    # que los dos lados ven sin copia de por medio.
    published = record.get("spec_sha256_published_by_syn")
    if published and files.get("spec.md") != published:
        failures.append(
            f"spec.md: la procedencia fija {str(files.get('spec.md'))[:12]} y SYN publicó "
            f"{published[:12]} en el canal. Uno de los dos está atrasado y hay que decirlo allí."
        )

    # Mitad 2: el otro lado, cuando se alcanza. Para un contrato que escribimos nosotros, el otro
    # lado es la copia PUBLICADA — y que esté atrasada es nuestro defecto y no el de nadie más.
    other = record.get("canonical_path") or record.get("published_path") or ""
    canonical = pathlib.Path(other).expanduser()
    reachable = canonical.is_dir()
    if reachable:
        for name, want in files.items():
            upstream = canonical / name
            if not upstream.is_file():
                failures.append(f"{name}: ya no existe en el canónico — lo movieron o lo renombraron")
                continue
            got = sha256(upstream)
            if got != want:
                if record.get("direction", "").startswith("we_own_it"):
                    failures.append(
                        f"{contract}/{name}: la copia PUBLICADA hashea {got[:12]} y la nuestra "
                        f"{want[:12]}. Este contrato lo escribimos nosotros, así que la copia que leen "
                        f"los demás está atrasada por nuestra culpa: publícala y di la versión y la "
                        f"huella en el canal."
                    )
                else:
                    failures.append(
                        f"{contract}/{name}: el canónico hashea {got[:12]} y nuestra copia {want[:12]}. "
                        f"Lo editaron: re-vendoriza y corre el corpus, porque un caso nuevo suyo es "
                        f"evidencia que todavía no hemos corrido."
                    )

    where = "el otro lado se alcanza y coincide" if reachable else (
        "el otro lado NO se alcanza desde aquí, así que solo se comprobó la huella fijada")
    return len(files), where


def main() -> int:
    failures: list[str] = []
    lines: list[str] = []
    for contract in CONTRACTS:
        count, where = check(contract, failures)
        lines.append(f"{contract}: {count} fichero(s), {where}")

    if failures:
        print(f"contract-vendoring: {len(failures)} problema(s):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    print("contract-vendoring: OK — " + " · ".join(lines))
    return 0


if __name__ == "__main__":
    sys.exit(main())
