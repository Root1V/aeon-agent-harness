"""Headers for every call the worker makes to an Aeon service (SEC-005 + OBS-006b).

ONE FUNCTION FOR BOTH, and that is the whole reason this module exists rather than two calls at each
site. Before SEC-005 the three outbound call sites each wrote
`inject_trace_context({"Content-Type": "application/json"})` by hand; adding a second required header
to three hand-written dicts is how one of them ends up with the trace and not the credential — and the
failure mode is a 401 from one activity and not the others, which reads as a flaky service.

THE TOKEN IS AN ENVIRONMENT VARIABLE AND NOT A FILE, because the worker is a caller rather than a
verifier: it needs its own credential, not the bundle of everyone's hashes. `AEON_CALLER_TOKEN` is the
secret; the bundle the gateways load holds only its SHA-256 (go/internal/auth).

AND AN EMPTY TOKEN SENDS NO HEADER AT ALL, rather than `Bearer `. The gateway's answer to a missing
credential is a 401 that names the variable; its answer to a malformed one would be the same 401 with
the operator believing the token was read and rejected. "Not configured" and "configured wrong" are
different problems and the first one has to look like itself.
"""
from __future__ import annotations

import os

from aeon_observability import inject_trace_context


def caller_token() -> str:
    return os.environ.get("AEON_CALLER_TOKEN", "")


def service_headers(extra: dict[str, str] | None = None) -> dict[str, str]:
    """Content type, the trace context, and this worker's bearer credential."""
    headers = {"Content-Type": "application/json"}
    if extra:
        headers.update(extra)
    token = caller_token()
    if token:
        headers["Authorization"] = "Bearer " + token
    return inject_trace_context(headers)
