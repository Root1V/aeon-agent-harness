#!/usr/bin/env python3
"""Fail if a published port in deploy/compose is reachable from the network or is not overridable.

TWO RULES, adopted with Veritium on 2026-10-08 and proposed to ARG/PRM/SYN as an ecosystem
convention (`VRT-ARG-001` point 4):

  1. Every published port binds 127.0.0.1, never 0.0.0.0. `SEC-005` already recorded what the
     previous binding meant in practice: whoever could reach 9404 could approve runs. A control
     plane reachable from the network because nobody revisited a default is not a deployment
     decision, it is an accident.
  2. Every published host port is overridable by variable. It is what let this project keep working
     for a week with two foreign OTel collectors holding 24317/24318, and it is what lets another
     team's stack live on the same machine.

WHY A SCRIPT AND NOT A REVIEW HABIT. Both regressions are silent. A new service with
`- "9500:9500"` publishes to every interface and nothing says so — the stack comes up, the tests
pass, and the only symptom is a port somebody else cannot use or a surface somebody else can reach.
The same reasoning as scripts/check_skips.py: this is where a comment stops being a comment and
becomes a build.

PLAIN TEXT AND NOT YAML, deliberately: CI's cheapest job runs with nothing but the system python3
(`no toolchain setup`, by design, so a roadmap row claiming a missing test fails in under a minute),
and importing pyyaml there would mean installing a toolchain to check a line format. What is being
checked IS the line.

Anything this script cannot parse is a FAILURE and never a skip, for the reason the migration runner
gives about stray files: an entry nobody can classify is one nobody is checking.
"""
from __future__ import annotations

import pathlib
import re
import sys

COMPOSE_DIR = pathlib.Path(__file__).resolve().parents[1] / "deploy" / "compose"

# A published-port entry inside a `ports:` block. Long-form syntax (`- target:`/`published:`) would
# not match and is reported as unparseable rather than passed over.
ENTRY = re.compile(r'^\s*-\s*"(?P<spec>[^"]+)"\s*(?:#.*)?$')
PORTS_BLOCK = re.compile(r'^(?P<indent>\s*)ports:\s*(?:#.*)?$')
# 127.0.0.1:<host>:<container>, where <host> must be a ${VAR:-default}.
GOOD = re.compile(r'^127\.0\.0\.1:\$\{[A-Z0-9_]+:-\d+\}:\d+$')


def published_entries(path: pathlib.Path) -> list[tuple[int, str]]:
    """Every line inside a `ports:` block, with its line number."""
    out: list[tuple[int, str]] = []
    lines = path.read_text().splitlines()
    i = 0
    while i < len(lines):
        m = PORTS_BLOCK.match(lines[i])
        if not m:
            i += 1
            continue
        block_indent = len(m.group("indent"))
        i += 1
        while i < len(lines):
            line = lines[i]
            if not line.strip() or line.lstrip().startswith("#"):
                i += 1
                continue
            # The block ends at the first line indented no deeper than `ports:` itself.
            if len(line) - len(line.lstrip()) <= block_indent:
                break
            out.append((i + 1, line))
            i += 1
    return out


def main() -> int:
    if not COMPOSE_DIR.is_dir():
        print(f"compose-ports: {COMPOSE_DIR} does not exist", file=sys.stderr)
        return 1

    files = sorted(COMPOSE_DIR.glob("*.yml")) + sorted(COMPOSE_DIR.glob("*.yaml"))
    if not files:
        print(f"compose-ports: no compose files under {COMPOSE_DIR}", file=sys.stderr)
        return 1

    bad: list[str] = []
    checked = 0
    for path in files:
        for lineno, line in published_entries(path):
            m = ENTRY.match(line)
            where = f"{path.relative_to(COMPOSE_DIR.parents[1])}:{lineno}"
            if not m:
                bad.append(f"  {where}: cannot parse this published-port entry: {line.strip()}")
                continue
            spec = m.group("spec")
            checked += 1
            if GOOD.match(spec):
                continue
            if not spec.startswith("127.0.0.1:"):
                bad.append(
                    f"  {where}: {spec!r} publishes to every interface. Bind it: "
                    f'"127.0.0.1:${{AEON_<NAME>_PORT:-<default>}}:<container>"'
                )
            else:
                bad.append(
                    f"  {where}: {spec!r} is bound to loopback but its host port is fixed. Make it "
                    f"overridable: \"127.0.0.1:${{AEON_<NAME>_PORT:-<default>}}:<container>\""
                )

    if bad:
        print(f"compose-ports: FAILED — {len(bad)} published port(s) break the convention:\n", file=sys.stderr)
        print("\n".join(bad), file=sys.stderr)
        print(
            "\nBoth rules were adopted with Veritium and both regressions are silent: a port on "
            "0.0.0.0 comes up fine and is simply reachable by anyone on the network, and a fixed "
            "port comes up fine until somebody else's stack needs it. See .env.example.",
            file=sys.stderr,
        )
        return 1

    print(f"compose-ports: OK ({checked} published port(s) on loopback and overridable)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
