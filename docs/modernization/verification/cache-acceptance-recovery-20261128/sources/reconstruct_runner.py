"""Reconstruct the exact historical runner from an explicit work-area argument.

The public template contains role markers, never the original host path.
This utility only renders data and verifies its historical digest. It does not
import or execute the rendered runner, load a plan, or authorize a work area.
"""

import hashlib
from pathlib import Path, PureWindowsPath
import sys

ORIGINAL_RUNNER_SHA256 = "6d16652d4893273b7e9d584920892ce5f5917622e7e242a62eb78ec824bcfb93"
RAW_MARKER = b"__APPROVED_WORK_AREA_RAW__"
ESCAPED_MARKER = b"__APPROVED_WORK_AREA_ESCAPED__"


def reconstruct(template, approved_work_area):
    """Return exact historical bytes, or fail before creating any output."""
    if template.count(RAW_MARKER) != 1 or template.count(ESCAPED_MARKER) != 2:
        raise ValueError("unexpected work-area marker counts")
    if not isinstance(approved_work_area, str) or not approved_work_area:
        raise ValueError("an explicit approved work area is required")
    if not PureWindowsPath(approved_work_area).is_absolute():
        raise ValueError("the approved work area must be an absolute Windows path")
    if any(char in approved_work_area for char in ('"', "\0", "\r", "\n")):
        raise ValueError("the work area cannot be represented in the historical literals")
    if approved_work_area.endswith("\\"):
        raise ValueError("the historical raw literal cannot end in a backslash")
    raw_value = approved_work_area.encode("utf-8")
    escaped_value = approved_work_area.replace("\\", "\\\\").encode("utf-8")
    rendered = template.replace(RAW_MARKER, raw_value).replace(ESCAPED_MARKER, escaped_value)
    if hashlib.sha256(rendered).hexdigest() != ORIGINAL_RUNNER_SHA256:
        raise ValueError("the explicit work area does not reconstruct the historical runner")
    return rendered


def main():
    if len(sys.argv) != 4:
        raise SystemExit("usage: reconstruct_runner.py TEMPLATE APPROVED_WORK_AREA NEW_OUTPUT")
    template_path, approved_work_area, output_path = sys.argv[1:]
    rendered = reconstruct(Path(template_path).read_bytes(), approved_work_area)
    with Path(output_path).open("xb") as stream:
        stream.write(rendered)
    print("Verified historical runner SHA-256: " + ORIGINAL_RUNNER_SHA256)


if __name__ == "__main__":
    main()
