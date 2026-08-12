from __future__ import annotations

import copy
from dataclasses import replace
from pathlib import Path

import pytest


def _bundle(root: Path) -> Path:
    root.mkdir(parents=True)
    project = root / "model.cst"
    project.write_bytes(b"project")
    result = root / "model" / "Result"
    result.mkdir(parents=True)
    (result / "3d.slim").write_bytes(b"mesh")
    return project


def test_t09_red_windows_path_identity_v1_is_the_single_closed_proof_type() -> None:
    from mcphub_em_mcp import cst_saved_field_policy as module

    assert not hasattr(module, "ObjectIdentityEvidence")
    identity = module.WindowsPathIdentityV1(
        canonical_path=r"C:\allowed\model.cst",
        volume_serial=7,
        file_id="f" * 32,
        link_count=1,
        reparse=False,
        streams=("::$DATA",),
        long_name_exact=True,
        short_name_present=False,
    )

    class Provider:
        def prove(self, _path, *, kind):
            assert kind == "file"
            return identity

    assert (
        module.prove_windows_path(
            r"C:\allowed\model.cst",
            provider=Provider(),
            kind="file",
            role="project",
        )
        is identity
    )


def test_t09_red_manifest_rejects_duplicate_noncanonical_and_aggregate_drift(tmp_path: Path) -> None:
    from mcphub_em_mcp import cst_saved_field_transfer as module

    project = _bundle(tmp_path / "source")
    manifest = module.inventory_manifest_v2(project)
    with pytest.raises(module.TransferFailure):
        replace(manifest, rows=manifest.rows + (manifest.rows[0],))
    with pytest.raises(module.TransferFailure):
        replace(manifest, rows=tuple(reversed(manifest.rows)))
    with pytest.raises(module.TransferFailure):
        replace(manifest, aggregate_sha256="0" * 64)


class _Lease:
    def __init__(self) -> None:
        self.closed = False

    def hold_ancestor(self, relative):
        return f"opaque:{relative}"

    def hold_read_input(self, relative):
        return f"opaque:{relative}"

    def prepare_output(self, relative):
        return f"opaque:{relative}"

    def seal_output(self, relative):
        return relative

    def create_clean_input(self, source_relative, destination_relative, expected_sha256):
        return destination_relative

    def revalidate_all(self):
        return None

    def settle(self):
        from mcphub_em_mcp.cst_saved_field_port import VendorPathLeaseSettlement

        self.closed = True
        return VendorPathLeaseSettlement(0, 0, True, 0)


def test_t09_red_snapshot_is_one_noncopyable_lease_factory_and_blocks_early_settle(
    tmp_path: Path,
) -> None:
    from mcphub_em_mcp import cst_saved_field_transfer as module

    project = _bundle(tmp_path / "source")
    manifest = module.inventory_manifest_v2(project)
    built: list[object] = []

    def factory(capability):
        built.append(capability)
        return _Lease()

    snapshot = module.AuthorizedBundleTransfer(manifest).execute(
        project,
        tmp_path / "workspace",
        vendor_lease_factory=factory,
    )
    assert not hasattr(snapshot, "path_for_vendor")
    assert not hasattr(snapshot, "ensure_directory")
    lease = snapshot.create_vendor_path_lease()
    assert len(built) == 1
    with pytest.raises(TypeError):
        copy.copy(lease)
    with pytest.raises(module.TransferFailure):
        snapshot.create_vendor_path_lease()
    with pytest.raises(module.TransferFailure):
        snapshot.settle()
    assert lease.settle().complete is True
    snapshot.settle()
    assert snapshot.settled is True
    assert not (tmp_path / "workspace").exists()
