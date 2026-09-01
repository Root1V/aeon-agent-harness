"""Worker process entrypoint: `python -m aeon_worker` (see deploy/compose/Dockerfile.python).

Connects to Temporal and polls the AEON_TASK_QUEUE task queue, running AgentRunWorkflow,
GraphRunWorkflow (RUN-002), DeepResearchWorkflow (DX-001), LangGraphInteropWorkflow (INT-001),
CrewAIInteropWorkflow (INT-004), OpenAIAgentsInteropWorkflow (INT-005), MafInteropWorkflow
(INT-006), and their Activities. Also used directly (not via compose) by
tests/integration/test_crash_resume.py and the Temporal-integration tests under tests/integration/,
which spawn this as a real OS subprocess (the former to kill it and simulate a worker crash).
"""
from __future__ import annotations

import asyncio
import logging
import os

from temporalio.client import Client
from temporalio.worker import Worker

from aeon_worker.activities.deep_research_activities import build_report_activity, plan_research_activity, research_subtask_activity
from aeon_worker.activities.framework_adapter_activities import (
    run_crewai_interop_activity,
    run_langgraph_interop_activity,
    run_maf_interop_activity,
    run_openai_agents_interop_activity,
)
from aeon_worker.activities.model_activities import decide_activity
from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_worker.workflows.agent_run import AgentRunWorkflow
from aeon_worker.workflows.crewai_interop_run import CrewAIInteropWorkflow
from aeon_worker.workflows.deep_research_run import DeepResearchWorkflow
from aeon_worker.workflows.graph_run import GraphRunWorkflow
from aeon_worker.workflows.langgraph_interop_run import LangGraphInteropWorkflow
from aeon_worker.workflows.maf_interop_run import MafInteropWorkflow
from aeon_worker.workflows.openai_agents_interop_run import OpenAIAgentsInteropWorkflow

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
        workflows=[
            AgentRunWorkflow,
            GraphRunWorkflow,
            DeepResearchWorkflow,
            LangGraphInteropWorkflow,
            CrewAIInteropWorkflow,
            OpenAIAgentsInteropWorkflow,
            MafInteropWorkflow,
        ],
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
        ],
    )
    logger.info("worker ready, polling task_queue=%s", task_queue)
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
