# Microsoft Agent Framework interop — Modo B reference example (INT-006)

Demonstrates "bring your own loop" (roadmap.md §2.5, Modo B): a real, external framework's own
agent loop — [Microsoft Agent Framework](https://github.com/microsoft/agent-framework) (MAF) —
running inside Aeon, with its model calls going through Aeon's real Model Gateway and its tool
calls going through Aeon's real Tool Gateway, instead of a provider SDK or a tool implementation
directly. Same principle `examples/langgraph-interop` (INT-001), `examples/crewai-interop`
(INT-004) and `examples/openai-agents-interop` (INT-005) demonstrate for LangGraph, CrewAI and the
OpenAI Agents SDK.

## What's actually here

- **`run.py`** — the runnable entrypoint (`aeon_sdk.maf_interop.start_maf_interop_run`).
- The agent itself lives in
  `python/aeon_adapters/microsoft_agent_framework/example_agent.py` — a real, one-Agent
  `agent_framework.Agent` instructed to call a tool once and summarize the result.
- `python/aeon_adapters/microsoft_agent_framework/adapter.py` is the reusable `FrameworkAdapter`:
  `build_aeon_chat_client` points MAF's own `OpenAIChatCompletionClient` (the Chat-Completions-wire
  client, deliberately not the framework's default `OpenAIChatClient`, which speaks the newer
  Responses API instead — see the adapter's module docstring) at `aeon-modelgw`'s real
  `POST /v1/chat/completions` (INT-002). `build_tool_gateway_tool` wraps a real Aeon tool as a
  `agent_framework.tool`, so the Agent's own native tool-calling loop drives real calls to
  `execute_tool` (RUN-004's idempotent path).
- `python/aeon_worker/activities/framework_adapter_activities.py::run_maf_interop_activity` wraps
  the whole agent run inside **one Temporal Activity** — MAF's own internal turn loop is real,
  non-deterministic Python, so it can never run in workflow code (docs/adr/0001).
  `MafInteropWorkflow` (`python/aeon_worker/workflows/maf_interop_run.py`) is the thinnest possible
  wrapper around that one Activity call.

## Same shape as INT-005, for the same real reasons

- **No sync/async bridge** — `agent_framework.Agent.run` is natively a coroutine.
- **Native tool-calling, wired for real** — `agent_framework.tool` is a stable, typed decorator, so
  this example (like INT-005, unlike INT-004) lets the framework's own reasoning loop decide when to
  call an Aeon tool.
- **Minimal dependency footprint, deliberately.** Only `agent-framework-core` +
  `agent-framework-openai` are installed — not the `agent-framework` meta package, which pulls in
  ~30 provider integrations (Anthropic, Bedrock, Gemini, Azure, Redis, mem0, ...) this example never
  uses. Same over-installation problem CrewAI's dependency footprint already showed (backlog.md),
  deliberately not repeated here.

## The limitation this mode carries (by design, not a bug)

Temporal's replay reproduces this Activity's recorded input/output — not MAF's own internal
step-by-step turn trajectory. A framework running in Modo B gets Aeon's durability, policy-routed
model/tool access, and (once wired) tracing and budget accounting — it does **not** get bit-for-bit
replay of its own internal loop the way an Aeon-native graph (RUN-002) does.

## Running it

With Temporal, `aeon-modelgw` and the worker up:

```bash
uv run python examples/maf-interop/run.py "your query here"
```
