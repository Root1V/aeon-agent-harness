# Aeon — Agent Harness Platform

Aeon is a self-hosted, container-first platform that gives an AI agent project the infrastructure
it always needs and always reimplements badly: a durable execution loop, typed context management,
verifiable evidence and citations, authorization enforced outside the model, hard budgets, OTel
tracing, and eval gates — without locking you into one model provider or one agent framework.

> **Status:** early scaffolding. See [roadmap.md](roadmap.md) for what's actually done (with a
> passing acceptance test) vs. stubbed vs. not started. Don't trust a feature is real until its row
> says `DONE` — that status is mechanically checked, see `make roadmap-check`.

## Why

The thesis (see the original spec this project is built from,
`Especificacion_Arnes_Agentico_AI_2026.md`, and the architecture decisions in
[docs/adr/](docs/adr/)): **the harness, not the model, determines whether an agent survives
production.** Aeon is that harness, built once, reused across projects.

## Architecture at a glance

- **Control plane (Go):** Agent/Tool/Prompt/Skill/Eval/Policy registries, Cedar-based
  authorization, approvals, ABOM.
- **Model Gateway (Go):** the only component allowed to talk to a model provider directly.
  Adapters for `anthropic`, `openai`, `gemini`, `prometheus_inference` (local inference), and a
  generic `openai_compatible` fallback — behind one `Provider` interface
  ([docs/adr/0004](docs/adr/0004-model-gateway-provider-abstraction.md)). An `AgentManifest` names
  a capability profile, never a concrete model.
- **Tool Gateway (Go):** typed tool schemas, risk classification, policy check *after* arguments
  are generated and *before* execution, idempotent execution with a dedupe table, MCP client/server.
- **Agent Workers (Python, on Temporal):** the deterministic workflow/non-deterministic activity
  split that makes crash-and-resume safe — see
  [docs/adr/0001](docs/adr/0001-temporal-determinism-boundary.md). This is proven, not aspirational:
  `python/tests/integration/test_crash_resume.py` kills a real worker process mid-write and asserts
  the resumed run does not repeat it.
- **Interoperability:** an external framework (LangGraph, CrewAI, OpenAI Agents SDK, Microsoft
  Agent Framework, Claude Agent SDK) can run *inside* Aeon as a graph node, or Aeon can be consumed
  *as a service* from outside via an OpenAI-compatible endpoint, an outbound MCP server, or an A2A
  Agent Card — see [docs/adr/0005](docs/adr/0005-framework-interoperability-modes.md).

Everything runs in containers. There is no required local Go or Python toolchain.

## Quickstart

```bash
cp .env.example .env   # fill in provider keys you have; unset ones are simply unavailable
make dev                # docker compose --profile full up -d --build
make ps                 # check everything is healthy
make test                # go test + pytest, both in throwaway containers
```

Temporal UI: http://localhost:8080 · MinIO console: http://localhost:9001 · Grafana:
http://localhost:3000 · Tempo: http://localhost:3200.

The reference agent lives in [examples/deep-research/](examples/deep-research/): an `agent.yaml`
(`AgentManifest`) and a `model_policy_bundle.yaml` binding its `reasoning-high` / `reasoning-local`
profiles to concrete providers. The Deep Research profile itself (planner, isolated researchers,
citation verifier) is not implemented yet — see roadmap.md F2.

## Repository layout

```
proto/        JSON Schema + .proto contracts — the source of truth for every cross-process type
go/           control plane, model gateway, tool gateway, CLI (cmd/aeon), provider adapters
python/       Temporal worker, context/evidence/memory layers, framework adapters, Deep Research
evals/        eval suites and datasets (EvalOps)
examples/     runnable reference agents
deploy/       docker-compose (dev/reference stack) and Helm (cluster deployment)
docs/adr/     one ADR per non-obvious architecture decision
roadmap.md    live status per feature — DONE means "has a passing named test", nothing less
backlog.md    everything deliberately out of scope right now, with an explicit entry criterion
```

## Contributing to the roadmap

1. Read [roadmap.md](roadmap.md) for the current phase and pick a `TODO` row.
2. Read the ADR(s) it references before touching the relevant boundary — most of the hard
   constraints in this codebase (determinism, policy timing, provider abstraction) are ADR-backed,
   not accidental.
3. Implement it with a named acceptance test (unit or integration) that proves the behavior, not
   just exercises the code path.
4. Flip its `roadmap.md` row to `DONE` referencing that test, in the same PR. `make roadmap-check`
   fails the build if a `DONE` row's named test doesn't actually exist in the repo.
5. If you're deferring something instead of building it, move it to `backlog.md` with a real entry
   criterion — don't leave it half-described in a PR description.

## Development without `make dev`

Each language can be developed directly if you'd rather not rebuild containers on every change:

```bash
# Go (needs a container since no local Go toolchain is assumed):
docker run --rm -v "$PWD/go:/src" -w /src golang:1.23-alpine go build ./...

# Python (uv is commonly already on a dev machine; falls back to make test-python otherwise):
cd python && uv sync --extra dev && uv run pytest
```
