"""Decision (proto/schemas/decision.schema.json): the typed output of a model.decide call. Per
docs/adr/0001-temporal-determinism-boundary.md, a Decision is what a deterministic workflow (or, for
now, aeon_profiles.deep_research.researcher's bounded ReAct loop) validates and applies — it never
calls a model directly itself.

decision_id is assigned here, not by the model: it's a tracking id for this decision, not something
worth spending model output tokens on or trusting the model to keep unique.
"""
from __future__ import annotations

import json
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator

_SCHEMA_PATH = Path(__file__).resolve().parents[2] / "proto" / "schemas" / "decision.schema.json"
_VALIDATOR = Draft202012Validator(json.loads(_SCHEMA_PATH.read_text()))

VALID_ACTIONS = frozenset({"CALL_TOOL", "RECALL_OBSERVATION", "EMIT_MESSAGE", "REQUEST_REPLAN", "FINISH"})


class DecisionError(ValueError):
    """Raised when a model's raw output isn't NormalizedChatResponse-shaped, isn't valid JSON, or
    doesn't validate against decision.schema.json (including an action outside VALID_ACTIONS)."""


@dataclass
class Decision:
    decision_id: str
    action: str
    tool_name: str | None = None
    args: dict[str, Any] | None = None
    recall_id: str | None = None
    message: str | None = None


def parse_decision(raw_model_output: dict[str, Any]) -> Decision:
    """Extracts the model's JSON decision from a NormalizedChatResponse-shaped dict (choices[0].
    message.content), assigns it a fresh decision_id, and validates the result against
    decision.schema.json. Raises DecisionError on anything that doesn't validate."""
    try:
        content = raw_model_output["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise DecisionError(f"model output is not NormalizedChatResponse-shaped: {raw_model_output!r}") from exc

    try:
        decision_doc = json.loads(content)
    except json.JSONDecodeError as exc:
        raise DecisionError(f"model output is not valid JSON: {content!r}") from exc

    if not isinstance(decision_doc, dict):
        raise DecisionError(f"model output must be a JSON object, got: {decision_doc!r}")

    decision_doc.setdefault("decision_id", str(uuid.uuid4()))

    errors = sorted(_VALIDATOR.iter_errors(decision_doc), key=lambda e: list(e.path))
    if errors:
        raise DecisionError("decision failed schema validation: " + "; ".join(e.message for e in errors))

    return Decision(
        decision_id=decision_doc["decision_id"],
        action=decision_doc["action"],
        tool_name=decision_doc.get("tool_name"),
        args=decision_doc.get("args"),
        recall_id=decision_doc.get("recall_id"),
        message=decision_doc.get("message"),
    )
