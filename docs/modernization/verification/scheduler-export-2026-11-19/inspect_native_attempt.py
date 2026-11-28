"""Verify saved R17 records and write a new redacted report; no native calls."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import sys
import xml.etree.ElementTree as ET

RUN_ID = "d55a6763eb064445a46ef592d52a9e6f"
HEAD = "1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3"
FIXTURE_SHA = "6a7d123334ae13aaf5890a491813091a2a2a4831650d25940d1814ca40a2ee4c"
SOURCE_SHA = "e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa"
EXPECTED = {
    "intent.json": "eab630c66d1c377d7201f204c6b29f2fd6933a462e856936af4de5e2e5eb2d8d",
    "result.json": "c151564a1c5d873aa4f3197b91124289235b8d73e4d1d472f6f778afe0971833",
    f"mcp-r17-{RUN_ID}-visible.xml": "28affda1fe9d55a47e7bc5cbd9add47a041bc7e28c0e7d1da2b10f988b853c97",
    f"mcp-r17-{RUN_ID}-hidden.xml": "09348cba1f2219e842d78bab9c9d94bb8a46bd27e81cb37587900acdbdbbfd53",
    "status-visible.json": "7f1acf1e4659982ec7992fb4868fc476707d160bb9e9d25998a30a4bdefcc13b",
    "status-visible.stderr.txt": "2370f40cd9e212d09714414f67c671a9f624d59210af1cadae4922f5df21bba7",
}


def main(run_dir: Path, preparation_dir: Path, destination: Path) -> None:
    assert run_dir.name == RUN_ID
    blobs = {name: (run_dir / name).read_bytes() for name in EXPECTED}
    for name, digest in EXPECTED.items():
        assert hashlib.sha256(blobs[name]).hexdigest() == digest, name
    for name, digest in [("Invoke-R17HiddenFixture.ps1", FIXTURE_SHA),
                         ("scheduler_com_windows-1b870e04.go", SOURCE_SHA)]:
        assert hashlib.sha256((preparation_dir / name).read_bytes()).hexdigest() == digest
    intent = json.loads(blobs["intent.json"])
    result = json.loads(blobs["result.json"])
    response = json.loads(blobs["status-visible.json"])
    observation_bytes = (run_dir / "recovery-observation-01.json").read_bytes()
    observation = json.loads(observation_bytes)
    assert intent["run_id"] == RUN_ID and intent["head"] == result["head"] == HEAD
    assert intent["source_sha256"] == result["source_sha256"] == SOURCE_SHA
    assert result["reads_passed"] is False and result["cleanup_complete"] is False
    assert result["checks"] == [] and len(result["children"]) == 1
    child = result["children"][0]
    assert child["check"] == "status-visible" and child["settled"] is True
    assert child["exit_code"] == 0
    visible, hidden = result["tasks"]
    assert [t["name"] for t in (visible, hidden)] == intent["names"]
    assert [t["hidden"] for t in (visible, hidden)] == [False, True]
    assert all(t["attempted"] and t["created"] for t in (visible, hidden))
    assert visible["confirmed"] is False and visible["cleanup"] == "retained"
    assert hidden["confirmed"] is True and hidden["cleanup"] == "removed"
    for task in (visible, hidden):
        assert blobs[task["name"] + ".xml"].decode("utf-8") == task["xml"]
        assert intent["marker"] in task["xml"]
    assert response["ok"] is True and response["task"]["name"] == visible["name"]
    assert response["task"]["state"] == 1 and response["task"]["xml"] == visible["xml"]
    stderr = blobs["status-visible.stderr.txt"].decode("utf-8")
    header, separator, payload = stderr.partition("\n")
    assert separator and header in ("#< CLIXML", "#< CLIXML\r")
    tree = ET.fromstring(payload)
    ns = {"p": "http://schemas.microsoft.com/powershell/2004/04"}
    objects = list(tree)
    assert len(objects) == 1 and objects[0].attrib["S"] == "progress"
    assert objects[0].find("p:MS/p:PR/p:T", ns).text == "Completed"
    assert observation["read_only"] and observation["hidden_inclusive_enumeration_completed"]
    assert observation["current_user_matches_intent"] and observation["saved_status_owner_matches"]
    current, absent = observation["tasks"]
    assert current["name"] == visible["name"] and current["observed_count"] == 1
    for key in ("exact_path", "same_xml", "same_principal", "task_disabled", "settings_disabled",
                "demand_start_disabled", "marker_matches", "uri_matches", "action_matches",
                "empty_arguments", "action_file_absent"):
        assert current[key] is True, key
    assert (current["state"], current["trigger_count"], current["logon_type"],
            current["run_level"], current["action_count"], current["action_type"]) == (1, 0, 3, 0, 1, 0)
    assert current["hidden_setting"] is False
    assert absent["name"] == hidden["name"] and absent["observed_absent"] and absent["observed_count"] == 0
    report = {
        "schema_version": 1, "run_id": RUN_ID, "source_head": HEAD,
        "executed_fixture_sha256": FIXTURE_SHA, "source_sha256": SOURCE_SHA,
        "authorization": "One user-approved Apply invocation with session RemoteSigned; consumed",
        "native_qualification_passed": False, "accepted_checks": result["checks"],
        "cleanup_complete": False, "recorded_failure": result["error"],
        "first_child": {k: child[k] for k in ("check", "started_utc", "settled", "exit_code")},
        "saved_first_response": {"ok": True, "exact_name_state_xml": True,
                                 "owner_normalized_in_read_only_observation": True,
                                 "accepted_by_original_fixture": False},
        "stderr": {"format": "CLIXML", "header_line_ending": "CRLF" if header.endswith("\r") else "LF",
                   "top_level_record_count": 1,
                   "record_streams": ["progress"], "progress_state": "Completed",
                   "activity_meaning": "Preparing modules for first use"},
        "tasks": [{k: t[k] for k in ("name", "hidden", "attempted", "created", "confirmed", "cleanup")}
                  for t in (visible, hidden)],
        "saved_files": [{"name": n, "bytes": len(blobs[n]), "sha256": h} for n, h in EXPECTED.items()],
        "read_only_observation": observation,
        "observation_sha256": hashlib.sha256(observation_bytes).hexdigest(),
        "analyzer_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "limits": ["Captured UTF-8 text files, not a new native replay or raw pipe-byte capture",
                   "Private XML and principal identifiers are omitted; raw records remain local",
                   "The first saved answer is an observation, not a passed seven-check suite",
                   "Current-state observation is not atomic deletion authority",
                   "Separate recovery and another native Apply are not yet authorized"],
    }
    with destination.open("x", encoding="utf-8", newline="\n") as stream:
        json.dump(report, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    print(json.dumps({"saved": str(destination), "native_qualification_passed": False,
                      "retained_task": visible["name"], "hidden_observed_absent": True}))


if __name__ == "__main__":
    main(*(Path(arg) for arg in sys.argv[1:]))
