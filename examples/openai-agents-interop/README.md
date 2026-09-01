# OpenAI Agents SDK interop — Modo B reference example (INT-005)

Demonstrates "bring your own loop" (roadmap.md §2.5, Modo B): a real, external framework's own
agent loop — the [OpenAI Agents SDK](https://openai.github.io/openai-agents-python/) — running
inside Aeon, with its model calls going through Aeon's real Model Gateway and its tool calls going
through Aeon's real Tool Gateway, instead of a provider SDK or a tool implementation directly. Same
principle `examples/langgraph-interop` (INT-001) and `examples/crewai-interop` (INT-004)
demonstrate for LangGraph and CrewAI.

## What's actually here

- **`run.py`** — the runnable entrypoint (`aeon_sdk.openai_agents_interop.start_openai_agents_interop_run`).
- The agent itself lives in `python/aeon_adapters/openai_agents/example_agent.py` — a real, one-Agent
  `agents.Agent` instructed to call a tool once and summarize the result.
- `python/aeon_adapters/openai_agents/adapter.py` is the reusable `FrameworkAdapter`:
  `build_aeon_model` points the SDK's own `OpenAIChatCompletionsModel` at `aeon-modelgw`'s real
  `POST /v1/chat/completions` (INT-002) — no custom `Model` subclass needed, since the SDK already
  ships an integration for any OpenAI-Chat-Completions-compatible endpoint. `build_tool_gateway_tool`
  wraps a real Aeon tool as a `FunctionTool`, so the Agent's own native tool-calling loop drives real
  calls to `execute_tool` (RUN-004's idempotent path).
- `python/aeon_worker/activities/framework_adapter_activities.py::run_openai_agents_interop_activity`
  wraps the whole agent run inside **one Temporal Activity** — the SDK's own internal turn loop is
  real, non-deterministic Python, so it can never run in workflow code (docs/adr/0001).
  `OpenAIAgentsInteropWorkflow` (`python/aeon_worker/workflows/openai_agents_interop_run.py`) is the
  thinnest possible wrapper around that one Activity call.

## Two real differences from INT-001/INT-004

- **No sync/async bridge.** `agents.Runner.run` is natively a coroutine (unlike CrewAI's synchronous
  `Crew.kickoff`), so it's awaited directly from inside the Activity — see
  `aeon_adapters/openai_agents/adapter.py`'s module docstring.
- **Native tool-calling, wired for real.** Unlike CrewAI's own tool-calling (deliberately bypassed in
  `INT-004` because it isn't a stable public contract), the Agents SDK's `FunctionTool` interface is
  stable, typed and officially supported — so this example is the first Modo B integration where the
  external framework's own reasoning loop decides when to call an Aeon tool, rather than the Activity
  driving a fixed plan-then-tool-call sequence.

## The limitation this mode carries (by design, not a bug)

Temporal's replay reproduces this Activity's recorded input/output — not the SDK's own internal
step-by-step turn trajectory. A framework running in Modo B gets Aeon's durability, policy-routed
model/tool access, and (once wired) tracing and budget accounting — it does **not** get bit-for-bit
replay of its own internal loop the way an Aeon-native graph (RUN-002) does.

## Running it

With Temporal, `aeon-modelgw` and the worker up:

```bash
uv run python examples/openai-agents-interop/run.py "your query here"
```
