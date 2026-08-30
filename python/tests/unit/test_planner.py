"""DR-001's acceptance test: the Research Planner enforces a 3-5 subtask plan, with coverage and
budgets, before anything downstream (DR-002's Researchers) can see it. Uses a fake `decide` — this
module has no Temporal import and no real Model Gateway call, by design (see planner.py's
docstring); the real decide_activity HTTP path is exercised separately, not by this unit test.
"""
from __future__ import annotations

import json

import pytest

from aeon_profiles.deep_research.planner import (
    MAX_SUBTASKS,
    MIN_SUBTASKS,
    Planner,
    PlannerError,
    build_planner_request,
    parse_plan,
)


def _normalized_response(content: dict | str) -> dict:
    text = content if isinstance(content, str) else json.dumps(content)
    return {
        "model": "test-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


def _subtask(i: int, topic: str) -> dict:
    return {
        "id": f"st-{i}",
        "description": f"Investigate {topic}",
        "coverage_topic": topic,
        "budget": {"max_tool_calls": 5, "max_model_calls": 3},
    }


def _plan_doc(n: int, query: str = "state of the art in agent harnesses 2026") -> dict:
    return {"query": query, "subtasks": [_subtask(i, f"topic-{i}") for i in range(n)]}


@pytest.mark.parametrize("n", [MIN_SUBTASKS, MIN_SUBTASKS + 1, MAX_SUBTASKS])
def test_planner_subtask_bounds_accepts_in_range_plans(n: int):
    plan = parse_plan(_normalized_response(_plan_doc(n)))
    assert len(plan.subtasks) == n
    assert plan.query == "state of the art in agent harnesses 2026"
    assert all(st.max_tool_calls == 5 and st.max_model_calls == 3 for st in plan.subtasks)


@pytest.mark.parametrize("n", [0, 1, MIN_SUBTASKS - 1])
def test_planner_subtask_bounds_rejects_too_few(n: int):
    with pytest.raises(PlannerError):
        parse_plan(_normalized_response(_plan_doc(n)))


@pytest.mark.parametrize("n", [MAX_SUBTASKS + 1, MAX_SUBTASKS + 3])
def test_planner_subtask_bounds_rejects_too_many(n: int):
    with pytest.raises(PlannerError):
        parse_plan(_normalized_response(_plan_doc(n)))


def test_planner_rejects_a_subtask_missing_required_fields():
    doc = _plan_doc(MIN_SUBTASKS)
    del doc["subtasks"][0]["budget"]
    with pytest.raises(PlannerError):
        parse_plan(_normalized_response(doc))


def test_planner_rejects_non_json_model_output():
    with pytest.raises(PlannerError):
        parse_plan(_normalized_response("here is your plan: <not json>"))


def test_planner_rejects_output_that_is_not_normalized_chat_response_shaped():
    with pytest.raises(PlannerError):
        parse_plan({"unexpected": "shape"})


def test_build_planner_request_is_openai_chat_completions_shaped():
    request = build_planner_request("who invented the transformer?", "test-model")
    assert request["model"] == "test-model"
    assert request["messages"][-1] == {"role": "user", "content": "who invented the transformer?"}
    assert str(MIN_SUBTASKS) in request["messages"][0]["content"]
    assert str(MAX_SUBTASKS) in request["messages"][0]["content"]


async def test_planner_plan_calls_decide_and_returns_a_validated_plan():
    seen_requests = []

    async def fake_decide(rendered_context: dict) -> dict:
        seen_requests.append(rendered_context)
        return _normalized_response(_plan_doc(MIN_SUBTASKS, query=rendered_context["messages"][-1]["content"]))

    planner = Planner(model="test-model")
    plan = await planner.plan("how do agent harnesses handle context?", fake_decide)

    assert len(seen_requests) == 1
    assert seen_requests[0]["model"] == "test-model"
    assert plan.query == "how do agent harnesses handle context?"
    assert len(plan.subtasks) == MIN_SUBTASKS


async def test_planner_plan_propagates_out_of_bounds_plans_as_an_error():
    async def fake_decide(_rendered_context: dict) -> dict:
        return _normalized_response(_plan_doc(MAX_SUBTASKS + 2))

    planner = Planner(model="test-model")
    with pytest.raises(PlannerError):
        await planner.plan("a query that gets an over-broad plan", fake_decide)
