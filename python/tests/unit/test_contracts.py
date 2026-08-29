"""Contract sanity checks (§3/§4 of the plan: "ningún componente define su propio esquema" and
the anti-drift CI check). This is the beginning of that anti-drift test, scoped for now to:
  1. every JSON Schema under proto/ is itself a valid JSON Schema draft 2020-12 document;
  2. the example AgentManifest / ModelPolicyBundle in examples/deep-research validate against
     their respective schemas.

Full generated-code drift checking (Go structs / Python models vs proto/) lands with the code
generators themselves (roadmap.md F0, still TODO) — this test only guards the schemas' own
integrity, which is a prerequisite for that.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest
import yaml
from jsonschema import Draft202012Validator
from referencing import Registry, Resource

REPO_ROOT = Path(__file__).resolve().parents[3]
SCHEMAS_DIR = REPO_ROOT / "proto" / "schemas"
MANIFESTS_DIR = REPO_ROOT / "proto" / "manifests"
EXAMPLES_DIR = REPO_ROOT / "examples" / "deep-research"


def _all_schema_files():
    return sorted(SCHEMAS_DIR.glob("*.schema.json")) + sorted(MANIFESTS_DIR.glob("*.schema.json"))


@pytest.mark.parametrize("schema_path", _all_schema_files(), ids=lambda p: p.name)
def test_schema_is_valid_json_schema(schema_path: Path):
    schema = json.loads(schema_path.read_text())
    Draft202012Validator.check_schema(schema)


def test_deep_research_agent_manifest_matches_schema():
    schema = json.loads((MANIFESTS_DIR / "agent_manifest.schema.json").read_text())
    doc = yaml.safe_load((EXAMPLES_DIR / "agent.yaml").read_text())
    Draft202012Validator(schema).validate(doc)


def test_deep_research_model_policy_bundle_matches_schema():
    schema = json.loads((MANIFESTS_DIR / "model_policy_bundle.schema.json").read_text())
    # model_policy_bundle.schema.json $refs model_profile.schema.json by $id; the validator needs
    # a registry for that when the referenced schema isn't hosted. We inline-register it here
    # since there is no schema registry service running in unit tests.
    profile_schema = json.loads((SCHEMAS_DIR / "model_profile.schema.json").read_text())
    registry = Registry().with_resource(profile_schema["$id"], Resource.from_contents(profile_schema))
    doc = yaml.safe_load((EXAMPLES_DIR / "model_policy_bundle.yaml").read_text())
    Draft202012Validator(schema, registry=registry).validate(doc)


def test_deep_research_policy_bundle_matches_schema():
    """Guards the same file go/internal/api/tool_gateway_handlers_test.go loads directly — if this
    ever stops matching proto/manifests/policy_bundle.schema.json, that Go test would still pass
    (it only cares that Cedar can parse the cedarSource strings), so this Python-side schema check
    is the only thing that would catch a structural drift (e.g. a typo'd field name)."""
    schema = json.loads((MANIFESTS_DIR / "policy_bundle.schema.json").read_text())
    doc = yaml.safe_load((EXAMPLES_DIR / "policy_bundle.yaml").read_text())
    Draft202012Validator(schema).validate(doc)


def test_graph_all_node_kinds_fixture_matches_schema():
    """Guards the same fixture tests/integration/test_graph_runtime.py executes against a real
    Temporal server — a structural drift here (e.g. a typo'd 'kind') would otherwise only surface
    as a runtime GraphError deep inside that slower integration test."""
    schema = json.loads((SCHEMAS_DIR / "graph_spec.schema.json").read_text())
    doc = json.loads((Path(__file__).resolve().parents[1] / "fixtures" / "graph_all_node_kinds.json").read_text())
    Draft202012Validator(schema).validate(doc)
