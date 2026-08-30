"""`python -m aeon_evalops.cli run <suite> [--trials N]` — a stopgap entrypoint for EVAL-002 until
the Go `aeon` CLI talks to a real eval-running service instead of shelling out to this. See
go/cmd/aeon/main.go's runEvalRun, which invokes exactly this module (via `make eval-run`, which
runs it inside the same Python container as `make test-python`).
"""
from __future__ import annotations

import argparse
import asyncio
import sys

from aeon_evalops.runner import format_report, run_suite


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="aeon_evalops")
    subparsers = parser.add_subparsers(dest="command", required=True)
    run_parser = subparsers.add_parser("run")
    run_parser.add_argument("suite_name")
    run_parser.add_argument("--trials", type=int, default=1)

    args = parser.parse_args(argv)
    report = asyncio.run(run_suite(args.suite_name, trials=args.trials))
    print(format_report(report))
    return 0 if report.passed else 1


if __name__ == "__main__":
    sys.exit(main())
