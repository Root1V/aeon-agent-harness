"""Worker process entrypoint: `python -m aeon_worker` (see deploy/compose/Dockerfile.python).

Connects to Temporal and polls the AEON_TASK_QUEUE task queue, running AgentRunWorkflow,
GraphRunWorkflow (RUN-002), DeepResearchWorkflow (DX-001), LangGraphInteropWorkflow (INT-001),
CrewAIInteropWorkflow (INT-004), OpenAIAgentsInteropWorkflow (INT-005), MafInteropWorkflow
(INT-006), ClaudeAgentInteropWorkflow (INT-007), and their Activities. Also used directly (not via
compose) by tests/integration/test_crash_resume.py and the Temporal-integration tests under
tests/integration/, which spawn this as a real OS subprocess (the former to kill it and simulate a
worker crash).
"""
from __future__ import annotations

import asyncio
import logging
import os

from temporalio.client import Client
from temporalio.worker import Worker

from aeon_worker.activities.deep_research_activities import build_report_activity, plan_research_activity, research_subtask_activity
from aeon_worker.activities.framework_adapter_activities import (
    run_claude_agent_interop_activity,
    run_crewai_interop_activity,
    run_langgraph_interop_activity,
    run_maf_interop_activity,
    run_openai_agents_interop_activity,
)
from aeon_worker.activities.model_activities import decide_activity
from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_observability import init_tracing
from aeon_worker.registry import WORKFLOWS

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("aeon_worker")


async def main() -> None:
    address = os.environ.get("AEON_TEMPORAL_ADDRESS", "localhost:7233")
    task_queue = os.environ.get("AEON_TASK_QUEUE", "aeon-agent-run")
    namespace = os.environ.get("AEON_TEMPORAL_NAMESPACE", "default")

    # Tracing initialised before the worker connects, so the very first Activity is already traced.
    # Never fatal: see aeon_observability.init_tracing — a worker that refuses to start because a collector
    # is unreachable would trade a diagnostic for an outage.
    init_tracing(os.environ.get("AEON_SERVICE_NAME", "aeon-worker"))

    logger.info("connecting to Temporal at %s (namespace=%s, task_queue=%s)", address, namespace, task_queue)
    client = await Client.connect(address, namespace=namespace)

    worker = Worker(
        client,
        task_queue=task_queue,
        # From aeon_worker.registry, shared with the replayer (DX-002's --assert-identical). A replayer
        # registered with a different list than the worker ran reports the difference as an unknown
        # workflow type, which looks like a corrupt history instead of a stale list.
        workflows=WORKFLOWS,
        activities=[
            execute_tool_activity,
            decide_activity,
            plan_research_activity,
            research_subtask_activity,
            build_report_activity,
            run_langgraph_interop_activity,
            run_crewai_interop_activity,
            run_openai_agents_interop_activity,
            run_maf_interop_activity,
            run_claude_agent_interop_activity,
        ],
    )
    logger.info("worker ready, polling task_queue=%s", task_queue)
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
