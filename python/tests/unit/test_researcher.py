"""DR-002's acceptance test: Isolated Researchers run a bounded ReAct loop per subtask, with no
cross-contamination between subtasks even when run concurrently (test_researcher_isolation). Uses
fake decide/execute_tool/recall — this module has no Temporal import and no real Model/Tool
Gateway call, by design (see researcher.py's docstring).
"""
from __future__ import annotations

import json

import pytest

from aeon_profiles.deep_research.planner import ResearchPlan, Subtask
from aeon_profiles.deep_research.researcher import (
    Researcher,
    ResearcherError,
    run_researchers_in_parallel,
)


def _subtask(id_: str, topic: str, max_tool_calls: int = 5, max_model_calls: int = 5) -> Subtask:
    return Subtask(
        id=id_,
        description=f"Investigate {topic}",
        coverage_topic=topic,
        max_tool_calls=max_tool_calls,
        max_model_calls=max_model_calls,
    )


def _decision_response(decision: dict) -> dict:
    return {
        "model": "test-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": json.dumps(decision)}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


async def test_researcher_isolation():
    """Two subtasks run concurrently; neither Researcher's transcript or tool-call history ever
    mentions the other's topic, and each finishes with its own subtask's summary — proving state is
    per-Researcher, not shared (e.g. via an accidental class-level mutable default)."""

    async def fake_decide(rendered_context: dict) -> dict:
        # First call for a subtask: request a tool call naming its own topic. Second call: finish,
        # echoing back the subtask's own description in the summary.
        if len(rendered_context["messages"]) <= 2:
            topic = rendered_context["messages"][1]["content"]
            return _decision_response({"action": "CALL_TOOL", "tool_name": "search.web", "args": {"query": topic}})
        return _decision_response({"action": "FINISH", "message": f"done: {rendered_context['messages'][1]['content']}"})

    async def fake_execute_tool(tool_name: str, args: dict) -> dict:
        return {"status": "ok", "tool_name": tool_name, "echo": args}

    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A"), _subtask("st-B", "topic-B")])
    results = await run_researchers_in_parallel(plan, model="test-model", decide=fake_decide, execute_tool=fake_execute_tool)

    by_id = {r.subtask_id: r for r in results}
    assert set(by_id) == {"st-A", "st-B"}

    assert by_id["st-A"].final_message == "done: Investigate topic-A"
    assert by_id["st-B"].final_message == "done: Investigate topic-B"

    transcript_a = json.dumps([m["content"] for m in by_id["st-A"].messages])
    transcript_b = json.dumps([m["content"] for m in by_id["st-B"].messages])
    assert "topic-B" not in transcript_a
    assert "topic-A" not in transcript_b

    assert len(by_id["st-A"].tool_calls) == 1
    assert len(by_id["st-B"].tool_calls) == 1
    assert by_id["st-A"].tool_calls[0].args == {"query": "Investigate topic-A"}
    assert by_id["st-B"].tool_calls[0].args == {"query": "Investigate topic-B"}


async def test_researcher_stops_at_model_call_budget():
    async def always_call_tool(_rendered_context: dict) -> dict:
        return _decision_response({"action": "CALL_TOOL", "tool_name": "search.web", "args": {}})

    async def fake_execute_tool(_tool_name: str, _args: dict) -> dict:
        return {"status": "ok"}

    subtask = _subtask("st-1", "topic", max_tool_calls=100, max_model_calls=3)
    result = await Researcher(subtask, "test-model").research(always_call_tool, fake_execute_tool)

    assert result.finished_reason == "budget_exhausted"
    assert len(result.tool_calls) == 3


async def test_researcher_stops_at_tool_call_budget_even_with_model_calls_to_spare():
    async def always_call_tool(rendered_context: dict) -> dict:
        return _decision_response({"action": "CALL_TOOL", "tool_name": "search.web", "args": {}})

    async def fake_execute_tool(tool_name: str, args: dict) -> dict:
        return {"status": "ok"}

    subtask = _subtask("st-1", "topic", max_tool_calls=2, max_model_calls=100)
    result = await Researcher(subtask, "test-model").research(always_call_tool, fake_execute_tool)

    assert result.finished_reason == "budget_exhausted"
    assert len(result.tool_calls) == 2


async def test_researcher_request_replan_stops_the_loop_without_exhausting_budget():
    async def fake_decide(rendered_context: dict) -> dict:
        return _decision_response({"action": "REQUEST_REPLAN", "message": "coverage gap found"})

    async def fake_execute_tool(tool_name: str, args: dict) -> dict:
        raise AssertionError("no tool should be called on REQUEST_REPLAN")

    subtask = _subtask("st-1", "topic")
    result = await Researcher(subtask, "test-model").research(fake_decide, fake_execute_tool)

    assert result.finished_reason == "replan_requested"
    assert result.final_message == "coverage gap found"


async def test_researcher_emit_message_continues_the_loop():
    calls = []

    async def fake_decide(rendered_context: dict) -> dict:
        calls.append(len(rendered_context["messages"]))
        if len(calls) == 1:
            return _decision_response({"action": "EMIT_MESSAGE", "message": "thinking out loud"})
        return _decision_response({"action": "FINISH", "message": "done"})

    async def fake_execute_tool(tool_name: str, args: dict) -> dict:
        raise AssertionError("no tool should be called")

    subtask = _subtask("st-1", "topic")
    result = await Researcher(subtask, "test-model").research(fake_decide, fake_execute_tool)

    assert len(calls) == 2
    assert result.finished_reason == "finish"


async def test_researcher_raises_on_recall_observation_without_a_recall_callable():
    async def fake_decide(rendered_context: dict) -> dict:
        return _decision_response({"action": "RECALL_OBSERVATION", "recall_id": "obs-123"})

    async def fake_execute_tool(tool_name: str, args: dict) -> dict:
        raise AssertionError("no tool should be called")

    subtask = _subtask("st-1", "topic")
    with pytest.raises(ResearcherError):
        await Researcher(subtask, "test-model").research(fake_decide, fake_execute_tool)


async def test_researcher_recall_observation_uses_the_configured_recall_callable():
    async def fake_decide(rendered_context: dict) -> dict:
        if len(rendered_context["messages"]) <= 2:
            return _decision_response({"action": "RECALL_OBSERVATION", "recall_id": "obs-123"})
        return _decision_response({"action": "FINISH", "message": "done"})

    async def fake_execute_tool(tool_name: str, args: dict) -> dict:
        raise AssertionError("no tool should be called")

    seen_recall_ids = []

    async def fake_recall(recall_id: str) -> str:
        seen_recall_ids.append(recall_id)
        return "recalled content"

    subtask = _subtask("st-1", "topic")
    result = await Researcher(subtask, "test-model").research(fake_decide, fake_execute_tool, recall=fake_recall)

    assert seen_recall_ids == ["obs-123"]
    assert result.finished_reason == "finish"


def _decision_response_with_reasoning(decision: dict, reasoning: str) -> dict:
    """A reasoning model's answer: the decision in `content` AND its analysis channel alongside."""
    return {
        "model": "test-model",
        "choices": [
            {
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": json.dumps(decision),
                    "reasoning_content": reasoning,
                },
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


async def test_the_models_reasoning_survives_into_the_next_turn():
    """PRM-VRT-003: the harness must return a reasoning model's own analysis channel to it.

    THE INSTRUMENT THAT DID NOT EXIST IS THE POINT. Prometheus found their gateway deleting
    `reasoning_content` from every message of every client, and Synaptum found the same on their
    side with the loss written down as deliberate. In all three places the field arrived, was
    stored, and never went back — and nothing failed, because a model that loses its analysis
    channel answers worse rather than erroring. Every test here asserted what the loop DID with a
    decision; none asserted what it sent BACK.

    Upstream measured the cost: llama.cpp#27720, closed as not fixable in the parser, 7 failed turns
    of 120 with the reasoning withheld versus 0 of 120 returning it, and the maintainer's note that
    omitting it on assistant messages carrying tool calls "produces erratic behaviour".
    """
    sent: list[list[dict]] = []

    async def decide(payload: dict) -> dict:
        sent.append([dict(m) for m in payload["messages"]])
        if len(sent) == 1:
            return _decision_response_with_reasoning(
                {"action": "CALL_TOOL", "tool_name": "search.web", "args": {"query": "q"}},
                reasoning="I should search before answering.",
            )
        return _decision_response_with_reasoning(
            {"action": "FINISH", "message": "done"}, reasoning="I have enough now."
        )

    async def execute_tool(name: str, args: dict) -> dict:
        return {"ok": True}

    result = await Researcher(_subtask("s1", "topic"), "test-model").research(decide, execute_tool)
    assert result.finished_reason == "finish"
    assert len(sent) == 2, "the loop did not take a second turn, so this measures nothing"

    second_turn = sent[1]
    assistant_turns = [m for m in second_turn if m.get("role") == "assistant"]
    assert assistant_turns, "no assistant turn was replayed at all"

    # The assertion is on what went BACK ON THE WIRE, not on what the loop stored.
    assert assistant_turns[0].get("reasoning_content") == "I should search before answering.", (
        "the assistant turn was replayed without its reasoning_content: the model is asked to "
        "continue from its own turn with its analysis channel removed"
    )
    # And `content` is still there: the reasoning is carried ALONGSIDE the answer, never instead of
    # it. A turn that only reasons has no valid wire form at all (measured against the real platform
    # in MDL-022: reasoning alone comes back 400).
    assert assistant_turns[0].get("content"), "the decision itself was lost while adding the reasoning"


async def test_a_turn_with_no_reasoning_gains_no_empty_field():
    """Absent and empty are different claims. A model that reported no reasoning must not get a
    `reasoning_content: ""` invented for it — the same absent-not-zero rule OBS-013 applied to the
    usage counters, and the reason the key is omitted rather than defaulted."""
    sent: list[list[dict]] = []

    async def decide(payload: dict) -> dict:
        sent.append([dict(m) for m in payload["messages"]])
        if len(sent) == 1:
            return _decision_response({"action": "EMIT_MESSAGE", "message": "thinking out loud"})
        return _decision_response({"action": "FINISH", "message": "done"})

    async def execute_tool(name: str, args: dict) -> dict:
        return {}

    await Researcher(_subtask("s1", "topic"), "test-model").research(decide, execute_tool)
    assert len(sent) == 2
    for message in sent[1]:
        if message.get("role") == "assistant":
            assert "reasoning_content" not in message, (
                f"an empty reasoning_content was invented for a model that reported none: {message}"
            )
