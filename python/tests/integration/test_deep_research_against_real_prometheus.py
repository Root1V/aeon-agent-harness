"""MDL-015's acceptance test: the whole Deep Research pipeline against the REAL platform.

Gap 3 of the three F4.6 found: `DX-001` proves the pipeline orchestrates (Planner -> Researchers ->
Sufficiency Gate -> Reporter -> Citation Verifier) but every model answer comes from a local HTTP
double that reads the system prompt and returns well-formed JSON by construction. `MDL-006` proves
the adapter talks to Prometheus, with one model and no graph. Nothing put the two together, so
nothing ever asked whether a real model — which can answer in prose, run out of budget mid-thought,
or spend its whole allowance reasoning — survives a pipeline that validates every step against a
JSON Schema.

What runs for real here: a real Temporal server, a real separate worker process, a real
`aeon-modelgw` reached over HTTP, and real inference on the real Prometheus deployment. Nothing is
faked. The run costs real money (fractions of a cent) and takes real time.

Finding this test produced before it could run at all, and the reason it is worth having: the
gateway's OAuth scope was a hand-written string naming ONE model while routing picks the model per
request, so every other candidate in the bundle answered
`403 "This client is not authorized to use model '<id>'"`. The declared fallback chain was unusable
in production. See go/cmd/aeon-modelgw's prometheusScope.
"""
from __future__ import annotations

import os
import subprocess
import sys
import time
import uuid
from pathlib import Path

import pytest
from temporalio.client import Client
from temporalio.testing import WorkflowEnvironment

from aeon_sdk.deep_research import start_deep_research_run

REPO_PYTHON_DIR = Path(__file__).resolve().parents[2]

# The model is configurable because which models a client is authorized for is a property of the
# deployment, not of this test. The default is the one the reference bundle puts at priority 0.
# Both candidates the reference bundle's reasoning-local profile declares, at the priorities it
# declares them. Passing the CHAIN and not one model is the point: the platform intermittently
# answers 500 "The model produced output that does not match the expected peg-native format" —
# its own parser failing on its own model's output — and a chain is what makes that survivable.
#
# The first version of this test passed a single candidate, so a 500 had nowhere to fall back to and
# failed the whole run. That was the test's construction, not a product defect; but it also meant the
# test would not have noticed if the chain were broken, which it was until MDL-015 derived the OAuth
# scope from the bundle (a token scoped to one model 403s on every other candidate).
DEFAULT_CANDIDATES = [
    {"provider": "prometheus_inference", "model": "gpt-oss-20b-mxfp4", "priority": 0},
    {"provider": "prometheus_inference", "model": "qwen3-0.6b", "priority": 1},
]
DEFAULT_MODEL = "gpt-oss-20b-mxfp4"


def _spawn_worker(address: str, task_queue: str, modelgw_addr: str) -> subprocess.Popen:
    env = os.environ.copy()
    env["AEON_TEMPORAL_ADDRESS"] = address
    env["AEON_TASK_QUEUE"] = task_queue
    env["AEON_MODELGW_ADDR"] = modelgw_addr
    env["PYTHONPATH"] = str(REPO_PYTHON_DIR) + os.pathsep + env.get("PYTHONPATH", "")
    # The worker must reach the REAL Tool Gateway, and this line is the difference between a run that
    # researches and one that only looks like it.
    #
    # The first version set AEON_TOOL_EXECUTION_MODE=local-ledger, which executes NOTHING. The run
    # then completed with cited_claim_ids=[], sufficient=False and all four topics queued for
    # replanning — the pipeline being honest about having retrieved nothing, which is DR-003's
    # Sufficiency Gate doing its job. But it made the test prove the opposite of MDL-015: a full
    # pipeline that never searched. With the gateway configured, search.web really searches (TOOL-007)
    # and the Researchers have something to extract claims from.
    env["AEON_TOOLGW_ADDR"] = os.environ.get("AEON_TEST_TOOLGW_ADDR", "127.0.0.1:9403")
    env.pop("AEON_TOOL_EXECUTION_MODE", None)  # both set is a contradiction execute_tool refuses
    return subprocess.Popen(
        [sys.executable, "-m", "aeon_worker"],
        cwd=str(REPO_PYTHON_DIR),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )


def _cause_chain(exc: BaseException) -> str:
    """Every message in the exception chain, outermost first. Temporal nests the real reason several
    levels down inside WorkflowFailureError, whose own message is always the same sentence."""
    parts: list[str] = []
    seen: set[int] = set()
    current: BaseException | None = exc
    while current is not None and id(current) not in seen:
        seen.add(id(current))
        parts.append(f"{type(current).__name__}: {current}")
        current = getattr(current, "cause", None) or current.__cause__
    return "\n  -> ".join(parts)


def _drain(proc: subprocess.Popen) -> str:
    """The worker's output so far, without blocking on a process that is still running."""
    proc.terminate()
    try:
        out, _ = proc.communicate(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        out, _ = proc.communicate()
    return (out or b"").decode(errors="replace")[-4000:]


def _assert_still_running(proc: subprocess.Popen) -> None:
    if proc.poll() is not None:
        output = proc.stdout.read().decode(errors="replace") if proc.stdout else ""
        raise RuntimeError(f"worker exited early (code {proc.returncode}):\n{output}")


@pytest.mark.asyncio
async def test_deep_research_against_real_prometheus():
    modelgw_addr = os.environ.get("AEON_TEST_MODELGW_ADDR")
    if not modelgw_addr:
        pytest.skip(
            "AEON_TEST_MODELGW_ADDR not set — MDL-015 runs against a real aeon-modelgw wired to the "
            "real platform, or not at all (see make test-mdl-015)"
        )
    model = os.environ.get("AEON_TEST_PROMETHEUS_MODEL", DEFAULT_MODEL)
    candidates = (
        [{"provider": "prometheus_inference", "model": model, "priority": 0}]
        if model != DEFAULT_MODEL
        else DEFAULT_CANDIDATES
    )

    task_queue = f"aeon-mdl015-{uuid.uuid4().hex[:8]}"
    async with await WorkflowEnvironment.start_local() as env:
        address = env.client.service_client.config.target_host
        worker = _spawn_worker(address, task_queue, modelgw_addr)
        try:
            time.sleep(0.5)
            _assert_still_running(worker)
            client: Client = env.client

            try:
                report = await start_deep_research_run(
                    "what is the state of the art in agent harnesses?",
                    candidates=candidates,
                    model=model,
                    temporal_address=address,
                    task_queue=task_queue,
                    # The principal examples/deep-research/policy_bundle.yaml permits. Without it the
                    # Tool Gateway refuses every tool call rather than running it unattributed
                    # (TOOL-004), so the run would complete having searched nothing.
                    agent_manifest_ref="deep-research-general@0.1.0",
                    # examples/deep-research/agent.yaml's own tools.allow list. Passed rather than
                    # hardcoded in the prompt so the tools the Researcher is TOLD about are exactly
                    # the ones the Tool Gateway permits — naming one it forbids invites a call that
                    # will be denied (MDL-015).
                    allowed_tools=["search.web", "search.rag", "repository.read", "artifact.read"],
                )
            except Exception as exc:
                # A real model can fail this pipeline in ways a double never does — prose instead of
                # JSON, a plan with the wrong number of subtasks, an allowance spent entirely on
                # reasoning. Temporal wraps all of it as "Workflow execution failed", which says
                # nothing, so the cause chain and the worker's own log are printed here. A test that
                # fails without naming the cause turns a finding into a mystery.
                raise AssertionError(
                    f"the pipeline failed against the real platform.\n"
                    f"cause chain: {_cause_chain(exc)}\n"
                    f"--- worker output ---\n{_drain(worker)}"
                ) from exc

            # The workflow really completed, durably. Checked first: every assertion below is about a
            # run, and a run that did not finish makes them all meaningless.
            handle = client.get_workflow_handle(report.run_id)
            describe = await handle.describe()
            assert describe.status.name == "COMPLETED", f"workflow status={describe.status.name}"

            # The report came out of a real model, so its TEXT is not assertable — that would be
            # asserting a model's behaviour, which is the mistake Synaptum reported making in two of
            # its own examples. What IS assertable is the pipeline's contract over whatever the model
            # said: a plan was parsed, subtasks ran, claims were extracted, and every claim the
            # Reporter cited exists in the ledger.
            # The worker's own log is the only place that says what each Researcher decided, and an
            # assertion failure needs it as much as an exception does. Draining it here rather than
            # only on the exception path is the difference between a finding and a guess.
            worker_log = _drain(worker) if not report.cited_claim_ids else ""
            assert report.cited_claim_ids, (
                f"--- worker output ---\n{worker_log}\n"
                "the run produced no cited claims. Note what this does NOT mean: the pipeline is "
                "honest about it — DR-003's Sufficiency Gate returns sufficient=False and queues "
                "every topic for replanning, so nothing here claims success. What it means is that "
                "the Researchers gathered no evidence, and a Deep Research run that cites nothing "
                "is the outcome this whole phase exists to make impossible.\n"
                f"sufficient={report.sufficient} topics_to_replan={report.topics_to_replan}"
            )
            assert report.report_text.strip(), "the report is empty"
            assert report.topics_to_replan == [] or report.sufficient is False, (
                "topics_to_replan is non-empty but the run claims to be sufficient — those two "
                f"cannot both hold: sufficient={report.sufficient} topics={report.topics_to_replan}"
            )

            print(
                f"\nMDL-015: model={model} claims={len(report.cited_claim_ids)} "
                f"sufficient={report.sufficient} report_chars={len(report.report_text)}"
            )
        finally:
            worker.terminate()
            try:
                worker.wait(timeout=5)
            except subprocess.TimeoutExpired:
                worker.kill()
