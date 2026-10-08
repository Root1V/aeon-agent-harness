# Aeon — Agent Harness Platform

Aeon is a self-hosted, container-first platform that gives an AI agent project the infrastructure
it always needs and always reimplements badly: a durable execution loop, typed context management,
verifiable evidence and citations, authorization enforced outside the model, hard budgets, OTel
tracing, and eval gates — without locking you into one model provider or one agent framework.

> **Status:** 106 features `DONE`, each with a named acceptance test that runs against real
> infrastructure — real Postgres, real Temporal, real model providers, real money where the feature is
> about money. [roadmap.md](roadmap.md) is the index; don't trust a feature is real until its row says
> `DONE`, and that status is mechanically checked (`make roadmap-check`) against a test that exists.
> CI runs the whole thing on every push.
>
> **Not yet suitable for:** multi-tenant deployments.
> (Per-run cost, token and model-call ceilings from the agent manifest are enforced — `MDL-017`,
> `MDL-018`.) [backlog.md](backlog.md) says which of those is missing and why, with the entry
> criterion for each.
>
> **Credentials that are not yours:** every one accepts `<NAME>_FILE`, so a real secret store feeds
> the stack without the value entering any process's environment — measured: not in `docker inspect`,
> not in `/proc/<pid>/environ`, not inherited by the `claude` CLI the worker spawns. `make
> dev-secrets`. What is still plaintext, and why, is stated in
> [docs/secrets.md](docs/secrets.md) (`SEC-006`).
>
> **To run your own agent on it: [docs/your-first-use-case.md](docs/your-first-use-case.md)** — two
> files and one HTTP call, no code in this repository changes.
>
> **To run your own platform's steps on it:** a `kind: activity` graph node schedules a named Temporal
> activity on your task queue, served by your worker, with per-node timeout and retries — under Aeon's
> Cedar policy, budgets, approvals and replay (`RUN-006`). Aeon is not in the data path there; what it
> guarantees is that a governed run will not schedule work the bundle does not permit.

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
cp .env.example .env     # fill in provider keys you have; unset ones are simply unavailable
PROFILE=core make dev    # core = the harness. `make dev` alone adds vLLM, which needs a GPU
make ps                  # check everything is healthy
make first-use-case      # run a real agent end to end: see docs/your-first-use-case.md
make test                # go test + pytest, both in throwaway containers
```

Temporal UI: http://localhost:8080 · Grafana: http://localhost:3000 · Tempo: http://localhost:3200.
Son los puertos por defecto: cada uno es overridable (`AEON_TEMPORAL_UI_PORT`, `AEON_GRAFANA_PORT`,
`AEON_TEMPO_PORT`, …) y **todos atan `127.0.0.1`**, no `0.0.0.0` — ver `.env.example`.
(MinIO is in the compose file and nothing uses it yet — see backlog.md.)

Two agents to start from:

- **[examples/first-use-case/](examples/first-use-case/)** — the smallest thing that works: a graph, a
  policy bundle, a caller bundle. Copy it. Walked through in
  [docs/your-first-use-case.md](docs/your-first-use-case.md).
- **[examples/deep-research/](examples/deep-research/)** — the reference profile, and it is
  implemented: planner, isolated researchers with per-subtask budgets, a sufficiency gate, a reporter
  and a citation verifier that refuses a claim the evidence does not support. It runs against the real
  Prometheus deployment with real inference (`make test-mdl-015`, which spends real money).

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
