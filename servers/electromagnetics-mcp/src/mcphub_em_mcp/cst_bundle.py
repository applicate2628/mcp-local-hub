from __future__ import annotations

import shutil
from pathlib import Path


def _copy_cst_bundle(project: Path, destination: Path) -> Path:
    project_copy = destination / project.name
    shutil.copy2(project, project_copy)
    data = project.with_suffix("")
    if data.is_dir():
        shutil.copytree(data, destination / data.name)
    return project_copy
