"""test_aeon_attributes_conform_to_argus_semconv — the guard against the drift I named and did not close.

WHY THIS EXISTS. Four of Aeon's six services are Go, Argus ships no Go SDK, and those services emit Argus's
attributes BY HAND — the names and the value shapes copied out of `argus_semconv`. I wrote exactly that risk
into the Argus channel: "el día que cambiéis un nombre o añadáis un atributo acompañante, nuestro lado Go se
queda atrás sin que nada falle". Asking them for a machine-readable artefact and not checking against the one
they already publish would have been raising a concern and then not acting on it.

`argus_semconv.attributes` IS that artefact: canonical keys, and `*_VALUES` tuples for the closed sets. So
this test reads OUR source — Go and Python — and checks every `argus.*` key we emit against theirs.

WHAT IT ALREADY CAUGHT, before any drift: I had claimed in the roadmap and in the channel that their
`Step.outcome()` is INT-011's outcome. It is not. Their `ARGUS_OUTCOME_VALUES` is
('ok','error','timeout','cancelled','degraded') — the outcome of an EXECUTION. INT-011's is
result/denied_by_policy/approval_granted/approval_denied/approval_expired — WHY a step completed. A denied
step is `argus.outcome=ok` in their vocabulary, because refusing correctly is not an error, with
`argus.guardrail=policy-denied` carrying the reason. Two different axes, and asserting an equivalence that
does not hold is how a field ends up holding values nothing can aggregate.
"""
from __future__ import annotations

import re
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[3]

# Keys Aeon emits that are NOT Argus's, declared here on purpose.
#
# An allowlist rather than a pattern match, because "starts with aeon." is a rule that lets a typo through:
# `aeon.gaurdrail` would pass a prefix check and fail silently forever. Naming them means adding one is a
# decision someone takes in a diff.
AEON_OWN_PREFIX = "aeon."

# The guardrail kinds Aeon declares, split by who owns the name.
#
# READ FROM THE GO SOURCE, not restated here — and the first version of this file DID restate them, which
# made the whole test vacuous in the one direction that matters: someone editing a constant in argus.go
# would not have been noticed by a test whose expectations lived in the test. It would have checked that
# two lists in two files agreed with Argus while saying nothing about what the gateways actually emit.
ARGUS_OWNED_KINDS = {"tool-call-budget", "tool-call-loop", "token-budget", "cost-budget"}
# `approval-required` is GONE: Argus rejected it on 2026-09-29 because setting argus.guardrail at all
# requests a two-second page, and an approval wait is normal operation. `fan-out-budget`, `policy-denied`
# and `destination-not-declared` they adopted verbatim into their own model — so two of the three are no
# longer "ours" in any meaningful sense, and the split below is about who NAMED them.
AEON_OWNED_KINDS = {"policy-denied", "fan-out-budget", "destination-not-declared"}


def _kinds_declared_in_go() -> set[str]:
    """Every guardrail kind value the Go tracing package declares."""
    text = (REPO / "go/internal/tracing/argus.go").read_text()
    block = text[text.index("// Guardrail kinds Aeon reports"):]
    block = block[: block.index("\n)")]
    return set(re.findall(r'=\s*"([a-z][a-z0-9-]*)"', block))


def _semconv():
    try:
        import argus_semconv.attributes as attrs
        import argus_semconv.guardrails as guardrails
    except ImportError:  # pragma: no cover - the package is a declared dependency of the worker
        pytest.skip("argus-obs-semconv not installed")
    return attrs, guardrails


def _argus_keys_in_source() -> dict[str, set[str]]:
    """Every `argus.<something>` string literal our source emits as an attribute key.

    Scoped to the two files that set them, rather than the whole tree, for a reason worth stating: a grep
    over everything also matches Python module paths like `argus.propagate.run`, and a test that cannot tell
    a module path from an attribute key would either fail on correct code or be loosened until it fails on
    nothing.
    """
    found: dict[str, set[str]] = {}
    for rel in ("go/internal/tracing/argus.go", "python/aeon_observability/tracing.py"):
        path = REPO / rel
        text = path.read_text()
        keys = set()
        # Only literals assigned to a constant, which is how both files declare their keys.
        for match in re.finditer(r'=\s*"(argus\.[a-z_.]+)"', text):
            keys.add(match.group(1))
        found[rel] = keys
    return found


def test_every_argus_key_aeon_emits_is_one_argus_publishes():
    attrs, _ = _semconv()
    published = {
        value
        for name in dir(attrs)
        if not name.startswith("_")
        for value in [getattr(attrs, name)]
        if isinstance(value, str) and value.startswith("argus.")
    }
    assert published, "argus_semconv.attributes published no argus.* keys — the artefact this test reads is gone"

    per_file = _argus_keys_in_source()
    assert any(per_file.values()), "found no argus.* keys in our source, so this test is checking nothing"

    for rel, keys in per_file.items():
        for key in sorted(keys):
            assert key in published, (
                f"{rel} emits {key!r} and argus_semconv does not publish it. Either it was renamed upstream "
                f"or we invented it — and a key Argus does not know is a span attribute their dashboards and "
                f"alert rules will never match. Published argus.* keys: {sorted(published)}"
            )


def test_the_go_source_declares_exactly_the_kinds_this_test_reasons_about():
    """The link between the assertions below and the code that emits them.

    Without it, every other check here compares two lists inside this file against Argus and concludes
    something about the gateways — which is the shape of a guard that passes for the wrong reason.
    """
    in_go = _kinds_declared_in_go()
    expected = ARGUS_OWNED_KINDS | AEON_OWNED_KINDS
    assert in_go == expected, (
        f"go/internal/tracing/argus.go declares {sorted(in_go)} and this test reasons about "
        f"{sorted(expected)}. Whichever moved, the other is now checking a vocabulary the gateways do not "
        "emit — added kinds are unchecked and removed ones are still asserted"
    )


def test_the_guardrail_kinds_we_borrow_match_theirs_verbatim():
    _, guardrails = _semconv()
    import inspect

    source = inspect.getsource(guardrails)
    theirs = set(re.findall(r'"([a-z]+(?:-[a-z]+)+)"', source))

    missing = ARGUS_OWNED_KINDS - theirs
    assert not missing, (
        f"we declare {sorted(missing)} as Argus's own guardrail kinds and their guardrails module no longer "
        "contains them. A breach we report under a name they retired lands in a bucket nothing reads"
    )
    collide = AEON_OWNED_KINDS & theirs
    assert not collide, (
        f"{sorted(collide)} are declared as AEON's kinds and Argus now defines them too. Two teams writing "
        "different meanings into the same value is worse than either meaning alone"
    )


def test_aeon_guardrail_kinds_follow_their_shape():
    """Hyphenated, lowercase, and comma-free — because `argus.guardrail` is a COMMA-SEPARATED LIST.

    Their AgentRun joins breaches with commas, so a kind containing one would split into two kinds that
    mean nothing. This is the kind of constraint that is obvious once stated and invisible otherwise.
    """
    for kind in sorted(AEON_OWNED_KINDS | ARGUS_OWNED_KINDS):
        assert "," not in kind, f"{kind!r} contains a comma and argus.guardrail is a comma-separated list"
        assert kind == kind.lower(), f"{kind!r} is not lowercase; theirs are"
        assert "_" not in kind, (
            f"{kind!r} uses snake_case and theirs are hyphenated — a value in the wrong style lands in a "
            "different bucket from theirs on the same dashboard, which is exactly the mistake this "
            "integration already made once"
        )


def test_their_outcome_is_not_int011s_outcome():
    """The misconception this test caught, pinned so it cannot come back.

    I claimed `Step.outcome()` was INT-011's outcome in both the roadmap and the coordination channel. It is
    not, and the assertion below is what makes the difference legible: their values describe HOW AN EXECUTION
    ENDED, ours describe WHY A STEP COMPLETED. A denied step is `ok` to them — refusing correctly is not an
    error — and the reason travels in `argus.guardrail`.
    """
    attrs, _ = _semconv()
    theirs = set(attrs.ARGUS_OUTCOME_VALUES)
    int011 = {"result", "denied_by_policy", "approval_granted", "approval_denied", "approval_expired"}

    overlap = theirs & int011
    assert not overlap, (
        f"{sorted(overlap)} appears in both vocabularies. They were distinct when this was written, and if "
        "they now overlap the two concepts need reconciling deliberately rather than by coincidence"
    )
    assert "ok" in theirs and "error" in theirs, (
        f"ARGUS_OUTCOME_VALUES = {sorted(theirs)} no longer looks like an execution outcome. If it has become "
        "a step-reason vocabulary, INT-011's outcomes should map onto it and this test should say how"
    )


def test_the_outcome_values_aeon_emits_are_in_their_closed_set():
    """Aeon's argus.outcome values must all be in ARGUS_OUTCOME_VALUES.

    `denied` and `suspended` exist in that tuple because we asked for them, which is exactly why this
    check matters: a value we needed and they added is a value a future release could rename, and the Go
    constants are hand-written.
    """
    attrs, _ = _semconv()
    theirs = set(attrs.ARGUS_OUTCOME_VALUES)

    text = (REPO / "go/internal/tracing/argus.go").read_text()
    block = text[text.index("AttrArgusOutcome") :]
    block = block[: block.index("\n)")]
    ours = set(re.findall(r'=\s*"([a-z]+)"', block)) - {"argus.outcome"}

    assert ours, "found no outcome constants in the Go source, so this check verifies nothing"
    missing = ours - theirs
    assert not missing, (
        f"Aeon declares outcome value(s) {sorted(missing)} that argus_semconv does not publish. "
        f"ARGUS_OUTCOME_VALUES = {sorted(theirs)}. An outcome outside their closed set is a value their "
        "store cannot aggregate and their dashboards will not group"
    )
    for required in ("denied", "suspended"):
        assert required in theirs, (
            f"{required!r} is no longer in ARGUS_OUTCOME_VALUES. It was added at our request — a denial "
            "counted as `ok` dirties the success rate, and a step waiting for a person recorded as `ok` is "
            "an approval wait stored as a completed success"
        )


def test_approval_required_is_not_a_guardrail():
    """The removal Argus asked for, pinned so it cannot come back by good intentions.

    Their router triggers on the PRESENCE of argus.guardrail, so marking an approval wait would page on
    every approval — alert fatigue built inside the platform that exists to prevent it. A run waiting for a
    person carries argus.outcome=suspended instead, which records the fact without paging anyone.
    """
    in_go = _kinds_declared_in_go()
    assert "approval-required" not in in_go, (
        "approval-required is back as a guardrail kind. Setting argus.guardrail requests a notification "
        "within two seconds, and an approval wait is normal operation — it belongs in argus.outcome as "
        "`suspended`, not in the guardrail field"
    )
