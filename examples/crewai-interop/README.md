# CrewAI interop — Modo B reference example (INT-004)

Demonstrates "bring your own loop" (roadmap.md §2.5, Modo B): a real, external framework's own
agent loop — [CrewAI](https://github.com/crewAIInc/crewAI) — running inside Aeon, with its Agent
calling Aeon's real Model Gateway instead of a provider SDK directly. Same principle
`examples/langgraph-interop` (INT-001) demonstrates for LangGraph.

## What's actually here

- **`run.py`** — the runnable entrypoint (`aeon_sdk.crewai_interop.start_crewai_interop_run`).
- The crew itself lives in `python/aeon_adapters/crewai/example_crew.py` — a real, one-Agent
  `crewai.Crew` that states a one-sentence research plan for the query.
- `python/aeon_adapters/crewai/adapter.py` is the reusable `FrameworkAdapter`: `AeonLLM` (a real
  `crewai.BaseLLM` subclass) routes every model call through Aeon's Model Gateway, and
  `run_crewai_crew` runs the crew.
- `python/aeon_worker/activities/framework_adapter_activities.py::run_crewai_interop_activity`
  wraps the whole crew invocation inside **one Temporal Activity** — CrewAI's own internal
  orchestration is real, non-deterministic Python, so it can never run in workflow code
  (docs/adr/0001). `CrewAIInteropWorkflow` (`python/aeon_worker/workflows/crewai_interop_run.py`)
  is the thinnest possible wrapper around that one Activity call.
- After the crew produces its plan, the same Activity makes a direct Aeon Tool Gateway call
  (`execute_tool`, RUN-004's idempotent path) for the "research" step — deliberately *not* CrewAI's
  own tool-calling/ReAct mechanism, so this example doesn't depend on matching CrewAI's exact
  tool-call parsing format. See `aeon_adapters/crewai/example_crew.py`'s module docstring.

## A real, deliberate sync/async boundary

Unlike LangGraph's `ainvoke` (async, awaited directly), CrewAI's own `Crew.kickoff` and
`BaseLLM.call` are synchronous by design. `run_crewai_crew` runs `kickoff` inside a worker thread
(`asyncio.to_thread`), so `AeonLLM.call` can safely start its own event loop with `asyncio.run()` —
safe specifically because no event loop is already running on that worker thread. See
`aeon_adapters/crewai/adapter.py`'s module docstring for the full reasoning.

## The limitation this mode carries (by design, not a bug)

Temporal's replay reproduces this Activity's recorded input/output — not CrewAI's own internal
step-by-step trajectory. A framework running in Modo B gets Aeon's durability, policy-routed model/
tool access, and (once wired) tracing and budget accounting — it does **not** get bit-for-bit replay
of its own internal loop the way an Aeon-native graph (RUN-002) does.

## Running it

With Temporal and the worker up:

```bash
uv run python examples/crewai-interop/run.py "your query here"
```
