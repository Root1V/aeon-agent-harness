"""FrameworkAdapter for the Microsoft Agent Framework (MAF) (INT-006, Modo B — "bring your own
loop"): runs a real `agent_framework.Agent` loop inside a single Temporal Activity boundary, giving
it a chat client bound to Aeon's Model Gateway and function tools bound to Aeon's Tool Gateway — an
Agent never talks to a provider SDK or executes a tool directly, the same rule every Aeon-native/
Modo-B call follows (docs/adr/0004, RUN-004).

Same shape as `INT-005` (OpenAI Agents SDK) for the same real reasons:

1. **No custom chat client protocol implementation.** MAF ships two OpenAI-wire-format clients:
   `OpenAIChatClient` (talks the newer Responses API, `client.responses.create`) and
   `OpenAIChatCompletionClient` (talks the classic Chat Completions API,
   `client.chat.completions.create`). `INT-002` already put a real Chat-Completions-shaped endpoint
   on `aeon-modelgw` (`POST /v1/chat/completions`) — so this adapter deliberately uses
   `OpenAIChatCompletionClient`, not the framework's own default `OpenAIChatClient`, to match that
   wire format. Getting this wrong (using the Responses-API client against a Chat-Completions
   endpoint) would silently 404/malform every request — verified against the real package's source,
   not assumed from the docs' more commonly-shown `OpenAIChatClient` examples.
2. **Native tool-calling is wired for real**, exactly like `INT-005` and unlike `INT-004`'s
   deliberate CrewAI bypass: `agent_framework.tool` is a stable, typed decorator (JSON-schema
   parameters inferred from type hints/docstring, an async callable) — not a prompt-parsed
   convention — so wrapping it around Aeon's real `execute_tool` (RUN-004's idempotent path) is safe
   and idiomatic. The Agent's own reasoning loop decides when to call it.

`Agent.run` is natively a coroutine (same as `agents.Runner.run` in `INT-005`, unlike CrewAI's
synchronous `Crew.kickoff`), so no sync/async bridge is needed here either.

Only `agent-framework-core` + `agent-framework-openai` are installed (not the `agent-framework` meta
package) — the meta package pulls in ~30 provider integrations (Anthropic, Bedrock, Gemini, Azure,
Redis, mem0, ...) Aeon never uses, the same over-installation problem `INT-004`'s CrewAI dependency
already showed (see backlog.md) — deliberately not repeated here.

Modo B's stated limitation applies here as everywhere: the framework's own internal turn loop is
real, non-deterministic Python — it cannot run inside a Temporal *workflow* (docs/adr/0001), which is
exactly why the whole run happens inside ONE Activity instead. Temporal's replay reproduces this
Activity's recorded input/output, not the framework's own internal step-by-step trajectory — see
aeon_worker.activities.framework_adapter_activities and roadmap.md's Modo B note.
"""
from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from agent_framework import Agent, tool
from agent_framework.openai import OpenAIChatCompletionClient

from aeon_worker.activities.model_activities import DEFAULT_MODELGW_ADDR
from aeon_worker.activities.tool_activities import ExecuteToolInput, execute_tool


def build_aeon_chat_client(model_profile: str, modelgw_addr: str | None = None) -> OpenAIChatCompletionClient:
    """Builds the chat client an Agent calls instead of a real provider SDK. `model_profile` is read
    by `aeon-modelgw`'s `POST /v1/chat/completions` (INT-002) as a capability profile name, resolved
    against a real `ModelPolicyBundle` — never a concrete provider/model (docs/adr/0004). The API
    key is a placeholder: authentication to `aeon-modelgw` isn't implemented yet (see backlog.md)."""
    return OpenAIChatCompletionClient(
        model=model_profile, api_key="unused", base_url=f"http://{modelgw_addr or DEFAULT_MODELGW_ADDR}/v1"
    )


@dataclass
class ToolCallRecord:
    """One real invocation of an Aeon-bound tool, captured for callers that want to verify the
    Agent's own reasoning loop genuinely triggered it (as opposed to a fixed activity-driven call)."""

    tool_name: str
    args: dict[str, Any]
    result: dict[str, Any]


def build_tool_gateway_tool(tool_name: str, *, run_id: str, node_id: str, calls: list[ToolCallRecord]) -> Callable[..., Any]:
    """Wraps one Aeon Tool Gateway tool as a real MAF tool (`agent_framework.tool`) an Agent can call
    natively. Every invocation goes through `execute_tool` (RUN-004's idempotent path) — the Agent
    never runs a tool itself. `calls` accumulates a record of each real invocation so a caller can
    confirm the tool genuinely ran (this example only wires the single-`query`-argument shape
    `search.web` uses)."""
    step_seq = {"n": 0}

    @tool(name=tool_name.replace(".", "_"), description=f"Call the Aeon-governed tool {tool_name!r}.")
    async def _call(query: str) -> dict[str, Any]:
        step_seq["n"] += 1
        output = await execute_tool(
            ExecuteToolInput(run_id=run_id, node_id=node_id, step_seq=step_seq["n"], tool_name=tool_name, tool_args={"query": query})
        )
        calls.append(ToolCallRecord(tool_name=tool_name, args={"query": query}, result=output.result))
        return output.result

    return _call


# An agent builder receives the Aeon-bound chat client and tools and returns a real, ready-to-run
# agent_framework.Agent.
AgentBuilder = Callable[[OpenAIChatCompletionClient, list[Callable[..., Any]]], Agent]


async def run_maf_agent(
    build_agent: AgentBuilder,
    input_text: str,
    *,
    run_id: str,
    node_id: str,
    model: str,
    tool_names: tuple[str, ...] = ("search.web",),
    modelgw_addr: str | None = None,
) -> dict[str, Any]:
    """Builds the agent with Aeon-bound chat client/tools injected, runs it, and returns its final
    output plus every real tool call the run made. Meant to be called from inside a single Temporal
    Activity — never from workflow code, since the framework's own turn loop is not something
    Temporal can safely replay directly."""
    calls: list[ToolCallRecord] = []
    aeon_client = build_aeon_chat_client(model, modelgw_addr)
    tools = [build_tool_gateway_tool(name, run_id=run_id, node_id=node_id, calls=calls) for name in tool_names]
    agent = build_agent(aeon_client, tools)

    result = await agent.run(input_text)

    return {"final_output": result.text, "tool_calls": calls}
