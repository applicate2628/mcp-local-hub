"""Verify saved R17 recovery/V2 evidence; no subprocess or Scheduler calls.

Arguments: old run directory, V2 run directory, preparation directory, new report.
The executor separately verifies approval and launch order; timestamps alone do
not prove that the recovery receipt was read before launching V2.
"""
from __future__ import annotations

from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import sys
import xml.etree.ElementTree as ET

OLD = "d55a6763eb064445a46ef592d52a9e6f"
NEW = "68c2db3e983a41f9bded8e068e88bab8"
HEAD = "1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3"
SOURCE = "e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa"
PROGRESS = "2370f40cd9e212d09714414f67c671a9f624d59210af1cadae4922f5df21bba7"
CHECKS = ["status-visible", "export-visible", "status-hidden", "export-hidden",
          "list-visible-only", "status-absent", "export-absent"]
PREPARATION = {
    "Recover-R17VisibleFixture.ps1": "3e0f68e2858d6a214a2ecd14db26a973d92149c553275df2e3a323b06a1a0c89",
    "Invoke-R17HiddenFixture-v2.ps1": "8118e1c02937dbf27a9aaa12f63f4e34a91b0ce3e7ad675b4e1758960c9cb010",
    "Invoke-R17HiddenFixture.ps1": "6a7d123334ae13aaf5890a491813091a2a2a4831650d25940d1814ca40a2ee4c",
    "scheduler_com_windows-1b870e04.go": SOURCE,
}
HISTORY = {
    "intent.json": "eab630c66d1c377d7201f204c6b29f2fd6933a462e856936af4de5e2e5eb2d8d",
    "result.json": "c151564a1c5d873aa4f3197b91124289235b8d73e4d1d472f6f778afe0971833",
    f"mcp-r17-{OLD}-visible.xml": "28affda1fe9d55a47e7bc5cbd9add47a041bc7e28c0e7d1da2b10f988b853c97",
    f"mcp-r17-{OLD}-hidden.xml": "09348cba1f2219e842d78bab9c9d94bb8a46bd27e81cb37587900acdbdbbfd53",
    "status-visible.json": "7f1acf1e4659982ec7992fb4868fc476707d160bb9e9d25998a30a4bdefcc13b",
    "status-visible.stderr.txt": PROGRESS,
    "recovery-visible-intent.json": "12ef4dcc3619a7672ffeb9d184c1de71821ee1f11bdbcedfca99d7e1d8ebc1e6",
    "recovery-visible-result.json": "144796b7bb6c8666a3449b7389b0b891df296796ce5178eeecacba3f823b4f56",
}
CURRENT = {
    "intent.json": "52274ba812a9736ec186d7b71200a03bb9226be456bd5a5769b204436f83be74",
    "result.json": "e7bd76b95ac98b1548162529a06ee4d5674c4cd5449b0ea2a11f5df452b95136",
    f"mcp-r17-{NEW}-visible.xml": "893942a4df7f409c9c65b7b739006ccb40ab1b7eb03bdfa455433242d556c0f2",
    f"mcp-r17-{NEW}-hidden.xml": "369a2f938a41480f89faf3f6e3fd5f9592714f26e25e192754adc4defd670795",
    "status-visible.json": "9d80a5c9d9be48988cc50b703687d9f652e92a1855f165f3c5f8662a56b50ad3",
    "export-visible.json": "9d80a5c9d9be48988cc50b703687d9f652e92a1855f165f3c5f8662a56b50ad3",
    "status-hidden.json": "23d2b3f52fe7409b23258de32c4169fec5f09152901ad047b1ad93fd2eb700dc",
    "export-hidden.json": "23d2b3f52fe7409b23258de32c4169fec5f09152901ad047b1ad93fd2eb700dc",
    "list-visible-only.json": "0dbdd20752c9dfd2ed9a667eec94befda1340b605ee04e875d05228e77282204",
    "status-absent.json": "13b6bdc9a30f4989a7fc5a2928381ed08210b2d6d332c5355f9e90fd7c3906e2",
    "export-absent.json": "13b6bdc9a30f4989a7fc5a2928381ed08210b2d6d332c5355f9e90fd7c3906e2",
    "native-v2-observation.json": "e148779f337234e2f7b57c77f5e8ab32e57a151783dc0a01ff0bcfda77107078",
    **{label + ".stderr.txt": PROGRESS for label in CHECKS},
}


def verified_bytes(directory: Path, expected: dict[str, str]) -> dict[str, bytes]:
    blobs = {name: (directory / name).read_bytes() for name in expected}
    for name, digest in expected.items():
        assert hashlib.sha256(blobs[name]).hexdigest() == digest, name
    return blobs


def catalog(blobs: dict[str, bytes]) -> list[dict]:
    return [{"name": name, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
            for name, data in blobs.items()]


def main(old_dir: Path, new_dir: Path, preparation: Path, destination: Path) -> None:
    assert old_dir.name == OLD and new_dir.name == NEW and OLD != NEW
    prepared = verified_bytes(preparation, PREPARATION)
    old = verified_bytes(old_dir, HISTORY)
    new = verified_bytes(new_dir, CURRENT)
    original = json.loads(old["result.json"])
    old_intent = json.loads(old["intent.json"])
    recovery_intent = json.loads(old["recovery-visible-intent.json"])
    recovery = json.loads(old["recovery-visible-result.json"])
    intent, result = (json.loads(new[name]) for name in ("intent.json", "result.json"))
    observation = json.loads(new["native-v2-observation.json"])
    assert original["checks"] == [] and original["reads_passed"] is False
    assert original["cleanup_complete"] is False
    assert original["tasks"][0]["confirmed"] is False
    assert original["tasks"][0]["cleanup"] == "retained"
    assert original["tasks"][1]["cleanup"] == "removed"
    for record in (original, old_intent, recovery_intent, recovery, intent, result):
        assert record["head"] == HEAD and record["source_sha256"] == SOURCE
    for record in (recovery_intent, recovery):
        assert record["task_path"] == "\\mcp-r17-" + OLD + "-visible"
        assert record["original_fixture_sha256"] == PREPARATION["Invoke-R17HiddenFixture.ps1"]
    pinned = {name: HISTORY[name] for name in ("intent.json", "result.json",
              f"mcp-r17-{OLD}-visible.xml", "status-visible.json", "status-visible.stderr.txt")}
    assert recovery_intent["expected_files"] == pinned and recovery_intent["run_id"] == OLD
    assert recovery["delete_attempted"] is True and recovery["absence_observed"] is True
    assert recovery["state"] == "ABSENCE_OBSERVED" and recovery["error"] is None
    assert result["reads_passed"] is True and result["cleanup_complete"] is True
    assert result["error"] is None and result["checks"] == CHECKS
    assert len(result["children"]) == 7
    assert [child["check"] for child in result["children"]] == CHECKS
    assert all(child["settled"] is True and child["exit_code"] == 0 for child in result["children"])
    tasks = result["tasks"]
    expected_names = [f"mcp-r17-{NEW}-{suffix}" for suffix in ("visible", "hidden")]
    assert intent["run_id"] == NEW and intent["names"] == expected_names
    assert len(tasks) == 2 and [task["name"] for task in tasks] == expected_names
    assert [task["hidden"] for task in tasks] == [False, True]
    assert intent["marker"] == f"R17 #660 {NEW} Ж§"
    ns = {"t": "http://schemas.microsoft.com/windows/2004/02/mit/task"}
    for task in tasks:
        assert all(task[key] is True for key in ("attempted", "created", "confirmed"))
        assert task["cleanup"] == "removed" and task["error"] is None
        assert new[task["name"] + ".xml"].decode("utf-8") == task["xml"]
        xml = ET.fromstring(task["xml"])
        assert xml.findtext("t:RegistrationInfo/t:Description", namespaces=ns) == intent["marker"]
        assert xml.findtext("t:RegistrationInfo/t:URI", namespaces=ns) == "\\" + task["name"]
    verified_responses = []
    for label in CHECKS:
        response = json.loads(new[label + ".json"])
        if label.endswith("-absent"):
            assert response["ok"] is False and response["kind"] == "task_absent"
            verified_responses.append({"check": label, "ok": False, "kind": "task_absent"})
            continue
        assert response["ok"] is True
        if label == "list-visible-only":
            assert len(response["tasks"]) == 1
            row, expected = response["tasks"][0], tasks[0]
        else:
            row, expected = response["task"], tasks[1 if label.endswith("-hidden") else 0]
        assert row["name"] == expected["name"] and row["state"] == 1
        assert row["xml"] == expected["xml"]
        verified_responses.append({"check": label, "ok": True, "exact_name_state_xml": True,
                                   "marker_preserved": intent["marker"] in row["xml"]})
    stderr = new["status-visible.stderr.txt"].decode("utf-8")
    header, separator, payload = stderr.partition("\n")
    assert separator and header == "#< CLIXML\r"
    progress = list(ET.fromstring(payload))
    pns = {"p": "http://schemas.microsoft.com/powershell/2004/04"}
    assert len(progress) == 1 and progress[0].attrib["S"] == "progress"
    assert progress[0].findtext("p:MS/p:PR/p:T", namespaces=pns) == "Completed"
    all_names = old_intent["names"] + intent["names"]
    assert observation["scheduler_query"] == "GetTasks(1)" and observation["mutates_scheduler"] is False
    assert [item["name"] for item in observation["names"]] == all_names and len(set(all_names)) == 4
    assert observation["all_four_absent"] is True and all(item["count"] == 0 for item in observation["names"])
    assert observation["current_principal_matches_old_intent"] is True
    assert observation["current_principal_matches_new_intent"] is True
    assert [item["check"] for item in observation["response_principals"]] == CHECKS[:5]
    assert all(item["principal_matches_intent"] is True for item in observation["response_principals"])
    parse_time = datetime.fromisoformat
    assert parse_time(recovery_intent["started_utc"]) < parse_time(recovery["completed_utc"])
    assert parse_time(recovery["completed_utc"]) < parse_time(result["children"][0]["started_utc"])
    assert parse_time(result["children"][-1]["started_utc"]) < parse_time(observation["observed_utc"])
    report = {
        "schema_version": 1, "analyzed_utc": datetime.now(timezone.utc).isoformat(),
        "old_run_id": OLD, "run_id": NEW, "source_head": HEAD, "source_sha256": SOURCE,
        "recovery_sha256": PREPARATION["Recover-R17VisibleFixture.ps1"],
        "v2_fixture_sha256": PREPARATION["Invoke-R17HiddenFixture-v2.ps1"],
        "recovery": {key: recovery[key] for key in ("task_path", "state", "delete_attempted",
                                                    "absence_observed", "completed_utc", "error")},
        "original_failure_unchanged": True, "original_checks": original["checks"],
        "native_script_checks_passed": True, "cleanup_complete": True,
        "accepted_checks": result["checks"], "responses": verified_responses,
        "children": [{key: child[key] for key in ("check", "started_utc", "settled", "exit_code")}
                     for child in result["children"]],
        "tasks": [{key: task[key] for key in ("name", "hidden", "attempted", "created", "confirmed", "cleanup")}
                  for task in tasks],
        "stderr": {"files": 7, "each_bytes": len(new["status-visible.stderr.txt"]),
                   "each_sha256": PROGRESS, "same_as_original": True, "format": "CLIXML",
                   "header_line_ending": "CRLF", "records_per_file": 1,
                   "stream": "progress", "state": "Completed"},
        "read_only_observation": observation,
        "files": {"preparation": catalog(prepared), "history_and_recovery": catalog(old),
                  "v2": catalog(new)},
        "analyzer_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "limits": ["Only the unchanged extracted PowerShell script from #660, via EncodedCommand",
                   "Public Go backend and full R17 qualification remain open",
                   "Outer commands finished; numeric outer shell exit codes were not exposed by the tool",
                   "Only seven child exit codes are recorded as zero",
                   "Approval, one-shot invocation counts and receipt-read ordering come from executor records",
                   "Saved UTF-8 text follows .NET decoding; this is not a raw pipe-byte capture",
                   "Private XML and principal values remain local; published data cannot fully reconstruct them",
                   "No task actions, RunOwned, Stop or production Delete were qualified",
                   "Absence is observed at the recorded time; compare and delete are not atomic",
                   "Full Go tests retain their earlier FAIL results; no new Go test run"],
    }
    with destination.open("x", encoding="utf-8", newline="\n") as stream:
        json.dump(report, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    print(json.dumps({"saved": str(destination), "native_script_checks_passed": True,
                      "accepted_check_count": 7, "cleanup_complete": True,
                      "all_four_absent_at_observation": True}))


if __name__ == "__main__":
    main(*(Path(arg) for arg in sys.argv[1:]))
