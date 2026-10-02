# Your first use case

Running your own agent on Aeon is **two files and one HTTP call**. No code in this repository has to
change, and nothing has to be recompiled.

Everything below is executed by `make first-use-case`, and by
`test_first_use_case_runs_end_to_end` in CI. If a command here is wrong, that test fails — which is
the only reason to trust a document like this one.

```bash
make first-use-case
```

```
1. Starting the run as first-use-case@0.1.0
   run_id = first-use-case-1790981370
2. Waiting for it to finish
   status = SUCCEEDED  tool_calls = 2
3. The tools really ran: reading the same file directly through the gateway
   allowed = True | policy = allow-first-use-case-reads
   content = '# ADR-001: Workflow/Activity determinism boundary on Temporal'
4. And a tool the policy forbids is refused, by name
   allowed = False | policy = forbid-shell-for-first-use-case | disposition = deny_step
```

## The two files

Both live in [`examples/first-use-case/`](../examples/first-use-case/). Copy the directory and edit it.

### 1. The graph — what the run does

[`graph.json`](../examples/first-use-case/graph.json). A `sequential` node with two `tool_call`
children. The node kinds are `tool_call`, `sequential`, `parallel`, `conditional`, `loop`, `subgraph`
and `fan_in` (`proto/schemas/graph_spec.schema.json`); a tool call takes `tool_name` and `tool_args`.

There is no DSL and no compile step: the graph is data in the request body, so a use case can build
one at runtime.

### 2. The policy bundle — what the run is allowed to do

[`policy_bundle.yaml`](../examples/first-use-case/policy_bundle.yaml). Cedar, **default-deny**: a tool
not named by a `permit` is refused, and the refusal carries the id of the policy that decided.

That last part is worth knowing before you need it. A call stopped by a `forbid` reports
`forbid-shell-for-first-use-case` — a human decided this. A call stopped by default-deny reports that
nothing permitted it — a configuration gap. Two different problems, and the policy id is what
distinguishes them.

### And a third, once more than you can reach the port

[`callers.yaml`](../examples/first-use-case/callers.yaml) says who may call (`SEC-005`). The committed
tokens are **public** — they are in a committed file — which is fine on a laptop and nothing else. For
anything another person can reach:

```bash
token=$(openssl rand -hex 32)          # keep this where you keep secrets
printf '%s' "$token" | shasum -a 256   # this hash goes in tokenSHA256
```

Two things in that file matter more than they look:

- **`mayActAs`** is which agents a caller may present. The Cedar principal comes from here, not from
  the request body — otherwise any caller could be judged as any agent.
- **`mayApprove`** is separate from everything else, in both directions. The worker cannot approve the
  irreversible call it is itself blocked on, and the operator who approves cannot execute tools.

## Starting a run

```bash
curl -X POST http://127.0.0.1:9404/runs \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"run_id":"my-run-1","agent_manifest_ref":"first-use-case@0.1.0","graph":{...}}'

curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9404/runs/my-run-1
```

`GET /runs/{id}` reports `status`, whether it is paused, and `budgets_consumed`. `POST
/runs/{id}/{cancel,pause,resume,approve,reject}` are the rest of the control surface.

## What you get without asking for it

- **Crash-and-resume without repeating effects.** A tool call is keyed and deduplicated, so a worker
  killed mid-write does not write twice (`python/tests/integration/test_crash_resume.py` kills a real
  process to prove it).
- **One trace per run**, from the HTTP request down to each tool call, exported over OTLP.
- **A cost ledger** attributed to the run and the agent, at `GET /finops/costs` on the model gateway.
- **Deterministic replay**: `aeon replay <run_id> --assert-identical` re-feeds a recorded history
  through the workflow code and reports whether it would still make the same decisions.

## What to know before a real workload

Three things, stated plainly because finding them yourself is worse:

1. **The base stack's worker executes nothing.** `AEON_TOOL_EXECUTION_MODE=local-ledger` records a
   tool call's intent to a file and runs no tool. That is the right default for the test suite — whose
   graphs call `artifact.write`, which is not implemented — and it is why this guide uses an override
   ([`deploy/compose/first-use-case.override.yml`](../deploy/compose/first-use-case.override.yml)) to
   point the worker at the gateway. For a real workload, copy that override.
2. **Two tools are implemented**: `repository.read` and `artifact.read`. `search.web` needs a SearXNG
   instance the public engines are not rate-limiting; `search.rag` needs the platform's embedding
   credentials; `shell.exec` exists, is sandboxed, and the reference bundle forbids it for everyone.
   Your own tools go in `go/internal/toolexec` and are registered in `go/cmd/aeon-toolgw`.
3. **Budgets stop tool calls, depth and wall-clock — not tokens or money.** `max_tool_calls`,
   `max_depth` and `deadline_seconds` are enforced; `max_tokens` and `max_cost_usd` are declared and
   not. A loop that stays inside its tool-call budget can still spend.

## Where the stack is

`make dev` brings everything up; `PROFILE=core make dev` skips the GPU-only vLLM and the observability
extras. `make ps` shows health. Run Controller `:9404`, Tool Gateway `:9403`, Model Gateway `:9402`,
control plane `:9401`, Temporal UI `:8080`.
