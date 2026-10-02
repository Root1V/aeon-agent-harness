#!/usr/bin/env python3
"""Fail a CI run whose tests skipped because nothing was configured.

WHY THIS EXISTS. Every integration test in this repo self-skips when its infrastructure is absent —
`AEON_TEST_PG_DSN not set`, `AEON_TEST_TEMPORAL_ADDRESS not set`, and so on. That is the right
behaviour for a developer running the suite on a laptop, and it is the single most likely way for a CI
job to be green over nothing: forget one environment variable in the workflow and the job passes,
having asserted almost nothing, forever. The repo already wrote the lesson down about a different
target — "a self-skipping test in the only target that would run it is a test nobody runs" — and CI is
where that stops being a comment and becomes a build.

WHAT IS TOLERATED, and the list is deliberately short. An external service that refuses — a search
engine rate-limiting the SearXNG instance — is a condition of the world, not of the deployment, and a
build that goes red for it teaches people to ignore the build. Everything else fails, including a skip
with no stated reason: a skip whose cause cannot be read is exactly the kind this script exists to
catch.

Reads both formats: `pytest -rs` and `go test -v`. Either way the run has to be verbose enough to
print the reasons, or there is nothing to judge.

Usage: check_skips.py <test-output-file>
"""
from __future__ import annotations

import re
import sys

# pytest -rs prints: SKIPPED [1] tests/integration/x.py:12: the reason
SKIP_LINE = re.compile(r"^SKIPPED\s+\[\d+\]\s+(?P<where>[^:]+:\d+):\s*(?P<reason>.*)$")

# `go test -v` prints the reason BEFORE the verdict, which is why Go needs two patterns and a lookback:
#     === RUN   TestX
#         foo_test.go:12: the reason
#     --- SKIP: TestX (0.00s)
GO_SKIP_VERDICT = re.compile(r"^--- SKIP: (?P<name>\S+)")
GO_REASON = re.compile(r"^(?P<where>[a-z0-9_]+_test\.go:\d+):\s*(?P<reason>.*)$")

# Substrings of a reason that a CI run may legitimately report. Each one is a condition of the world
# that no amount of configuration fixes, and each is here with its own justification.
TOLERATED = (
    # The public engines behind SearXNG rate-limit and serve CAPTCHAs. Only the Go searcher's own test
    # reports this now: TOOL-008 gave the deployment an offline tool (repository.read), so the
    # end-to-end tool test no longer depends on the public web at all — which is what this tolerance
    # used to be covering for.
    "every upstream search engine refused",
    # A generator, not a test: it REWRITES the shared golden corpus and is guarded by an env var so a
    # normal run cannot overwrite the artefact three teams verify against.
    "set AEON_WRITE_STEP_IDENTITY_CORPUS=1",
    # These reconcile against the real Prometheus platform with real credentials and real money. They
    # must NOT run in CI: the secret does not belong in a runner, and `make test-mdl-015` is the target
    # that runs them deliberately, by a person, on a machine that has the credentials.
    "Prometheus credentials not set",
    # The same argument, different wording, and worth listing separately rather than loosening the
    # pattern above: TOOL-006's retrieval half needs the platform's embedding credentials. The first
    # version of this list had three variants of this message and missed the fourth, which is the kind
    # of near-miss a broad pattern hides.
    "Prometheus embedding credentials not set",
    # The golden corpus lives OUTSIDE this repository, in the three teams' coordination folder — a
    # decision recorded in roadmap.md ("Extracción del repo de contratos", DEFERRED). So the acceptance
    # test of the contract we share with Synaptum and Axonium runs on exactly one laptop. Tolerated
    # because no workflow change fixes it; recorded in backlog.md because it should not stay that way.
    "golden corpus not readable",
    # The RFC 8785 vectors come from the jcs module's own testdata, which is only present when the
    # module cache has it. Nothing in a workflow puts it there.
    "RFC testdata is not on this machine",
    # The streaming half of the seam measurement needs a real aeon-modelgw wired to a real provider —
    # which means provider credentials, same argument as the Prometheus ones above.
    "AEON_TEST_MODELGW_ADDR not set",
)


def main(path: str) -> int:
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            lines = fh.read().splitlines()
    except OSError as exc:
        print(f"check_skips: cannot read {path}: {exc}", file=sys.stderr)
        return 2

    skips = []
    last_go_reason: tuple[str, str] | None = None
    for line in lines:
        stripped = line.strip()
        m = SKIP_LINE.match(stripped)
        if m:
            skips.append((m.group("where"), m.group("reason").strip()))
            continue
        if GO_SKIP_VERDICT.match(stripped):
            # A Go skip with no preceding reason line is reported as one with no reason, which fails:
            # t.Skip() with no message is the least readable way to not run a test.
            where, reason = last_go_reason or ("<go test>", "")
            skips.append((where, reason))
            last_go_reason = None
            continue
        if gm := GO_REASON.match(stripped):
            last_go_reason = (gm.group("where"), gm.group("reason").strip())

    # CAN THIS SCRIPT EVEN SEE A SKIP? `go test` without -v prints no SKIP lines at all, so a
    # non-verbose log produces "no tests skipped" — a reassuring answer from a script that was unable to
    # look. Found by running it against `make test-go`, which is not verbose: it reported zero skips for
    # a suite that has nine. Refusing to answer is the only honest option.
    looks_like_go = any(line.startswith("ok  \t") or line.startswith("--- FAIL") for line in lines)
    went_verbose = any(line.startswith("=== RUN") for line in lines)
    if looks_like_go and not went_verbose:
        print(
            "check_skips: FAILED — this looks like `go test` output without -v, so SKIP lines are not in "
            "it and this script cannot tell whether anything skipped. Run the suite with -v, or do not "
            "run this check on that log and say why.",
            file=sys.stderr,
        )
        return 1

    if not skips:
        print("check_skips: no tests skipped")
        return 0

    # Printed whether or not they fail the build. A tolerated skip that nobody ever sees becomes a
    # permanent hole: the point is that someone reads "the searcher was broken again" and eventually
    # gets tired of it.
    print(f"check_skips: {len(skips)} skip(s) reported")
    bad = []
    for where, reason in skips:
        tolerated = any(t in reason for t in TOLERATED)
        print(f"  [{'tolerated' if tolerated else 'NOT TOLERATED'}] {where}: {reason or '<no reason given>'}")
        if not tolerated:
            bad.append((where, reason))

    if bad:
        print(
            "\ncheck_skips: FAILED — the skips above are not conditions of the world, they are tests "
            "that did not run.\nAn integration test skips when its infrastructure is missing, so a job "
            "that reports one is a job asserting less than it looks like it does. Either configure what "
            "it needs in the workflow, or add the reason to TOLERATED in this file with an argument for "
            "why no configuration can fix it.",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        raise SystemExit(2)
    raise SystemExit(main(sys.argv[1]))
