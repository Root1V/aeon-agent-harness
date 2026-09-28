"""Deterministic replay of a recorded run (DX-002's `--assert-identical`).

WHAT THIS PROVES, and it is the point of ADR-001. The workflow is the only deterministic half of the
system: it decides, and Activities do everything non-deterministic. Replay is the only way to find out
whether that boundary actually holds, because a violation does not fail when the code is written — it
fails later, on a resumed run, in production, when the history no longer matches what the code now does.
It is risk #1 of the plan for exactly that reason.

WHY IT LIVES IN PYTHON. The replayer must be registered with the workflow DEFINITIONS, and those are
here. The Go CLI can fetch a history and it cannot replay one: a Go replayer has no AgentRunWorkflow to
run it against. So the CLI delegates, and this module is what it delegates to.

WHAT "IDENTICAL" MEANS, stated precisely rather than generously. Temporal's Replayer feeds the recorded
history back through the workflow code and compares the COMMANDS the code produces against the ones the
history records — every Activity scheduled, every timer, every signal handled, and the final completion
with its result. A replay that finishes without error is the assertion that the code would make the same
decisions and reach the same result. It is not a re-execution: no Activity runs again, nothing is called
twice, and the model is never asked anything.
"""
from __future__ import annotations

import argparse
import asyncio
import dataclasses
import logging
import os
import sys

from temporalio.client import Client
from temporalio.worker import Replayer

from aeon_worker.registry import WORKFLOWS

logger = logging.getLogger("aeon_worker.replay")


@dataclasses.dataclass
class ReplayVerdict:
    """The outcome of replaying one run.

    `identical` and `reason` rather than a bare bool: a failed replay is only useful with the reason,
    which names the exact point where the code and the history stopped agreeing.
    """

    run_id: str
    identical: bool
    events: int
    reason: str = ""

    def as_dict(self) -> dict:
        return {"run_id": self.run_id, "identical": self.identical, "events": self.events, "reason": self.reason}


class EmptyHistory(Exception):
    """Raised when a run has no history to replay.

    ITS OWN ERROR, not a false verdict. A run_id that never existed and a run whose code diverged are
    different facts, and reporting the first as `identical: false` would send someone hunting for a
    determinism bug in a typo — while reporting it as `identical: true` would pass a run that was never
    checked, which is worse.
    """


async def replay_run(client: Client, run_id: str) -> ReplayVerdict:
    """Fetch a run's real history and replay it against the registered workflows."""
    handle = client.get_workflow_handle(run_id)
    history = await handle.fetch_history()

    events = len(history.events)
    if events == 0:
        raise EmptyHistory(f"run {run_id} has no history events")

    replayer = Replayer(workflows=WORKFLOWS)
    try:
        await replayer.replay_workflow(history)
    except Exception as exc:  # noqa: BLE001 - the replayer surfaces several unrelated failure types
        # Every failure is reported with its message rather than classified here. Temporal raises
        # different types for a non-determinism mismatch, an unknown workflow type and a workflow that
        # failed during replay, and deciding which is which from the outside is how a real determinism
        # bug gets filed as "stale registry".
        return ReplayVerdict(run_id=run_id, identical=False, events=events, reason=f"{type(exc).__name__}: {exc}")
    return ReplayVerdict(run_id=run_id, identical=True, events=events)


async def _main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="python -m aeon_worker.replay")
    parser.add_argument("run_id")
    parser.add_argument("--address", default=os.environ.get("AEON_TEMPORAL_ADDRESS", "localhost:7233"))
    parser.add_argument("--namespace", default=os.environ.get("AEON_TEMPORAL_NAMESPACE", "default"))
    args = parser.parse_args(argv)

    client = await Client.connect(args.address, namespace=args.namespace)
    try:
        verdict = await replay_run(client, args.run_id)
    except EmptyHistory as exc:
        # Exit 2, distinct from the 1 a divergence gets: "nothing to check" must not look like "checked
        # and it failed" to a script, which is the only consumer that reads an exit code.
        print(f"replay: {exc}", file=sys.stderr)
        return 2

    if verdict.identical:
        print(f"replay: run {verdict.run_id} is deterministic — {verdict.events} events replayed, identical")
        return 0
    print(f"replay: run {verdict.run_id} DIVERGED after {verdict.events} events\n{verdict.reason}", file=sys.stderr)
    return 1


def main() -> None:
    logging.basicConfig(level=logging.WARNING)
    raise SystemExit(asyncio.run(_main(sys.argv[1:])))


if __name__ == "__main__":
    main()
