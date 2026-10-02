"""test_first_use_case_runs_end_to_end — the on-ramp, checked.

WHY A TEST FOR A DOCUMENT. `docs/your-first-use-case.md` tells a newcomer to run three commands and
promises what they will see. A document is the artefact most likely to assert something the code no
longer does — this repository's own README claimed the Deep Research profile "is not implemented yet"
for weeks after it was implemented and running against real inference, and nothing said so because
nothing executed the README.

So the guide's example is executed here, through the real Run Controller and the real Tool Gateway, and
the three things the guide promises are the three things asserted: the run succeeds, a permitted tool
really reads the file, and a forbidden tool is refused BY NAME. If the example, the override, the
bundles or the guide drift apart, this fails.
"""
from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

import pytest

RUNCONTROLLER_ADDR = os.environ.get("AEON_TEST_RUNCONTROLLER_ADDR", "")
TOOLGW_ADDR = os.environ.get("AEON_TEST_TOOLGW_ADDR", "")
AGENT = "first-use-case@0.1.0"
TOKEN = os.environ.get("AEON_TEST_FIRST_USE_CASE_TOKEN", "dev-first-use-case-token-not-a-secret")

EXAMPLE = Path(__file__).resolve().parents[3] / "examples" / "first-use-case"


def _require_stack() -> None:
    missing = [n for n, v in (
        ("AEON_TEST_RUNCONTROLLER_ADDR", RUNCONTROLLER_ADDR),
        ("AEON_TEST_TOOLGW_ADDR", TOOLGW_ADDR),
    ) if not v]
    if missing:
        pytest.skip(
            f"{', '.join(missing)} not set — this needs the stack started with "
            "deploy/compose/first-use-case.override.yml (see make test-first-use-case)"
        )


def _post(url: str, body: dict) -> tuple[int, dict]:
    request = urllib.request.Request(
        url, data=json.dumps(body).encode(), method="POST",
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN},
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            return exc.code, json.loads(raw)
        except json.JSONDecodeError:
            return exc.code, {"error": raw.decode(errors="replace")}


def _get(url: str) -> dict:
    request = urllib.request.Request(url, headers={"Authorization": "Bearer " + TOKEN})
    with urllib.request.urlopen(request, timeout=30) as resp:
        return json.loads(resp.read())


def test_first_use_case_runs_end_to_end():
    _require_stack()
    graph = json.loads((EXAMPLE / "graph.json").read_text())
    run_id = f"first-use-case-test-{uuid.uuid4().hex[:8]}"

    status, body = _post(
        f"http://{RUNCONTROLLER_ADDR}/runs",
        {"run_id": run_id, "agent_manifest_ref": AGENT, "graph": graph},
    )
    assert status == 201, f"POST /runs = {status}: {body}"

    deadline = time.time() + 60
    state: dict = {}
    while time.time() < deadline:
        state = _get(f"http://{RUNCONTROLLER_ADDR}/runs/{run_id}")
        if state.get("status") != "RUNNING":
            break
        time.sleep(1)

    assert state.get("status") == "SUCCEEDED", f"the guide's own example did not succeed: {state}"
    # TWO tool calls, which is the assertion that the graph ran rather than that the run existed: a
    # graph whose children were skipped would also report SUCCEEDED.
    assert state["budgets_consumed"]["tool_calls"] == 2, (
        f"tool_calls = {state['budgets_consumed']['tool_calls']}, want 2 — the graph has two children "
        "and a run that executed neither would still say SUCCEEDED"
    )

    status, allowed = _post(f"http://{TOOLGW_ADDR}/execute", {
        "agent_manifest_ref": AGENT,
        "tool_name": "repository.read",
        "args": {"path": "adr/0001-temporal-determinism-boundary.md", "start_line": 1, "end_line": 1},
    })
    assert status == 200, f"the permitted read was refused: {allowed}"
    assert allowed.get("allowed") is True, allowed
    assert allowed.get("policy_id") == "allow-first-use-case-reads", (
        f"policy_id = {allowed.get('policy_id')!r}. The guide prints this id, and an auditor reads it to "
        "know WHICH rule let a call through"
    )
    # The content, not just the status: a tool that returned its own arguments would satisfy everything
    # above, and that is exactly what TOOL-007 removed from this codebase.
    assert "ADR-001" in allowed.get("result", {}).get("content", ""), allowed

    status, refused = _post(f"http://{TOOLGW_ADDR}/execute", {
        "agent_manifest_ref": AGENT, "tool_name": "shell.exec", "args": {"command": "id"},
    })
    assert refused.get("allowed") is False, f"shell.exec was permitted: {refused}"
    assert refused.get("policy_id") == "forbid-shell-for-first-use-case", (
        f"policy_id = {refused.get('policy_id')!r}, want the forbid rule's id. A refusal by default-deny "
        "and a refusal by an explicit forbid are different facts — one is a configuration gap and the "
        "other is a decision somebody made — and the id is what tells them apart"
    )
