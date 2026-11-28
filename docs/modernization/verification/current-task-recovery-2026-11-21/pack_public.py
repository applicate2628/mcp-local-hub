"""Verify and redact the six R21 qualification phases; no subprocess or network.

Copy this reviewed script to the approved Windows task root and supply the
private packaging-input.json. Inputs are retained, never rewritten or removed.
Native harness artifacts need an explicit supplemental allowlist. Raw native
XML is hash-verified only and is never included in the public archive.
"""
import sys
sys.dont_write_bytecode = True

import base64
from collections import Counter
import datetime as dt
import hashlib
import io
import json
import math
import os
from pathlib import Path, PurePosixPath
import re
import stat
import zipfile

ROOT = Path(r"<RECOVERY_ROOT>")
SOURCE = ROOT / "source"
OLD = Path(r"<INSTALL_SNAPSHOT_ROOT>\source")
BASE = "c55d9ca619f6f9301ba1113615370fc6218d608f"
BASE_TREE = "06a85c6efe53f5a4a8babbcadb7818752bb7acee"
GO = r"C:\msys64\ucrt64\bin\go.exe"
GOFMT = r"C:\msys64\ucrt64\bin\gofmt.exe"
GIT = r"C:\Program Files\Git\cmd\git.exe"
REMOTE = "https://github.com/applicate2628/mcp-local-hub.git"
PHASES = ("control", "candidate", "adjacent", "build-vet", "published-checkout", "published")
STATIC = ("source-manifest.json", "source-plan.json", "check_recovery.py", "prepare_source.py", "checkout_published.py", "control.patch", "product.patch", "candidate.patch", "publication-input.json", "pack_public.py")
ENV = {"GOENV": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOVCS": "*:off", "GOWORK": "off", "GOFLAGS": "-mod=readonly", "GOTELEMETRY": "off", "GOMAXPROCS": "4", "CGO_ENABLED": "1", "MCPHUB_NO_AUTO_GUI": "1", "MCPHUB_E2E_SCHEDULER": "none"}
MAX_FILE = 16 * 1024 * 1024
inputs, inventories = {}, {}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def encode(value):
    return (json.dumps(value, indent=2, ensure_ascii=False, sort_keys=True) + "\n").encode("utf-8")


def unique(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "Duplicate JSON key")
        result[key] = value
    return result


def parse(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=unique, parse_constant=lambda value: (_ for _ in ()).throw(ValueError("Nonfinite JSON value")))


def relative(name):
    require(isinstance(name, str) and re.fullmatch(r"[A-Za-z0-9_./-]+", name), "Invalid relative path")
    path = PurePosixPath(name)
    require(not path.is_absolute() and str(path) == name and not any(p in {".", "..", ".git"} for p in path.parts), "Path escapes or aliases the reviewed root")
    return path


def checked_path(name):
    path = ROOT.joinpath(*relative(name).parts)
    for parent in (ROOT, *reversed(path.relative_to(ROOT).parents[:-1])):
        current = parent if parent == ROOT else ROOT / parent
        info = current.lstat()
        require(stat.S_ISDIR(info.st_mode) and not getattr(info, "st_file_attributes", 0) & 0x400, "Input parent is not a retained ordinary directory")
    info = path.lstat()
    require(not stat.S_ISLNK(info.st_mode) and not getattr(info, "st_file_attributes", 0) & 0x400, "Input is a link or reparse point")
    return path, info


def read(name):
    path, info = checked_path(name)
    require(stat.S_ISREG(info.st_mode) and 0 <= info.st_size <= MAX_FILE, "Input is not a bounded regular file: " + name)
    with path.open("rb") as stream:
        raw = stream.read(MAX_FILE + 1)
    require(len(raw) <= MAX_FILE and len(raw) == info.st_size, "Input size changed: " + name)
    if name in inputs:
        require(inputs[name] == raw, "Retained input changed: " + name)
    else:
        require(len(inputs) < 8192 and sum(map(len, inputs.values())) + len(raw) <= 384 * 1024 * 1024, "Input inventory exceeds the reviewed bound")
        inputs[name] = raw
    return raw


def inventory(name, expected):
    path, info = checked_path(name)
    require(stat.S_ISDIR(info.st_mode), "Evidence container is not a directory")
    names = {p.name for p in path.iterdir()}
    require(names == set(expected), "Unexpected or missing evidence files: " + name)
    inventories[name] = names


def sha(value, length=64):
    require(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{%d}" % length, value) and len(set(value)) > 1, "Expected a concrete digest")
    return value


def timestamp(value):
    require(isinstance(value, str), "Missing timestamp")
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(parsed.tzinfo is not None, "Timestamp lacks an explicit timezone")
    return parsed


def source(value, state, manifest, publication):
    require(isinstance(value, dict), "Missing source observation")
    files = manifest["files"]
    if state in {"absent", "unmaterialized"}:
        require(value.get("sha256") == {f["path"]: None for f in files}, "Pre-checkout bytes differ")
        require(value.get("exists") is (state == "unmaterialized"), "Pre-checkout directory state differs")
        if state == "absent":
            require(set(value) == {"exists", "sha256"}, "Absent source has unexpected metadata")
            return
        require(value.get("materialized") is False and value.get("head") == BASE and value.get("tree") == BASE_TREE, "Unmaterialized source identity differs")
        return
    hashes, status = {}, set()
    staged = state == "staged"
    for item in files:
        path = item["path"]
        changed = state in {"candidate", "staged", "published"} or state == "control" and path in manifest["control_paths"]
        hashes[path] = item[("candidate" if changed else "base") + "_sha256"]
        if changed and state != "published":
            code = ("A  " if item["base_sha256"] is None else "D  " if item["candidate_sha256"] is None else "M  ") if staged else ("?? " if item["base_sha256"] is None else " D " if item["candidate_sha256"] is None else " M ")
            status.add(code + path)
    require(value.get("exists") is True and value.get("materialized") is True and value.get("sha256") == hashes, "Observed source bytes differ: " + state)
    require(value.get("head") == (publication["commit"] if state == "published" else BASE), "Observed source head differs")
    require(value.get("tree") == (publication["tree"] if state == "published" else BASE_TREE), "Observed source tree differs")
    require(set(value.get("status", "").splitlines()) == status and set(value.get("staged", "").splitlines()) == ({f["path"] for f in files} if staged else set()), "Observed source status/index differs")
    if state == "published":
        require(value.get("parents") == [BASE], "Published commit is not a single-parent child")


def git_args(phase, cwd, args):
    return ["-c", "core.autocrlf=false", "-c", "core.hooksPath=" + str(ROOT / "runtime" / phase / "empty-hooks"), "-c", "credential.helper=", "-c", "core.askPass=", "-c", "core.fsmonitor=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0", "-C", str(cwd), *args]


def test_args(suite):
    return ["test", "-json", "-tags=test_state_path_env", *(["-race"] if suite["race"] else []), "-count=" + str(suite["count"]), "-timeout=" + str(suite["timeout_seconds"]) + "s", "-run", "^(" + "|".join(re.escape(n) for n in suite["roots"]) + ")$", "./" + suite["package"].removeprefix("mcp-local-hub/")]


def test_oracle(raw, row, suite, qualifying=True):
    lines = raw.splitlines()
    require(len(lines) <= 100000 and all(len(line) <= 256 * 1024 for line in lines), "Go event stream exceeds bound")
    events = [parse(line) for line in lines]
    require(all(isinstance(e, dict) and e.get("Package") == suite["package"] for e in events), "Go stream contains an unexpected package")
    observed = {action: [e.get("Test", "PACKAGE:" + e["Package"]) for e in events if e.get("Action") == action] for action in ("pass", "fail", "skip")}
    require(row.get("tests") == observed, "Go receipt differs from the original event stream")
    if qualifying:
        require(all(Counter(observed[a]) == Counter(suite["expected_events"][a]) for a in observed), "Go terminal-event oracle differs")
        require(row["exit_code"] == suite["expected_exit_code"], "Go exit differs from the frozen oracle")
    for requirement in suite["required_output"] if qualifying else []:
        output = "".join(e.get("Output", "") for e in events if e.get("Test") == requirement["test"])
        require(any(marker in output for marker in requirement["any_of"]), "Expected negative diagnostic absent: " + requirement["test"])
    return {kind + "_" + action: sum(n.startswith("PACKAGE:") == (kind == "package") for n in observed[action]) for kind in ("test", "package") for action in observed}


def commands(phase, manifest, publication):
    paths = [f["path"] for f in manifest["files"]]
    labels = {"control": ["clone", "checkout-base", "check-control", "apply-control"], "candidate": ["check-product", "apply-product"], "published-checkout": ["stage-exact-files", "fetch-exact-commit", "checkout-detached"]}.get(phase, [])[:]
    if phase != "published-checkout":
        labels += ["gofmt-check", "go-version"]
        labels += ["build", "vet"] if phase == "build-vet" else [s["label"] for s in manifest["suites"][phase]]
        labels += ["diff-check"]
    specific = {
        "clone": ["clone", "--single-branch", "--local", "--no-hardlinks", "--no-checkout", str(OLD), str(SOURCE)],
        "checkout-base": ["checkout", "--detach", BASE], "check-control": ["apply", "--check", str(ROOT / "control.patch")], "apply-control": ["apply", str(ROOT / "control.patch")],
        "check-product": ["apply", "--check", str(ROOT / "product.patch")], "apply-product": ["apply", str(ROOT / "product.patch")], "diff-check": ["diff", "--check"],
        "stage-exact-files": ["add", "--", *paths], "fetch-exact-commit": ["fetch", "--no-tags", REMOTE, publication["commit"]], "checkout-detached": ["checkout", "--detach", publication["commit"]],
    }
    suites = {s["label"]: s for s in manifest["suites"].get(phase, [])}
    result = {}
    for label in labels:
        cwd = ROOT if label == "clone" else SOURCE
        if label in specific:
            exe, args, timeout = GIT, git_args(phase, cwd, specific[label]), manifest["command_timeouts"]["git"]
        elif label in suites:
            exe, args, timeout = GO, test_args(suites[label]), suites[label]["direct_timeout_seconds"]
        elif label == "gofmt-check":
            state = "control" if phase == "control" else "candidate"
            existing = [f["path"] for f in manifest["files"] if f[("candidate" if state == "candidate" or f["path"] in manifest["control_paths"] else "base") + "_sha256"] is not None]
            exe, args, timeout = GOFMT, ["-l", *existing], manifest["command_timeouts"]["format"]
        else:
            exe, args = GO, ["version"] if label == "go-version" else [label, "./..."]
            timeout = manifest["command_timeouts"]["format" if label == "go-version" else label]
        result[label] = (exe, args, str(cwd), timeout, suites.get(label))
    return result


def observation(row, phase, publication):
    cwd = row.get("cwd")
    require(cwd in ({str(SOURCE), str(OLD)} if phase == "control" else {str(SOURCE)}), "Observation cwd differs")
    allowed = [["rev-parse", "HEAD"], ["rev-parse", "HEAD^{tree}"], ["show", "-s", "--format=%P", "HEAD"], ["diff", "--cached", "--name-only"], ["status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching"]]
    if phase in {"published-checkout", "published"}:
        allowed += [["diff", "--name-only", BASE, publication["commit"]]]
    if phase == "published-checkout":
        allowed += [["write-tree"], ["show", "-s", "--format=%P", publication["commit"]], ["rev-parse", publication["commit"] + "^{tree}"]]
    require(row.get("executable") == GIT and row.get("args") in [git_args(phase, Path(cwd), a) for a in allowed], "Observation command outside the frozen read scope")


def command_files(folder, row, phase, pins, expected_files, is_observation):
    label = row["label"]
    require(re.fullmatch(r"[a-z][a-z0-9-]{0,79}", label), "Invalid command label")
    require(row.get("settled") is True and row.get("timed_out") is False and not any(key in row for key in ("error", "error_type", "event_error", "source_after_error", "stdout_capture_error", "stderr_capture_error", "process_tree_unresolved", "termination_error")), "Unsettled or failed capture cannot qualify")
    require(row.get("whole_process_tree_deadline_enforced") is False, "Unsupported process-tree deadline claim")
    require(row.get("runner_sha256") == pins["execution_runner_sha256"] and row.get("manifest_sha256") == pins["source_manifest_sha256"], "Command identity differs")
    require(type(row.get("seconds")) in (int, float) and math.isfinite(row["seconds"]) and row["seconds"] >= 0 and type(row.get("pid")) is int and row["pid"] > 0, "Invalid command duration or direct PID")
    require(timestamp(row["finished_utc"]) >= timestamp(row["started_utc"]), "Command timestamp order differs")
    for prefix in ("started", "finished"):
        require(timestamp(row[prefix + "_moscow"]).utcoffset() == dt.timedelta(hours=3) and timestamp(row[prefix + "_moscow"]) >= timestamp(row[prefix + "_utc"]) - dt.timedelta(seconds=2) and timestamp(row[prefix + "_moscow"]) <= timestamp(row[prefix + "_utc"]) + dt.timedelta(seconds=2), "Moscow timestamp differs")
    saved = {}
    for suffix in (".started.json", ".launched.json", ".receipt.json", ".stdout.log", ".stderr.log"):
        name = label + suffix
        require(name not in expected_files, "Duplicate command file")
        expected_files.add(name)
        saved[suffix] = read(folder + "/" + name)
    require(parse(saved[".receipt.json"]) == row, "Phase and command receipts differ")
    started, launched = parse(saved[".started.json"]), parse(saved[".launched.json"])
    keys = {"label", "executable", "args", "cwd", "started_utc", "started_moscow", "settled", "timed_out", "direct_timeout_seconds", "whole_process_tree_deadline_enforced", "runner_sha256", "manifest_sha256"} | (set() if is_observation else {"source_before"})
    require(set(started) == keys and started["settled"] is False and started["timed_out"] is False and all(started[k] == row[k] for k in keys - {"settled"}), "Started receipt differs")
    require(set(launched) == keys | {"pid", "launched_utc"} and all(launched[k] == v for k, v in started.items()) and launched["pid"] == row["pid"] and timestamp(launched["launched_utc"]) >= timestamp(row["started_utc"]), "Launch receipt differs")
    for stream in ("stdout", "stderr"):
        raw = saved["." + stream + ".log"]
        require(row[stream].get("bytes") == len(raw) and row[stream].get("sha256") == digest(raw) and row[stream].get("hash_scope") == "bytes observed after direct-child exit", "Original stream hash or scope differs")
        require(b"WARNING: DATA RACE" not in raw, "Race diagnostic present")
    return saved


def redact(raw, mappings):
    value = raw.decode("utf-8")
    for literal, replacement in sorted(mappings, key=lambda pair: len(pair[0]), reverse=True):
        # Exact known values, with only Windows separator/JSON-escaping variants.
        if "\\" in literal or "/" in literal:
            parts = re.split(r"[\\/]", literal)
            pattern = r"[\\/]{1,4}".join(re.escape(p) for p in parts)
        else:
            pattern = re.escape(literal)
        value = re.sub(r"(?<![A-Za-z0-9_.-])" + pattern + r"(?![A-Za-z0-9_.-])", replacement, value, flags=re.IGNORECASE)
    require(not re.search(r"[A-Za-z]:[\\/]{1,4}Users[\\/]|D:[\\/]{1,4}dev[\\/]{1,4}ChatGPT", value, re.I), "Unredacted concrete profile/work path")
    require(not re.search(r"S-1-5-21(?:-\d+){3,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(?:gh[pousr]_|github_pat_|sk-proj-)[A-Za-z0-9_]{12,}", value), "Unreviewed account SID or credential marker")
    for name in ("USERNAME", "USERDOMAIN", "COMPUTERNAME"):
        literal = os.environ.get(name, "")
        if len(literal) >= 3 and literal.casefold() not in {"system", "nt authority", "workgroup"}:
            require(not re.search(r"(?<![A-Za-z0-9_.-])" + re.escape(literal) + r"(?![A-Za-z0-9_.-])", value, re.I), "Unmapped current account/host identity")
    return value.encode("utf-8")


def make_archive(members):
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, raw in sorted(members.items()):
            info = zipfile.ZipInfo(name, date_time=(2026, 11, 21, 0, 0, 0))
            info.compress_type, info.external_attr = zipfile.ZIP_DEFLATED, 0o600 << 16
            archive.writestr(info, raw)
    raw = buffer.getvalue()
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        require(archive.testzip() is None and len(archive.namelist()) == len(members) and set(archive.namelist()) == set(members), "ZIP inventory/CRC differs")
        require(all(archive.read(name) == value for name, value in members.items()), "ZIP member bytes differ")
    return raw


def initial_diagnostics(config, manifest, publication, pins):
    require(isinstance(config, dict) and set(config) <= {"initial"}, "Only the reviewed initial diagnostic attempt is supported")
    if not config:
        require(not (ROOT / "diagnostics/initial").exists() and not (ROOT / "test-assertion-amendment.json").exists(), "Initial attempted results must not be omitted")
        return {}, 0
    entry = config["initial"]
    require(set(entry) == {"manifest_sha256", "receipt_sha256", "amendment_sha256"} and set(entry["receipt_sha256"]) == {"control", "candidate"}, "Explicit initial diagnostic identities required")
    for value in (entry["manifest_sha256"], entry["amendment_sha256"], *entry["receipt_sha256"].values()):
        sha(value)
    folder = "diagnostics/initial"
    old_raw = read(folder + "/inputs/source-manifest.json")
    old = parse(old_raw)
    require(digest(old_raw) == entry["manifest_sha256"], "Initial source-manifest identity differs")
    require({k: v for k, v in old.items() if k not in {"files", "patch_sha256"}} == {k: v for k, v in manifest.items() if k not in {"files", "patch_sha256"}}, "Diagnostic amendment changed suite selection, base or runners")
    require([f["path"] for f in old["files"]] == [f["path"] for f in manifest["files"]], "Diagnostic amendment changed source-file scope")
    changes = [(a, b) for a, b in zip(old["files"], manifest["files"]) if a != b]
    require(len(changes) == 1 and changes[0][0]["path"] == "internal/api/install_task_recovery_test.go" and all(changes[0][0][k] == changes[0][1][k] for k in ("path", "base_blob", "base_sha256")), "Diagnostic amendment was not limited to the reviewed test")
    require(old["patch_sha256"]["product.patch"] == manifest["patch_sha256"]["product.patch"], "Product bytes changed in the diagnostic amendment")
    for name, expected in old["patch_sha256"].items():
        require(name in {"control.patch", "product.patch", "candidate.patch"} and digest(read(folder + "/inputs/" + name)) == expected, "Initial patch differs")
    inventory(folder + "/inputs", {"source-manifest.json", "control.patch", "product.patch", "candidate.patch"})
    amendment_raw = read("test-assertion-amendment.json")
    amendment = parse(amendment_raw)
    require(digest(amendment_raw) == entry["amendment_sha256"] and amendment.get("product_changes") is False and amendment.get("changed_files") == ["internal/api/install_task_recovery_test.go"] and amendment.get("old_manifest_sha256") == entry["manifest_sha256"] and amendment.get("new_manifest_sha256") == pins["source_manifest_sha256"], "Diagnostic amendment record differs")
    require(amendment.get("retained_directory") == str(ROOT / "diagnostics/initial"), "Initial retained directory differs")
    inventory(folder + "/evidence", {"control", "candidate"})
    result, streams, receipts = {}, 0, {}
    old_pins = {**pins, "source_manifest_sha256": entry["manifest_sha256"]}
    for phase in ("control", "candidate"):
        directory = folder + "/evidence/" + phase
        receipt_raw = read(directory + "/receipt.json")
        receipt, initial = parse(receipt_raw), parse(read(directory + "/receipt-initial.json"))
        receipts[phase] = receipt
        require(digest(receipt_raw) == entry["receipt_sha256"][phase], "Initial attempted receipt differs")
        require(receipt.get("schema") == 2 and receipt.get("phase") == phase and receipt.get("base") == BASE and receipt.get("base_tree") == BASE_TREE and receipt.get("runner_sha256") == pins["execution_runner_sha256"] and receipt.get("manifest_sha256") == entry["manifest_sha256"] and receipt.get("patch_sha256") == old["patch_sha256"], "Initial attempt source identity differs")
        require(not receipt.get("process_tree_unresolved") and receipt.get("native_task_operations") is False and receipt.get("whole_process_tree_deadline_enforced") is False and receipt.get("direct_child_wait_bounded") is True and receipt.get("process_tree_settlement_observed") is False, "Initial attempt scope/deadline differs")
        require(initial.get("completed") is False and initial.get("commands") == [] and initial.get("observations") == [] and initial.get("state") == "preflight" and all(receipt.get(k) == v for k, v in initial.items() if k not in {"completed", "commands", "observations", "state"}), "Initial attempted receipt header changed")
        source(receipt["source_before"], "base" if phase == "control" else "control", old, publication)
        source(receipt["source_final"], "control" if phase == "control" else "candidate", old, publication)
        if phase == "control":
            require(receipt.get("completed") is True and receipt.get("state") == "complete" and receipt.get("negative_control_confirmed") is True and receipt["source_after"] == receipt["source_final"], "Initial control did not complete")
        else:
            # The runner rejects exit 1 before its event-count oracle; the
            # command_files checks below still require an actually settled run.
            require(receipt.get("completed") is False and receipt.get("state") == "failed" and receipt.get("error_type") == "RuntimeError" and receipt.get("error") == "Test command did not settle with the required exit", "Initial candidate failed in a different way")
        expected = commands(phase, old, publication)
        actual = [r["label"] for r in receipt["commands"]]
        require(actual == list(expected)[:len(actual)] and (phase != "control" or len(actual) == len(expected)), "Initial command selection/order differs")
        require(actual and (phase != "candidate" or actual[-1] == old["suites"]["candidate"][0]["label"]), "Initial candidate did not stop at the documented first suite")
        require(1 <= len(receipt["observations"]) <= 1000 and [r["label"] for r in receipt["observations"]] == ["observation-%04d" % i for i in range(1, len(receipt["observations"]) + 1)], "Initial observation inventory differs")
        names, rows = {"receipt.json", "receipt-initial.json"}, []
        for row in receipt["observations"]:
            observation(row, phase, publication)
            require(row["exit_code"] == 0 and row["direct_timeout_seconds"] == old["command_timeouts"]["git"] and "tests" not in row, "Initial observation failed")
            command_files(directory, row, phase, old_pins, names, True)
            streams += 2
        state = "absent" if phase == "control" else "control"
        for row in receipt["commands"]:
            exe, args, cwd, timeout, suite = expected[row["label"]]
            require((row["executable"], row["args"], row["cwd"], row["direct_timeout_seconds"]) == (exe, args, cwd, timeout), "Initial command differs")
            source(row["source_before"], state, old, publication)
            state = {"clone": "unmaterialized", "checkout-base": "base", "apply-control": "control", "apply-product": "candidate"}.get(row["label"], state)
            source(row["source_after"], state, old, publication)
            saved = command_files(directory, row, phase, old_pins, names, False)
            counts = test_oracle(saved[".stdout.log"], row, suite, qualifying=phase == "control") if suite else None
            require(row["exit_code"] == (1 if suite else 0), "Initial attempted command exit differs")
            if row["label"] == "gofmt-check":
                require(not saved[".stdout.log"].strip(), "Initial source needed formatting")
            rows.append({"label": row["label"], "exit_code": row["exit_code"], "seconds": row["seconds"], "counts": counts, "outcome": ("expected FAIL retained" if phase == "control" else "FAIL retained diagnostic") if suite else "PASS", "evidence_path": directory + "/" + row["label"] + ".receipt.json"})
            streams += 2
        inventory(directory, names)
        result[phase] = {"completed": receipt["completed"], "state": receipt["state"], "receipt_source_sha256": digest(receipt_raw), "source_before": receipt["source_before"], "source_final": receipt["source_final"], "commands": rows, "observations": len(receipt["observations"]), "finished_utc": receipt["finished_utc"], "finished_moscow": receipt["finished_moscow"]}
    require(receipts["candidate"].get("control_rerun") is False and receipts["candidate"].get("retained_control_receipt_sha256") == entry["receipt_sha256"]["control"], "Initial retained control link differs")
    counts = result["candidate"]["commands"][-1]["counts"]
    require(amendment.get("initial_pass_events") == counts["test_pass"] + counts["package_pass"] and amendment.get("initial_fail_events") == counts["test_fail"] + counts["package_fail"] and counts["test_skip"] == counts["package_skip"] == 0, "Initial failure counters differ from amendment record")
    return {"initial": {"source_manifest_sha256": entry["manifest_sha256"], "amendment_source_sha256": digest(amendment_raw), "qualifies_final_candidate": False, "product_patch_unchanged": True, "phases": result, "reason_as_recorded": amendment["reason"], "full_original_streams_retained": True}}, streams


def supplemental(public_entries, private_entries):
    require(isinstance(public_entries, list) and isinstance(private_entries, list) and len(public_entries) + len(private_entries) <= 2048, "Supplemental allowlist is not bounded")
    public, private, directories, names = [], [], {}, set()
    native_json = r"native-r21-[0-9a-f]{32}/(?:manifest\.json|observations\.jsonl)"
    native_xml = r"(?:native-r21-[0-9a-f]{32}/export-[12]\.xml|diagnostics/native-observation/retained-task\.xml)"
    outer = r"evidence/(?:native-qualification|native-observer|native-qualification-final|native-retained-cleanup)/[A-Za-z0-9_.-]+\.(?:json|log)"
    observation_json = {"diagnostics/native-observation/intent.json", "diagnostics/native-observation/observation.json", "diagnostics/native-observation/comparison.json"}
    setup_receipt = "diagnostics/native-preflight/evidence/receipt.json"
    for retained_receipt in (setup_receipt, "evidence/native-qualification/receipt.json"):
        require(not (ROOT / retained_receipt).exists() or any(entry.get("path") == retained_receipt for entry in public_entries), "Retained native attempt receipt must not be omitted: " + retained_receipt)
    for entry in public_entries + private_entries:
        path = str(relative(entry["path"]))
        require(path not in names, "Duplicate supplemental artifact")
        names.add(path)
        is_private = entry in private_entries
        if is_private:
            require(set(entry) == {"path", "sha256", "bytes"} and (re.fullmatch(native_xml, path) or re.fullmatch(outer, path)), "Private supplemental artifact outside the reviewed native scope")
        else:
            require(set(entry) == {"path", "sha256", "format"} and (re.fullmatch(native_json, path) or re.fullmatch(outer, path) or path in observation_json or path == setup_receipt), "Public supplemental artifact outside the reviewed scope")
        raw = read(path)
        require(digest(raw) == sha(entry["sha256"]), "Supplemental artifact hash differs")
        directory, leaf = path.rsplit("/", 1)
        if path != setup_receipt:
            directories.setdefault(directory, set()).add(leaf)
        if is_private:
            require(type(entry["bytes"]) is int and entry["bytes"] == len(raw), "Invalid retained private artifact size")
            private.append({"path": path, "bytes": len(raw), "sha256": digest(raw), "raw_bytes_archived": False})
            continue
        value = raw.decode("utf-8")
        require("\0" not in value and not re.search(r"<\??(?:xml|Task)\b|&lt;\??(?:xml|Task)\b|\\u003[cC]", value, re.I), "Native XML bytes must remain private")
        native_json_without_xml(value)
        require(entry["format"] in {"json", "jsonl", "text"}, "Supplemental public artifact must be reviewed text")
        if entry["format"] == "json":
            parsed = parse(raw)
            native_json_without_xml(parsed)
            if path == setup_receipt:
                require(parsed.get("completed") is False and parsed.get("native_task_operations") is False and parsed.get("commands") == [], "Setup diagnostic unexpectedly contains product/native execution")
        elif entry["format"] == "jsonl":
            require(len(raw.splitlines()) <= 100000, "Unbounded native event stream")
            for line in raw.splitlines():
                native_json_without_xml(parse(line))
        else:
            # A .log can contain JSON despite its outer format. Inspect decoded
            # scalar values too, while retaining the original bytes unchanged.
            for line in raw.splitlines():
                if line.lstrip().startswith((b"{", b"[")):
                    try:
                        parsed = parse(line)
                    except (ValueError, UnicodeError):
                        continue
                    native_json_without_xml(parsed)
        require(not path.endswith(".json") or entry["format"] == "json", "JSON file needs JSON validation")
        require(not path.endswith(".jsonl") or entry["format"] == "jsonl", "JSONL file needs event validation")
        public.append({"path": path, "source_sha256": digest(raw), "source_bytes": len(raw), "kind": "setup_diagnostic" if path == setup_receipt else "native_evidence"})
    for directory, leaves in directories.items():
        inventory(directory, leaves)
    return {"public_files": public, "private_artifacts": private, "native_artifacts_present": any(row["kind"] == "native_evidence" for row in public), "setup_diagnostic_streams_reverified": False, "native_execution_outcome_verified_by_packer": False, "interpretation": "Supplemental hashes and text privacy checked; native behavior/cleanup conclusions require independent review. A setup diagnostic receipt retains recorded stream hashes, but its private duplicate streams are not reread by this packer."}, {row["path"] for row in private}, directories


def native_json_without_xml(value, depth=0):
    require(depth <= 32, "Native JSON nesting exceeds reviewed bound")
    if isinstance(value, dict):
        require(not any(re.fullmatch(r"(?:raw_)?xml(?:_bytes|_base64|_text|_data)?|base64|payload", key, re.I) for key in value), "Raw native XML/payload fields require separate explicit redaction")
        for child in value.values():
            native_json_without_xml(child, depth + 1)
    elif isinstance(value, list):
        for child in value:
            native_json_without_xml(child, depth + 1)
    elif isinstance(value, str):
        require("\0" not in value and not re.search(r"<\??(?:xml|Task)\b|&lt;\??(?:xml|Task)\b", value, re.I), "Decoded native XML must remain private")
        # Inspect recognizable Base64 tokens anywhere in a text log, not only
        # a complete JSON string. Remove UTF-16 zero bytes solely for detection.
        for match in re.finditer(r"(?<![A-Za-z0-9+/])([A-Za-z0-9+/]{40,}={0,2})(?![A-Za-z0-9+/=])", value):
            try:
                decoded = base64.b64decode(match.group(1), validate=True)
            except ValueError:
                continue
            probe = decoded.replace(b"\0", b"").lower()
            require(b"<task" not in probe and b"<?xml" not in probe, "Encoded native XML must remain private")


def main():
    require(os.name == "nt" and not sys.flags.optimize and len(sys.argv) == 1 and Path(__file__).resolve() == ROOT / "pack_public.py", "Use the reviewed script unoptimized from the approved Windows root")
    require(not (ROOT / "public-evidence").exists() and not (ROOT / "packaging-private").exists(), "Retain the earlier package; choose a separately reviewed fresh output")
    config_raw = read("packaging-input.json")
    config = parse(config_raw)
    require(set(config) == {"schema", "observed_at_utc", "pins", "redactions", "diagnostics", "extra_public_files", "private_artifacts"} and config["schema"] == 1, "Unexpected packaging input")
    observed = timestamp(config["observed_at_utc"])
    require(observed.utcoffset() == dt.timedelta(0), "Observed time must be UTC")
    pins = config["pins"]
    require(set(pins) == {"source_manifest_sha256", "execution_runner_sha256", "checkout_runner_sha256", "publication_input_sha256"}, "Expected four explicit input pins")
    for value in pins.values():
        sha(value)
    raw = {name: read(name) for name in STATIC}
    require(digest(raw["source-manifest.json"]) == pins["source_manifest_sha256"] and digest(raw["check_recovery.py"]) == pins["execution_runner_sha256"] and digest(raw["checkout_published.py"]) == pins["checkout_runner_sha256"] and digest(raw["publication-input.json"]) == pins["publication_input_sha256"], "Pinned source/script/publication bytes differ")
    manifest, publication = parse(raw["source-manifest.json"]), parse(raw["publication-input.json"])
    require(manifest.get("schema") == 2 and manifest.get("base_commit") == BASE and manifest.get("base_tree") == BASE_TREE and manifest.get("native_scheduler_operations") is False, "Source manifest base/scope differs")
    require(manifest["execution_runner_sha256"] == pins["execution_runner_sha256"] and manifest["generator_sha256"] == digest(raw["prepare_source.py"]) and manifest["source_plan_sha256"] == digest(raw["source-plan.json"]) and manifest["patch_sha256"] == {name: digest(raw[name]) for name in ("control.patch", "product.patch", "candidate.patch")}, "Frozen source preparation differs")
    require(set(publication) == {"schema", "base", "commit", "tree", "parents", "checkout_runner_sha256", "execution_runner_sha256", "source_manifest_sha256"} and publication["schema"] == 1 and publication["base"] == BASE and publication["parents"] == [BASE] and publication["commit"] != BASE, "Publication ancestry differs")
    for key in ("commit", "tree"):
        sha(publication[key], 40)
    require(all(publication[k] == pins[k] for k in pins if k != "publication_input_sha256"), "Publication script/manifest identities differ")
    require(isinstance(manifest["files"], list) and 1 <= len(manifest["files"]) <= 64, "Unbounded source path list")
    paths = [str(relative(f["path"])) for f in manifest["files"]]
    require(len(set(paths)) == len(paths) and all(p.endswith(".go") for p in paths), "Duplicate or non-Go source path")
    for item in manifest["files"]:
        if item["candidate_sha256"] is None:
            require(not (SOURCE / item["path"]).exists(), "Deleted candidate path exists")
            continue
        data = read("source/" + item["path"])
        require(digest(data) == item["candidate_sha256"] and hashlib.sha1(("blob " + str(len(data)) + "\0").encode() + data).hexdigest() == item["candidate_blob"], "Published source bytes differ")
    supplements, private_paths, supplemental_dirs = supplemental(config["extra_public_files"], config["private_artifacts"])
    inventory("evidence", {*PHASES, *(name.removeprefix("evidence/") for name in supplemental_dirs if name.startswith("evidence/"))})
    diagnostic_attempts, diagnostic_streams = initial_diagnostics(config["diagnostics"], manifest, publication, pins)
    receipts, phases, summaries = {}, {}, []
    stream_count = 0
    for phase in PHASES:
        folder = "evidence/" + phase
        receipt_raw = read(folder + "/receipt.json")
        receipt, initial = parse(receipt_raw), parse(read(folder + "/receipt-initial.json"))
        receipts[phase] = receipt
        require(receipt.get("schema") == 2 and receipt.get("phase") == phase and receipt.get("base") == BASE and receipt.get("base_tree") == BASE_TREE and receipt.get("completed") is True and receipt.get("state") == "complete" and not receipt.get("process_tree_unresolved") and not receipt.get("error"), "Incomplete phase: " + phase)
        require(receipt.get("runner_sha256") == pins["execution_runner_sha256"] and receipt.get("manifest_sha256") == pins["source_manifest_sha256"] and receipt.get("patch_sha256") == manifest["patch_sha256"] and receipt.get("source_plan_sha256") == manifest["source_plan_sha256"] and receipt.get("command_timeouts") == manifest["command_timeouts"], "Phase source identity differs")
        require(receipt.get("native_task_operations") is False and receipt.get("whole_process_tree_deadline_enforced") is False and receipt.get("direct_child_wait_bounded") is True and receipt.get("process_tree_settlement_observed") is False, "Phase scope/deadline claims differ")
        require(timestamp(receipt["finished_utc"]) <= observed and timestamp(receipt["finished_utc"]) >= timestamp(receipt["started_utc"]), "Observed time predates phase completion")
        require(initial.get("completed") is False and initial.get("commands") == [] and initial.get("observations") == [] and initial.get("state") == "preflight", "Initial receipt differs")
        require(all(receipt.get(k) == v for k, v in initial.items() if k not in {"completed", "commands", "observations", "state"}), "Initial identity/configuration changed")
        environment = receipt["environment"]
        tool_hashes = receipt.get("tools_sha256", {})
        require(set(tool_hashes) == {GO, GOFMT, GIT, r"C:\msys64\ucrt64\bin\gcc.exe"}, "Installed tool inventory differs")
        for value in tool_hashes.values():
            sha(value)
        require(tool_hashes == receipts["control"]["tools_sha256"], "Installed tools changed between phases")
        require(all(environment.get(k) == v for k, v in ENV.items()), "Offline/test-isolation environment differs")
        for key, suffix in {"USERPROFILE": "profile", "APPDATA": "appdata", "LOCALAPPDATA": "localappdata", "TEMP": "tmp", "GOTMPDIR": "tmp", "XDG_CACHE_HOME": "xdg-cache"}.items():
            require(environment.get(key) == str(ROOT / "runtime" / phase / suffix), "Output/profile isolation differs")
        require(environment.get("GOCACHE") == r"<WORK_ROOT>\mcp-pr616-qualification-20261114\gocache" and environment.get("GOMODCACHE") == r"<WORK_ROOT>\mcp-pr616-qualification-20261114\gomodcache", "Preexisting offline caches differ")
        before_state = "base" if phase == "control" else "control" if phase == "candidate" else "published" if phase == "published" else "candidate"
        after_state = "control" if phase == "control" else "published" if phase in {"published", "published-checkout"} else "candidate"
        source(receipt["source_before"], before_state, manifest, publication)
        source(receipt["source_after"], after_state, manifest, publication)
        require(receipt["source_final"] == receipt["source_after"] and receipt["source_sha256_before"] == receipt["source_before"]["sha256"] and receipt["source_sha256_after"] == receipt["source_after"]["sha256"] and receipt["source_head"] == receipt["source_after"]["head"] and receipt["source_tree"] == receipt["source_after"]["tree"] and receipt["working_status"] == receipt["source_after"]["status"], "Final source observation differs")
        if phase in {"published", "published-checkout"}:
            require(receipt.get("publication_input_sha256") == pins["publication_input_sha256"], "Publication input receipt differs")
        if phase == "published-checkout":
            require(receipt.get("checkout_runner_sha256") == pins["checkout_runner_sha256"] and receipt.get("expected_commit") == publication["commit"] and receipt.get("expected_tree") == publication["tree"] and receipt.get("remote_read_url") == REMOTE, "Checkout receipt identity differs")
        expected = commands(phase, manifest, publication)
        require([r["label"] for r in receipt["commands"]] == list(expected), "Command selection/order differs")
        require(1 <= len(receipt["observations"]) <= 1000 and [r["label"] for r in receipt["observations"]] == ["observation-%04d" % i for i in range(1, len(receipt["observations"]) + 1)], "Observation labels are not complete and sequential")
        names = {"receipt.json", "receipt-initial.json"}
        for row in receipt["observations"]:
            observation(row, phase, publication)
            require(row["exit_code"] == 0 and row["direct_timeout_seconds"] == manifest["command_timeouts"]["git"] and "tests" not in row, "Observation did not complete as expected")
            command_files(folder, row, phase, pins, names, True)
            stream_count += 2
        state = "absent" if phase == "control" else before_state
        transitions = {"clone": "unmaterialized", "checkout-base": "base", "apply-control": "control", "apply-product": "candidate", "stage-exact-files": "staged", "checkout-detached": "published"}
        for row in receipt["commands"]:
            label = row["label"]
            exe, args, cwd, timeout, suite = expected[label]
            require((row["executable"], row["args"], row["cwd"], row["direct_timeout_seconds"]) == (exe, args, cwd, timeout), "Command arguments/deadline differ: " + label)
            source(row["source_before"], state, manifest, publication)
            state = transitions.get(label, state)
            source(row["source_after"], state, manifest, publication)
            saved = command_files(folder, row, phase, pins, names, False)
            counts = test_oracle(saved[".stdout.log"], row, suite) if suite else None
            require(suite is not None or row["exit_code"] == 0 and "tests" not in row, "Non-test command did not pass")
            if label == "gofmt-check":
                require(not saved[".stdout.log"].strip(), "Source needs formatting")
            summaries.append({"phase": phase, "label": label, "executable": exe, "args": args, "exit_code": row["exit_code"], "seconds": row["seconds"], "counts": counts, "outcome": "expected FAIL retained" if suite and suite["expected_exit_code"] == 1 else "PASS", "evidence_path": folder + "/" + label + ".receipt.json", "stdout": row["stdout"], "stderr": row["stderr"]})
            stream_count += 2
        require(state == after_state, "Phase source transition ended in an unexpected state")
        inventory(folder, names)
        phases[phase] = {"completed": True, "receipt_source_sha256": digest(receipt_raw), "source_before": receipt["source_before"], "source_after": receipt["source_after"], "commands": len(receipt["commands"]), "observations": len(receipt["observations"]), "finished_utc": receipt["finished_utc"], "finished_moscow": receipt["finished_moscow"]}
    require(receipts["control"].get("negative_control_confirmed") is True and receipts["candidate"].get("control_rerun") is False and receipts["candidate"].get("retained_control_receipt_sha256") == digest(inputs["evidence/control/receipt.json"]), "Retained negative-control link differs")
    source(receipts["control"]["clone_source"], "base", manifest, publication)
    source(receipts["control"]["control_source"], "control", manifest, publication)
    source(receipts["candidate"]["candidate_source"], "candidate", manifest, publication)
    summary = {"schema": 2, "publication": publication, "phases": phases, "commands": summaries, "diagnostic_attempts": diagnostic_attempts, "supplemental_evidence": supplements, "raw_command_streams_verified": stream_count + diagnostic_streams, "qualification_streams_verified": stream_count, "diagnostic_streams_verified": diagnostic_streams, "expected_failures_retained": True, "control_executions": 1 + len(diagnostic_attempts), "initial_attempt_control_and_candidate_retained": bool(diagnostic_attempts), "final_candidate_control_reused": True, "observed_at_utc": observed.isoformat(), "observed_at_moscow": observed.astimezone(dt.timezone(dt.timedelta(hours=3))).isoformat(), "limits": {"qualification_phase_direct_child_wait_bounded": True, "whole_process_tree_deadline_enforced": False, "process_tree_settlement_observed": False, "qualification_phase_native_task_operations": False, "native_artifacts_included": supplements["native_artifacts_present"], "raw_native_xml_included": False, "full_test_suites_run": False, "native_concurrent_replacement_qualified": False}}
    mappings = [(str(ROOT), "<RECOVERY_ROOT>"), (str(OLD.parent), "<INSTALL_SNAPSHOT_ROOT>"), (r"<WORK_ROOT>", "<WORK_ROOT>")]
    require(isinstance(config["redactions"], list) and len(config["redactions"]) <= 32, "Unbounded private redaction map")
    for item in config["redactions"]:
        require(set(item) == {"literal", "replacement"} and isinstance(item["literal"], str) and 3 <= len(item["literal"]) <= 512 and not any(c in item["literal"] for c in "\r\n\0") and re.fullmatch(r"<[A-Z][A-Z0-9_]{2,63}>", item["replacement"]), "Redaction must name a concrete private identity and stable placeholder")
        mappings.append((item["literal"], item["replacement"]))
    require(len({literal.casefold() for literal, _ in mappings}) == len(mappings), "Duplicate private redaction identity")
    members, rows = {}, []
    for name, original in [*((name, data) for name, data in inputs.items() if name != "packaging-input.json" and name not in private_paths and not name.startswith("source/")), ("execution-summary.json", encode(summary))]:
        public = redact(original, mappings)
        if name.endswith(".json"):
            parse(public)
        members[name] = public
        rows.append({"path": name, "source_bytes": len(original), "source_sha256": digest(original), "saved_bytes": len(public), "saved_sha256": digest(public), "redacted": public != original, "generated_in_memory": name == "execution-summary.json"})
    rows.sort(key=lambda r: r["path"])
    catalog = {"schema": 2, "files": rows, "source_manifest_sha256": pins["source_manifest_sha256"], "original_diagnostics_retained": True, "native_artifacts_included": supplements["native_artifacts_present"], "raw_native_xml_included": False, "private_artifacts_verified_not_archived": supplements["private_artifacts"], "source_files_verified_not_archived": {item["path"]: item["candidate_sha256"] for item in manifest["files"]}, "redaction": "Known local roots and explicitly mapped private identities only; no diagnostic fields removed. Original hashes identify retained private input bytes; saved hashes identify public bytes. Scripts/patches are redacted audit records, not execution inputs. MANIFEST.json is hashed outside the archive to avoid self-reference."}
    members["MANIFEST.json"] = encode(catalog)
    archived = make_archive(members)
    outputs = {"evidence-redacted.zip": archived, "MANIFEST.json": members["MANIFEST.json"], "execution-summary.json": members["execution-summary.json"], "evidence-transfer.b64": base64.b64encode(archived)}
    require(base64.b64decode(outputs["evidence-transfer.b64"], validate=True) == archived, "Transfer encoding differs")
    metadata = {"schema": 2, "archive": "evidence-redacted.zip", "bytes": len(archived), "sha256": digest(archived), "entries": len(members), "manifest_files": len(rows), "raw_command_streams_verified": stream_count + diagnostic_streams, "crc_and_manifest_verified": True, "original_hashes_distinct_from_saved_hashes": True, "saved_files": {name: {"bytes": len(data), "sha256": digest(data)} for name, data in outputs.items()}, "metadata_self_hash_omitted_to_avoid_self_reference": True}
    outputs["archive-metadata.json"] = encode(metadata)
    # Re-read the full original allowlist and exact phase inventories before writing.
    for name in tuple(inputs):
        read(name)
    for name, names in tuple(inventories.items()):
        inventory(name, names)
    private = {"schema": 1, "packaging_input_sha256": digest(config_raw), "packer_sha256": digest(raw["pack_public.py"]), "observed_at_utc": observed.isoformat(), "observed_at_moscow": summary["observed_at_moscow"], "inputs_retained_in_place": True, "inputs": [{"path": name, "bytes": len(data), "sha256": digest(data)} for name, data in sorted(inputs.items())], "public_outputs": metadata["saved_files"], "native_inputs_read": bool(supplements["public_files"] or private_paths), "raw_native_xml_in_public_outputs": False}
    (ROOT / "packaging-private").mkdir(exist_ok=False)
    with (ROOT / "packaging-private/input-audit.json").open("xb") as stream:
        stream.write(encode(private))
    out = ROOT / "public-evidence"
    out.mkdir(exist_ok=False)
    for name, data in outputs.items():
        with (out / name).open("xb") as stream:
            stream.write(data)
        require((out / name).read_bytes() == data, "Saved output differs from verified memory")
    print(json.dumps(metadata, sort_keys=True), flush=True)


if __name__ == "__main__":
    main()
