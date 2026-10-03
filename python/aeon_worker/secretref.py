"""Resolve a secret from an environment variable or a file, and remove it from this process (SEC-006).

The Go counterpart (`go/internal/secretref`) carries the full argument; this is the same contract on
the Python side, and it exists here for one reason the Go side does not have as sharply:

**THE WORKER SPAWNS A THIRD-PARTY BINARY WITH ITS WHOLE ENVIRONMENT.** `claude_agent_sdk`'s
subprocess transport builds its child's environment as `{**os.environ, **options.env}` — read from
its source, not assumed — so every variable this process holds is handed to the `claude` CLI. Before
this module, that included `AEON_CALLER_TOKEN`: the worker's own harness credential, whose caller
entry lists `mayActAs: deep-research-general`. The CLI has no use for it and no reason to hold it.

There is no removal mechanism in `options.env` — a key can only be *overridden*, so the best it could
do is set the variable to an empty string, leaving it present. Taking the value out of `os.environ`
at startup is strictly better and covers every subprocess rather than this one SDK's.

**READING A SECRET AND SCRUBBING IT ARE TWO OPERATIONS, and the split is not tidiness — it is a
defect this module had and CI caught.** The first version scrubbed inside `resolve`, so
`outbound.caller_token()` — a library function that just builds a header — removed
`AEON_CALLER_TOKEN` from whatever process imported it. In the Python integration target that process
is pytest: `test_approval_wait_is_observable` runs a worker IN-PROCESS, which called
`caller_token()`, which emptied the test process's environment — and the next file,
`test_deep_research_workflow`, spawns its worker with `os.environ.copy()` and makes its own
authenticated HTTP calls. Both lost the credential and the run died on `HTTP Error 401` inside an
activity. Measured in CI, not reasoned about: 21 passed before, one failure after.

So "remove this secret from this process" is a statement a PROCESS makes about itself, and only an
entrypoint can make it. `resolve` reads; `take` reads and scrubs; `aeon_worker/__main__.py` — the
process that goes on to spawn the `claude` CLI — is the one that calls `take`. A library that scrubs
is a library that decides something about its host, and it decided wrong.

The same three-state rule as the Go side: absent is not an error, misconfigured is.
"""
from __future__ import annotations

import os

FILE_SUFFIX = "_FILE"

_cache: dict[str, str] = {}


class SecretRefError(RuntimeError):
    """A secret this deployment believes it provisioned cannot be resolved."""


def resolve(name: str) -> str:
    """Resolve `name` (or `name + "_FILE"`) WITHOUT touching `os.environ`.

    Memoised, so the value survives a later `take` of the same name. Returns "" when neither is set,
    which means "this deployment has no such credential" and is a supported state —
    `outbound.service_headers` sends no Authorization header at all for it, so the gateway answers
    with the 401 that names the variable.
    """
    if name in _cache:
        return _cache[name]

    direct = os.environ.get(name) or ""
    path = (os.environ.get(name + FILE_SUFFIX) or "").strip()

    if direct and path:
        raise SecretRefError(
            f"both {name} and {name}{FILE_SUFFIX} are set; remove one — with two sources for one "
            "secret, rotating either leaves the other in force and nothing fails"
        )
    if path:
        try:
            with open(path, encoding="utf-8") as fh:
                raw = fh.read()
        except OSError as exc:
            raise SecretRefError(
                f"{name}{FILE_SUFFIX} points at {path}, which cannot be read: {exc}"
            ) from exc
        # Trailing newlines only; see the Go doc comment. Anything else could be part of the secret.
        value = raw.rstrip("\r\n")
        if not value:
            raise SecretRefError(
                f"{name}{FILE_SUFFIX} points at {path}, which is empty; an unmounted volume and a "
                "credential this deployment deliberately does not have would otherwise look the same"
            )
    else:
        value = direct

    _cache[name] = value
    return value


def take(name: str) -> str:
    """`resolve`, and then remove both variables from this process's environment.

    Called by an ENTRYPOINT, not by a library: the point of removing it is that nothing this process
    spawns inherits it, and only the process itself can decide that. The resolved value stays
    memoised, so later `resolve` calls — including the ones inside `outbound` — still work.

    The pointer goes too, so a child is not even told where to look. It can still reach the path if
    the same filesystem is mounted for it, which is a mount decision and not this module's to make.
    """
    value = resolve(name)
    os.environ.pop(name, None)
    os.environ.pop(name + FILE_SUFFIX, None)
    return value


def reset_for_test() -> None:
    """Drop the memoised values. Only tests need this; a process resolves each secret once."""
    _cache.clear()
