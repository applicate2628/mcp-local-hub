"""Inspect only saved files of the one approved positive run; no native/process calls.

Run once as python -I -B inspect_saved_positive.py from the pinned preparation
folder on Windows. Creates positive-evidence.json exclusively there, also on a
verification failure. Original evidence is never changed. A separate native
absence observation is deliberately not an input or claim of this analyzer.
"""
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import sys
import xml.etree.ElementTree as ET

HEAD = "1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3"
RUN_ID = "027ca2dd03624a318f469ad87b259bff"
ROOT = Path(r"D:\dev\ChatGPT")
PREP = ROOT / "mcp-r17-public-go-positive-prepare-20261119"
RUN = ROOT / "mcp-r17-native-qualification-20261119" / RUN_ID
BUILD = ROOT / "mcp-r17-hidden-work-20261119/public-go-positive-empty-input"
DESTINATION = PREP / "positive-evidence.json"
PREFIX = "mcp-r17-" + RUN_ID + "-"
NAMES = [PREFIX + kind for kind in ("visible", "hidden")]
CHECKS = ["status-visible", "export-visible", "status-hidden", "export-hidden", "list-visible-only"]
SOURCE = "e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa"
FIXTURE = "b3cf12165b57df5f8ab43e41fc00b57ecdc824c650203ab3d4732386beebc514"
READER = "c618d809208ffa0be23b8c27bd26c0ee0c825faf981bed0310ca8b16ee809650"
BINARY = "b97b069c42cc56ef632962864495b10482f886e94a12ea2a0335a9ba4a7d3bfc"
LAUNCHER = "c317bd988f9519a4bb0f085b12f90c8e090d66e7a8d29dcc97ab3436ba4eca64"
POWERSHELL = "3247bcfd60f6dd25f34cb74b5889ab10ef1b3ec72b4d4b3d95b5b25b534560b8"
NS = {"t": "http://schemas.microsoft.com/windows/2004/02/mit/task"}
# Raw lengths and hashes captured by the executor before this analysis.
RAW_PINS = {
    'run/intent.json': (856, 'f82f6ef06dfdb41cfb90ad84cbd0abed19e68b0e885ed3a2f6ded5e441d50832'),
    'run/go-manifest.json': (4300, 'de2950bd13bf531672d637036c1c465cee02585cbd8cb84cfa204a425af3c3e3'),
    'run/go-child-start.json': (441, 'f73ccfcb54f8c362abaae862041fd7ad89652c7ca26c2924cb132885e3ca4451'),
    'run/result.json': (6042, 'a788ded4305a1603070f6384fba76caab50a3995a33c93ae96a111815b37f062'),
    'run/positive-go.jsonl': (1882, 'b3baa880df91b16b02cf5f2b5dbf0205619acdbe29ef1d4e70b569133db71b93'),
    'run/positive-go.stderr.txt': (0, 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'),
    'run/mcp-r17-027ca2dd03624a318f469ad87b259bff-visible.xml': (1262, 'f6d69017190841e2bd671ee3358ac424cfaf2a7e781124731c61a1647b0e7cd8'),
    'run/mcp-r17-027ca2dd03624a318f469ad87b259bff-hidden.xml': (1288, 'dba11cbad453e2b9d2c62eb80a5e6fe694b6bc76fc3646253982618dccd74f7a'),
    'preparation/launch-intent.json': (989, 'eb7cad1e5049bee340d95f3fa1079e4d3d0458749f07747af773a9a12a293c00'),
    'preparation/launch-result.json': (413, 'af0552e437ac9b9faa718ad1cac4214681a15637351c1c9321ec7ebc621ec655'),
    'preparation/launch.stdout.log': (209, '17f9cd73d96afb7527a99ed95bb2e88f881d04dbe9ea46f7c80a5009d4967887'),
    'preparation/launch.stderr.log': (0, 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'),
    'preparation/execution-preflight.json': (1697, '616bdcd45534bb10b7d6945253df8feee113332376bd174cc1f48fb85690ae21'),
}

INPUTS = {
    "fixture": (PREP / "Invoke-R17PublicGoPositiveFixture.ps1", 16825, FIXTURE),
    "launcher": (PREP / "launch_positive.py", 5724, LAUNCHER),
    "probe_source": (PREP / "probe.go", 5636, READER),
    "runner": (PREP / "run_probe.py", 12810, "7f2b35cfdba2bd1b726920bb49c1bf9fbe3aa9b0bf556e6a10d54e4340c290f1"),
    "probe_executable": (BUILD / "probe.exe", 5365760, BINARY),
    "production_source": (ROOT / "mcp-r17-resume-20261119/scheduler_com_windows-1b870e04.go", 10040, SOURCE),
    "build_receipt": (BUILD / "execution.json", 5128, "11e3f401b38a4c19e00000c64255070495de2e08fec450592c2a955a9e65b5b3"),
    "empty_input": (BUILD / "probe.stdout.log", 270, "1bc5ca1d7a514f477dfd4c168d63dfbfb9d0804f5f444a30850adfb07455d8cb"),
}


class EvidenceMismatch(Exception):
    """Contains only a fixed, non-private condition label."""


def need(condition, label):
    if not condition:
        raise EvidenceMismatch(label)


def integer(value, minimum=0):
    return type(value) is int and value >= minimum


def timestamp(value):
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    need(parsed.tzinfo is not None, "timestamp_timezone")
    return parsed


def inspect(report):
    blobs = {}
    paths = {key: ((RUN if key.startswith("run/") else PREP) / key.split("/", 1)[1], size, sha)
             for key, (size, sha) in RAW_PINS.items()}
    paths.update(INPUTS)
    for label, (path, size, sha) in paths.items():
        need(path.resolve(strict=True).is_relative_to(ROOT), "input_outside_root")
        raw = path.read_bytes()  # Hash raw CRLF bytes first; never use read_text().
        actual = hashlib.sha256(raw).hexdigest()
        report["files"][label] = {"bytes": len(raw), "sha256": actual}
        need(len(raw) == size and actual == sha, "input_bytes:" + label)
        blobs[label] = raw
    report["verified_phases"].append("all_21_input_byte_pins")
    records = {key: json.loads(blobs[key].decode("utf-8")) for key in (
        "run/intent.json", "run/go-manifest.json", "run/go-child-start.json", "run/result.json",
        "preparation/launch-intent.json", "preparation/launch-result.json",
        "preparation/launch.stdout.log", "preparation/execution-preflight.json", "build_receipt")}
    intent, manifest, result = (records["run/" + name] for name in ("intent.json", "go-manifest.json", "result.json"))
    launch, outcome, preflight = (records["preparation/" + name] for name in
                                 ("launch-intent.json", "launch-result.json", "execution-preflight.json"))
    early = records["run/go-child-start.json"]
    build = records["build_receipt"]
    report["recorded_error_presence"] = {"owner": result["error"] is not None,
        "launcher": outcome["error"] is not None, "tasks": [task["error"] is not None for task in result["tasks"]]}

    need(build["source_head"] == HEAD and build["source_head_after"] == HEAD, "build_head")
    need(build["source_unchanged"] is True and build["source_sha256_before"] == build["source_sha256_after"], "build_source_unchanged")
    need(build["source_sha256_before"]["internal/scheduler/scheduler_com_windows.go"] == SOURCE, "build_production_source")
    need(build["probe_sha256"] == READER and build["probe_executable_sha256"] == BINARY, "build_reader_binding")
    need([entry["name"] for entry in build["commands"]] == ["go-version", "gofmt", "build", "probe"], "build_command_order")
    need(all(entry["settled"] is True and type(entry["exit_code"]) is int and entry["exit_code"] == code
             for entry, code in zip(build["commands"], (0, 0, 0, 1))), "build_and_empty_input_outcomes")
    empty = [json.loads(line) for line in blobs["empty_input"].decode("utf-8").splitlines()]
    need(len(empty) == 2 and empty[0]["manifest_valid"] is False and empty[-1]["pass"] is False, "earlier_empty_input_guard")
    need(preflight["run_id"] == RUN_ID and preflight["native_run_directory_absent"] is True
         and preflight["launch_markers_absent"] is True, "preflight_one_shot_absence")
    for label, sha in (("launcher", LAUNCHER), ("fixture", FIXTURE), ("probe_source", READER),
                       ("probe_executable", BINARY), ("production_source", SOURCE), ("system_powershell", POWERSHELL)):
        entry = preflight["input_files"][label]
        need(entry["sha256"] == sha and entry["expected_sha256"] == sha, "preflight_input_binding:" + label)
    need(launch["mode"] == "APPLY" and launch["run_id"] == RUN_ID
         and launch["named_tasks"] == ["\\" + name for name in NAMES], "launch_scope")
    for key, sha in (("fixture_sha256", FIXTURE), ("probe_sha256", BINARY), ("source_sha256", SOURCE),
                     ("powershell_sha256", POWERSHELL), ("launcher_sha256", LAUNCHER)):
        need(launch[key] == sha, "launch_input_binding:" + key)
    need(outcome["run_id"] == RUN_ID and outcome["started_utc"] == launch["started_utc"], "launch_receipt_binding")
    need(outcome["start_attempted"] is True and outcome["settled"] is True and outcome["error"] is None
         and type(outcome["exit_code"]) is int and outcome["exit_code"] == 0, "recorded_fixture_process_outcome")
    for stream in ("stdout", "stderr"):
        need(outcome[stream + "_sha256"] == report["files"]["preparation/launch." + stream + ".log"]["sha256"], "launch_stream_binding:" + stream)
    stdout = records["preparation/launch.stdout.log"]
    need(stdout["reads_passed"] is True and stdout["cleanup_complete"] is True
         and stdout["failure_recorded"] is False and stdout["evidence"] == str(RUN), "launch_stdout")
    report["verified_phases"].append("preparation_and_launch_binding")

    need(intent["head"] == manifest["head"] == result["head"] == HEAD, "run_head")
    need(intent["source_sha256"] == result["source_sha256"] == SOURCE
         and intent["probe_sha256"] == result["probe_sha256"] == BINARY, "run_input_binding")
    need(intent["run_id"] == RUN_ID and intent["names"] == NAMES and manifest["prefix"] == PREFIX, "run_names")
    action = RUN / "never-created-fixture-action.exe"
    need(intent["action"] == str(action) and intent["marker"] == "R17 #660 " + RUN_ID + " Ж§", "inert_definition_intent")
    try:
        action.lstat()
    except FileNotFoundError:
        action_missing = True
    else:
        action_missing = False
    need(action_missing, "inert_action_still_absent_at_file_inspection")
    need(result["reads_passed"] is True and result["cleanup_complete"] is True
         and result["error"] is None and result["checks"] == CHECKS, "owner_result")
    need(len(result["tasks"]) == len(manifest["tasks"]) == 2, "exact_pair")
    for index, (task, expected) in enumerate(zip(result["tasks"], manifest["tasks"])):
        hidden = index == 1
        need(task["name"] == NAMES[index] and expected["name"] == "\\" + NAMES[index], "exact_task_name")
        need(task["hidden"] is hidden and expected["hidden"] is hidden, "exact_visibility")
        need(all(task[key] is True for key in ("attempted", "created", "confirmed"))
             and task["cleanup"] == "removed" and task["error"] is None, "owned_task_cleanup_receipt")
        need(isinstance(expected["owner"], str) and bool(expected["owner"].strip())
             and task["owner"] == expected["owner"], "raw_principal_exact")
        xml = blobs["run/" + task["name"] + ".xml"].decode("utf-8")
        need(xml == task["xml"] == expected["xml"] and "Ж§" in xml, "exact_utf8_definition_snapshots")
        document = ET.fromstring(xml)
        need(document.tag == "{" + NS["t"] + "}Task", "task_xml_namespace")
        text = lambda path, default=None: document.findtext("t:" + path.replace("/", "/t:"), default, NS)
        need(text("RegistrationInfo/Description") == intent["marker"]
             and text("RegistrationInfo/URI") == expected["name"], "xml_marker_uri")
        principals = document.findall("t:Principals/t:Principal", NS)
        need(len(principals) == 1 and principals[0].get("id") == "R17Fixture"
             and text("Principals/Principal/UserId") == intent["principal_sid"], "xml_principal_sid")
        need(text("Principals/Principal/LogonType") == "InteractiveToken"
             and text("Principals/Principal/RunLevel", "LeastPrivilege") == "LeastPrivilege", "xml_interactive_least_privilege")
        need(text("Settings/Enabled") == "false" and text("Settings/AllowStartOnDemand") == "false"
             and text("Settings/Hidden", "false") == ("true" if hidden else "false"), "xml_disabled_visibility_demand")
        need(len(document.findall("t:Triggers/*", NS)) == 0, "xml_no_triggers")
        actions = document.findall("t:Actions/*", NS)
        need(len(actions) == 1 and actions[0].tag == "{" + NS["t"] + "}Exec"
             and document.find("t:Actions", NS).get("Context") == "R17Fixture", "xml_single_exec")
        need(text("Actions/Exec/Command") == intent["action"] and text("Actions/Exec/Arguments", "") == "", "xml_inert_action")
    report["verified_phases"].append("intent_manifest_pair_snapshots_and_cleanup_receipt")

    need(len(result["children"]) == 1, "one_child")
    child = result["children"][0]
    report["recorded_error_presence"]["child_persistence_error_count"] = len(child["persistence_errors"])
    for key in ("check", "pid", "started_utc", "probe_sha256"):
        need(early[key] == child[key], "child_start_binding:" + key)
    need(child["check"] == "public-go-positive" and integer(child["pid"], 1)
         and child["probe_sha256"] == BINARY, "child_identity")
    need(early["start_attempted"] is True and early["exit_code"] is None and early["persistence_errors"] == []
         and all(early[key] is False for key in ("settled", "capture_complete", "stdout_written", "stderr_written")), "early_child_receipt_is_pre_completion")
    need(all(child[key] is True for key in ("start_attempted", "settled", "capture_complete", "stdout_written", "stderr_written"))
         and child["persistence_errors"] == [] and type(child["exit_code"]) is int and child["exit_code"] == 0, "settled_child_and_persisted_capture")
    need(timestamp(preflight["checked_utc"]) < timestamp(launch["started_utc"]) < timestamp(child["started_utc"])
         < timestamp(outcome["finished_utc"]), "recorded_time_order")
    report["verified_phases"].append("early_and_final_child_receipts")

    events = [json.loads(line) for line in blobs["run/positive-go.jsonl"].decode("utf-8").splitlines()]
    need(len(events) == 12, "exact_12_go_events")
    admission = events[0]
    need(admission["event"] == "admission" and admission["pass"] is True and admission["manifest_valid"] is True
         and admission["error_present"] is False and admission["backend_type"] == "*scheduler.windowsScheduler"
         and admission["target_head"] == HEAD, "real_public_go_admission")
    accepted = []
    for index, label in enumerate(CHECKS):
        begin, event = events[1 + index * 2:3 + index * 2]
        need(begin == {"event": "begin", "check": label}, "go_begin_order:" + label)
        need(event["event"] == "result" and event["check"] == label and event["pass"] is True
             and integer(event["duration_ns"]) and all(event[key] is False for key in
             ("error_present", "is_task_not_found", "is_unavailable")), "go_result:" + label)
        observations = event["observations"]
        required = ["xml_exact", "utf8_valid", "unicode_marker_preserved"] if label.startswith("export-") else [
            "name_exact", "owner_exact", "runtime_disabled", "state_disabled"]
        if label == "list-visible-only":
            required.append("exactly_one")
        need(all(observations[key] is True for key in required), "go_semantics:" + label)
        safe = {key: observations[key] for key in required}
        if not label.startswith("export-"):
            need(type(observations["last_result_observed"]) is int, "last_result_observation_type")
            safe["last_result_observed"] = observations["last_result_observed"]  # Never gate on its value.
        accepted.append({"check": label, "pass": True, "duration_ns": event["duration_ns"], "observations": safe})
    overall = events[-1]
    need(overall["event"] == "overall" and overall["status"] == "PASS" and overall["pass"] is True
         and overall["panic_recovered"] is False and overall["task_status_subset_only"] is True
         and overall["last_result_semantics_qualified"] is False and integer(overall["duration_ns"]), "go_overall")
    report["verified_phases"].append("five_public_go_results_in_exact_order")
    report.update({"status": "PASS", "public_go_positive_reads_passed": True, "accepted_checks": accepted,
        "go_event_count": len(events), "go_duration_ns": overall["duration_ns"],
        "cleanup_complete_recorded": True, "pair_definitions_verified": True,
        "action_missing_at_analysis": action_missing, "raw_owner_equality_verified": True,
        "last_result_semantics_qualified": False,
        "child": {key: child[key] for key in ("pid", "started_utc", "exit_code", "settled", "capture_complete", "stdout_written", "stderr_written")},
        "launcher": {key: outcome[key] for key in ("started_utc", "finished_utc", "settled", "exit_code")},
        "stderr_metadata": {key: report["files"][key] for key in ("run/positive-go.stderr.txt", "preparation/launch.stderr.log")}})


def main():
    if os.name != "nt" or ROOT.resolve(strict=True) != ROOT or not PREP.resolve(strict=True).is_relative_to(ROOT):
        print(json.dumps({"saved": False, "status": "FAIL", "condition": "admitted_windows_paths_required"}))
        return 2
    report = {"schema_version": 1, "analyzed_utc": datetime.now(timezone.utc).isoformat(),
        "run_id": RUN_ID, "source_head": HEAD, "status": "FAIL", "failure": None,
        "files": {}, "verified_phases": [], "analyzer_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "limits": ["Saved files only; no Scheduler, account-resolution or process calls",
            "The owner receipt records cleanup; independent post-exit task absence is separate",
            "Raw owners, XML and raw errors remain in unchanged private input files",
            "Only the named five reads and the TaskStatus subset are qualified; LastResult is observational",
            "Recorded launcher exit_code belongs to its PowerShell child; outer RDC code is separate",
            "No task execution, production mutators, live cancellation, full Unicode or process-tree deadline qualification",
            "The system PowerShell hash is checked against admission records, not re-read outside D",
            "Full Go FAIL results and other R17 limits remain unchanged"]}
    try:
        inspect(report)
    except Exception as error:
        report["status"] = "FAIL"
        report["failure"] = {"type": type(error).__name__,
            "condition": str(error) if isinstance(error, EvidenceMismatch) else "input_read_or_decode_failed"}
    try:
        with DESTINATION.open("x", encoding="utf-8", newline="\n") as stream:
            json.dump(report, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
    except OSError as error:
        print(json.dumps({"saved": False, "status": "FAIL", "report_error_type": type(error).__name__}))
        return 2
    print(json.dumps({"saved": True, "status": report["status"], "inspected_files": len(report["files"]),
                      "public_go_positive_reads_passed": report.get("public_go_positive_reads_passed", False)}))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    try:
        exit_code = main()
    except Exception as error:
        print(json.dumps({"saved": False, "status": "FAIL", "analyzer_error_type": type(error).__name__}))
        exit_code = 2
    sys.exit(exit_code)
