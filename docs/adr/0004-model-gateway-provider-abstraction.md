# ADR-004: Model Gateway provider abstraction, including local inference

## Status
Accepted. The "Open question" below is resolved — confirmed directly against the Prometheus
platform team's own integration docs and a live dev instance — see "Resolution".

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

## Resolution: the real Prometheus platform contract

The OpenAI-compatible guess for the inference endpoint was right; the auth model was not — it's
not an API key, it's OAuth2 against a *separate* auth-service. Confirmed facts (go/internal/
providers/prometheus_inference/):

- **Two services, two base URLs.** The auth-service (token issuance) and the gateway (inference +
  model listing) are independently deployed and addressed — `PROMETHEUS_AUTH_URL` and
  `PROMETHEUS_GATEWAY_URL` in `.env`, never assumed to be the same host.
- **`POST {auth-service}/oauth2/token`, `grant_type=client_credentials`**, form-urlencoded body
  (`grant_type`, `client_id`, `client_secret`, `scope`). Response is JSON:
  `{access_token, token_type, expires_in, scope}`. `access_token` is a signed JWT the gateway
  validates on every request (`role`, `scope`, `aud: prometheus-gateway` in the payload).
- **No refresh_token grant.** Tokens are short-lived and stateless (role-based TTL — 5 min for role
  `app`, up to 3 h for `admin`). "Refreshing" is just re-POSTing `/oauth2/token` with the same
  `client_id`/`client_secret` — that credential pair, not the token, is the durable thing. Our
  `TokenSource` caches the token and proactively re-requests within `tokenRefreshMargin` (15s) of
  expiry, and reactively via `Invalidate()` on any 401.
- **`scope` gates model access.** A client's scope includes `inference:read`/`inference:stream`
  plus one `model:<id>` entry per model it may use — assigned by whoever issues the credentials
  (`POST /admin/clients`, admin-key-gated) and can change after the client is created. There is no
  static allowlist in our config for this reason: `GET /v1/models/mine` (Bearer-authenticated) is
  the live source of truth for what a given client can currently use, separate from
  `GET /v1/models` (public, every active model on the gateway, regardless of who can use it).
- **`POST /v1/chat/completions` is genuinely OpenAI-compatible** — same request shape (`model`,
  `messages[]`, `stream`, `max_tokens`, `temperature`, `tools`/`tool_choice`) and response shape
  (`choices[].message`, `usage`). No translation layer needed in `Adapter.Decide` beyond defaulting
  `model` when the caller didn't set one.
- **Base URLs are operator-specific**, not fixed dev/staging domains — this is self-hosted
  infrastructure. The dev instance used to confirm this (`http://127.0.0.1:8020` gateway,
  `http://127.0.0.1:9000` auth-service) had zero models registered at confirmation time; the
  adapter's automated test therefore runs against a `httptest` fake server implementing this exact
  contract (`prometheus_inference_test.go`), not the live instance — a live smoke test against a
  real model is a follow-up once one is registered.

Naming note unchanged: this project's inference platform is called `prometheus_inference`
everywhere in code/config to avoid collision with Prometheus-the-metrics-system
(`prometheus_metrics`), also part of this stack (OBS-001/OBS-003).

## Consequences
- No agent code ever imports a provider SDK directly; only `go/internal/providers/*` does.
- Adding a new provider (backlog.md: Bedrock, Azure AI Foundry native, Mistral, Cohere) is
  estimated S-sized specifically because this interface already exists.
