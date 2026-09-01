#!/usr/bin/env python3
"""Runs the Microsoft Agent Framework interop example (INT-006, Modo B) end-to-end through
aeon_sdk — this is what "examples/maf-interop corre dentro de una Activity" (roadmap.md) means
concretely: a real `agent_framework.Agent` (aeon_adapters/microsoft_agent_framework/example_agent.py)
runs inside a single Temporal Activity, calling Aeon's real Model Gateway's OpenAI-compatible
endpoint (INT-002) and, when the Agent itself decides to, a real Aeon tool — never a provider SDK or
a tool implementation directly.

Usage (from the repo root, with Temporal, aeon-modelgw and the worker already up):
    uv run --project python python examples/maf-interop/run.py "your query here"
"""
from __future__ import annotations

import asyncio
import os
import sys

from aeon_sdk.maf_interop import start_maf_interop_run

DEFAULT_QUERY = "what is the state of the art in AI agent harnesses in 2026?"

# "reasoning-balanced" is a real profile from examples/deep-research/model_policy_bundle.yaml —
# aeon-modelgw's POST /v1/chat/completions (INT-002) resolves it against that bundle, exactly like
# examples/openai-agents-interop/run.py.
MODEL_PROFILE = "reasoning-balanced"


async def main(query: str) -> None:
    result = await start_maf_interop_run(
        query,
        model=MODEL_PROFILE,
        temporal_address=os.environ.get("AEON_TEMPORAL_ADDRESS", "localhost:7233"),
    )
    print(f"run_id: {result.run_id}")
    print(f"final_output: {result.final_output}")
    print(f"tool_result: {result.tool_result}")


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1] if len(sys.argv) > 1 else DEFAULT_QUERY))
