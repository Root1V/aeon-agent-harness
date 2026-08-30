"""Isolated Researchers (DR-002): one bounded ReAct loop per Research Planner (DR-001) subtask,
each with its own model-call/tool-call transcript — no subtask's Researcher ever sees another's
messages or tool results, even when several run concurrently (run_researchers_in_parallel below).
A bad tool result or a coverage gap in one subtask can never leak into a sibling's evidence.

Bounded: a Researcher stops the moment its subtask's own budget (max_model_calls/max_tool_calls,
from DR-001's ResearchPlan) is exhausted, or the model itself returns a FINISH/REQUEST_REPLAN
decision — whichever comes first. There is no unbounded loop.

Like planner.py, model-calling and tool-execution are injected callables (`decide`/`execute_tool`)
— this module has no Temporal import and is directly unit-testable with fakes; the real path is
aeon_worker.activities.model_activities.decide_activity / .tool_activities.execute_tool_activity via
workflow.execute_activity.
"""
from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

from aeon_profiles.deep_research.planner import ResearchPlan, Subtask
from aeon_worker.decision import parse_decision

DecideFn = Callable[[dict[str, Any]], Awaitable[dict[str, Any]]]
ExecuteToolFn = Callable[[str, dict[str, Any]], Awaitable[dict[str, Any]]]
RecallFn = Callable[[str], Awaitable[str]]


class ResearcherError(RuntimeError):
    """Raised when a Decision names an action this Researcher can't apply — including
    RECALL_OBSERVATION with no `recall` callable configured. Failing loudly here beats silently
    treating an unhandled action as a no-op, which would hide a real capability gap."""


@dataclass
class ToolCallRecord:
    tool_name: str
    args: dict[str, Any]
    result: dict[str, Any]


@dataclass
class ResearchResult:
    subtask_id: str
    messages: list[dict[str, str]]
    tool_calls: list[ToolCallRecord] = field(default_factory=list)
    finished_reason: str = "finish"  # finish | budget_exhausted | replan_requested
    final_message: str | None = None


def build_researcher_instructions(subtask: Subtask) -> str:
    return (
        "You are an isolated Researcher for a Deep Research agent, responsible for exactly one "
        f"subtask: {subtask.description!r} (coverage topic: {subtask.coverage_topic!r}). You do not "
        "see any other subtask's work. Respond with ONLY a JSON object matching: "
        '{"action": "CALL_TOOL"|"RECALL_OBSERVATION"|"EMIT_MESSAGE"|"REQUEST_REPLAN"|"FINISH", '
        '"tool_name": string|null, "args": object|null, "recall_id": string|null, '
        '"message": string|null}. No prose, no markdown fences — the JSON object alone. Call FINISH '
        "with a summary in `message` once you have enough evidence for this subtask alone."
    )


class Researcher:
    """Runs a single subtask's bounded ReAct loop, isolated from every other subtask. All state
    (the message transcript, tool-call history, and budget counters) lives in local variables
    inside `research()` — never on `self` or any shared object — so nothing here can be shared by
    accident across concurrent Researcher instances."""

    def __init__(self, subtask: Subtask, model: str) -> None:
        self.subtask = subtask
        self.model = model

    async def research(
        self, decide: DecideFn, execute_tool: ExecuteToolFn, recall: RecallFn | None = None
    ) -> ResearchResult:
        messages: list[dict[str, str]] = [
            {"role": "system", "content": build_researcher_instructions(self.subtask)},
            {"role": "user", "content": self.subtask.description},
        ]
        tool_calls: list[ToolCallRecord] = []
        model_calls = 0
        tool_call_count = 0

        while True:
            if model_calls >= self.subtask.max_model_calls:
                return ResearchResult(
                    subtask_id=self.subtask.id, messages=messages, tool_calls=tool_calls, finished_reason="budget_exhausted"
                )

            raw_output = await decide({"model": self.model, "messages": messages})
            model_calls += 1
            decision = parse_decision(raw_output)
            messages.append({"role": "assistant", "content": raw_output["choices"][0]["message"]["content"]})

            if decision.action == "FINISH":
                return ResearchResult(
                    subtask_id=self.subtask.id,
                    messages=messages,
                    tool_calls=tool_calls,
                    finished_reason="finish",
                    final_message=decision.message,
                )

            if decision.action == "REQUEST_REPLAN":
                return ResearchResult(
                    subtask_id=self.subtask.id,
                    messages=messages,
                    tool_calls=tool_calls,
                    finished_reason="replan_requested",
                    final_message=decision.message,
                )

            if decision.action == "EMIT_MESSAGE":
                messages.append({"role": "user", "content": "(noted; continue)"})
                continue

            if decision.action == "CALL_TOOL":
                if tool_call_count >= self.subtask.max_tool_calls:
                    return ResearchResult(
                        subtask_id=self.subtask.id, messages=messages, tool_calls=tool_calls, finished_reason="budget_exhausted"
                    )
                tool_call_count += 1
                args = decision.args or {}
                result = await execute_tool(decision.tool_name, args)
                tool_calls.append(ToolCallRecord(tool_name=decision.tool_name, args=args, result=result))
                messages.append({"role": "user", "content": json.dumps({"tool_result": result})})
                continue

            if decision.action == "RECALL_OBSERVATION":
                if recall is None:
                    raise ResearcherError(
                        f"subtask {self.subtask.id!r}: decision requested RECALL_OBSERVATION but no "
                        "recall callable was configured for this Researcher"
                    )
                content = await recall(decision.recall_id or "")
                messages.append({"role": "user", "content": json.dumps({"recalled": content})})
                continue

            raise ResearcherError(f"subtask {self.subtask.id!r}: unhandled decision action {decision.action!r}")


async def run_researchers_in_parallel(
    plan: ResearchPlan, model: str, decide: DecideFn, execute_tool: ExecuteToolFn, recall: RecallFn | None = None
) -> list[ResearchResult]:
    """The "parallel worker contexts" half of DR-002: every subtask's Researcher runs concurrently
    via asyncio.gather, each with its own freshly constructed local state (see Researcher.research)
    — sharing the same `decide`/`execute_tool`/`recall` callables across them is safe because those
    are stateless I/O calls (the real Model/Tool Gateways), not per-researcher state."""
    results = await asyncio.gather(
        *(Researcher(subtask, model).research(decide, execute_tool, recall) for subtask in plan.subtasks)
    )
    return list(results)
