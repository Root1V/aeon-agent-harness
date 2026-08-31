# Deep Research — reference template (DX-003)

The platform's reference agent: a full Deep Research pipeline (Planner → Researchers → Sufficiency
Gate → Reporter → Citation Verifier, `DR-001`..`DR-005`) driven entirely by config-as-code.

## Files

- **`agent.yaml`** — the `AgentManifest`: model policy profile, allowed/denied tools, runtime
  budgets, context policy, memory policy, and the eval suites that gate its release
  (`spec.evalGates`). Never names a concrete model — only a capability profile
  (`spec.modelPolicy.profile`), per `docs/adr/0004`.
- **`policy_bundle.yaml`** — the Cedar `PolicyBundle` (`SEC-001`) authorizing exactly the tools
  `agent.yaml` allows, and forbidding `shell.*`/`external.write.*` for every principal.
- **`model_policy_bundle.yaml`** — the `ModelPolicyBundle` binding `agent.yaml`'s profile (and its
  fallback chain) to real provider/model candidates.
- **`run.py`** — a real, runnable script (`aeon_sdk`, `DX-001`) that loads the two bundles above,
  resolves the manifest's profile to candidates, and starts a real `DeepResearchWorkflow`.

These four files are checked for internal consistency by
`python/tests/unit/test_deep_research_template.py` — every allowed tool has a matching Cedar
permit, every model policy profile (and fallback) is declared in the bundle, and every `evalGates`
entry is a real suite under `evals/suites/`. Edit one; that test will catch a forgotten sibling.

## Running it

With the compose stack up (`make dev`, or at minimum `--profile core`) and a Model Gateway
provider configured (`.env` — see `.env.example`):

```bash
aeon run examples/deep-research "your query here"
```

Or directly, from `python/`:

```bash
uv run python ../examples/deep-research/run.py "your query here"
```

## Copying this as a starting point for a new agent

`aeon init <directory>` scaffolds a minimal, valid three-file starting point (no `run.py` — that
part is Deep-Research-specific). Copying this directory instead gives you a working profile with a
real pipeline behind it; `aeon init` gives you the smallest thing that validates.
