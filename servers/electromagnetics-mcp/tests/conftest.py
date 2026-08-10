from pathlib import Path

import pytest


@pytest.fixture(autouse=True)
def trusted_output_root(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """Keep every test's solver artifacts inside its isolated temp directory."""
    monkeypatch.setenv("MCPHUB_EM_OUTPUT_ROOT", str(tmp_path))
