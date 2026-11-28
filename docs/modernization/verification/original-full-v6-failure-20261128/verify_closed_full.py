"""Verify preserved closed full-v6 failure data; never execute preserved sources.

This is a DATA receiver. It reads a reconstructed evidence directory, checks
frozen hashes and outcome ledgers, and writes one new summary. It does not
launch a process, import a preserved script, run Git, use a cache, or confer
product/test qualification.
"""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import re

BINDINGS_SHA256 = "4cdec1bc4436a72f7e75df04ee78c78082c3dc9f07fc24d6911267bc83cb6af9"
EXPECTED_MAIN = "6cd606a5ec4e6e1a3e74d4b8324786cc98dffe8cd587f4a41c89a751ea7f0c4a"
EXPECTED_AUDIT = "f02bf04c80d0e84a5989297f742662ea874bbf7743c78ab3da947305004849d5"
EXPECTED_SOURCE = "727d66753814dc218e26d699e57402db2d13190a"
PACKAGES = (
    ("default-cli", "literal-default", "mcp-local-hub/internal/cli", 2533),
    ("default-oneapi", "literal-default", "mcp-local-hub/internal/oneapi", 19),
    ("default-daemon", "literal-default", "mcp-local-hub/internal/daemon", 423),
    ("default-process", "literal-default", "mcp-local-hub/internal/process", 1205),
    ("state-api", "literal-state", "mcp-local-hub/internal/api", 6281),
)
TERMINAL = re.compile(
    r"(?:^|\x16)[ \t]*--- (PASS|FAIL|SKIP): (\S+) \(([0-9.]+)s\)\r?$",
    re.MULTILINE,
)


def require(ok, message):
    if not ok:
        raise ValueError(message)


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def outcomes(rows):
    return dict(Counter(row["outcome"] for row in rows))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--restored", required=True, type=Path,
                        help="Output directory of reconstruct_sources.py.")
    parser.add_argument("--summary", required=True, type=Path,
                        help="A new, absent JSON summary file.")
    args = parser.parse_args()
    capsule = Path(__file__).resolve().parent
    raw_bindings = (capsule / "source-bindings.json").read_bytes()
    require(sha256(raw_bindings) == BINDINGS_SHA256, "bindings changed")
    bindings = json.loads(raw_bindings)
    require(bindings["source_file_count"] == 20 and len(bindings["files"]) == 20,
            "fixed original evidence inventory differs")
    originals = {}
    for item in bindings["files"]:
        relative = item["original_role_path"]
        require(relative and "\\" not in relative and ":" not in relative
                and ".." not in Path(relative).parts
                and not Path(relative).is_absolute(), "unclean evidence path")
        path = args.restored / relative
        require(path.is_file() and not path.is_symlink(),
                "ordinary reconstructed evidence file required")
        raw = path.read_bytes()
        require(len(raw) == item["original_bytes"]
                and sha256(raw) == item["original_sha256"],
                "original evidence differs: " + relative)
        require(relative not in originals, "duplicate original evidence path")
        originals[relative] = raw
    require(sha256(originals["full/receipt.json"]) == EXPECTED_MAIN,
            "full original changed")
    require(sha256(originals["root-receiving/result.json"]) == EXPECTED_AUDIT,
            "root receiving changed")
    full = json.loads(originals["full/receipt.json"])
    parent = json.loads(originals["full/parent.json"])
    outer = json.loads(originals["full/outer-original.json"])
    audit = json.loads(originals["root-receiving/result.json"])
    require(full["status"] == "FAIL"
            and full["literal_pre_push_commands_passed"] is False
            and full["source_before"]["head"] == EXPECTED_SOURCE,
            "original failing qualification or Source differs")
    for flag in ("all_original_invocations_closed", "commands_quiescent",
                 "source_unchanged", "original_inputs_unchanged",
                 "actual_product_unchanged", "private_migration_exports_unchanged"):
        require(full[flag] is True, "full closed observation differs: " + flag)
    require(parent["state"] == "received" and parent["exit_code"] == 1
            and parent["status"] == "FAIL" and parent["physical_complete"]
            and parent["terminal_exit_received"]
            and parent["parent_capture_files_closed"]
            and parent["commands_quiescent"] and not parent["deadline_exceeded"],
            "original parent is not the frozen FAIL/CLOSED receipt")
    require(outer["same_process_start_object"] and outer["wait_returned"]
            and outer["original_process_object_disposed"]
            and outer["exit_code"] == 1 and not outer["wait_errors"]
            and not outer["new_job_or_tree_wait"], "outer custody differs")
    require(audit["classification"] == "FAIL/CLOSED"
            and audit["qualification_passed"] is False
            and audit["original_failure_reclassified"] is False
            and audit["cache_qualification_inherited"] is False,
            "root audit classification differs")
    expected_audit = {
        "commands_checked": 283, "package_commands_checked": 272,
        "command_captures_hashed": 566, "package_receipts_hashed": 65,
        "original_owner_receipts_hashed": 130, "source_files_hashed": 3509,
        "input_files_hashed": 115,
    }
    for key, value in expected_audit.items():
        require(audit[key] == value, "root audit cardinality differs: " + key)
    require(len(audit["restored_absent_resource"]) == 1,
            "temporary resource restoration differs")
    coverage = {row["lane"]: row for row in full["coverage"]}
    default, state = coverage["literal-default"], coverage["literal-state"]
    require(outcomes(default["top_level"]) ==
            {"pass": 9746, "skip": 194, "fail": 7, "NOT_RUN": 389},
            "default top-level ledger differs")
    require(outcomes(default["all_terminal"]) ==
            {"pass": 17286, "skip": 265, "fail": 8},
            "default full terminal ledger differs")
    require(outcomes(state["top_level"]) ==
            {"pass": 3460, "skip": 65, "fail": 4},
            "tagged API top-level ledger differs")
    require(outcomes(state["all_terminal"]) ==
            {"pass": 6194, "skip": 83, "fail": 4},
            "tagged API full terminal ledger differs")
    require(not default["mandatory_passes_satisfied"]
            and not state["mandatory_passes_satisfied"],
            "unearned acceptance qualification")
    for name, expected, completed, missing in (
            ("default", 64, 64, []),
            ("state", 2, 1, ["mcp-local-hub/internal/cli"])):
        phase = full["literal_phases"][name]
        require(len(phase["expected_invocations"]) == expected
                and len(phase["completed_invocations"]) == completed
                and phase["missing_invocations"] == missing
                and phase["native_custody_settled"] is True
                and phase["passed"] is False
                and phase["command_exit_code"] == 1,
                "literal phase result differs: " + name)
    require(state["expected"]["mcp-local-hub/internal/cli"] == [],
            "tagged CLI acquired an unexpected test inventory")
    frame_checks = []
    for role, lane, package, expected_terminals in PACKAGES:
        receipt = json.loads(originals[role + "/receipt.json"])
        require(receipt["native_custody_settled"] is True
                and receipt["commands_quiescent"] is True,
                "package not physically closed: " + role)
        for command in receipt["commands"]:
            require(command["quiescence_state"] == "complete"
                    and command["captures_measured"] is True,
                    "package command not closed: " + role)
        command = next(c for c in receipt["commands"]
                       if c["stdout"].endswith("\\suite-owner.stdout.txt"))
        for stream in ("stdout", "stderr"):
            raw = originals[role + "/suite-owner." + stream + ".txt"]
            require(sha256(raw) == command[stream + "_sha256"],
                    "original package stream differs: " + role + "/" + stream)
        text = originals[role + "/suite-owner.stdout.txt"].decode("utf-8")
        parsed = [(package, match[2], match[1].lower())
                  for match in TERMINAL.finditer(text)]
        expected = [(row["package"], row["test"], row["outcome"])
                    for row in coverage[lane]["all_terminal"]
                    if row["package"] == package]
        require(len(parsed) == expected_terminals
                and len(parsed) == len(set(parsed))
                and sorted(parsed) == sorted(expected),
                "frame-aware raw terminal ledger differs: " + role)
        frame_checks.append({"role": role, "terminal_records": len(parsed),
                             "exact_outcome_ledger": True})
    cli = originals["default-cli/suite-owner.stdout.txt"].decode("utf-8")
    naive = re.findall(r"^--- PASS: ([^ /]+) \(", cli.replace("\x16", ""),
                       flags=re.MULTILINE)
    require(len(naive) == 1584, "documented naive CLI count differs")
    require("Branch [A/B]: \x16--- PASS: "
            "TestStrictModeRecover_OrdinaryIntentFailureStopsBeforeShim (0.02s)"
            in cli, "documented framing discrepancy differs")
    cli_stderr = originals["default-cli/suite-owner.stderr.txt"].decode("utf-8")
    require("panic: test timed out after 5m0s" in cli_stderr
            and "TestRunSupervisorHeartbeat_StopsOnDone" in cli_stderr,
            "original CLI timeout diagnostic differs")
    result = {
        "schema": "closed-full-v6-data-verification-v1",
        "status": "EXACT_CLOSED_FAILURE_DATA_VERIFIED",
        "source_bindings_sha256": BINDINGS_SHA256,
        "original_files_verified": len(originals),
        "primary_sha256": EXPECTED_MAIN,
        "classification": "FAIL/CLOSED",
        "qualification_passed": False,
        "original_failure_reclassified": False,
        "frame_aware_checks": frame_checks,
        "terminal_records_checked": sum(x["terminal_records"] for x in frame_checks),
        "closed_run_commands_reexecuted": False,
        "source_test_or_native_commands_executed": False,
        "cache_qualification_inherited": False,
        "original_566_capture_audit_reexecuted": False,
        "original_root_audit_preserved": expected_audit,
    }
    require(not args.summary.exists() and not args.summary.is_symlink(),
            "summary must be a new absent file")
    with args.summary.open("xb") as stream:
        stream.write((json.dumps(result, indent=2) + "\n").encode("utf-8"))
    print(json.dumps({"status": result["status"],
                      "original_files_verified": len(originals),
                      "terminal_records_checked": result["terminal_records_checked"],
                      "qualification_passed": False}))


if __name__ == "__main__":
    main()
