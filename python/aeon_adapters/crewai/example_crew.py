"""The example crew examples/crewai-interop demonstrates (INT-004): a real, minimal `crewai.Crew`
— one Agent, one Task — whose Agent is configured with the injected `AeonLLM`, never a provider SDK
directly. Mirrors `aeon_adapters.langgraph.example_graph`'s "plan" step exactly: the model states a
one-sentence plan for the query. The "research" half (an actual Aeon tool call) is deliberately
*not* CrewAI's own tool-calling mechanism — see `aeon_worker.activities.framework_adapter_activities.
run_crewai_interop_activity`, which calls Aeon's Tool Gateway directly after this crew finishes,
the same division of labor LangGraph's `plan`/`research` nodes use, without depending on CrewAI's
own ReAct-style tool-call parsing format.
"""
from __future__ import annotations

from crewai import Agent, Crew, Process, Task

from aeon_adapters.crewai.adapter import AeonLLM


def build_example_crew(llm: AeonLLM) -> Crew:
    planner = Agent(
        # Deliberately distinct from aeon_profiles.deep_research.planner.Planner's own
        # "Research Planner" system-prompt text (used to key DR-001's own fake-gateway branch in
        # tests) — CrewAI interpolates role/backstory verbatim into its own prompt, and this must
        # never collide with that dispatch.
        role="CrewAI Interop Planner",
        goal="State a one-sentence plan for the given query.",
        backstory="You are a careful planner for a CrewAI interop example used to test Aeon's Model Gateway integration.",
        llm=llm,
    )
    task = Task(
        description="Query: {query}\n\nState a one-sentence plan for researching this query.",
        expected_output="A single sentence describing the plan.",
        agent=planner,
    )
    return Crew(agents=[planner], tasks=[task], process=Process.sequential)
