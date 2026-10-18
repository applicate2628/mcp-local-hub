"""Contained, test-only CST ResultTree selector probe."""

from __future__ import annotations

import json
import hashlib
import os
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import NoReturn


RECEIPT_SCHEMA = "mcphub.cst.saved_field.result_tree_probe.receipt.v1"
REQUEST_SCHEMA = "mcphub.cst.saved_field.result_tree_probe.request.v1"
REQUEST_KEYS = {
    "schema",
    "source_project",
    "workspace_root",
    "expected_project_sha256",
    "expected_helper_sha256",
    "expected_python_sha256",
    "source_manifest",
}
OPTIONAL_REQUEST_KEYS = {"test_stall_after_open_ms", "test_preservation_mutation"}
MAX_REQUEST_BYTES = 64 << 10
MAX_TREE_DEPTH = 64
MAX_TREE_NODES = 4096
MAX_SELECTOR_BYTES = 1 << 20
ALLOWED_API_CALLS = {
    "environment-census",
    "environment-new",
    "project-open",
    "result-tree-first-child",
    "result-tree-next-item",
    "project-close",
    "environment-close",
}
FORBIDDEN_API_CALLS = {"solve", "remesh", "history", "export", "project-save", "3DSave"}


class ProbeFailure(RuntimeError):
    def __init__(
        self,
        failure_id: str,
        *,
        preservation: dict[str, object] | None = None,
        api_trace: dict[str, object] | None = None,
        receipt_fields: dict[str, object] | None = None,
    ) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id
        self.preservation = preservation
        self.api_trace = api_trace
        self.receipt_fields = receipt_fields or {}


class _APICallObserver:
    def __init__(self) -> None:
        self._events: list[dict[str, str]] = []
        self._counts = {name: 0 for name in sorted(ALLOWED_API_CALLS | FORBIDDEN_API_CALLS)}

    def snapshot(self) -> dict[str, object]:
        return {
            "events": list(self._events),
            "counts": dict(self._counts),
        }

    def call(self, name: str, operation, *args):
        if name not in ALLOWED_API_CALLS:
            if name in self._counts:
                self._counts[name] += 1
            self._events.append({"name": name, "outcome": "error"})
            raise ProbeFailure("unexpected_api_call", api_trace=self.snapshot())
        self._counts[name] += 1
        try:
            value = operation(*args)
        except BaseException:
            self._events.append({"name": name, "outcome": "error"})
            raise
        self._events.append({"name": name, "outcome": "finish"})
        return value


def _emit(value: dict[str, object]) -> None:
    sys.stdout.write(json.dumps(value, sort_keys=True, separators=(",", ":")))
    sys.stdout.write("\n")
    sys.stdout.flush()


def _blocked(failure_id: str) -> NoReturn:
    _emit(
        {
            "schema": RECEIPT_SCHEMA,
            "status": f"BLOCKED:{failure_id}",
            "failure_id": failure_id,
        }
    )
    raise SystemExit(0)


def _read_request() -> dict[str, object]:
    raw = sys.stdin.buffer.read(MAX_REQUEST_BYTES + 1)
    if len(raw) > MAX_REQUEST_BYTES:
        _blocked("probe_configuration_invalid")
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        _blocked("probe_configuration_invalid")
    if not isinstance(value, dict) or value.get("schema") != REQUEST_SCHEMA:
        _blocked("probe_configuration_invalid")
    keys = frozenset(value)
    if not REQUEST_KEYS.issubset(keys) or not keys.issubset(REQUEST_KEYS | OPTIONAL_REQUEST_KEYS):
        _blocked("probe_configuration_invalid")
    if "test_stall_after_open_ms" in value:
        stall = value["test_stall_after_open_ms"]
        if (
            os.environ.get("MCPHUB_CST_PROBE_SYNTHETIC") != "1"
            or type(stall) is not int
            or not 1 <= stall <= 30_000
        ):
            _blocked("probe_configuration_invalid")
    if "test_preservation_mutation" in value:
        mutation = value["test_preservation_mutation"]
        if (
            os.environ.get("MCPHUB_CST_PROBE_SYNTHETIC") != "1"
            or mutation
            not in {
                "source-change",
                "copy-change",
                "copy-mtime-only",
                "copy-missing",
                "copy-extra",
                "forbidden-call",
            }
        ):
            _blocked("probe_configuration_invalid")
    return value


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while block := stream.read(1024 * 1024):
            digest.update(block)
    return digest.hexdigest()


def _text(value: object, *, absolute: bool = False) -> str:
    if not isinstance(value, str) or not value or "\x00" in value:
        raise ProbeFailure("probe_configuration_invalid")
    if absolute and not Path(value).is_absolute():
        raise ProbeFailure("probe_configuration_invalid")
    return value


def _digest(value: object) -> str:
    text = _text(value)
    if len(text) != 64 or any(character not in "0123456789abcdef" for character in text):
        raise ProbeFailure("probe_configuration_invalid")
    return text


def _validated_paths(request: dict[str, object]) -> tuple[Path, Path]:
    source = Path(_text(request["source_project"], absolute=True))
    workspace = Path(_text(request["workspace_root"], absolute=True))
    if (
        source != Path(os.path.abspath(source))
        or workspace != Path(os.path.abspath(workspace))
        or source.suffix.lower() != ".cst"
        or not source.is_file()
        or not workspace.is_dir()
        or source.is_symlink()
        or workspace.is_symlink()
        or any(workspace.iterdir())
    ):
        raise ProbeFailure("probe_configuration_invalid")
    return source, workspace


def _load_manifest(value: object):
    from mcphub_em_mcp.cst_saved_field_transfer import ManifestRowV2, ManifestV2, _canonical_aggregate

    if not isinstance(value, dict) or set(value) != {"schema", "rows", "aggregate_sha256"}:
        raise ProbeFailure("probe_configuration_invalid")
    rows_value = value["rows"]
    if not isinstance(rows_value, list) or not rows_value:
        raise ProbeFailure("probe_configuration_invalid")
    rows = []
    seen = set()
    for item in rows_value:
        if not isinstance(item, dict) or set(item) != {
            "path",
            "type",
            "stream",
            "size",
            "sha256",
            "modified_ns",
        }:
            raise ProbeFailure("probe_configuration_invalid")
        if (
            item["type"] != "regular"
            or item["stream"] != "::$DATA"
            or type(item["size"]) is not int
            or item["size"] < 0
            or type(item["modified_ns"]) is not int
            or item["modified_ns"] < 0
        ):
            raise ProbeFailure("probe_configuration_invalid")
        path = _text(item["path"])
        if path in seen:
            raise ProbeFailure("probe_configuration_invalid")
        seen.add(path)
        rows.append(
            ManifestRowV2(
                path=path,
                type="regular",
                stream="::$DATA",
                size=item["size"],
                sha256=_digest(item["sha256"]),
                modified_ns=item["modified_ns"],
            )
        )
    try:
        manifest = ManifestV2(
            schema=_text(value["schema"]),
            rows=tuple(rows),
            aggregate_sha256=_digest(value["aggregate_sha256"]),
        )
        if manifest.aggregate_sha256 != _canonical_aggregate(manifest.rows):
            raise ProbeFailure("probe_configuration_invalid")
        return manifest
    except Exception as exc:
        raise ProbeFailure("probe_configuration_invalid") from exc


def _validate_identities(request: dict[str, object], source: Path, manifest) -> None:
    expected_helper = _digest(request["expected_helper_sha256"])
    expected_python = _digest(request["expected_python_sha256"])
    expected_project = _digest(request["expected_project_sha256"])
    if (
        _sha256(Path(__file__).resolve()) != expected_helper
        or _sha256(Path(sys.executable).resolve()) != expected_python
        or _sha256(source) != expected_project
    ):
        raise ProbeFailure("probe_configuration_invalid")
    project_rows = [row for row in manifest.rows if row.path == source.name]
    if len(project_rows) != 1 or project_rows[0].sha256 != expected_project:
        raise ProbeFailure("probe_configuration_invalid")


def _running(interface, observer: _APICallObserver) -> set[int]:
    try:
        values = observer.call("environment-census", interface.running_design_environments)
    except Exception as exc:
        if isinstance(exc, ProbeFailure):
            raise
        raise ProbeFailure(
            "documented_listing_binding_unavailable", api_trace=observer.snapshot()
        ) from exc
    if not isinstance(values, (list, tuple, set)):
        raise ProbeFailure("documented_listing_binding_unavailable")
    try:
        return {int(value) for value in values}
    except (TypeError, ValueError) as exc:
        raise ProbeFailure(
            "documented_listing_binding_unavailable", api_trace=observer.snapshot()
        ) from exc


@dataclass
class _OwnedResource:
    interface: object
    environment: object
    project: object
    tree: object
    copied_project: Path
    process_identity: int
    ownership_token: str
    foreign_before: set[int]
    observer: _APICallObserver
    final_manifest: object | None = None
    foreign_preserved: bool = False
    foreign_after: set[int] | None = None
    _closed: bool = False

    @property
    def handle(self):
        return self.tree

    def is_live(self) -> bool:
        return not self._closed and self.process_identity in _running(self.interface, self.observer)

    def clear_geometry_data_cache(self) -> None:
        # No geometry/field evaluation occurs, so this test-only resource owns no cache.
        return None

    def close_without_save(self) -> None:
        if self._closed:
            return
        errors: list[BaseException] = []
        try:
            self.observer.call("project-close", self.project.close)
        except BaseException as exc:
            errors.append(exc)
        try:
            self.observer.call("environment-close", self.environment.close)
        except BaseException as exc:
            errors.append(exc)
        self._closed = True
        try:
            from mcphub_em_mcp.cst_saved_field_transfer import inventory_manifest_v2

            self.final_manifest = inventory_manifest_v2(self.copied_project)
        except BaseException as exc:
            errors.append(exc)
        if errors:
            raise ProbeFailure("probe_preservation_failed") from errors[0]

    def is_absent(self) -> bool:
        deadline = time.monotonic() + 5.0
        while True:
            current = _running(self.interface, self.observer)
            if self.process_identity not in current:
                self.foreign_after = current
                self.foreign_preserved = current == self.foreign_before
                return True
            if time.monotonic() >= deadline:
                return False
            time.sleep(0.05)


def _safe_create_owned(
    interface, copied_project: Path, manifest, observer: _APICallObserver
):
    from mcphub_em_mcp.cst_saved_field_vendor import VendorCreateError

    before = _running(interface, observer)
    environment = None
    project = None
    try:
        design_environment = getattr(interface, "DesignEnvironment")
        constructor = getattr(design_environment, "new")
        if not callable(constructor):
            raise ProbeFailure("documented_listing_binding_unavailable")
        environment = observer.call("environment-new", constructor)
        opener = getattr(environment, "open_project")
        if not callable(opener):
            raise ProbeFailure("documented_listing_binding_unavailable")
        project = observer.call("project-open", opener, str(copied_project))
        model3d = getattr(project, "model3d")
        tree = getattr(model3d, "ResultTree")
        if not callable(getattr(tree, "GetFirstChildName", None)) or not callable(
            getattr(tree, "GetNextItemName", None)
        ):
            raise ProbeFailure("documented_listing_binding_unavailable")
        after = _running(interface, observer)
        created = after - before
        if len(created) != 1:
            raise ProbeFailure("owned_process_identity_ambiguous")
        process_identity = created.pop()
        token = hashlib.sha256(
            f"{copied_project.name}\0{manifest.aggregate_sha256}\0{process_identity}".encode()
        ).hexdigest()
        return _OwnedResource(
            interface=interface,
            environment=environment,
            project=project,
            tree=tree,
            copied_project=copied_project,
            process_identity=process_identity,
            ownership_token=token,
            foreign_before=before,
            observer=observer,
        )
    except BaseException as exc:
        if project is not None:
            try:
                observer.call("project-close", project.close)
            except BaseException:
                pass
        if environment is not None:
            try:
                observer.call("environment-close", environment.close)
            except BaseException:
                pass

        def zero_created() -> bool:
            return _running(interface, observer) == before

        raise VendorCreateError("owned ResultTree probe creation failed", verify_zero_created=zero_created) from exc


def _selectors(tree: object, observer: _APICallObserver) -> list[str]:
    root = "2D/3D Results"
    seen: set[str] = set()
    ordered: list[str] = []
    total_bytes = 0

    def walk(parent: str, depth: int) -> None:
        nonlocal total_bytes
        if depth >= MAX_TREE_DEPTH:
            raise ProbeFailure("result_tree_listing_invalid")
        current = observer.call("result-tree-first-child", tree.GetFirstChildName, parent)
        while current:
            if (
                not isinstance(current, str)
                or not current.startswith(root + "\\")
                or current in seen
            ):
                raise ProbeFailure("result_tree_listing_invalid")
            seen.add(current)
            ordered.append(current)
            total_bytes += len(current.encode("utf-8"))
            if len(ordered) > MAX_TREE_NODES or total_bytes > MAX_SELECTOR_BYTES:
                raise ProbeFailure("result_tree_listing_invalid")
            walk(current, depth + 1)
            following = observer.call("result-tree-next-item", tree.GetNextItemName, current)
            if following and (not isinstance(following, str) or following == current):
                raise ProbeFailure("result_tree_listing_invalid")
            current = following

    try:
        walk(root, 0)
    except ProbeFailure:
        raise
    except Exception as exc:
        raise ProbeFailure(
            "documented_listing_binding_unavailable", api_trace=observer.snapshot()
        ) from exc
    return ordered


def _apply_test_preservation_mutation(
    request: dict[str, object], source: Path, copied_project: Path
) -> None:
    mutation = request.get("test_preservation_mutation")
    if not isinstance(mutation, str):
        return
    source_storage = source.with_suffix("") / "Result" / "Storage.sdb"
    copied_storage = copied_project.with_suffix("") / "Result" / "Storage.sdb"
    if mutation == "source-change":
        with source_storage.open("ab") as stream:
            stream.write(b"SECRET_PRIVATE_SENTINEL-source-change")
    elif mutation == "copy-change":
        with copied_storage.open("ab") as stream:
            stream.write(b"SECRET_PRIVATE_SENTINEL-copy-change")
    elif mutation == "copy-mtime-only":
        current = copied_storage.stat()
        os.utime(
            copied_storage,
            ns=(current.st_atime_ns, current.st_mtime_ns + 1_000_000_000),
        )
    elif mutation == "copy-missing":
        copied_storage.unlink()
    elif mutation == "copy-extra":
        extra = copied_project.with_suffix("") / "Result" / "private-extra.bin"
        extra.write_bytes(b"SECRET_PRIVATE_SENTINEL-extra")


def _logical_label(relative: str, project_name: str) -> str:
    stem = Path(project_name).stem
    known = {
        project_name: "project",
        f"{stem}/Result/db.parmap": "result-root-parmap",
        f"{stem}/Result/DS/db.parmap": "result-ds-parmap",
        f"{stem}/Result/Model.res": "model-res",
        f"{stem}/Result/Storage.sdb": "storage-sdb",
    }
    label = known.get(relative)
    if label is not None:
        return label
    return "extra:" + hashlib.sha256(relative.encode("utf-8")).hexdigest()


def _row_state(row) -> dict[str, object]:
    return {
        "sha256": row.sha256,
        "size": row.size,
        "modified_ns": row.modified_ns,
    }


def _manifest_differences(side: str, before, after, project_name: str) -> list[dict[str, object]]:
    before_rows = {row.path: row for row in before.rows}
    after_rows = {} if after is None else {row.path: row for row in after.rows}
    differences: list[dict[str, object]] = []
    for relative in sorted(set(before_rows) | set(after_rows), key=lambda value: value.encode("utf-8")):
        prior = before_rows.get(relative)
        current = after_rows.get(relative)
        if prior == current:
            continue
        membership = "changed"
        if prior is None:
            membership = "extra"
        elif current is None:
            membership = "missing"
        differences.append(
            {
                "side": side,
                "label": _logical_label(relative, project_name),
                "membership": membership,
                "before": None if prior is None else _row_state(prior),
                "after": None if current is None else _row_state(current),
            }
        )
    return differences


def _hashed_identity_delta(
    before: set[int], after: set[int], ownership_token: str
) -> dict[str, list[str]]:
    def hashed(value: int) -> str:
        return hashlib.sha256(
            f"cst-process-v1\0{ownership_token}\0{value}".encode("utf-8")
        ).hexdigest()

    return {
        "added": sorted(hashed(value) for value in after - before),
        "missing": sorted(hashed(value) for value in before - after),
    }


def _preservation_evidence(manifest, source_after, resource, project_name: str) -> dict[str, object]:
    rows = _manifest_differences("source", manifest, source_after, project_name)
    rows.extend(_manifest_differences("copy", manifest, resource.final_manifest, project_name))
    foreign_after = resource.foreign_after if resource.foreign_after is not None else set()
    return {
        "rows": rows,
        "foreign_identity_delta": _hashed_identity_delta(
            resource.foreign_before,
            foreign_after,
            resource.ownership_token,
        ),
    }


def _execute(request: dict[str, object]) -> dict[str, object]:
    source, workspace = _validated_paths(request)
    manifest = _load_manifest(request["source_manifest"])
    _validate_identities(request, source, manifest)

    from mcphub_em_mcp.cst_saved_field_application import SamplerSession
    from mcphub_em_mcp.cst_saved_field_transfer import (
        AuthorizedBundleTransfer,
        inventory_manifest_v2,
    )
    from mcphub_em_mcp.cst_saved_field_vendor import open_owned_sampler_session
    import cst.interface as interface

    observer = _APICallObserver()
    if inventory_manifest_v2(source) != manifest:
        raise ProbeFailure("probe_preservation_failed")
    destination = workspace / "bundle"
    snapshot = AuthorizedBundleTransfer(manifest).execute(source, destination)
    copied_project = destination / source.name
    session = SamplerSession(snapshot)
    resource = None
    selectors: list[str] = []
    settlement = None
    pending_failure: ProbeFailure | None = None
    try:
        owned, acquisition = open_owned_sampler_session(
            lambda copied: _safe_create_owned(
                interface, Path(copied), manifest, observer
            ),
            copied_project,
        )
        if not acquisition.transfer_committed or acquisition.handles_received != 1:
            raise ProbeFailure("owned_process_identity_ambiguous")
        session.adopt_owned(owned)
        resource = owned._resource
        stall_ms = request.get("test_stall_after_open_ms")
        if type(stall_ms) is int:
            time.sleep(stall_ms / 1000.0)
        selectors = _selectors(owned.handle, observer)
        if request.get("test_preservation_mutation") == "forbidden-call":
            observer.call("project-save", lambda: None)
        _apply_test_preservation_mutation(request, source, copied_project)
    except ProbeFailure as exc:
        pending_failure = exc
    finally:
        settlement = session.settle(source_changed_role=None)

    source_after = inventory_manifest_v2(source)
    if resource is None:
        raise ProbeFailure("probe_preservation_failed")
    preservation = _preservation_evidence(manifest, source_after, resource, source.name)
    foreign_delta = preservation["foreign_identity_delta"]
    source_changed = any(row["side"] == "source" for row in preservation["rows"])
    foreign_changed = bool(foreign_delta["added"] or foreign_delta["missing"])
    session_identity = hashlib.sha256(
        f"cst-session-v1\0{resource.ownership_token}".encode("utf-8")
    ).hexdigest()
    common = {
        "source_manifest_sha256": manifest.aggregate_sha256,
        "destination_manifest_sha256": resource.final_manifest.aggregate_sha256,
        "session_identity_sha256": session_identity,
        "workspace_settled": settlement.workspace_settled,
        "session_settled": settlement.session_settled,
        "source_unchanged": not source_changed,
        "cache_cleared": settlement.cache_cleared,
        "closed_without_save": settlement.closed_without_save,
        "foreign_processes_preserved": not foreign_changed,
        "preservation": preservation,
        "api_trace": observer.snapshot(),
    }
    if pending_failure is not None:
        pending_failure.api_trace = observer.snapshot()
        pending_failure.preservation = preservation
        pending_failure.receipt_fields.update(common)
        raise pending_failure
    if source_changed or foreign_changed:
        raise ProbeFailure(
            "probe_preservation_failed",
            preservation=preservation,
            api_trace=observer.snapshot(),
            receipt_fields=common,
        )
    has_field_selector = any(
        "\\e-field" in selector.lower() or "\\h-field" in selector.lower()
        for selector in selectors
    )
    status = "PASS:owned_result_tree_listed" if has_field_selector else "BLOCKED:field_reference_absent"
    failure_id = "" if has_field_selector else "field_reference_absent"
    return {
        "schema": RECEIPT_SCHEMA,
        "status": status,
        "failure_id": failure_id,
        **common,
        "root": "2D/3D Results",
        "selectors": selectors,
        "traversal_count": len(selectors),
        "max_depth": MAX_TREE_DEPTH,
        "max_nodes": MAX_TREE_NODES,
        "max_selector_bytes": MAX_SELECTOR_BYTES,
    }


def main() -> int:
    try:
        request = _read_request()
        receipt = _execute(request)
    except ProbeFailure as exc:
        receipt: dict[str, object] = {
            "schema": RECEIPT_SCHEMA,
            "status": f"BLOCKED:{exc.failure_id}",
            "failure_id": exc.failure_id,
        }
        receipt.update(exc.receipt_fields)
        if exc.preservation is not None:
            receipt["preservation"] = exc.preservation
        if exc.api_trace is not None:
            receipt["api_trace"] = exc.api_trace
        _emit(receipt)
        return 0
    except SystemExit as exc:
        return int(exc.code or 0)
    except BaseException as exc:
        print(type(exc).__name__, file=sys.stderr, flush=True)
        _emit(
            {
                "schema": RECEIPT_SCHEMA,
                "status": "BLOCKED:probe_execution_failed",
                "failure_id": "probe_execution_failed",
            }
        )
        return 0
    _emit(receipt)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
