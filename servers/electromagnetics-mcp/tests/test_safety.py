from pathlib import Path

import pytest

from mcphub_em_mcp.safety import existing_output_root, existing_project_file, require_confirmation


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


@pytest.mark.parametrize(
    "output_root",
    [
        r"\\server\share",
        r"\\?\UNC\server\share",
        "//server/share",
    ],
)
def test_output_root_rejects_windows_network_paths(output_root: str) -> None:
    with pytest.raises(ValueError, match="local directory"):
        existing_output_root(output_root)


def test_output_root_accepts_existing_local_directory(tmp_path: Path) -> None:
    assert existing_output_root(str(tmp_path)) == tmp_path.resolve()
