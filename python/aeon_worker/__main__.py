"""Worker process entrypoint: `python -m aeon_worker` (see deploy/compose/Dockerfile.python).

Connects to Temporal and polls the AEON_TASK_QUEUE task queue, running AgentRunWorkflow and its
Activities. Also used directly (not via compose) by tests/integration/test_crash_resume.py, which
spawns this as a real OS subprocess so it can kill it to simulate a worker crash.
"""
from __future__ import annotations

import asyncio
import logging
import os

from temporalio.client import Client
from temporalio.worker import Worker

from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_worker.workflows.agent_run import AgentRunWorkflow

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("aeon_worker")


async def main() -> None:
    address = os.environ.get("AEON_TEMPORAL_ADDRESS", "localhost:7233")
    task_queue = os.environ.get("AEON_TASK_QUEUE", "aeon-agent-run")
    namespace = os.environ.get("AEON_TEMPORAL_NAMESPACE", "default")

    logger.info("connecting to Temporal at %s (namespace=%s, task_queue=%s)", address, namespace, task_queue)
    client = await Client.connect(address, namespace=namespace)

    worker = Worker(
        client,
        task_queue=task_queue,
        workflows=[AgentRunWorkflow],
        activities=[execute_tool_activity],
    )
    logger.info("worker ready, polling task_queue=%s", task_queue)
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
