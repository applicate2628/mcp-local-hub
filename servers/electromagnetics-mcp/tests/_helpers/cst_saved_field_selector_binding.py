"""Test-only, copied-project binding of four saved CST field selectors to payloads."""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
import sys
import time
import unicodedata
import xml.etree.ElementTree as ET
from pathlib import Path, PureWindowsPath

from cst_saved_field_fixture_generation import full_bundle_manifest, load_full_bundle_manifest
from cst_saved_field_result_tree_probe import (
    ProbeFailure,
    _digest,
    _running,
    _safe_create_owned,
    _selectors,
    _sha256,
    _text,
)


REQUEST_SCHEMA = "mcphub.cst.saved_field.selector_binding.request.v1"
RECEIPT_SCHEMA = "mcphub.cst.saved_field.selector_binding.receipt.v1"
REQUEST_KEYS = {
    "schema", "source_project", "workspace_root", "expected_project_sha256",
    "expected_helper_sha256", "expected_python_sha256", "full_manifest",
    "result_manifest", "pairs",
}
TEST_KEYS = {"test_forbidden_call", "test_stall_after_open_ms"}
MAX_REQUEST = 128 << 10
ALLOWED = {
    "environment-census", "environment-new", "project-open", "result-tree-first-child",
    "result-tree-next-item", "result-tree-file", "select-tree-item", "field-frequency",
    "project-close", "environment-close",
}
FORBIDDEN = {
    "solve", "remesh", "history", "export", "project-save", "3DSave",
    "GetFieldVector", "GetResultFromTreeItem", "Result3D", "registration",
}
SELECTOR_LEAF = re.compile(r"mcp_line10_([EH])_3GHz \[([0-9]+)\]\Z")
DIAGNOSTIC_MAX_BYTES = 8 << 10
DIAGNOSTIC_MAX_LABELS = 32
DIAGNOSTIC_LABEL_BYTES = 96


class Observer:
    def __init__(self, source: Path | None = None, workspace: Path | None = None) -> None:
        self.events: list[dict[str, str]] = []
        self.counts = {name: 0 for name in sorted(ALLOWED | FORBIDDEN)}
        self.returned_count = 0
        self.labels: list[dict[str, str]] = []
        self.labels_truncated = False
        self.private_components = {
            part.casefold()
            for path in (source, workspace, Path.home()) if path is not None
            for part in path.parts if len(part) > 1
        }
        if source is not None:
            self.private_components.add(source.stem.casefold())

    def _component(self, value: str) -> tuple[str, bool]:
        if (
            not value or value in {".", ".."} or value.casefold() in self.private_components
            or any(char in value for char in ("/", "\\", ":"))
            or any(unicodedata.category(char).startswith("C") for char in value)
        ):
            return "<redacted>", False
        encoded = value.encode("utf-8")
        if len(encoded) <= DIAGNOSTIC_LABEL_BYTES:
            return value, False
        result = ""
        byte_count = 0
        for char in value:
            char_bytes = len(char.encode("utf-8"))
            if byte_count + char_bytes > DIAGNOSTIC_LABEL_BYTES:
                break
            result += char
            byte_count += char_bytes
        return result, True

    def _label(self, value: object) -> tuple[dict[str, str], bool]:
        if not isinstance(value, str) or PureWindowsPath(value).drive or value.startswith(("/", "\\")):
            return {"parent": "<redacted>", "leaf": "<redacted>"}, False
        parts = value.split("\\")
        if len(parts) < 3 or parts[0] != "2D/3D Results":
            return {"parent": "<redacted>", "leaf": "<redacted>"}, False
        checked = [self._component(part) for part in parts[1:]]
        if any(part == "<redacted>" or safe == "<redacted>" for part, (safe, _) in zip(parts[1:], checked)):
            return {"parent": "<redacted>", "leaf": "<redacted>"}, False
        parent, parent_clipped = checked[-2]
        leaf, leaf_clipped = checked[-1]
        return {"parent": parent, "leaf": leaf}, parent_clipped or leaf_clipped

    def _returned(self, value: object) -> None:
        if not value:
            return
        self.returned_count += 1
        if len(self.labels) == DIAGNOSTIC_MAX_LABELS:
            self.labels_truncated = True
            return
        label, clipped = self._label(value)
        self.labels.append(label)
        self.labels_truncated = self.labels_truncated or clipped

    def diagnostic(self, branch: str, candidate_count: int | None, *, offender: str | None = None, offender_index: int | None = None) -> dict[str, object]:
        labels = list(self.labels)
        truncated = self.labels_truncated
        reserve_last = offender is not None and offender_index is not None and offender_index >= DIAGNOSTIC_MAX_LABELS
        if reserve_last:
            label, _ = self._label(offender)
            labels[-1] = label
            truncated = True
        diagnostic: dict[str, object] = {
            "branch": branch, "total_count": self.returned_count,
            "candidate_count": candidate_count, "labels": labels, "truncated": truncated,
        }
        while len(json.dumps(diagnostic, sort_keys=True, separators=(",", ":")).encode("utf-8")) > DIAGNOSTIC_MAX_BYTES:
            if not labels:
                break
            labels.pop(-2 if reserve_last and len(labels) > 1 else -1)
            diagnostic["truncated"] = True
        return diagnostic

    def call(self, name: str, operation, *args):
        if name not in ALLOWED:
            if name in self.counts:
                self.counts[name] += 1
            self.events.append({"name": name, "outcome": "error"})
            raise ProbeFailure("selector_payload_ambiguous")
        self.counts[name] += 1
        try:
            value = operation(*args)
        except BaseException:
            self.events.append({"name": name, "outcome": "error"})
            raise
        self.events.append({"name": name, "outcome": "finish"})
        if name in {"result-tree-first-child", "result-tree-next-item"}:
            self._returned(value)
        return value

    def snapshot(self) -> dict[str, object]:
        return {"events": list(self.events), "counts": dict(self.counts)}


def _blocked(failure_id: str, **fields: object) -> dict[str, object]:
    return {"schema": RECEIPT_SCHEMA, "status": f"BLOCKED:{failure_id}", "failure_id": failure_id, **fields}


def _read_request() -> dict[str, object]:
    raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProbeFailure("probe_configuration_invalid") from exc
    if len(raw) > MAX_REQUEST or not isinstance(value, dict) or value.get("schema") != REQUEST_SCHEMA:
        raise ProbeFailure("probe_configuration_invalid")
    keys = set(value)
    if not REQUEST_KEYS.issubset(keys) or not keys.issubset(REQUEST_KEYS | TEST_KEYS):
        raise ProbeFailure("probe_configuration_invalid")
    if keys & TEST_KEYS:
        if os.environ.get("MCPHUB_CST_BINDING_SYNTHETIC") != "1":
            raise ProbeFailure("probe_configuration_invalid")
        if "test_forbidden_call" in value and value["test_forbidden_call"] not in FORBIDDEN:
            raise ProbeFailure("probe_configuration_invalid")
        if "test_stall_after_open_ms" in value and (
            type(value["test_stall_after_open_ms"]) is not int
            or not 1 <= value["test_stall_after_open_ms"] <= 30_000
        ):
            raise ProbeFailure("probe_configuration_invalid")
    return value


def _result_manifest(full, project_name: str):
    from mcphub_em_mcp.cst_saved_field_transfer import ManifestV2, _canonical_aggregate

    prefix = Path(project_name).stem + "/Result/"
    rows = tuple(row for row in full.rows if row.path == Path(project_name).name or row.path.startswith(prefix))
    if not rows:
        raise ProbeFailure("probe_configuration_invalid")
    return ManifestV2(full.schema, rows, _canonical_aggregate(rows))


def _admit(request: dict[str, object]):
    from mcphub_em_mcp.cst_saved_field_transfer import inventory_manifest_v2

    source = Path(_text(request["source_project"], absolute=True))
    workspace = Path(_text(request["workspace_root"], absolute=True))
    if (
        source != Path(os.path.abspath(source)) or workspace != Path(os.path.abspath(workspace))
        or source.suffix.lower() != ".cst" or not source.is_file() or source.is_symlink()
        or not workspace.is_dir() or workspace.is_symlink() or any(workspace.iterdir())
    ):
        raise ProbeFailure("probe_configuration_invalid")
    try:
        full = load_full_bundle_manifest(request["full_manifest"])
        result = load_full_bundle_manifest(request["result_manifest"])
    except Exception as exc:
        raise ProbeFailure("probe_configuration_invalid") from exc
    if (
        _sha256(Path(__file__).resolve()) != _digest(request["expected_helper_sha256"])
        or _sha256(Path(sys.executable).resolve()) != _digest(request["expected_python_sha256"])
        or _sha256(source) != _digest(request["expected_project_sha256"])
        or full_bundle_manifest(source) != full
        or _result_manifest(full, source.name) != result
        or _result_manifest(inventory_manifest_v2(source), source.name) != result
    ):
        raise ProbeFailure("probe_configuration_invalid")
    pairs = request["pairs"]
    if not isinstance(pairs, list) or len(pairs) != 4:
        raise ProbeFailure("probe_configuration_invalid")
    expected = {(field, port) for field in ("E", "H") for port in (1, 2)}
    actual = set()
    rows = {row.path: row for row in result.rows}
    for pair in pairs:
        if not isinstance(pair, dict) or set(pair) != {
            "field", "port", "m3d_path", "m3d_sha256", "rex_path", "rex_sha256",
        }:
            raise ProbeFailure("probe_configuration_invalid")
        field, port = pair["field"], pair["port"]
        if type(port) is not int or (field, port) not in expected:
            raise ProbeFailure("probe_configuration_invalid")
        actual.add((field, port))
        base = f"{source.stem}/Result/mcp_line10_{field}_3GHz_{port},1"
        if (
            pair["m3d_path"] != base + ".m3d"
            or pair["rex_path"] != base + "_m3d.rex"
            or rows.get(pair["m3d_path"]) is None
            or rows.get(pair["rex_path"]) is None
            or rows[pair["m3d_path"]].sha256 != _digest(pair["m3d_sha256"])
            or rows[pair["rex_path"]].sha256 != _digest(pair["rex_sha256"])
        ):
            raise ProbeFailure("selector_payload_ambiguous")
    if actual != expected:
        raise ProbeFailure("probe_configuration_invalid")
    return source, workspace, full, result, pairs


def _resolve_file(returned: object, copied: Path, pairs: list[dict[str, object]]) -> tuple[str, Path]:
    if not isinstance(returned, str) or not returned or "\x00" in returned:
        raise ProbeFailure("selector_payload_ambiguous")
    result_root = copied.with_suffix("") / "Result"
    if result_root.is_symlink() or (hasattr(result_root, "is_junction") and result_root.is_junction()):
        raise ProbeFailure("selector_payload_ambiguous")
    raw = PureWindowsPath(returned)
    if raw.is_absolute():
        try:
            relative = Path(returned).relative_to(result_root)
        except ValueError as exc:
            raise ProbeFailure("selector_payload_ambiguous") from exc
        parts = relative.parts
    else:
        parts = raw.parts
        if parts[:2] == (copied.stem, "Result"):
            parts = parts[2:]
        elif parts[:1] == ("Result",):
            parts = parts[1:]
    if not parts or any(part in {"", ".", ".."} for part in parts):
        raise ProbeFailure("selector_payload_ambiguous")
    expected_paths = {str(pair[key]): pair for pair in pairs for key in ("m3d_path", "rex_path")}
    matches = [relative for relative in expected_paths if Path(relative).parts[-len(parts):] == parts]
    if len(matches) != 1:
        raise ProbeFailure("selector_payload_ambiguous")
    relative = matches[0]
    resolved = copied.parent / Path(relative)
    if resolved != result_root / resolved.name or not resolved.is_file():
        raise ProbeFailure("selector_payload_ambiguous")
    current = result_root
    for part in resolved.relative_to(result_root).parts:
        current = current / part
        if current.is_symlink() or (hasattr(current, "is_junction") and current.is_junction()):
            raise ProbeFailure("selector_payload_ambiguous")
    return relative, resolved


def _check_rex(path: Path, selector: str, pair: dict[str, object]) -> None:
    try:
        root = ET.parse(path).getroot()
        simulation = root.findall("SimulationProperties")
        data_type = root.findall("ResultDataType")
        linked = root.findall(".//FieldResultFile")
    except (OSError, ET.ParseError) as exc:
        raise ProbeFailure("selector_payload_ambiguous") from exc
    field = pair["field"]
    unit = "&U:V^1.:m^-1" if field == "E" else "&U:A^1.:m^-1"
    if (
        root.tag != "MetaResultFile" or len(simulation) != 1 or len(data_type) != 1
        or len(linked) != 1
        or simulation[0].get("fieldname") != selector.rsplit("\\", 1)[-1]
        or simulation[0].get("fieldtype") != f"{field}-Field"
        or simulation[0].get("encoded_unit") != unit
        or simulation[0].get("fieldscaling") != "PEAK"
        or simulation[0].get("frequency") != "3"
        or data_type[0].get("vector") != "1" or data_type[0].get("complex") != "1"
        or linked[0].get("type") != "m3d"
        or linked[0].get("filename") != Path(str(pair["m3d_path"])).name
    ):
        raise ProbeFailure("selector_payload_ambiguous")


def _bind(resource, copied: Path, pairs: list[dict[str, object]], observer: Observer) -> list[dict[str, object]]:
    try:
        selectors = _selectors(resource.tree, observer)
    except ProbeFailure as exc:
        raise ProbeFailure("selector_payload_ambiguous", receipt_fields={
            "selector_diagnostic": observer.diagnostic("traversal_invalid", None),
        }) from exc
    leaf_candidates = [selector for selector in selectors if SELECTOR_LEAF.fullmatch(selector.rsplit("\\", 1)[-1])]
    candidates = []
    first_wrong_parent = None
    for selector in leaf_candidates:
        field = SELECTOR_LEAF.fullmatch(selector.rsplit("\\", 1)[-1]).group(1)
        parts = selector.rsplit("\\", 2)
        if len(parts) == 3 and parts[-2] == f"{field}-Field":
            candidates.append(selector)
        elif first_wrong_parent is None:
            first_wrong_parent = selector
    if len(candidates) != 4:
        if len(leaf_candidates) == 4 and first_wrong_parent is not None:
            raise ProbeFailure("selector_payload_ambiguous", receipt_fields={
                "selector_diagnostic": observer.diagnostic(
                    "parent_segment_mismatch", len(leaf_candidates),
                    offender=first_wrong_parent, offender_index=selectors.index(first_wrong_parent),
                ),
            })
        raise ProbeFailure("selector_payload_ambiguous", receipt_fields={
            "selector_diagnostic": observer.diagnostic("candidate_count_mismatch", len(leaf_candidates)),
        })
    tree = resource.tree
    model = resource.project.model3d
    lookup = getattr(tree, "GetFileFromTreeItem", None)
    select = getattr(model, "SelectTreeItem", None)
    frequency = getattr(model, "GetFieldFrequency", None)
    if not all(callable(method) for method in (lookup, select, frequency)):
        raise ProbeFailure("selector_binding_api_unavailable")
    matched = []
    seen: set[tuple[str, int]] = set()
    for selector in candidates:
        leaf = selector.rsplit("\\", 1)[-1]
        match = SELECTOR_LEAF.fullmatch(leaf)
        if match is None:
            raise ProbeFailure("selector_payload_ambiguous")
        field, run_id = match.groups()
        try:
            returned = observer.call("result-tree-file", lookup, selector)
        except Exception as exc:
            raise ProbeFailure("selector_binding_api_unavailable") from exc
        relative, path = _resolve_file(returned, copied, pairs)
        pair = next((p for p in pairs if relative in (p["m3d_path"], p["rex_path"])), None)
        if pair is None or pair["field"] != field or (field, pair["port"]) in seen:
            raise ProbeFailure("selector_payload_ambiguous")
        seen.add((field, pair["port"]))
        rex = copied.parent / str(pair["rex_path"])
        m3d = copied.parent / str(pair["m3d_path"])
        if (
            _sha256(path) != (pair["rex_sha256"] if path == rex else pair["m3d_sha256"])
            or _sha256(rex) != pair["rex_sha256"] or _sha256(m3d) != pair["m3d_sha256"]
        ):
            raise ProbeFailure("selector_payload_ambiguous")
        _check_rex(rex, selector, pair)
        matched.append((selector, run_id, returned, pair))
    if seen != {(field, port) for field in ("E", "H") for port in (1, 2)}:
        raise ProbeFailure("selector_payload_ambiguous")
    rows = []
    for selector, run_id, returned, pair in matched:
        try:
            selected = observer.call("select-tree-item", select, selector)
        except Exception as exc:
            raise ProbeFailure("selector_binding_api_unavailable") from exc
        if selected is not True:
            raise ProbeFailure("selector_binding_api_unavailable")
        try:
            raw_frequency = observer.call("field-frequency", frequency)
        except Exception as exc:
            raise ProbeFailure("selector_binding_api_unavailable") from exc
        if type(raw_frequency) not in (int, float) or not math.isfinite(raw_frequency) or raw_frequency != 3:
            raise ProbeFailure("selector_frequency_mismatch")
        returned_relative, _ = _resolve_file(returned, copied, pairs)
        rows.append({
            "field": pair["field"], "port": pair["port"], "selector": selector,
            "run_id": run_id, "returned_file": returned_relative,
            "m3d_path": pair["m3d_path"], "m3d_sha256": pair["m3d_sha256"],
            "rex_path": pair["rex_path"], "rex_sha256": pair["rex_sha256"],
            "raw_frequency": float(raw_frequency),
        })
    return rows


def _execute(request: dict[str, object]) -> dict[str, object]:
    from mcphub_em_mcp.cst_bundle import _copy_cst_bundle
    from mcphub_em_mcp.cst_saved_field_transfer import inventory_manifest_v2
    from mcphub_em_mcp.cst_saved_field_vendor import open_owned_sampler_session

    source, workspace, full, result, pairs = _admit(request)
    destination = workspace / "bundle"
    destination.mkdir()
    copied = _copy_cst_bundle(source, destination)
    if full_bundle_manifest(copied) != full or _result_manifest(inventory_manifest_v2(copied), copied.name) != result:
        raise ProbeFailure("probe_preservation_failed")
    import cst.interface as interface

    observer = Observer(source, workspace)
    foreign_before = None
    baseline_census_error = False
    try:
        foreign_before = _running(interface, observer)
    except BaseException:
        baseline_census_error = True
    owned = None
    resource = None
    rows: list[dict[str, object]] = []
    failure: ProbeFailure | None = None
    closed = False
    absent = False
    try:
        if baseline_census_error:
            raise ProbeFailure("probe_preservation_failed")
        owned, acquisition = open_owned_sampler_session(
            lambda path: _safe_create_owned(interface, Path(path), result, observer), copied
        )
        if not acquisition.transfer_committed or acquisition.handles_received != 1:
            raise ProbeFailure("probe_preservation_failed")
        resource = owned._resource
        if os.environ.get("MCPHUB_CST_BINDING_SYNTHETIC") == "1" and os.environ.get("MCPHUB_CST_BINDING_FAULT") == "copy-drift-before-timeout":
            with (copied.parent / str(pairs[0]["m3d_path"])).open("ab") as stream:
                stream.write(b"synthetic-copy-drift")
        if "test_stall_after_open_ms" in request:
            time.sleep(request["test_stall_after_open_ms"] / 1000)
        rows = _bind(resource, copied, pairs, observer)
        if "test_forbidden_call" in request:
            observer.call(request["test_forbidden_call"], lambda: None)
    except ProbeFailure as exc:
        failure = exc
        rows = []
    except BaseException:
        failure = ProbeFailure("probe_preservation_failed")
        rows = []
    finally:
        if owned is not None:
            if os.environ.get("MCPHUB_CST_BINDING_SYNTHETIC") == "1" and (
                os.environ.get("MCPHUB_CST_BINDING_FAULT") == "copy-metadata-delta"
                or os.environ.get("MCPHUB_CST_BINDING_COPY_DELTA") == "1"
            ):
                try:
                    (copied.with_suffix("") / "Result" / "Storage.sdb").write_bytes(b"synthetic copy metadata after")
                except BaseException:
                    sys.stderr.write("synthetic copy metadata injection failed\n")
                    if failure is None:
                        failure = ProbeFailure("probe_preservation_failed")
                        rows = []
            try:
                owned.clear_geometry_data_cache()
                owned.close_without_save()
                closed = True
                absent = bool(owned.is_absent())
            except BaseException:
                closed = False
                absent = False
    postcheck_errors: list[str] = []

    def observe(label, operation):
        try:
            return operation()
        except BaseException as exc:
            postcheck_errors.append(f"{label}:{type(exc).__name__}")
            return None

    foreign_after = observe("foreign-census", lambda: _running(interface, observer))
    source_after = observe("source-full", lambda: full_bundle_manifest(source))
    copy_after = observe("copy-full", lambda: full_bundle_manifest(copied))
    source_result_after = observe("source-result", lambda: _result_manifest(inventory_manifest_v2(source), source.name))
    copy_result_after = observe("copy-result", lambda: _result_manifest(inventory_manifest_v2(copied), copied.name))
    if postcheck_errors:
        sys.stderr.write("binding postchecks unavailable: " + ",".join(postcheck_errors) + "\n")
    source_unchanged = source_after is not None and source_result_after is not None and source_after == full and source_result_after == result
    copy_unchanged = copy_after is not None and copy_result_after is not None and copy_after == full and copy_result_after == result
    foreign_preserved = foreign_before is not None and foreign_after is not None and foreign_after == foreign_before
    session_settled = (closed and absent) if owned is not None else foreign_preserved
    common = {
        "full_manifest_sha256": full.aggregate_sha256,
        "result_manifest_sha256": result.aggregate_sha256,
        "copy_manifest_sha256": copy_after.aggregate_sha256 if copy_after is not None else "",
        "copy_result_sha256": copy_result_after.aggregate_sha256 if copy_result_after is not None else "",
        "source_unchanged": source_unchanged,
        "copy_unchanged": copy_unchanged,
        "foreign_processes_preserved": foreign_preserved,
        "session_settled": session_settled,
        "closed_without_save": closed,
        "api_trace": observer.snapshot(),
    }
    if failure is not None:
        diagnostic = failure.receipt_fields.get("selector_diagnostic")
        if diagnostic is not None:
            common["selector_diagnostic"] = diagnostic
        return _blocked(failure.failure_id, rows=[], **common)
    return {"schema": RECEIPT_SCHEMA, "status": "PASS:selector_payload_bound", "failure_id": "", "rows": rows, **common}


def main() -> int:
    try:
        request = _read_request()
        receipt = _execute(request)
    except ProbeFailure as exc:
        receipt = _blocked(exc.failure_id)
    except BaseException:
        receipt = _blocked("probe_preservation_failed")
    if os.environ.get("MCPHUB_CST_BINDING_SYNTHETIC") == "1" and receipt.get("status") == "PASS:selector_payload_bound":
        if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "receipt-duplicate-selector":
            receipt["rows"][1]["selector"] = receipt["rows"][0]["selector"]
        if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "receipt-wrong-runid":
            receipt["rows"][1]["run_id"] = "999"
    if os.environ.get("MCPHUB_CST_BINDING_SYNTHETIC") == "1" and os.environ.get("MCPHUB_CST_BINDING_FAULT") == "malformed-after-copy":
        sys.stdout.write("synthetic vendor text before receipt\n")
    sys.stdout.write(json.dumps(receipt, sort_keys=True, separators=(",", ":")) + "\n")
    sys.stdout.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
