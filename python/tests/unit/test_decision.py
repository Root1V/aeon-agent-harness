"""Unit tests for aeon_worker.decision.parse_decision — the typed model.decide output DR-002's
Researcher (and, later, the Graph Runtime's own ReAct loop) applies without ever calling a model
itself (docs/adr/0001)."""
from __future__ import annotations

import json

import pytest

from aeon_worker.decision import VALID_ACTIONS, DecisionError, parse_decision


def _response(content: dict | str) -> dict:
    text = content if isinstance(content, str) else json.dumps(content)
    return {
        "model": "test-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


def test_parse_decision_assigns_a_fresh_decision_id_when_the_model_omits_one():
    decision = parse_decision(_response({"action": "FINISH", "message": "done"}))
    assert decision.decision_id
    assert decision.action == "FINISH"
    assert decision.message == "done"


def test_parse_decision_two_calls_get_distinct_decision_ids():
    d1 = parse_decision(_response({"action": "FINISH"}))
    d2 = parse_decision(_response({"action": "FINISH"}))
    assert d1.decision_id != d2.decision_id


@pytest.mark.parametrize("action", sorted(VALID_ACTIONS))
def test_parse_decision_accepts_every_documented_action(action: str):
    decision = parse_decision(_response({"action": action}))
    assert decision.action == action


def test_parse_decision_rejects_an_action_outside_the_enum():
    with pytest.raises(DecisionError):
        parse_decision(_response({"action": "DELETE_EVERYTHING"}))


def test_parse_decision_rejects_missing_action():
    with pytest.raises(DecisionError):
        parse_decision(_response({"message": "no action field"}))


def test_parse_decision_rejects_an_undocumented_field():
    with pytest.raises(DecisionError):
        parse_decision(_response({"action": "FINISH", "chain_of_thought": "leaked reasoning"}))


def test_parse_decision_rejects_non_json_content():
    with pytest.raises(DecisionError):
        parse_decision(_response("I think the answer is 42"))


def test_parse_decision_rejects_output_that_is_not_normalized_chat_response_shaped():
    with pytest.raises(DecisionError):
        parse_decision({"unexpected": "shape"})


def test_parse_decision_carries_tool_call_fields_through():
    decision = parse_decision(_response({"action": "CALL_TOOL", "tool_name": "search.web", "args": {"query": "x"}}))
    assert decision.tool_name == "search.web"
    assert decision.args == {"query": "x"}
