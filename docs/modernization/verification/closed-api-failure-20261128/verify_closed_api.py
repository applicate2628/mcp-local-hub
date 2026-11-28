"""Verify reconstructed CLOSED API evidence as DATA only.

Reads four frozen files, validates their original byte hashes and native closure,
and reproduces every terminal test outcome from original Go output. No Source,
test binary, compiler, shell, Git, network, or preserved program is executed.
"""
import argparse
from collections import Counter
from decimal import Decimal
import hashlib
import json
from pathlib import Path
import re

EXPECTED = {
    "receipt.json": (2338457, "2282b4b297e5d78f90da965383880674dd3f5529cfcfd6b9243cb56145927de8"),
    "namespace-suite-receipt.json": (5732, "2fe7149ad8fb358eba3aabae1d7d0d4e11a535b19622b0057f52ff7e034dd98b"),
    "suite-owner.stdout.txt": (1207113, "664ff94ef54d97d0088b74b0b669edc292589f1911b49798c71192aa79b17541"),
    "suite-owner.stderr.txt": (341, "0c954a4a1713930d9b849eaa415d7f195a799745a14df141f7cf71b81d614107"),
}
LEDGER_SHA256 = "9c5e0262be78f623ec0f744c721b0b856592796c00e93208526f793513c40001"
LEDGER_BYTES = 810017
PACKAGE = "mcp-local-hub/internal/api"
EXPECTED_FAILS = {
    "TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned_RealGit",
    "TestIsDeadGitWorktreePath_SubmoduleInsideWorktree_RealGit",
    "TestPreparedSerenaBuildConstraintCompactAttemptLifecycle",
    "TestPreparedSerenaFailureDiagnosticsOriginalCommand",
}
TERMINAL = re.compile(r"^\x16\s*--- (PASS|FAIL|SKIP): (.+) \((\d+(?:\.\d+)?)s\)$")
RUN = re.compile(r"^\x16=== RUN   (.+)$")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def original_file(root, name):
    path = root / name
    require(path.is_file() and not path.is_symlink(), "ordinary original file required: " + name)
    raw = path.read_bytes()
    size, expected = EXPECTED[name]
    require(len(raw) == size and digest(raw) == expected, "original bytes differ: " + name)
    return raw


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path,
                        help="The reconstructed closed-api directory.")
    parser.add_argument("--output", type=Path,
                        help="Optional new, absent directory for the derived ledger and verification result.")
    args = parser.parse_args()
    original = {name: original_file(args.root, name) for name in EXPECTED}
    api = json.loads(original["receipt.json"])
    owner = json.loads(original["namespace-suite-receipt.json"])
    require(api["status"] == "FAIL" and api["adapter_exit_code"] == 1,
            "historical API FAIL must remain unchanged")
    require(api["test_source_commit"] == "727d66753814dc218e26d699e57402db2d13190a"
            and api["product_source_commit"] == "3ebe335a2f666a5279d7fafc31a6f4dc3feedff4",
            "source identities differ")
    require(api["owner"]["proof"] == owner
            and api["owner"]["sha256"] == EXPECTED["namespace-suite-receipt.json"][1],
            "standalone namespace receipt is not the embedded original")
    require(api["native_custody_settled"] and api["commands_quiescent"]
            and api["command_quiescence"]["complete"], "API commands not physically settled")
    require(len(api["commands"]) == 8
            and all(c["quiescence_state"] == "complete" for c in api["commands"]),
            "original command closure differs")
    require(owner["root_wait_known"] and owner["root_exit_code"] == 1
            and owner["settlement_complete"] and owner["native_complete"]
            and owner["io_complete"] and not owner["custody_retained"],
            "original namespace closure differs")
    native = owner["native"]
    require(native["RootWaitKnown"] and native["RootExitCode"] == 1
            and native["MembersExited"] and native["AccountingKnown"]
            and native["FinalActive"] == 0 and native["HandlesClosed"]
            and native["TerminalAction"] == "not-requested"
            and native["Error"] is None, "native settlement not complete")
    for channel in ("Stdout", "Stderr"):
        stream = owner["io"][channel]
        require(stream["WorkerComplete"] and stream["EOF"]
                and not stream["LimitExceeded"] and stream["Error"] is None,
                "original stream not complete: " + channel)
    for channel in ("Stdin", "StdoutRead", "StdoutWrite", "StderrRead", "StderrWrite"):
        handle = owner["io"][channel]
        require(handle["Acquired"] and handle["CloseAttempted"] and handle["Closed"]
                and handle["CloseError"] is None, "original handle not closed: " + channel)

    stdout = original["suite-owner.stdout.txt"].decode("utf-8")
    rows, starts, timings = [], [], []
    for number, line in enumerate(stdout.split("\n"), 1):
        line = line.removesuffix("\r")
        matched = RUN.fullmatch(line)
        if matched:
            starts.append(matched.group(1))
        matched = TERMINAL.fullmatch(line)
        if not matched:
            continue
        outcome, name, seconds = matched.groups()
        rows.append({"package": PACKAGE, "test": name, "outcome": outcome.lower()})
        if "/" not in name:
            timings.append({"test": name, "outcome": outcome.lower(),
                            "elapsed_rounded_seconds": Decimal(seconds),
                            "stdout_line": number})
    require(len(rows) == 6275 and len({x["test"] for x in rows}) == 6275,
            "terminal ledger cardinality or uniqueness differs")
    ledger = sorted(rows, key=lambda row: row["test"])
    raw_ledger = (json.dumps(ledger, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8")
    require(len(raw_ledger) == LEDGER_BYTES and digest(raw_ledger) == LEDGER_SHA256,
            "reconstructed exact terminal ledger differs")
    coverage = api["coverage"][0]
    require(ledger == coverage["all_terminal"], "raw capture differs from primary coverage")
    roots = [row for row in ledger if "/" not in row["test"]]
    expected_roots = set(coverage["expected"][PACKAGE])
    require(len(roots) == 3523 and {row["test"] for row in roots} == expected_roots,
            "top-level inventory coverage differs")
    root_starts = [name for name in starts if "/" not in name]
    require(len(root_starts) == 3523 and set(root_starts) == expected_roots,
            "original top-level starts differ")
    require(Counter(x["outcome"] for x in roots) == {"pass": 3455, "skip": 64, "fail": 4},
            "top-level counts differ")
    require(Counter(x["outcome"] for x in ledger) == {"pass": 6189, "skip": 82, "fail": 4},
            "all terminal counts differ")
    require({row["test"] for row in ledger if row["outcome"] == "fail"} == EXPECTED_FAILS,
            "failure set changed")
    require(not coverage["started_without_terminal"] and not coverage["missing_top_level"]
            and not coverage["missing_package_terminal"]
            and coverage["package_terminal"][PACKAGE] == "fail"
            and coverage["mandatory_passes_satisfied"], "coverage qualifications differ")
    outcomes = {row["test"]: row["outcome"] for row in ledger}
    require(len(coverage["mandatory_passes"]) == 7
            and all(package == PACKAGE and outcomes[name] == "pass"
                    for package, name in coverage["mandatory_passes"]),
            "mandatory checks are not all PASS")
    require(sum((row["elapsed_rounded_seconds"] for row in timings), Decimal(0)) == Decimal("298.20"),
            "rounded timing total differs")
    require("panic: test timed out" not in stdout and "fatal error:" not in stdout,
            "unexpected panic or timeout text")
    result = {
        "schema": "closed-api-exact-data-verification-v1",
        "data_verification": "PASS",
        "historical_test_status": "FAIL",
        "physical_state": "CLOSED",
        "top_level": {"total": 3523, "pass": 3455, "skip": 64, "fail": 4},
        "including_subtests": {"total": 6275, "pass": 6189, "skip": 82, "fail": 4},
        "canonical_ledger_bytes": len(raw_ledger),
        "canonical_ledger_sha256": digest(raw_ledger),
        "rounded_top_level_seconds": "298.20",
        "timestamps_available": False,
        "new_native_or_test_execution": False,
        "full_run_qualification_claimed": False,
        "cache_or_process_authority_issued": False,
    }
    if args.output is not None:
        require(not args.output.exists() and not args.output.is_symlink(),
                "derived output must be a new absent directory")
        args.output.mkdir(parents=False, exist_ok=False)
        with (args.output / "all-terminal-outcomes.json").open("xb") as stream:
            stream.write(raw_ledger)
        with (args.output / "verification.json").open("xb") as stream:
            stream.write((json.dumps(result, indent=2) + "\n").encode("utf-8"))
    print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    main()
