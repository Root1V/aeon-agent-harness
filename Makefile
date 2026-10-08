# Aeon — every workflow goes through a container. No local Go/Python toolchain is required.
#
# --env-file IS LOAD-BEARING (SEC-006). Compose looks for its interpolation file in the PROJECT
# directory, which is deploy/compose/ (where the compose file lives) — not the repository root, where
# `.env` and `.env.example` are. So every `${VAR:-default}` in docker-compose.yml was resolved
# without ever reading the `.env` the documentation tells you to edit, and there are 16 of them.
#
# Measured, because the consequences do not look like a configuration problem:
#   - AEON_OTEL_ENDPOINT set in .env stayed `otel-collector:4318` in all four Go services, so
#     ".env.example says pointing the whole stack at an Argus agent is these three variables" was
#     false. That single-variable change was itself the fix for having to edit four services by hand.
#   - AEON_CALLER_TOKEN set in .env reached controlplane, modelgw and toolgw — the three services
#     that VERIFY tokens and never present one — and did NOT reach the worker, the only service that
#     needs it, which stayed on the public committed development token. Rotating the worker's
#     credential by editing .env rotated nothing.
#   - AEON_MEMORY_HMAC_KEY set in .env stayed `dev-only-insecure-memory-hmac-key-change-me`, the
#     committed default. That key is the whole basis of MEM-001/SEC-004 tamper detection: anyone who
#     knows it can forge a provenance_hmac, and the public value is in this repository.
#
# Conditional on the file existing, because `--env-file` on a missing path is an error and a fresh
# clone has no `.env` — `make dev` has to work before anything is configured.
COMPOSE := docker compose $(if $(wildcard .env),--env-file .env,) -f deploy/compose/docker-compose.yml
PROFILE ?= full

# RUN-004: the step-identity golden corpus is a THREE-TEAM contract and lives outside this repository,
# in the coordination folder. A vendored copy under proto/contracts/ is what CI checks, and a test
# compares the two byte for byte wherever the real folder is reachable — so the targets mount it when
# it exists. Without the mount that comparison self-skips, which is how a vendored copy falls behind
# while every test still passes.
#
# Conditional, because the folder is on exactly one machine. $(wildcard) also keeps `docker run -v`
# from CREATING an empty directory on a host that does not have it, which would turn "not on this
# machine" into "the contract is empty".
SHARED_CONTRACTS := $(PWD)/../../Victor/coordinacion_project/contratos
CORPUS_MOUNT := $(if $(wildcard $(SHARED_CONTRACTS)),-v "$(SHARED_CONTRACTS):/contracts:ro" -e AEON_STEP_IDENTITY_SHARED_CORPUS=/contracts/identidad-de-paso/fixtures/hashes-dorados.json,)

.PHONY: dev-secret-files dev-secrets dev down logs ps build test test-go test-go-integration test-python test-python-integration test-mdl-015 test-vrt-aeon-003 test-first-use-case first-use-case lint roadmap-check clean

dev-secret-files: ## SEC-006: write deploy/compose/secrets/* for secrets.override.yml (no values printed)
	bash scripts/secret_files.sh

dev-secrets: dev-secret-files ## SEC-006: the reference stack with its secrets delivered as FILES, not env vars
	$(COMPOSE) -f deploy/compose/secrets.override.yml --profile $(PROFILE) up -d --build

dev: ## Start the full reference stack (Temporal, Postgres, MinIO, OTel, Tempo, Grafana, gateways, worker)
	$(COMPOSE) --profile $(PROFILE) up -d --build

down: ## Stop and remove containers (volumes are preserved)
	$(COMPOSE) --profile $(PROFILE) down

logs: ## Tail logs for all services
	$(COMPOSE) --profile $(PROFILE) logs -f

ps: ## Show service status
	$(COMPOSE) --profile $(PROFILE) ps

build: ## Build all images without starting them
	$(COMPOSE) --profile $(PROFILE) build

test: test-go test-python ## Run the full test suite, both languages, in containers (Postgres-backed registry tests are skipped here — see test-go-integration)

test-go: ## Run Go tests in a throwaway container (registry Postgres tests self-skip without AEON_TEST_PG_DSN)
	# Mounts the whole repo, not just go/: some tests (e.g. the policy-engine acceptance test)
	# load config-as-code files from examples/ to exercise the real checked-in manifests. Also
	# mounts the host's Docker socket: go/internal/sandbox's tests (TOOL-003) run real, sandboxed
	# containers via the Docker Engine API — sibling containers on the host daemon, not nested
	# Docker-in-Docker. They self-skip (like the Postgres tests) if the socket isn't reachable.
	docker run --rm -v "$(PWD):/repo" -v /var/run/docker.sock:/var/run/docker.sock $(CORPUS_MOUNT) -w /repo/go golang:1.25-alpine go test ./...

test-go-integration: ## Run Go tests against real Postgres + Temporal + a real worker + OTel/Tempo (starts/stops them around the run)
	# --build no es opcional. Sin él este target levanta la imagen del worker que hubiera, así que
	# la suite puede fallar por código viejo — o, peor, PASAR probándolo. Observado las dos veces:
	# aquí con los cambios de TOOL-004, y en test-python-integration con una toolgw anterior a
	# TOOL-005 que ignoraba la idempotency_key por completo.
	# toolgw is in this list because of what its absence was doing: TestEnforcementSeamLatencyByDeployment
	# Shape and TestSeamShapesAreBothReachable both self-skip on AEON_TEST_TOOLGW_ADDR, so INT-010's
	# latency measurement had never run in any target. Found by counting what the suite skips
	# (scripts/check_skips.py), which is the whole reason that script exists.
	$(COMPOSE) --profile core --profile obs up -d --build --wait postgres temporal worker toolgw otel-collector tempo searxng
	# The docker socket, which this target did NOT mount until CI started counting skips: TOOL-003's five
	# sandbox tests were reporting "docker daemon not reachable" in the one target whose whole claim is
	# that everything is real. They ran under `make test-go`, so nothing was uncovered — but the target
	# that looks most thorough was the one skipping them.
	docker run --rm --network aeon_default -v "$(PWD):/repo" -v /var/run/docker.sock:/var/run/docker.sock $(CORPUS_MOUNT) -w /repo/go \
		-e AEON_TEST_PG_DSN="postgres://aeon:aeon@postgres:5432/aeon?sslmode=disable" \
		-e AEON_TEST_TEMPORAL_ADDRESS="temporal:7233" \
		-e AEON_TEST_TOOLGW_ADDR="toolgw:9403" \
		-e AEON_CALLER_TOKEN="$(or $(AEON_CALLER_TOKEN),dev-test-token-not-a-secret)" \
		-e AEON_TEST_OTEL_ENDPOINT="otel-collector:4318" \
		-e AEON_TEST_TEMPO_QUERY_URL="http://tempo:3200" \
		-e AEON_TEST_SEARXNG_URL="http://searxng:8080" \
		golang:1.25-alpine sh -c \
		"apk add --no-cache postgresql-client >/dev/null && until pg_isready -h postgres -U aeon >/dev/null 2>&1; do sleep 1; done && go test ./... -v"
	$(COMPOSE) --profile core --profile obs stop postgres temporal worker toolgw otel-collector tempo searxng

test-python-integration: ## Run Python tests against a real Tool Gateway + Postgres (starts/stops them around the run)
	# TOOL-004: the worker's execute_tool talks to the real aeon-toolgw over HTTP, so proving it
	# works needs the gateway actually running — a fake would test the request shape and nothing
	# about policy or deduplication, which is the whole point.
	# --build no es opcional: sin él este target levanta la imagen que hubiera, y una toolgw
	# anterior a TOOL-005 ignora la idempotency_key por completo — el test mediría código viejo.
	# OBS-001's Python half needs the observability stack too: test_end_to_end_tracing asserts that ONE
	# trace contains spans from both languages, which cannot be checked without a real collector and a real
	# Tempo to read back from. Without these variables those tests self-skip, and a self-skipping test in the
	# only target that would run it is a test nobody runs.
	# OBS-010's test needs the same two variables and no gateway: it runs its own Temporal
	# (start_local) and its own worker in-process, and reads the approval.wait records back out of
	# the real Tempo.
	$(COMPOSE) --profile core --profile obs up -d --build --wait postgres toolgw controlplane otel-collector tempo
	docker run --rm --network aeon_default -v "$(PWD):/repo" -w /repo/python \
		-e AEON_TEST_TOOLGW_ADDR="toolgw:9403" \
		-e AEON_TEST_CONTROLPLANE_ADDR="controlplane:9401" \
		-e AEON_TEST_OTEL_ENDPOINT="otel-collector:4318" \
		-e AEON_TEST_TEMPO_QUERY_URL="http://tempo:3200" \
		-e AEON_TOOL_EXECUTION_MODE=local-ledger \
		-e AEON_CALLER_TOKEN="$(or $(AEON_CALLER_TOKEN),dev-test-token-not-a-secret)" \
		-e AEON_TEST_ARTIFACT_ROOT="/repo/.artifacts" \
		python:3.13-slim sh -c \
		"pip install --no-cache-dir uv >/dev/null && uv run --with-editable '.[dev]' pytest -q -rs tests/integration/test_tool_execution_through_gateway.py tests/integration/test_end_to_end_tracing.py tests/integration/test_argus_semconv_conformance.py tests/integration/test_approval_wait_is_observable.py tests/integration/test_deep_research_workflow.py tests/integration/test_external_activity_node.py"
	$(COMPOSE) --profile core --profile obs stop postgres toolgw controlplane otel-collector tempo

test-python: ## Run Python unit + integration tests in a throwaway container via uv
	# Mounts the whole repo, not just python/: test_contracts.py and (from DR-001) the Deep
	# Research profile's Planner load JSON Schemas from proto/schemas and fixtures from examples/,
	# both outside python/. '.[dev]' pulls in pytest/pytest-asyncio/pyyaml/referencing — plain
	# '--with-editable .' only installs the package's runtime dependencies. nodejs+npm+the real
	# `claude` CLI are installed for aeon_adapters.claude_agent_sdk's tests (INT-007) — it wraps that
	# CLI as a subprocess, it does not reimplement it.
	# AEON_TOOL_EXECUTION_MODE is explicit (TOOL-004): the worker refuses to pick an execution
	# backend on its own, because the wrong guess — the file ledger, which runs nothing — looks
	# exactly like a working deployment. These tests want that ledger, and now they say so.
	docker run --rm -v "$(PWD):/repo" -w /repo/python -e AEON_TOOL_EXECUTION_MODE=local-ledger python:3.13-slim sh -c \
		"apt-get update -qq && apt-get install -y -qq --no-install-recommends nodejs npm >/dev/null && npm install -g @anthropic-ai/claude-code >/dev/null 2>&1 && pip install --no-cache-dir uv >/dev/null && uv run --with-editable '.[dev]' pytest -q"

test-mdl-015: ## Run the real-platform acceptance tests (real inference, real money): MDL-015 + OBS-003b
	# THIS TARGET DID NOT EXIST, and the test that needs it has been naming it since MDL-015:
	# test_deep_research_against_real_prometheus's own skip message said "see make test-mdl-015".
	# So the only instruction for how to run MDL-015's acceptance test pointed at nothing, and the
	# test self-skipped in every target that does exist — which means the acceptance test for a DONE
	# feature had no runner at all. Found while looking for somewhere to verify OBS-003b's real-money
	# half, which needs exactly this: a real gateway, real inference and a real ledger.
	#
	# IT SPENDS REAL MONEY (fractions of a cent) on the real Prometheus deployment, which is why it
	# is its own target and not part of `make test`. Credentials come from .env, which is gitignored
	# and never committed; see .env.example for the names.
	#
	# --build for the same reason every other integration target says so: without it this runs the
	# modelgw image that happened to be lying around, and a gateway predating OBS-003b renders no
	# per-run section — the test would report a missing attribution that is only a stale image.
	$(COMPOSE) --profile core --profile obs up -d --build --wait postgres modelgw toolgw otel-collector
	docker run --rm --network aeon_default -v "$(PWD):/repo" -w /repo/python \
		-e AEON_TEST_MODELGW_ADDR="modelgw:9402" \
		-e AEON_TEST_TOOLGW_ADDR="toolgw:9403" \
		-e AEON_TOOL_EXECUTION_MODE=local-ledger \
		-e AEON_CALLER_TOKEN="$(or $(AEON_CALLER_TOKEN),dev-worker-token-not-a-secret)" \
		python:3.13-slim sh -c \
		"pip install --no-cache-dir uv >/dev/null && uv run --with-editable '.[dev]' pytest -q -s tests/integration/test_deep_research_against_real_prometheus.py"
	$(COMPOSE) --profile core --profile obs stop postgres modelgw toolgw otel-collector

test-vrt-aeon-003: ## Run VRT-AEON-003's adapter-fidelity acceptance tests (real inference, real money)
	# Veritium's request: the prometheus_inference adapter was dropping tools, tool_choice,
	# response_format, chat_template_kwargs and any `content` that was an ARRAY OF PARTS rather than a
	# string — and dropping them silently, because the platform answers 200 to the narrowed request
	# that leaves. So an agent using native tool calling got a model that said nothing, one asking for
	# structured output got prose, and one asking about an image got an answer about no image.
	#
	# WHY ITS OWN TARGET AND NOT CI: the condition we attached when accepting the request is that
	# these run against REAL prometheus. A double asserts the shape of the request we send and nothing
	# about fidelity, and fidelity is the whole subject — every assertion here passed against the OLD
	# code when written against a double. Real inference spends real money (fractions of a cent), so
	# it is excluded from CI for the same reason as test-mdl-015.
	#
	# The gateway is mounted in-process with its REAL handler, REAL routing and the REAL adapter. A-2
	# needs no database (it is about the wire, and its ledger is left nil — which is what a gateway
	# deployed without one does); A-3's half needs a real ledger, because "not charged twice" is a
	# claim about rows and nothing else can check it.
	#
	# Postgres is reached on the HOST port (5442, deliberately non-default — see the compose file)
	# rather than from inside aeon_default, because this target runs the tests on the host: they need
	# the real platform's credentials, which live in .env and are not handed to a container.
	#
	# A-3's ceiling half is NOT here. It is in `make test-go-integration` with the rest of the suite,
	# because it needs a provider that reports exactly N tokens and real inference reports whatever it
	# reports — a ceiling test against a real model is a flaky test, not a stronger one. The split is
	# deliberate: real platform where fidelity and replay are the subject, controlled provider where
	# our own arithmetic is.
	#
	# Credentials come from .env, which is gitignored and never committed; see .env.example.
	$(COMPOSE) --profile core up -d --wait postgres
	set -a && . ./.env && set +a && cd go && \
		AEON_TEST_PG_DSN="postgres://aeon:aeon@localhost:5442/aeon?sslmode=disable" \
		go test ./internal/api/ -count=1 -v \
			-run 'TestAdapterFidelityAgainstRealPrometheus|TestIdempotencyHeaderIsNotBilledTwiceThroughTheOpenAISurface'

test-first-use-case: ## Check the on-ramp in docs/your-first-use-case.md still works
	# A document is the artefact most likely to assert something the code no longer does — this repo's
	# README claimed the Deep Research profile was unimplemented for weeks after it ran against real
	# inference. So the guide's example is executed, not proofread.
	$(COMPOSE) -f deploy/compose/first-use-case.override.yml --profile core up -d --build --wait \
		postgres temporal worker toolgw runcontroller
	docker run --rm --network aeon_default -v "$(PWD):/repo" -w /repo/python \
		-e AEON_TEST_RUNCONTROLLER_ADDR="runcontroller:9404" \
		-e AEON_TEST_TOOLGW_ADDR="toolgw:9403" \
		python:3.13-slim sh -c \
		"pip install --no-cache-dir uv >/dev/null && uv run --with-editable '.[dev]' pytest -q -rs tests/integration/test_first_use_case.py"
	$(COMPOSE) -f deploy/compose/first-use-case.override.yml --profile core stop postgres temporal worker toolgw runcontroller

first-use-case: ## Run examples/first-use-case end to end against the real gateway (docs/your-first-use-case.md)
	# The on-ramp, and it is a TARGET rather than a block of shell in a document because a documented
	# command nobody runs is a document that rots. `make test-python-integration` runs this same example
	# through test_first_use_case_runs_end_to_end, so the guide cannot drift from what works.
	$(COMPOSE) -f deploy/compose/first-use-case.override.yml --profile core up -d --build --wait \
		postgres temporal worker toolgw runcontroller
	scripts/first_use_case.sh
	@echo
	@echo "Stack still up. 'make down' when you are finished, or read docs/your-first-use-case.md for what to change next."

eval-run: ## Run an EvalSuite offline (EVAL-002): make eval-run SUITE=deep_research_core [TRIALS=3]
	# `aeon eval run` (go/cmd/aeon) shells out to this same aeon_evalops.cli entrypoint when a local
	# Python is on PATH; this target is the containerized fallback when it isn't.
	docker run --rm -v "$(PWD):/repo" -w /repo/python python:3.13-slim sh -c \
		"pip install --no-cache-dir uv >/dev/null && uv run --with-editable '.[dev]' python -m aeon_evalops.cli run $(SUITE) --trials $(or $(TRIALS),1)"

lint: ## Lint proto/schemas, Go and Python sources
	for f in proto/schemas/*.json proto/manifests/*.json; do python3 -m json.tool "$$f" >/dev/null || exit 1; done
	@echo "schemas OK"

roadmap-check: ## Fail if roadmap.md references a DONE feature without a matching test name in the repo
	python3 scripts/roadmap_check.py

clean: ## Stop containers and remove volumes (destructive — local data is lost)
	$(COMPOSE) --profile $(PROFILE) down -v

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'
