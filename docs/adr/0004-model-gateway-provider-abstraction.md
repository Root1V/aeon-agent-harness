# ADR-004: Model Gateway provider abstraction, including local inference

## Status
Accepted — with one open item (see "Open question" below)

## Context
The user requires first-class support for OpenAI, Anthropic, Gemini, and local inference via a
platform named "Prometheus", plus general portability so the platform is not locked to any one
provider. An `AgentManifest` must never name a concrete model.

Four properties differ sharply between cloud and local providers and would otherwise leak into
agent logic if not abstracted at the gateway:
1. **Structured output reliability** — cloud providers offer native structured outputs; many local
   serving stacks need grammar/JSON-schema-constrained decoding or a bounded repair loop.
2. **Tool calling fidelity** — native vs. emulated (JSON-in-text parsed by us), which must be
   visible in the trace, not hidden.
3. **Prompt caching** — automatic (OpenAI), explicit breakpoints (Anthropic/Gemini), or absent
   (many local setups) — the Budgeter (ADR-003) needs to know which.
4. **Cost model** — token-based vs. compute-based (GPU-seconds) — FinOps (OBS-003) needs both to
   make "cost per successful task" comparable across providers.

## Decision
- A single `Provider` interface in `go/internal/providers/` with one adapter per provider:
  `anthropic`, `openai`, `gemini`, `prometheus_inference`, and a generic `openai_compatible`
  fallback (covers vLLM/Ollama/TGI/LM Studio and is what the dev compose stack uses by default).
- `AgentManifest.spec.modelPolicy.profile` names a capability profile (e.g. `reasoning-high`);
  the binding to concrete provider/model pairs lives in a Git-versioned `ModelPolicyBundle`
  (proto/manifests/model_policy_bundle.schema.json), never in application code.
- A `provider_conformance` eval suite (same test cases: tool calling, structured output, constraint
  respect, long context, injection rejection) runs against every adapter. A provider cannot be used
  in a `Released` agent without passing it. This is what makes "changing model/provider requires
  running the relevant suites" (spec §9) mechanical rather than aspirational.
- Routing (Cedar-evaluated) can pin `data_sensitivity: restricted` to `prometheus_inference` only,
  so sensitive data never leaves the local network — this is a security requirement, not just a
  cost optimization.

## Open question
We are implementing the `prometheus_inference` adapter assuming it exposes an OpenAI-compatible
`/v1/chat/completions`-style API, since that is the common shape for local-inference serving
platforms (vLLM, TGI, Ollama all do this). **If the user's Prometheus platform instead exposes a
native/proprietary API, this adapter needs revising** — estimated 1-2 days of work once we have the
API specification. Naming note: this project's inference platform is called
`prometheus_inference` everywhere in code/config to avoid collision with Prometheus-the-metrics-
system (`prometheus_metrics`), which is also part of this stack (OBS-001/OBS-003).

## Consequences
- No agent code ever imports a provider SDK directly; only `go/internal/providers/*` does.
- Adding a new provider (backlog.md: Bedrock, Azure AI Foundry native, Mistral, Cohere) is
  estimated S-sized specifically because this interface already exists.
