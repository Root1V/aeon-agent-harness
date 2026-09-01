"""FrameworkAdapter for CrewAI (INT-004, Modo B — "bring your own loop"): runs a real
`crewai.Crew` inside a single Temporal Activity boundary, giving its Agent(s) a custom LLM bound to
Aeon's Model Gateway — an Agent never talks to a provider SDK directly, the same rule every
Aeon-native/Modo-B call follows (docs/adr/0004, RUN-004).

Unlike LangGraph's `ainvoke` (async, awaited directly from a node), CrewAI's own execution is
synchronous by design: `crewai.BaseLLM.call` is a plain method, not a coroutine, and `Crew.kickoff`
blocks until the crew finishes. Bridging that back to Aeon's async `call_model_gateway` needs a
real, deliberate sync/async boundary, not a hack: `run_crewai_crew` below runs `Crew.kickoff` inside
a worker thread (`asyncio.to_thread`), so `AeonLLM.call` can safely start its own fresh event loop
with `asyncio.run()` — safe specifically because no event loop is already running on that worker
thread, unlike the async Activity that spawned it.

Modo B's stated limitation applies here as everywhere: CrewAI's own internal orchestration is real,
non-deterministic Python — it cannot run inside a Temporal *workflow* (docs/adr/0001), which is
exactly why the whole crew invocation happens inside ONE Activity instead. Temporal's replay
reproduces this Activity's recorded input/output, not the crew's own internal step-by-step
trajectory — see aeon_worker.activities.framework_adapter_activities and roadmap.md's Modo B note.
"""
from __future__ import annotations

import asyncio
from collections.abc import Callable
from typing import Any

from crewai import BaseLLM, Crew

from aeon_worker.activities.model_activities import DecideCandidate, DecideInput, call_model_gateway


class AeonLLM(BaseLLM):
    """The LLM a CrewAI Agent calls instead of litellm/a real provider SDK — every call goes
    through Aeon's real Model Gateway, with the same routing/fallback any Aeon-native caller gets.

    `call` is CrewAI's own synchronous contract (see module docstring for why `asyncio.run` here is
    safe rather than a footgun): it is only ever invoked from inside the worker thread
    `run_crewai_crew` spawns for `Crew.kickoff`, never directly from async Aeon code.
    """

    candidates: list[DecideCandidate]
    data_sensitivity: str = ""

    def call(
        self,
        messages: str | list[dict[str, Any]],
        tools: list[dict] | None = None,
        callbacks: list[Any] | None = None,
        available_functions: dict[str, Any] | None = None,
        from_task: Any = None,
        from_agent: Any = None,
        response_model: Any = None,
    ) -> str:
        if isinstance(messages, str):
            messages = [{"role": "user", "content": messages}]
        rendered_context = {"model": self.model, "messages": list(messages)}
        result = asyncio.run(
            call_model_gateway(
                DecideInput(candidates=self.candidates, rendered_context=rendered_context, data_sensitivity=self.data_sensitivity)
            )
        )
        return result.output["choices"][0]["message"]["content"]


# A crew builder receives the Aeon-bound LLM and returns a real, ready-to-run crewai.Crew.
CrewBuilder = Callable[[AeonLLM], Crew]


async def run_crewai_crew(
    build_crew: CrewBuilder,
    inputs: dict[str, Any],
    *,
    candidates: list[DecideCandidate],
    model: str,
    data_sensitivity: str = "",
) -> dict[str, Any]:
    """Builds the crew with the Aeon-bound LLM injected, runs it (via a worker thread — see module
    docstring), and returns its output. Meant to be called from inside a single Temporal Activity —
    never from workflow code, since CrewAI's own kickoff loop is not something Temporal can safely
    replay directly."""
    llm = AeonLLM(model=model, candidates=candidates, data_sensitivity=data_sensitivity)
    crew = build_crew(llm)

    def _kickoff() -> Any:
        return crew.kickoff(inputs=inputs)

    output = await asyncio.to_thread(_kickoff)
    return {"raw": output.raw}
