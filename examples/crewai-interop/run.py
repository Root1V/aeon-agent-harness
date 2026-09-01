#!/usr/bin/env python3
"""Runs the CrewAI interop example (INT-004, Modo B) end-to-end through aeon_sdk — this is what
"examples/crewai-interop corre dentro de una Activity" (roadmap.md) means concretely: a real
`crewai.Crew` (aeon_adapters/crewai/example_crew.py) runs inside a single Temporal Activity, with
its Agent calling Aeon's real Model Gateway — never a provider SDK directly.

Usage (from the repo root, with Temporal and the worker already up):
    uv run --project python python examples/crewai-interop/run.py "your query here"
"""
from __future__ import annotations

import asyncio
import os
import sys

from aeon_sdk.crewai_interop import start_crewai_interop_run

DEFAULT_QUERY = "what is the state of the art in AI agent harnesses in 2026?"

# This example doesn't ship its own ModelPolicyBundle — reuses the Deep Research one's
# reasoning-balanced profile candidates directly, since the point here is Modo B integration, not
# a new routing configuration.
CANDIDATES = [{"provider": "gemini", "model": "gemini-2.5-pro", "priority": 0}]


async def main(query: str) -> None:
    result = await start_crewai_interop_run(
        query,
        CANDIDATES,
        model=CANDIDATES[0]["model"],
        temporal_address=os.environ.get("AEON_TEMPORAL_ADDRESS", "localhost:7233"),
    )
    print(f"run_id: {result.run_id}")
    print(f"plan: {result.plan}")
    print(f"tool_result: {result.tool_result}")


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1] if len(sys.argv) > 1 else DEFAULT_QUERY))
