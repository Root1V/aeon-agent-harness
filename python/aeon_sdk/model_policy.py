"""Resolves an AgentManifest's spec.modelPolicy.profile into concrete Model Gateway candidates via
a checked-in ModelPolicyBundle (proto/manifests/model_policy_bundle.schema.json) — the manifest
itself never names a concrete provider/model, only a capability profile (docs/adr/0004)."""
from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml


class ProfileNotFoundError(KeyError):
    """Raised when a ModelPolicyBundle has no entry for the requested profile name."""


def load_model_policy_bundle(path: str | Path) -> dict[str, Any]:
    return yaml.safe_load(Path(path).read_text())


def resolve_candidates(bundle: dict[str, Any], profile: str) -> list[dict[str, Any]]:
    """Returns [{"provider", "model", "priority"}, ...] for `profile`, in whatever order the bundle
    declares them (the Model Gateway itself sorts by priority — see go/internal/modelgateway) —
    raises ProfileNotFoundError rather than silently returning an empty list for a typo'd profile."""
    for entry in bundle.get("profiles", []):
        if entry.get("profile") == profile:
            return [{"provider": c["provider"], "model": c["model"], "priority": c["priority"]} for c in entry["candidates"]]
    known = [entry.get("profile") for entry in bundle.get("profiles", [])]
    raise ProfileNotFoundError(f"no profile named {profile!r} in this ModelPolicyBundle (known: {known})")
