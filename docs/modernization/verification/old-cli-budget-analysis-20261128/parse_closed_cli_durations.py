"""Read one pinned historical CLOSED CLI capture as DATA and report test costs.
No source imports, process creation, native test execution, or current-run access.
"""
import argparse
from decimal import Decimal
import hashlib
import json
from pathlib import Path
import re

ROOT_SHA = "f02bf04c80d0e84a5989297f742662ea874bbf7743c78ab3da947305004849d5"
RECEIPT_SHA = "a5bd2bae2fd38c7c28ef516840f613852d211b4c407fa62b0944c10a2c92fc8b"
STDOUT_SHA = "d680bab54bba9cd6043a1f46359076d650a08d9061d550ceee54bff8019f787f"
PACKAGE = "mcp-local-hub/internal/cli"


def need(ok, message):
    if not ok:
        raise ValueError(message)


def pinned(path, expected):
    raw = path.read_bytes()
    need(hashlib.sha256(raw).hexdigest() == expected, "Pinned input differs")
    return raw


def receive_closed(root_path, receipt_path):
    root = json.loads(pinned(root_path, ROOT_SHA))
    value = json.loads(pinned(receipt_path, RECEIPT_SHA))
    need(root["classification"] == "FAIL/CLOSED"
         and root["qualification_passed"] is False
         and any(row["phase"] == "default" and row["package"] == PACKAGE
                 and row["exit"] == 2 and row["receipt_sha256"] == RECEIPT_SHA
                 for row in root["nonzero_packages"]), "Wrong original scope")
    need(value["status"] == "FAIL" and value["phase"] == "default"
         and value["package"] == PACKAGE
         and value["native_custody_settled"] is True
         and value["commands_quiescent"] is True
         and value["canonical_receiving"]["physical_complete"] is True,
         "Original CLI closure is absent")
    proof = value["owner"]["proof"]
    outer = proof["outer"]
    native = outer["native"]
    need(proof["composite_complete"] is True
         and proof["captures_finalized"] is True
         and proof["custody_retained"] is False
         and outer["settlement_complete"] is True
         and outer["custody_retained"] is False
         and native["RootWaitKnown"] is True and native["RootExitCode"] == 2
         and native["MembersExited"] is True
         and native["AccountingKnown"] is True and native["FinalActive"] == 0
         and native["HandlesClosed"] is True and native["Error"] is None,
         "Original native completion is incomplete")
    for name in ("Stdin", "StdoutRead", "StdoutWrite", "StderrRead", "StderrWrite"):
        item = outer["io"][name]
        need(item["CloseAttempted"] is True and item["Closed"] is True
             and item["CloseError"] is None, "Original I/O handle remains open")
    for name in ("Stdout", "Stderr"):
        item = outer["io"][name]
        need(item["WorkerComplete"] is True and item["EOF"] is True
             and item["LimitExceeded"] is False and item["Error"] is None,
             "Original capture reader is incomplete")
    need(value["raw_replay"]["stdout"]["sha256"] == STDOUT_SHA
         and value["raw_output_binding"]["stdout_sha256"] == STDOUT_SHA
         and value["raw_output_binding"]["stdout_forwarded_identical"] is True,
         "Original stdout binding differs")
    return value


def parse(raw):
    frame_pattern = re.compile(rb"\x16([^\n\x16]*)")
    terminal_pattern = re.compile(rb"^( *)--- (PASS|FAIL|SKIP): (\S+) \((\d+(?:\.\d+)?)s\)$")
    transition_pattern = re.compile(rb"^=== (RUN|PAUSE|CONT)\s+(\S+)$")
    active, paused, started = set(), set(), set()
    ledger, all_terminals, issues = [], [], []
    cumulative = Decimal("0")
    line, cursor, frame_count, pause_count, cont_count = 1, 0, 0, 0, 0
    for index, match in enumerate(frame_pattern.finditer(raw)):
        frame_count += 1
        line += raw.count(b"\n", cursor, match.start())
        cursor = match.start()
        text = match.group(1).rstrip(b"\r")
        terminal = terminal_pattern.fullmatch(text)
        if terminal:
            indent, action, name, elapsed = terminal.groups()
            name, action = name.decode("ascii"), action.decode("ascii")
            duration = Decimal(elapsed.decode("ascii"))
            top = not indent and "/" not in name
            all_terminals.append({"test": name, "outcome": action,
                                  "seconds": float(duration), "top": top})
            if top:
                need(name in active, "Terminal without active root")
                active.remove(name)
                cumulative += duration
                ledger.append({"ordinal": len(ledger) + 1, "test": name,
                               "outcome": action, "printed_seconds": float(duration),
                               "cumulative_printed_seconds": float(cumulative),
                               "stdout_line": line, "frame_index": index,
                               "byte_offset": match.start()})
            continue
        transition = transition_pattern.fullmatch(text)
        if not transition:
            need(re.match(rb"^=== (NAME|ATTR)\s", text)
                 or text in (b"PASS", b"FAIL"), "Unexpected framed record")
            continue
        action, name = transition.groups()
        action, name = action.decode("ascii"), name.decode("ascii")
        if "/" in name:
            continue
        if action == "RUN":
            need(name not in started, "Duplicate root RUN")
            if active:
                issues.append({"event": name, "active": sorted(active)})
            started.add(name)
            active.add(name)
        elif action == "PAUSE":
            need(name in active, "PAUSE without active root")
            active.remove(name)
            paused.add(name)
            pause_count += 1
        else:
            need(name in paused, "CONT without paused root")
            paused.remove(name)
            active.add(name)
            cont_count += 1
    need(not issues, "Historical top-level execution overlaps")
    return {"frames": frame_count, "ledger": ledger, "all_terminals": all_terminals,
            "paused": sorted(paused), "active": sorted(active),
            "started": sorted(started), "pause_count": pause_count,
            "cont_count": cont_count, "printed_completed_seconds": float(cumulative),
            "overlapping_active_roots": issues}


def normalize_numbers(value):
    if isinstance(value, float) and value.is_integer():
        return int(value)
    if isinstance(value, list):
        return [normalize_numbers(item) for item in value]
    if isinstance(value, dict):
        return {key: normalize_numbers(item) for key, item in value.items()}
    return value


def write_new(path, value):
    raw = (json.dumps(normalize_numbers(value), indent=2) + "\n").encode("utf-8")
    with path.open("xb") as stream:
        stream.write(raw)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root-receiving", required=True, type=Path)
    parser.add_argument("--receipt", required=True, type=Path)
    parser.add_argument("--stdout", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    receipt = receive_closed(args.root_receiving, args.receipt)
    # Read the capture only after accepting the pinned old closure above.
    raw = pinned(args.stdout, STDOUT_SHA)
    need(len(raw) == 516257, "Unexpected original stdout size")
    result = parse(raw)
    inventory = receipt["inventory"]["tests"][PACKAGE]
    never = sorted(set(inventory) - set(result["started"]))
    passes = [row for row in result["ledger"] if row["outcome"] == "PASS"]
    skips = [row for row in result["ledger"] if row["outcome"] == "SKIP"]
    need(len(inventory) == 1988 and len(result["started"]) == 1614
         and len(passes) == 1585 and len(skips) == 14
         and len(result["ledger"]) == 1599 and len(never) == 374
         and result["pause_count"] == 14 and result["cont_count"] == 0
         and result["printed_completed_seconds"] == 299.30
         and result["active"] == ["TestRunSupervisorHeartbeat_StopsOnDone"],
         "Historical event totals differ")
    need(not args.output.exists(), "Output must be a new directory")
    args.output.mkdir(parents=False)
    write_new(args.output / "top-level-terminal-ledger.json",
              {"schema": "old-closed-cli-top-level-ledger-v1",
               "stdout_sha256": STDOUT_SHA, "units": "seconds", "rows": result["ledger"]})
    write_new(args.output / "summary.json",
              {"schema": "old-closed-cli-duration-replay-v1",
               "status": "EXACT_HISTORICAL_DATA_PARSED", "stdout_sha256": STDOUT_SHA,
               "frames": result["frames"], "top_pass": len(passes), "top_skip": len(skips),
               "printed_completed_seconds": result["printed_completed_seconds"],
               "never_started": never, "paused": result["paused"], "active": result["active"],
               "top20": sorted(passes, key=lambda row: -row["printed_seconds"])[:20],
               "new_native_or_test_commands": False, "current_qualification_inherited": False})
    print(json.dumps({"status": "EXACT_HISTORICAL_DATA_PARSED",
                      "top_pass": len(passes), "printed_seconds": 299.30,
                      "new_test_commands": False}))


if __name__ == "__main__":
    main()
