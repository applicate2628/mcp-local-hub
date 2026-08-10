from __future__ import annotations

import math
import re
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO

from .provenance import canonical_mesh_hash


class SlimFormatError(ValueError):
    pass


@dataclass(frozen=True)
class SlimMesh:
    nodes: tuple[tuple[float, float, float, int], ...]
    tetrahedra: tuple[tuple[int, int, int, int, int], ...]
    triangles: tuple[tuple[int, int, int, int], ...]
    trailing_bytes: int

    @property
    def mesh_hash(self) -> str:
        return canonical_mesh_hash(
            ((x, y, z) for x, y, z, _ in self.nodes),
            self.tetrahedra,
        )


_SCHEMAS = {
    "node": (("float64", "x"), ("float64", "y"), ("float64", "z"), ("int32", "uid")),
    "edge": (("uint32", "node_index0"), ("uint32", "node_index1"), ("int32", "uid")),
    "triangle": (
        ("uint32", "node_index0"),
        ("uint32", "node_index1"),
        ("uint32", "node_index2"),
        ("int32", "uid"),
    ),
    "tetrahedron": (
        ("uint32", "node_index0"),
        ("uint32", "node_index1"),
        ("uint32", "node_index2"),
        ("uint32", "node_index3"),
        ("int32", "uid"),
    ),
}
_STRUCTS = {"node": "<dddi", "edge": "<IIi", "triangle": "<IIIi", "tetrahedron": "<IIIIi"}


def _header(stream: BinaryIO) -> tuple[bytes, list[tuple[str, int]]]:
    raw = bytearray()
    while True:
        line = stream.readline()
        if not line:
            raise SlimFormatError("truncated SLIM header")
        raw.extend(line)
        if len(raw) > 8 * 1024 * 1024:
            raise SlimFormatError("SLIM header exceeds 8 MiB")
        if line == b"end\n":
            break
    try:
        lines = raw.decode("ascii", "strict").splitlines()
    except UnicodeDecodeError as exc:
        raise SlimFormatError("SLIM header is not ASCII") from exc
    if lines[:2] != ["SLIM", "format binary_little_endian 1.4:0"]:
        raise SlimFormatError("unsupported SLIM format; expected binary_little_endian 1.4:0")
    layouts: list[tuple[str, int]] = []
    current: str | None = None
    properties: list[tuple[str, str]] = []

    def finish() -> None:
        nonlocal current, properties
        if current is None:
            return
        if tuple(properties) != _SCHEMAS[current]:
            raise SlimFormatError(f"{current}: unsupported property schema")
        current = None
        properties = []

    opaque = False
    for raw_line in lines[2:-1]:
        tokens = raw_line.split("#", 1)[0].strip().lower().replace(",", " ").split()
        if not tokens:
            continue
        if tokens[:2] == ["element", "list"]:
            finish()
            opaque = True
            continue
        if opaque:
            continue
        if len(tokens) == 3 and tokens[0] == "element" and tokens[1] in _SCHEMAS:
            finish()
            current = tokens[1]
            if not re.fullmatch(r"\d+", tokens[2]):
                raise SlimFormatError(f"{current}: invalid count")
            layouts.append((current, int(tokens[2])))
        elif len(tokens) == 3 and tokens[0] == "property" and current is not None:
            properties.append((tokens[1], tokens[2]))
        else:
            raise SlimFormatError(f"unsupported SLIM header declaration: {raw_line!r}")
    finish()
    names = tuple(name for name, _ in layouts)
    if names not in {("node", "edge", "triangle"), ("node", "edge", "triangle", "tetrahedron")}:
        raise SlimFormatError("unexpected fixed SLIM element order")
    return bytes(raw), layouts


def read_slim(path: Path) -> SlimMesh:
    nodes: tuple[tuple[float, float, float, int], ...] = ()
    triangles: tuple[tuple[int, int, int, int], ...] = ()
    tetrahedra: tuple[tuple[int, int, int, int, int], ...] = ()
    with path.open("rb") as stream:
        _, layouts = _header(stream)
        for name, count in layouts:
            layout = struct.Struct(_STRUCTS[name])
            size = count * layout.size
            data = stream.read(size)
            if len(data) != size:
                raise SlimFormatError(f"truncated {name} payload")
            values = tuple(layout.iter_unpack(data))
            if name == "node":
                nodes = values
                if not nodes or any(not all(math.isfinite(v) for v in item[:3]) for item in nodes):
                    raise SlimFormatError("SLIM nodes are empty or non-finite")
            elif name == "triangle":
                triangles = values
            elif name == "tetrahedron":
                tetrahedra = values
            for row in values:
                arity = {"node": 0, "edge": 2, "triangle": 3, "tetrahedron": 4}[name]
                if arity and (
                    any(index >= len(nodes) for index in row[:arity]) or len(set(row[:arity])) != arity
                ):
                    raise SlimFormatError(f"{name}: invalid node indexes")
        tail = len(stream.read())
    return SlimMesh(nodes, tetrahedra, triangles, tail)


def write_volume_gmsh(path: Path, mesh: SlimMesh, coordinate_unit: str) -> None:
    if not mesh.tetrahedra:
        raise SlimFormatError("volume SLIM has no tetrahedra")
    if coordinate_unit not in {"m", "mm"}:
        raise ValueError("coordinate_unit must be m or mm")
    materials = sorted({tet[4] for tet in mesh.tetrahedra})
    with path.open("w", encoding="ascii", newline="\n") as stream:
        stream.write("$MeshFormat\n2.2 0 8\n$EndMeshFormat\n")
        stream.write(
            f"$Comments\ncoordinate_unit={coordinate_unit}\nmesh_hash={mesh.mesh_hash}\n$EndComments\n"
        )
        stream.write(f"$PhysicalNames\n{len(materials)}\n")
        for material in materials:
            stream.write(f'3 {material} "cst_uid_{material}"\n')
        stream.write(f"$EndPhysicalNames\n$Nodes\n{len(mesh.nodes)}\n")
        for index, (x, y, z, _) in enumerate(mesh.nodes, 1):
            stream.write(f"{index} {x:.17g} {y:.17g} {z:.17g}\n")
        stream.write(f"$EndNodes\n$Elements\n{len(mesh.tetrahedra)}\n")
        for index, (a, b, c, d, material) in enumerate(mesh.tetrahedra, 1):
            stream.write(f"{index} 4 2 {material} {material} {a + 1} {b + 1} {c + 1} {d + 1}\n")
        stream.write("$EndElements\n")


def write_surface_gmsh(path: Path, mesh: SlimMesh, coordinate_unit: str, surface_name: str) -> None:
    if mesh.tetrahedra or not mesh.triangles:
        raise SlimFormatError("surface SLIM must contain triangles and no tetrahedra")
    if coordinate_unit not in {"m", "mm"}:
        raise ValueError("coordinate_unit must be m or mm")
    with path.open("w", encoding="ascii", newline="\n") as stream:
        stream.write("$MeshFormat\n2.2 0 8\n$EndMeshFormat\n")
        stream.write(f"$Comments\ncoordinate_unit={coordinate_unit}\n$EndComments\n")
        stream.write(f'$PhysicalNames\n1\n2 1 "{surface_name}"\n$EndPhysicalNames\n')
        stream.write(f"$Nodes\n{len(mesh.nodes)}\n")
        for index, (x, y, z, _) in enumerate(mesh.nodes, 1):
            stream.write(f"{index} {x:.17g} {y:.17g} {z:.17g}\n")
        stream.write(f"$EndNodes\n$Elements\n{len(mesh.triangles)}\n")
        for index, (a, b, c, _) in enumerate(mesh.triangles, 1):
            stream.write(f"{index} 2 2 1 1 {a + 1} {b + 1} {c + 1}\n")
        stream.write("$EndElements\n")
