from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Iterable
from json.encoder import _make_iterencode
from pathlib import Path
from typing import Any


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


class FullPrecisionJSONEncoder(json.JSONEncoder):
    """Emit finite JSON numbers with the required C ``%.17g`` precision."""

    def iterencode(self, value: Any, _one_shot: bool = False) -> Iterable[str]:
        markers = {} if self.check_circular else None
        encoder = (
            json.encoder.encode_basestring_ascii if self.ensure_ascii else json.encoder.encode_basestring
        )

        def floatstr(number: float, allow_nan: bool = self.allow_nan) -> str:
            if not math.isfinite(number):
                if not allow_nan:
                    raise ValueError("non-finite numbers are forbidden in provenance JSON")
                return "NaN" if math.isnan(number) else ("Infinity" if number > 0 else "-Infinity")
            return format(number, ".17g")

        indent = self.indent
        if indent is not None and not isinstance(indent, str):
            indent = " " * indent
        iterator = _make_iterencode(
            markers,
            self.default,
            encoder,
            indent,
            floatstr,
            self.key_separator,
            self.item_separator,
            self.sort_keys,
            self.skipkeys,
            False,
        )
        return iterator(value, 0)


def dumps(value: Any) -> str:
    return (
        FullPrecisionJSONEncoder(
            ensure_ascii=False,
            allow_nan=False,
            indent=2,
            sort_keys=True,
        ).encode(value)
        + "\n"
    )


def write_json(path: Path, value: Any) -> None:
    path.write_text(dumps(value), encoding="utf-8", newline="\n")


def artifact_record(path: Path, root: Path, *, media_type: str) -> dict[str, Any]:
    resolved = path.resolve(strict=True)
    relative = resolved.relative_to(root.resolve(strict=True)).as_posix()
    return {
        "path": relative,
        "sha256": sha256_file(resolved),
        "bytes": resolved.stat().st_size,
        "media_type": media_type,
    }


def canonical_mesh_hash(
    nodes: Iterable[tuple[float, float, float]],
    tetrahedra: Iterable[tuple[int, int, int, int, int]],
) -> str:
    """Hash canonical ordered nodes and material-tagged tetrahedra."""

    digest = hashlib.sha256()
    for index, (x, y, z) in enumerate(nodes, start=1):
        digest.update(f"v {index} {x:.17g} {y:.17g} {z:.17g}\n".encode("ascii"))
    canonical_tets = sorted(
        (int(material), *sorted((int(a), int(b), int(c), int(d)))) for a, b, c, d, material in tetrahedra
    )
    for material, a, b, c, d in canonical_tets:
        digest.update(f"t {material} {a} {b} {c} {d}\n".encode("ascii"))
    return digest.hexdigest()
