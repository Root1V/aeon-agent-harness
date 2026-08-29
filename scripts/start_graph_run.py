"""Ad-hoc script to start a GraphRunWorkflow against the real compose stack, until DX-002's `aeon
run` CLI exists. Requires the compose `core` (or `full`) profile up — `make dev` or
`docker compose -f deploy/compose/docker-compose.yml --profile core up -d` — and its Temporal port
(7233) reachable at localhost. Run from the python/ directory, so `uv run` picks up its venv:

    cd python && uv run python ../scripts/start_graph_run.py

Starts the same six-node-kind fixture the acceptance test (tests/integration/test_graph_runtime.py)
uses, on the real Temporal server, picked up by the real aeon-worker-1 container.
"""
import asyncio
import json
import uuid
from pathlib import Path

from temporalio.client import Client

REPO_ROOT = Path(__file__).resolve().parent.parent
FIXTURE = REPO_ROOT / "python" / "tests" / "fixtures" / "graph_all_node_kinds.json"


async def main() -> None:
    graph = json.loads(FIXTURE.read_text())
    run_id = str(uuid.uuid4())

    client = await Client.connect("localhost:7233", namespace="default")
    handle = await client.start_workflow(
        "GraphRunWorkflow",
        {"run_id": run_id, "graph": graph},
        id=f"graph-run-{run_id}",
        task_queue="aeon-agent-run",
    )
    print(f"started workflow_id={handle.id} run_id={handle.result_run_id or handle.first_execution_run_id}")
    print(f"watch it live at: http://localhost:8080/namespaces/default/workflows/{handle.id}")

    result = await handle.result()
    print("\n--- final result ---")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
