"""Activities that run an external framework's own agent loop inside a single Activity boundary
(INT-001..007, Modo B) — the framework's internal orchestration is real, non-deterministic Python,
exactly why it can never run directly in workflow code (docs/adr/0001). Temporal's replay
reproduces only this Activity's recorded input/output, not the framework's own internal steps — the
documented limitation of Modo B (see roadmap.md's Modo B note), not a bug.
"""
from __future__ import annotations

from dataclasses import dataclass

from temporalio import activity

from aeon_adapters.crewai.adapter import run_crewai_crew
from aeon_adapters.crewai.example_crew import build_example_crew
from aeon_adapters.langgraph.adapter import run_langgraph_graph
from aeon_adapters.langgraph.example_graph import build_example_graph
from aeon_adapters.openai_agents.adapter import run_openai_agent
from aeon_adapters.openai_agents.example_agent import build_example_agent
from aeon_worker.activities.model_activities import DecideCandidate
from aeon_worker.activities.tool_activities import ExecuteToolInput, execute_tool


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


@dataclass
class CrewAIInteropInput:
    run_id: str
    query: str
    model: str
    candidates: list[DecideCandidate]
    data_sensitivity: str = ""


@dataclass
class CrewAIInteropOutput:
    query: str
    plan: str
    tool_result: dict


@activity.defn
async def run_crewai_interop_activity(inp: CrewAIInteropInput) -> CrewAIInteropOutput:
    """Same "plan then research" shape as run_langgraph_interop_activity, split the same way: the
    crew (a real crewai.Crew, via AeonLLM) produces the plan; the tool call that follows is a
    direct Aeon Tool Gateway call (execute_tool), not CrewAI's own tool-calling mechanism — see
    aeon_adapters.crewai.example_crew's module docstring for why."""
    crew_result = await run_crewai_crew(
        build_example_crew,
        {"query": inp.query},
        candidates=inp.candidates,
        model=inp.model,
        data_sensitivity=inp.data_sensitivity,
    )

    tool_output = await execute_tool(
        ExecuteToolInput(run_id=inp.run_id, node_id="crewai-interop-research", step_seq=1, tool_name="search.web", tool_args={"query": inp.query})
    )

    return CrewAIInteropOutput(query=inp.query, plan=crew_result["raw"], tool_result=tool_output.result)


@dataclass
class OpenAIAgentsInteropInput:
    run_id: str
    query: str
    model: str


@dataclass
class OpenAIAgentsInteropOutput:
    query: str
    final_output: str
    tool_result: dict


@activity.defn
async def run_openai_agents_interop_activity(inp: OpenAIAgentsInteropInput) -> OpenAIAgentsInteropOutput:
    """Unlike run_langgraph_interop_activity/run_crewai_interop_activity's fixed plan-then-tool-call
    split, this Activity's tool call is driven entirely by the Agent's own native reasoning loop
    (see aeon_adapters.openai_agents.adapter's module docstring) — the Activity itself only builds
    the agent and runs it once."""
    result = await run_openai_agent(
        build_example_agent,
        inp.query,
        run_id=inp.run_id,
        node_id="openai-agents-interop",
        model=inp.model,
    )

    tool_calls = result["tool_calls"]
    tool_result = tool_calls[-1].result if tool_calls else {}
    return OpenAIAgentsInteropOutput(query=inp.query, final_output=result["final_output"], tool_result=tool_result)
