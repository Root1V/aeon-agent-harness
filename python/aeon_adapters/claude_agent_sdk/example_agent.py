"""The reference system prompt for `examples/claude-agent-sdk-interop` (INT-007): the only thing an
example gets to customize in this adapter (see `adapter.py`'s module docstring for why the tool/model
surface itself is fixed, not overridable, by example code).
"""
from __future__ import annotations


def build_example_system_prompt() -> str:
    return (
        "You are a research assistant for a Claude Agent SDK interop example. Use the "
        "search_web tool exactly once to investigate the user's query, then answer with a "
        "one-sentence summary of what the tool returned."
    )
