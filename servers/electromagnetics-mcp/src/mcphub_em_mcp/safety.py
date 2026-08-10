from __future__ import annotations

import os
from pathlib import Path, PureWindowsPath
from typing import Any


def require_windows() -> None:
    if os.name != "nt":
        raise RuntimeError("connected desktop automation is supported only on Windows")


def require_confirmation(confirm: bool, operation: str) -> None:
    if confirm is not True:
        raise PermissionError(f"{operation} requires confirm=true")


def existing_project_file(raw_path: str, suffixes: tuple[str, ...]) -> Path:
    path = Path(raw_path)
    if not path.is_absolute():
        raise ValueError("project_path must be absolute")
    if path.suffix.lower() not in suffixes:
        raise ValueError(f"project_path must end with one of: {', '.join(suffixes)}")
    if not path.is_file():
        raise ValueError("project_path must name an existing regular file")
    return path.resolve(strict=True)


def output_project_file(raw_path: str, suffix: str, allow_overwrite: bool) -> Path:
    path = Path(raw_path)
    if not path.is_absolute():
        raise ValueError("output_path must be absolute")
    if path.suffix.lower() != suffix:
        raise ValueError(f"output_path must end with {suffix}")
    parent = path.parent.resolve(strict=True)
    resolved = parent / path.name
    if resolved.exists() and not allow_overwrite:
        raise FileExistsError("output_path exists; set allow_overwrite=true to replace it")
    return resolved


def existing_output_root(raw_path: str) -> Path:
    # Parse with Windows semantics even when tests run on another platform. UNC
    # and device-namespace paths have drives beginning with ``\\`` and may
    # cause project data (and credentials) to be sent to a remote host.
    if PureWindowsPath(raw_path).drive.startswith("\\\\"):
        raise ValueError("output_root must be a local directory")
    path = Path(raw_path)
    if not path.is_absolute():
        raise ValueError("output_root must be absolute")
    resolved = path.resolve(strict=True)
    if not resolved.is_dir():
        raise ValueError("output_root must name an existing directory")
    return resolved


def json_safe(value: Any, *, depth: int = 0) -> Any:
    if depth > 5:
        return str(value)
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    if isinstance(value, dict):
        return {str(k): json_safe(v, depth=depth + 1) for k, v in value.items()}
    if isinstance(value, (list, tuple, set)):
        return [json_safe(v, depth=depth + 1) for v in value]
    return str(value)
