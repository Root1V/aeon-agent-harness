# Claude Agent SDK interop — Modo B reference example (INT-007)

Demonstrates "bring your own loop" (roadmap.md §2.5, Modo B): a real, external framework's own
agent loop — the [Claude Agent SDK](https://github.com/anthropics/claude-agent-sdk-python) —
running inside Aeon. This is the fourth and last of the `FrameworkAdapter` examples
(`langgraph-interop`, `crewai-interop`, `openai-agents-interop`, `maf-interop`), and it is
structurally different from all three of the others — read on before assuming it's the same story
again.

## What's actually here

- **`run.py`** — the runnable entrypoint (`aeon_sdk.claude_agent_interop.start_claude_agent_interop_run`).
- The system prompt lives in `python/aeon_adapters/claude_agent_sdk/example_agent.py` — the only
  thing an example gets to customize (see below for why).
- `python/aeon_adapters/claude_agent_sdk/adapter.py` is the reusable `FrameworkAdapter`. It wraps a
  real `claude_agent_sdk.ClaudeSDKClient` turn loop, which itself wraps the real `claude` (Claude
  Code) CLI as a subprocess — a genuine Node.js/npm dependency, not a Python reimplementation.
- `python/aeon_worker/activities/framework_adapter_activities.py::run_claude_agent_interop_activity`
  wraps the whole run inside **one Temporal Activity** — the CLI's own turn loop is a real, separate
  OS process, so it can never run in workflow code (docs/adr/0001).
  `ClaudeAgentInteropWorkflow` (`python/aeon_worker/workflows/claude_agent_interop_run.py`) is the
  thinnest possible wrapper around that one Activity call.

## Why this integration is a different shape from the other three

Every other `FrameworkAdapter` in this project accepts a swappable model client that gets pointed at
`aeon-modelgw`. The Claude Agent SDK has no such seam: it spawns the real Claude Code CLI, which
talks to Anthropic's Messages API directly (or whatever Bedrock/Vertex/proxy config the host's
Claude Code installation already uses) — there's no "custom Model" hook to redirect. Routing this
through Aeon would need a new Anthropic-Messages-API-compatible endpoint on `aeon-modelgw`
(`INT-002` only speaks OpenAI Chat Completions) — a real prerequisite this feature doesn't attempt,
tracked honestly in `backlog.md` instead of glossed over.

So the real Aeon-governance boundary here is on the **tool** side, not the model side — the reverse
trade-off from `INT-004` (CrewAI routed the model, bypassed native tool-calling): every built-in
Claude Code tool (Bash, Read, Write, Edit, Glob, Grep, WebFetch, WebSearch, ...) is excluded
entirely (`ClaudeAgentOptions(tools=[])`), and the only tool ever exposed is a custom in-process MCP
tool bound to Aeon's real `execute_tool` (RUN-004's idempotent path). Whatever Claude decides to do,
the only side effect it can ever cause is a real, governed Aeon tool call.

## A real, heavier infra dependency

Unlike `langgraph`/`crewai`/`openai-agents`/`agent-framework` (pure Python packages), this adapter
needs the actual `@anthropic-ai/claude-code` npm CLI on `PATH`. `deploy/compose/Dockerfile.python`
and `Makefile`'s `test-python` target both install Node.js + that CLI for this reason.

## Running it

With Temporal and the worker up, and the host's Claude Code auth already configured (a real
`ANTHROPIC_API_KEY`, or whatever the installation normally uses):

```bash
uv run python examples/claude-agent-sdk-interop/run.py "your query here"
```
