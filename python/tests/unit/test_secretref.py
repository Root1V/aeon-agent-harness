"""SEC-006: a secret the worker reads must not reach the processes the worker spawns.

The subprocess here is real (`sh -c env`) rather than an assertion about `os.environ`. `os.environ`
is what the module edits, so asserting on it would only prove that `pop` pops. The question SEC-006
exists to answer is what the `claude` CLI can see — and that is decided by the environment Python
hands to `fork`/`exec`, which is what these tests read back.
"""
from __future__ import annotations

import os
import subprocess

import pytest

from aeon_worker import outbound, secretref

NAME = "AEON_TEST_SECRET"
SENTINEL = "sentinel-value-7f3a1c"  # distinctive enough to grep a whole environment for


@pytest.fixture(autouse=True)
def _clean():
    secretref.reset_for_test()
    for key in (NAME, NAME + secretref.FILE_SUFFIX):
        os.environ.pop(key, None)
    yield
    secretref.reset_for_test()
    for key in (NAME, NAME + secretref.FILE_SUFFIX):
        os.environ.pop(key, None)


def child_env_contains(needle: str) -> bool:
    out = subprocess.run(["sh", "-c", "env"], capture_output=True, text=True, check=True)
    return needle in out.stdout


def test_a_value_in_the_environment_is_scrubbed_so_children_do_not_inherit_it():
    os.environ[NAME] = SENTINEL
    assert child_env_contains(SENTINEL), (
        "negative control failed: the sentinel must be visible to a child BEFORE take(), or this "
        "test proves nothing about take()"
    )

    assert secretref.take(NAME) == SENTINEL
    assert not child_env_contains(SENTINEL), "a child process still inherits the secret after take()"


def test_a_value_in_a_file_is_never_in_the_environment_at_any_point(tmp_path):
    path = tmp_path / "secret"
    # With the trailing newline `echo` would leave, because that is the realistic file and the
    # credential has to survive it.
    path.write_text(SENTINEL + "\n")
    os.environ[NAME + secretref.FILE_SUFFIX] = str(path)

    assert not child_env_contains(SENTINEL)
    assert secretref.take(NAME) == SENTINEL
    assert not child_env_contains(SENTINEL)
    # The POINTER goes too, so a child is not even told where to look.
    assert NAME + secretref.FILE_SUFFIX not in os.environ


def test_two_sources_for_one_secret_is_refused(tmp_path):
    path = tmp_path / "secret"
    path.write_text("from-the-file")
    os.environ[NAME] = "from-the-environment"
    os.environ[NAME + secretref.FILE_SUFFIX] = str(path)

    with pytest.raises(secretref.SecretRefError) as exc:
        secretref.resolve(NAME)
    assert NAME in str(exc.value) and NAME + secretref.FILE_SUFFIX in str(exc.value)


def test_an_unreadable_secret_store_is_not_an_unconfigured_credential(tmp_path):
    """Absent, present and broken are three states; collapsing the third into the first is how an
    unmounted volume comes back as "this worker has no credential" — which the gateway answers with
    the same 401 as a deployment that was never given one."""
    os.environ[NAME + secretref.FILE_SUFFIX] = str(tmp_path / "never-mounted")
    with pytest.raises(secretref.SecretRefError):
        secretref.resolve(NAME)

    secretref.reset_for_test()
    empty = tmp_path / "empty"
    empty.write_text("\n")
    os.environ[NAME + secretref.FILE_SUFFIX] = str(empty)
    with pytest.raises(secretref.SecretRefError):
        secretref.resolve(NAME)


def test_neither_set_is_absent_and_not_an_error():
    assert secretref.resolve(NAME) == ""


def test_a_read_after_the_scrub_still_gets_the_secret():
    os.environ[NAME] = SENTINEL
    assert secretref.take(NAME) == SENTINEL
    assert NAME not in os.environ
    assert secretref.resolve(NAME) == SENTINEL, (
        "the read after the scrub got a different value: without memoisation the scrub hands the "
        "first caller a credential and every later one an empty string, in one process"
    )


def test_the_caller_token_is_resolved_through_secretref(tmp_path):
    """outbound.service_headers is the only place the worker's credential is used, so the file path
    has to work THERE and not only in the resolver."""
    secretref.reset_for_test()
    previous = os.environ.pop(outbound.CALLER_TOKEN_ENV, None)
    path = tmp_path / "caller-token"
    path.write_text("token-from-a-file\n")
    os.environ[outbound.CALLER_TOKEN_ENV + secretref.FILE_SUFFIX] = str(path)
    try:
        headers = outbound.service_headers()
        assert headers["Authorization"] == "Bearer token-from-a-file"
        assert not child_env_contains("token-from-a-file")
    finally:
        secretref.reset_for_test()
        os.environ.pop(outbound.CALLER_TOKEN_ENV + secretref.FILE_SUFFIX, None)
        if previous is not None:
            os.environ[outbound.CALLER_TOKEN_ENV] = previous


def test_building_a_header_does_not_scrub_the_callers_environment():
    """THE REGRESSION. The first version of this module scrubbed inside the read, so
    `outbound.caller_token()` — a library function that builds a header — removed AEON_CALLER_TOKEN
    from whatever process imported it.

    In the Python integration target that process is pytest: one file runs a worker IN-PROCESS (so
    the scrub fired there) and the next spawns its worker with `os.environ.copy()` and makes its own
    authenticated HTTP calls. Both lost the credential, and the symptom was `HTTP Error 401` raised
    inside a Temporal activity in a test that had nothing to do with secrets. Scrubbing is a decision
    a process makes about itself; `aeon_worker/__main__.py` makes it.
    """
    secretref.reset_for_test()
    previous = os.environ.get(outbound.CALLER_TOKEN_ENV)
    os.environ[outbound.CALLER_TOKEN_ENV] = "token-the-test-process-still-needs"
    try:
        assert outbound.service_headers()["Authorization"].endswith("token-the-test-process-still-needs")
        assert os.environ.get(outbound.CALLER_TOKEN_ENV) == "token-the-test-process-still-needs", (
            "building a header removed the credential from this process's environment: a subprocess "
            "spawned with os.environ.copy() after this point starts with no credential"
        )
        assert child_env_contains("token-the-test-process-still-needs"), (
            "a child of this process can no longer see the credential — same defect, measured the "
            "way the integration suite hit it"
        )
    finally:
        secretref.reset_for_test()
        os.environ.pop(outbound.CALLER_TOKEN_ENV, None)
        if previous is not None:
            os.environ[outbound.CALLER_TOKEN_ENV] = previous
