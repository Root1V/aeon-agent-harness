# ADR-005: Three modes of interoperability with external agent frameworks

## Status
Accepted

## Context
The user requires the platform to be usable alongside existing agent frameworks (LangGraph, CrewAI,
OpenAI Agents SDK, Microsoft Agent Framework, Claude Agent SDK), not only via our own SDK. Adoption
research (2026) shows the realistic interoperability surface for agent platforms is not "everyone
rewrites on our SDK" but rather standard protocol surfaces (MCP, A2A, OpenAI-compatible chat
completions) that any framework can already speak.

## Decision
Three supported modes, all of which must work — not just the first:

- **Mode A — Native.** Agents defined against `aeon_sdk` directly. Full guarantees: durable replay,
  typed context, evidence, policy.
- **Mode B — Bring your own loop.** An external framework's own orchestration loop runs inside a
  Temporal Activity via a `FrameworkAdapter` (python/aeon_adapters/<framework>/), which injects: a
  model client pointed at the Model Gateway (never the raw provider), a tool executor pointed at the
  Tool Gateway (policy/dedupe/sandbox still apply), and event translation into OTel GenAI spans and
  durable approvals. Explicit limitation: the external loop is not itself deterministic, so bit-exact
  replay is not available in this mode — only replay at the granularity of that Activity. This is
  documented, not hidden.
- **Mode C — Aeon as a service, no SDK adoption required.** Three standard surfaces: an
  OpenAI-compatible chat completions endpoint on the Model Gateway (adopt by changing a
  `base_url`), an outbound MCP server exposing the governed tool catalog (Tool Gateway), and a
  published Agent Card per agent for A2A networks (F4).

Only the LangGraph `FrameworkAdapter` (Mode B) and the OpenAI-compatible endpoint (Mode C) ship in
the MVP (F2); CrewAI/OpenAI Agents SDK/MAF/Claude Agent SDK adapters and the outbound MCP
server/Agent Card move to F4, gated in backlog.md on real per-project demand.

## Consequences
- Design rule: no Aeon capability may live only inside the Python SDK. If it isn't reachable via
  gRPC, MCP, or the OpenAI-compatible endpoint, it isn't considered done.
- `examples/langgraph-interop/` exists specifically to keep Mode B honest against a real external
  framework, not just an internal mock.
