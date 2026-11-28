"""Reproduce the historical 717-root ledger from explicitly supplied original files.
DATA only: read JSON/text, verify hashes and original closure records, reconcile roots.
No controller imports, subprocesses, native calls, network access or file writes.
"""
import argparse
import hashlib
import json
from pathlib import Path, PureWindowsPath
import sys

sys.dont_write_bytecode = True

def require(ok, message):
    if not ok:
        raise ValueError(message)

def read_bytes(path, limit=4 << 20):
    with Path(path).open("rb") as stream:
        raw = stream.read(limit + 1)
    require(len(raw) <= limit, "input exceeds the declared data bound")
    return raw

def digest(raw):
    return hashlib.sha256(raw).hexdigest()

def receive(path, pin):
    raw = read_bytes(path)
    require(len(raw) == pin["bytes"] and digest(raw) == pin["sha256"],
            "original data size or SHA-256 differs")
    return raw

def number(value):
    return int(value) if isinstance(value, float) and value.is_integer() else value

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--paths", required=True,
                        help="private JSON object: expected_roots plus runs keyed by published run ID")
    parser.add_argument("--capsule", required=True,
                        help="directory containing evidence.json, cli717-roots.txt and ledger.jsonl")
    parser.add_argument("--emit-ledger", action="store_true")
    args = parser.parse_args()
    capsule = Path(args.capsule)
    evidence = json.loads(read_bytes(capsule / "evidence.json"))
    paths = json.loads(read_bytes(args.paths))
    require(set(paths) == {"expected_roots", "runs"}, "explicit original data roles required")
    expected_raw = receive(paths["expected_roots"], evidence["expected_inventory"])
    require(expected_raw == read_bytes(capsule / "cli717-roots.txt"),
            "original expected inventory differs from the published exact copy")
    expected = expected_raw.decode("utf-8").splitlines()
    require(len(expected) == len(set(expected)) == 717, "exact 717 unique roots required")
    require(all(name.startswith("Test") and "/" not in name for name in expected),
            "expected inventory must contain root test names")
    order = evidence["selection_rule"]["run_order"]
    require(len(order) == len(set(order)) == 4 and set(paths["runs"]) == set(order),
            "all four original run inputs are required")
    records = {name: [] for name in expected}
    started_sets = []
    for run in evidence["runs"]:
        run_id = run["id"]
        original = paths["runs"][run_id]
        require(set(original) == {"parent", "execution", "stdout", "stderr"},
                "parent, execution and both original test streams are required")
        parent_raw = receive(original["parent"], run["parent"])
        execution_raw = receive(original["execution"], run["execution"])
        parent, execution = json.loads(parent_raw), json.loads(execution_raw)
        require(digest(execution_raw) == parent["child_sha256"],
                "execution is not the original parent's child result")
        require(parent["status"] == run["original_status"]
                and parent["exit_code"] == run["parent"]["exit_code"]
                and execution["status"] == run["execution_status"]
                and execution["plan"]["source_head"] == run["source_commit"],
                "original status or source identity differs")
        require(parent["terminal_exit_received"] is True
                and parent["parent_capture_files_closed"] is True
                and parent["commands_quiescent"] is True
                and execution["commands_quiescent"] is True,
                "original physical completion is not received")
        commands = execution["commands"]
        require(len(commands) == run["execution"]["command_count"],
                "original command count differs")
        for command in commands:
            native = command["native"]
            require(command["quiescence_state"] == "complete"
                    and command["original_owner_retained"] is True
                    and command["original_readers_joined"] is True
                    and command["captures_measured"] is True
                    and native["direct_reaped"] is True
                    and native["active_zero"] is True
                    and native["handles_closed"] is True
                    and native["issues"] == []
                    and command["completion_receiving"]["lifecycle_state"] == "CLOSED",
                    "original command owner remains incomplete")
            require(set(command["io"]) == {"stdout", "stderr"},
                    "original stream roles differ")
            for stream in command["io"].values():
                require(all(stream[key] is True for key in (
                    "worker_started", "worker_complete", "eof",
                    "pipe_close_returned", "capture_close_returned"))
                    and stream["error"] is None
                    and stream["bytes_seen"] == stream["bytes_stored"],
                    "original reader/EOF/close result is incomplete")
        selected = [command for command in commands
                    if PureWindowsPath(command["stdout"]).name == run["root_capture"]["file"]]
        require(len(selected) == 1, "one original root capture is required")
        command = selected[0]
        stdout_raw = receive(original["stdout"], run["root_capture"])
        stderr_raw = receive(original["stderr"], run["root_stderr"])
        require(digest(stdout_raw) == command["stdout_sha256"]
                and len(stdout_raw) == command["io"]["stdout"]["bytes_stored"]
                and digest(stderr_raw) == command["stderr_sha256"]
                and len(stderr_raw) == command["io"]["stderr"]["bytes_stored"] == 0,
                "test streams do not match the original closed command")
        observations = {}
        for line in stdout_raw.decode("utf-8").splitlines():
            event = json.loads(line)
            name, action = event.get("Test"), event.get("Action")
            if not name or "/" in name or action not in ("run", "pass", "fail", "skip"):
                continue
            require(name in records, "capture contains an unexpected root")
            observed = observations.setdefault(name, {"starts": 0, "terminals": []})
            if action == "run":
                observed["starts"] += 1
            else:
                observed["terminals"].append(event)
        started_sets.append(set(observations))
        for name, observed in observations.items():
            require(observed["starts"] == 1 and len(observed["terminals"]) <= 1,
                    "duplicate start/terminal or terminal without start")
            terminal = observed["terminals"][0] if observed["terminals"] else None
            records[name].append({
                "run": run_id,
                "source_commit": run["source_commit"],
                "outcome": terminal["Action"].upper() if terminal else "UNTERMINATED",
                "duration_seconds": number(terminal.get("Elapsed")) if terminal else None,
                "terminal_received": terminal is not None,
            })
    require([run["id"] for run in evidence["runs"]] == order,
            "public run order differs")
    require(not (started_sets[0] & started_sets[1])
            and (started_sets[0] | started_sets[1]) == set(expected),
            "first two original runs do not partition the exact inventory")
    result = []
    counts = {"PASS": 0, "SKIP": 0}
    for name in expected:
        history = records[name]
        require(history and history[-1]["outcome"] in counts,
                "historical selected root remains unresolved")
        selected = history[-1]
        counts[selected["outcome"]] += 1
        result.append({
            "test": name,
            "selected_outcome": selected["outcome"],
            "selected_run": selected["run"],
            "selected_source_commit": selected["source_commit"],
            "selected_duration_seconds": selected["duration_seconds"],
            "observations": history,
        })
    require(counts == {"PASS": 712, "SKIP": 5}, "historical aggregate differs")
    derived = ("".join(json.dumps(row, ensure_ascii=False, separators=(",", ":")) + "\n"
                       for row in result)).encode("utf-8")
    require(derived == read_bytes(capsule / "ledger.jsonl"),
            "derived complete ledger differs from the published projection")
    if args.emit_ledger:
        sys.stdout.buffer.write(derived)
        sys.stdout.buffer.flush()
    else:
        print(json.dumps({
            "schema": "historical-cli717-data-verification-v1",
            "historical_aggregate_verified": True,
            "roots": len(result),
            **counts,
            "ledger_sha256": digest(derived),
            "single_full_run_pass": False,
            "mandatory_gate_qualification": False,
            "source_or_cache_reuse_authorized": False,
            "processes_started": False,
        }, indent=2))

if __name__ == "__main__":
    main()
