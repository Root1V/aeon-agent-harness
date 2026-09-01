#!/usr/bin/env python3
"""Runs the Claude Agent SDK interop example (INT-007, Modo B) end-to-end through aeon_sdk — this
is what "examples/claude-agent-sdk-interop corre dentro de una Activity" (roadmap.md) means
concretely: a real `claude_agent_sdk.ClaudeSDKClient` turn loop
(aeon_adapters/claude_agent_sdk/adapter.py) runs inside a single Temporal Activity. Unlike the other
interop examples, its model calls go directly to Anthropic (or whatever the host's Claude Code
installation is already configured with) — see the adapter's module docstring for why — but every
tool it can possibly call is a real Aeon-governed tool (RUN-004's execute_tool), since every
built-in Claude Code tool is excluded.

Usage (from the repo root, with Temporal, the worker, and the real `claude` CLI on PATH already
set up — see the worker's own auth/config, e.g. ANTHROPIC_API_KEY):
    uv run --project python python examples/claude-agent-sdk-interop/run.py "your query here"
"""
from __future__ import annotations

import asyncio
import os
import sys

from aeon_sdk.claude_agent_interop import start_claude_agent_interop_run

DEFAULT_QUERY = "what is the state of the art in AI agent harnesses in 2026?"


async def main(query: str) -> None:
    result = await start_claude_agent_interop_run(
        query,
        temporal_address=os.environ.get("AEON_TEMPORAL_ADDRESS", "localhost:7233"),
    )
    print(f"run_id: {result.run_id}")
    print(f"final_output: {result.final_output}")
    print(f"tool_result: {result.tool_result}")


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1] if len(sys.argv) > 1 else DEFAULT_QUERY))
