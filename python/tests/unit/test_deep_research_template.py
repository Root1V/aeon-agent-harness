"""DX-003's acceptance test: examples/deep-research isn't just individually-valid files
(FND-003's test_contracts.py) or executable (DX-002's test_examples_deep_research_run.py) — as a
*template*, its config-as-code files must actually agree with each other and with the rest of the
platform's registries. This catches the class of bug a schema check alone can't: a tool added to
`tools.allow` with no matching Cedar permit, a fallback profile that doesn't exist in the
ModelPolicyBundle, or an evalGate naming a suite nobody registered.
"""
from __future__ import annotations

import re
from pathlib import Path

import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
EXAMPLE_DIR = REPO_ROOT / "examples" / "deep-research"
EVALS_SUITES_DIR = REPO_ROOT / "evals" / "suites"


def _load(name: str) -> dict:
    return yaml.safe_load((EXAMPLE_DIR / name).read_text())


def _permitted_tools_for(principal: str, policy_bundle: dict) -> set[str]:
    """Extracts the tool names a permit policy's `[...].contains(resource.name)` list actually
    covers for `principal` — Cedar's set-membership syntax is `[...].contains(x)` (the list comes
    BEFORE the call, not inside its parens — see docs/adr/0002 and go/internal/policy). A
    deliberately narrow reader of this template's own policy style, not a general Cedar parser."""
    permitted: set[str] = set()
    for policy in policy_bundle["policies"]:
        if policy["effect"] != "permit" or f'Agent::"{principal}"' not in policy["cedarSource"]:
            continue
        contains_match = re.search(r"\[(.*?)\]\s*\.contains\(", policy["cedarSource"], re.DOTALL)
        if not contains_match:
            continue
        permitted.update(re.findall(r'"([\w.*-]+)"', contains_match.group(1)))
    return permitted


def test_every_allowed_tool_has_a_matching_cedar_permit():
    agent = _load("agent.yaml")
    policy_bundle = _load("policy_bundle.yaml")

    principal = f"{agent['metadata']['name']}@{agent['metadata']['version']}"
    allowed_tools = set(agent["spec"]["tools"]["allow"])
    permitted_tools = _permitted_tools_for(principal, policy_bundle)

    missing = allowed_tools - permitted_tools
    assert not missing, f"agent.yaml allows {missing} but no Cedar permit in policy_bundle.yaml covers them for {principal}"


def test_denied_tools_are_not_also_in_the_allow_list():
    """A tool pattern in both allow and deny wouldn't be a security bug (Cedar's forbid always
    wins over permit), but it would be a confusing, self-contradicting template to learn from."""
    agent = _load("agent.yaml")
    tools = agent["spec"]["tools"]
    assert set(tools["allow"]).isdisjoint(tools.get("deny", []))


def test_model_policy_profile_and_every_fallback_exist_in_the_bundle():
    agent = _load("agent.yaml")
    bundle = _load("model_policy_bundle.yaml")

    declared_profiles = {p["profile"] for p in bundle["profiles"]}
    model_policy = agent["spec"]["modelPolicy"]
    needed = {model_policy["profile"], *model_policy.get("fallbacks", [])}

    missing = needed - declared_profiles
    assert not missing, f"agent.yaml's modelPolicy references profile(s) {missing} not declared in model_policy_bundle.yaml"


def test_every_eval_gate_is_a_real_registered_suite():
    agent = _load("agent.yaml")
    real_suites = {p.stem for p in EVALS_SUITES_DIR.glob("*.yaml")}

    missing = set(agent["spec"].get("evalGates", [])) - real_suites
    assert not missing, f"agent.yaml's evalGates references suite(s) {missing} with no file under evals/suites/"


def test_run_py_and_supporting_files_all_exist():
    """The template is meant to be a self-contained, copyable starting point — every file
    examples/deep-research/run.py (DX-001) actually needs must ship alongside it."""
    for name in ("agent.yaml", "policy_bundle.yaml", "model_policy_bundle.yaml", "run.py"):
        assert (EXAMPLE_DIR / name).exists(), f"expected examples/deep-research/{name} to exist"
