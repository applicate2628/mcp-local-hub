#!/usr/bin/env python3
"""Validate and describe the frozen R25 CMake pair; never execute repository code."""

import argparse
import collections
import hashlib
import json
import math
import re
import statistics
from pathlib import Path, PureWindowsPath


ROOT = "TestCMakeConsumerRetentionLifecycle"
PACKAGE = "mcp-local-hub/internal/vcpkgmcp/cmaketrace"
CASES = {"selected-32": (32, 32), "selected-1000": (1000, 0), "last-marker": (1, 1)}
PHASES = ("before", "after_work", "results_and_wire", "wire_only", "released")
MEMORY = ("heap_alloc_bytes", "heap_objects", "heap_inuse_bytes", "heap_released_bytes",
          "stack_inuse_bytes", "runtime_sys_bytes", "total_alloc_bytes", "mallocs", "gc_cycles")
ROW_KEYS = {"case", "clients", "sample", "go_version", "gomaxprocs", "trace_sha256",
            "trace_bytes", "wire_sha256", "wire_bytes_each", "wire_records_each",
            "internal_records_each", "record_capacity_each", "work_ns_instrumented",
            "worker_intervals_ns", *PHASES}
SEMANTIC = ("trace_sha256", "trace_bytes", "wire_sha256", "wire_bytes_each",
            "wire_records_each", "internal_records_each", "record_capacity_each")
MARKER = "R25_CMAKE_RETENTION "
HELPER = "internal/vcpkgmcp/cmaketrace/cmake_retention_diagnostic_test.go"
PRODUCTION = "internal/vcpkgmcp/cmaketrace/cmaketrace.go"
HEAD = "81dcf67db013ba2879878c2ee6fe07aa0bb4aec3"
TREE = "50585004a112d5ca4c8c35b105d129fac5d88c4c"
MANIFEST_SHA = "6c7340d211be812e70f88e278ac062c95dbfd1a98bfc0a9c54021d228de070e1"
WRAPPER_SHA = "610269f4e6841d336c4339abafe94751db8386d49c3fa4a2ce3bb103a794984e"
HELPER_SHA = "c73994c1e23f1ebdc00f56abae36a1f987ac88e6f8f711e753c889940ff4c13f"
CURRENT_SHA = "53a704edffffdd7ee7c8de95584f20f46ac7e6cad405446c05b56ed7c5c396b0"
CONTROL_SHA = "d6418595218992d1b285705829f4375b54068bdafcdc643d56ec054a28aef9b0"
ORACLE_SHA = "75cfff1d98fbc3ebd1e7d2acafc37d56f6999d0b8112de68ceefe80e634fb381"
TRACE_SHA = "755212e57a9318e4945f480d4152e3352053002606f97d20dc3c033055b1a7e8"
TRACE_BYTES = 11827770
STDOUT_PINS = {
    "before-clear": (47952, "0b588dc82813457059169c560ae9972210420bea788b8274540e028d2199e227"),
    "current": (47860, "bf0b212592753716be0a4956cd3433287fea9d6c939c4a2103082ada503a3bda"),
}
OWNED_ENV = {"APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "USERPROFILE", "GOPATH", "TMPDIR",
             "GOTMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
             "PSModuleAnalysisCachePath", "GIT_CONFIG_GLOBAL", "HOME"}
TEST_NAMES = [ROOT] + [f"{ROOT}/{case}/clients-{clients}" for case in CASES for clients in (1, 4)]
ROW_ORDER = [(case, clients, sample) for case in CASES for clients in (1, 4) for sample in (1, 2, 3)]


class InvalidEvidence(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise InvalidEvidence(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique_object,
                      parse_constant=lambda value: (_ for _ in ()).throw(InvalidEvidence(f"invalid number {value}")))


def read_bytes(path):
    require(path.stat().st_size <= 64 << 20, f"input too large: {path.name}")
    return path.read_bytes()


def integer(value, name, minimum=0, maximum=(1 << 64) - 1):
    require(type(value) is int and minimum <= value <= maximum, f"invalid integer {name}: {value!r}")


def digest(value, name):
    require(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value), f"invalid {name}")


def validate_row(row, test):
    require(isinstance(row, dict) and set(row) == ROW_KEYS, "unexpected observation schema")
    require(isinstance(row["case"], str) and row["case"] in CASES, "unexpected case")
    integer(row["clients"], "clients", 1, 4)
    require(row["clients"] in (1, 4), "unexpected clients")
    integer(row["sample"], "sample", 1, 3)
    require(test == f"{ROOT}/{row['case']}/clients-{row['clients']}", "row/Test attribution mismatch")
    require(isinstance(row["go_version"], str) and bool(row["go_version"]), "missing Go version")
    integer(row["gomaxprocs"], "gomaxprocs", 1)
    require(row["trace_sha256"] == TRACE_SHA and type(row["trace_bytes"]) is int
            and row["trace_bytes"] == TRACE_BYTES, "original trace pin mismatch")
    digest(row["wire_sha256"], "wire digest")
    integer(row["wire_bytes_each"], "wire bytes", 1, 262144)
    for name in ("wire_records_each", "internal_records_each", "record_capacity_each"):
        integer(row[name], name)
    internal, wire = CASES[row["case"]]
    require(row["internal_records_each"] == internal and row["wire_records_each"] == wire,
            "semantic record count differs from pinned case")
    require(row["record_capacity_each"] >= internal, "capacity smaller than record length")
    integer(row["work_ns_instrumented"], "work interval", 1, (1 << 63) - 1)
    intervals = row["worker_intervals_ns"]
    require(isinstance(intervals, list) and len(intervals) == row["clients"], "worker interval count mismatch")
    for interval in intervals:
        require(isinstance(interval, list) and len(interval) == 2, "invalid worker interval shape")
        integer(interval[0], "worker start", 0, (1 << 63) - 1)
        integer(interval[1], "worker end", 0, (1 << 63) - 1)
        require(interval[0] <= interval[1] <= row["work_ns_instrumented"], "worker interval outside joined work")
    for phase in PHASES:
        memory = row[phase]
        require(isinstance(memory, dict) and set(memory) == set(MEMORY), "unexpected memory schema")
        for key in MEMORY:
            integer(memory[key], f"{phase}.{key}", maximum=(1 << 32) - 1 if key == "gc_cycles" else (1 << 64) - 1)
    require(row["before"]["gc_cycles"] >= 2, "initial baseline lacks two observed GC cycles")
    for left, right in zip(PHASES, PHASES[1:]):
        for key in ("total_alloc_bytes", "mallocs", "gc_cycles"):
            require(row[right][key] >= row[left][key], f"cumulative counter decreased: {left}/{right}/{key}")
        if right != "after_work":
            require(row[right]["gc_cycles"] - row[left]["gc_cycles"] >= 2,
                    f"two-cycle quiescent boundary missing before {right}")


def parse_events(raw):
    """Reassemble split test2json output by Test before parsing complete lines."""
    buffers = collections.defaultdict(str)
    runs = collections.Counter()
    terminals = collections.defaultdict(list)
    test_events = {}
    rows, package_terminals = [], []
    start_count, split_rows, event_count = 0, 0, 0
    for line_number, line in enumerate(raw.splitlines(), 1):
        event_count += 1
        event = decode(line)
        require(isinstance(event, dict), f"event {line_number} is not an object")
        action, test = event.get("Action"), event.get("Test", "")
        require(isinstance(action, str) and isinstance(test, str), "invalid event types")
        require(event.get("Package") == PACKAGE, "foreign package event")
        require(action in {"start", "run", "output", "pass"}, f"unexpected action {action}")
        require(not test or test in TEST_NAMES, f"unexpected Test {test}")
        require(not package_terminals, "event after package terminal")
        if action == "start":
            require(not test and event_count == 1, "package start must be the first unattributed event")
            start_count += 1
        elif action == "run":
            require(start_count == 1, "run before package start")
            require(bool(test), "test run without Test")
            runs[test] += 1
            require(runs[test] == 1 and not terminals[test], "duplicate or late test run")
            test_events.setdefault(test, {})["run"] = event.get("Time")
        elif action == "pass":
            elapsed = event.get("Elapsed")
            require(type(elapsed) in (int, float) and math.isfinite(elapsed) and elapsed >= 0,
                    "invalid Go elapsed duration")
            if test:
                require(runs[test] == 1 and not terminals[test], "duplicate or unstarted test pass")
                require(not buffers[test], "incomplete test output at terminal")
                terminals[test].append("pass")
                test_events[test].update(terminal=event.get("Time"), go_elapsed_seconds=event.get("Elapsed"))
            else:
                require(all(terminals[name] == ["pass"] for name in TEST_NAMES), "package pass before all test passes")
                package_terminals.append(event)
        else:
            require(start_count == 1, "output before package start")
            output = event.get("Output")
            require(isinstance(output, str), "invalid output field")
            if test:
                require(runs[test] == 1 and not terminals[test], "output outside test lifecycle")
            if buffers[test] and MARKER in buffers[test]:
                split_rows += 1
            buffers[test] += output
            require(len(buffers[test]) <= 65536, "unterminated diagnostic line exceeds bound")
            while "\n" in buffers[test]:
                text_line, buffers[test] = buffers[test].split("\n", 1)
                if "R25_CMAKE_RETENTION" not in text_line:
                    continue
                require(text_line.count(MARKER) == 1, "malformed/duplicate diagnostic marker")
                prefix, payload = text_line.split(MARKER, 1)
                require(re.fullmatch(r"\s+cmake_retention_diagnostic_test\.go:\d+: ", prefix), "foreign marker emitter")
                row = decode(payload)
                validate_row(row, test)
                rows.append(row)
    require(start_count == 1 and len(package_terminals) == 1, "expected exactly one package start/pass")
    require(all(not value for value in buffers.values()), "trailing incomplete output")
    require(set(runs) == set(TEST_NAMES) and all(runs[name] == 1 and terminals[name] == ["pass"] for name in TEST_NAMES),
            "expected exactly seven complete passing test lifecycles")
    keys = [(row["case"], row["clients"], row["sample"]) for row in rows]
    require(len(keys) == 18 and len(set(keys)) == 18 and keys == ROW_ORDER,
            "expected 18 unique rows in the source-defined case/client/sample order")
    for previous, current in zip(rows, rows[1:]):
        for key in ("total_alloc_bytes", "mallocs", "gc_cycles"):
            require(current["before"][key] >= previous["released"][key], f"counter decreased between samples: {key}")
        require(current["before"]["gc_cycles"] - previous["released"]["gc_cycles"] >= 2,
                "next baseline lacks two additional collections")
    return {"rows": rows, "event_lines": event_count, "output_continuations_reassembled": split_rows,
            "passing_tests": 7, "passing_packages": 1, "test_events": test_events,
            "package_terminal": package_terminals[0]}


def same_semantics(variants):
    by_case, runtime = {}, set()
    for variant in variants.values():
        for row in variant["rows"]:
            value = {key: row[key] for key in SEMANTIC}
            require(row["case"] not in by_case or by_case[row["case"]] == value,
                    f"wire/semantic mismatch across samples or variants: {row['case']}")
            by_case[row["case"]] = value
            runtime.add((row["go_version"], row["gomaxprocs"]))
    require(len(runtime) == 1, "Go runtime identity differs across rows or variants")
    return by_case, next(iter(runtime))


def describe(values):
    return {"n": len(values), "median": statistics.median(values), "minimum": min(values), "maximum": max(values)}


def derive(row):
    before = row["before"]
    metrics = {"work_ns_instrumented": row["work_ns_instrumented"],
               "work_total_alloc_delta_bytes": row["after_work"]["total_alloc_bytes"] - before["total_alloc_bytes"],
               "work_mallocs_delta": row["after_work"]["mallocs"] - before["mallocs"]}
    for phase in PHASES[1:]:
        metrics[phase + "_heap_alloc_delta_bytes"] = row[phase]["heap_alloc_bytes"] - before["heap_alloc_bytes"]
    metrics["held_to_wire_heap_difference_bytes"] = row["results_and_wire"]["heap_alloc_bytes"] - row["wire_only"]["heap_alloc_bytes"]
    metrics["wire_to_released_heap_difference_bytes"] = row["wire_only"]["heap_alloc_bytes"] - row["released"]["heap_alloc_bytes"]
    for left, right in zip(PHASES, PHASES[1:]):
        metrics[left + "_to_" + right + "_gc_delta"] = row[right]["gc_cycles"] - row[left]["gc_cycles"]
    intervals = row["worker_intervals_ns"]
    points = sorted((point, change) for start, end in intervals if end > start
                    for point, change in ((start, 1), (end, -1)))
    active, peak = 0, 0
    for _, change in points:
        active += change
        peak = max(peak, active)
    metrics["max_observed_worker_overlap"] = peak
    metrics["all_workers_common_overlap_ns"] = max(0, min(v[1] for v in intervals) - max(v[0] for v in intervals))
    return metrics


def normalized_environment(receipt, phase):
    environment = receipt["environment"]
    require(isinstance(environment, dict) and all(isinstance(k, str) and isinstance(v, str) for k, v in environment.items()),
            "invalid environment")
    own_root = PureWindowsPath(receipt["source_directory"]).parent / "runtime" / ("cmake-retention-" + phase)
    normalized = dict(environment)
    for key in OWNED_ENV:
        require(key in environment, f"missing scoped environment {key}")
        path = PureWindowsPath(environment[key])
        require(path.is_absolute() and ".." not in path.parts, f"invalid owned path {key}")
        try:
            relative = path.relative_to(own_root)
        except ValueError as exc:
            raise InvalidEvidence(f"environment {key} escapes its phase root") from exc
        normalized[key] = "<OWNED_PHASE_ROOT>/" + relative.as_posix()
    for key, value in {"GOOS": "windows", "GOARCH": "amd64", "GOFLAGS": "-mod=readonly",
                       "GOPROXY": "off", "GOSUMDB": "off", "GOVCS": "*:off", "GOWORK": "off",
                       "GOTOOLCHAIN": "local", "GOENV": "off"}.items():
        require(environment.get(key) == value, f"environment {key} differs from reviewed run")
    return normalized


def validate_receipt(receipt, phase, raw, manifest):
    require(receipt.get("phase") == "cmake-retention-" + phase and receipt.get("scope") == "cmake-retention",
            "receipt phase mismatch")
    require(receipt.get("completed") is True and receipt.get("state") == "complete", "incomplete receipt")
    require(receipt.get("base") == HEAD and receipt.get("base_tree") == TREE, "receipt base mismatch")
    require(receipt.get("scope_manifest_sha256") == MANIFEST_SHA and receipt.get("scope_wrapper_sha256") == WRAPPER_SHA,
            "receipt scope/wrapper pin mismatch")
    expected_source = {HELPER: None, PRODUCTION: CURRENT_SHA}
    for name in ("source_before", "source_after", "source_final"):
        snapshot = receipt[name]
        require(snapshot.get("head") == HEAD and snapshot.get("tree") == TREE
                and snapshot.get("staged") == "" and snapshot.get("status") == ""
                and snapshot.get("sha256") == expected_source, f"source snapshot mismatch: {name}")
    require(len(receipt["commands"]) == 1, "unexpected command count")
    command = receipt["commands"][0]
    require(command.get("label") == "consumer-retention" and type(command.get("exit_code")) is int
            and command.get("exit_code") == 0
            and command.get("settled") is True and command.get("timed_out") is False
            and command.get("scope_oracle_matched") is True, "command did not settle with expected result")
    require(type(command.get("seconds")) in (int, float) and math.isfinite(command["seconds"])
            and command["seconds"] >= 0, "invalid external command duration")
    require(not any(key in command for key in ("error_type", "event_error_type", "source_after_error",
                                               "stdout_capture_error", "stderr_capture_error")), "command capture error")
    require(command["stdout"]["bytes"] == len(raw) and command["stdout"]["sha256"] == sha(raw), "stdout/receipt mismatch")
    require(command["stderr"]["bytes"] == 0 and command["stderr"]["sha256"] == sha(b""), "unexpected stderr")
    arguments = command["args"][:]
    require(len(arguments) > 2 and arguments[1] == "-overlay", "missing overlay argument")
    path = PureWindowsPath(arguments[2])
    expected_path = PureWindowsPath(receipt["source_directory"]).parent / "evidence" / ("cmake-retention-" + phase) / "overlay.json"
    require(path == expected_path, "overlay path differs from phase")
    arguments[2] = "{overlay}"
    expected_command = manifest["phases"][phase]["commands"][0]
    require(arguments == expected_command["args"], "command selection differs from manifest")
    expected_pass = TEST_NAMES + ["PACKAGE:" + PACKAGE]
    require(set(command["tests"]) == {"pass", "fail", "skip"}
            and collections.Counter(command["tests"]["pass"]) == collections.Counter(expected_pass)
            and command["tests"]["fail"] == [] and command["tests"]["skip"] == [], "receipt terminal oracle mismatch")
    digest(receipt["overlay_sha256"], "recorded overlay digest")
    return command, normalized_environment(receipt, phase)


def reduce_pair(runtime_dir):
    manifest_path = runtime_dir / "payload/scopes/cmake-retention/manifest.json"
    manifest_raw = read_bytes(manifest_path)
    require(sha(manifest_raw) == MANIFEST_SHA, "scope manifest differs from reviewed pin")
    manifest = decode(manifest_raw)
    require(manifest["source_head"] == HEAD and manifest["source_tree"] == TREE, "manifest source identity")
    require(sha(read_bytes(runtime_dir / "qualify_scope.py")) == WRAPPER_SHA, "wrapper differs from reviewed pin")
    require(manifest["external_input_pins"]["original_oracle_sha256"] == ORACLE_SHA
            and manifest["external_input_pins"]["original_oracle_bytes"] == 891803
            and manifest["external_input_pins"]["original_trace_sha256"] == TRACE_SHA
            and manifest["external_input_pins"]["original_trace_bytes"] == TRACE_BYTES, "external input pins differ")
    for file in manifest["files"]:
        raw = read_bytes(manifest_path.parent / "candidate" / file["path"])
        expected = {HELPER: HELPER_SHA, PRODUCTION: CONTROL_SHA}[file["path"]]
        require(sha(raw) == expected == file["candidate_sha256"] and len(raw) == file["bytes"], "candidate digest/size mismatch")
        blob = hashlib.sha1(b"blob " + str(len(raw)).encode("ascii") + b"\0" + raw).hexdigest()
        require(blob == file["candidate_git_blob"], "candidate Git blob mismatch")
    require(manifest["phases"]["before-clear"]["overlay_paths"] == [HELPER, PRODUCTION]
            and manifest["phases"]["current"]["overlay_paths"] == [HELPER], "variant overlay distinction missing")
    variants, receipts, environments, integrity = {}, {}, {}, {}
    for phase, (size, expected_sha) in STDOUT_PINS.items():
        raw = read_bytes(runtime_dir / ("cmake-" + phase + ".stdout.jsonl"))
        require(len(raw) == size and sha(raw) == expected_sha, f"{phase}: original stdout pin mismatch")
        receipt_raw = read_bytes(runtime_dir / ("cmake-" + phase + "-receipt-readback.json"))
        receipt = decode(receipt_raw)
        command, environment = validate_receipt(receipt, phase, raw, manifest)
        variant = parse_events(raw)
        variant["external_command_seconds"] = command["seconds"]
        variant["recorded_overlay_sha256"] = receipt["overlay_sha256"]
        variant["settlement"] = {key: receipt.get(key) for key in ("direct_child_wait_bounded",
            "whole_process_tree_deadline_enforced", "process_tree_settlement_observed")}
        variant["settlement"]["direct_child_settled"] = command["settled"]
        variant["derived_rows"] = [{"case": row["case"], "clients": row["clients"], "sample": row["sample"],
                                     **derive(row)} for row in variant["rows"]]
        variants[phase], receipts[phase], environments[phase] = variant, receipt, environment
        integrity[phase] = {"stdout_bytes": len(raw), "stdout_sha256": sha(raw),
                            "local_reserialized_receipt_sha256": sha(receipt_raw),
                            "receipt_hash_scope": "local parsed JSON serialization; original receipt bytes retained by runner"}
    semantics, runtime = same_semantics(variants)
    require(environments["before-clear"] == environments["current"], "environments differ beyond explicit phase-owned paths")
    require(receipts["before-clear"]["tools_sha256"] == receipts["current"]["tools_sha256"], "toolchain/tool hashes differ")
    require(str(runtime[1]) == environments["current"]["GOMAXPROCS"], "row/runtime GOMAXPROCS mismatch")
    comparisons = []
    for case in CASES:
        for clients in (1, 4):
            comparison = {"case": case, "clients": clients, "samples_per_variant": 3}
            for phase, variant in variants.items():
                group = [row for row in variant["derived_rows"] if row["case"] == case and row["clients"] == clients]
                comparison[phase] = {key: describe([row[key] for row in group]) for key in group[0]
                                     if key not in {"case", "clients", "sample"}}
            comparison["held_heap_delta_difference_of_medians_bytes"] = (
                comparison["before-clear"]["results_and_wire_heap_alloc_delta_bytes"]["median"]
                - comparison["current"]["results_and_wire_heap_alloc_delta_bytes"]["median"])
            comparisons.append(comparison)
    return {"schema": "r25-cmake-retention-reduction-v1", "validated": True, "problems": [],
            "source_head": HEAD, "source_tree": TREE, "helper_sha256": HELPER_SHA,
            "scope_manifest_sha256": MANIFEST_SHA, "wrapper_sha256": WRAPPER_SHA,
            "external_input_pins": manifest["external_input_pins"], "integrity": integrity,
            "matching_runtime": {"go_version": runtime[0], "gomaxprocs": runtime[1]},
            "normalized_equal_environment": environments["current"],
            "gc_settings_in_environment": {key: environments["current"].get(key, "<unset>") for key in ("GOGC", "GOMEMLIMIT", "GODEBUG")},
            "equal_tool_hashes": receipts["current"]["tools_sha256"],
            "semantics_equal_across_all_rows_by_case": semantics,
            "variants": variants, "cohort_descriptive_statistics": comparisons,
            "interpretation": [
                "Three descriptive samples per cohort; no population inference or performance threshold.",
                "Heap deltas are whole-process observations; not exact Result/body sizes, peak memory or RSS.",
                "Work includes Trace, encoding and worker orchestration; not ordinary production request latency.",
                "Allocation deltas use AfterWork-Before, excluding subsequent semantic validator allocations.",
                "GC gates require at least two cycles at each quiescent boundary; autonomous cycles may add more.",
                "Observed worker intervals establish overlap; client count alone does not.",
                "Complete semantic validation and final original-input recheck are supported by the pinned helper's passing root.",
                "Bodies are compared by their pinned-helper SHA-256 and length, not independently reread by this reducer.",
                "Receipts confirm direct-child completion; whole-process-tree settlement remains separately reported.",
            ]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        result = reduce_pair(args.runtime_dir)
    except (InvalidEvidence, ValueError, OSError, KeyError, TypeError, IndexError) as exc:
        result = {"schema": "r25-cmake-retention-reduction-v1", "validated": False,
                  "problems": [f"{type(exc).__name__}: {exc}"]}
    with args.output.open("x", encoding="utf-8") as stream:
        json.dump(result, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    print(json.dumps({"validated": result["validated"], "problems": result["problems"],
                      "rows": {key: len(value["rows"]) for key, value in result.get("variants", {}).items()}}))
    return 0 if result["validated"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
