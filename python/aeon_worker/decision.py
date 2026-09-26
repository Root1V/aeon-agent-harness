"""Decision (proto/schemas/decision.schema.json): the typed output of a model.decide call. Per
docs/adr/0001-temporal-determinism-boundary.md, a Decision is what a deterministic workflow (or, for
now, aeon_profiles.deep_research.researcher's bounded ReAct loop) validates and applies — it never
calls a model directly itself.

decision_id is assigned here, not by the model: it's a tracking id for this decision, not something
worth spending model output tokens on or trusting the model to keep unique.
"""
from __future__ import annotations

import json
import os
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator

# AEON_SCHEMAS_DIR (same convention the Go CLI's resolveProtoDir uses) first, falling back to this
# repo's own layout — see aeon_profiles.deep_research.planner's identical comment for why the
# fallback alone isn't enough inside deploy/compose's worker container.
_PROTO_DIR = Path(os.environ["AEON_SCHEMAS_DIR"]) if os.environ.get("AEON_SCHEMAS_DIR") else Path(__file__).resolve().parents[2] / "proto"
_SCHEMA_PATH = _PROTO_DIR / "schemas" / "decision.schema.json"
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


def extract_json_object(content: str) -> str:
    """Returns the first complete JSON object in `content`, with any markdown fence removed (MDL-015).

    THE BOUNDARY, stated because this is the easy thing to erode: the object itself must be complete
    and valid JSON, and it must still validate against its schema afterwards. What is tolerated is
    only what surrounds it — a code fence, or trailing characters after the closing brace. Nothing
    here repairs a malformed object, guesses a missing field, or accepts a second object.

    Every accommodation was forced by a measured answer from the real platform, in this order:

        1. ```json {"action": ...} ```        a markdown fence, despite "no markdown fences"
        2. {"action": ...}.                   a trailing full stop after the closing brace

    Two in a row is the actual finding, and it is not about either model: an INSTRUCTION IS A REQUEST.
    The gateway drops `response_format`, so the only real constraint never reaches the model, and
    putting the shape in the prompt is a mitigation. Synaptum reported the same from their side. The
    honest fix is grammar-constrained decoding (ADR-004 names it, nothing implements it) — until then
    this function is the seam where that absence is paid for, and the list above is the receipt.
    """

    text = content.strip()
    if text.startswith("```"):
        # Drop the opening fence with its optional language tag, then everything from the closing one.
        text = text.split("\n", 1)[1] if "\n" in text else ""
        closing = text.rfind("```")
        text = (text[:closing] if closing != -1 else text).strip()

    start = text.find("{")
    if start == -1:
        return text  # no object at all: let the caller's json.loads produce the real error
    try:
        # raw_decode reads ONE complete JSON value and reports where it ended, so trailing characters
        # are ignored without the object itself being treated any more loosely.
        _, end = json.JSONDecoder().raw_decode(text[start:])
    except json.JSONDecodeError:
        return text  # malformed: return it unchanged so the caller reports it with the real content
    return text[start : start + end]


def parse_decision(raw_model_output: dict[str, Any]) -> Decision:
    """Extracts the model's JSON decision from a NormalizedChatResponse-shaped dict (choices[0].
    message.content), assigns it a fresh decision_id, and validates the result against
    decision.schema.json. Raises DecisionError on anything that doesn't validate."""
    try:
        content = raw_model_output["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise DecisionError(f"model output is not NormalizedChatResponse-shaped: {raw_model_output!r}") from exc

    # An EMPTY content string gets its own error, before the JSON parse, because "not valid JSON: ''"
    # names the wrong problem (MDL-015). This is the frequent case with a reasoning model, not an edge
    # one: the whole token allowance goes into reasoning_content, `content` comes back empty and
    # finish_reason is "length" — the model thought and never answered. Synaptum reported the
    # identical complaint about their own parser in the shared channel: an "Expecting value: line 1
    # column 1" over an empty string tells you nothing about what came back.
    #
    # Saying reasoning WAS present is the actionable half: it means the budget, not the prompt, is
    # what needs changing. MDL-016 is why that field survives to be looked at here at all.
    if not content or not content.strip():
        message = raw_model_output.get("choices", [{}])[0].get("message", {})
        reasoning = message.get("reasoning_content") or ""
        finish = raw_model_output.get("choices", [{}])[0].get("finish_reason")
        detail = f"finish_reason={finish!r}, reasoning_content={len(reasoning)} chars"
        # The advice depends on finish_reason, and getting that wrong is how this message misled its
        # own author: the first version said "raise max_tokens" whenever reasoning was present, and
        # the real case here came back finish_reason='stop' with 85 characters of reasoning — the
        # model was not out of budget, it chose to answer nothing. Suggesting a bigger budget there
        # sends the reader to tune a number that is not the problem.
        if finish == "length":
            detail += (
                ": the allowance ran out mid-generation, so raising max_tokens for this call is the "
                "fix — not changing the prompt"
            )
        elif reasoning:
            detail += (
                ": the model stopped cleanly having written only reasoning. The budget is NOT the "
                "problem. A reasoning model can put everything in its analysis channel and leave the "
                "answer channel empty, and this profile needs the answer"
            )
        else:
            detail += ": the model returned nothing at all, with no reasoning either"
        raise DecisionError(f"the model returned empty content ({detail})")

    try:
        decision_doc = json.loads(extract_json_object(content))
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
