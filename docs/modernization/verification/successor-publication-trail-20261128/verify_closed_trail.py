"""Verify reconstructed, closed publication/preparation evidence as DATA only."""
import argparse
import hashlib
import json
from pathlib import Path

EVENTS = ("stage-v11", "stage-v12", "commit-v7", "stage-v13", "commit-v8",
          "stage-v14", "commit-v9", "push-v7", "stage-v15", "commit-v10")
COUNTS = (4, 9, 6, 11, 6, 7, 6, 4, 11, 6)


def need(ok, message):
    if not ok:
        raise ValueError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def read_json(path):
    return json.loads(path.read_bytes())


def check_command(row, root, event, position, capture_index):
    need(row["command_success"] is True and row["exit_code"] == 0
         and row["quiescence_state"] == "complete"
         and row["timed_out"] is False
         and row["work_deadline_exceeded"] is False
         and row["cleanup_deadline_exceeded"] is False,
         event + ": original command result differs")
    native = row["native"]
    need(native["direct_exit_code"] == 0 and native["direct_reaped"] is True
         and native["active_zero"] is True and native["handles_closed"] is True
         and native["job_terminated"] is False and not native["issues"],
         event + ": original native closure differs")
    need(row["original_readers_joined"] is True
         and row["completion_receiving"]["lifecycle_state"] == "CLOSED"
         and len(row["worker_join_observations"]) == 2
         and all(x["joined"] is True and x["alive_after"] is False
                 and x["join_error_count"] == 0 for x in row["worker_join_observations"]),
         event + ": original readers differ")
    for side in ("stdout", "stderr"):
        io = row["io"][side]
        need(all(io[k] is True for k in ("acquired", "worker_complete", "eof",
                                         "pipe_close_returned", "capture_close_returned"))
             and not io["error"] and io["limit_exceeded"] is False,
             event + ": original IO closure differs")
        item = capture_index[(event, position, side)]
        raw = (root / item["original_role_path"]).read_bytes()
        need(len(raw) == item["bytes"] == io["bytes_stored"]
             and sha(raw) == item["sha256"] == row[side + "_sha256"],
             event + ": complete original capture differs")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--reconstructed", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    capsule = Path(__file__).resolve().parent
    bindings = read_json(capsule / "source-bindings.json")
    need(bindings["source_file_count"] == 213 and len(bindings["files"]) == 213,
         "fixed reconstruction inventory differs")
    need(args.reconstructed.is_dir() and not args.reconstructed.is_symlink(),
         "ordinary reconstructed DATA directory required")
    for item in bindings["files"]:
        path = args.reconstructed / item["original_role_path"]
        need(path.is_file() and not path.is_symlink(), "ordinary DATA artifact required")
        raw = path.read_bytes()
        need(len(raw) == item["original_bytes"] and sha(raw) == item["original_sha256"],
             "reconstructed original differs: " + item["original_role_path"])
    index = read_json(capsule / "capture-index.json")
    need(index["original_capture_count"] == 148 and len(index["captures"]) == 148,
         "fixed command capture inventory differs")
    capture_index = {(x["event"], x["command_index"], x["stream"]): x for x in index["captures"]}
    need(len(capture_index) == 148, "duplicate capture binding")

    checked = 0
    for event, count in zip(EVENTS, COUNTS):
        base = args.reconstructed / "events" / event
        receipt = read_json(base / "receipt.json")
        plan = read_json(base / "plan.json")
        parent = read_json(base / "root.json")
        outer = read_json(base / "outer.json")
        expected_status = "FAIL" if event == "stage-v11" else "PASS"
        expected_exit = 1 if event == "stage-v11" else 0
        need(receipt["status"] == expected_status and len(receipt["commands"]) == count
             and receipt["command_quiescence"]["complete"] is True
             and receipt["config_sha256"] == sha((base / "plan.json").read_bytes())
             and receipt["expected_head"] == plan["head"],
             event + ": primary classification/binding differs")
        need(parent["controller_exit_code"] == expected_exit
             and parent["original_controller_reaped"] is True
             and parent["controller_capture_handles_closed"] is True
             and parent["receipt_sha256"] == sha((base / "receipt.json").read_bytes()),
             event + ": original controller receiving differs")
        need(outer["same_process_start_object"] is True and outer["exit_code"] == expected_exit
             and outer["wait_returned"] is True
             and outer["original_process_object_disposed"] is True
             and outer["new_job_or_tree_wait"] is False and not outer["wait_errors"]
             and outer["config_sha256"] == receipt["config_sha256"],
             event + ": original outer receiving differs")
        if event == "stage-v11":
            need(receipt["error"] == "RuntimeError: test changed observed source inputs"
                 and len(receipt["steps_received"]) == 2
                 and plan["steps"][2]["name"] == "apply-minimal-successor-tests"
                 and "mutates_inputs" not in plan["steps"][2],
                 "stage-v11 failure and omitted mutation declaration must remain")
        else:
            need(len(receipt["steps_received"]) == len(plan["steps"]),
                 event + ": complete declared step receiving differs")
        for position, row in enumerate(receipt["commands"]):
            check_command(row, args.reconstructed, event, position, capture_index)
            checked += 1
        audit_path = base / "root-data-audit.json"
        if audit_path.exists():
            audit = read_json(audit_path)
            need(audit["status"] == "PASS" and audit["physical_complete"] is True
                 and audit["tests_executed"] is False and audit["commands_verified"] == count
                 and audit["captures_verified"] == 2 * count
                 and audit["config_sha256"] == receipt["config_sha256"]
                 and audit["receipt_sha256"] == sha((base / "receipt.json").read_bytes())
                 and audit["root_sha256"] == sha((base / "root.json").read_bytes())
                 and audit["outer_sha256"] == sha((base / "outer.json").read_bytes()),
                 event + ": independent original DATA audit differs")

    warm = args.reconstructed / "warm-cache"
    plan, execution, parent = [read_json(warm / name) for name in
                               ("plan.json", "execution.json", "parent.json")]
    audit = read_json(warm / "root-data-audit.json")
    outer = read_json(warm / "outer-original.json")
    need(parent["status"] == "PASS" and parent["child_status"] == "PASS"
         and parent["exit_code"] == 0 and parent["physical_complete"] is True
         and parent["terminal_exit_received"] is True and parent["parent_capture_files_closed"] is True
         and parent["deadline_exceeded"] is False
         and parent["plan_sha256"] == sha((warm / "plan.json").read_bytes())
         and parent["execution_sha256"] == sha((warm / "execution.json").read_bytes()),
         "original warm parent differs")
    need(execution["status"] == "PASS" and execution["commands_quiescent"] is True
         and execution["input_files_unchanged"] is True and execution["payload"] == plan["payload"]
         and execution["plan_sha256"] == parent["plan_sha256"]
         and len(execution["commands"]) == 4 and len(execution["step_inputs"]) == 4,
         "original warm execution differs")
    need(all(x["before"] == plan["input_files"] == x["after"] for x in execution["step_inputs"]),
         "closed warm before/after maps differ")
    need(outer["same_process_start_object"] is True and outer["exit_code"] == 0
         and outer["wait_returned"] is True and outer["original_process_object_disposed"] is True,
         "original warm outer differs")
    need(audit["status"] == "PASS" and audit["physical_complete"] is True
         and audit["tests_executed"] is False and audit["full_acceptance_pass"] is False
         and audit["parent_sha256"] == sha((warm / "parent.json").read_bytes())
         and audit["outer_sha256"] == sha((warm / "outer-original.json").read_bytes())
         and audit["execution_sha256"] == sha((warm / "execution.json").read_bytes())
         and audit["cache_copy_receipt_sha256"] == sha((warm / "cache-copy-receipt.json").read_bytes()),
         "original warm root audit differs")
    history = read_json(warm / "source04e-input-sha256.json")
    need(history["commit"] == "04e5b4eadd6407cbff4e04ba453546d278314bd7"
         and history["tree"] == "f68f7584dba7744e7607b07f615758f3ff633f54"
         and history["tracked_files"] == len(history["files"]) == 3369
         and all(plan["input_files"].get(history["source_root"] + "\\" + relative.replace("/", "\\")) == pin
                 for relative, pin in history["files"].items()),
         "original warm Source04e evidence differs")
    for position, row in enumerate(execution["commands"]):
        check_command(row, args.reconstructed, "warm-cache", position, capture_index)
        checked += 1
    need(checked == 74, "full closed command inventory differs")
    root_proof = read_json(args.reconstructed / "preservation" / "root-original-byte-verification.json")
    original_spec = read_json(args.reconstructed / "preservation" / "original-byte-inputs.json")
    need(root_proof["status"] == "MATCH" and root_proof["files"] == root_proof["matched"] == 209
         and root_proof["spec_sha256"] == sha((args.reconstructed / "preservation" / "original-byte-inputs.json").read_bytes())
         and root_proof["native_or_test_execution"] is False and root_proof["qualification_passed"] is False
         and len(original_spec["original_files"]) == 209 and len(root_proof["rows"]) == 209,
         "root's actual original byte comparison differs")
    for expected, observed in zip(original_spec["original_files"], root_proof["rows"]):
        need(expected["role_path"] == observed["role_path"] and observed["matched"] is True
             and expected["bytes"] == observed["expected_bytes"] == observed["actual_bytes"]
             and expected["sha256"] == observed["expected_sha256"] == observed["actual_sha256"],
             "root's original byte comparison row differs")
    data = read_json(args.reconstructed / "preservation" / "full-v6-root-data-verification.json")
    need(data["classification"] == "FAIL/CLOSED" and data["qualification_passed"] is False
         and data["original_failure_reclassified"] is False
         and data["original_files_verified"] == 20 and data["terminal_records_checked"] == 10461,
         "previous exact failure DATA replay differs")
    trash = read_json(args.reconstructed / "preservation" / "root-reconstruction-trash-receiving.json")
    need(trash["status"] == "MOVED_AND_VERIFIED" and trash["destination_files"] == 21
         and trash["exact_original_data_copies"] == 20 and trash["original_bytes"] == 14916637
         and trash["primary_run_moved"] is False and trash["source_or_cache_moved"] is False
         and trash["unknown_owner_scope_touched"] is False and trash["permanent_deletion_performed"] is False,
         "bounded disposable reconstruction disposition differs")
    report_trash = read_json(args.reconstructed / "preservation" / "report-backup-trash-receiving.json")
    need(report_trash["status"] == "MOVED_AND_VERIFIED" and report_trash["files"] == 1
         and report_trash["bytes"] == 258462 and report_trash["source_absent"] is True
         and report_trash["destination_bytes_verified"] is True
         and report_trash["unique_data_removed"] is False
         and report_trash["permanent_deletion_performed"] is False
         and report_trash["trash_emptied"] is False,
         "published report duplicate disposition differs")
    result = {
        "schema": "closed-successor-trail-data-verification-v1",
        "status": "EXACT_CLOSED_TRAIL_DATA_VERIFIED",
        "original_files_verified": 213, "original_native_commands": checked,
        "original_command_captures": len(capture_index),
        "historical_stage_v11_failure_preserved": True,
        "historical_full_v6_failure_preserved": True,
        "current_source_test_acceptance_inherited": False,
        "original_commands_reexecuted": False,
        "live_source_or_cache_payload_read": False,
        "original_trash_operation_reexecuted": False,
    }
    need(not args.output.exists() and not args.output.is_symlink(), "fresh DATA output required")
    with args.output.open("xb") as stream:
        stream.write((json.dumps(result, indent=2) + "\n").encode("utf-8"))
    print(json.dumps(result))


if __name__ == "__main__":
    main()
