"""aeon_sdk.model_policy resolves an AgentManifest's modelPolicy.profile against the real, checked-
in examples/deep-research/model_policy_bundle.yaml — the same file the Go Model Gateway's tests
(go/internal/api/tool_gateway_handlers_test.go's sibling files) load, so a drift here would be a
drift in what the SDK and the platform actually agree a profile resolves to.
"""
from __future__ import annotations

from pathlib import Path

import pytest

from aeon_sdk.model_policy import ProfileNotFoundError, load_model_policy_bundle, resolve_candidates

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE_PATH = REPO_ROOT / "examples" / "deep-research" / "model_policy_bundle.yaml"


def test_resolve_candidates_returns_the_real_reasoning_high_candidates():
    bundle = load_model_policy_bundle(BUNDLE_PATH)
    candidates = resolve_candidates(bundle, "reasoning-high")

    assert candidates == [
        {"provider": "anthropic", "model": "claude-opus-5", "priority": 0},
        {"provider": "openai", "model": "gpt-5.1", "priority": 1},
    ]


def test_resolve_candidates_returns_the_real_reasoning_local_candidate():
    bundle = load_model_policy_bundle(BUNDLE_PATH)
    candidates = resolve_candidates(bundle, "reasoning-local")

    assert candidates == [{"provider": "prometheus_inference", "model": "local-default", "priority": 0}]


def test_resolve_candidates_rejects_an_unknown_profile():
    bundle = load_model_policy_bundle(BUNDLE_PATH)
    with pytest.raises(ProfileNotFoundError):
        resolve_candidates(bundle, "not-a-real-profile")
