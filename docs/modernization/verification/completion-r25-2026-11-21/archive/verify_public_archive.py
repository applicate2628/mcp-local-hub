"""Verify an R25 public ZIP in memory, without extraction or execution.

The externally supplied index pin binds the expected scopes and outcomes.
Integrity and consistency do not authenticate execution or a machine identity.
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

ROOT = PureWindowsPath(r"D:\dev\ChatGPT\mcp-r25-focused-20261121")
OWNER_SHA = "6f69fc636f8ec47b725f07238f7ea2180cee9fb8efbd8e8a7275f4665ea87e61"
GO = r"C:\msys64\ucrt64\bin\go.exe"
LIMIT = 512 * 1024 * 1024
ERROR_KEYS = {"error_type", "event_error_type", "source_after_error", "stdout_capture_error", "stderr_capture_error"}
NOT_RUN = {"NOT_RUN", "REJECTED_BEFORE_RUNTIME"}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def blob(raw):
    return hashlib.sha1(b"blob " + str(len(raw)).encode("ascii") + b"\0" + raw).hexdigest()


def safe_name(name):
    require(isinstance(name, str) and re.fullmatch(r"[A-Za-z0-9_./-]+", name), "Unsafe member name")
    path = PurePosixPath(name)
    reserved = {"CON", "PRN", "AUX", "NUL"} | {p + str(i) for p in ("COM", "LPT") for i in range(1, 10)}
    require(not path.is_absolute() and str(path) == name and all(
        part not in (".", "..") and not part.endswith(".") and part.split(".")[0].upper() not in reserved
        for part in path.parts), "Noncanonical or reserved member name")
    return name


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "Duplicate JSON key")
        result[key] = value
    return result


def decode(raw):
    def reject_constant(value):
        raise ValueError("Nonfinite JSON constant: " + value)
    return json.loads(raw.decode("utf-8"), object_pairs_hook=unique_object, parse_constant=reject_constant)


def keyed(rows, key):
    require(isinstance(rows, list), "Expected a list of records")
    result = {}
    for row in rows:
        require(isinstance(row, dict) and isinstance(row.get(key), str) and row[key] not in result, "Invalid or duplicate record")
        result[row[key]] = row
    return result


def test_counts(events):
    counts = Counter({kind + "_" + action + "_events": 0
                      for kind in ("root", "subtest", "package") for action in ("pass", "fail", "skip")})
    for event in events:
        if event.get("Action") in ("pass", "fail", "skip"):
            kind = "package" if "Test" not in event else ("subtest" if "/" in event["Test"] else "root")
            counts[kind + "_" + event["Action"] + "_events"] += 1
    counts["build_fail_events"] = sum(event.get("Action") == "build-fail" for event in events)
    return dict(counts)


def verify_evidence(data, mapping, index_sha256, public=True):
    """Pure byte validation shared by the packager; never imports archive code."""
    require(re.fullmatch(r"[0-9a-f]{64}", index_sha256 or ""), "An external index SHA-256 is required")
    require(set(data) == set(mapping), "Input membership differs from file map")
    for name, item in mapping.items():
        safe_name(name)
        require(type(item.get("changed")) is bool, "Invalid redaction flag: " + name)
        for side in ("original", "public"):
            require(type(item.get(side + "_bytes")) is int and item[side + "_bytes"] >= 0 and
                    re.fullmatch(r"[0-9a-f]{64}", item.get(side + "_sha256", "")), "Invalid digest record: " + name)
        side = "public" if public else "original"
        require(len(data[name]) == item[side + "_bytes"] and sha(data[name]) == item[side + "_sha256"], "Member digest differs: " + name)
        require(item["changed"] == (item["original_sha256"] != item["public_sha256"]), "Redaction identity differs: " + name)
        if not item["changed"]:
            require(item["original_bytes"] == item["public_bytes"], "Unchanged size differs: " + name)
        replacements = item.get("replacement_counts", {})
        require(set(replacements) == {"plain", "json_escaped"} and all(type(n) is int and n >= 0 for n in replacements.values()) and
                bool(sum(replacements.values())) == item["changed"], "Invalid redaction accounting: " + name)

    def document(name):
        require(name in data, "Missing member: " + name)
        value = decode(data[name])
        require(isinstance(value, dict), "Expected JSON object: " + name)
        return value

    def identity(name, digest=None, size=None, git_blob=None, unchanged=False):
        require(name in mapping, "Missing pinned member: " + name)
        item = mapping[name]
        require(not unchanged or not item["changed"], "Authority metadata was redacted: " + name)
        require(digest is None or item["original_sha256"] == digest, "Pinned digest differs: " + name)
        require(size is None or item["original_bytes"] == size, "Pinned size differs: " + name)
        if git_blob is not None and (not public or not item["changed"]):
            require(blob(data[name]) == git_blob, "Pinned Git blob differs: " + name)
        return item["original_sha256"]

    identity("archive-index.json", index_sha256, unchanged=True)
    index = document("archive-index.json")
    require(index.get("schema") == "r25-archive-index-v1" and index.get("root") == str(ROOT), "Index schema/root differs")
    extras = keyed(index.get("files"), "path")
    require({"qualify_scope.py", "package_evidence.py", "verify_public_archive.py"} <= set(extras), "Missing packaging/runner pins")
    for name, item in extras.items():
        safe_name(name)
        require(("/" not in name and name.endswith((".py", ".json", ".txt"))) or name.startswith("transfer-readbacks/"), "Extra input is outside allowlist")
        require(name != "archive-index.json", "Index cannot pin itself")
        identity(name, item["sha256"])
    identity("frozen-owner/check_recovery.py", OWNER_SHA)
    scopes = keyed(index.get("scopes"), "scope")
    require(scopes, "Empty scope index")
    expected = set(extras) | {"archive-index.json", "frozen-owner/check_recovery.py"}
    expected.update(name for name in data if name.startswith("format/"))
    phases, totals, candidate_count, test_commands, build_vet_commands = [], Counter(), 0, 0, 0
    expected_folders = set()
    for scope, entry in scopes.items():
        require(re.fullmatch(r"[a-z][a-z0-9-]{0,40}", scope), "Invalid scope")
        manifest_name = "scopes/" + scope + "/manifest.json"
        identity(manifest_name, entry["manifest_sha256"], unchanged=True)
        expected.add(manifest_name)
        plan = document(manifest_name)
        require(plan.get("schema") == "r25-reviewed-source-overlay-v1" and plan.get("scope") == scope and
                plan.get("wrapper_sha256") == extras["qualify_scope.py"]["sha256"] and
                re.fullmatch(r"source-pr[0-9]+", plan.get("source_directory", "")), "Plan binding differs: " + scope)
        require(set(entry["phases"]) == set(plan["phases"]), "Every planned phase must have an explicit outcome")
        source = ROOT / plan["source_directory"]
        originals = {}
        files = keyed(plan["files"], "path")
        for path, item in files.items():
            safe_name(path)
            require(path.endswith(".go"), "Only explicit Go preimages are admitted")
            candidate = "scopes/" + scope + "/candidate/" + path
            identity(candidate, item["candidate_sha256"], item["bytes"], item["candidate_git_blob"])
            expected.add(candidate)
            candidate_count += 1
            original = "originals/" + plan["source_directory"] + "/" + path
            if item["original_git_blob"] is None:
                require(original not in data, "New file unexpectedly has a preimage")
                originals[path] = None
            else:
                originals[path] = identity(original, git_blob=item["original_git_blob"])
                expected.add(original)
        for item in plan.get("source_evidence", []):
            path = safe_name(item["path"])
            require(path.endswith(".go"), "Source evidence is not a Go file")
            name = "originals/" + plan["source_directory"] + "/" + path
            identity(name, item["sha256"], git_blob=item["git_blob"])
            expected.add(name)

        def snapshot(value):
            require(isinstance(value, dict) and value.get("exists") is True and value.get("materialized") is True and
                    value.get("head") == plan["source_head"] and value.get("tree") == plan["source_tree"] and
                    value.get("status") == "" and value.get("staged") == "" and value.get("sha256") == originals,
                    "Source observation differs: " + scope)

        for phase, outcome in entry["phases"].items():
            status = outcome["status"]
            require(status in NOT_RUN | {"PASS", "COMPILE_FAILED_BEFORE_TESTS"}, "Unknown expected outcome")
            phase_plan = plan["phases"][phase]
            planned = keyed(phase_plan["commands"], "label")
            report = {"scope": scope, "phase": phase, "state": status, "commands": []}
            phases.append(report)
            if status in NOT_RUN:
                report["commands"] = [{"label": label, "state": "NOT_RUN"} for label in planned]
                continue
            key = scope + "-" + phase
            folder = "evidence/" + key
            expected_folders.add(key)
            receipt = document(folder + "/receipt.json")
            document(folder + "/receipt-initial.json")  # Retained legacy admission; not an R25 source oracle.
            phase_files = {folder + "/" + name for name in ("receipt.json", "receipt-initial.json", "overlay.json", "scope-environment.json")}
            failed = status == "COMPILE_FAILED_BEFORE_TESTS"
            require(receipt.get("state") == ("failed" if failed else "complete") and
                    receipt.get("completed") is (not failed) and not receipt.get("process_tree_unresolved") and
                    not receipt.get("source_final_error"), "Phase terminal state differs: " + key)
            require(receipt.get("scope") == scope and receipt.get("phase") == key and
                    receipt.get("base") == plan["source_head"] and receipt.get("base_tree") == plan["source_tree"] and
                    receipt.get("scope_manifest_sha256") == entry["manifest_sha256"] and
                    receipt.get("scope_wrapper_sha256") == plan["wrapper_sha256"] and
                    receipt.get("runner_sha256") == receipt.get("reused_capture_owner_sha256") == OWNER_SHA and
                    receipt.get("source_directory") == str(source), "Receipt binding differs: " + key)
            for field in ("source_before", "source_final") if failed else ("source_before", "source_after", "source_final"):
                snapshot(receipt.get(field))
            if not failed:
                require(not ERROR_KEYS.intersection(receipt) and receipt.get("source_head") == plan["source_head"] and
                        receipt.get("source_tree") == plan["source_tree"], "Completed receipt has an error")
            else:
                require(receipt.get("error_type") == "RuntimeError" and receipt.get("error") ==
                        "Execution differs from the expected result: " + outcome["command"], "Historical phase error differs")
            overlay_name = folder + "/overlay.json"
            overlay_paths = phase_plan["overlay_paths"]
            require(len(overlay_paths) == len(set(overlay_paths)) and set(overlay_paths) <= set(files), "Invalid overlay selection")
            replacements = {str(source / path): str(ROOT / "scopes" / scope / "candidate" / path) for path in overlay_paths}
            require(document(overlay_name) == {"Replace": replacements}, "Overlay paths differ")
            identity(overlay_name, receipt["overlay_sha256"], unchanged=True)
            environment = document(folder + "/scope-environment.json")
            require(environment.get("scope") == scope and environment.get("phase") == phase and
                    environment.get("wrapper_sha256") == plan["wrapper_sha256"] and
                    environment.get("environment") == receipt.get("environment"), "Environment capture differs")
            recorded = keyed(receipt["commands"], "label")
            prelude = (["init", "fetch-exact", "checkout-exact"] if phase == "prepare" else [])
            if phase in ("prepare", "admit") and files:
                prelude += ["gofmt-check"]
            labels = prelude + list(planned)
            if failed:
                require(outcome.get("command") in planned and outcome.get("required_output"), "Compile failure needs an explicit command and diagnostic")
                labels = labels[:labels.index(outcome["command"]) + 1]
            require(list(recorded) == labels, "Executed command sequence differs: " + key)

            def stream_row(row, exit_code=0):
                label = row["label"]
                require(re.fullmatch(r"[a-z][a-z0-9-]{0,63}", label), "Invalid capture label")
                name = folder + "/" + label
                phase_files.add(name + ".receipt.json")
                require(document(name + ".receipt.json") == {k: v for k, v in row.items() if k != "scope_oracle_matched"}, "Per-command receipt differs")
                require(row.get("settled") is True and row.get("timed_out") is False and row.get("exit_code") == exit_code and
                        not row.get("process_tree_unresolved") and not ERROR_KEYS.intersection(row) and
                        row.get("runner_sha256") == OWNER_SHA, "Unsettled or unexpected command result")
                for suffix in ("started", "launched") if "pid" in row else ("started",):
                    phase_files.add(name + "." + suffix + ".json")
                    lifecycle = document(name + "." + suffix + ".json")
                    keys = ("label", "executable", "args", "cwd", "started_utc", "runner_sha256")
                    require(all(lifecycle.get(k) == row.get(k) for k in keys) and
                            (suffix != "launched" or lifecycle.get("pid") == row.get("pid")), "Command lifecycle differs")
                for stream in ("stdout", "stderr"):
                    phase_files.add(name + "." + stream + ".log")
                    identity(name + "." + stream + ".log", row[stream]["sha256"], row[stream]["bytes"])
                return data[name + ".stdout.log"]

            for label, row in recorded.items():
                compile_failed = failed and label == outcome["command"]
                output = stream_row(row, 1 if compile_failed else 0)
                result = {"label": label, "state": "COMPILE_FAILED_BEFORE_TESTS" if compile_failed else "PASS"}
                report["commands"].append(result)
                if label not in planned:
                    require(label != "gofmt-check" or output == b"", "Gofmt reported changes")
                    continue
                command = planned[label]
                args = [str(ROOT / folder / "overlay.json") if x == "{overlay}" else x for x in command["args"]]
                require(row.get("executable") == GO and row.get("args") == args and row.get("cwd") == str(source) and
                        row.get("direct_timeout_seconds") == command["timeout_seconds"], "Planned command identity or direct timeout differs")
                snapshot(row.get("source_before"))
                snapshot(row.get("source_after"))
                require(command["expected_exit_code"] == 0 and (compile_failed or row.get("scope_oracle_matched") is True), "Unexpected planned result")
                result["kind"] = command["args"][0]
                if result["kind"] != "test":
                    require(result["kind"] in ("build", "vet") and not compile_failed, "Unsupported command kind")
                    build_vet_commands += 1
                    continue
                test_commands += 1
                events = [decode(line) for line in output.splitlines()]
                require(all(isinstance(e, dict) for e in events), "Non-object Go event")
                terminal = {a: [e.get("Test", "PACKAGE:" + e.get("Package", "")) for e in events if e.get("Action") == a]
                            for a in ("pass", "fail", "skip")}
                require(row.get("tests") == terminal, "Raw terminal events differ from capture")
                package = "mcp-local-hub/" + command["args"][-1].removeprefix("./")
                require(all(e.get("Package") == package for e in events if "Package" in e), "Unexpected test package")
                rendered = "".join(e.get("Output", "") for e in events)
                if compile_failed:
                    failures = [e for e in events if e.get("Action") == "build-fail"]
                    require(not any("Test" in e for e in events) and len(failures) == 1 and
                            terminal == {"pass": [], "fail": ["PACKAGE:" + package], "skip": []} and
                            any(e.get("Action") == "fail" and e.get("FailedBuild") == failures[0].get("ImportPath") and
                                isinstance(e.get("FailedBuild"), str) for e in events), "Failure is not compilation before tests")
                    require(all(text in rendered for text in outcome["required_output"]), "Expected compilation diagnostic absent")
                else:
                    require(not terminal["fail"] and not terminal["skip"] and terminal["pass"].count("PACKAGE:" + package) == 1, "Unexpected terminal event")
                    if "expected_tests" in command:
                        require(set(command["expected_tests"]) == set(terminal) and all(
                            Counter(terminal[a]) == Counter(command["expected_tests"][a]) for a in terminal), "Planned terminal events differ")
                    counts = Counter(terminal["pass"])
                    require(all(counts[name] == count for name, count in command.get("required_root_pass_counts", {}).items()), "Root count oracle differs")
                    require(all(text.encode() in output for text in command.get("required_output", [])), "Required diagnostic absent")
                    for name in command.get("required_benchmarks", []):
                        require(re.search(r"(?m)^" + re.escape(name) + r"(?:-\d+)?\s+1\s+[0-9]+(?:\.[0-9]+)?\s+ns/op\b", rendered), "Expected benchmark result absent")
                result.update(test_counts(events))
                totals.update(test_counts(events))
            for label in planned:
                if label not in recorded:
                    report["commands"].append({"label": label, "state": "NOT_RUN"})
            for label, row in keyed(receipt["observations"], "label").items():
                require(re.fullmatch(r"observation-[0-9]+", label) and label not in recorded, "Invalid observation identity")
                stream_row(row)
            require({name for name in data if name.startswith(folder + "/")} == phase_files, "Unaccounted or missing phase capture")
            expected.update(phase_files)
    actual_folders = {name.split("/")[1] for name in data if name.startswith("evidence/")}
    require(actual_folders == expected_folders, "Evidence phase membership differs")
    require(set(data) == expected, "Unexpected or missing archive input")
    return {"schema": "r25-evidence-summary-v1", "evidence_consistency": "PASS", "qualification_state": "BOUNDED_RESULTS_ONLY",
            "phases": phases, "scopes": len(scopes), "candidate_go_files": candidate_count,
            "test_command_attempts": test_commands, "build_vet_commands": build_vet_commands,
            "observed_totals": dict(totals), "historical_compile_failures": sum(p["state"] == "COMPILE_FAILED_BEFORE_TESTS" for p in phases),
            "limits": ["Integrity and consistency do not authenticate runtime execution or publication provenance.",
                       "Redacted originals are hash claims; public bytes alone cannot reconstruct or authenticate them.",
                       "Preparation PASS does not execute a rejected candidate; historical failures remain failures.",
                       "Captured source observations and explicit preimages do not requalify full test gates or process-tree settlement."]}


def verify_archive(path, index_sha256, expected_sha256=None):
    require(path.stat().st_size <= LIMIT, "Archive exceeds 512 MiB compressed bound")
    raw = path.read_bytes()
    require(expected_sha256 is None or sha(raw) == expected_sha256, "External ZIP SHA-256 differs")
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        infos = archive.infolist()
        names = [safe_name(info.filename) for info in infos]
        require(len(names) <= 50000 and len(names) == len(set(names)) == len({n.casefold() for n in names}), "Duplicate or excessive ZIP members")
        require(sum(i.file_size for i in infos) <= LIMIT and all(i.file_size <= LIMIT // 4 for i in infos), "ZIP expansion exceeds bounds")
        require(all(not i.is_dir() and i.filename == i.orig_filename and stat.S_IFMT(i.external_attr >> 16) in (0, stat.S_IFREG) and
                    not i.flag_bits & 1 and i.compress_type in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED) for i in infos), "Unsupported ZIP member")
        require(archive.testzip() is None, "ZIP CRC failure")
        data = {i.filename: archive.read(i) for i in infos}
    manifest = decode(data.pop("_package/manifest.json"))
    summary_raw = data.pop("_package/summary.json")
    require(manifest.get("schema") == "r25-raw-public-file-map-v1" and manifest.get("index_sha256") == index_sha256 and
            manifest.get("summary_sha256") == sha(summary_raw), "Generated metadata binding differs")
    mapping = keyed(manifest["files"], "path")
    summary = verify_evidence(data, mapping, index_sha256)
    require(decode(summary_raw) == summary, "Embedded summary differs from independent reconstruction")
    streams = [item for name, item in mapping.items() if name.endswith((".stdout.log", ".stderr.log"))]
    return {**summary, "schema": "r25-public-verification-v1", "archive_integrity": "PASS", "runtime_authentication": "NOT_ESTABLISHED",
            "archive_sha256": sha(raw), "archive_bytes": len(raw), "external_archive_pin_matched": expected_sha256 is not None,
            "external_index_pin_matched": True, "zip_members": len(infos),
            "redacted_files": sum(item["changed"] for item in mapping.values()),
            "raw_stream_digests_recomputed": sum(not item["changed"] for item in streams),
            "raw_stream_digests_remaining_claims": sum(item["changed"] for item in streams)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument("--index-sha256", required=True)
    parser.add_argument("--expected-sha256")
    args = parser.parse_args()
    try:
        print(json.dumps(verify_archive(args.archive, args.index_sha256, args.expected_sha256), indent=2))
        return 0
    except (OSError, ValueError, KeyError, TypeError, AttributeError, zipfile.BadZipFile, RuntimeError) as error:
        print(json.dumps({"archive_integrity": "FAIL", "error_type": type(error).__name__, "reason": str(error)}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
