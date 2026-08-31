"""The example graph examples/langgraph-interop demonstrates (INT-001): a real, minimal
`langgraph.graph.StateGraph` — plan -> research -> END — whose two nodes are plain Python functions
that call the Aeon-bound clients `run_langgraph_graph` injects, never a provider SDK or a tool
directly. This is what proves Modo B's core property: an external framework's own graph, running
inside Aeon, still respects "the model is never the point of a policy application, and never talks
to a provider directly" (roadmap.md's guiding principle) — the graph itself is unaware of Anthropic,
OpenAI, or any specific tool implementation; it only knows the two clients it was handed.
"""
from __future__ import annotations

from typing import TypedDict

from langgraph.graph import END, START, StateGraph

from aeon_adapters.langgraph.adapter import ModelGatewayChatClient, ToolGatewayCaller


class InteropState(TypedDict):
    query: str
    plan: str
    tool_result: dict


def build_example_graph(model_client: ModelGatewayChatClient, tool_client: ToolGatewayCaller):
    async def plan(state: InteropState) -> dict:
        output = await model_client.chat(
            [
                {
                    "role": "system",
                    "content": "You are a research planner for a LangGraph interop example. State a one-sentence plan for the given query.",
                },
                {"role": "user", "content": state["query"]},
            ]
        )
        return {"plan": output["choices"][0]["message"]["content"]}

    async def research(state: InteropState) -> dict:
        result = await tool_client.call("search.web", {"query": state["query"]})
        return {"tool_result": result}

    graph = StateGraph(InteropState)
    graph.add_node("plan", plan)
    graph.add_node("research", research)
    graph.add_edge(START, "plan")
    graph.add_edge("plan", "research")
    graph.add_edge("research", END)
    return graph.compile()
