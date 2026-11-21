"""Reviewed native qualification wrapper; prepare only, execution is separate."""
import sys
sys.dont_write_bytecode = True

from collections import Counter
import json
from pathlib import Path
from check_recovery import Runner, ROOT, SOURCE, GO, GOFMT, sha, BASE, save_new

HARNESS_SHA = "7691f5c760e0b024eb1d45c145d2f729efac9fd3e54daae29d3cc88fa4913c27"
RUN_ID = "e8b3694319384cb5af9cf33f6b1396ef"
NATIVE = ROOT / ("native-r21-" + RUN_ID)
NAMES = ["\\MCPHub-R21-Native-" + RUN_ID + suffix for suffix in ("-U8", "-U16")]


def json_lines(raw):
    values = [json.loads(line) for line in raw.decode("utf-8").splitlines()]
    if not values or any(not isinstance(value, dict) for value in values):
        raise RuntimeError("Expected nonempty object-only JSON observations")
    return values


def verify_native(runner):
    stdout = json_lines((runner.out / "native-harness.stdout.log").read_bytes())
    expected = {"final": True, "passed": True, "directory": str(NATIVE), "created_tasks": 2,
                "deleted_tasks": 2, "cleanup_requested": True, "cleanup_complete": True}
    if stdout[-1] != expected or any(type(stdout[-1].get(k)) is not type(v) for k, v in expected.items()) or any(event.get("final") for event in stdout[:-1]):
        raise RuntimeError("Native final receipt does not show exactly two creations and two verified deletions")
    if (runner.out / "native-harness.stderr.log").read_bytes():
        raise RuntimeError("Unexpected native stderr; retain all evidence")
    raw_manifest = (NATIVE / "manifest.json").read_bytes()
    manifest = json.loads(raw_manifest)
    rows_raw = (NATIVE / "observations.jsonl").read_bytes()
    rows = json_lines(rows_raw)
    if rows != stdout[:-1]:
        raise RuntimeError("Durable native observations differ from stdout")
    fixtures = manifest.get("fixtures", [])
    if manifest.get("base_commit") != BASE or manifest.get("run_id") != RUN_ID or [f.get("name") for f in fixtures] != NAMES or [f.get("case_alias") for f in fixtures] != [n.lower() for n in NAMES]:
        raise RuntimeError("Native manifest identity or exact task names differ")
    if manifest.get("globally_disabled") is not True or manifest.get("hidden") is not True or manifest.get("triggers") != "none" or manifest.get("cleanup_requested") is not True:
        raise RuntimeError("Native fixture policy differs")
    counts = Counter(row.get("operation") for row in rows)
    expected_counts = {"preflight": 2, "invalid_bytes": 2, "create_attempt": 2, "create_result": 2,
                       "export_observation": 2, "conflict_attempt": 4, "conflict": 4,
                       "cleanup_attempt": 2, "cleanup_complete": 2, "complete": 1}
    if counts != expected_counts:
        raise RuntimeError("Unexpected native observation counts or an uncertain/retained task")
    selected = lambda operation: [row for row in rows if row["operation"] == operation]
    for operation in ("preflight", "invalid_bytes", "create_attempt", "create_result", "export_observation", "cleanup_attempt", "cleanup_complete"):
        if {row.get("name") for row in selected(operation)} != set(NAMES):
            raise RuntimeError("Observation names do not match the two fixtures")
    if any(row.get("error") != "" or row.get("outcome_uncertain") is not False for row in selected("create_result")) or any(row.get("corrupt") is not True for row in selected("invalid_bytes")):
        raise RuntimeError("Native creation/input-refusal observations failed")
    conflicts = selected("conflict")
    if {(row.get("name"), row.get("attempt_name")) for row in conflicts} != {(n, a) for n in NAMES for a in (n, n.lower())} or any(row.get("already_exists") is not True or row.get("full_xml_unchanged") is not True or row.get("export_error") != "" for row in conflicts):
        raise RuntimeError("Native conflict preservation observations failed")
    if any(row.get("absent") is not True for row in selected("preflight") + selected("cleanup_complete")):
        raise RuntimeError("Preflight or post-cleanup absence was not observed")
    exports = {row["name"]: row for row in selected("export_observation")}
    attempts = {row["name"]: row for row in selected("create_attempt")}
    if any(attempts[f["name"]].get("input_sha256") != f.get("input_sha256") or attempts[f["name"]].get("encoding") != f.get("encoding") for f in fixtures):
        raise RuntimeError("Creation observations differ from the input manifest")
    if any(row.get("after_sha256") != exports[row["name"]]["xml_text_sha256"] for row in conflicts) or any(row.get("verified_xml_sha256") != exports[row["name"]]["xml_text_sha256"] for row in selected("cleanup_attempt")):
        raise RuntimeError("Conflict or cleanup XML hashes differ from the retained definition")
    for index, name in enumerate(NAMES, 1):
        raw = (NATIVE / ("export-%d.xml" % index)).read_bytes()
        if sha(raw) != exports[name]["xml_file_sha256"] or sha(raw.decode("utf-16").encode("utf-8")) != exports[name]["xml_text_sha256"]:
            raise RuntimeError("Retained native XML differs from its observation")
    runner.receipt.update(native_result=stdout[-1], native_operation_counts=dict(counts),
                          native_counter_basis="Reviewed harness call/result observations, reconciled with durable JSONL; not syscall tracing",
                          native_manifest_sha256=sha(raw_manifest), native_observations_sha256=sha(rows_raw),
                          stdout_matches_durable_observations=True)


def qualify(runner):
    runner.begin()
    runner.receipt["source_before"] = runner.admit("candidate")
    if len(runner.files) != 9:
        raise RuntimeError("Native build requires the exact nine-file candidate manifest")
    original = ROOT / "native-harness/main.go"
    raw = original.read_bytes()
    if sha(raw) != HARNESS_SHA:
        raise RuntimeError("Native harness source differs from the reviewed hash")
    formatted = ROOT / "native-harness-formatted/main.go"
    formatted.parent.mkdir(exist_ok=False)
    with formatted.open("xb") as stream:
        stream.write(raw)
    runner.receipt["harness_before_format_sha256"] = sha(formatted.read_bytes())
    runner.require_pass(runner.run("native-gofmt", GOFMT, ["-w", str(formatted)], runner.manifest["command_timeouts"]["format"], ROOT))
    runner.receipt["harness_after_format_sha256"] = sha(formatted.read_bytes())
    virtual = SOURCE / "cmd/r21-native-qualification/main.go"
    overlay, executable = ROOT / "native-overlay.json", ROOT / "native-r21.exe"
    if virtual.parent.exists() or executable.exists() or NATIVE.exists():
        raise RuntimeError("Native qualification destination already exists; no reuse or fallback")
    save_new(overlay, {"Replace": {str(virtual): str(formatted)}})
    runner.require_pass(runner.run("native-build", GO, ["build", "-overlay", str(overlay), "-o", str(executable), "./cmd/r21-native-qualification"], runner.manifest["command_timeouts"]["build"]))
    if virtual.parent.exists():
        raise RuntimeError("Build materialized the virtual source directory; native launch refused")
    source = runner.admit("candidate")
    pin = {"base": BASE, "source_sha256": source["sha256"], "manifest_sha256": runner.receipt["manifest_sha256"],
           "harness_original_sha256": HARNESS_SHA, "harness_formatted_sha256": sha(formatted.read_bytes()),
           "overlay_sha256": sha(overlay.read_bytes()), "executable_sha256": sha(executable.read_bytes()),
           "native_runner_sha256": sha(Path(__file__).read_bytes()), "native_environment_override": {"MCPHUB_E2E_SCHEDULER": ""}}
    if sha(original.read_bytes()) != HARNESS_SHA or sha((ROOT / "source-manifest.json").read_bytes()) != pin["manifest_sha256"] or pin["harness_formatted_sha256"] != runner.receipt["harness_after_format_sha256"]:
        raise RuntimeError("Pinned native inputs changed during build")
    save_new(runner.out / "native-launch-inputs.json", pin)
    runner.receipt["native_launch_inputs"] = pin
    previous, row = runner.env["MCPHUB_E2E_SCHEDULER"], None
    try:
        runner.env["MCPHUB_E2E_SCHEDULER"] = ""
        row = runner.run("native-harness", executable, ["--cleanup-owned"], 600, ROOT)
    finally:
        runner.env["MCPHUB_E2E_SCHEDULER"] = previous
        started = bool(row and row.get("pid")) or any(command.get("label") == "native-harness" and command.get("pid") for command in runner.receipt["commands"]) or (runner.out / "native-harness.launched.json").is_file()
        runner.receipt.update(native_harness_started=True if started else None, native_task_operations=True if started else None,
                              native_task_operations_basis="Reviewed native phase enabled after an observed harness process launch; operation counts require its reconciled receipts" if started else "Launch was attempted; missing launch evidence leaves the native outcome unresolved")
    runner.require_pass(row)
    verify_native(runner)
    if sha(executable.read_bytes()) != pin["executable_sha256"] or sha(formatted.read_bytes()) != pin["harness_formatted_sha256"] or sha(overlay.read_bytes()) != pin["overlay_sha256"] or sha(original.read_bytes()) != HARNESS_SHA or sha((ROOT / "source-manifest.json").read_bytes()) != pin["manifest_sha256"]:
        raise RuntimeError("Native executable or overlay inputs changed")
    after = runner.admit("candidate")
    runner.receipt.update(source_after=after, source_head=after["head"], source_tree=after["tree"], completed=True, state="complete")


if __name__ == "__main__":
    runner, failure = Runner("native-qualification"), None
    runner.receipt.update(native_runner_sha256=sha(Path(__file__).read_bytes()), native_task_operations_authorized=True,
                          native_task_operations_basis="Native launch not yet observed; the source manifest's false flag applies only to its unit suites")
    try:
        if Path(__file__).resolve().parent != ROOT or len(sys.argv) != 1:
            raise RuntimeError("Use the native wrapper from the approved root without arguments")
        qualify(runner)
    except BaseException as error:
        failure = error
    finally:
        runner.finish(failure)
    if failure is not None or not runner.receipt["completed"]:
        raise SystemExit(1)
