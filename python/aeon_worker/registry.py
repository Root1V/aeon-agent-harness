"""The single list of workflows this worker runs.

ONE LIST, TWO CONSUMERS: the worker (`aeon_worker.__main__`) and the replayer
(`aeon_worker.replay`). It lives here rather than inline in the worker because a replayer registered
with fewer workflows than the worker ran does not fail cleanly — it reports the missing one as an
unknown workflow type, which reads like a broken history rather than a stale list. The two cannot
disagree if there is only one list.
"""
from __future__ import annotations

from aeon_worker.workflows.agent_run import AgentRunWorkflow
from aeon_worker.workflows.claude_agent_interop_run import ClaudeAgentInteropWorkflow
from aeon_worker.workflows.crewai_interop_run import CrewAIInteropWorkflow
from aeon_worker.workflows.deep_research_run import DeepResearchWorkflow
from aeon_worker.workflows.graph_run import GraphRunWorkflow
from aeon_worker.workflows.langgraph_interop_run import LangGraphInteropWorkflow
from aeon_worker.workflows.maf_interop_run import MafInteropWorkflow
from aeon_worker.workflows.openai_agents_interop_run import OpenAIAgentsInteropWorkflow

WORKFLOWS: list[type] = [
    AgentRunWorkflow,
    GraphRunWorkflow,
    DeepResearchWorkflow,
    LangGraphInteropWorkflow,
    CrewAIInteropWorkflow,
    OpenAIAgentsInteropWorkflow,
    MafInteropWorkflow,
    ClaudeAgentInteropWorkflow,
]
