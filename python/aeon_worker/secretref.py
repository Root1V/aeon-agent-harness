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

The same three-state rule as the Go side: absent is not an error, misconfigured is.
"""
from __future__ import annotations

import os

FILE_SUFFIX = "_FILE"

_cache: dict[str, str] = {}


class SecretRefError(RuntimeError):
    """A secret this deployment believes it provisioned cannot be resolved."""


def take(name: str) -> str:
    """Resolve `name` (or `name + "_FILE"`) and delete both from `os.environ`.

    Memoised, so a second caller gets the value rather than the empty string the environment now
    holds. Returns "" when neither is set, which means "this deployment has no such credential" and
    is a supported state — `outbound.service_headers` sends no Authorization header at all for it,
    so the gateway answers with the 401 that names the variable.
    """
    if name in _cache:
        return _cache[name]

    direct = os.environ.get(name) or ""
    path = (os.environ.get(name + FILE_SUFFIX) or "").strip()
    os.environ.pop(name, None)
    os.environ.pop(name + FILE_SUFFIX, None)

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


def reset_for_test() -> None:
    """Drop the memoised values. Only tests need this; a process resolves each secret once."""
    _cache.clear()
