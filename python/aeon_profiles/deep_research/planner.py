"""Research Planner (DR-001): produces a fixed set of 3-5 subtasks covering a query, each with a
distinct coverage topic and its own tool/model-call budget, before any Researcher (DR-002) starts.

Model-calling is deliberately factored out as an injected `decide` callable rather than imported
directly: the real path is aeon_worker.activities.model_activities.decide_activity, invoked via
workflow.execute_activity from within a workflow (docs/adr/0001 — this module performs no I/O of
its own and has no Temporal import, so it's directly unit-testable with a fake `decide`).
"""
from __future__ import annotations

import json
import os
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator

# AEON_SCHEMAS_DIR (same convention the Go CLI's resolveProtoDir uses) first, falling back to this
# repo's own layout — the fallback only holds in a full checkout, not inside a container image
# that ships python/ alone. See deploy/compose's worker service, which mounts proto/ read-only and
# sets AEON_SCHEMAS_DIR — without it, this import crashes the whole worker process on startup.
_PROTO_DIR = Path(os.environ["AEON_SCHEMAS_DIR"]) if os.environ.get("AEON_SCHEMAS_DIR") else Path(__file__).resolve().parents[3] / "proto"
_SCHEMA_PATH = _PROTO_DIR / "schemas" / "research_plan.schema.json"
_VALIDATOR = Draft202012Validator(json.loads(_SCHEMA_PATH.read_text()))

MIN_SUBTASKS = 3
MAX_SUBTASKS = 5


class PlannerError(ValueError):
    """Raised when the model's plan doesn't validate against research_plan.schema.json — including
    having fewer than MIN_SUBTASKS or more than MAX_SUBTASKS subtasks. There is no silent clamping
    or truncation: an invalid plan must never reach a Researcher (DR-002)."""


@dataclass
class Subtask:
    id: str
    description: str
    coverage_topic: str
    max_tool_calls: int
    max_model_calls: int


@dataclass
class ResearchPlan:
    query: str
    subtasks: list[Subtask]


def build_planner_request(query: str, model: str) -> dict[str, Any]:
    """The rendered_context (an OpenAI-Chat-Completions-shaped request body) sent to the Model
    Gateway's /decide — see go/internal/providers.NormalizedChatResponse for the response shape
    this is designed to elicit, and parse_plan below for how it's read back."""
    instructions = (
        "You are the Research Planner for a Deep Research agent. Given the user's query, break it "
        f"into {MIN_SUBTASKS} to {MAX_SUBTASKS} subtasks, each covering a distinct facet of the "
        "query (no overlapping coverage_topic values). Respond with ONLY a JSON object matching "
        'this shape: {"query": string, "subtasks": [{"id": string, "description": string, '
        '"coverage_topic": string, "budget": {"max_tool_calls": integer, "max_model_calls": '
        'integer}}]}. No prose, no markdown fences — the JSON object alone.'
    )
    return {
        "model": model,
        "messages": [
            {"role": "system", "content": instructions},
            {"role": "user", "content": query},
        ],
    }


def parse_plan(raw_model_output: dict[str, Any]) -> ResearchPlan:
    """Extracts the model's JSON plan from a NormalizedChatResponse-shaped dict (choices[0].message.
    content) and validates it against research_plan.schema.json. Raises PlannerError on anything
    that doesn't validate — malformed JSON, missing fields, or a subtask count outside
    [MIN_SUBTASKS, MAX_SUBTASKS]."""
    try:
        content = raw_model_output["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise PlannerError(f"model output is not NormalizedChatResponse-shaped: {raw_model_output!r}") from exc

    try:
        plan_doc = json.loads(content)
    except json.JSONDecodeError as exc:
        raise PlannerError(f"model output is not valid JSON: {content!r}") from exc

    errors = sorted(_VALIDATOR.iter_errors(plan_doc), key=lambda e: list(e.path))
    if errors:
        raise PlannerError("plan failed schema validation: " + "; ".join(e.message for e in errors))

    subtasks = [
        Subtask(
            id=s["id"],
            description=s["description"],
            coverage_topic=s["coverage_topic"],
            max_tool_calls=s["budget"]["max_tool_calls"],
            max_model_calls=s["budget"]["max_model_calls"],
        )
        for s in plan_doc["subtasks"]
    ]
    return ResearchPlan(query=plan_doc["query"], subtasks=subtasks)


DecideFn = Callable[[dict[str, Any]], Awaitable[dict[str, Any]]]


class Planner:
    """Calls a model via `decide` (the real path: aeon_worker.activities.model_activities.
    decide_activity through workflow.execute_activity) and validates its plan. Kept as a thin class
    rather than a bare function so DR-002/DR-003 can hold a configured instance (default model)
    without threading extra parameters through every call site."""

    def __init__(self, model: str) -> None:
        self.model = model

    async def plan(self, query: str, decide: DecideFn) -> ResearchPlan:
        raw_output = await decide(build_planner_request(query, self.model))
        return parse_plan(raw_output)
