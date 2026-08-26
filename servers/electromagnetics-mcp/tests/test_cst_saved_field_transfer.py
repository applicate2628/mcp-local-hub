from __future__ import annotations

import importlib.util
import os
from pathlib import Path

import pytest


def _transfer():
    name = "mcphub_em_mcp.cst_saved_field_transfer"
    assert importlib.util.find_spec(name) is not None, "manifest-v2 transfer owner is missing"
    return __import__(name, fromlist=["*"])


def _bundle(root: Path) -> Path:
    root.mkdir(parents=True)
    project = root / "model.cst"
    project.write_bytes(b"project")
    result = root / "model" / "Result"
    result.mkdir(parents=True)
    (result / "3d.slim").write_bytes(b"mesh")
    (result / "meta.bin").write_bytes(b"ancillary")
    saved = result / "saved"
    saved.mkdir()
    (saved / "e1.sct").write_bytes(b"field")
    return project


def test_manifest_v2_is_complete_canonical_and_order_independent(tmp_path: Path) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    first = module.inventory_manifest_v2(project)
    second = module.inventory_manifest_v2(project, enumeration_order="reverse")
    assert first == second
    assert first.schema == "sha256-canonical-file-list-v2"
    assert [row.path for row in first.rows] == sorted(
        (row.path for row in first.rows), key=lambda value: value.encode("utf-8")
    )
    assert {row.path for row in first.rows} == {
        "model.cst",
        "model/Result/3d.slim",
        "model/Result/meta.bin",
        "model/Result/saved/e1.sct",
    }
    assert all(row.type == "regular" and row.stream == "::$DATA" for row in first.rows)


@pytest.mark.parametrize(
    "boundary",
    ["enumerate", "pre_open", "read", "copy", "source_close", "destination_enumerate", "pre_commit"],
)
@pytest.mark.parametrize("mutation", ["add", "remove", "rename", "replace", "size", "hash", "metadata"])
def test_saved_field_complete_manifest_transfer(tmp_path: Path, boundary: str, mutation: str) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    expected = module.inventory_manifest_v2(project)
    workspace = tmp_path / "workspace"
    vendor_calls: list[str] = []

    def mutate(stage: str, _relative: str | None) -> None:
        if stage != boundary:
            return
        ancillary = project.with_suffix("") / "Result" / "meta.bin"
        if mutation == "add":
            (project.with_suffix("") / "Result" / "added.bin").write_bytes(b"added")
        elif mutation == "remove":
            ancillary.unlink(missing_ok=True)
        elif mutation == "rename":
            if ancillary.exists():
                ancillary.rename(ancillary.with_name("renamed.bin"))
        elif mutation in {"replace", "hash"}:
            ancillary.write_bytes(b"changed")
        elif mutation == "size":
            ancillary.write_bytes(b"a different size")
        else:
            ancillary.touch()

    transfer = module.AuthorizedBundleTransfer(expected, boundary_hook=mutate)
    with pytest.raises(module.TransferFailure) as raised:
        transfer.execute(project, workspace, on_vendor_start=lambda _snapshot: vendor_calls.append("vendor"))
    assert raised.value.failure_id in {
        "cst_saved_field.source_changed",
        "cst_saved_field.authorized_copy_changed",
    }
    assert vendor_calls == []
    assert not workspace.exists()


def test_transfer_exact_destination_equality_commits_one_snapshot(tmp_path: Path) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    expected = module.inventory_manifest_v2(project)
    workspace = tmp_path / "workspace"
    seen = []
    snapshot = module.AuthorizedBundleTransfer(expected).execute(
        project, workspace, on_vendor_start=seen.append
    )
    assert snapshot.manifest == expected
    assert all(row.stream == "::$DATA" for row in snapshot.manifest.rows)
    assert len({(row.path, row.sha256) for row in snapshot.manifest.rows}) == len(snapshot.manifest.rows)
    assert seen == [snapshot]
    assert (workspace / "model.cst").read_bytes() == b"project"
    snapshot.settle()
    assert not workspace.exists()


def test_transfer_workspace_creation_is_transactional_and_preserves_siblings(tmp_path: Path) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    expected = module.inventory_manifest_v2(project)
    parent = tmp_path / "workspaces"
    parent.mkdir()
    sibling = parent / "sibling"
    sibling.mkdir()
    child = parent / "child"
    for stage in ("create", "permission", "identity", "initialize", "before_transfer"):
        with pytest.raises(module.TransferFailure) as raised:
            module.create_workspace_lease(child, fail_stage=stage)
        settlement = raised.value.workspace_settlement
        assert settlement is not None
        assert settlement.stage == stage
        assert type(settlement.child_removed) is bool
        assert type(settlement.lease_transferred) is bool
        assert not child.exists()
        assert sibling.is_dir()
    lease = module.create_workspace_lease(child)
    transfer = module.AuthorizedBundleTransfer(expected)
    snapshot = transfer.execute(project, lease.path, workspace_lease=lease)
    snapshot.settle()
    assert not child.exists()
    assert sibling.is_dir()


def test_saved_field_budget_boundaries(tmp_path: Path) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    expected = module.inventory_manifest_v2(project)
    budgets = (
        module.TransferBudget(max_depth=2),
        module.TransferBudget(max_entries=len(expected.rows) - 1),
        module.TransferBudget(max_files=len(expected.rows) - 1),
        module.TransferBudget(max_file_bytes=6),
        module.TransferBudget(max_total_bytes=10),
        module.TransferBudget(absolute_deadline=0.0),
    )
    for index, budget in enumerate(budgets):
        workspace = tmp_path / f"workspace-{index}"
        with pytest.raises(module.TransferFailure) as raised:
            module.AuthorizedBundleTransfer(expected, budget=budget).execute(project, workspace)
        assert raised.value.failure_id == "cst_saved_field.resource_limit_exceeded"
        assert not workspace.exists()


@pytest.mark.parametrize(
    "budget",
    [
        lambda module: module.TransferBudget(max_depth=2),
        lambda module: module.TransferBudget(max_entries=3),
        lambda module: module.TransferBudget(max_files=3),
        lambda module: module.TransferBudget(max_file_bytes=6),
        lambda module: module.TransferBudget(max_total_bytes=10),
        lambda module: module.TransferBudget(absolute_deadline=0.0),
    ],
)
def test_inventory_rejects_each_crossing_budget_before_copy(tmp_path: Path, budget) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    with pytest.raises(module.TransferFailure) as raised:
        module.inventory_manifest_v2(project, budget=budget(module))
    assert raised.value.failure_id == "cst_saved_field.resource_limit_exceeded"


@pytest.mark.parametrize(
    "mutation",
    ["missing", "extra", "path", "type", "stream", "size", "hash", "cardinality"],
)
def test_destination_mismatch_removes_workspace_with_complete_settlement(
    tmp_path: Path, mutation: str
) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    expected = module.inventory_manifest_v2(project)
    workspace = tmp_path / "workspace"
    vendor_calls: list[object] = []

    def mutate(stage: str, _relative: str | None) -> None:
        if stage != "destination_enumerate":
            return
        ancillary = workspace / "model" / "Result" / "meta.bin"
        if mutation == "missing":
            ancillary.unlink()
        elif mutation == "extra":
            (workspace / "model" / "Result" / "extra.bin").write_bytes(b"extra")
        elif mutation == "path":
            ancillary.rename(ancillary.with_name("renamed.bin"))
        elif mutation == "type":
            ancillary.unlink()
            ancillary.mkdir()
        elif mutation == "stream":
            Path(f"{ancillary}:hidden").write_bytes(b"hidden")
        elif mutation == "size":
            ancillary.write_bytes(b"different-size")
        elif mutation == "hash":
            ancillary.write_bytes(b"changed!!")
        else:
            (workspace / "model" / "Result" / "another.bin").write_bytes(b"another")

    transfer = module.AuthorizedBundleTransfer(expected, boundary_hook=mutate)
    with pytest.raises(module.TransferFailure) as raised:
        transfer.execute(project, workspace, on_vendor_start=vendor_calls.append)
    assert raised.value.failure_id == "cst_saved_field.authorized_copy_changed"
    assert raised.value.workspace_settlement is not None
    assert raised.value.workspace_settlement.stage == "destination_enumerate"
    assert raised.value.workspace_settlement.child_removed is True
    assert raised.value.workspace_settlement.lease_transferred is False
    assert vendor_calls == []
    assert not workspace.exists()


@pytest.mark.skipif(not hasattr(Path, "is_junction"), reason="requires Windows NTFS streams")
def test_destination_alternate_stream_is_rejected_and_settled(tmp_path: Path) -> None:
    module = _transfer()
    project = _bundle(tmp_path / "source")
    expected = module.inventory_manifest_v2(project)
    workspace = tmp_path / "workspace"

    def add_stream(stage: str, _relative: str | None) -> None:
        if stage == "destination_enumerate":
            Path(f"{workspace / 'model' / 'Result' / 'meta.bin'}:canary").write_bytes(b"hidden")

    with pytest.raises(module.TransferFailure) as raised:
        module.AuthorizedBundleTransfer(expected, boundary_hook=add_stream).execute(project, workspace)
    assert raised.value.failure_id == "cst_saved_field.authorized_copy_changed"
    assert raised.value.workspace_settlement is not None
    assert not workspace.exists()


@pytest.mark.skipif(os.name != "nt", reason="requires native RootDirectory-relative open")
def test_native_relative_owner_stays_on_held_root_across_name_swap(tmp_path: Path) -> None:
    module = _transfer()
    root = tmp_path / "root"
    moved = tmp_path / "moved"
    root.mkdir()
    (root / "input.bin").write_bytes(b"trusted")
    with module._relative_file_owner(root) as owner:  # noqa: SLF001
        root.rename(moved)
        root.mkdir()
        (root / "input.bin").write_bytes(b"attacker")
        source_fd = owner.open("input.bin")
        try:
            assert os.read(source_fd, 7) == b"trusted"
        finally:
            os.close(source_fd)
        destination_fd = owner.open("created.bin", create=True)
        try:
            os.write(destination_fd, b"owned")
        finally:
            os.close(destination_fd)
    assert (moved / "created.bin").read_bytes() == b"owned"
    assert not (root / "created.bin").exists()
