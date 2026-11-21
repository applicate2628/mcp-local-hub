"""Verify a public R24 ZIP without extraction, execution, or private literals.

This checks archive integrity and internal consistency, not runtime authenticity.
An optional externally obtained SHA-256 binds the exact input archive bytes.
"""
import sys
sys.dont_write_bytecode = True

import argparse
from collections import Counter
import hashlib
import io
import json
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import stat
import zipfile


SCOPES = {"pr635", "pr637", "pr643", "pr647", "pr650"}
PHASES = {"prepare", "qualify", "build-vet"}
EXPECTED = {"phases": 15, "candidate_go_files": 13, "test_commands": 11,
            "root_pass_events": 57, "build_vet_commands": 10}
TOTAL_KEYS = ("root_pass_events", "test_and_subtest_pass_events", "leaf_pass_events",
              "package_pass_events", "fail_events", "skip_events")


class InvalidArchive(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise InvalidArchive(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def safe_name(name):
    require(isinstance(name, str) and name and "\\" not in name and ":" not in name and "\0" not in name,
            "Invalid archive path")
    path = PurePosixPath(name)
    require(not path.is_absolute() and str(path) == name and all(p not in (".", "..") for p in path.parts),
            "Non-relative or non-canonical archive path")
    return name


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "Duplicate JSON object key")
        result[key] = value
    return result


def json_value(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=unique_object)


def object_value(raw, name):
    result = json_value(raw)
    require(isinstance(result, dict), "JSON object required: " + name)
    return result


def keyed(rows, key, name):
    require(isinstance(rows, list) and all(isinstance(row, dict) and key in row for row in rows), "Invalid rows: " + name)
    result = {}
    for row in rows:
        value = row[key]
        require(isinstance(value, str) and value not in result, "Duplicate or invalid key: " + name)
        result[value] = row
    return result


def count_events(raw, row, command, name):
    events = [object_value(line, name) for line in raw.splitlines()]
    terminal = {action: [event.get("Test", "PACKAGE:" + event.get("Package", ""))
                         for event in events if event.get("Action") == action]
                for action in ("pass", "fail", "skip")}
    require(row.get("tests") == terminal, "Receipt disagrees with JSON events: " + name)
    require(not terminal["fail"] and not terminal["skip"], "FAIL or SKIP event: " + name)
    roots = Counter(item for item in terminal["pass"] if not item.startswith("PACKAGE:") and "/" not in item)
    require(roots == Counter(command["required_root_pass_counts"]), "Root oracle differs: " + name)
    require(sum(roots.values()) == command["expected_root_count"], "Root count differs: " + name)
    package = "mcp-local-hub/" + command["args"][-1].removeprefix("./")
    require(all(event.get("Package") == package for event in events), "Unexpected event package: " + name)
    require(terminal["pass"].count("PACKAGE:" + package) == 1, "Package PASS count differs: " + name)
    passed = [item for item in terminal["pass"] if not item.startswith("PACKAGE:")]
    all_names = {item for items in terminal.values() for item in items if not item.startswith("PACKAGE:")}
    leaves = {item for item in passed if not any(other.startswith(item + "/") for other in all_names)}
    return {"root_pass_events": sum(roots.values()), "test_and_subtest_pass_events": len(passed),
            "leaf_pass_events": sum(item in leaves for item in passed), "package_pass_events": 1,
            "fail_events": len(terminal["fail"]), "skip_events": len(terminal["skip"])}


def verify(archive_path, expected_sha):
    archive_bytes = archive_path.read_bytes()
    digest = sha(archive_bytes)
    if expected_sha is not None:
        require(re.fullmatch(r"[0-9a-f]{64}", expected_sha) and digest == expected_sha, "Archive SHA-256 differs from supplied pin")
    with zipfile.ZipFile(io.BytesIO(archive_bytes)) as archive:
        infos = archive.infolist()
        names = [safe_name(info.filename) for info in infos]
        require(len(names) == len(set(names)) == len({name.casefold() for name in names}), "Duplicate ZIP member")
        require(all(not info.is_dir() and info.filename == info.orig_filename and
                    not stat.S_ISLNK(info.external_attr >> 16) for info in infos), "Directory, truncated name or symlink member")
        require(sum(info.file_size for info in infos) <= 512 * 1024 * 1024, "R24 archive exceeds 512 MiB verification bound")
        require(archive.testzip() is None, "ZIP CRC failure")
        data = {info.filename: archive.read(info) for info in infos}

    def document(name):
        require(name in data, "Missing archive member: " + name)
        return object_value(data[name], name)

    manifest = document("_package/manifest.json")
    summary = document("_package/summary.json")
    require(manifest.get("schema") == "r24-raw-public-file-map-v1", "Unexpected manifest schema")
    require(summary.get("schema") == "r24-evidence-summary-v1", "Unexpected summary schema")
    mapping = keyed(manifest.get("files"), "path", "manifest files")
    for name in mapping:
        safe_name(name)
        require(not name.startswith("_package/"), "Generated member listed as an input")
    require(set(data) == set(mapping) | {"_package/manifest.json", "_package/summary.json"}, "ZIP input membership differs from manifest")
    require(manifest.get("input_file_count") == len(mapping), "Manifest file count differs")
    changed = 0
    for name, item in mapping.items():
        require(type(item.get("public_bytes")) is int and item["public_bytes"] == len(data[name]) and
                item.get("public_sha256") == sha(data[name]), "Public size or digest differs: " + name)
        require(type(item.get("original_bytes")) is int and item["original_bytes"] >= 0 and
                isinstance(item.get("original_sha256"), str) and re.fullmatch(r"[0-9a-f]{64}", item["original_sha256"]), "Invalid original hash claim: " + name)
        require(type(item.get("changed")) is bool, "Invalid changed flag: " + name)
        if item["changed"]:
            changed += 1
        else:
            require(item["original_bytes"] == item["public_bytes"] and item["original_sha256"] == item["public_sha256"], "Unchanged input has unequal hashes: " + name)
    require(manifest.get("sanitized_file_count") == changed, "Changed-file count differs")
    require(manifest["generated_summary"]["public_sha256"] == sha(data["_package/summary.json"]), "Embedded summary digest differs")
    require(summary.get("qualification_state") == "PASS_SELECTED_SCOPE" and summary.get("issues") == [], "Summary does not report a clean selected qualification")
    require(summary.get("packaging_state") == "CAPTURED_FOR_ARCHIVE", "Unexpected packaging summary state")
    require(summary.get("expected") == EXPECTED, "Expected scope counts differ")
    reports = keyed([{**p, "key": p["scope"] + "-" + p["phase"]} for p in summary["phases"]], "key", "phase reports")
    require(set(reports) == {scope + "-" + phase for scope in SCOPES for phase in PHASES}, "Phase set differs")
    index = keyed(json_value(data["scope-index.json"]), "scope", "scope index")
    require(set(index) == SCOPES, "Scope index differs")
    pins = keyed([{**p, "key": p["scope"] + ":" + p["path"]} for p in manifest["original_source_pins"]], "key", "source preimages")
    require(len(pins) == EXPECTED["candidate_go_files"], "Original preimage count differs")

    def stream_row(row, folder):
        require(isinstance(row.get("label"), str) and re.fullmatch(r"[a-z][a-z0-9-]{0,63}", row["label"]), "Invalid command label")
        name = folder + "/" + row["label"]
        require(document(name + ".receipt.json") == {k: v for k, v in row.items() if k != "scope_oracle_matched"}, "Command receipt differs: " + name)
        require(row.get("settled") is True and row.get("timed_out") is False and row.get("exit_code") == 0 and
                not {"error_type", "event_error_type", "source_after_error", "stdout_capture_error", "stderr_capture_error"}.intersection(row), "Command did not succeed: " + name)
        require(row.get("runner_sha256") == mapping["frozen-owner/check_recovery.py"]["original_sha256"], "Command capture owner differs")
        for stream in ("stdout", "stderr"):
            path = name + "." + stream + ".log"
            require(path in mapping, "Missing stream: " + path)
            require(row[stream]["bytes"] == mapping[path]["original_bytes"] and row[stream]["sha256"] == mapping[path]["original_sha256"], "Stream receipt digest differs: " + path)
        return data[name + ".stdout.log"]

    commands_verified = []
    totals = Counter({key: 0 for key in TOTAL_KEYS})
    build_vet_count = 0
    candidates = set()
    expected_payload = {"qualify_scope.py", "scope-index.json"}
    for scope in sorted(SCOPES):
        manifest_name = "scopes/" + scope + "/manifest.json"
        expected_payload.add(manifest_name)
        plan = document(manifest_name)
        require(plan["scope"] == scope and set(plan["phases"]) == PHASES and
                index[scope]["manifest_sha256"] == mapping[manifest_name]["original_sha256"] and
                index[scope]["source_head"] == plan["source_head"] and index[scope]["candidate_tree"] == plan["candidate_tree"] and
                plan["wrapper_sha256"] == mapping["qualify_scope.py"]["original_sha256"], "Scope source binding differs")
        original_hashes = {}
        for item in plan["files"]:
            path = safe_name(item["path"])
            key = scope + ":" + path
            require(pins[key]["original_git_blob"] == item["original_git_blob"], "Original Git blob claim differs")
            original_hashes[path] = pins[key]["original_sha256"]
            candidate = "scopes/" + scope + "/candidate/" + path
            expected_payload.add(candidate)
            candidates.add(candidate)
            require(candidate.endswith(".go") and mapping[candidate]["original_sha256"] == item["candidate_sha256"] and
                    mapping[candidate]["original_bytes"] == item["bytes"], "Candidate identity differs")
            if not mapping[candidate]["changed"]:
                raw = data[candidate]
                git_blob = hashlib.sha1(b"blob " + str(len(raw)).encode("ascii") + b"\0" + raw).hexdigest()
                require(git_blob == item["candidate_git_blob"], "Candidate Git blob differs")
        for phase in sorted(PHASES):
            key = scope + "-" + phase
            folder = "evidence/" + key
            report = reports[key]
            require(report.get("state") == "PASS" and report.get("recorded_state") == "complete" and report.get("recorded_completed") is True, "Phase summary is not PASS: " + key)
            require(all(report.get(field) == plan[field] for field in ("source_head", "source_tree", "candidate_tree")), "Phase summary source differs")
            receipt = document(folder + "/receipt.json")
            require(receipt.get("state") == "complete" and receipt.get("completed") is True and not receipt.get("process_tree_unresolved"), "Final phase receipt is not complete: " + key)
            require(receipt.get("scope") == scope and receipt.get("phase") == key and
                    receipt.get("scope_manifest_sha256") == mapping[manifest_name]["original_sha256"] and
                    receipt.get("base") == plan["source_head"] and receipt.get("base_tree") == plan["source_tree"] and
                    receipt.get("source_head") == plan["source_head"] and receipt.get("source_tree") == plan["source_tree"] and
                    receipt.get("scope_wrapper_sha256") == plan["wrapper_sha256"], "Final receipt scope differs")
            for field in ("source_before", "source_after", "source_final"):
                observation = receipt[field]
                require(observation.get("head") == plan["source_head"] and observation.get("tree") == plan["source_tree"] and
                        observation.get("sha256") == original_hashes and observation.get("status") == "" and observation.get("staged") == "", "Source observation differs: " + key)
            source = PureWindowsPath(receipt["source_directory"])
            require(source.is_absolute() and source.name == plan["source_directory"], "Source directory identity differs")
            overlay_name = folder + "/overlay.json"
            overlay = document(overlay_name)
            replacement = {str(source / path): str(source.parent / "scopes" / scope / "candidate" / path)
                           for path in plan["phases"][phase]["overlay_paths"]}
            require(overlay == {"Replace": replacement} and receipt["overlay_sha256"] == mapping[overlay_name]["original_sha256"], "Overlay identity differs")
            planned = keyed(plan["phases"][phase]["commands"], "label", "planned commands")
            recorded = keyed(receipt["commands"], "label", "command receipts")
            reported = keyed(report["commands"], "label", "summary commands")
            expected_labels = set(planned) | ({"init", "fetch-exact", "checkout-exact", "gofmt-check"} if phase == "prepare" else set())
            require(set(recorded) == set(reported) == expected_labels, "Command membership differs: " + key)
            for label, row in recorded.items():
                stdout = stream_row(row, folder)
                require(reported[label].get("state") == "PASS", "Command summary not PASS")
                if label not in planned:
                    if label == "gofmt-check":
                        require(stdout == b"", "Gofmt reported changes")
                    continue
                command = planned[label]
                overlay_path = str(source.parent / PureWindowsPath(folder) / "overlay.json")
                args = [overlay_path if arg == "{overlay}" else arg for arg in command["args"]]
                require(row["args"] == args and row["cwd"] == str(source) and row.get("scope_oracle_matched") is True and command["expected_exit_code"] == 0, "Planned command differs: " + key + "/" + label)
                if command["args"][0] == "test":
                    counts = count_events(stdout, row, command, key + "/" + label)
                    require(all(reported[label].get(k) == v for k, v in counts.items()), "Per-command summary counts differ")
                    totals.update(counts)
                    commands_verified.append({"scope": scope, "label": label, **counts})
                else:
                    require(command["args"][0] in ("build", "vet"), "Unexpected Go command kind")
                    build_vet_count += 1
            for row in keyed(receipt["observations"], "label", "observations").values():
                stream_row(row, folder)

    require(len(candidates) == 13 and len(commands_verified) == 11 and build_vet_count == 10 and totals["root_pass_events"] == 57, "Reconstructed scope counts differ")
    require(dict(totals) == summary["observed_totals"], "Reconstructed totals differ from summary")
    payload = document("payload-readback.json")
    payload_files = keyed(payload["files"], "path", "payload readback")
    require(set(payload_files) == expected_payload and len(payload_files) == 20, "Payload membership differs")
    for name, row in payload_files.items():
        require(row["sha256"] == mapping[name]["original_sha256"] and row["bytes"] == mapping[name]["original_bytes"], "Payload hash claim differs")
    return {"schema": "r24-public-archive-verification-v1", "archive_integrity": "PASS",
            "internal_evidence_consistency": "PASS", "runtime_authentication": "NOT_ESTABLISHED",
            "archive_bytes": len(archive_bytes), "archive_sha256": digest,
            "external_archive_pin_matched": expected_sha is not None,
            "zip_members": len(data), "verified_input_files": len(mapping), "changed_public_files": changed,
            "verified_phases": len(reports), "verified_candidate_go_files": len(candidates),
            "verified_test_commands": len(commands_verified), "verified_build_vet_commands": build_vet_count,
            "reconstructed_totals": dict(totals), "commands": commands_verified,
            "limits": ["ZIP integrity and evidence consistency do not authenticate execution, machine identity or publication provenance.",
                       "Without an externally trusted archive digest the manifest and its files can be replaced together.",
                       "For changed public files, original hashes and redaction correctness remain claims; only public bytes are directly verified.",
                       "This does not run tests, read installed tools, repeat native operations, or qualify full test gates and process-tree settlement.",
                       "CAPTURED_FOR_ARCHIVE is the embedded snapshot state; the separate packaging completion marker is not inside this ZIP."]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("zip", type=Path)
    parser.add_argument("--expected-sha256", help="Optional independently obtained lowercase archive digest")
    args = parser.parse_args()
    try:
        result = verify(args.zip, args.expected_sha256)
    except (OSError, ValueError, KeyError, TypeError, AttributeError, zipfile.BadZipFile, RuntimeError) as error:
        print(json.dumps({"archive_integrity": "FAIL", "error_type": type(error).__name__, "reason": str(error)}, ensure_ascii=False, indent=2))
        return 1
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
