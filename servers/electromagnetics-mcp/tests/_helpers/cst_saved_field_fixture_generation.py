"""Contained test-only generation of one copied Line10 3 GHz E/H fixture."""

from __future__ import annotations

import hashlib
import json
import os
import re
import sys
import time
from pathlib import Path

from cst_saved_field_result_tree_probe import ProbeFailure, _digest, _load_manifest, _sha256, _text


REQUEST_SCHEMA = "mcphub.cst.saved_field.fixture_generation.request.v1"
RECEIPT_SCHEMA = "mcphub.cst.saved_field.fixture_generation.receipt.v1"
METADATA_SCHEMA = "mcphub.cst.saved_field.fixture_generation.metadata.v1"
REQUEST_KEYS = {
    "schema", "source_project", "output_root", "expected_project_sha256",
    "expected_helper_sha256", "expected_python_sha256", "source_manifest",
}
OPTIONAL_KEYS = {
    "test_stall_before_solver_ms", "test_inject_stage", "test_setup_only",
    "diagnostic_copy_mode", "full_source_manifest", "test_port2_completion",
}
MAX_METADATA_REQUEST = 64 << 10
MAX_REQUEST = 128 << 10
ALLOWED = {
    "environment-census", "environment-new", "project-open", "add-history", "run-solver", "project-save",
    "result-tree-first-child", "result-tree-next-item", "port-state-query", "port-state-read", "port2-vba", "port2-readback",
    "project-close", "environment-close",
}
FORBIDDEN = {"remote-worker", "export", "remesh", "second-start", "source-save"}


class GenerationFailure(RuntimeError):
    def __init__(self, failure_id: str, stage: str = "configuration", error_type: str = "GenerationFailure", detail: str = "", observed_selectors: list[str] | None = None) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id
        self.stage = stage
        self.error_type = error_type
        self.detail = detail
        self.observed_selectors = observed_selectors or []


class Observer:
    def __init__(self) -> None:
        self.events: list[dict[str, str]] = []
        self.counts = {name: 0 for name in sorted(ALLOWED | FORBIDDEN)}

    def call(self, name: str, operation, *args, **kwargs):
        if name not in ALLOWED:
            if name in self.counts:
                self.counts[name] += 1
            self.events.append({"name": name, "outcome": "error"})
            raise GenerationFailure("field_generation_unexpected_call")
        self.counts[name] += 1
        try:
            if os.environ.get("MCPHUB_CST_GENERATION_INJECT_STAGE") == name:
                raise RuntimeError("synthetic vendor boundary failure")
            value = operation(*args, **kwargs)
        except BaseException as exc:
            self.events.append({"name": name, "outcome": "error"})
            if isinstance(exc, GenerationFailure):
                raise
            raise GenerationFailure("field_generation_vendor_exception", name, type(exc).__name__, str(exc)) from exc
        self.events.append({"name": name, "outcome": "finish"})
        return value

    def receipt(self) -> dict[str, object]:
        return {"events": list(self.events), "counts": dict(self.counts)}


def emit(value: dict[str, object]) -> None:
    print(json.dumps(value, sort_keys=True, separators=(",", ":")), flush=True)


def metadata_only() -> int:
    raw = sys.stdin.buffer.read(MAX_METADATA_REQUEST + 1)
    try:
        if len(raw) > MAX_METADATA_REQUEST:
            raise ValueError
        value = json.loads(raw)
        if not isinstance(value, dict) or set(value) != {"schema", "source_project"} or value["schema"] != METADATA_SCHEMA:
            raise ValueError
        source = Path(_text(value["source_project"], absolute=True))
        from mcphub_em_mcp.cst_saved_field_transfer import inventory_manifest_v2
        manifest = inventory_manifest_v2(source)
        emit({"schema": METADATA_SCHEMA, "source_manifest": {"schema": manifest.schema, "aggregate_sha256": manifest.aggregate_sha256, "rows": [
            {"path": row.path, "type": row.type, "stream": row.stream, "size": row.size, "sha256": row.sha256, "modified_ns": row.modified_ns}
            for row in manifest.rows
        ]}})
        return 0
    except BaseException:
        emit({"schema": METADATA_SCHEMA, "status": "BLOCKED:metadata_invalid"})
        return 1


def read_request() -> dict[str, object]:
    raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
    try:
        value = json.loads(raw)
    except Exception as exc:
        raise GenerationFailure("field_generation_configuration_invalid") from exc
    keys = set(value) if isinstance(value, dict) else set()
    if (
        len(raw) > MAX_REQUEST or not isinstance(value, dict)
        or value.get("schema") != REQUEST_SCHEMA
        or not REQUEST_KEYS.issubset(keys) or not keys.issubset(REQUEST_KEYS | OPTIONAL_KEYS)
    ):
        raise GenerationFailure("field_generation_configuration_invalid")
    if "test_stall_before_solver_ms" in value:
        stall = value["test_stall_before_solver_ms"]
        if os.environ.get("MCPHUB_CST_GENERATION_SYNTHETIC") != "1" or type(stall) is not int or not 1 <= stall <= 30000:
            raise GenerationFailure("field_generation_configuration_invalid")
    if "test_inject_stage" in value:
        stage = value["test_inject_stage"]
        if os.environ.get("MCPHUB_CST_GENERATION_SYNTHETIC") != "1" or stage not in ALLOWED:
            raise GenerationFailure("field_generation_configuration_invalid")
    if "test_setup_only" in value and value["test_setup_only"] is not True:
        raise GenerationFailure("field_generation_configuration_invalid")
    if "test_port2_completion" in value and value["test_port2_completion"] is not True:
        raise GenerationFailure("field_generation_configuration_invalid")
    diagnostic_mode = value.get("diagnostic_copy_mode")
    if diagnostic_mode not in (None, "five-row", "full-bundle"):
        raise GenerationFailure("field_generation_configuration_invalid")
    if diagnostic_mode is not None and "full_source_manifest" not in value:
        raise GenerationFailure("field_generation_configuration_invalid")
    if diagnostic_mode is None and os.environ.get("MCPHUB_CST_GENERATION_SYNTHETIC") != "1" and "full_source_manifest" not in value:
        raise GenerationFailure("field_generation_configuration_invalid")
    return value


def _redact_diagnostic(value: str, source: Path, copied: Path | None, output_root: Path) -> str:
    text = value[:2048]
    for raw, replacement in ((str(source), "<source>"), (str(copied) if copied else "", "<copy>"), (str(output_root), "<output>")):
        if raw:
            text = text.replace(raw, replacement)
    text = re.sub(r"(?i)(password|token|secret)[^\s:=]*\s*[:=]\s*\S+", "<redacted-credential>", text)
    return text


def running(interface, observer: Observer) -> set[int]:
    values = observer.call("environment-census", interface.running_design_environments)
    return {int(value) for value in values}


def tree_selectors(tree, observer: Observer) -> list[str]:
    root = "2D/3D Results"
    seen: set[str] = set()
    result: list[str] = []

    def walk(parent: str, depth: int) -> None:
        if depth >= 64:
            raise GenerationFailure("field_generation_incomplete")
        current = observer.call("result-tree-first-child", tree.GetFirstChildName, parent)
        while current:
            if not isinstance(current, str) or not current.startswith(root + "\\") or current in seen:
                raise GenerationFailure("field_generation_incomplete")
            seen.add(current)
            result.append(current)
            if len(result) > 4096:
                raise GenerationFailure("field_generation_incomplete")
            walk(current, depth + 1)
            current = observer.call("result-tree-next-item", tree.GetNextItemName, current)

    walk(root, 0)
    return result


def classify_results(selectors: list[str]) -> list[dict[str, object]]:
    rows: list[dict[str, object]] = []
    for selector in selectors:
        lower = selector.lower()
        field = "E" if "e-field" in lower else "H" if "h-field" in lower else ""
        frequency = "3ghz" in lower or "f=3" in lower
        port_match = re.search(r"port\s*([12])", lower)
        mode_match = re.search(r"mode\s*1", lower)
        if field and frequency and port_match and mode_match:
            rows.append({"field": field, "port": int(port_match.group(1)), "mode": 1, "path": selector})
    keys = {(row["field"], row["port"], row["mode"]) for row in rows}
    if keys != {("E", 1, 1), ("E", 2, 1), ("H", 1, 1), ("H", 2, 1)} or len(rows) != 4:
        raise GenerationFailure("field_generation_incomplete", "result-tree-classification", observed_selectors=selectors[:64])
    return rows


def history_command() -> str:
    return "\n".join([
        "Solver.FrequencyRange 1.0, 20.0",
        "With Monitor", "    .Reset", '    .Name \"mcp_line10_E_3GHz\"',
        '    .FieldType \"Efield\"', '    .Domain \"frequency\"', "    .Frequency 3.0", "    .Create", "End With",
        "With Monitor", "    .Reset", '    .Name \"mcp_line10_H_3GHz\"',
        '    .FieldType \"Hfield\"', '    .Domain \"frequency\"', "    .Frequency 3.0", "    .Create", "End With",
        "With FDSolver", "    .MeshAdaptionTet False", '    .ResetSampleIntervals \"all\"',
        '    .AddSampleInterval 3.0, 3.0, 1, \"Single\", False', '    .Stimulation \"All\", \"All\"',
        "    .ModesOnly False", "    .AddMonitorSamples True", '    .SetKeepSolutionCoefficients \"Monitors\"',
        "    .ForceRecalculateOldSamples True", "End With",
    ])


def port2_completion_command() -> str:
    return "\n".join([
        "Sub Main",
        "With Solver", "    .StimulationPort 2", "    .StimulationMode 1",
        "    .ConsiderTwoPortReciprocity False", "End With",
        'SetGlobalData("MCPHUB_PORT2", CStr(Solver.GetStimulationPort) & ":" & CStr(Solver.GetStimulationMode))',
        "End Sub",
    ])


def read_port2_guard(model3d, observer: Observer) -> None:
    observer.call("port2-vba", model3d._execute_vba_code, port2_completion_command())
    if observer.call("port2-readback", model3d.GetGlobalData, "MCPHUB_PORT2") != "2:1":
        raise GenerationFailure("field_generation_port2_guard_failed", "port2-readback")


def full_bundle_manifest(project: Path):
    from mcphub_em_mcp.cst_saved_field_transfer import ManifestRowV2, ManifestV2, _canonical_aggregate

    parent = project.parent
    data = project.with_suffix("")
    members = [project]
    for path in data.rglob("*"):
        if path.is_symlink():
            raise GenerationFailure("field_generation_configuration_invalid")
        if path.is_file():
            members.append(path)
    rows = []
    for path in sorted(members, key=lambda item: item.relative_to(parent).as_posix()):
        stat = path.stat()
        rows.append(ManifestRowV2(
            path=path.relative_to(parent).as_posix(), type="regular", stream="::$DATA",
            size=stat.st_size, sha256=_sha256(path), modified_ns=stat.st_mtime_ns,
        ))
    frozen = tuple(rows)
    return ManifestV2(schema="sha256-canonical-file-list-v2", rows=frozen, aggregate_sha256=_canonical_aggregate(frozen))


def load_full_bundle_manifest(value: object):
    from mcphub_em_mcp.cst_saved_field_transfer import ManifestRowV2, ManifestV2, _canonical_aggregate

    if not isinstance(value, dict) or set(value) != {"schema", "rows", "aggregate_sha256"}:
        raise GenerationFailure("field_generation_configuration_invalid")
    rows_value = value["rows"]
    if value["schema"] != "sha256-canonical-file-list-v2" or not isinstance(rows_value, list) or not 2 <= len(rows_value) <= 10000:
        raise GenerationFailure("field_generation_configuration_invalid")
    rows = []
    for item in rows_value:
        if not isinstance(item, dict) or set(item) != {"path", "type", "stream", "size", "sha256", "modified_ns"}:
            raise GenerationFailure("field_generation_configuration_invalid")
        rows.append(ManifestRowV2(
            path=_text(item["path"]), type=item["type"], stream=item["stream"],
            size=item["size"], sha256=_digest(item["sha256"]), modified_ns=item["modified_ns"],
        ))
    frozen = tuple(rows)
    manifest = ManifestV2(schema=value["schema"], rows=frozen, aggregate_sha256=_digest(value["aggregate_sha256"]))
    if manifest.aggregate_sha256 != _canonical_aggregate(frozen):
        raise GenerationFailure("field_generation_configuration_invalid")
    return manifest


def read_port_count(model3d, observer: Observer) -> int:
    key = "MCPHUB_BUNDLE_PROBE_V1"
    code = "\n".join([
        "Sub Main",
        f'SetGlobalData("{key}", CStr(Port.StartPortNumberIteration))',
        "End Sub",
    ])
    observer.call("port-state-query", model3d._execute_vba_code, code)
    raw = observer.call("port-state-read", model3d.GetGlobalData, key)
    if not isinstance(raw, str) or not raw.isascii() or not raw.isdigit():
        raise GenerationFailure("field_generation_port_state_invalid", "port-state-read")
    return int(raw)


def execute(request: dict[str, object]) -> dict[str, object]:
    from mcphub_em_mcp.cst_saved_field_transfer import AuthorizedBundleTransfer, inventory_manifest_v2
    from mcphub_em_mcp.cst_bundle import _copy_cst_bundle

    source = Path(_text(request["source_project"], absolute=True))
    output_root = Path(_text(request["output_root"], absolute=True))
    try:
        manifest = _load_manifest(request["source_manifest"])
    except ProbeFailure as exc:
        raise GenerationFailure("field_generation_configuration_invalid", "manifest", type(exc).__name__) from exc
    diagnostic_mode = request.get("diagnostic_copy_mode")
    copy_mode = diagnostic_mode or ("five-row" if os.environ.get("MCPHUB_CST_GENERATION_SYNTHETIC") == "1" else "full-bundle")
    expected_full_manifest = load_full_bundle_manifest(request["full_source_manifest"]) if "full_source_manifest" in request else None
    if (
        not source.is_file() or not output_root.is_dir()
        or _sha256(Path(__file__).resolve()) != _digest(request["expected_helper_sha256"])
        or _sha256(Path(sys.executable).resolve()) != _digest(request["expected_python_sha256"])
        or _sha256(source) != _digest(request["expected_project_sha256"])
        or inventory_manifest_v2(source) != manifest
        or (expected_full_manifest is not None and full_bundle_manifest(source) != expected_full_manifest)
    ):
        raise GenerationFailure("field_generation_configuration_invalid")
    staging = output_root / "staging"
    final = output_root / "retained-line10-3ghz"
    if staging.exists() or final.exists():
        raise GenerationFailure("field_generation_output_exists")
    import cst.interface as interface
    observer = Observer()
    environment = None
    project = None
    snapshot = None
    before: set[int] | None = None
    success = False
    rows: list[dict[str, object]] = []
    failure: GenerationFailure | None = None
    cleanup_stage = ""
    cleanup_error_type = ""
    retain_unverified = False
    try:
        if copy_mode == "full-bundle":
            staging.mkdir()
            copied = _copy_cst_bundle(source, staging)
        else:
            snapshot = AuthorizedBundleTransfer(manifest).execute(source, staging)
            copied = staging / source.name
        before = running(interface, observer)
        environment = observer.call("environment-new", interface.DesignEnvironment.new)
        project = observer.call("project-open", environment.open_project, str(copied))
        after = running(interface, observer)
        created = after - before
        if len(created) != 1:
            raise GenerationFailure("field_generation_owned_process_ambiguous")
        owned_pid = created.pop()
        model3d = project.model3d
        if diagnostic_mode is not None:
            port_count = read_port_count(model3d, observer)
            observer.call("project-close", project.close)
            project = None
            observer.call("environment-close", environment.close)
            environment = None
            deadline = time.monotonic() + 5.0
            while owned_pid in running(interface, observer):
                if time.monotonic() >= deadline:
                    raise GenerationFailure("field_generation_settlement_failed")
                time.sleep(0.05)
            full_after = full_bundle_manifest(source)
            if running(interface, observer) != before or inventory_manifest_v2(source) != manifest or full_after != expected_full_manifest:
                raise GenerationFailure("field_generation_preservation_failed")
            if snapshot is not None:
                snapshot.settle()
            return {
                "schema": RECEIPT_SCHEMA, "status": "PASS:bundle_port_state_inspected", "failure_id": "",
                "stage": "", "exception_type": "", "compiler_diagnostic": "",
                "source_manifest_sha256": manifest.aggregate_sha256, "retained_manifest_sha256": "",
                "session_identity_sha256": "", "results": [], "api_trace": observer.receipt(),
                "source_unchanged": True, "foreign_processes_preserved": True, "session_settled": True,
                "retained_bundle": "", "full_source_manifest_sha256": full_after.aggregate_sha256,
                "full_source_row_count": len(full_after.rows),
                "bundle_probe": {"copy_mode": diagnostic_mode, "port_count": port_count},
            }
        port_count = None
        if copy_mode == "full-bundle":
            port_count = read_port_count(model3d, observer)
            if port_count != 2:
                raise GenerationFailure("field_generation_port_count_invalid", "port-state-read")
        if request.get("test_port2_completion") is True:
            read_port2_guard(model3d, observer)
            if observer.call("run-solver", model3d.run_solver) is not True:
                raise GenerationFailure("field_generation_solver_failed")
            observer.call("project-save", project.save, include_results=True)
            selectors = tree_selectors(model3d.ResultTree, observer)
            raise GenerationFailure("field_generation_incomplete", "postsolve-observation", observed_selectors=selectors[:64])
        observer.call("add-history", model3d.add_to_history, "mcphub: 3 GHz E/H retained fixture", history_command())
        if request.get("test_setup_only") is True:
            observer.call("project-close", project.close)
            project = None
            observer.call("environment-close", environment.close)
            environment = None
            if snapshot is not None:
                snapshot.settle()
            return {
                "schema": RECEIPT_SCHEMA, "status": "PASS:setup_only", "failure_id": "",
                "stage": "", "exception_type": "", "compiler_diagnostic": "",
                "source_manifest_sha256": manifest.aggregate_sha256, "retained_manifest_sha256": "",
                "session_identity_sha256": "", "results": [], "api_trace": observer.receipt(),
                "source_unchanged": inventory_manifest_v2(source) == manifest,
                "foreign_processes_preserved": running(interface, observer) == before,
                "session_settled": True, "retained_bundle": "",
            }
        stall = request.get("test_stall_before_solver_ms")
        if type(stall) is int:
            time.sleep(stall / 1000.0)
        if observer.call("run-solver", model3d.run_solver) is not True:
            raise GenerationFailure("field_generation_solver_failed")
        observer.call("project-save", project.save, include_results=True)
        rows = classify_results(tree_selectors(model3d.ResultTree, observer))
        observer.call("project-close", project.close)
        project = None
        observer.call("environment-close", environment.close)
        environment = None
        deadline = time.monotonic() + 5.0
        while owned_pid in running(interface, observer):
            if time.monotonic() >= deadline:
                raise GenerationFailure("field_generation_settlement_failed")
            time.sleep(0.05)
        if running(interface, observer) != before or inventory_manifest_v2(source) != manifest or (expected_full_manifest is not None and full_bundle_manifest(source) != expected_full_manifest):
            raise GenerationFailure("field_generation_preservation_failed")
        retained_manifest = inventory_manifest_v2(copied)
        os.replace(staging, final)
        if snapshot is not None:
            snapshot.settle()
        success = True
        return {
            "schema": RECEIPT_SCHEMA, "status": "PASS:copied_line10_fields_generated", "failure_id": "",
            "source_manifest_sha256": manifest.aggregate_sha256,
            "retained_manifest_sha256": retained_manifest.aggregate_sha256,
            "session_identity_sha256": hashlib.sha256(f"generation-session-v1\0{owned_pid}\0{manifest.aggregate_sha256}".encode()).hexdigest(),
            "results": rows, "api_trace": observer.receipt(), "source_unchanged": True,
            "foreign_processes_preserved": True, "session_settled": True,
            "retained_bundle": final.name,
            "full_source_manifest_sha256": expected_full_manifest.aggregate_sha256 if expected_full_manifest is not None else "",
            "full_source_row_count": len(expected_full_manifest.rows) if expected_full_manifest is not None else 0,
            "bundle_probe": {"copy_mode": copy_mode, "port_count": port_count} if port_count is not None else None,
        }
    except GenerationFailure as exc:
        failure = exc
        retain_unverified = observer.counts["run-solver"] == 1 and observer.counts["project-save"] == 1 and (
            exc.failure_id == "field_generation_incomplete" or exc.stage == "project-save"
        )
    except BaseException as exc:
        failure = GenerationFailure("field_generation_execution_failed", "unclassified", type(exc).__name__)
    finally:
        if project is not None:
            try:
                observer.call("project-close", project.close)
            except GenerationFailure as exc:
                cleanup_stage, cleanup_error_type = exc.stage, exc.error_type
            else:
                project = None
        if environment is not None:
            try:
                observer.call("environment-close", environment.close)
            except GenerationFailure as exc:
                if not cleanup_stage:
                    cleanup_stage, cleanup_error_type = exc.stage, exc.error_type
            else:
                environment = None
        if not success and not retain_unverified:
            if snapshot is not None:
                try: snapshot.settle()
                except BaseException as exc:
                    if not cleanup_stage:
                        cleanup_stage, cleanup_error_type = "copy-settle", type(exc).__name__
            import shutil
            for owned in (staging, final):
                if owned.exists():
                    shutil.rmtree(owned)

    if failure is not None:
        source_unchanged = False
        try:
            source_unchanged = inventory_manifest_v2(source) == manifest
        except BaseException:
            pass
        source_full_unchanged = expected_full_manifest is None
        if expected_full_manifest is not None:
            try:
                source_full_unchanged = full_bundle_manifest(source) == expected_full_manifest
            except BaseException:
                source_full_unchanged = False
        if retain_unverified and source_unchanged and source_full_unchanged and before is not None and running(interface, observer) == before and environment is None and project is None and not cleanup_stage:
            unverified = output_root / "unverified-line10-3ghz"
            if not unverified.exists() and staging.exists():
                os.replace(staging, unverified)
                return {
                    "schema": RECEIPT_SCHEMA, "status": "UNVERIFIED:postsolve_result_tree", "failure_id": failure.failure_id,
                    "stage": failure.stage, "exception_type": failure.error_type, "compiler_diagnostic": _redact_diagnostic(failure.detail, source, staging / source.name, output_root),
                    "cleanup_stage": "", "cleanup_exception_type": "", "source_manifest_sha256": manifest.aggregate_sha256,
                    "retained_manifest_sha256": "", "session_identity_sha256": "", "results": [], "api_trace": observer.receipt(),
                    "source_unchanged": True, "foreign_processes_preserved": True, "session_settled": True, "retained_bundle": unverified.name,
                    "full_source_manifest_sha256": expected_full_manifest.aggregate_sha256 if expected_full_manifest is not None else "",
                    "full_source_row_count": len(expected_full_manifest.rows) if expected_full_manifest is not None else 0,
                    "bundle_probe": {"copy_mode": copy_mode, "port_count": locals().get("port_count")},
                    "observed_selectors": failure.observed_selectors,
                }
        return {
            "schema": RECEIPT_SCHEMA,
            "status": f"BLOCKED:{failure.failure_id}",
            "failure_id": failure.failure_id,
            "stage": failure.stage,
            "exception_type": failure.error_type,
            "compiler_diagnostic": _redact_diagnostic(failure.detail, source, staging / source.name, output_root),
            "cleanup_stage": cleanup_stage,
            "cleanup_exception_type": cleanup_error_type,
            "source_manifest_sha256": manifest.aggregate_sha256,
            "retained_manifest_sha256": "",
            "session_identity_sha256": "",
            "results": [],
            "api_trace": observer.receipt(),
            "source_unchanged": source_unchanged,
            "foreign_processes_preserved": before is not None,
            "session_settled": environment is None and project is None,
            "retained_bundle": "",
            "full_source_manifest_sha256": expected_full_manifest.aggregate_sha256 if expected_full_manifest is not None else "",
            "full_source_row_count": len(expected_full_manifest.rows) if expected_full_manifest is not None else 0,
            "bundle_probe": {"copy_mode": copy_mode, "port_count": locals().get("port_count")} if locals().get("port_count") is not None else None,
            "observed_selectors": [],
        }


def main() -> int:
    try:
        receipt = execute(read_request())
    except GenerationFailure as exc:
        receipt = {
            "schema": RECEIPT_SCHEMA, "status": f"BLOCKED:{exc.failure_id}", "failure_id": exc.failure_id,
            "stage": exc.stage, "exception_type": exc.error_type, "cleanup_stage": "", "cleanup_exception_type": "",
            "results": [], "api_trace": {"events": [], "counts": {name: 0 for name in sorted(ALLOWED | FORBIDDEN)}},
            "source_unchanged": False, "foreign_processes_preserved": False, "session_settled": False, "retained_bundle": "",
        }
    except BaseException as exc:
        receipt = {
            "schema": RECEIPT_SCHEMA, "status": "BLOCKED:field_generation_execution_failed", "failure_id": "field_generation_execution_failed",
            "stage": "unclassified", "exception_type": type(exc).__name__, "cleanup_stage": "", "cleanup_exception_type": "",
            "results": [], "api_trace": {"events": [], "counts": {name: 0 for name in sorted(ALLOWED | FORBIDDEN)}},
            "source_unchanged": False, "foreign_processes_preserved": False, "session_settled": False, "retained_bundle": "",
        }
    emit(receipt)
    return 0 if str(receipt["status"]).startswith("PASS:") else 1


if __name__ == "__main__":
    raise SystemExit(metadata_only() if sys.argv[1:] == ["--metadata"] else main())
