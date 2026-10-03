"""Headers for every call the worker makes to an Aeon service (SEC-005 + OBS-006b).

ONE FUNCTION FOR BOTH, and that is the whole reason this module exists rather than two calls at each
site. Before SEC-005 the three outbound call sites each wrote
`inject_trace_context({"Content-Type": "application/json"})` by hand; adding a second required header
to three hand-written dicts is how one of them ends up with the trace and not the credential — and the
failure mode is a 401 from one activity and not the others, which reads as a flaky service.

THE TOKEN IS A CREDENTIAL AND NOT A BUNDLE, because the worker is a caller rather than a verifier: it
needs its own secret, not everyone's hashes. `AEON_CALLER_TOKEN` is the secret; the bundle the
gateways load holds only its SHA-256 (go/internal/auth). Since SEC-006 it can arrive as
`AEON_CALLER_TOKEN_FILE`, and either way it is removed from `os.environ` on first read — the worker
spawns the `claude` CLI with its whole environment, and that binary has no use for the credential
that lets you call the Tool Gateway as this agent (see aeon_worker.secretref).

AND AN EMPTY TOKEN SENDS NO HEADER AT ALL, rather than `Bearer `. The gateway's answer to a missing
credential is a 401 that names the variable; its answer to a malformed one would be the same 401 with
the operator believing the token was read and rejected. "Not configured" and "configured wrong" are
different problems and the first one has to look like itself.
"""
from __future__ import annotations

from aeon_observability import inject_trace_context

from aeon_worker import secretref

CALLER_TOKEN_ENV = "AEON_CALLER_TOKEN"


def caller_token() -> str:
    # resolve, NOT take: this is a library function, and scrubbing here removed the variable from
    # whatever process imported it — including pytest, which runs an in-process worker in one file
    # and spawns one with os.environ.copy() in the next. See aeon_worker.secretref's docstring; the
    # scrub belongs to aeon_worker/__main__.py, which is the process that spawns the `claude` CLI.
    return secretref.resolve(CALLER_TOKEN_ENV)


def service_headers(extra: dict[str, str] | None = None) -> dict[str, str]:
    """Content type, the trace context, and this worker's bearer credential."""
    headers = {"Content-Type": "application/json"}
    if extra:
        headers.update(extra)
    token = caller_token()
    if token:
        headers["Authorization"] = "Bearer " + token
    return inject_trace_context(headers)
