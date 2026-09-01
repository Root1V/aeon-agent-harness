"""FrameworkAdapter for the Claude Agent SDK (INT-007, Modo B — "bring your own loop"): runs a real
`claude_agent_sdk.ClaudeSDKClient` turn loop inside a single Temporal Activity boundary. This
adapter is structurally different from `INT-001`/`INT-004`/`INT-005`/`INT-006`, and the difference is
real, not a shortcut — read on before assuming it's "the same pattern again."

**Why the model call is NOT routed through Aeon's Model Gateway here (a real, documented
limitation, tracked in backlog.md):** every other `FrameworkAdapter` in this project accepts a
swappable model client (a `Model`, `BaseLLM`, or chat client) that this adapter points at
`aeon-modelgw`. The Claude Agent SDK has no such seam — `claude-agent-sdk` is a thin Python wrapper
that spawns the real `claude` (Claude Code) CLI as a subprocess and talks to it over stdio; that CLI
calls Anthropic's Messages API (`POST /v1/messages`) directly, or whatever Bedrock/Vertex/proxy
config the host's Claude Code installation already uses — there is no "custom Model" hook to
redirect. Routing this through Aeon would require a new Anthropic-Messages-API-compatible endpoint
on `aeon-modelgw` (mirroring `INT-002`, which only speaks OpenAI Chat Completions) — a real,
non-trivial prerequisite this feature does not attempt, rather than pretend to satisfy it.

**Where real Aeon governance IS enforced here, as a deliberate compensating design:** the security
boundary this integration is built around is the *tool* boundary, not the model boundary — the
opposite trade-off from `INT-004` (CrewAI routed the model, bypassed native tool-calling).
`ClaudeAgentOptions(tools=[], ...)` excludes **every** built-in Claude Code tool (Bash, Read, Write,
Edit, Glob, Grep, WebFetch, WebSearch, ...) entirely — not merely leaving them unapproved, they are
not offered to the model at all — and the only tool ever exposed is a custom in-process MCP tool
(`claude_agent_sdk.tool` + `create_sdk_mcp_server`) that calls Aeon's real `execute_tool` (RUN-004's
idempotent path). Whatever Claude decides to do, the only side effect it can ever cause is a real,
governed Aeon tool call.

**A real, heavier infra dependency, worth flagging plainly:** unlike the four prior adapters (pure
Python packages), this one requires the actual `@anthropic-ai/claude-code` npm CLI on `PATH` — the
Python package is a wrapper, not a reimplementation. `deploy/compose/Dockerfile.python` and
`Makefile`'s `test-python` target both install Node.js + that CLI for this reason.

**How the acceptance test stays hermetic (no live LLM, no cost — same rule every test in this
project follows):** the CLI subprocess inherits its parent's environment (verified by reading
`claude_agent_sdk`'s own transport source, not assumed: `inherited_env = os.environ`, then
`ClaudeAgentOptions.env` only overrides on top of that). So the test sets `ANTHROPIC_BASE_URL` on
the *worker process* itself (the same way every other interop test sets `AEON_MODELGW_ADDR` on the
spawned worker in `_spawn_worker`) before it reaches this adapter — no code here needs to know
about it. That base URL points at a local fake HTTP server that speaks the real Anthropic Messages
API shape. This is explicitly **not** a claim that this proves routing through `aeon-modelgw` — no
Aeon endpoint speaks this wire format yet — it is the same "fake the external network boundary"
technique used everywhere else in this project, applied to a different wire format.

Modo B's stated limitation applies here as everywhere: the CLI's own internal turn loop is real,
non-deterministic, and (here) an entirely separate OS process — it cannot run inside a Temporal
*workflow* (docs/adr/0001), which is exactly why the whole run happens inside ONE Activity instead.
"""
from __future__ import annotations

import json
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    SdkMcpTool,
    TextBlock,
    ToolUseBlock,
    create_sdk_mcp_server,
    tool,
)

from aeon_worker.activities.tool_activities import ExecuteToolInput, execute_tool


@dataclass
class ToolCallRecord:
    """One real invocation of an Aeon-bound tool, captured for callers that want to verify Claude's
    own reasoning loop genuinely triggered it (as opposed to a fixed activity-driven call)."""

    tool_name: str
    args: dict[str, Any]
    result: dict[str, Any]


def _mcp_tool_name(tool_name: str) -> str:
    """`claude_agent_sdk` names an in-process MCP tool `mcp__{server_name}__{tool_name}` — this
    fixes the server name at "aeon" so a caller only needs the Aeon tool name."""
    return f"mcp__aeon__{tool_name.replace('.', '_')}"


def build_tool_gateway_tool(tool_name: str, *, run_id: str, node_id: str, calls: list[ToolCallRecord]) -> SdkMcpTool[Any]:
    """Wraps one Aeon Tool Gateway tool as a real in-process MCP tool Claude can call natively.
    Every invocation goes through `execute_tool` (RUN-004's idempotent path) — Claude never runs a
    tool itself. `calls` accumulates a record of each real invocation so a caller can confirm the
    tool genuinely ran (this example only wires the single-`query`-argument shape `search.web`
    uses). The return shape (`{"content": [{"type": "text", ...}]}`) is the MCP tool-result envelope
    `claude_agent_sdk.tool` requires, not a raw passthrough."""
    step_seq = {"n": 0}
    bare_name = tool_name.replace(".", "_")

    @tool(bare_name, f"Call the Aeon-governed tool {tool_name!r}.", {"query": str})
    async def _call(args: dict[str, Any]) -> dict[str, Any]:
        step_seq["n"] += 1
        output = await execute_tool(
            ExecuteToolInput(
                run_id=run_id, node_id=node_id, step_seq=step_seq["n"], tool_name=tool_name, tool_args={"query": args["query"]}
            )
        )
        calls.append(ToolCallRecord(tool_name=tool_name, args={"query": args["query"]}, result=output.result))
        return {"content": [{"type": "text", "text": json.dumps(output.result)}]}

    return _call


# A system-prompt builder receives nothing (the tool/model surface is fixed by the adapter itself,
# deliberately not overridable — see module docstring) and returns the instructions text for the run.
SystemPromptBuilder = Callable[[], str]


async def run_claude_agent(
    build_system_prompt: SystemPromptBuilder,
    input_text: str,
    *,
    run_id: str,
    node_id: str,
    tool_names: tuple[str, ...] = ("search.web",),
    max_turns: int = 5,
) -> dict[str, Any]:
    """Runs Claude with Aeon-bound tools injected and every built-in Claude Code tool excluded, and
    returns its final text output plus every real tool call the run made. Meant to be called from
    inside a single Temporal Activity — never from workflow code, since the CLI's own turn loop is a
    separate OS process, not something Temporal can safely replay directly.

    No `env`/base-URL override is passed to `ClaudeAgentOptions` here — the CLI subprocess inherits
    this process's own environment, so whatever `ANTHROPIC_BASE_URL`/auth the worker process (or, in
    tests, `_spawn_worker`) was started with is what the CLI uses. See module docstring.
    """
    calls: list[ToolCallRecord] = []
    mcp_tools = [build_tool_gateway_tool(name, run_id=run_id, node_id=node_id, calls=calls) for name in tool_names]
    server = create_sdk_mcp_server(name="aeon", version="1.0.0", tools=mcp_tools)

    options = ClaudeAgentOptions(
        system_prompt=build_system_prompt(),
        tools=[],  # exclude every built-in Claude Code tool (Bash/Read/Write/WebFetch/...) — see module docstring
        mcp_servers={"aeon": server},
        allowed_tools=[_mcp_tool_name(name) for name in tool_names],
        max_turns=max_turns,
    )

    final_text = ""
    async with ClaudeSDKClient(options=options) as client:
        await client.query(input_text)
        async for message in client.receive_response():
            if isinstance(message, AssistantMessage):
                for block in message.content:
                    if isinstance(block, TextBlock):
                        final_text = block.text
                    # ToolUseBlock invocations are handled by the SDK's own in-process MCP
                    # transport (build_tool_gateway_tool above) — nothing to do with them here.
                    elif isinstance(block, ToolUseBlock):
                        pass

    return {"final_output": final_text, "tool_calls": calls}
