"""GOV-001g, the writer's half: a run's artifact is parked in ITS tenant's store or nowhere.

THE DEFECT, measured through the real gateway on 2026-10-08 before the fix. Every tenant's artifacts
lived in one flat directory, and an artifact id is
`art_ + uuid5(FIXED_NAMESPACE, "<run_id>/<name>").hex` — deterministic, with the namespace a constant
in this repository and the name a literal ("report.md" for every Deep Research run). A caller in one
tenant that had seen another tenant's run id could compute the id of its finished report and read it.

So the id being deterministic is NOT the bug — it is what makes a run able to fetch what it produced
without storing a pointer. The bug was that the id was the whole address. Now the tenant is the
directory and the id only names a file inside it.
"""
from __future__ import annotations

import importlib
from pathlib import Path

import pytest

from aeon_worker.activities import artifact_activities


@pytest.fixture
def rooted(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """Reload the module with AEON_ARTIFACT_ROOT set — it is read at import time."""
    monkeypatch.setenv("AEON_ARTIFACT_ROOT", str(tmp_path))
    module = importlib.reload(artifact_activities)
    yield module, tmp_path
    monkeypatch.delenv("AEON_ARTIFACT_ROOT", raising=False)
    importlib.reload(artifact_activities)


@pytest.mark.asyncio
async def test_an_artifact_is_parked_under_its_runs_tenant(rooted) -> None:
    module, root = rooted
    out = await module.write_artifact_activity(
        module.WriteArtifactInput(run_id="run-of-b-0001", name="report.md", content="B's report", tenant="tenant-b")
    )
    assert out.note == "", out.note
    assert out.bytes_written == len(b"B's report")

    expected = root / "tenant-b" / out.artifact_id
    assert expected.read_text() == "B's report"
    # AND NOT IN THE ROOT, which is where it used to land and where every tenant could reach it. The
    # positive assertion above would pass with a copy sitting in both places.
    assert not (root / out.artifact_id).exists(), "the artifact is also in the shared root"


@pytest.mark.asyncio
async def test_two_tenants_running_the_same_run_id_do_not_collide(rooted) -> None:
    """The id is a function of (run_id, name) ONLY, so two tenants running the same run id derive the
    same id. Before the per-tenant directory that was one file and the second write reported
    `diverged` against content it had never seen — which reads as non-determinism in your own run."""
    module, root = rooted
    first = await module.write_artifact_activity(
        module.WriteArtifactInput(run_id="shared-id", name="report.md", content="A's report", tenant="tenant-a")
    )
    second = await module.write_artifact_activity(
        module.WriteArtifactInput(run_id="shared-id", name="report.md", content="B's report", tenant="tenant-b")
    )
    assert first.artifact_id == second.artifact_id, "the premise of this test is that the ids collide"
    assert not second.diverged, "B's write was reported as diverging from A's content"
    assert (root / "tenant-a" / first.artifact_id).read_text() == "A's report"
    assert (root / "tenant-b" / second.artifact_id).read_text() == "B's report"


@pytest.mark.asyncio
@pytest.mark.parametrize("tenant", ["", "..", "../..", "Tenant-B", "tenant b", "/abs"])
async def test_a_tenant_that_is_not_a_tenant_name_parks_nothing(rooted, tenant: str) -> None:
    """REFUSED AND REPORTED, never written one level up.

    Writing it to the root would put it exactly where the shared directory used to be — readable by
    every tenant — while the run's output said it was parked successfully. And the tenant is now a
    path component, so this is the line between a name and a path: `..` is not a tenant.
    """
    module, root = rooted
    out = await module.write_artifact_activity(
        module.WriteArtifactInput(run_id="run-x", name="report.md", content="should not land", tenant=tenant)
    )
    assert out.bytes_written == 0
    assert "not a tenant name" in out.note, out.note
    # Nothing anywhere under the root: not in it, not beside it, not in a directory named after the
    # attempt. A recursive check rather than one path, because a traversal's whole point is landing
    # somewhere the obvious assertion does not look.
    assert [p for p in root.rglob("*") if p.is_file()] == [], "something was written"
