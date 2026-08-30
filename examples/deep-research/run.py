#!/usr/bin/env python3
"""Runs the Deep Research example end-to-end through aeon_sdk (DX-001) — this is what
"examples/deep-research corre con aeon_sdk" (roadmap.md) means concretely: this script, unmodified,
against a real Temporal + worker + Model Gateway.

Usage (from the repo root, with the worker and Temporal already up — see README/Makefile):
    uv run --project python python examples/deep-research/run.py "your query here"

Loads the real, checked-in agent.yaml and model_policy_bundle.yaml — the manifest never names a
concrete model, only a capability profile (spec.modelPolicy.profile); this script resolves that
profile against the bundle exactly the way the platform is supposed to (docs/adr/0004), rather than
hardcoding a provider here.
"""
from __future__ import annotations

import asyncio
import os
import sys
from pathlib import Path

import yaml

from aeon_sdk.deep_research import start_deep_research_run
from aeon_sdk.model_policy import load_model_policy_bundle, resolve_candidates

EXAMPLE_DIR = Path(__file__).resolve().parent
DEFAULT_QUERY = "what is the state of the art in AI agent harnesses in 2026?"


async def main(query: str) -> None:
    manifest = yaml.safe_load((EXAMPLE_DIR / "agent.yaml").read_text())
    bundle = load_model_policy_bundle(EXAMPLE_DIR / "model_policy_bundle.yaml")
    profile = manifest["spec"]["modelPolicy"]["profile"]
    candidates = resolve_candidates(bundle, profile)

    report = await start_deep_research_run(
        query,
        candidates,
        model=candidates[0]["model"],
        temporal_address=os.environ.get("AEON_TEMPORAL_ADDRESS", "localhost:7233"),
    )

    print(f"run_id: {report.run_id}")
    print(f"sufficient: {report.sufficient}")
    if report.topics_to_replan:
        print(f"topics needing another planning round: {report.topics_to_replan}")
    print(f"cited claims: {report.cited_claim_ids}")
    print()
    print(report.report_text)


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1] if len(sys.argv) > 1 else DEFAULT_QUERY))
