"""Read-only replay of A1/A2 archive integrity, source composition and raw oracles.

Only the requested output JSON is written, exclusively. Git is used solely for
ls-tree, rev-parse and cat-file; no Go/project/remote command is executed.
"""
import sys
sys.dont_write_bytecode = True
import argparse
from collections import Counter, defaultdict
import hashlib
import io
import json
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import subprocess
import zipfile

ARCHIVE_SHA = "1e95d5b473a2da7dbddf174d2ac4fd5bf8f0ee62415dc05c55c4e00fc8938cfb"
ARCHIVE_BYTES = 2208961
PREVIOUS_SHA = "858912769a69d45fd6fdd8e422ed169fd7bbc69c9a125e7fab8c2a0fe584bd29"
BASE_HEAD = "858812988833a84368c60fba1540ae601155fa71"
BASE_TREE = "dba685c090952a93a80a1e82403c4646ad8aaed1"
RUNNER_SHA = "a483536d689df8c5713fddbdfd221b85953cf0ce4f9d578a31c4cdbfc6b9a26e"
REMOTE = PureWindowsPath(r"D:\dev\ChatGPT\mcp-residual-next-20261122-dcb40fba")
GO = r"C:\msys64\ucrt64\bin\go.exe"
GOFMT = r"C:\msys64\ucrt64\bin\gofmt.exe"
TOOL_PINS = {
    GO: "0176dbac02ff27ef54c6bc5b316c7c2a2a89f7c8cf5fd314a0e03d6d2b924113",
    GOFMT: "95368e4aafefa20f962a345ab02f53ffc035ed69349612abbf385b52f4fb25c5",
    r"C:\msys64\ucrt64\bin\gcc.exe": "f6ff9378c1edb591131304518eac6456708d28720284105930d0d687136bcdd3",
}
API_PACKAGE = "mcp-local-hub/internal/api"
GUI_PACKAGE = "mcp-local-hub/internal/gui"
TX = "TestWeeklyRefreshTransaction_AppliedReleaseDisposition"
APPLY = "TestApplyWeeklyRefreshSchedule_AppliedReleaseDisposition"
GUI = "TestWeeklyScheduleHandler_RealApplyReleaseSettlement"
INVENTORY = "TestSupervisorIntentLockOwnershipProductionInventory"
FIXTURE = "TestSupervisorIntentLockOwnershipFixtureMatrix"
SIBLING = "TestAppliedReleaseClassifierFixtureRejectsSibling"
WEEKLY = "TestWeeklyRefreshReleaseSettlementMatrix"
SETTINGS = "TestWeeklyScheduleSettingsReleaseSettlementMatrix"
JOIN = "TestReleaseAndJoinAppliedSettlementMatrix"
LOCK_ORDER = "TestWeeklyRefreshLockOrderInventory"


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def summary(rows):
    values = [[row["path"], row["bytes"], row["sha256"]] for row in rows]
    return {"files": len(rows), "bytes": sum(row[1] for row in values),
            "inventory_sha256": sha(json.dumps(values, separators=(",", ":"), ensure_ascii=True).encode())}


def row(path, raw):
    return {"path": path, "bytes": len(raw), "sha256": sha(raw)}


def inventory(contents):
    return [row(path, contents[path]) for path in sorted(contents)]


def unpack(raw):
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        names = archive.namelist()
        require(len(names) == len(set(names)), "Duplicate archive member")
        for item in archive.infolist():
            path = PurePosixPath(item.filename)
            require(not path.is_absolute() and path.as_posix() == item.filename and
                    not any(part in (".", "..") for part in path.parts) and "\\" not in item.filename,
                    "Unsafe archive member path")
            require(not item.is_dir(), "Unexpected archive directory")
        return {name: archive.read(name) for name in names}


def verify_rows(contents, rows, prefix=""):
    require(len(rows) == len({item["path"] for item in rows}), "Duplicate inventory row")
    for item in rows:
        raw = contents[prefix + item["path"]]
        require(row(item["path"], raw) == item, "Hash/size mismatch: " + prefix + item["path"])


def git_source(repository, expected_rows):
    def git(*args, data=None):
        return subprocess.run(["git", *args], cwd=repository, input=data, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, check=True).stdout
    require(git("rev-parse", BASE_HEAD + "^{tree}").decode().strip() == BASE_TREE, "Wrong immutable Git base tree")
    entries = []
    for record in git("ls-tree", "-r", "-z", "--full-tree", BASE_HEAD).split(b"\0"):
        if record:
            header, name = record.split(b"\t", 1)
            mode, kind, oid = header.decode().split()
            require(mode == "100644" and kind == "blob", "Unexpected base Git mode/type")
            entries.append((name.decode(), oid))
    response = io.BytesIO(git("cat-file", "--batch", data="".join(oid + "\n" for _, oid in entries).encode()))
    result = {}
    for path, oid in entries:
        found, kind, size = response.readline().decode().split()
        raw = response.read(int(size))
        require(found == oid and kind == "blob" and response.read(1) == b"\n", "Git batch framing mismatch")
        require(hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest() == oid, "Git blob identifier mismatch")
        result[path] = raw
    require(not response.read() and inventory(result) == expected_rows, "Independent full Git blobs differ from preserved base inventory")
    return result


def apply_replacements(base, plan, inputs):
    result = dict(base)
    require(len(plan["replacements"]) == len({r["path"] for r in plan["replacements"]}), "Duplicate replacement")
    for replacement in plan["replacements"]:
        result[replacement["path"]] = inputs["inputs/" + replacement["from"]]
    require(inventory(result) == plan["source_files"], "Reconstructed physical composition differs")
    return result


def function_text(source, name):
    start = source.index("func " + name + "(")
    next_test = re.search(r"^func Test\w+\(", source[start + 5:], re.M)
    return source[start:start + 5 + next_test.start()] if next_test else source[start:]


def table_names(source, root, count, extra=()):
    body = function_text(source.decode(), root)
    names = re.findall(r'^\s*name:\s*"([^"\n]+)"', body, re.M)
    names += list(extra)
    require(len(names) == count and len(set(names)) == count, "Reviewed fixture names changed: " + root)
    return [root] + [root + "/" + name.replace(" ", "_") for name in names]


def expected_names(files, physical):
    old_atomic = files["inputs/a1/internal/api/weekly_refresh_atomic_test.go"]
    origins = files["inputs/a1/internal/api/weekly_refresh_call_graph_origins_test.go"]
    a1 = table_names(old_atomic, "TestWeeklyTypedCallGraphResolution", 33)
    a1 += table_names(old_atomic, "TestWeeklyTypedExportXMLBoundary", 5)
    a1 += table_names(origins, "TestWeeklyTypedLocalLiteralOrigins", 9)
    a1 += table_names(origins, "TestWeeklyTypedContextOrigins", 21, ("context budget fails closed",))
    api = [TX, TX + "/acknowledged-create", TX + "/create-error-after-write", APPLY]
    api += [APPLY + "/" + outcome + "/" + release for outcome in
            ("committed", "create-failed", "create-failed-after-write", "prior-restored")
            for release in ("weekly", "settings", "both")]
    gui = [GUI] + [GUI + "/" + name for name in (
        "committed-weekly-release", "committed-settings-release", "committed-both-releases",
        "failed-create-weekly-release", "create-error-after-write-both-releases", "prior-restored-settings-release")]
    join = [JOIN] + [JOIN + "/" + name for name in
                    ("success-nil", "success-primary", "release-unapplied", "release-applied", "primary-and-release-applied")]
    for name in join[1:]:
        require(('"' + name.split("/", 1)[1] + '"').encode() in physical["internal/api/lock_release_ledger_test.go"], "Join case absent from compiled source")
    weekly = [WEEKLY]
    for name in ("direct owner requires acknowledged creation and readback before applied-release-unconfirmed",
                 "Apply distinguishes acknowledged creation from failed creation on release failure"):
        require(('t.Run("' + name + '"').encode() in old_atomic, "Weekly parent case absent from compiled source")
        parent = WEEKLY + "/" + name.replace(" ", "_")
        weekly += [parent] + [parent + "/create-error-current-matches=" + value for value in ("false", "true")]
    weekly += [WEEKLY + "/" + owner + "_preserves_release_failure_and_later_fail-fast" for owner in ("Ensure", "swap", "upgrade")]
    settings = [SETTINGS] + [SETTINGS + "/" + name.replace(" ", "_") for name in (
        "successful combined Apply reports settings release failure after the requested pair settles",
        "callback failure after rollback remains primary and joins settings release failure")]
    fixtures = table_names(files["inputs/a2-v3/internal/api/supervisor_intent_lock_ownership_test.go"], FIXTURE, 14)
    adjacent = [INVENTORY, SIBLING] + join + weekly + settings
    return {"a1": a1, "api": api, "gui": gui, "adjacent": adjacent, "census": adjacent + fixtures,
            "lock_order": [LOCK_ORDER]}


def command_expectation(label, names):
    kind = {"a1-old-analyzer": "lock_order", "a1-lock-order": "lock_order", "a1-origin-fixtures": "a1",
            "a2-api-old-owner": "api", "a2-api-result": "api", "a2-gui-old-owner": "gui", "a2-gui-result": "gui",
            "a2-gui-corrected-old-owner": "gui", "a2-gui-corrected-result": "gui", "a2-adjacent": "adjacent",
            "a2-owner-census-and-adjacent": "census", "a2-composition-owner-census": "census"}[label]
    package = GUI_PACKAGE if kind == "gui" else API_PACKAGE
    failed = set()
    if kind == "lock_order":
        failed.add(LOCK_ORDER)
    elif label == "a2-api-old-owner":
        failed.update([TX, TX + "/acknowledged-create", APPLY])
        failed.update(APPLY + "/committed/" + release for release in ("weekly", "settings", "both"))
    elif label in ("a2-gui-old-owner", "a2-gui-result", "a2-gui-corrected-old-owner"):
        failed.add(GUI)
        failed.update(name for name in names[kind] if name.startswith(GUI + "/committed-"))
    elif label in ("a2-adjacent", "a2-owner-census-and-adjacent"):
        failed.add(INVENTORY)
    all_names = set(names[kind]) | {"PACKAGE:" + package}
    if failed:
        failed.add("PACKAGE:" + package)
    return package, {"pass": sorted(all_names - failed), "fail": sorted(failed), "skip": []}


def parse_tests(raw, package, expected):
    terminals = {name: [] for name in ("pass", "fail", "skip")}
    runs = []
    outputs = defaultdict(str)
    actions = Counter()
    for line in raw.splitlines():
        event = json.loads(line)
        require(isinstance(event, dict) and event.get("Package") == package, "Unexpected raw test event/package")
        action = event["Action"]
        actions[action] += 1
        name = event.get("Test", "PACKAGE:" + package)
        if action in terminals:
            terminals[action].append(name)
        if action == "run":
            runs.append(name)
        if action == "output":
            outputs[name] += event.get("Output", "")
    for action in terminals:
        require(Counter(terminals[action]) == Counter(expected[action]), "Unexpected exact terminal set: " + action)
    named = [name for values in expected.values() for name in values if not name.startswith("PACKAGE:")]
    require(Counter(runs) == Counter(named), "Named tests did not each run exactly once")
    require(actions["start"] == 1, "Package start count differs")
    leaves = [name for name in named if not any(other.startswith(name + "/") for other in named)]
    require(b"WARNING: DATA RACE" not in raw and b"race detected during execution of test" not in raw, "Race diagnostic in qualification output")
    return {"terminals": terminals, "counts": {key: len(value) for key, value in terminals.items()},
            "named_tests": len(named), "leaf_cases": len(leaves), "event_actions": dict(actions),
            "exact_names_and_single_execution_verified": True,
            "failed_outputs": {name: outputs[name] for name in terminals["fail"]},
            "all_test_outputs_sha256": {name: sha(text.encode()) for name, text in sorted(outputs.items())}}, outputs


def check_failure_cause(label, result, outputs, raw):
    reasons = {
        "a1-old-analyzer": "intended_cleanup_origin_boundary_observed_but_plan_requires_absent_literal_cleanup",
        "a1-lock-order": "candidate_analysis_fails_closed_at_local_dynamic_Unwrap_boundary_in_errnoIs",
        "a2-api-old-owner": "valid_negative_control_committed_results_lack_applied_release_marker",
        "a2-gui-old-owner": "invalid_initial_XML_fixture_fails_before_applied_result_classification",
        "a2-gui-result": "same_invalid_initial_XML_fixture_blocks_candidate_commit",
        "a2-gui-corrected-old-owner": "valid_negative_control_verified_commit_projects_as_HTTP_500_updated_false",
        "a2-adjacent": "stale_owner_census_names_appendHubMcpLogLine_instead_of_actual_closure_owner",
        "a2-owner-census-and-adjacent": "publication_28_owner_census_is_incomplete_for_30_owner_qualification_composition",
    }
    text = raw.decode()
    if label == "a1-old-analyzer":
        require("secureCreateClientConfigIfMissingImpl" in text and "got 4 at" in text and "cleanup" not in text,
                "Old analyzer did not fail at the reviewed four-call origin boundary")
    if label == "a1-lock-order":
        require("unresolved typed boundary from errnoIs" in text and "secure_create_client_config_windows.go:315:10" in text and
                "local function or method has no indexed body" in text, "Candidate lock-order failure differs")
    if label == "a2-api-old-owner":
        for name in result["terminals"]["fail"]:
            if "/" in name and not name.startswith("PACKAGE:"):
                require("applied disposition = false, want true" in outputs[name], "API control failed before applied classification")
    if label in ("a2-gui-old-owner", "a2-gui-result", "a2-gui-corrected-old-owner"):
        for name in result["terminals"]["fail"]:
            if name.startswith(GUI + "/"):
                output = outputs[name]
                require('status = 500, want 200' in output and '"updated":false' in output, "GUI failure result differs")
                if label == "a2-gui-corrected-old-owner":
                    require('"restore_status":"n/a"' in output and "does not match the requested definition" not in output,
                            "Corrected control still fails on an invalid XML fixture")
                else:
                    require('"restore_status":"degraded"' in output and "does not match the requested definition" in output,
                            "Initial GUI failure cause differs")
    if label == "a2-adjacent":
        require("appendHubMcpLogLine calls=0, reviewed=1" in text, "Initial census failure differs")
    if label == "a2-owner-census-and-adjacent":
        require("AcquireSupervisorLockQuietObserved calls=1, reviewed=0" in text, "Publication census composition failure differs")
    return reasons.get(label, "all_selected_assertions_passed")


def overlay_details(files, plan, args, physical):
    if "-overlay" not in args:
        return {"overlay": None, "effective_inventory": summary(plan["source_files"]), "replacements": []}
    relative = PureWindowsPath(args[args.index("-overlay") + 1]).relative_to(REMOTE).as_posix()
    mapping = json.loads(files[relative])["Replace"]
    pinned = {"inputs/" + item["path"]: item for item in plan["input_files"]}
    require(relative in pinned, "Overlay map is not pinned")
    effective = dict(physical)
    replacements = []
    for target, replacement in mapping.items():
        source_path = PureWindowsPath(target).relative_to(REMOTE / "candidate-source").as_posix()
        input_path = PureWindowsPath(replacement).relative_to(REMOTE).as_posix()
        require(input_path in pinned and source_path in physical, "Overlay escapes its admitted source/input paths")
        effective[source_path] = files[input_path]
        replacements.append({"source_path": source_path, "input_path": input_path, **row(source_path, files[input_path])})
    return {"overlay": relative, "overlay_sha256": sha(files[relative]), "replacements": replacements,
            "effective_inventory": summary(inventory(effective))}


def audit_command(files, directory, command, phase_row=None, cwd=None):
    label = command["label"]
    receipt = json.loads(files[directory + label + ".receipt.json"])
    if phase_row is not None:
        require(receipt == phase_row, "Per-command and phase receipt differ")
    started = json.loads(files[directory + label + ".started.json"])
    launched = json.loads(files[directory + label + ".launched.json"])
    executable = GO if command["tool"] == "go" else GOFMT
    argv = [executable, *command["args"]]
    require(receipt["argv"] == started["argv"] == launched["argv"] == argv, "Literal argv differs")
    require(receipt["cwd"] == started["cwd"] == launched["cwd"] and (cwd is None or receipt["cwd"] == cwd), "Working directory differs")
    require(receipt["pid"] == launched["pid"] and receipt["pid"] > 0, "Original direct-child identity missing")
    require(receipt["settled"] is True and receipt["timed_out"] is False and not receipt.get("process_tree_unresolved") and
            not receipt.get("error_type"), "Command did not settle cleanly")
    require(receipt["direct_timeout_seconds"] == command["timeout_seconds"], "Declared direct deadline differs")
    streams = {}
    for stream in ("stdout", "stderr"):
        raw = files[directory + label + "." + stream + ".log"]
        require(receipt[stream] == {"bytes": len(raw), "sha256": sha(raw)}, "Raw stream/command receipt hash mismatch")
        streams[stream] = raw
    require(not streams["stderr"], "Unexpected nonempty command stderr")
    return receipt, streams


def raw_owner_map(raw):
    body = raw.decode().split("var reviewedRawFlockOwners = map[string]int{", 1)[1].split("\n}", 1)[0]
    return {name: int(count) for name, count in re.findall(r'"([^"]+)":\s*(\d+)', body)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", required=True, type=Path)
    parser.add_argument("--previous-archive", required=True, type=Path)
    parser.add_argument("--git-repo", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    raw = args.archive.read_bytes()
    require(len(raw) == ARCHIVE_BYTES and sha(raw) == ARCHIVE_SHA, "A1/A2 archive identity differs")
    files = unpack(raw)
    package = json.loads(files["PACKAGE_FILES.json"])
    require(len(files) == 178 and package["file_count"] == 177 and
            set(files) == {item["path"] for item in package["files"]} | {"PACKAGE_FILES.json"}, "Archive inventory is incomplete")
    verify_rows(files, package["files"])
    require(sha(files["qualify_go.py"]) == RUNNER_SHA, "Archived runner pin differs")
    previous_raw = args.previous_archive.read_bytes()
    require(len(previous_raw) == 1695231 and sha(previous_raw) == PREVIOUS_SHA, "Previous archive identity differs")
    previous = unpack(previous_raw)
    previous_manifest = json.loads(previous["manifest.json"])
    verify_rows(previous, previous_manifest["files"])
    require(files["previous-materialization.json"] == previous["plans/materialize-weekly.json"], "Predecessor materialization plan changed")
    previous_plan = json.loads(files["previous-materialization.json"])
    base = git_source(args.git_repo, previous_plan["base_files"])
    before = apply_replacements(base, previous_plan, previous)
    require(inventory(before) == json.loads(files["baseline-manifest.json"])["source_files"], "Restored previous composition differs")
    materialization = json.loads(files["plans/materialize-a1-a2.json"])
    require(materialization["base_files"] == inventory(before), "Current materialization base differs")
    verify_rows(files, materialization["input_files"], "inputs/")
    physical = apply_replacements(before, materialization, files)
    materialized_receipt = json.loads(files["materialize-a1-a2-receipt.json"])
    require(materialized_receipt["completed"] is True and materialized_receipt["project_execution"] is False and
            materialized_receipt["manifest_sha256"] == sha(files["plans/materialize-a1-a2.json"]), "Materialization receipt differs")
    require(materialized_receipt["base_before"] == materialized_receipt["base_after"] == summary(inventory(before)) and
            materialized_receipt["source"] == summary(inventory(physical)) and
            materialized_receipt["inputs_before"] == materialized_receipt["inputs_after"] == summary(materialization["input_files"]),
            "Materialization before/after inventory differs")
    preparation = json.loads(files["evidence/prepare/receipt.json"])
    require(preparation["completed"] is True and preparation["project_execution"] is False and
            preparation["archive_sha256"] == PREVIOUS_SHA and preparation["source"] == summary(inventory(before)), "Source preparation receipt differs")
    require(files["inputs/control/weekly_refresh.go"] == before["internal/api/weekly_refresh.go"] and
            files["inputs/control/weekly_refresh_atomic_test.go"] == before["internal/api/weekly_refresh_atomic_test.go"] and
            files["inputs/control/weekly_refresh_call_graph_origins_test.go"] == b"package api\n", "Control source provenance differs")

    names = expected_names(files, physical)
    plans = {}
    for path, data in files.items():
        if path.startswith("plans/") and path.endswith(".json"):
            plan = json.loads(data)
            if "commands" in plan:
                plans[plan["phase"]] = (path, data, plan)
    phases = []
    command_count = 0
    for phase in ("a1-origin-proof", "a1-candidate-proof", "a2-applied-result", "a2-route-fixture", "a2-owner-census", "a2-composition-census"):
        plan_path, plan_raw, plan = plans[phase]
        directory = "evidence/" + phase + "/"
        receipt = json.loads(files[directory + "receipt.json"])
        require(receipt["manifest_sha256"] == sha(plan_raw) and receipt["runner_sha256"] == RUNNER_SHA and
                plan["runner_sha256"] == RUNNER_SHA and receipt["tools_sha256"] == TOOL_PINS, "Phase executable/manifest pins differ")
        require(receipt["source_head"] == BASE_HEAD and receipt["source_tree"] == BASE_TREE and
                plan["source_files"] == inventory(physical), "Phase physical source differs")
        verify_rows(files, plan["input_files"], "inputs/")
        for kind, rows in (("source", plan["source_files"]), ("inputs", plan["input_files"])):
            require(receipt[kind + "_before"] == receipt[kind + "_after"] == summary(rows), "Phase before/after invariant failed")
        require(receipt["native_scheduler_operations"] is False, "Native task execution was admitted")
        for key, value in {"GOOS": "windows", "GOARCH": "amd64", "GOTOOLCHAIN": "local", "GOPROXY": "off",
                           "GOSUMDB": "off", "GOVCS": "*:off", "GOFLAGS": "-mod=readonly", "GOTELEMETRY": "off",
                           "MCPHUB_E2E_SCHEDULER": "none"}.items():
            require(receipt["environment"][key] == value, "Phase environment differs: " + key)
        commands = []
        for index, phase_row in enumerate(receipt["commands"]):
            command = plan["commands"][index]
            label = command["label"]
            result, streams = audit_command(files, directory, command, phase_row, str(REMOTE / plan["source_directory"]))
            output = streams["stdout"] + streams["stderr"]
            missing = [needle for needle in command.get("required_output", []) if needle.encode() not in output]
            oracle = result["exit_code"] == command["expected_exit_code"] and not missing
            record = {"label": label, "argv": result["argv"], "cwd": result["cwd"], "pid": result["pid"],
                      "started_utc": result["started_utc"], "finished_utc": result["finished_utc"],
                      "exit_code": result["exit_code"], "planned_exit_code": command["expected_exit_code"],
                      "stdout": result["stdout"], "stderr": result["stderr"], "raw_directory": directory,
                      "settled": True, "timed_out": False, "whole_process_tree_deadline_enforced": result["whole_process_tree_deadline_enforced"],
                      "missing_required_output": missing, "overlay": overlay_details(files, plan, command["args"], physical)}
            if command["args"][0] == "test":
                package_name, expected = command_expectation(label, names)
                tests, outputs = parse_tests(streams["stdout"], package_name, expected)
                require(tests["terminals"] == result["tests"], "Raw events differ from command receipt")
                record.update(tests=tests, interpretation=check_failure_cause(label, tests, outputs, streams["stdout"]))
                expected_roots = sorted(name for values in expected.values() for name in values if "/" not in name and not name.startswith("PACKAGE:"))
                run_arg = command["args"][command["args"].index("-run") + 1]
                require(run_arg.startswith("^(") and run_arg.endswith(")$") and sorted(run_arg[2:-2].split("|")) == expected_roots,
                        "Anchored root selection differs from exact raw oracles")
                require(all(flag in command["args"] for flag in ("-race", "-count=1", "-timeout=180s", "-tags=test_state_path_env", "-json")),
                        "Focused test flags differ")
                for action, required in command.get("required_tests", {}).items():
                    observed = Counter(tests["terminals"][action])
                    oracle = oracle and all(observed[name] >= count for name, count in Counter(required).items())
                if command["expected_exit_code"] == 0:
                    oracle = oracle and not tests["terminals"]["fail"]
            else:
                require(command["args"][0] in ("build", "vet") and not output and result["exit_code"] == 0, "Unexpected build/vet result")
                record["interpretation"] = "full_package_build_or_vet_passed_on_this_exact_overlay_composition_no_test_suite_execution"
            require(bool(oracle) == result["oracle_pass"], "Runner oracle does not recompute from raw evidence")
            record["runner_oracle_pass"] = bool(oracle)
            commands.append(record)
            command_count += 1
        expected_complete = len(commands) == len(plan["commands"]) and all(command["runner_oracle_pass"] for command in commands)
        require(receipt["completed"] == expected_complete, "Phase completion disagrees with actual command prefix")
        if not expected_complete:
            require(commands and not commands[-1]["runner_oracle_pass"] and all(x["runner_oracle_pass"] for x in commands[:-1]),
                    "Phase did not stop at its first failed oracle")
        phases.append({"phase": phase, "plan_path": plan_path, "plan_sha256": sha(plan_raw),
                       "started_utc": receipt["started_utc"], "finished_utc": receipt["finished_utc"],
                       "completed": expected_complete, "error": receipt.get("error"),
                       "physical_source": receipt["source_before"], "input_inventory": receipt["inputs_before"],
                       "before_after_equal": True, "commands": commands,
                       "not_run_after_failure": [x["label"] for x in plan["commands"][len(commands):]]})

    formatting = []
    for phase in ("format-a1", "format-a2", "format-a2-v2", "format-a2-v3", "format-a2-composition"):
        directory = "evidence/" + phase + "/"
        receipt = json.loads(files[directory + "receipt.json"])
        plan = json.loads(files["plans/" + phase + ".json"])
        require(receipt["before"] == plan["files"] and receipt["settled"] is True and receipt["timed_out"] is False and receipt["exit_code"] == 0,
                "Formatting write did not satisfy its exact plan")
        verify_rows(files, receipt["after"])
        require(receipt["argv"] == [GOFMT, "-w", *[str(REMOTE / item["path"]) for item in plan["files"]]], "Formatting argv differs")
        require(not files[directory + "stdout.log"] and not files[directory + "stderr.log"], "Formatting write emitted output")
        command = {"label": "gofmt-check", "tool": "gofmt", "args": ["-l", *receipt["argv"][2:]],
                   "timeout_seconds": 60, "expected_exit_code": 0}
        result, streams = audit_command(files, directory, command, cwd=str(REMOTE))
        require(result["exit_code"] == 0 and result["oracle_pass"] is True and not streams["stdout"], "Read-only format check failed")
        formatting.append({"phase": phase, "pid": receipt["pid"], "exit_code": 0, "settled": True,
                           "before": receipt["before"], "after": receipt["after"],
                           "write_stream_hash_source": "PACKAGE_FILES.json (write receipt has no per-stream hash fields)",
                           "gofmt_check_stream_hashes_verified": True})

    publication_map = raw_owner_map(files["inputs/a2-v3/internal/api/supervisor_intent_lock_ownership_test.go"])
    composition_map = raw_owner_map(files["inputs/a2-composition/internal/api/supervisor_intent_lock_ownership_test.go"])
    initial_map = raw_owner_map(files["inputs/a2/internal/api/supervisor_intent_lock_ownership_test.go"])
    extra = sorted(set(composition_map) - set(publication_map))
    require(len(initial_map) == 26 and len(publication_map) == 28 and len(composition_map) == 30 and extra == [
        "internal/api/supervisor_lock.go:func:AcquireSupervisorLockQuietObserved",
        "internal/api/supervisor_lock.go:func:ReadOccupiedSupervisorLockOwner/func-lit#1"], "Census scopes differ from reviewed source composition")
    for owner in ("AcquireSupervisorLockQuietObserved", "ReadOccupiedSupervisorLockOwner"):
        require(("func " + owner + "(").encode() in physical["internal/api/supervisor_lock.go"], "Qualification-only owner has no physical body")
    require(b"defer func() { _ = lock.Unlock() }()" in physical["internal/api/secrets_init_recovery.go"], "Separate secrets release defect source changed")
    publication_inputs = ["inputs/a1/internal/api/weekly_refresh_atomic_test.go", "inputs/a1/internal/api/weekly_refresh_call_graph_origins_test.go",
                          "inputs/a2/internal/api/weekly_refresh.go", "inputs/a2/internal/api/weekly_refresh_testhooks.go",
                          "inputs/a2/internal/api/weekly_refresh_applied_result_test.go", "inputs/a2-v2/internal/gui/weekly_schedule_applied_result_test.go",
                          "inputs/a2-v3/internal/api/supervisor_intent_lock_ownership_test.go"]
    result = {"schema": 1, "audit_kind": "independent_local_raw_evidence_and_full_Git_blob_replay",
              "audit_script_sha256": sha(Path(__file__).read_bytes()), "audit_project_execution": False, "audit_remote_execution": False,
              "archive": {"filename": "raw-evidence.zip", "bytes": len(raw), "sha256": sha(raw), "members": len(files),
                          "manifest_sha256": sha(files["PACKAGE_FILES.json"]), "all_177_payload_hashes_verified": True},
              "previous_archive": {"bytes": len(previous_raw), "sha256": sha(previous_raw), "members": len(previous),
                                   "materialization_plan_identical": True, "all_manifest_payload_hashes_verified": True},
              "source_provenance": {"base_commit": BASE_HEAD, "base_tree": BASE_TREE, "base": summary(inventory(base)),
                                    "base_full_git_blobs_rehashed_locally": True, "previous_replacements": previous_plan["replacements"],
                                    "previous_composition": summary(inventory(before)), "current_replacements": materialization["replacements"],
                                    "physical_composition": summary(inventory(physical)),
                                    "all_3309_source_contents_reconstructed_and_hashed_locally": True,
                                    "receipt_source_head_and_tree_identify_base_not_composite_commit": True},
              "runner_sha256": RUNNER_SHA, "tool_pins": TOOL_PINS, "phases": phases, "formatting": formatting,
              "qualification_command_count": command_count, "checksummed_command_stream_pairs": command_count + len(formatting),
              "all_test_skips": 0, "all_direct_commands_settled_without_timeout": True,
              "publication_candidate_inputs": [row(name, files[name]) for name in publication_inputs],
              "census_scope": {"initial_owners": len(initial_map), "publication_owners": len(publication_map),
                               "qualification_composition_owners": len(composition_map), "qualification_only_extra_owners": extra,
                               "publication_test_sha256": sha(files[publication_inputs[-1]]),
                               "qualification_only_test_sha256": sha(files["inputs/a2-composition/internal/api/supervisor_intent_lock_ownership_test.go"]),
                               "full_qualification_supervisor_lock_file": row("internal/api/supervisor_lock.go", physical["internal/api/supervisor_lock.go"])},
              "remaining_limits": {"a1_production_lock_order_gate": "FAILED: errnoIs dynamic local Unwrap boundary",
                                   "initial_GUI_control_is_not_valid_behavioral_evidence": True,
                                   "full_Go_test_suite": "NOT_RUN", "native_scheduler_and_installed_Product_qualification": "NOT_RUN",
                                   "whole_process_tree_settlement_independently_proved": False,
                                   "publication_head_or_external_review": "NOT_ESTABLISHED_BY_THIS_ARCHIVE",
                                   "separate_secrets_unlock_defect": {"path": "internal/api/secrets_init_recovery.go", "owner": "(*secretsInitRecoveryOwner).locked",
                                       "source_sha256": sha(physical["internal/api/secrets_init_recovery.go"]),
                                       "condition": "lock.Unlock returns an error after fn", "consequence": "deferred Unlock error is discarded; fn result is returned without that release failure",
                                       "fixed_or_behaviorally_qualified_here": False}}}
    with args.output.open("x", encoding="utf-8", newline="\n") as stream:
        json.dump(result, stream, indent=2, ensure_ascii=False)
        stream.write("\n")
    print(json.dumps({"verified": True, "archive_members": len(files), "physical_source": result["source_provenance"]["physical_composition"],
                      "commands": command_count, "stream_pairs": result["checksummed_command_stream_pairs"],
                      "skip_events": 0, "output_bytes": args.output.stat().st_size, "output_sha256": sha(args.output.read_bytes())}, indent=2))


if __name__ == "__main__":
    main()
