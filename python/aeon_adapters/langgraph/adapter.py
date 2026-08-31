"""FrameworkAdapter for LangGraph (INT-001, Modo B — "bring your own loop"): runs a compiled
LangGraph graph inside a single Temporal Activity boundary, giving its node functions real clients
bound to Aeon's Model Gateway and Tool Gateway — a node never talks to a provider SDK or executes a
tool directly, the same rule every Aeon-native call follows (docs/adr/0004, RUN-004).

Modo B's stated limitation applies here as everywhere: LangGraph's own internal orchestration is
real, non-deterministic Python — it cannot run inside a Temporal *workflow* (docs/adr/0001), which
is exactly why the whole graph invocation happens inside ONE Activity instead. Temporal's replay
reproduces this Activity's recorded input/output, not the graph's own internal step-by-step
trajectory — see aeon_worker.activities.framework_adapter_activities and roadmap.md's Modo B note.
"""
from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from aeon_worker.activities.model_activities import DecideCandidate, DecideInput, call_model_gateway
from aeon_worker.activities.tool_activities import ExecuteToolInput, execute_tool


@dataclass
class ModelGatewayChatClient:
    """The LLM client a LangGraph node calls instead of a real provider SDK — every call goes
    through Aeon's real Model Gateway, with the same routing/fallback any Aeon-native caller gets."""

    candidates: list[DecideCandidate]
    model: str
    data_sensitivity: str = ""

    async def chat(self, messages: list[dict[str, Any]]) -> dict[str, Any]:
        """Returns the Model Gateway's NormalizedChatResponse-shaped output (choices[0].message.
        content, usage, ...) — a LangGraph node reads content out of this exactly like it would
        from a real provider's chat completion."""
        rendered_context = {"model": self.model, "messages": messages}
        result = await call_model_gateway(
            DecideInput(candidates=self.candidates, rendered_context=rendered_context, data_sensitivity=self.data_sensitivity)
        )
        return result.output


@dataclass
class ToolGatewayCaller:
    """The tool-execution client a LangGraph node calls instead of running a tool directly —
    idempotent, the same path RUN-004's execute_tool_activity uses. Not policy-checked yet in this
    first version (that's the real Tool Gateway's HTTP path, go/internal/api — see backlog.md);
    this uses the same local, idempotent execution DR-002's Researcher already uses."""

    run_id: str
    node_id: str
    _step_seq: int = field(default=0, init=False)

    async def call(self, tool_name: str, args: dict[str, Any]) -> dict[str, Any]:
        self._step_seq += 1
        output = await execute_tool(
            ExecuteToolInput(run_id=self.run_id, node_id=self.node_id, step_seq=self._step_seq, tool_name=tool_name, tool_args=args)
        )
        return output.result


# A graph builder receives Aeon-bound clients and returns a compiled LangGraph graph (anything with
# an async `.ainvoke(state) -> state` method — langgraph.graph.StateGraph.compile()'s return type).
GraphBuilder = Callable[[ModelGatewayChatClient, ToolGatewayCaller], Any]


async def run_langgraph_graph(
    build_graph: GraphBuilder,
    initial_state: dict[str, Any],
    *,
    run_id: str,
    node_id: str,
    candidates: list[DecideCandidate],
    model: str,
    data_sensitivity: str = "",
) -> dict[str, Any]:
    """Builds the graph with Aeon-bound clients injected, invokes it, and returns its final state.
    Meant to be called from inside a single Temporal Activity — never from workflow code, since
    LangGraph's own `ainvoke` loop is not something Temporal can safely replay directly."""
    model_client = ModelGatewayChatClient(candidates=candidates, model=model, data_sensitivity=data_sensitivity)
    tool_client = ToolGatewayCaller(run_id=run_id, node_id=node_id)
    graph = build_graph(model_client, tool_client)
    return await graph.ainvoke(initial_state)
