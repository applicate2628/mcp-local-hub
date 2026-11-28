"""Read-only DATA checks for the existing prestart-failure receipt receiver.

Usage: python prestart_receiver_check.py RUNNER.py PLAN.json EXECUTION.json
No controller module is imported, no native process is started, and no command
capture is read. Only three selected pure receiver functions are compiled.
Mutated fixtures remain in memory and are never written as execution receipts.
"""

import ast
import copy
import hashlib
import json
from pathlib import Path
import sys


def check(runner_bytes, plan_bytes, execution_bytes):
    plan = json.loads(plan_bytes)
    execution = json.loads(execution_bytes)
    pins = {
        "runner_sha256": hashlib.sha256(runner_bytes).hexdigest(),
        "plan_sha256": hashlib.sha256(plan_bytes).hexdigest(),
        "execution_sha256": hashlib.sha256(execution_bytes).hexdigest(),
    }
    assert pins["runner_sha256"] == plan["payload"]["runner"]
    assert pins["plan_sha256"] == execution["plan_sha256"]
    assert execution["payload"] == plan["payload"]
    assert len(execution["commands"]) == 1
    assert execution["commands"][0]["lifecycle_state"] == "CLOSED_PRESTART_FAIL"

    names = {"require", "receive_prestart_failure", "receive_execution"}
    nodes = [node for node in ast.parse(runner_bytes).body
             if isinstance(node, ast.FunctionDef) and node.name in names]
    assert len(nodes) == len(names)

    def no_io(*args, **kwargs):
        raise AssertionError("DATA receiver attempted capture I/O")

    namespace = {"SCHEMA": "fresh-owned-commands-v1", "Path": no_io, "sha": no_io}
    exec(compile(ast.Module(body=nodes, type_ignores=[]),
                 "<exact-pure-receiver-functions>", "exec"), namespace)
    receive = namespace["receive_execution"]
    assert receive(plan, execution) == "FAIL"

    def bind_proof(row):
        proof = row["completion_receiving"]
        proof["native"] = copy.deepcopy(row["native"])
        proof["process_creation"] = copy.deepcopy(row["process_creation"])

    accepted = []
    for code in [0, 3221684229, 0xffffffff]:
        fixture = copy.deepcopy(execution)
        row = fixture["commands"][0]
        row["process_creation"]["win32_last_error"] = code
        bind_proof(row)
        assert receive(plan, fixture) == "FAIL"
        accepted.append(code)

    cases = [
        ("old_signed_error", lambda r, s: r["process_creation"].update(win32_last_error=-1073283067)),
        ("error_above_uint32", lambda r, s: r["process_creation"].update(win32_last_error=0x100000000)),
        ("boolean_error", lambda r, s: r["process_creation"].update(win32_last_error=True)),
        ("missing_creation_fact", lambda r, s: r["process_creation"].pop("attempted")),
        ("creation_not_attempted", lambda r, s: r["process_creation"].update(attempted=False)),
        ("process_was_created", lambda r, s: r["process_creation"].update(created=True)),
        ("unclosed_handle", lambda r, s: r["native"].update(handles_closed=False)),
        ("nonempty_job", lambda r, s: r["native"].update(active_zero=False)),
        ("native_error", lambda r, s: r["native"].update(issues=["WJOB-HANDLE-CLOSE"])),
        ("invented_exit", lambda r, s: r.update(exit_code=0)),
        ("invented_pid", lambda r, s: r.update(original_pid=123)),
        ("readers_started", lambda r, s: r.update(readers="STARTED")),
        ("capture_measured", lambda r, s: r.update(captures_measured=True)),
        ("resumed_command", lambda r, s: r.update(before_resume_entered=True)),
        ("unresolved_state", lambda r, s: s.update(state="unknown")),
    ]
    rejected = []
    for name, mutate in cases:
        fixture = copy.deepcopy(execution)
        row = fixture["commands"][0]
        state = fixture["command_quiescence"]["commands"][0]
        mutate(row, state)
        bind_proof(row)
        try:
            receive(plan, fixture)
        except RuntimeError:
            rejected.append(name)
        else:
            raise AssertionError("invalid fixture accepted: " + name)

    direct = namespace["receive_prestart_failure"]
    for name, status, is_last in [
        ("false_success", "PASS", True),
        ("nonterminal_position", "FAIL", False),
        ("mismatched_proof", "FAIL", True),
    ]:
        fixture = copy.deepcopy(execution)
        row = fixture["commands"][0]
        state = fixture["command_quiescence"]["commands"][0]
        if name == "mismatched_proof":
            row["completion_receiving"]["original_owner_retained"] = True
        try:
            direct(row, state, status, is_last)
        except RuntimeError:
            rejected.append(name)
        else:
            raise AssertionError("invalid fixture accepted: " + name)

    return {
        "scope": "DATA-only receiver checks; no native execution or capture I/O",
        **pins,
        "original_receipt_received_as": "FAIL",
        "accepted_synthetic_closed_failure_codes": accepted,
        "rejected_cases": rejected,
        "result": "PASS",
    }


if __name__ == "__main__":
    if len(sys.argv) != 4:
        raise SystemExit("usage: prestart_receiver_check.py RUNNER.py PLAN.json EXECUTION.json")
    inputs = [Path(name).read_bytes() for name in sys.argv[1:]]
    print(json.dumps(check(*inputs), indent=2))
