"""Recompute retained R17 export evidence; read files only, never call Scheduler.

Usage: python -I -B verify_saved_native_export.py SAVED_WORK NEW_REPORT_JSON
Raw task definitions, names, account identifiers and localized errors stay local.
"""
import base64
import datetime as dt
import hashlib
import json
from pathlib import Path
import sys
import xml.etree.ElementTree as ET


def require(value, label):
    if not value:
        raise ValueError(label)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def read_response_xml(raw):
    response = json.loads(raw)
    require(response.get("ok") is True, "unsuccessful XML response")
    return base64.b64decode(response["xml_utf16le_base64"], validate=True).decode(
        "utf-16-le", errors="strict"
    ).encode("utf-8", errors="strict")


def verify_run(root, folder, runner, expected_runner, labels):
    directory = root / folder
    source = (root / runner).read_bytes()
    receipt_bytes = (directory / "receipt.json").read_bytes()
    receipt = json.loads(receipt_bytes)
    require(digest(source) == expected_runner == receipt["runner_sha256"], "runner mismatch")
    require([c["label"] for c in receipt["commands"]] == labels, "command set mismatch")
    require(receipt["task_mutations"] == [], "receipt records a task mutation")
    commands = []
    for command in receipt["commands"]:
        require(command["process_outcome"] == "completed" and command["returncode"] == 0,
                "historical process did not complete successfully")
        streams = {}
        for stream in ("stdout", "stderr"):
            raw = (directory / (command["label"] + "." + stream + ".bin")).read_bytes()
            observed = {"bytes": len(raw), "sha256": digest(raw)}
            require(observed == command[stream], "stream mismatch")
            if stream == "stderr":
                require(not raw, "historical stderr was not empty")
            streams[stream] = observed
        commands.append({key: command[key] for key in (
            "label", "started_utc", "finished_utc", "seconds", "returncode", "process_outcome"
        )} | streams)
    return directory, receipt, {
        "label": folder, "receipt_sha256": digest(receipt_bytes),
        "receipt_bytes": len(receipt_bytes), "runner_sha256": digest(source),
        "commands": commands,
    }


def verify(root):
    direct_dir, direct_receipt, direct_run = verify_run(
        root, "known-task-query", "probe_export_readonly.py",
        "1f9cb276c26e7d11697259fc56d0b96c729eaee30f36c1d474f63a08f1795845",
        ["com-before", "schtasks-query-hresult", "com-after"],
    )
    cim_dir, cim_receipt, cim_run = verify_run(
        root, "cim-query", "probe_cim_readonly.py",
        "1b61dae22318dc5d27fee58b3efbc80f9584afe4aaa03cb07ffd47f169042258",
        ["com-before", "cim-export", "com-after"],
    )
    snapshots = []
    for directory in (direct_dir, cim_dir):
        for label in ("com-before", "com-after"):
            value = read_response_xml((directory / (label + ".stdout.bin")).read_bytes())
            require(value == (directory / (label + ".xml.utf8.private")).read_bytes(), "copy mismatch")
            snapshots.append(value)
    require(all(value == snapshots[0] for value in snapshots), "observed COM drift")
    original = snapshots[0]
    original_text = original.decode("utf-8", errors="strict")
    original_tree = ET.fromstring(original_text)
    query = (direct_dir / "schtasks-query-hresult.stdout.bin").read_bytes()
    query_text = query.decode("utf-8", errors="strict")
    require(original_text.count("\u00a7") == 1 and query_text.count("\x15") == 1, "character mismatch")
    require(original_text.count("\r\n") == 36 and query_text.count("\r\r\n") == 36, "newline mismatch")
    require(original_text.replace("\u00a7", "\x15").replace("\r\n", "\r\r\n") == query_text,
            "transformation does not explain all bytes")
    try:
        ET.fromstring(query_text)
    except ET.ParseError as error:
        query_error = {"code": error.code, "position": list(error.position)}
    else:
        raise ValueError("direct query unexpectedly parses")

    response = json.loads((cim_dir / "cim-export.stdout.bin").read_bytes())
    require(response.get("ok") is True, "CIM envelope failed")
    require(len(response["rows"]) == 3, "unexpected CIM count")
    rows = {row["label"]: row for row in response["rows"]}
    require(set(rows) == {"exact", "case-variant", "previous-missing-name"}, "unexpected CIM set")
    for label in ("exact", "case-variant"):
        require(rows[label]["output_count"] == 1, "CIM result count")
    exact = read_response_xml(json.dumps(rows["exact"]))
    case = read_response_xml(json.dumps(rows["case-variant"]))
    require(exact == original == (cim_dir / "exact.xml.utf8.private").read_bytes(), "CIM exact mismatch")
    require(case == (cim_dir / "case-variant.xml.utf8.private").read_bytes(), "CIM case copy mismatch")
    request_names = {row["label"]: row["name"] for row in cim_receipt["cases"]}
    old_name = (cim_receipt["task_path"] + request_names["exact"]).encode("utf-8")
    upper_name = (cim_receipt["task_path"] + request_names["case-variant"]).encode("utf-8")
    require(original.count(old_name) == 1 and original.replace(old_name, upper_name) == case,
            "case transformation incomplete")
    namespace = {"t": "http://schemas.microsoft.com/windows/2004/02/mit/task"}
    case_tree = ET.fromstring(case.decode("utf-8"))
    require(original_tree.findtext("t:RegistrationInfo/t:URI", namespaces=namespace) == old_name.decode(),
            "original URI mismatch")
    require(case_tree.findtext("t:RegistrationInfo/t:URI", namespaces=namespace) == upper_name.decode(),
            "case URI mismatch")
    negative = rows["previous-missing-name"]
    require(negative["ok"] is False and negative["phase"] == "export", "negative outcome mismatch")
    error_codes = [p["numeric_value"] for e in negative["exception_chain"]
                   for p in e.get("error_data_properties", []) if p["name"] == "error_Code"]
    require(error_codes == [0x80070002], "native error code mismatch")

    return {
        "verified_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
        "verification": "recomputed from retained private bytes; no new scheduler or process calls",
        "verifier_sha256": digest(Path(__file__).read_bytes()),
        "retained_runs": [direct_run, cim_run], "streams_verified": 12,
        "windows": direct_receipt["windows"],
        "codepages": {k: direct_receipt[k] for k in ("oem_codepage", "ansi_codepage")},
        "binary_sha256": {k: v["sha256"] for k, v in direct_receipt["executables"].items()},
        "module_versions": response["module_versions"],
        "module_source_sha256": cim_receipt["module_source_sha256"],
        "direct": {"before_after_and_later_com_equal": True,
                   "com_utf8_bytes": len(original), "com_utf8_sha256": digest(original),
                   "query_bytes": len(query), "query_sha256": digest(query),
                   "exact_transformation": {"U+00A7_to_U+0015": 1, "CRLF_to_CRCRLF": 36},
                   "com_xml_well_formed": True, "query_xml_well_formed": False,
                   "query_parse_error": query_error},
        "cim": {"exact_matches_four_com_snapshots": True,
                "exact_bytes": len(exact), "exact_sha256": digest(exact),
                "case_bytes": len(case), "case_sha256": digest(case),
                "case_xml_well_formed": True, "case_request_changes_only_uri": True,
                "negative_error_code": "0x80070002", "semantic_absence_established": False},
        "not_qualified": ["hidden task", "access denied", "service unavailable",
                          "full Unicode alphabet", "production replacement", "atomic task snapshot"],
    }


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("Expected saved evidence directory and new report JSON path")
    result = verify(Path(sys.argv[1]))
    encoded = json.dumps(result, ensure_ascii=True, indent=2) + "\n"
    with Path(sys.argv[2]).open("x", encoding="utf-8") as output:
        output.write(encoded)
    print(encoded, end="")
