"""Activities that run an external framework's own agent loop inside a single Activity boundary
(INT-001..007, Modo B) — the framework's internal orchestration is real, non-deterministic Python,
exactly why it can never run directly in workflow code (docs/adr/0001). Temporal's replay
reproduces only this Activity's recorded input/output, not the framework's own internal steps — the
documented limitation of Modo B (see roadmap.md's Modo B note), not a bug.
"""
from __future__ import annotations

from dataclasses import dataclass

from temporalio import activity

from aeon_adapters.langgraph.adapter import run_langgraph_graph
from aeon_adapters.langgraph.example_graph import build_example_graph
from aeon_worker.activities.model_activities import DecideCandidate


@dataclass
class LangGraphInteropInput:
    run_id: str
    query: str
    model: str
    candidates: list[DecideCandidate]
    data_sensitivity: str = ""


@dataclass
class LangGraphInteropOutput:
    query: str
    plan: str
    tool_result: dict


@activity.defn
async def run_langgraph_interop_activity(inp: LangGraphInteropInput) -> LangGraphInteropOutput:
    final_state = await run_langgraph_graph(
        build_example_graph,
        {"query": inp.query, "plan": "", "tool_result": {}},
        run_id=inp.run_id,
        node_id="langgraph-interop",
        candidates=inp.candidates,
        model=inp.model,
        data_sensitivity=inp.data_sensitivity,
    )
    return LangGraphInteropOutput(query=inp.query, plan=final_state["plan"], tool_result=final_state["tool_result"])
