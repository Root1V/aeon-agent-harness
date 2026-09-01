"""The reference Agent for `examples/openai-agents-interop` (INT-005): a real `agents.Agent` whose
model and tools are both injected by `aeon_adapters.openai_agents.adapter.run_openai_agent` — it
never sees a provider SDK or a real tool implementation, only Aeon-bound stand-ins.
"""
from __future__ import annotations

from agents import Agent, FunctionTool, OpenAIChatCompletionsModel


def build_example_agent(model: OpenAIChatCompletionsModel, tools: list[FunctionTool]) -> Agent:
    return Agent(
        name="OpenAI Agents Interop Researcher",
        instructions=(
            "You are a research assistant for an OpenAI Agents SDK interop example. Use the "
            "search_web tool exactly once to investigate the user's query, then answer with a "
            "one-sentence summary of what the tool returned."
        ),
        model=model,
        tools=tools,
    )
