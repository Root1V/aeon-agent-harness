"""One place where this worker makes a blocking HTTP call off the event loop (`RUN-007`).

WHAT THIS FIXES, and it is not a style point. Every gateway call in this worker lived inside an
`async def` Activity and used `urllib.request.urlopen(...)` directly — a **synchronous** call on the
event loop. A Temporal Python worker is one process with one loop, so each of those calls stopped
*everything* for the duration of the round trip: every other run's activities, and the worker's own
workflow-task polling. With timeouts of up to 60 seconds, one slow model call was enough to stall
every run on the worker.

HOW IT SURFACED, which is the part worth keeping. `TestA2ATaskLifecycle` failed in
`make test-go-integration` and passed 3 of 3 in isolation: its A2A task waited 15 seconds for a real
run to complete and the run never got the loop. It read as a flaky test — a timeout with no
assertion behind it — and the honest reading was the opposite: the test was reporting a real
property of the worker under concurrent load, which is exactly the property a production deployment
has.

WHY `asyncio.to_thread` AND NOT AN ASYNC CLIENT. The request semantics stay byte-identical — same
`urllib` call, same timeout, same exceptions reaching the same `except urllib.error.HTTPError`
branches — so nothing about how these activities behave changes except that they stop monopolising
the loop. Swapping in `httpx`/`aiohttp` would be a dependency and a rewrite of four error-handling
paths for no additional property.
"""
from __future__ import annotations

import asyncio
import urllib.request


def read_blocking(request: urllib.request.Request, *, timeout: float) -> bytes:
    """The blocking call itself, kept separate so the thread boundary is visible in one place."""
    with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 — fixed internal URLs
        return response.read()


async def read(request: urllib.request.Request, *, timeout: float) -> bytes:
    """Perform `request` without blocking the event loop, and return the body.

    Exceptions propagate unchanged — `urllib.error.HTTPError` included — because every caller
    distinguishes status codes to decide whether a failure is retryable.
    """
    return await asyncio.to_thread(read_blocking, request, timeout=timeout)
