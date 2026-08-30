# Aeon — every workflow goes through a container. No local Go/Python toolchain is required.
COMPOSE := docker compose -f deploy/compose/docker-compose.yml
PROFILE ?= full

.PHONY: dev down logs ps build test test-go test-go-integration test-python lint roadmap-check clean

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
	# load config-as-code files from examples/ to exercise the real checked-in manifests.
	docker run --rm -v "$(PWD):/repo" -w /repo/go golang:1.25-alpine go test ./...

test-go-integration: ## Run Go tests against real Postgres + Temporal + a real worker + OTel/Tempo (starts/stops them around the run)
	$(COMPOSE) --profile core --profile obs up -d --wait postgres temporal worker otel-collector tempo
	docker run --rm --network aeon_default -v "$(PWD):/repo" -w /repo/go \
		-e AEON_TEST_PG_DSN="postgres://aeon:aeon@postgres:5432/aeon?sslmode=disable" \
		-e AEON_TEST_TEMPORAL_ADDRESS="temporal:7233" \
		-e AEON_TEST_OTEL_ENDPOINT="otel-collector:4318" \
		-e AEON_TEST_TEMPO_QUERY_URL="http://tempo:3200" \
		golang:1.25-alpine sh -c \
		"apk add --no-cache postgresql-client >/dev/null && until pg_isready -h postgres -U aeon >/dev/null 2>&1; do sleep 1; done && go test ./... -v"
	$(COMPOSE) --profile core --profile obs stop postgres temporal worker otel-collector tempo

test-python: ## Run Python unit + integration tests in a throwaway container via uv
	# Mounts the whole repo, not just python/: test_contracts.py and (from DR-001) the Deep
	# Research profile's Planner load JSON Schemas from proto/schemas and fixtures from examples/,
	# both outside python/. '.[dev]' pulls in pytest/pytest-asyncio/pyyaml/referencing — plain
	# '--with-editable .' only installs the package's runtime dependencies.
	docker run --rm -v "$(PWD):/repo" -w /repo/python python:3.13-slim sh -c \
		"pip install --no-cache-dir uv >/dev/null && uv run --with-editable '.[dev]' pytest -q"

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
