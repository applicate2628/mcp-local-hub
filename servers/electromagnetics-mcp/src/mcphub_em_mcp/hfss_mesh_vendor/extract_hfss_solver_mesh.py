from __future__ import annotations

import argparse
import csv
import hashlib
import math
import re
import struct
import sys
from collections import Counter, OrderedDict
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .hfss_ngmesh_binary import (
    decode_header_hints,
    find_point_scalar_block,
    find_volume_record_table,
    id_token_value,
    inspect_ngmesh,
)
from .mesh_path_utils import sanitize_output_prefix
from .mesh_project_model import candidate_to_row, row_to_legacy_summary

PLOT_RECORD_INTS = 15
PLOT_ELEMENT_PREFIX = np.array([4, 3, 3, 3, 10], dtype=np.int32)

# AEDT mesh-plot cache order for one quadratic tetra:
# A, AB, B, AC, BC, C, AD, BD, CD, D.
# Gmsh 2.2 type 11 order is:
# A, B, C, D, AB, BC, CA, AD, BD, CD.
GMSH_LOCAL_ORDER = np.array([0, 2, 5, 9, 1, 4, 3, 6, 7, 8], dtype=np.int64)
CORNER_LOCAL_ORDER = np.array([0, 2, 5, 9], dtype=np.int64)
LINEAR_TET_EDGES = (
    (0, 1),
    (0, 2),
    (0, 3),
    (1, 2),
    (1, 3),
    (2, 3),
)
NUM_RE = re.compile(
    r"[-+]?\d+(?:\.\d*)?(?:e[-+]?\d+)?|[-+]?\.\d+(?:e[-+]?\d+)?",
    re.IGNORECASE,
)
NGMESH_COORD_XOR_MASK = int.from_bytes(bytes.fromhex("00093d0000093d00"), "little")


@dataclass(frozen=True)
class MeshCandidate:
    path: Path
    source_format: str
    header: tuple[int, ...]
    n_vertices: int
    n_elements: int
    body_counts: Counter[int]
    body_names: dict[int, str] | None = None


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Extract an HFSS solver mesh from an Ansys Electronics Desktop "
            "mesh-plot cache (fldplt/Plot*.tmp) to Gmsh 2.2 ASCII."
        )
    )
    parser.add_argument(
        "project",
        nargs="?",
        default=".",
        help="Path to a project directory, .aedt file, .aedtresults directory, or Plot*.tmp file.",
    )
    parser.add_argument(
        "--out-dir",
        default=None,
        help="Output directory. Default: <project directory>/mesh_export.",
    )
    parser.add_argument(
        "--prefix",
        default=None,
        help="Output filename prefix, not a path. Default: derived from project/results name.",
    )
    parser.add_argument(
        "--plot-cache",
        default=None,
        help="Explicit fldplt/Plot*.tmp file. Overrides automatic discovery.",
    )
    parser.add_argument(
        "--mesh-cache",
        default=None,
        help=(
            "Explicit mesh cache to export: Plot*.tmp, current.ngmesh, initial.ngmesh, "
            "or defn_native_vmesh.ngmesh. Overrides automatic discovery."
        ),
    )
    parser.add_argument(
        "--stats",
        default=None,
        help="Explicit current.stats file for validation. Default: auto-match by body counts.",
    )
    parser.add_argument(
        "--hfss-stats",
        default=None,
        help="Optional AEDT ExportMeshStats text file with object names for Gmsh PhysicalNames.",
    )
    parser.add_argument(
        "--section",
        action="append",
        default=[],
        metavar="AXIS=MM",
        help="Render an additional section, for example --section x=15 --section z=0.97.",
    )
    parser.add_argument(
        "--no-auto-sections",
        action="store_true",
        help="Do not render default x/y/z middle sections.",
    )
    parser.add_argument(
        "--no-sections",
        action="store_true",
        help="Only export the .msh and validation files; render no PNG sections.",
    )
    parser.add_argument(
        "--allow-unvalidated",
        action="store_true",
        help="Allow mesh export when no matching current.stats file is found.",
    )
    parser.add_argument(
        "--exclude-background",
        action="store_true",
        help="Exclude only the HFSS background mesh identified by body_id=0.",
    )
    return parser.parse_args()


def resolve_project_path(project_arg: str) -> tuple[Path, Path | None]:
    project = Path(project_arg).resolve()
    if not project.exists():
        raise FileNotFoundError(project)
    if project.is_file() and project.name.lower().startswith("plot") and project.suffix.lower() == ".tmp":
        return project.parent.parent.parent, project
    if project.is_file() and project.suffix.lower() == ".aedt":
        return project.with_suffix(".aedtresults"), None
    if project.is_dir() and project.name.lower().endswith(".aedtresults"):
        return project, None
    if project.is_dir():
        results_dirs = sorted(project.glob("*.aedtresults"))
        if len(results_dirs) == 1:
            return results_dirs[0], None
        if results_dirs:
            with_plot = [path for path in results_dirs if list(path.rglob("fldplt/Plot*.tmp"))]
            if len(with_plot) == 1:
                return with_plot[0], None
            if with_plot:
                newest = max(with_plot, key=lambda path: path.stat().st_mtime)
                return newest, None
        return project, None
    raise ValueError(f"Unsupported project path: {project}")


def default_prefix(results_dir: Path, plot_cache: Path | None) -> str:
    if results_dir.name.lower().endswith(".aedtresults"):
        return results_dir.name[: -len(".aedtresults")]
    if plot_cache is not None:
        return plot_cache.stem.lower()
    return "hfss"


def read_plot_candidate(path: Path) -> MeshCandidate:
    size = path.stat().st_size
    with path.open("rb") as f:
        header_data = f.read(44)
        if len(header_data) != 44:
            raise ValueError("file is too small")
        header = np.frombuffer(header_data, dtype="<i4").copy()
        n_vertices = int(header[9])
        n_elements = int(header[10])
        if n_vertices <= 0 or n_elements <= 0:
            raise ValueError("non-positive mesh sizes in header")

        element_start = 44
        coord_start = element_start + n_elements * PLOT_RECORD_INTS * 4
        coord_end = coord_start + n_vertices * 3 * 8
        tail_start = coord_end
        expected_size = tail_start + 8 + n_elements * 4
        if expected_size != size:
            raise ValueError(f"unexpected size: expected {expected_size}, got {size}")

        f.seek(element_start)
        first_record = np.frombuffer(f.read(PLOT_RECORD_INTS * 4), dtype="<i4")
        if len(first_record) != PLOT_RECORD_INTS or not np.array_equal(first_record[:5], PLOT_ELEMENT_PREFIX):
            raise ValueError("not a 10-node tetra mesh-plot cache")

        f.seek(tail_start)
        tail_header = np.frombuffer(f.read(8), dtype="<i4").copy()
        if tail_header.tolist() != [0, n_elements]:
            raise ValueError(f"unexpected body-id tail header {tail_header.tolist()}")
        body_ids = np.frombuffer(f.read(n_elements * 4), dtype="<i4").copy()

    return MeshCandidate(
        path=path,
        source_format="plot-tmp",
        header=tuple(int(value) for value in header.tolist()),
        n_vertices=n_vertices,
        n_elements=n_elements,
        body_counts=Counter(int(value) for value in body_ids.tolist()),
    )


def find_plot_candidates(results_dir: Path) -> list[MeshCandidate]:
    candidates = []
    for path in sorted(results_dir.rglob("fldplt/Plot*.tmp")):
        try:
            candidates.append(read_plot_candidate(path))
        except ValueError:
            continue
    return candidates


def find_ngmesh_hints(results_dir: Path) -> list[Path]:
    hints = []
    for path in sorted(results_dir.rglob("*.ngmesh")):
        if path.name.lower() in {"current.ngmesh", "initial.ngmesh", "defn_native_vmesh.ngmesh"}:
            hints.append(path)
    return hints


def read_ngmesh_ascii_candidate(path: Path) -> MeshCandidate:
    with path.open("rb") as f:
        head = f.read(32)
    if not head.startswith(b"unencryptedASC"):
        raise ValueError("not an ASCII ngmesh file")

    npoints: int | None = None
    n_vol_elems: int | None = None
    header_body_counts: Counter[int] = Counter()
    element_body_counts: Counter[int] = Counter()
    body_names: dict[int, str] = {}
    in_vol = False
    for raw_line in path.read_text(encoding="latin1").splitlines():
        line = raw_line.strip()
        if not line:
            continue
        parts = line.split()
        if len(parts) >= 2 and parts[0] == "npoints":
            npoints = int(parts[1])
        elif len(parts) >= 2 and parts[0] == "n_vol_elems":
            n_vol_elems = int(parts[1])
        elif parts[0] == "body_id" and "body_name" in parts and "nvelems_on_body" in parts:
            body_id = int(parts[1])
            name_index = parts.index("body_name") + 1
            count_index = parts.index("nvelems_on_body") + 1
            body_names[body_id] = parts[name_index]
            header_body_counts[body_id] = int(parts[count_index])
        elif line == "begin_vol_element_data":
            in_vol = True
        elif line == "end_vol_element_data":
            in_vol = False
        elif in_vol and parts and parts[0] == "veid":
            body_id = int(parts[parts.index("body_id") + 1])
            element_body_counts[body_id] += 1

    if npoints is None or n_vol_elems is None:
        raise ValueError("ASCII ngmesh header has no npoints/n_vol_elems")
    body_counts = element_body_counts or header_body_counts
    if sum(body_counts.values()) != n_vol_elems:
        raise ValueError(
            f"ASCII ngmesh body counts do not match n_vol_elems: {sum(body_counts.values())} != {n_vol_elems}"
        )
    return MeshCandidate(
        path=path,
        source_format="ngmesh-ascii",
        header=(),
        n_vertices=npoints,
        n_elements=n_vol_elems,
        body_counts=body_counts,
        body_names=body_names,
    )


def find_ngmesh_ascii_candidates(results_dir: Path) -> list[MeshCandidate]:
    candidates = []
    for path in find_ngmesh_hints(results_dir):
        try:
            candidates.append(read_ngmesh_ascii_candidate(path))
        except ValueError:
            continue
    return candidates


def read_ngmesh_binary_candidate(path: Path) -> MeshCandidate:
    stats_path = sibling_stats_path(path)
    if stats_path is None:
        raise ValueError("binary ngmesh has no sibling stats file")
    stats = parse_current_stats(stats_path)
    payload = inspect_ngmesh(path, stats_path)
    header = payload.get("header")
    point_block = payload.get("point_scalar_block")
    volume_table = payload.get("volume_record_table")
    if not isinstance(header, dict) or not isinstance(point_block, dict):
        raise ValueError("binary ngmesh has no decoded point scalar block")
    if not isinstance(volume_table, dict) or not volume_table.get("matches_stats_counts"):
        raise ValueError("binary ngmesh volume records do not match sibling stats")
    npoints = int(header["npoints_hint_0x34"])
    nvol = int(volume_table["record_count"])
    return MeshCandidate(
        path=path,
        source_format="ngmesh-binary",
        header=(),
        n_vertices=npoints,
        n_elements=nvol,
        body_counts=stats_body_counts_with_background(stats),
    )


def find_ngmesh_binary_candidates(results_dir: Path) -> list[MeshCandidate]:
    candidates = []
    for path in find_ngmesh_hints(results_dir):
        with path.open("rb") as f:
            head = f.read(32)
        if head.startswith(b"unencryptedASC"):
            continue
        try:
            candidates.append(read_ngmesh_binary_candidate(path))
        except ValueError:
            continue
    return candidates


def read_mesh_candidate(path: Path) -> MeshCandidate:
    if path.name.lower().startswith("plot") and path.suffix.lower() == ".tmp":
        return read_plot_candidate(path)
    if path.suffix.lower() == ".ngmesh":
        try:
            return read_ngmesh_ascii_candidate(path)
        except ValueError:
            return read_ngmesh_binary_candidate(path)
    raise ValueError(f"Unsupported mesh cache path: {path}")


def mesh_candidate_sort_key(item: MeshCandidate) -> tuple[int, int, float, str]:
    return (
        item.n_elements,
        item.n_vertices,
        item.path.stat().st_mtime,
        str(item.path).lower(),
    )


def discover_mesh_candidates(results_dir: Path) -> list[MeshCandidate]:
    candidates = find_plot_candidates(results_dir)
    candidates.extend(find_ngmesh_ascii_candidates(results_dir))
    candidates.extend(find_ngmesh_binary_candidates(results_dir))
    candidates.sort(key=mesh_candidate_sort_key, reverse=True)
    return candidates


def sibling_stats_path(path: Path) -> Path | None:
    for name in ("current.stats", "initial.stats", "defn_native_vmesh.stats"):
        candidate = path.with_name(name)
        if candidate.exists():
            return candidate
    return None


def describe_binary_ngmesh_hints(paths: list[Path], limit: int = 4) -> str:
    lines: list[str] = []
    for path in paths[:limit]:
        try:
            payload = inspect_ngmesh(path, sibling_stats_path(path))
        except Exception as exc:  # noqa: BLE001
            lines.append(f"  - {path} (metadata read failed: {exc})")
            continue
        body_table = payload.get("body_name_table")
        body_text = "body names: not found"
        if isinstance(body_table, dict):
            names = body_table.get("names") or []
            shown = ", ".join(str(name) for name in names[:8])
            if len(names) > 8:
                shown += f", ... +{len(names) - 8}"
            body_text = f"{body_table.get('count')} body names: {shown}"
        stats_counts = payload.get("stats_body_counts") or {}
        total = 0
        if isinstance(stats_counts, dict):
            total = sum(int(count) for count in stats_counts.values() if int(count) > 0)
        total_text = f", stats tets={total}" if total else ""
        header = payload.get("header") or {}
        npoints = header.get("npoints_hint_0x34") if isinstance(header, dict) else None
        nvol = header.get("nvol_hint_0x3c") if isinstance(header, dict) else None
        volume_table = payload.get("volume_record_table")
        volume_text = ""
        if isinstance(volume_table, dict):
            volume_text = (
                f", volume records={volume_table.get('record_count')} "
                f"match_stats={volume_table.get('matches_stats_counts')}"
            )
        hint_text = f", npoints={npoints}, nvol={nvol}" if npoints or nvol else ""
        lines.append(f"  - {path} ({body_text}{hint_text}{total_text}{volume_text})")
    if len(paths) > limit:
        lines.append(f"  ... {len(paths) - limit} more")
    return "\n".join(lines)


def choose_mesh_candidate(results_dir: Path, explicit_plot: str | None) -> MeshCandidate:
    if explicit_plot:
        return read_plot_candidate(Path(explicit_plot).resolve())
    candidates = discover_mesh_candidates(results_dir)
    if not candidates:
        message = f"No valid fldplt/Plot*.tmp mesh cache found under {results_dir}"
        ngmesh_hints = find_ngmesh_hints(results_dir)
        if ngmesh_hints:
            listed = describe_binary_ngmesh_hints(ngmesh_hints)
            message += (
                "\nFound AEDT solver .ngmesh caches instead, but none is a supported ASCII ng01 cache. "
                "Binary HFSS ngmesh metadata is readable, but no validated binary geometry candidate "
                "was found:"
                f"\n{listed}"
                "\nUse a results folder with fldplt/Plot*.tmp or an ASCII ng01 mesh cache for .msh export. "
                "For metadata, run scripts/inspect_hfss_ngmesh.py on current.ngmesh."
            )
        raise FileNotFoundError(message)
    return candidates[0]


def choose_plot_candidate(results_dir: Path, explicit_plot: str | None) -> MeshCandidate:
    return choose_mesh_candidate(results_dir, explicit_plot)


def mesh_candidate_summary(candidate: MeshCandidate, results_dir: Path) -> dict[str, object]:
    stats_path, current_stats = choose_stats(results_dir, candidate, None)
    if current_stats is None:
        validation_status = "WARN"
        validation_message = "No matching stats file found; enable unvalidated export to continue."
    else:
        validation_status = "PASS"
        validation_message = f"Matched stats file: {stats_path}"
    return row_to_legacy_summary(
        candidate_to_row(
            candidate,
            results_dir,
            validation_status=validation_status,
            validation_message=validation_message,
            stats_path=stats_path,
        )
    )


def list_mesh_candidates(project: str | Path) -> list[dict[str, object]]:
    results_dir, plot_from_project = resolve_project_path(str(project))
    if plot_from_project is not None:
        return [mesh_candidate_summary(read_plot_candidate(plot_from_project), results_dir)]
    return [
        mesh_candidate_summary(candidate, results_dir) for candidate in discover_mesh_candidates(results_dir)
    ]


def parse_current_stats(path: Path) -> OrderedDict[int, dict[str, float]]:
    stats: OrderedDict[int, dict[str, float]] = OrderedDict()
    for line in path.read_text(errors="ignore").splitlines():
        values = NUM_RE.findall(line)
        if len(values) < 9:
            continue
        body_id = int(values[0])
        stats[body_id] = {
            "count": int(values[1]),
            "min_max_edge": float(values[2]),
            "max_max_edge": float(values[3]),
            "rms_max_edge": float(values[4]),
            "min_volume": float(values[5]),
            "max_volume": float(values[6]),
            "mean_volume": float(values[7]),
            "std_volume": float(values[8]),
        }
    if not stats:
        raise ValueError(f"No mesh-stat rows found in {path}")
    return stats


def stats_body_counts(stats: OrderedDict[int, dict[str, float]]) -> Counter[int]:
    return Counter(
        {
            body_id: int(row["count"])
            for body_id, row in stats.items()
            if body_id != 0 and int(row["count"]) > 0
        }
    )


def stats_body_counts_with_background(stats: OrderedDict[int, dict[str, float]]) -> Counter[int]:
    return Counter({body_id: int(row["count"]) for body_id, row in stats.items() if int(row["count"]) > 0})


def choose_stats(
    results_dir: Path, candidate: MeshCandidate, explicit_stats: str | None
) -> tuple[Path | None, OrderedDict[int, dict[str, float]] | None]:
    if explicit_stats:
        path = Path(explicit_stats).resolve()
        return path, parse_current_stats(path)

    matches: list[tuple[Path, OrderedDict[int, dict[str, float]]]] = []
    sibling = sibling_stats_path(candidate.path)
    stat_paths: list[Path] = []
    if sibling is not None:
        stat_paths.append(sibling)
    stat_paths.extend(path for path in sorted(results_dir.rglob("*.stats")) if path not in stat_paths)

    for path in stat_paths:
        try:
            stats = parse_current_stats(path)
        except ValueError:
            continue
        candidate_counts_without_background = Counter(
            {body_id: count for body_id, count in candidate.body_counts.items() if body_id != 0}
        )
        if (
            stats_body_counts(stats) == candidate.body_counts
            or stats_body_counts_with_background(stats) == candidate.body_counts
            or stats_body_counts(stats) == candidate_counts_without_background
        ):
            matches.append((path, stats))

    if matches:
        matches.sort(key=lambda item: item[0].stat().st_mtime, reverse=True)
        return matches[0]
    return None, None


def parse_body_names(
    hfss_stats_path: Path | None, current_stats: OrderedDict[int, dict[str, float]] | None
) -> dict[int, str]:
    if hfss_stats_path is None or current_stats is None:
        return {}

    name_rows: list[tuple[str, int]] = []
    for line in hfss_stats_path.read_text(errors="ignore").splitlines():
        if "|" not in line or "Num Tets" in line:
            continue
        parts = [part.strip() for part in line.split("|")]
        if len(parts) < 2 or not parts[0]:
            continue
        try:
            count = int(parts[1])
        except ValueError:
            continue
        name_rows.append((parts[0], count))

    names = {}
    stats_rows = [(body_id, int(row["count"])) for body_id, row in current_stats.items()]
    for (body_id, expected_count), (name, actual_count) in zip(stats_rows, name_rows, strict=False):
        if expected_count == actual_count and body_id != 0 and expected_count > 0:
            names[body_id] = name
    return names


def parse_plot_tmp(path: Path) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    candidate = read_plot_candidate(path)
    data = path.read_bytes()
    n_vertices = candidate.n_vertices
    n_elements = candidate.n_elements
    element_start = 44
    element_end = element_start + n_elements * PLOT_RECORD_INTS * 4
    coord_end = element_end + n_vertices * 3 * 8

    elements = np.frombuffer(data[element_start:element_end], dtype="<i4").reshape(
        n_elements, PLOT_RECORD_INTS
    )
    if not np.all(elements[:, 0:5] == PLOT_ELEMENT_PREFIX):
        raise ValueError("Element records are not uniform 10-node tetra records")

    coords = np.frombuffer(data[element_end:coord_end], dtype="<f8").reshape(n_vertices, 3)
    connectivity = elements[:, 5:15].astype(np.int64)
    body_ids = np.frombuffer(data[coord_end + 8 : coord_end + 8 + n_elements * 4], dtype="<i4").copy()
    if int(connectivity.min()) != 1 or int(connectivity.max()) != n_vertices:
        raise ValueError("Connectivity is not valid 1-based node indexing")
    return np.array(candidate.header, dtype=np.int32), coords.copy(), connectivity, body_ids


def parse_ngmesh_ascii(path: Path) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, dict[int, str]]:
    point_rows: dict[int, tuple[float, float, float]] = {}
    element_rows: list[tuple[int, list[int]]] = []
    body_names: dict[int, str] = {}
    in_points = False
    in_vol = False

    for raw_line in path.read_text(encoding="latin1").splitlines():
        line = raw_line.strip()
        if not line:
            continue
        if line == "begin_point_data":
            in_points = True
            continue
        if line == "end_point_data":
            in_points = False
            continue
        if line == "begin_vol_element_data":
            in_vol = True
            continue
        if line == "end_vol_element_data":
            in_vol = False
            continue

        parts = line.split()
        if parts[0] == "body_id" and "body_name" in parts:
            body_names[int(parts[1])] = parts[parts.index("body_name") + 1]
        elif in_points and parts[0] == "pid":
            pid = int(parts[1])
            coords_index = parts.index("coords") + 1
            point_rows[pid] = (
                float(parts[coords_index]),
                float(parts[coords_index + 1]),
                float(parts[coords_index + 2]),
            )
        elif in_vol and parts[0] == "veid":
            body_id = int(parts[parts.index("body_id") + 1])
            nv = int(parts[parts.index("nv") + 1])
            if nv != 4:
                raise ValueError(f"Only linear tetra ngmesh elements are supported, got nv={nv}")
            verts_index = parts.index("vert_ids") + 1
            element_rows.append((body_id, [int(value) for value in parts[verts_index : verts_index + 4]]))

    if not point_rows:
        raise ValueError("ASCII ngmesh has no point rows")
    if not element_rows:
        raise ValueError("ASCII ngmesh has no volume element rows")

    sorted_ids = sorted(point_rows)
    id_to_dense = {pid: index + 1 for index, pid in enumerate(sorted_ids)}
    coords = np.array([point_rows[pid] for pid in sorted_ids], dtype=float)
    connectivity = np.array(
        [[id_to_dense[pid] for pid in verts] for _body_id, verts in element_rows],
        dtype=np.int64,
    )
    body_ids = np.array([body_id for body_id, _verts in element_rows], dtype=np.int32)
    return np.array([], dtype=np.int32), coords, connectivity, body_ids, body_names


def parse_ngmesh_binary(path: Path) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, dict[int, str]]:
    stats_path = sibling_stats_path(path)
    if stats_path is None:
        raise ValueError("binary ngmesh needs sibling current.stats/initial.stats for validation")
    stats = parse_current_stats(stats_path)
    stats_counts = OrderedDict((body_id, int(row["count"])) for body_id, row in stats.items())
    data = path.read_bytes()
    header = decode_header_hints(data)
    point_block = find_point_scalar_block(data, header)
    if point_block is None:
        raise ValueError("binary ngmesh point scalar block not found")
    npoints = int(point_block["point_count"])
    scalar_count = int(point_block["scalar_count"])
    if scalar_count != npoints * 3:
        raise ValueError("binary ngmesh scalar count does not equal 3*npoints")

    coords_m = np.empty(scalar_count, dtype=np.float64)
    cursor = int(point_block["scalar_data_offset"])
    for index in range(scalar_count):
        encoded = struct.unpack_from("<Q", data, cursor)[0]
        decoded = encoded ^ NGMESH_COORD_XOR_MASK
        coords_m[index] = struct.unpack("<d", struct.pack("<Q", decoded))[0]
        cursor += 8
    coords = coords_m.reshape(npoints, 3) * 1000.0

    volume_table = find_volume_record_table(data, stats_counts)
    if volume_table is None or not volume_table.get("matches_stats_counts"):
        raise ValueError("binary ngmesh volume records do not match stats")
    nvol = int(volume_table["record_count"])
    connectivity = np.empty((nvol, 4), dtype=np.int64)
    body_ids = np.empty(nvol, dtype=np.int32)
    cursor = int(volume_table["offset"])
    for index in range(nvol):
        body_id = id_token_value(data, cursor + 4)
        if body_id is None:
            raise ValueError("binary ngmesh body id decode failed")
        body_ids[index] = int(body_id)
        for local in range(4):
            node_id = id_token_value(data, cursor + 8 + local * 4)
            if node_id is None:
                raise ValueError("binary ngmesh node id decode failed")
            connectivity[index, local] = int(node_id)
        cursor += 28
    if int(connectivity.min()) != 1 or int(connectivity.max()) > npoints:
        raise ValueError("binary ngmesh connectivity is not valid 1-based node indexing")
    summary_header = np.array(
        [
            int(header.get("major") or 0),
            int(header.get("minor") or 0),
            npoints,
            nvol,
        ],
        dtype=np.int32,
    )
    return summary_header, coords, connectivity, body_ids, {}


def parse_mesh_source(
    path: Path, source_format: str
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, dict[int, str]]:
    if source_format == "plot-tmp":
        header, coords, connectivity, body_ids = parse_plot_tmp(path)
        return header, coords, connectivity, body_ids, {}
    if source_format == "ngmesh-ascii":
        return parse_ngmesh_ascii(path)
    if source_format == "ngmesh-binary":
        return parse_ngmesh_binary(path)
    raise ValueError(f"Unsupported mesh source format: {source_format}")


def filter_background_mesh(
    coords: np.ndarray,
    connectivity: np.ndarray,
    body_ids: np.ndarray,
    *,
    exclude_background: bool,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, dict[str, int]]:
    removed = {
        "background_tets": 0,
        "removed_nodes": 0,
        "original_vertices": int(len(coords)),
        "original_elements": int(len(connectivity)),
    }
    if not exclude_background:
        return coords, connectivity, body_ids, removed

    keep = body_ids != 0
    removed["background_tets"] = int(len(body_ids) - int(np.count_nonzero(keep)))
    if removed["background_tets"] == 0:
        return coords, connectivity, body_ids, removed
    if not np.any(keep):
        raise ValueError("Cannot exclude background mesh because all tetrahedra have body_id=0")

    filtered_connectivity = connectivity[keep]
    used_node_ids = np.unique(filtered_connectivity.reshape(-1))
    if int(used_node_ids.min()) < 1 or int(used_node_ids.max()) > len(coords):
        raise ValueError("Connectivity references nodes outside the coordinate table")

    remap = np.zeros(int(used_node_ids.max()) + 1, dtype=np.int64)
    remap[used_node_ids] = np.arange(1, len(used_node_ids) + 1, dtype=np.int64)
    compact_connectivity = remap[filtered_connectivity]
    compact_coords = coords[used_node_ids - 1].copy()
    compact_body_ids = body_ids[keep].copy()
    removed["removed_nodes"] = int(len(coords) - len(compact_coords))
    return compact_coords, compact_connectivity, compact_body_ids, removed


def corner_node_order(connectivity: np.ndarray) -> np.ndarray:
    if connectivity.shape[1] == 10:
        return CORNER_LOCAL_ORDER
    if connectivity.shape[1] == 4:
        return np.arange(4, dtype=np.int64)
    raise ValueError(f"Unsupported tetra node count: {connectivity.shape[1]}")


def tet_quality_by_body(
    coords: np.ndarray, connectivity: np.ndarray, body_ids: np.ndarray
) -> dict[int, dict[str, float]]:
    zero_conn = connectivity - 1
    corner_points = coords[zero_conn[:, corner_node_order(connectivity)]]
    edge_lengths = np.stack(
        [
            np.linalg.norm(corner_points[:, left] - corner_points[:, right], axis=1)
            for left, right in LINEAR_TET_EDGES
        ],
        axis=1,
    )
    max_edge = edge_lengths.max(axis=1)
    volumes = (
        np.abs(
            np.einsum(
                "ij,ij->i",
                np.cross(
                    corner_points[:, 1] - corner_points[:, 0],
                    corner_points[:, 2] - corner_points[:, 0],
                ),
                corner_points[:, 3] - corner_points[:, 0],
            )
        )
        / 6.0
    )

    rows = {}
    for body_id in sorted(set(body_ids.tolist())):
        mask = body_ids == body_id
        body_max_edge = max_edge[mask]
        body_volume = volumes[mask]
        rows[body_id] = {
            "count": int(mask.sum()),
            "min_max_edge": float(body_max_edge.min()),
            "max_max_edge": float(body_max_edge.max()),
            "rms_max_edge": float(math.sqrt(float(np.mean(body_max_edge * body_max_edge)))),
            "min_volume": float(body_volume.min()),
            "max_volume": float(body_volume.max()),
            "mean_volume": float(body_volume.mean()),
            "std_volume": float(body_volume.std(ddof=1)) if len(body_volume) > 1 else 0.0,
        }
    return rows


def rel_error(actual: float, expected: float) -> float:
    return abs(actual - expected) / max(abs(expected), 1e-300)


def write_validation(
    path: Path,
    actual: dict[int, dict[str, float]],
    expected: OrderedDict[int, dict[str, float]] | None,
) -> bool:
    fields = [
        "count",
        "min_max_edge",
        "max_max_edge",
        "rms_max_edge",
        "min_volume",
        "max_volume",
        "mean_volume",
        "std_volume",
    ]
    if expected is None:
        with path.open("w", newline="") as f:
            writer = csv.writer(f)
            writer.writerow(["status", "reason"])
            writer.writerow(["SKIPPED", "No matching current.stats file found"])
        return False

    all_pass = True
    with path.open("w", newline="") as f:
        writer = csv.writer(f)
        writer.writerow(["body_id", "metric", "actual", "expected", "relative_error", "status"])
        for body_id in sorted(actual):
            if body_id not in expected:
                all_pass = False
                writer.writerow([body_id, "body_id", body_id, "", "", "FAIL"])
                continue
            for field in fields:
                actual_value = actual[body_id][field]
                expected_value = expected[body_id][field]
                if field == "count":
                    passed = int(actual_value) == int(expected_value)
                    error = 0.0 if passed else 1.0
                else:
                    error = rel_error(float(actual_value), float(expected_value))
                    passed = error <= 1e-4
                all_pass = all_pass and passed
                writer.writerow(
                    [
                        body_id,
                        field,
                        actual_value,
                        expected_value,
                        error,
                        "PASS" if passed else "FAIL",
                    ]
                )
    return all_pass


def write_gmsh(
    path: Path,
    coords: np.ndarray,
    connectivity: np.ndarray,
    body_ids: np.ndarray,
    body_names: dict[int, str],
) -> None:
    if connectivity.shape[1] == 10:
        element_type = 11
        ordered = connectivity[:, GMSH_LOCAL_ORDER]
    elif connectivity.shape[1] == 4:
        element_type = 4
        ordered = connectivity
    else:
        raise ValueError(f"Unsupported tetra node count: {connectivity.shape[1]}")
    present_body_ids = sorted(set(body_ids.tolist()))
    with path.open("w", newline="\n") as f:
        f.write("$MeshFormat\n2.2 0 8\n$EndMeshFormat\n")
        f.write("$PhysicalNames\n")
        f.write(f"{len(present_body_ids)}\n")
        for body_id in present_body_ids:
            name = body_names.get(body_id, f"body_{body_id}")
            name = name.replace("\\", "\\\\").replace('"', '\\"')
            f.write(f'3 {body_id} "{name}"\n')
        f.write("$EndPhysicalNames\n")
        f.write("$Nodes\n")
        f.write(f"{len(coords)}\n")
        for node_id, (x_coord, y_coord, z_coord) in enumerate(coords, start=1):
            f.write(f"{node_id} {x_coord:.16g} {y_coord:.16g} {z_coord:.16g}\n")
        f.write("$EndNodes\n")
        f.write("$Elements\n")
        f.write(f"{len(connectivity)}\n")
        for elem_id, (body_id, nodes) in enumerate(zip(body_ids, ordered, strict=True), start=1):
            node_text = " ".join(str(int(node)) for node in nodes)
            f.write(f"{elem_id} {element_type} 2 {int(body_id)} {int(body_id)} {node_text}\n")
        f.write("$EndElements\n")


def polygon_for_tet(points: np.ndarray, axis: int, value: float, tol: float):
    signed = points[:, axis] - value
    if signed.min() > tol or signed.max() < -tol:
        return None

    intersections: list[np.ndarray] = []
    for left, right in LINEAR_TET_EDGES:
        dl = signed[left]
        dr = signed[right]
        pl = points[left]
        pr = points[right]
        if abs(dl) <= tol and abs(dr) <= tol:
            intersections.extend([pl, pr])
        elif abs(dl) <= tol:
            intersections.append(pl)
        elif abs(dr) <= tol:
            intersections.append(pr)
        elif dl * dr < 0.0:
            fraction = dl / (dl - dr)
            intersections.append(pl + fraction * (pr - pl))

    unique: list[np.ndarray] = []
    for point in intersections:
        if not any(np.linalg.norm(point - existing) <= tol * 8 for existing in unique):
            unique.append(point)
    if len(unique) < 3:
        return None

    keep_axes = [index for index in range(3) if index != axis]
    poly = np.array([[point[keep_axes[0]], point[keep_axes[1]]] for point in unique])
    center = poly.mean(axis=0)
    angles = np.arctan2(poly[:, 1] - center[1], poly[:, 0] - center[0])
    return poly[np.argsort(angles)]


def body_color_map(body_ids: np.ndarray) -> dict[int, tuple[float, float, float, float]]:
    import matplotlib.pyplot as plt

    cmap = plt.get_cmap("tab20")
    return {body_id: cmap(index % 20) for index, body_id in enumerate(sorted(set(body_ids.tolist())))}


def render_section(
    out_dir: Path,
    prefix: str,
    coords: np.ndarray,
    connectivity: np.ndarray,
    body_ids: np.ndarray,
    axis_name: str,
    value: float,
) -> dict[str, object]:
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from matplotlib.collections import PolyCollection

    prefix = sanitize_output_prefix(prefix, "hfss")
    axis = {"x": 0, "y": 1, "z": 2}[axis_name]
    other_axes = [index for index in range(3) if index != axis]
    axis_labels = ["x (mm)", "y (mm)", "z (mm)"]
    bbox_min = coords.min(axis=0)
    bbox_max = coords.max(axis=0)
    span = bbox_max - bbox_min
    tol = max(float(span.max()) * 1e-10, 1e-9)
    corner_points = coords[(connectivity - 1)[:, corner_node_order(connectivity)]]
    signed = corner_points[:, :, axis] - value
    candidate_indices = np.where((signed.min(axis=1) <= tol) & (signed.max(axis=1) >= -tol))[0]

    colors = body_color_map(body_ids)
    polygons = []
    facecolors = []
    edgecolors = []
    body_counts = Counter()
    for elem_index in candidate_indices:
        poly = polygon_for_tet(corner_points[elem_index], axis, value, tol)
        if poly is None:
            continue
        body_id = int(body_ids[elem_index])
        color = colors[body_id]
        polygons.append(poly)
        facecolors.append((color[0], color[1], color[2], 0.14))
        edgecolors.append((color[0] * 0.55, color[1] * 0.55, color[2] * 0.55, 0.88))
        body_counts[body_id] += 1

    safe_value = f"{value:.3f}".replace(".", "p").replace("-", "m")
    out_path = out_dir / f"{prefix}_solver_section_{axis_name}_{safe_value}.png"
    x_span = max(float(span[other_axes[0]]), 1e-9)
    y_span = max(float(span[other_axes[1]]), 1e-9)
    ratio = x_span / y_span
    fig_w = min(max(7.0, 3.2 * ratio), 14.0)
    fig_h = min(max(3.2, fig_w / ratio), 10.0)
    fig, ax = plt.subplots(figsize=(fig_w, fig_h), constrained_layout=True)
    if polygons:
        ax.add_collection(
            PolyCollection(
                polygons,
                facecolors=facecolors,
                edgecolors=edgecolors,
                linewidths=0.18,
                antialiaseds=False,
            )
        )
    ax.set_aspect("equal", adjustable="box")
    ax.set_xlim(float(bbox_min[other_axes[0]]), float(bbox_max[other_axes[0]]))
    ax.set_ylim(float(bbox_min[other_axes[1]]), float(bbox_max[other_axes[1]]))
    ax.set_xlabel(axis_labels[other_axes[0]])
    ax.set_ylabel(axis_labels[other_axes[1]])
    ax.set_title(f"HFSS solver mesh section: {axis_name} = {value:.3f} mm")
    ax.grid(True, color="#d0d0d0", linewidth=0.25)
    fig.savefig(out_path, dpi=260)
    plt.close(fig)

    return {
        "axis": axis_name,
        "value_mm": value,
        "candidate_tets": int(len(candidate_indices)),
        "polygons": int(len(polygons)),
        "output": out_path.name,
        "body_counts": dict(sorted(body_counts.items())),
    }


def parse_section_arg(value: str) -> tuple[str, float]:
    if "=" not in value:
        raise argparse.ArgumentTypeError(f"section must look like AXIS=MM, got {value!r}")
    axis, raw = value.split("=", 1)
    axis = axis.strip().lower()
    if axis not in {"x", "y", "z"}:
        raise argparse.ArgumentTypeError(f"section axis must be x, y, or z, got {axis!r}")
    return axis, float(raw)


def write_section_summary(path: Path, rows: list[dict[str, object]]) -> None:
    with path.open("w", newline="") as f:
        writer = csv.writer(f)
        writer.writerow(["axis", "value_mm", "candidate_tets", "polygons", "output", "body_counts"])
        for row in rows:
            body_text = ";".join(f"{body}:{count}" for body, count in row["body_counts"].items())
            writer.writerow(
                [
                    row["axis"],
                    row["value_mm"],
                    row["candidate_tets"],
                    row["polygons"],
                    row["output"],
                    body_text,
                ]
            )


def write_summary(
    path: Path,
    source_path: Path,
    source_format: str,
    stats_path: Path | None,
    mesh_path: Path,
    validation_path: Path,
    header: np.ndarray,
    coords: np.ndarray,
    connectivity: np.ndarray,
    body_ids: np.ndarray,
    validation_pass: bool,
    body_names: dict[int, str],
    section_rows: list[dict[str, object]],
    background_filter: dict[str, int],
) -> None:
    bbox_min = coords.min(axis=0)
    bbox_max = coords.max(axis=0)
    counts = Counter(body_ids.tolist())
    lines = [
        "HFSS solver mesh extraction",
        "",
        f"Source mesh cache: {source_path}",
        f"Source format: {source_format}",
        f"Stats source: {stats_path if stats_path else 'not found'}",
        f"Output mesh: {mesh_path}",
        f"Text mesh format: Gmsh 2.2 ASCII, element type {11 if connectivity.shape[1] == 10 else 4}",
        f"Vertices: {len(coords)}",
        f"Elements: {len(connectivity)}",
        f"Header ints: {header.tolist() if len(header) else 'not available'}",
        f"Bounding box min mm: {bbox_min.tolist()}",
        f"Bounding box max mm: {bbox_max.tolist()}",
        (
            "Background mesh: excluded "
            f"({background_filter['background_tets']} tets, "
            f"{background_filter['removed_nodes']} nodes removed)"
            if background_filter["background_tets"]
            else "Background mesh: included or not present"
        ),
        f"Original vertices: {background_filter['original_vertices']}",
        f"Original elements: {background_filter['original_elements']}",
        "",
        "Validation:",
        f"- Status: {'PASS' if validation_pass else 'SKIPPED/FAIL'}",
        "- HFSS current.stats edge columns are interpreted as per-tetrahedron maximum-edge statistics.",
        f"- Full metric table: {validation_path}",
        "",
        "Body IDs:",
    ]
    for body_id in sorted(counts):
        lines.append(f"- {body_id}: {body_names.get(body_id, f'body_{body_id}')} ({counts[body_id]} tets)")
    if section_rows:
        lines.extend(["", "Rendered sections:"])
        for row in section_rows:
            lines.append(
                f"- {row['output']}: {row['axis']}={row['value_mm']:.6g} mm, "
                f"{row['polygons']} intersection polygons"
            )
    path.write_text("\n".join(lines) + "\n")


def extract_solver_mesh(
    project: str | Path,
    out_dir: str | Path | None = None,
    prefix: str | None = None,
    plot_cache: str | Path | None = None,
    mesh_cache: str | Path | None = None,
    stats: str | Path | None = None,
    hfss_stats: str | Path | None = None,
    sections: list[str] | None = None,
    no_auto_sections: bool = False,
    no_sections: bool = False,
    allow_unvalidated: bool = False,
    exclude_background: bool = False,
) -> dict[str, object]:
    results_dir, plot_from_project = resolve_project_path(str(project))
    if mesh_cache:
        candidate = read_mesh_candidate(Path(mesh_cache).resolve())
    else:
        explicit_plot = (
            str(plot_cache) if plot_cache else (str(plot_from_project) if plot_from_project else None)
        )
        candidate = choose_mesh_candidate(results_dir, explicit_plot)
    stats_path, current_stats = choose_stats(results_dir, candidate, str(stats) if stats else None)
    if current_stats is None and not allow_unvalidated:
        raise FileNotFoundError(
            "No matching current.stats file found. Use --stats PATH or --allow-unvalidated."
        )

    output_dir = Path(out_dir).resolve() if out_dir else results_dir.parent / "mesh_export"
    output_dir.mkdir(parents=True, exist_ok=True)
    prefix_value = sanitize_output_prefix(prefix or default_prefix(results_dir, candidate.path), "hfss")

    mesh_path = output_dir / f"{prefix_value}_solver_mesh_ascii.msh"
    validation_path = output_dir / f"{prefix_value}_solver_mesh_validation.csv"
    section_summary_path = output_dir / f"{prefix_value}_solver_section_summary.csv"
    summary_path = output_dir / f"{prefix_value}_solver_mesh_export_summary.txt"

    header, coords, connectivity, body_ids, source_body_names = parse_mesh_source(
        candidate.path, candidate.source_format
    )
    coords, connectivity, body_ids, background_filter = filter_background_mesh(
        coords, connectivity, body_ids, exclude_background=exclude_background
    )
    actual_stats = tet_quality_by_body(coords, connectivity, body_ids)
    validation_pass = write_validation(validation_path, actual_stats, current_stats)

    hfss_stats_path = Path(hfss_stats).resolve() if hfss_stats else None
    body_names = parse_body_names(hfss_stats_path, current_stats)
    body_names = {**source_body_names, **body_names}
    write_gmsh(mesh_path, coords, connectivity, body_ids, body_names)
    mesh_hash = canonical_mesh_hash(coords, connectivity, body_ids)

    section_rows: list[dict[str, object]] = []
    if not no_sections:
        bbox_min = coords.min(axis=0)
        bbox_max = coords.max(axis=0)
        requested_sections = sections or []
        section_specs: list[tuple[str, float]] = []
        if not no_auto_sections:
            section_specs.extend(
                [
                    ("x", float((bbox_min[0] + bbox_max[0]) * 0.5)),
                    ("y", float((bbox_min[1] + bbox_max[1]) * 0.5)),
                    ("z", float((bbox_min[2] + bbox_max[2]) * 0.5)),
                ]
            )
        section_specs.extend(parse_section_arg(item) for item in requested_sections)
        section_rows = [
            render_section(output_dir, prefix_value, coords, connectivity, body_ids, axis, value)
            for axis, value in section_specs
        ]
        write_section_summary(section_summary_path, section_rows)

    write_summary(
        summary_path,
        candidate.path,
        candidate.source_format,
        stats_path,
        mesh_path,
        validation_path,
        header,
        coords,
        connectivity,
        body_ids,
        validation_pass,
        body_names,
        section_rows,
        background_filter,
    )

    return {
        "results_dir": results_dir,
        "mesh_cache": candidate.path,
        "plot_cache": candidate.path,
        "source_format": candidate.source_format,
        "stats_path": stats_path,
        "mesh_path": mesh_path,
        "validation_path": validation_path,
        "summary_path": summary_path,
        "section_summary_path": section_summary_path,
        "validation_pass": validation_pass,
        "section_rows": section_rows,
        "vertices": int(len(coords)),
        "elements": int(len(connectivity)),
        "background_filter": background_filter,
        "mesh_hash": mesh_hash,
    }


def canonical_mesh_hash(coords: np.ndarray, connectivity: np.ndarray, body_ids: np.ndarray) -> str:
    """Hash the canonical ordered coordinate, tetrahedron, and material arrays."""
    digest = hashlib.sha256()
    for label, array, dtype in (
        (b"coords", coords, "<f8"),
        (b"tets", connectivity, "<i8"),
        (b"body_ids", body_ids, "<i8"),
    ):
        canonical = np.ascontiguousarray(array, dtype=dtype)
        digest.update(label)
        digest.update(struct.pack("<Q", canonical.ndim))
        digest.update(struct.pack("<" + "Q" * canonical.ndim, *canonical.shape))
        digest.update(canonical.tobytes(order="C"))
    return digest.hexdigest()


def main() -> int:
    args = parse_args()
    try:
        result = extract_solver_mesh(
            args.project,
            out_dir=args.out_dir,
            prefix=args.prefix,
            plot_cache=args.plot_cache,
            mesh_cache=args.mesh_cache,
            stats=args.stats,
            hfss_stats=args.hfss_stats,
            sections=args.section,
            no_auto_sections=args.no_auto_sections,
            no_sections=args.no_sections,
            allow_unvalidated=args.allow_unvalidated,
            exclude_background=args.exclude_background,
        )
    except (FileNotFoundError, ValueError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1

    print(f"mesh_cache={result['mesh_cache']}")
    print(f"source_format={result['source_format']}")
    print(f"mesh={result['mesh_path']}")
    print(f"validation={'PASS' if result['validation_pass'] else 'SKIPPED/FAIL'}")
    print(f"summary={result['summary_path']}")
    print(
        "background_removed="
        f"{result['background_filter']['background_tets']} tets, "
        f"{result['background_filter']['removed_nodes']} nodes"
    )
    for row in result["section_rows"]:
        print(f"section={row['output']} polygons={row['polygons']}")
    return 0 if result["validation_pass"] or args.allow_unvalidated else 2


if __name__ == "__main__":
    raise SystemExit(main())
