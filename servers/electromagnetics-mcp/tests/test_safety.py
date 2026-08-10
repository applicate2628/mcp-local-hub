from pathlib import Path

import pytest

from mcphub_em_mcp.safety import (
    _windows_path_is_remote,
    existing_output_root,
    existing_project_file,
    require_confirmation,
)


def test_confirmation_is_positive_polarity() -> None:
    with pytest.raises(PermissionError, match="confirm=true"):
        require_confirmation(False, "solve")
    require_confirmation(True, "solve")


def test_existing_project_requires_absolute_regular_file(tmp_path: Path) -> None:
    project = tmp_path / "model.aedt"
    project.write_text("fixture", encoding="utf-8")
    assert existing_project_file(str(project), (".aedt",)) == project.resolve()
    with pytest.raises(ValueError, match="absolute"):
        existing_project_file("model.aedt", (".aedt",))
    directory = tmp_path / "fake.aedt"
    directory.mkdir()
    with pytest.raises(ValueError, match="regular file"):
        existing_project_file(str(directory), (".aedt",))


def test_output_root_must_be_within_operator_configured_root(tmp_path: Path, monkeypatch) -> None:
    trusted = tmp_path / "trusted"
    allowed = trusted / "team-a"
    outside = tmp_path / "attacker"
    allowed.mkdir(parents=True)
    outside.mkdir()
    monkeypatch.setenv("MCPHUB_EM_OUTPUT_ROOT", str(trusted))

    assert existing_output_root(str(allowed)) == allowed.resolve()
    with pytest.raises(ValueError, match="within the trusted output root"):
        existing_output_root(str(outside))


@pytest.mark.parametrize("path", [r"\\server\share\jobs", "//server/share/jobs"])
def test_output_root_rejects_unc_paths_on_every_platform(path: str) -> None:
    assert _windows_path_is_remote(path)
