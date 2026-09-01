"""The reference Agent for `examples/maf-interop` (INT-006): a real `agent_framework.Agent` whose
chat client and tools are both injected by
`aeon_adapters.microsoft_agent_framework.adapter.run_maf_agent` — it never sees a provider SDK or a
real tool implementation, only Aeon-bound stand-ins.
"""
from __future__ import annotations

from collections.abc import Callable
from typing import Any

from agent_framework import Agent
from agent_framework.openai import OpenAIChatCompletionClient


def build_example_agent(client: OpenAIChatCompletionClient, tools: list[Callable[..., Any]]) -> Agent:
    return Agent(
        client,
        "You are a research assistant for a Microsoft Agent Framework interop example. Use the "
        "search_web tool exactly once to investigate the user's query, then answer with a "
        "one-sentence summary of what the tool returned.",
        name="MAF Interop Researcher",
        tools=tools,
    )
