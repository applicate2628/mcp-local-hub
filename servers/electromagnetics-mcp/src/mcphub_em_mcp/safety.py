from __future__ import annotations

import ctypes
import os
from pathlib import Path
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


def _trusted_output_root() -> Path:
    configured = os.environ.get("MCPHUB_EM_OUTPUT_ROOT")
    if configured:
        root = Path(configured)
    elif os.name == "nt":
        root = Path(os.environ.get("LOCALAPPDATA", Path.home() / "AppData" / "Local"))
        root /= "mcp-local-hub/electromagnetics-jobs"
    else:
        state_home = os.environ.get("XDG_STATE_HOME")
        root = Path(state_home) if state_home else Path.home() / ".local/state"
        root /= "mcp-local-hub/electromagnetics-jobs"
    if not root.is_absolute():
        raise ValueError("the trusted electromagnetics output root must be absolute")
    if _windows_path_is_remote(str(root)):
        raise ValueError("the trusted electromagnetics output root must be on a local filesystem")
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    return root.resolve(strict=True)


def _windows_path_is_remote(path: str) -> bool:
    if path.startswith(("\\\\", "//")):
        return True
    if os.name != "nt":
        return False
    drive = Path(path).drive
    if not drive:
        return False
    # DRIVE_REMOTE (4) also detects mapped network drives, which do not have a
    # UNC spelling and therefore cannot be rejected with string checks alone.
    return ctypes.windll.kernel32.GetDriveTypeW(f"{drive}\\") == 4


def existing_output_root(raw_path: str) -> Path:
    if _windows_path_is_remote(raw_path):
        raise ValueError("output_root must be on a local filesystem")
    path = Path(raw_path)
    if not path.is_absolute():
        raise ValueError("output_root must be absolute")
    resolved = path.resolve(strict=True)
    if not resolved.is_dir():
        raise ValueError("output_root must name an existing directory")
    if _windows_path_is_remote(str(resolved)):
        raise ValueError("output_root must be on a local filesystem")
    trusted = _trusted_output_root()
    if not resolved.is_relative_to(trusted):
        raise ValueError(f"output_root must be within the trusted output root: {trusted}")
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
