from __future__ import annotations

from collections import Counter
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any

import numpy as np


def _sibling_stats_path(path: Path) -> Path | None:
    for name in ("current.stats", "initial.stats", "defn_native_vmesh.stats"):
        candidate = path.with_name(name)
        if candidate.exists():
            return candidate
    return None


def _relative_text(path: Path, root: Path) -> str:
    try:
        return str(path.relative_to(root))
    except ValueError:
        return str(path)


@dataclass(frozen=True)
class MeshCandidateRow:
    path: Path
    relative_path: str
    source_format: str
    size_bytes: int
    modified_timestamp: float
    vertices: int
    elements: int
    background_tets: int
    body_count: int
    body_counts: Counter[int]
    stats_path: Path | None
    validation_status: str = "unknown"
    validation_message: str = ""


@dataclass(frozen=True)
class MeshValidationSummary:
    status: str
    stats_path: Path | None = None
    messages: tuple[str, ...] = ()
    body_counts: Counter[int] = field(default_factory=Counter)
    background_tets: int = 0

    @classmethod
    def from_messages(
        cls,
        messages: list[str] | tuple[str, ...],
        *,
        stats_path: Path | None = None,
        body_counts: Counter[int] | None = None,
        background_tets: int = 0,
    ) -> MeshValidationSummary:
        lowered = [message.lower() for message in messages]
        if any("fail" in message or "error" in message for message in lowered):
            status = "FAIL"
        elif any("warn" in message or "unvalidated" in message for message in lowered):
            status = "WARN"
        else:
            status = "PASS"
        return cls(
            status=status,
            stats_path=stats_path,
            messages=tuple(messages),
            body_counts=Counter(body_counts or {}),
            background_tets=int(background_tets),
        )


@dataclass(frozen=True)
class ActiveMeshState:
    path: Path
    size_bytes: int
    modified_timestamp: float
    bbox_min: tuple[float, float, float]
    bbox_max: tuple[float, float, float]
    vertices: int
    elements: int
    body_ids: tuple[int, ...]
    body_counts: Counter[int]
    physical_names: dict[int, str]
    source_row: MeshCandidateRow | None = None

    @classmethod
    def from_msh(cls, path: str | Path, source_row: MeshCandidateRow | None = None) -> ActiveMeshState:
        raise RuntimeError("GUI mesh readback is not part of the embedded MCP extractor")

    @classmethod
    def from_mesh(
        cls,
        path: str | Path,
        mesh: Any,
        source_row: MeshCandidateRow | None = None,
    ) -> ActiveMeshState:
        mesh_path = Path(path).resolve()
        stat = mesh_path.stat()
        finite_coords = mesh.coords_by_node_id[np.all(np.isfinite(mesh.coords_by_node_id), axis=1)]
        if finite_coords.size == 0:
            raise ValueError(f"Mesh has no finite coordinates: {mesh_path}")
        bbox_min = tuple(float(value) for value in np.min(finite_coords, axis=0).tolist())
        bbox_max = tuple(float(value) for value in np.max(finite_coords, axis=0).tolist())
        counts = Counter(int(value) for value in mesh.body_ids.tolist())
        return cls(
            path=mesh_path,
            size_bytes=int(stat.st_size),
            modified_timestamp=float(stat.st_mtime),
            bbox_min=bbox_min,
            bbox_max=bbox_max,
            vertices=int(len(finite_coords)),
            elements=int(len(mesh.body_ids)),
            body_ids=tuple(sorted(counts)),
            body_counts=counts,
            physical_names=dict(mesh.physical_names),
            source_row=source_row,
        )

    def is_current(self) -> bool:
        try:
            stat = self.path.stat()
        except FileNotFoundError:
            return False
        return int(stat.st_size) == self.size_bytes and float(stat.st_mtime) == self.modified_timestamp


@dataclass
class BodyLayerState:
    visible_by_body: dict[int, bool]
    opacity_by_body: dict[int, float] = field(default_factory=dict)

    @classmethod
    def from_counts(cls, body_counts: Counter[int], default_visible: bool = True) -> BodyLayerState:
        return cls(
            visible_by_body={int(body_id): bool(default_visible) for body_id in sorted(body_counts)},
            opacity_by_body={int(body_id): 1.0 for body_id in sorted(body_counts)},
        )

    def selected_body_ids(self) -> set[int] | None:
        selected = {body_id for body_id, visible in self.visible_by_body.items() if visible}
        if selected == set(self.visible_by_body):
            return None
        return selected

    def set_visible(self, body_id: int, visible: bool) -> None:
        self.visible_by_body[int(body_id)] = bool(visible)
        self.opacity_by_body.setdefault(int(body_id), 1.0)

    def set_opacity(self, body_id: int, opacity: float) -> None:
        self.opacity_by_body[int(body_id)] = max(0.0, min(1.0, float(opacity)))
        self.visible_by_body.setdefault(int(body_id), True)

    def hide_background(self) -> None:
        if 0 in self.visible_by_body:
            self.visible_by_body[0] = False

    def reset(self, visible: bool = True) -> None:
        for body_id in list(self.visible_by_body):
            self.visible_by_body[body_id] = bool(visible)
            self.opacity_by_body.setdefault(body_id, 1.0)


@dataclass(frozen=True)
class MeshComparisonSummary:
    left_path: Path
    right_path: Path
    vertex_delta: int
    element_delta: int
    bbox_min_delta: tuple[float, float, float]
    bbox_max_delta: tuple[float, float, float]
    added_body_ids: tuple[int, ...]
    removed_body_ids: tuple[int, ...]
    common_body_ids: tuple[int, ...]
    body_count_delta: dict[int, int]
    background_delta: int

    @classmethod
    def compare(cls, left: ActiveMeshState, right: ActiveMeshState) -> MeshComparisonSummary:
        left_ids = set(left.body_counts)
        right_ids = set(right.body_counts)
        all_ids = left_ids | right_ids
        return cls(
            left_path=left.path,
            right_path=right.path,
            vertex_delta=int(right.vertices - left.vertices),
            element_delta=int(right.elements - left.elements),
            bbox_min_delta=tuple(
                float(right_value - left_value)
                for left_value, right_value in zip(left.bbox_min, right.bbox_min, strict=True)
            ),
            bbox_max_delta=tuple(
                float(right_value - left_value)
                for left_value, right_value in zip(left.bbox_max, right.bbox_max, strict=True)
            ),
            added_body_ids=tuple(sorted(right_ids - left_ids)),
            removed_body_ids=tuple(sorted(left_ids - right_ids)),
            common_body_ids=tuple(sorted(left_ids & right_ids)),
            body_count_delta={
                int(body_id): int(right.body_counts.get(body_id, 0) - left.body_counts.get(body_id, 0))
                for body_id in sorted(all_ids)
            },
            background_delta=int(right.body_counts.get(0, 0) - left.body_counts.get(0, 0)),
        )


def _signed_int(value: int) -> str:
    return f"{value:+d}"


def _signed_float(value: float) -> str:
    return f"{value:+.6g}"


def mesh_comparison_body_rows(
    left: ActiveMeshState,
    right: ActiveMeshState,
    summary: MeshComparisonSummary,
) -> list[dict[str, object]]:
    rows: list[dict[str, object]] = []
    for body_id, delta in summary.body_count_delta.items():
        left_count = int(left.body_counts.get(body_id, 0))
        right_count = int(right.body_counts.get(body_id, 0))
        if left_count == 0 and right_count > 0:
            status = "added"
        elif left_count > 0 and right_count == 0:
            status = "removed"
        elif delta == 0:
            status = "same"
        else:
            status = "changed"
        name = right.physical_names.get(body_id) or left.physical_names.get(body_id) or f"body_{body_id}"
        rows.append(
            {
                "body_id": int(body_id),
                "name": name,
                "left_tets": left_count,
                "right_tets": right_count,
                "delta": int(delta),
                "status": status,
            }
        )
    return rows


def format_mesh_comparison_summary(
    left: ActiveMeshState,
    right: ActiveMeshState,
    summary: MeshComparisonSummary,
) -> str:
    bbox_min_delta = ", ".join(_signed_float(value) for value in summary.bbox_min_delta)
    bbox_max_delta = ", ".join(_signed_float(value) for value in summary.bbox_max_delta)
    changed_bodies = [
        body_id
        for body_id, delta in summary.body_count_delta.items()
        if int(delta) != 0 or body_id in summary.added_body_ids or body_id in summary.removed_body_ids
    ]
    lines = [
        f"Left: {left.path}",
        f"Right: {right.path}",
        f"Vertices: {left.vertices} -> {right.vertices} ({_signed_int(summary.vertex_delta)})",
        f"Tetrahedra: {left.elements} -> {right.elements} ({_signed_int(summary.element_delta)})",
        f"Background tets delta: {_signed_int(summary.background_delta)}",
        f"BBox min delta, mm: [{bbox_min_delta}]",
        f"BBox max delta, mm: [{bbox_max_delta}]",
        f"Added body ids: {', '.join(str(value) for value in summary.added_body_ids) or 'none'}",
        f"Removed body ids: {', '.join(str(value) for value in summary.removed_body_ids) or 'none'}",
        f"Changed body ids: {', '.join(str(value) for value in changed_bodies) or 'none'}",
    ]
    return "\n".join(lines)


@dataclass(frozen=True)
class RuntimeCheckResult:
    name: str
    status: str
    message: str
    path: Path | None = None
    remediation: str = ""


def validation_allows_export(row: MeshCandidateRow, allow_unvalidated: bool) -> bool:
    status = row.validation_status.upper()
    if status in {"FAIL", "ERROR", "INVALID"}:
        return False
    if status in {"WARN", "UNKNOWN"}:
        return bool(allow_unvalidated)
    return True


def format_size(size_bytes: int) -> str:
    size = float(size_bytes)
    for suffix in ("B", "KiB", "MiB", "GiB"):
        if size < 1024.0 or suffix == "GiB":
            if suffix == "B":
                return f"{int(size)} {suffix}"
            return f"{size:.1f} {suffix}"
        size /= 1024.0
    return f"{size_bytes} B"


def format_modified(timestamp: float) -> str:
    if timestamp <= 0.0:
        return ""
    return datetime.fromtimestamp(timestamp).strftime("%Y-%m-%d %H:%M:%S")


def format_body_counts(counts: Counter[int], limit: int = 12) -> str:
    items = [(int(body_id), int(count)) for body_id, count in sorted(counts.items()) if int(count) > 0]
    chunks = [f"{body}:{count}" for body, count in items[:limit]]
    if len(items) > limit:
        chunks.append(f"... +{len(items) - limit}")
    return ", ".join(chunks) if chunks else "none"


def format_candidate_details(row: MeshCandidateRow) -> str:
    stats_text = str(row.stats_path) if row.stats_path is not None else "not found"
    bbox_text = "available after export or preview"
    details = [
        f"Source: {row.source_format}",
        f"Mesh cache: {row.path}",
        f"Size: {format_size(row.size_bytes)}",
        f"Modified: {format_modified(row.modified_timestamp) or 'unknown'}",
        f"Vertices: {row.vertices}",
        f"Tetrahedra: {row.elements}",
        f"Background tets: {row.background_tets}",
        f"Body count: {row.body_count}",
        f"Body counts: {format_body_counts(row.body_counts)}",
        f"Stats: {stats_text}",
        f"Bounding box: {bbox_text}",
        f"Validation: {row.validation_status.upper()}",
    ]
    if row.validation_message:
        details.append(f"Message: {row.validation_message}")
    return "\n".join(details)


def candidate_to_row(
    candidate: Any,
    project_root: str | Path,
    *,
    validation_status: str = "unknown",
    validation_message: str = "",
    stats_path: Path | None = None,
) -> MeshCandidateRow:
    path = Path(candidate.path).resolve()
    root = Path(project_root).resolve()
    try:
        stat = path.stat()
        size_bytes = int(stat.st_size)
        modified_timestamp = float(stat.st_mtime)
    except FileNotFoundError:
        size_bytes = 0
        modified_timestamp = 0.0
    body_counts = Counter({int(body_id): int(count) for body_id, count in candidate.body_counts.items()})
    resolved_stats = stats_path if stats_path is not None else _sibling_stats_path(path)
    return MeshCandidateRow(
        path=path,
        relative_path=_relative_text(path, root),
        source_format=str(candidate.source_format),
        size_bytes=size_bytes,
        modified_timestamp=modified_timestamp,
        vertices=int(candidate.n_vertices),
        elements=int(candidate.n_elements),
        background_tets=int(body_counts.get(0, 0)),
        body_count=len([body_id for body_id, count in body_counts.items() if count > 0]),
        body_counts=body_counts,
        stats_path=resolved_stats,
        validation_status=validation_status,
        validation_message=validation_message,
    )


def row_to_legacy_summary(row: MeshCandidateRow) -> dict[str, object]:
    return {
        "path": row.path,
        "relative_path": row.relative_path,
        "source_format": row.source_format,
        "vertices": row.vertices,
        "elements": row.elements,
        "background_tets": row.background_tets,
        "body_count": row.body_count,
        "body_counts": Counter(row.body_counts),
        "stats_path": row.stats_path,
        "size_bytes": row.size_bytes,
        "modified_timestamp": row.modified_timestamp,
        "validation_status": row.validation_status,
        "validation_message": row.validation_message,
    }
