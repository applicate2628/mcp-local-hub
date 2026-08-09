from __future__ import annotations

from pathlib import Path, PureWindowsPath

_WINDOWS_RESERVED_NAMES = {
    "CON",
    "PRN",
    "AUX",
    "NUL",
    *(f"COM{index}" for index in range(1, 10)),
    *(f"LPT{index}" for index in range(1, 10)),
}
_WINDOWS_INVALID_CHARS = set('<>:"/\\|?*')


def sanitize_output_prefix(value: str | None, default: str) -> str:
    prefix = (value or default).strip()
    if not prefix:
        prefix = default.strip()
    if not prefix:
        raise ValueError("Output prefix must not be empty")

    path = Path(prefix)
    windows_path = PureWindowsPath(prefix)
    if (
        path.is_absolute()
        or windows_path.is_absolute()
        or windows_path.drive
        or windows_path.root
        or path.name != prefix
        or windows_path.name != prefix
        or prefix in {".", ".."}
    ):
        raise ValueError(f"Output prefix must be a filename prefix, not a path: {value!r}")

    invalid = sorted({char for char in prefix if ord(char) < 32 or char in _WINDOWS_INVALID_CHARS})
    if invalid:
        chars = "".join(invalid)
        raise ValueError(f"Output prefix contains invalid filename characters: {chars!r}")

    if prefix.rstrip(" .") != prefix:
        raise ValueError("Output prefix must not end with a space or dot")

    stem = prefix.split(".", 1)[0].upper()
    if stem in _WINDOWS_RESERVED_NAMES:
        raise ValueError(f"Output prefix uses a reserved Windows filename: {prefix!r}")

    return prefix
