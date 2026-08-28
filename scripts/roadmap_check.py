#!/usr/bin/env python3
"""
Fails CI if roadmap.md is lying.

Rule (see roadmap.md §6 in the plan / README):
  - A feature row marked DONE must name a test (in its "Criterio de DONE" cell) that actually
    exists somewhere in the repo (python/ tests, go/ tests, or evals/).
  - A feature row marked DONE with no test name at all is also a failure — DONE requires evidence.

This intentionally does NOT try to run the tests (that's `make test`'s job); it only checks that
the claimed evidence exists, so the roadmap can't drift from reality by someone hand-editing a
status cell.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ROADMAP = ROOT / "roadmap.md"

# Matches a markdown table row: | ID | Feature | Estado | Criterio de DONE | PR |
ROW_RE = re.compile(
    r"^\|\s*([\w-]+)\s*\|\s*(.+?)\s*\|\s*`(\w+)`\s*\|\s*(.+?)\s*\|\s*(.*?)\s*\|$"
)

# A test/gate reference inside the "Criterio de DONE" cell, e.g. `test_crash_resume_no_duplicate_write`
TEST_REF_RE = re.compile(r"`([\w./-]+)`")

SEARCH_DIRS = [ROOT / "python", ROOT / "go", ROOT / "evals"]


def find_reference(name: str) -> bool:
    """True if `name` appears anywhere under the search dirs (as a def/func name, filename, or suite id)."""
    needle = name
    for d in SEARCH_DIRS:
        if not d.exists():
            continue
        for path in d.rglob("*"):
            if path.is_dir():
                continue
            if path.name == needle or path.stem == needle:
                return True
            try:
                text = path.read_text(errors="ignore")
            except OSError:
                continue
            if needle in text:
                return True
    return False


def main() -> int:
    if not ROADMAP.exists():
        print("roadmap.md not found", file=sys.stderr)
        return 1

    failures: list[str] = []
    checked = 0

    for line in ROADMAP.read_text().splitlines():
        m = ROW_RE.match(line.strip())
        if not m:
            continue
        feature_id, feature, status, criterio, _pr = m.groups()
        if feature_id in ("ID", "---") or status not in {"DONE"}:
            continue

        checked += 1
        refs = TEST_REF_RE.findall(criterio)
        if not refs:
            failures.append(f"{feature_id} ({feature}): marked DONE but names no test in its DONE criterion")
            continue

        if not any(find_reference(ref) for ref in refs):
            failures.append(
                f"{feature_id} ({feature}): marked DONE, references {refs!r}, "
                f"but none of those were found under python/, go/, or evals/"
            )

    if failures:
        print(f"roadmap-check: {len(failures)} problem(s) found among {checked} DONE row(s):\n")
        for f in failures:
            print(f"  - {f}")
        return 1

    print(f"roadmap-check: OK ({checked} DONE row(s) verified, 0 currently DONE is a valid state)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
