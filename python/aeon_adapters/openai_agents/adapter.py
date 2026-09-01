"""FrameworkAdapter for the OpenAI Agents SDK (INT-005, Modo B — "bring your own loop"): runs a
real `agents.Agent`/`agents.Runner` loop inside a single Temporal Activity boundary, giving it a
model client bound to Aeon's Model Gateway and function tools bound to Aeon's Tool Gateway — an
Agent never talks to a provider SDK or executes a tool directly, the same rule every Aeon-native/
Modo-B call follows (docs/adr/0004, RUN-004).

Two real differences from `INT-001` (LangGraph) and `INT-004` (CrewAI), both worth the design
decisions they represent:

1. **No custom `Model` subclass.** The SDK ships `OpenAIChatCompletionsModel`, built to call any
   OpenAI-Chat-Completions-compatible endpoint. `INT-002` already put exactly that endpoint on
   `aeon-modelgw` (`POST /v1/chat/completions`, "model" read as a capability profile name) — so
   `build_aeon_model` below just points the SDK's own official `AsyncOpenAI` client at it. This is
   the SDK's own documented integration path for a custom provider (see
   `examples/model_providers/custom_example_provider.py` in the upstream repo), not a workaround,
   and it is the first real framework to exercise `INT-002`'s endpoint end-to-end (previously only
   verified by hand with the bare `openai` package).
2. **Native tool-calling is wired for real**, unlike `INT-004`'s deliberate choice to bypass
   CrewAI's own tool-calling. The SDK's `FunctionTool`/`@function_tool` contract is a stable, typed,
   officially-supported interface (JSON-schema parameters, a JSON-string argument payload, an async
   `on_invoke_tool` callback) — not a prompt-parsed convention — so wrapping it around Aeon's real
   `execute_tool` (RUN-004's idempotent path) is safe and idiomatic. This is the first Modo B example
   where the framework's own reasoning loop decides when to call an Aeon tool, rather than the
   Activity driving a fixed plan-then-tool-call sequence.

`Runner.run` is natively a coroutine (unlike CrewAI's synchronous `Crew.kickoff`), so no sync/async
bridge is needed here — it can be awaited directly from inside the async Activity.

Modo B's stated limitation applies here as everywhere: the SDK's own internal turn loop is real,
non-deterministic Python — it cannot run inside a Temporal *workflow* (docs/adr/0001), which is
exactly why the whole run happens inside ONE Activity instead. Temporal's replay reproduces this
Activity's recorded input/output, not the SDK's own internal step-by-step trajectory — see
aeon_worker.activities.framework_adapter_activities and roadmap.md's Modo B note.
"""
from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from agents import Agent, FunctionTool, OpenAIChatCompletionsModel, RunConfig, Runner, function_tool
from openai import AsyncOpenAI

from aeon_worker.activities.model_activities import DEFAULT_MODELGW_ADDR
from aeon_worker.activities.tool_activities import ExecuteToolInput, execute_tool


def build_aeon_model(model_profile: str, modelgw_addr: str | None = None) -> OpenAIChatCompletionsModel:
    """Builds the Model an Agent calls instead of a real provider SDK. `model_profile` is read by
    `aeon-modelgw`'s `POST /v1/chat/completions` (INT-002) as a capability profile name, resolved
    against a real `ModelPolicyBundle` — never a concrete provider/model (docs/adr/0004). The API
    key is a placeholder: authentication to `aeon-modelgw` isn't implemented yet (see backlog.md);
    the SDK's own client just requires a non-empty string to construct."""
    client = AsyncOpenAI(base_url=f"http://{modelgw_addr or DEFAULT_MODELGW_ADDR}/v1", api_key="unused")
    return OpenAIChatCompletionsModel(model=model_profile, openai_client=client)


@dataclass
class ToolCallRecord:
    """One real invocation of an Aeon-bound tool, captured for callers that want to verify the
    Agent's own reasoning loop genuinely triggered it (as opposed to a fixed activity-driven call)."""

    tool_name: str
    args: dict[str, Any]
    result: dict[str, Any]


def build_tool_gateway_tool(tool_name: str, *, run_id: str, node_id: str, calls: list[ToolCallRecord]) -> FunctionTool:
    """Wraps one Aeon Tool Gateway tool as a real `FunctionTool` an Agent can call natively. Every
    invocation goes through `execute_tool` (RUN-004's idempotent path) — the Agent never runs a tool
    itself. `calls` accumulates a record of each real invocation so a caller can confirm the tool
    genuinely ran (this example only wires the single-`query`-argument shape `search.web` uses)."""
    step_seq = {"n": 0}

    @function_tool(name_override=tool_name.replace(".", "_"), description_override=f"Call the Aeon-governed tool {tool_name!r}.")
    async def _call(query: str) -> dict[str, Any]:
        step_seq["n"] += 1
        output = await execute_tool(
            ExecuteToolInput(run_id=run_id, node_id=node_id, step_seq=step_seq["n"], tool_name=tool_name, tool_args={"query": query})
        )
        calls.append(ToolCallRecord(tool_name=tool_name, args={"query": query}, result=output.result))
        return output.result

    return _call


# An agent builder receives the Aeon-bound model and tools and returns a real, ready-to-run
# agents.Agent.
AgentBuilder = Callable[[OpenAIChatCompletionsModel, list[FunctionTool]], Agent]


async def run_openai_agent(
    build_agent: AgentBuilder,
    input_text: str,
    *,
    run_id: str,
    node_id: str,
    model: str,
    tool_names: tuple[str, ...] = ("search.web",),
    modelgw_addr: str | None = None,
) -> dict[str, Any]:
    """Builds the agent with Aeon-bound model/tools injected, runs it, and returns its final output
    plus every real tool call the run made. Meant to be called from inside a single Temporal
    Activity — never from workflow code, since the SDK's own turn loop is not something Temporal can
    safely replay directly.

    `tracing_disabled=True`: the SDK's default tracing exports to OpenAI's own backend, which Aeon
    must never depend on (or need an OpenAI API key for) — Aeon has its own OTel tracing (OBS-001).
    """
    calls: list[ToolCallRecord] = []
    aeon_model = build_aeon_model(model, modelgw_addr)
    tools = [build_tool_gateway_tool(name, run_id=run_id, node_id=node_id, calls=calls) for name in tool_names]
    agent = build_agent(aeon_model, tools)

    result = await Runner.run(agent, input_text, run_config=RunConfig(tracing_disabled=True))

    return {"final_output": result.final_output, "tool_calls": calls}
