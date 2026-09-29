from __future__ import annotations

import importlib
import sys
from pathlib import Path

import pytest


def _bundle_module():
    return importlib.import_module("mcphub_em_mcp.cst_bundle")


def test_copy_project_without_sibling_data_returns_destination_project(tmp_path: Path) -> None:
    project = tmp_path / "source" / "model.cst"
    project.parent.mkdir()
    project.write_bytes(b"project")
    destination = tmp_path / "destination"
    destination.mkdir()

    copied = _bundle_module()._copy_cst_bundle(project, destination)

    assert copied == destination / "model.cst"
    assert copied.read_bytes() == b"project"
    assert not (destination / "model").exists()


def test_copy_project_with_sibling_tree_preserves_relative_bytes(tmp_path: Path) -> None:
    project = tmp_path / "source" / "model.cst"
    project.parent.mkdir()
    project.write_bytes(b"project")
    data = project.with_suffix("")
    (data / "Result").mkdir(parents=True)
    (data / "Result" / "payload.bin").write_bytes(b"payload")
    destination = tmp_path / "destination"
    destination.mkdir()

    copied = _bundle_module()._copy_cst_bundle(project, destination)

    assert copied.read_bytes() == b"project"
    assert (destination / "model" / "Result" / "payload.bin").read_bytes() == b"payload"


def test_copy_collision_and_copy2_failure_propagate_without_fallback(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    project = tmp_path / "source" / "model.cst"
    project.parent.mkdir()
    project.write_bytes(b"project")
    data = project.with_suffix("")
    data.mkdir()
    (data / "source.bin").write_bytes(b"source")
    destination = tmp_path / "destination"
    destination.mkdir()
    (destination / "model").mkdir()
    (destination / "model" / "existing.bin").write_bytes(b"existing")

    with pytest.raises(FileExistsError):
        _bundle_module()._copy_cst_bundle(project, destination)
    assert (destination / "model.cst").read_bytes() == b"project"
    assert (destination / "model" / "existing.bin").read_bytes() == b"existing"

    destination = tmp_path / "failure"
    destination.mkdir()
    module = _bundle_module()
    monkeypatch.setattr(module.shutil, "copy2", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("copy failed")))
    with pytest.raises(OSError, match="copy failed"):
        module._copy_cst_bundle(project, destination)
    assert not (destination / "model.cst").exists()


def test_neutral_bundle_import_succeeds_when_mcp_is_unavailable(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "mcp", None)
    sys.modules.pop("mcphub_em_mcp.cst_bundle", None)

    module = _bundle_module()

    assert module.__name__ == "mcphub_em_mcp.cst_bundle"
