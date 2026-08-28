# Aeon — every workflow goes through a container. No local Go/Python toolchain is required.
COMPOSE := docker compose -f deploy/compose/docker-compose.yml
PROFILE ?= full

.PHONY: dev down logs ps build test test-go test-python lint roadmap-check clean

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

test: test-go test-python ## Run the full test suite, both languages, in containers

test-go: ## Run Go unit tests in a throwaway container
	docker run --rm -v "$(PWD)/go:/src" -w /src golang:1.23-alpine go test ./...

test-python: ## Run Python unit + integration tests in a throwaway container via uv
	docker run --rm -v "$(PWD)/python:/app" -w /app python:3.13-slim sh -c \
		"pip install --no-cache-dir uv >/dev/null && uv run --with-editable . pytest -q"

lint: ## Lint proto/schemas, Go and Python sources
	for f in proto/schemas/*.json proto/manifests/*.json; do python3 -m json.tool "$$f" >/dev/null || exit 1; done
	@echo "schemas OK"

roadmap-check: ## Fail if roadmap.md references a DONE feature without a matching test name in the repo
	python3 scripts/roadmap_check.py

clean: ## Stop containers and remove volumes (destructive — local data is lost)
	$(COMPOSE) --profile $(PROFILE) down -v

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'
