"""Test-only, two-point CST saved-field ASCII export calibration."""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
import math
import os
import re
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET
from pathlib import Path, PureWindowsPath


REQUEST_VERSION = "ASCIIPointCalibrationRequestV1"
RECEIPT_VERSION = "ASCIIPointCalibrationReceiptV1"
SECTION_ID = "2026-09-23 — one hardlink-guarded native D1 selector receiving run"
SECTION_SHA256 = "263ed9e3af251450097c6913c65c8154bdf5097a7315ff042bd00e2a25d58947"
HELP_SHA256 = "3c8447e6dc54b2f434f6e12353290f95148c75a5792d5840ea624b8a6e0aa850"
MESH_SHA256 = "27e10d39875077ae858f099d986a0b51945696edd0540a4dd13aac2dc1d6866a"
CASE_SHA256 = "debdb9ca1294c0c0c2bd060edea5ac93b634d2dc4e9cc70ac3cc388ca7b3f858"
POINTS = (
    ("p1:t19:f3:q0", ("0", "7.5184627019783594", "6.1002692254485309")),
    ("p2:t11613:f2:q0", ("10", "6.3645824908535733", "0.20448276178909414")),
)
REQUEST_KEYS = frozenset({
    "version", "copyRoot", "projectSha256", "fullAggregateSha256",
    "resultAggregateSha256", "selectorReceivingSectionId", "selectorReceivingSectionSha256",
    "meshSha256", "caseManifestSha256", "cstExecutableSha256", "helperSha256",
    "frames", "points", "pointFileBase64",
})
FRAME_KEYS = frozenset({
    "field", "selector", "m3dPath", "m3dSha256", "rexPath", "rexSha256", "rawFrequency",
})
MAX_REQUEST = 128 << 10
MAX_RECEIPT = 1 << 20
MAX_OUTPUT = 64 << 10


class CalibrationFailure(Exception):
    def __init__(self, status: str, detail: str = "") -> None:
        super().__init__(status)
        self.status = status
        self.detail = detail


def _hash_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _hash_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def _hex(value: object) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None


def _no_link(path: Path, stop: Path) -> bool:
    if path != stop and stop not in path.parents:
        return False
    current = path
    while True:
        if current.is_symlink() or (hasattr(current, "is_junction") and current.is_junction()):
            return False
        if current == stop:
            return True
        current = current.parent


def _preflight(request: dict[str, object], raw: bytes) -> tuple[Path, Path, bytes, list[dict[str, object]]]:
    bad = lambda: CalibrationFailure("BLOCKED:selector_evidence_missing_or_stale")
    if len(raw) > MAX_REQUEST or set(request) != REQUEST_KEYS or request.get("version") != REQUEST_VERSION:
        raise bad()
    if (request["selectorReceivingSectionId"] != SECTION_ID
            or request["selectorReceivingSectionSha256"] != SECTION_SHA256
            or request["meshSha256"] != MESH_SHA256
            or request["caseManifestSha256"] != CASE_SHA256
            or any(not _hex(request[key]) for key in (
                "projectSha256", "fullAggregateSha256", "resultAggregateSha256",
                "cstExecutableSha256", "helperSha256"))):
        raise bad()
    try:
        root = Path(request["copyRoot"])
        if (not root.is_absolute() or root != Path(os.path.abspath(root))
                or not root.is_dir() or not _no_link(root, root)):
            raise bad()
        frames = request["frames"]
        if not isinstance(frames, list) or len(frames) != 2:
            raise bad()
        first_path = frames[0]["m3dPath"]
        if not isinstance(first_path, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/Result/[^/]+\.m3d", first_path):
            raise bad()
        stem = first_path.split("/", 1)[0]
        if stem in {".", ".."}:
            raise bad()
        project = root / (stem + ".cst")
        if not project.is_file() or not _no_link(project, root):
            raise bad()
        if _hash_file(Path(__file__)) != request["helperSha256"] or _hash_file(project) != request["projectSha256"]:
            raise bad()
        for field, frame in zip(("E", "H"), frames):
            if not isinstance(frame, dict) or set(frame) != FRAME_KEYS or frame["field"] != field:
                raise bad()
            match = re.fullmatch(rf"{re.escape(stem)}/Result/mcp_line10_{field}_3GHz_([1-9][0-9]*),1\.m3d",
                                 frame["m3dPath"]) if isinstance(frame["m3dPath"], str) else None
            if match is None or frame["rexPath"] != frame["m3dPath"][:-4] + "_m3d.rex":
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field)
            if (not _hex(frame["m3dSha256"]) or not _hex(frame["rexSha256"])
                    or type(frame["rawFrequency"]) not in (int, float)
                    or not math.isfinite(frame["rawFrequency"]) or frame["rawFrequency"] != 3):
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field)
            rex = root / Path(frame["rexPath"])
            if not rex.is_file() or not _no_link(rex, root) or rex.stat().st_size > MAX_REQUEST:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field)
            rex_bytes = rex.read_bytes()
            if _hash_bytes(rex_bytes) != frame["rexSha256"]:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field)
            try:
                metadata = ET.fromstring(rex_bytes)
            except ET.ParseError as exc:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field) from exc
            properties = metadata.findall("SimulationProperties")
            if metadata.tag != "MetaResultFile" or len(properties) != 1:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field)
            leaf = properties[0].get("fieldname")
            if not isinstance(leaf, str) or re.fullmatch(rf"mcp_line10_{field}_3GHz \[[0-9]+\]", leaf) is None:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field)
            if frame["selector"] != f"2D/3D Results\\{field}-Field\\{leaf}":
                raise CalibrationFailure("BLOCKED:selector_binding_stale", field)
        _pair_hashes(root, frames)
        points = request["points"]
        if (not isinstance(points, list) or len(points) != 2
                or any(not isinstance(point, dict) or set(point) != {"id", "xyzMm"}
                       or point["id"] != name or point["xyzMm"] != list(xyz)
                       for point, (name, xyz) in zip(points, POINTS))):
            raise bad()
        candidate = b"".join((" ".join(xyz) + "\r\n").encode("utf-8") for _, xyz in POINTS)
        encoded = request["pointFileBase64"]
        if not isinstance(encoded, str) or base64.b64decode(encoded, validate=True) != candidate:
            raise bad()
        return root, project, candidate, frames
    except (OSError, TypeError, KeyError, ValueError, binascii.Error) as exc:
        raise bad() from exc


def _pair_hashes(root: Path, frames: list[dict[str, object]]) -> list[dict[str, object]]:
    rows = []
    for frame in frames:
        row = {"field": frame["field"]}
        for kind in ("m3d", "rex"):
            relative = frame[kind + "Path"]
            path = root / Path(relative)
            if not path.is_file() or not _no_link(path, root):
                raise CalibrationFailure("BLOCKED:selector_binding_stale", str(frame["field"]))
            actual = _hash_file(path)
            if actual != frame[kind + "Sha256"]:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", str(frame["field"]))
            row[kind + "Sha256"] = actual
        rows.append(row)
    return rows


def _returned_path(returned: object, expected: Path, root: Path) -> bool:
    if not isinstance(returned, str) or not returned or "\x00" in returned:
        return False
    path = PureWindowsPath(returned)
    if path.is_absolute():
        return Path(returned) == expected
    parts = path.parts
    if parts[:2] == (expected.parent.parent.name, "Result"):
        parts = parts[2:]
    elif parts[:1] == ("Result",):
        parts = parts[1:]
    return parts == (expected.name,) and _no_link(expected, root)


def _receipt(request: dict[str, object], raw: bytes) -> dict[str, object]:
    return {
        "version": RECEIPT_VERSION,
        "requestSha256": _hash_bytes(raw),
        "status": "BLOCKED:selector_evidence_missing_or_stale",
        "selectorReceivingSectionId": SECTION_ID if request.get("selectorReceivingSectionId") == SECTION_ID else "",
        "selectorReceivingSectionSha256": SECTION_SHA256 if request.get("selectorReceivingSectionSha256") == SECTION_SHA256 else "",
        "apiTrace": [], "boundFrames": [], "prePostHashes": {},
        "cstExecutableSha256": request.get("cstExecutableSha256") if _hex(request.get("cstExecutableSha256")) else "",
        "helpSha256": HELP_SHA256,
        "pointFileBase64": "", "pointFileSha256": "", "outputs": [],
        "error": "", "settlement": {"closedWithoutSave": False, "sessionSettled": False},
    }


def _census(value: object) -> set[int]:
    if not isinstance(value, (list, tuple, set)) or any(type(item) is not int or item <= 0 for item in value):
        raise CalibrationFailure("BLOCKED:probe_preservation_failed", "environment-census")
    return set(value)


def _execute(request: dict[str, object], raw: bytes, interface: object) -> dict[str, object]:
    receipt = _receipt(request, raw)
    trace: list[dict[str, str]] = receipt["apiTrace"]
    environment = project_handle = None
    pre_census = None

    def call(name: str, operation, *args):
        try:
            result = operation(*args)
        except BaseException:
            trace.append({"name": name, "outcome": "error"})
            raise
        trace.append({"name": name, "outcome": "finish"})
        return result

    try:
        root, project, candidate, frames = _preflight(request, raw)
        pre = {"projectSha256": _hash_file(project), "frames": _pair_hashes(root, frames)}
        receipt["prePostHashes"] = {"pre": pre}
        if interface is None:
            raise CalibrationFailure("BLOCKED:probe_preservation_failed", "cst-interface-unavailable")
        point_path = root / "ascii-calibration-points.txt"
        with point_path.open("xb") as stream:
            stream.write(candidate)
        if not _no_link(point_path, root) or point_path.read_bytes() != candidate:
            raise CalibrationFailure("BLOCKED:probe_preservation_failed", "point-file-write")
        receipt["pointFileBase64"] = base64.b64encode(candidate).decode("ascii")
        receipt["pointFileSha256"] = _hash_bytes(candidate)
        pre_census = _census(call("running_design_environments", interface.running_design_environments))
        environment = call("environment-new", interface.DesignEnvironment.new)
        project_handle = call("project-open", environment.open_project, str(project))
        after_open = _census(call("running_design_environments", interface.running_design_environments))
        if after_open < pre_census or len(after_open - pre_census) != 1:
            raise CalibrationFailure("BLOCKED:probe_preservation_failed", "owned-session-identity")
        model = project_handle.model3d
        tree = model.ResultTree
        exporter = model.ASCIIExport
        # Both copy associations and hashes must be current before the first export.
        for frame in frames:
            returned = call("GetFileFromTreeItem", tree.GetFileFromTreeItem, frame["selector"])
            expected = root / Path(frame["m3dPath"])
            if not _returned_path(returned, expected, root):
                raise CalibrationFailure("BLOCKED:selector_binding_stale", str(frame["field"]))
        _pair_hashes(root, frames)
        for frame in frames:
            selected = call("SelectTreeItem", model.SelectTreeItem, frame["selector"])
            if selected is not True:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", str(frame["field"]))
            selected_item = call("GetSelectedTreeItem", model.GetSelectedTreeItem)
            if selected_item != frame["selector"]:
                raise CalibrationFailure("BLOCKED:selector_binding_stale", str(frame["field"]))
            returned = call("GetFileFromTreeItem", tree.GetFileFromTreeItem, selected_item)
            if not _returned_path(returned, root / Path(frame["m3dPath"]), root):
                raise CalibrationFailure("BLOCKED:selector_binding_stale", str(frame["field"]))
            frequency = call("GetFieldFrequency", model.GetFieldFrequency)
            if (type(frequency) not in (int, float) or not math.isfinite(frequency)
                    or frequency != frame["rawFrequency"]):
                raise CalibrationFailure("BLOCKED:selector_binding_stale", str(frame["field"]))
            receipt["boundFrames"].append({
                "field": frame["field"], "selector": frame["selector"],
                "selectedTreeItem": selected_item,
                "returnedM3dPath": frame["m3dPath"], "selectionResult": True,
                "rawFrequency": frequency, "m3dSha256": frame["m3dSha256"],
                "rexSha256": frame["rexSha256"],
            })
            for repeat in (1, 2):
                output = root / f"ascii-calibration-{frame['field']}-{repeat}.txt"
                if output.exists() or output.is_symlink():
                    raise CalibrationFailure("BLOCKED:probe_preservation_failed", str(frame["field"]))
                call("Reset", exporter.Reset)
                call("FileName", exporter.FileName, str(output))
                try:
                    call("SetPointFile", exporter.SetPointFile, str(point_path))
                except Exception as exc:
                    raise CalibrationFailure("BLOCKED:point_file_grammar_unverified", str(frame["field"])) from exc
                call("Execute", exporter.Execute)
                if not output.is_file() or not _no_link(output, root):
                    raise CalibrationFailure("BLOCKED:point_file_grammar_unverified", str(frame["field"]))
                with output.open("rb") as stream:
                    data = stream.read(MAX_OUTPUT + 1)
                if not data or len(data) > MAX_OUTPUT:
                    raise CalibrationFailure("BLOCKED:point_file_grammar_unverified", str(frame["field"]))
                receipt["outputs"].append({
                    "field": frame["field"], "repeat": repeat,
                    "rawBase64": base64.b64encode(data).decode("ascii"), "sha256": _hash_bytes(data),
                })
        receipt["status"] = "PASS:ascii_point_calibration_raw"
    except CalibrationFailure as exc:
        receipt["status"] = exc.status
        receipt["error"] = exc.detail
    except BaseException as exc:
        receipt["status"] = "BLOCKED:probe_preservation_failed"
        receipt["error"] = type(exc).__name__
    finally:
        closed = True
        if project_handle is not None:
            try:
                call("project-close", project_handle.close)
            except BaseException:
                closed = False
        if environment is not None:
            try:
                call("environment-close", environment.close)
            except BaseException:
                closed = False
        foreign_preserved = process_settled = pre_census is None
        if pre_census is not None:
            try:
                post_census = _census(call("running_design_environments", interface.running_design_environments))
                foreign_preserved = pre_census <= post_census
                process_settled = post_census == pre_census
            except BaseException:
                foreign_preserved = process_settled = False
        settled = closed and process_settled
        receipt["settlement"] = {
            "closedWithoutSave": closed and project_handle is not None and environment is not None,
            "sessionSettled": settled,
            "ownedProcessSettled": process_settled,
            "foreignPreserved": foreign_preserved,
        }
        if not settled or not foreign_preserved:
            receipt["status"] = "BLOCKED:probe_preservation_failed"
        if ("root" in locals() and "frames" in locals() and "project" in locals()
                and "pre" in receipt["prePostHashes"]):
            try:
                receipt["prePostHashes"]["post"] = {
                    "projectSha256": _hash_file(project), "frames": _pair_hashes(root, frames),
                }
                if receipt["prePostHashes"]["post"]["projectSha256"] != request["projectSha256"]:
                    receipt["status"] = "BLOCKED:probe_preservation_failed"
            except BaseException:
                receipt["status"] = "BLOCKED:probe_preservation_failed"
    return receipt


def main() -> int:
    raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
    try:
        request = json.loads(raw)
        if not isinstance(request, dict):
            raise ValueError("request object required")
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
        request = {}
    interface = None
    try:
        _preflight(request, raw)
    except CalibrationFailure:
        pass
    else:
        try:
            import cst.interface as interface
        except Exception:
            pass
    receipt = _execute(request, raw, interface)
    encoded = json.dumps(receipt, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8") + b"\n"
    if len(encoded) > MAX_RECEIPT:
        receipt["status"] = "BLOCKED:probe_preservation_failed"
        receipt["outputs"] = []
        receipt["error"] = "receipt_size"
        encoded = json.dumps(receipt, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8") + b"\n"
    sys.stdout.buffer.write(encoded)
    sys.stdout.buffer.flush()
    return 0


class _FakeCST:
    """Only the external CST boundary is faked; files and bytes remain real."""

    def __init__(self, root: Path, *, selected: dict[str, object] | None = None,
                 frequency: dict[str, object] | None = None, missing_output: bool = False,
                 lingering: bool = False, interrupted: bool = False,
                 returned: dict[str, str] | None = None, fail_method: str | None = None,
                 readback: dict[str, str] | None = None,
                 returned_after_selection: dict[str, str] | None = None) -> None:
        self.root = root
        self.calls: list[str] = []
        self.selected = selected or {}
        self.frequency = frequency or {}
        self.missing_output = missing_output
        self.lingering = lingering
        self.interrupted = interrupted
        self.returned = returned or {}
        self.readback = readback or {}
        self.returned_after_selection = returned_after_selection or {}
        self.fail_method = fail_method
        self.opened = False
        self.closed = False
        self.current_selector = ""
        self.output = ""
        self.DesignEnvironment = self

    def new(self):
        self.calls.append("new")
        return self

    def open_project(self, path: str):
        self.calls.append("open_project")
        assert Path(path).parent == self.root
        self.opened = True
        return self

    def running_design_environments(self):
        self.calls.append("running_design_environments")
        return [17] if self.opened and (not self.closed or self.lingering) else []

    @property
    def model3d(self):
        return self

    @property
    def ResultTree(self):
        return self

    @property
    def ASCIIExport(self):
        return self

    def GetFileFromTreeItem(self, selector: str):
        self.calls.append("GetFileFromTreeItem")
        if self.current_selector == selector and selector in self.returned_after_selection:
            return self.returned_after_selection[selector]
        if selector in self.returned:
            return self.returned[selector]
        field = selector.split("\\")[-2][0]
        return str(self.root / "probe" / "Result" / f"mcp_line10_{field}_3GHz_1,1.m3d")

    def SelectTreeItem(self, selector: str):
        self.calls.append("SelectTreeItem")
        self.current_selector = selector
        return self.selected.get(selector, True)

    def GetSelectedTreeItem(self):
        self.calls.append("GetSelectedTreeItem")
        return self.readback.get(self.current_selector, self.current_selector)

    def GetFieldFrequency(self):
        self.calls.append("GetFieldFrequency")
        return self.frequency.get(self.current_selector, 3)

    def Reset(self):
        self.calls.append("Reset")
        if self.fail_method == "Reset":
            raise RuntimeError("synthetic Reset failure")

    def FileName(self, filename: str):
        self.calls.append("FileName")
        if self.fail_method == "FileName":
            raise RuntimeError("synthetic FileName failure")
        self.output = filename

    def SetPointFile(self, filename: str):
        self.calls.append("SetPointFile")
        if self.fail_method == "SetPointFile":
            raise RuntimeError("synthetic SetPointFile rejection")
        assert Path(filename).read_bytes() == (
            b"0 7.5184627019783594 6.1002692254485309\r\n"
            b"10 6.3645824908535733 0.20448276178909414\r\n"
        )

    def Execute(self):
        self.calls.append("Execute")
        if self.fail_method == "Execute":
            raise RuntimeError("synthetic Execute failure")
        if self.interrupted:
            raise KeyboardInterrupt
        if not self.missing_output:
            Path(self.output).write_bytes(b"# fake\r\n0 1 2 3 4 5 6 7 8\r\n")

    def close(self):
        self.calls.append("close")
        self.closed = True


class _CalibrationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        data = self.root / "probe" / "Result"
        data.mkdir(parents=True)
        (self.root / "probe.cst").write_bytes(b"copied project")
        self.frames = []
        for field in ("E", "H"):
            stem = f"mcp_line10_{field}_3GHz_1,1"
            m3d = data / (stem + ".m3d")
            rex = data / (stem + "_m3d.rex")
            m3d.write_bytes((field + " payload").encode())
            rex.write_bytes((f'<MetaResultFile><SimulationProperties fieldname="mcp_line10_{field}_3GHz [1]"/></MetaResultFile>').encode())
            self.frames.append({
                "field": field,
                "selector": f"2D/3D Results\\{field}-Field\\mcp_line10_{field}_3GHz [1]",
                "m3dPath": f"probe/Result/{m3d.name}",
                "m3dSha256": hashlib.sha256(m3d.read_bytes()).hexdigest(),
                "rexPath": f"probe/Result/{rex.name}",
                "rexSha256": hashlib.sha256(rex.read_bytes()).hexdigest(),
                "rawFrequency": 3,
            })
        self.point_bytes = (
            b"0 7.5184627019783594 6.1002692254485309\r\n"
            b"10 6.3645824908535733 0.20448276178909414\r\n"
        )
        self.request = {
            "version": "ASCIIPointCalibrationRequestV1",
            "copyRoot": str(self.root),
            "projectSha256": hashlib.sha256((self.root / "probe.cst").read_bytes()).hexdigest(),
            "fullAggregateSha256": "a" * 64,
            "resultAggregateSha256": "b" * 64,
            "selectorReceivingSectionId": "2026-09-23 — one hardlink-guarded native D1 selector receiving run",
            "selectorReceivingSectionSha256": "263ed9e3af251450097c6913c65c8154bdf5097a7315ff042bd00e2a25d58947",
            "meshSha256": "27e10d39875077ae858f099d986a0b51945696edd0540a4dd13aac2dc1d6866a",
            "caseManifestSha256": "debdb9ca1294c0c0c2bd060edea5ac93b634d2dc4e9cc70ac3cc388ca7b3f858",
            "cstExecutableSha256": "c" * 64,
            "helperSha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            "frames": self.frames,
            "points": [
                {"id": "p1:t19:f3:q0", "xyzMm": ["0", "7.5184627019783594", "6.1002692254485309"]},
                {"id": "p2:t11613:f2:q0", "xyzMm": ["10", "6.3645824908535733", "0.20448276178909414"]},
            ],
            "pointFileBase64": base64.b64encode(self.point_bytes).decode("ascii"),
        }

    def run_case(self, fake: _FakeCST | None = None):
        fake = fake or _FakeCST(self.root)
        raw = json.dumps(self.request, separators=(",", ":")).encode("utf-8")
        return _execute(self.request, raw, fake), fake.calls

    def test_exact_two_frame_export_sequence_and_raw_receipt(self):
        receipt, calls = self.run_case()
        self.assertEqual(receipt["status"], "PASS:ascii_point_calibration_raw")
        self.assertEqual(calls, ["running_design_environments", "new", "open_project", "running_design_environments",
                                 "GetFileFromTreeItem", "GetFileFromTreeItem",
                                  "SelectTreeItem", "GetSelectedTreeItem", "GetFileFromTreeItem", "GetFieldFrequency", *(["Reset", "FileName", "SetPointFile", "Execute"] * 2),
                                  "SelectTreeItem", "GetSelectedTreeItem", "GetFileFromTreeItem", "GetFieldFrequency", *(["Reset", "FileName", "SetPointFile", "Execute"] * 2),
                                 "close", "close", "running_design_environments"])
        self.assertEqual([(row["field"], row["repeat"]) for row in receipt["outputs"]],
                         [("E", 1), ("E", 2), ("H", 1), ("H", 2)])
        self.assertEqual(base64.b64decode(receipt["outputs"][0]["rawBase64"]),
                         b"# fake\r\n0 1 2 3 4 5 6 7 8\r\n")
        self.assertEqual(base64.b64decode(receipt["pointFileBase64"]), self.point_bytes)
        self.assertNotIn(str(self.root), json.dumps(receipt))
        self.assertTrue(receipt["settlement"]["sessionSettled"])

    def test_h_port_two_with_distinct_runid_exports_bound_payload(self):
        frame = self.frames[1]
        base = "mcp_line10_H_3GHz_2,1"
        m3d = self.root / "probe" / "Result" / (base + ".m3d")
        rex = self.root / "probe" / "Result" / (base + "_m3d.rex")
        m3d.write_bytes(b"H port two payload")
        rex.write_bytes(b'<MetaResultFile><SimulationProperties fieldname="mcp_line10_H_3GHz [7]"/></MetaResultFile>')
        frame.update({
            "selector": "2D/3D Results\\H-Field\\mcp_line10_H_3GHz [7]",
            "m3dPath": "probe/Result/" + m3d.name,
            "m3dSha256": hashlib.sha256(m3d.read_bytes()).hexdigest(),
            "rexPath": "probe/Result/" + rex.name,
            "rexSha256": hashlib.sha256(rex.read_bytes()).hexdigest(),
        })
        fake = _FakeCST(self.root, returned={frame["selector"]: frame["m3dPath"]})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "PASS:ascii_point_calibration_raw")
        self.assertEqual(receipt["boundFrames"][1]["selector"], frame["selector"])
        self.assertEqual(receipt["boundFrames"][1]["rexSha256"], frame["rexSha256"])
        self.assertEqual(calls.count("Execute"), 4)

    def test_selected_tree_readback_mismatch_blocks_before_export(self):
        fake = _FakeCST(self.root, readback={self.frames[0]["selector"]: self.frames[1]["selector"]})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertNotIn("Execute", calls)

    def test_h_selected_tree_readback_mismatch_has_no_h_export(self):
        fake = _FakeCST(self.root, readback={self.frames[1]["selector"]: self.frames[0]["selector"]})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertEqual(calls.count("Execute"), 2)

    def test_selected_tree_file_association_drift_blocks_before_export(self):
        fake = _FakeCST(self.root, returned_after_selection={self.frames[0]["selector"]: self.frames[1]["m3dPath"]})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertNotIn("Execute", calls)

    def test_wrong_selector_section_blocks_before_session(self):
        self.request["selectorReceivingSectionSha256"] = "d" * 64
        receipt, calls = self.run_case()
        self.assertEqual(receipt["status"], "BLOCKED:selector_evidence_missing_or_stale")
        self.assertEqual(calls, [])

    def test_invalid_receipt_echo_never_reveals_request_path(self):
        self.request["selectorReceivingSectionId"] = str(self.root / "private")
        receipt, calls = self.run_case()
        self.assertEqual(receipt["selectorReceivingSectionId"], "")
        self.assertEqual(calls, [])

    def test_wrong_selector_blocks_before_export(self):
        self.request["frames"][0]["selector"] += "-wrong"
        receipt, calls = self.run_case()
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertNotIn("Execute", calls)

    def test_wrong_payload_hash_blocks_before_export(self):
        self.request["frames"][1]["m3dSha256"] = "d" * 64
        receipt, calls = self.run_case()
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertNotIn("Execute", calls)

    def test_ambiguous_rex_fieldname_blocks_before_session(self):
        rex = self.root / self.frames[1]["rexPath"]
        rex.write_bytes(b'<MetaResultFile><SimulationProperties fieldname="mcp_line10_H_3GHz [1]"/><SimulationProperties fieldname="mcp_line10_H_3GHz [2]"/></MetaResultFile>')
        self.frames[1]["rexSha256"] = hashlib.sha256(rex.read_bytes()).hexdigest()
        receipt, calls = self.run_case()
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertEqual(calls, [])

    def test_rex_metadata_hash_mismatch_blocks_before_session(self):
        self.frames[1]["rexSha256"] = "d" * 64
        receipt, calls = self.run_case()
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertEqual(calls, [])

    def test_copy_association_mismatch_blocks_before_export(self):
        fake = _FakeCST(self.root, returned={self.frames[1]["selector"]: self.frames[0]["m3dPath"]})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertEqual(calls.count("GetFileFromTreeItem"), 2)
        self.assertNotIn("Execute", calls)

    def test_false_e_selection_blocks_even_if_frequency_is_three(self):
        fake = _FakeCST(self.root, selected={self.frames[0]["selector"]: False})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertNotIn("GetFieldFrequency", calls)
        self.assertNotIn("Execute", calls)

    def test_false_h_selection_has_no_h_export(self):
        fake = _FakeCST(self.root, selected={self.frames[1]["selector"]: False})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertEqual(calls.count("Execute"), 2)

    def test_stale_frequency_blocks_frame_export(self):
        fake = _FakeCST(self.root, frequency={self.frames[0]["selector"]: 2.5})
        receipt, calls = self.run_case(fake)
        self.assertEqual(receipt["status"], "BLOCKED:selector_binding_stale")
        self.assertNotIn("Execute", calls)

    def test_missing_completed_output_blocks_grammar(self):
        receipt, calls = self.run_case(_FakeCST(self.root, missing_output=True))
        self.assertEqual(receipt["status"], "BLOCKED:point_file_grammar_unverified")
        self.assertEqual(calls.count("Execute"), 1)
        self.assertEqual(calls[-3:], ["close", "close", "running_design_environments"])

    def _assert_export_api_failure(self, method: str, status: str):
        receipt, calls = self.run_case(_FakeCST(self.root, fail_method=method))
        self.assertEqual(receipt["status"], status)
        self.assertEqual(receipt["outputs"], [])
        self.assertEqual(calls.count(method), 1)
        self.assertEqual(calls[-3:], ["close", "close", "running_design_environments"])
        self.assertIn({"name": method, "outcome": "error"}, receipt["apiTrace"])
        self.assertTrue(receipt["settlement"]["closedWithoutSave"])
        self.assertTrue(receipt["settlement"]["sessionSettled"])

    def test_reset_exception_is_preservation_failure(self):
        self._assert_export_api_failure("Reset", "BLOCKED:probe_preservation_failed")

    def test_filename_exception_is_preservation_failure(self):
        self._assert_export_api_failure("FileName", "BLOCKED:probe_preservation_failed")

    def test_setpointfile_rejection_is_grammar_failure(self):
        self._assert_export_api_failure("SetPointFile", "BLOCKED:point_file_grammar_unverified")

    def test_execute_exception_is_preservation_failure(self):
        self._assert_export_api_failure("Execute", "BLOCKED:probe_preservation_failed")

    def test_wrong_point_bytes_blocks_before_session(self):
        self.request["pointFileBase64"] = base64.b64encode(b"0 0 0\n").decode()
        receipt, calls = self.run_case()
        self.assertEqual(receipt["status"], "BLOCKED:selector_evidence_missing_or_stale")
        self.assertEqual(calls, [])

    def test_lingering_owned_session_blocks_success(self):
        receipt, calls = self.run_case(_FakeCST(self.root, lingering=True))
        self.assertEqual(receipt["status"], "BLOCKED:probe_preservation_failed")
        self.assertFalse(receipt["settlement"]["sessionSettled"])
        self.assertEqual(calls[-1], "running_design_environments")

    def test_interrupted_export_closes_without_save(self):
        receipt, calls = self.run_case(_FakeCST(self.root, interrupted=True))
        self.assertEqual(receipt["status"], "BLOCKED:probe_preservation_failed")
        self.assertEqual(calls.count("Execute"), 1)
        self.assertEqual(calls[-3:-1], ["close", "close"])
        self.assertTrue(receipt["settlement"]["sessionSettled"])


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        unittest.main(argv=[sys.argv[0]])
    else:
        raise SystemExit(main())
