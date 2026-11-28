"""Reconstruct the historical namespace delta from an explicit work-area parameter.

This data utility does not apply the patch, execute Source, or invoke Git.
The original parameter has no embedded default and is not encoded here.
"""
import argparse
import hashlib
import json
from pathlib import Path

TEMPLATE_SHA256 = "a118ee2b3454f57210a5bbfc45d181ab1bc60697205953806836b3d0332e08c6"
PATCH_SHA256 = "d2f167d9e76c06d8398d02f5bd446e08756c662323f87733ecf9ae8ebb647722"
PATCH_BYTES = 14766
MARKER = b"__APPROVED_WORK_AREA__"
MARKER_COUNT = 6


def reconstruct(template, approved_work_area):
    if hashlib.sha256(template).hexdigest() != TEMPLATE_SHA256:
        raise ValueError("the pinned publication template changed")
    if template.count(MARKER) != MARKER_COUNT:
        raise ValueError("unexpected work-area marker count")
    if not approved_work_area or any(c in approved_work_area for c in "\x00\r\n"):
        raise ValueError("an explicit historical approved work area is required")
    value = approved_work_area.encode("utf-8")
    result = template.replace(MARKER, value)
    if len(result) != PATCH_BYTES or hashlib.sha256(result).hexdigest() != PATCH_SHA256:
        raise ValueError("the supplied work area does not reconstruct the historical patch")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--approved-work-area", required=True,
                        help="operator-supplied original approved work-area value; no default")
    parser.add_argument("--output", required=True,
                        help="new patch file to create; existing files are refused")
    args = parser.parse_args()
    template = Path(__file__).with_name("0145-to-727d.patch.template").read_bytes()
    result = reconstruct(template, args.approved_work_area)
    with Path(args.output).open("xb") as stream:
        stream.write(result)
    print(json.dumps({"schema": "original-namespace-delta-render-v1",
                      "bytes": len(result), "sha256": hashlib.sha256(result).hexdigest(),
                      "source_executed": False, "patch_applied": False}))


if __name__ == "__main__":
    main()
