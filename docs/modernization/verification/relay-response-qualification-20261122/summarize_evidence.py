"""Derive a publication summary from retained bytes, never from claimed PASS text."""
from pathlib import Path
import collections
import hashlib
import json
import shutil
import tarfile

ROOT = Path(__file__).resolve().parent
RAW = ROOT / "raw-evidence"
OUT = ROOT / "publish-ready"
OUT.mkdir(exist_ok=True)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def save(name, value):
    (OUT / name).write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")


inventory = json.loads((RAW / "archive-inventory.json").read_text())
with tarfile.open(ROOT / "relay-evidence.tar.xz") as archive:
    members = archive.getmembers()
    assert len(members) == len(inventory) + 1 == 220
    assert all(item.isfile() for item in members)
    assert {item.name for item in members} == set(inventory) | {"archive-inventory.json"}
for name, row in inventory.items():
    raw = (RAW / name).read_bytes()
    assert len(raw) == row["bytes"] and sha(raw) == row["sha256"], name
copies = json.loads((ROOT / "historical-receipt-copy-verification.json").read_text())
assert len(copies) == 12 and all(r["equal"] for r in copies)

variants = {}
for variant in ("candidate", "control", "prime-control"):
    path = RAW / "historical-input" / ("relay-v2-" + variant) / "manifest.json"
    manifest = json.loads(path.read_text())
    variants[variant] = {"manifest_sha256": sha(path.read_bytes()), "files": manifest["files"]}
for variant in ("control", "prime-control"):
    old = {x["path"]: x for x in variants[variant]["files"]}
    for row in variants["candidate"]["files"]:
        if row["path"] != "internal/daemon/relay.go":
            assert row == old[row["path"]]

leaves = {}
for record in (RAW / "evidence/prepare/source-tree.stdout.log").read_bytes().split(b"\0"):
    if not record:
        continue
    metadata, raw_path = record.split(b"\t", 1)
    mode, kind, blob = metadata.decode().split()
    assert kind == "blob"
    leaves[raw_path.decode()] = (mode, blob)
for row in variants["candidate"]["files"]:
    name = row["path"]
    leaves[name] = (leaves.get(name, ("100644", None))[0], row["candidate_git_blob"])
composition = json.loads((RAW / "composition.json").read_text())
assert set(leaves) == set(composition["files"])


def git_tree(items):
    directories, blobs = {}, {}
    for name, value in items.items():
        head, separator, tail = name.partition("/")
        if separator:
            directories.setdefault(head, {})[tail] = value
        else:
            blobs[head] = value
    records = [(name, False, mode, blob) for name, (mode, blob) in blobs.items()]
    records += [(name, True, "40000", git_tree(children)) for name, children in directories.items()]
    records.sort(key=lambda x: (x[0] + ("/" if x[1] else "")).encode())
    raw = b"".join(mode.encode() + b" " + name.encode() + b"\0" + bytes.fromhex(blob) for name, _, mode, blob in records)
    return hashlib.sha1(b"tree " + str(len(raw)).encode() + b"\0" + raw).hexdigest()


assert len(leaves) == 3293 and git_tree(leaves) == "fcd2ad6bce89c71a180973c8f0a65b912899fdc5"
for row in json.loads((ROOT / "candidate-pins.json").read_text())["files"]:
    raw = (ROOT / "candidate" / row["path"]).read_bytes()
    assert sha(raw) == row["sha256"] and len(raw) == row["bytes"]


def command_record(directory, command, historical):
    label = command["label"]
    row = {k: command[k] for k in ("label", "exit_code", "seconds", "started_utc", "finished_utc", "settled", "timed_out", "stdout", "stderr")}
    row["argv"] = command.get("argv", [command.get("executable"), *command.get("args", [])])
    for stream in ("stdout", "stderr"):
        raw = (directory / (label + "." + stream + ".log")).read_bytes()
        assert len(raw) == command[stream]["bytes"] and sha(raw) == command[stream]["sha256"]
        if stream == "stdout" and "-json" in row["argv"]:
            events = [json.loads(line) for line in raw.splitlines()]
            terminal = {action: [e.get("Test", "PACKAGE:" + e.get("Package", "")) for e in events if e.get("Action") == action] for action in ("pass", "fail", "skip")}
            if "tests" in command:
                assert terminal == command["tests"]
            row["terminal_events"] = terminal
            row["root_counts"] = {action: dict(collections.Counter(name for name in names if "/" not in name and not name.startswith("PACKAGE:"))) for action, names in terminal.items()}
            row["event_counts"] = {action: len(names) for action, names in terminal.items()}
            row["failure_assertions"] = [e["Output"].strip() for e in events if e.get("Action") == "output" and ".go:" in e.get("Output", "")]
            target = OUT / "logs" / (label + ".stdout.log")
            target.parent.mkdir(exist_ok=True)
            shutil.copyfile(directory / (label + ".stdout.log"), target)
            row["raw_stdout"] = {"archive": "relay-evidence.tar.xz", "member": (directory / (label + ".stdout.log")).relative_to(RAW).as_posix()}
    assert command["settled"] is True and command["timed_out"] is False
    if historical:
        oracle = json.loads((directory / (label + ".oracle.json")).read_text())
        assert oracle["matched"] is True
        row["frozen_oracle_matched"] = True
        row["historical_evidence_reused_without_rerun"] = True
    return row


phases = []
for directory in sorted((RAW / "history").iterdir()):
    receipt = json.loads((directory / "receipt.json").read_text())
    assert receipt["completed"] is True and receipt["state"] == "complete"
    assert receipt["source_before"] == receipt["source_after"] == receipt["source_final"]
    for field in ("runner_sha256", "manifest_sha256", "tools_sha256"):
        assert receipt[field] == receipt[field + "_after"]
    phases.append({"phase": directory.name, "historical": True,
        "receipt_sha256": sha((directory / "receipt.json").read_bytes()),
        "source": receipt["base"], "tree": receipt["base_tree"],
        "manifest_sha256": receipt["manifest_sha256"], "runner_sha256": receipt["runner_sha256"],
        "source_snapshot_sha256": sha(canonical(receipt["source_before"])),
        "source_unchanged": True, "tools_unchanged": True,
        "commands": [command_record(directory, c, True) for c in receipt["commands"]]})
for phase in ("neighbors", "build", "vet"):
    directory = RAW / "evidence" / phase
    summary = json.loads((directory / "summary.json").read_text())
    assert summary["source_unchanged"] is True
    command = json.loads((directory / (phase + ".receipt.json")).read_text())
    assert command["exit_code"] == 0
    format_check = json.loads((directory / "format.receipt.json").read_text())
    assert format_check["exit_code"] == 0 and format_check["stdout"]["bytes"] == 0
    phases.append({"phase": phase, "historical": False, "source_unchanged": True,
        "composition_sha256": summary["composition_sha256"],
        "format_exit": 0, "format_stdout_bytes": 0,
        "commands": [command_record(directory, command, False)]})

contract = next(p for p in phases if p["phase"] == "relay-v2-contract")["commands"][-1]
assert len(contract["root_counts"]["pass"]) == 25
assert set(contract["root_counts"]["pass"].values()) == {2}
assert contract["event_counts"] == {"pass": 205, "fail": 0, "skip": 0}
neighbors = next(p for p in phases if p["phase"] == "neighbors")["commands"][0]
assert len(neighbors["root_counts"]["pass"]) == 17
assert set(neighbors["root_counts"]["pass"].values()) == {1}
assert neighbors["event_counts"] == {"pass": 37, "fail": 0, "skip": 0}

save("qualification-results.json", {
    "schema": "relay-response-qualification-20261122-v1",
    "status": "SCOPED_QUALIFICATION_COMPLETE; publication and exact-published-SHA review belong to root",
    "candidate": json.loads((ROOT / "candidate-pins.json").read_text()),
    "source": {"materialized_checkpoint": "858812988833a84368c60fba1540ae601155fa71",
        "base_tree": "dba685c090952a93a80a1e82403c4646ad8aaed1",
        "composed_tree": "fcd2ad6bce89c71a180973c8f0a65b912899fdc5",
        "composed_files": 3293, "overlays": 16,
        "composition_manifest_sha256": sha((RAW / "composition.json").read_bytes()),
        "scope": "master-preserved composition plus complete donor paths, canonical daemon entry and candidate; not a standalone PR629 or current-master test"},
    "counts": {"contract_root_tests": 25, "contract_repetitions": 2,
        "contract_test_and_subtest_pass_events": 204, "contract_package_pass_events": 1,
        "neighbors_root_tests": 17, "neighbors_repetitions": 1,
        "neighbors_test_and_subtest_pass_events": 36, "neighbors_package_pass_events": 1},
    "raw_archive": {"name": "relay-evidence.tar.xz", "bytes": 347240,
        "sha256": "58e91f427ed92e02dbbaca74d5fdca07eeed3e84a002da01f9934415d622fae3",
        "indexed_data_files": len(inventory), "inventory_files": 1,
        "total_regular_entries": len(inventory) + 1},
    "whole_process_tree_deadline_enforced": False,
    "process_tree_settlement_observed": False,
    "full_go_test_gate": "NOT_RUN",
    "native_cli_fixture_tests": "NOT_RUN (compile only)",
    "real_get_sse_sdk_consumer_profile": "NOT_RUN",
    "installed_hub_scheduler_credentials_operations": "NOT_RUN",
    "phases": phases,
})
save("input-pins.json", {"schema": "relay-response-input-pins-20261122-v1", "variants": variants,
    "new_runner_sha256": sha((ROOT / "qualify_relay.py").read_bytes()),
    "collector_sha256": sha((ROOT / "collect_evidence.py").read_bytes()),
    "new_environment_and_tools": json.loads((RAW / "evidence/neighbors/input.json").read_text()),
    "historical_receipts_byte_equal": copies})
print(json.dumps({"phases": len(phases), "raw_log_files": len(list((OUT / "logs").glob("*.log"))),
    "contract": contract["event_counts"], "neighbors": neighbors["event_counts"]}))
