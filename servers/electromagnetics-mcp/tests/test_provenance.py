from __future__ import annotations

import struct
from pathlib import Path

from mcphub_em_mcp.provenance import artifact_record, canonical_mesh_hash, dumps
from mcphub_em_mcp.slim import read_slim, write_volume_gmsh


def test_json_uses_full_precision_and_no_nonfinite() -> None:
    assert "0.10000000000000001" in dumps({"value": 0.1})


def test_artifact_record_is_relative(tmp_path: Path) -> None:
    path = tmp_path / "data.csv"
    path.write_text("x\n", encoding="utf-8")
    assert artifact_record(path, tmp_path, media_type="text/csv")["path"] == "data.csv"


def test_slim_to_gmsh_preserves_material_and_mesh_hash(tmp_path: Path) -> None:
    source = tmp_path / "3d.slim"
    header = (
        "SLIM\nformat binary_little_endian 1.4:0\n"
        "element node 4\nproperty float64 x\nproperty float64 y\nproperty float64 z\nproperty int32 uid\n"
        "element edge 0\nproperty uint32 node_index0\nproperty uint32 node_index1\nproperty int32 uid\n"
        "element triangle 0\n"
        "property uint32 node_index0\nproperty uint32 node_index1\n"
        "property uint32 node_index2\nproperty int32 uid\n"
        "element tetrahedron 1\n"
        "property uint32 node_index0\nproperty uint32 node_index1\n"
        "property uint32 node_index2\nproperty uint32 node_index3\n"
        "property int32 uid\nend\n"
    ).encode("ascii")
    nodes = b"".join(
        struct.pack("<dddi", *row) for row in ((0, 0, 0, 1), (1, 0, 0, 1), (0, 1, 0, 1), (0, 0, 1, 1))
    )
    source.write_bytes(header + nodes + struct.pack("<IIIIi", 0, 1, 2, 3, 68))
    mesh = read_slim(source)
    output = tmp_path / "mesh.msh"
    write_volume_gmsh(output, mesh, "mm")
    text = output.read_text(encoding="ascii")
    assert '3 68 "cst_uid_68"' in text
    assert mesh.mesh_hash == canonical_mesh_hash(
        ((0, 0, 0), (1, 0, 0), (0, 1, 0), (0, 0, 1)),
        ((0, 1, 2, 3, 68),),
    )
    assert "mesh_hash=" + mesh.mesh_hash in text
