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
# LOOKS_LIKE_ROW_RE is deliberately lax: it matches the OPENING of a feature row so that a row
# ROW_RE cannot parse is reported instead of skipped. See the failure it exists for in main().
LOOKS_LIKE_ROW_RE = re.compile(r"^\|\s*[A-Z][A-Z0-9]*-\d+\s*\|")

ROW_RE = re.compile(
    r"^\|\s*([\w-]+)\s*\|\s*(.+?)\s*\|\s*`(\w+)`\s*\|\s*(.+?)\s*\|\s*(.*?)\s*\|$"
)

# A test/gate reference inside the "Criterio de DONE" cell, e.g. `test_crash_resume_no_duplicate_write`
TEST_REF_RE = re.compile(r"`([\w./-]+)`")

SEARCH_DIRS = [ROOT / "python", ROOT / "go", ROOT / "evals"]


# Directories that are not our source. Excluding them is not an optimisation, or not only one:
# python/.venv alone is ~979MB across ~15k vendored files, and a test name found inside a third-party
# package would satisfy this check for a DONE row whose test does not exist. A guard that can pass
# for the wrong reason is worse than no guard, so the walk is restricted to code we wrote.
EXCLUDED_DIRS = {".venv", "__pycache__", "node_modules", ".git", ".pytest_cache", ".mypy_cache", "vendor"}


def _source_files() -> list:
    """Every file under the search dirs, walked ONCE.

    find_reference used to re-walk and re-read all of them per DONE row — ~70 full passes over a
    gigabyte, which is why verifying the roadmap took minutes instead of a second.
    """
    files = []
    for d in SEARCH_DIRS:
        if not d.exists():
            continue
        for path in d.rglob("*"):
            if path.is_dir():
                continue
            if EXCLUDED_DIRS & set(path.parts):
                continue
            files.append(path)
    return files


_CORPUS: list | None = None


def find_reference(name: str) -> bool:
    """True if `name` appears anywhere under the search dirs (as a def/func name, filename, or suite id)."""
    global _CORPUS
    if _CORPUS is None:
        _CORPUS = []
        for path in _source_files():
            try:
                _CORPUS.append((path.name, path.stem, path.read_text(errors="ignore")))
            except OSError:
                continue
    return any(name == fname or name == stem or name in text for fname, stem, text in _CORPUS)


def main() -> int:
    if not ROADMAP.exists():
        print("roadmap.md not found", file=sys.stderr)
        return 1

    failures: list[str] = []
    checked = 0

    for line in ROADMAP.read_text().splitlines():
        stripped = line.strip()
        m = ROW_RE.match(stripped)
        if not m:
            # A line that OPENS like a feature row but does not parse is a failure, not something to
            # skip. Skipping is how OBS-009 stayed invisible: its criterion had a nested markdown
            # table embedded in it, which split the row across six physical lines, so ROW_RE never
            # matched it and the check reported OK over a DONE row it had never looked at. A guard
            # that silently ignores what it cannot read is a guard that passes for the wrong reason.
            if LOOKS_LIKE_ROW_RE.match(stripped):
                failures.append(
                    f"{stripped[:60]!r}...: opens like a feature row but does not parse — "
                    "most likely a newline inside its DONE criterion (keep each row on one line)"
                )
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
