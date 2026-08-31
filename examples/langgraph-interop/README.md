# LangGraph interop — Modo B reference example (INT-001)

Demonstrates "bring your own loop" (roadmap.md §2.5, Modo B): a real, external framework's own
agent graph — [LangGraph](https://github.com/langchain-ai/langgraph) — running inside Aeon, with
its nodes calling Aeon's real Model Gateway and Tool Gateway instead of a provider SDK or a tool
implementation directly.

## What's actually here

- **`run.py`** — the runnable entrypoint (`aeon_sdk.langgraph_interop.start_langgraph_interop_run`).
- The graph itself lives in `python/aeon_adapters/langgraph/example_graph.py` — a real, compiled
  `langgraph.graph.StateGraph` with two nodes: `plan` (calls the injected model client) and
  `research` (calls the injected tool client).
- `python/aeon_adapters/langgraph/adapter.py` is the reusable `FrameworkAdapter`: it builds the
  Aeon-bound clients and invokes the graph.
- `python/aeon_worker/activities/framework_adapter_activities.py::run_langgraph_interop_activity`
  wraps the whole graph invocation inside **one Temporal Activity** — LangGraph's own internal
  orchestration is real, non-deterministic Python, so it can never run in workflow code
  (docs/adr/0001). `LangGraphInteropWorkflow` (`python/aeon_worker/workflows/
  langgraph_interop_run.py`) is the thinnest possible wrapper around that one Activity call.

## The limitation this mode carries (by design, not a bug)

Temporal's replay reproduces this Activity's recorded input/output — not LangGraph's own internal
step-by-step trajectory. A framework running in Modo B gets Aeon's durability, policy-routed model/
tool access, and (once wired) tracing and budget accounting — it does **not** get bit-for-bit replay
of its own internal loop the way an Aeon-native graph (RUN-002) does.

## Running it

With Temporal and the worker up:

```bash
uv run python examples/langgraph-interop/run.py "your query here"
```
