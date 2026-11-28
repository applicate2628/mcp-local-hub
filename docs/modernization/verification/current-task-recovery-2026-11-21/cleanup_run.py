"""One provenance-pinned cleanup of the retained original R21 fixture."""
import sys
sys.dont_write_bytecode = True
import json
from pathlib import Path
from check_recovery import Runner, ROOT, SOURCE, GO, GOFMT, sha, save_new

HARNESS_SHA = "e471a1ebbd12bc4210e6687ad2bc4fb660dea03fcea48ab107b652650b482554"
COMMIT = "ba17867125b3728fb734e90b383639a3ff9175ee"
NAME = "\\MCPHub-R21-Native-e8b3694319384cb5af9cf33f6b1396ef-U8"


def cleanup(runner):
    runner.begin()
    runner.receipt["source_before"] = runner.admit("published", COMMIT)
    original = ROOT / "native-retained-cleanup.go"
    raw = original.read_bytes()
    if sha(raw) != HARNESS_SHA or len(runner.files) != 9:
        raise RuntimeError("Cleanup source or manifest differs")
    formatted = ROOT / "native-retained-cleanup-formatted.go"
    with formatted.open("xb") as stream:
        stream.write(raw)
    runner.require_pass(runner.run("cleanup-gofmt", GOFMT, ["-w", str(formatted)], 60, ROOT))
    virtual = SOURCE / "cmd/r21-native-retained-cleanup/main.go"
    overlay = ROOT / "native-retained-cleanup-overlay.json"
    executable = ROOT / "native-retained-cleanup.exe"
    if virtual.parent.exists() or executable.exists():
        raise RuntimeError("Cleanup destination already exists")
    save_new(overlay, {"Replace": {str(virtual): str(formatted)}})
    runner.require_pass(runner.run("cleanup-build", GO, ["build", "-overlay", str(overlay), "-o", str(executable), "./cmd/r21-native-retained-cleanup"], 900))
    if virtual.parent.exists():
        raise RuntimeError("Cleanup overlay materialized source files")
    runner.admit("published", COMMIT)
    if sha(original.read_bytes()) != HARNESS_SHA or sha((ROOT / "source-manifest.json").read_bytes()) != runner.receipt["manifest_sha256"]:
        raise RuntimeError("Cleanup inputs changed during build")
    pins = {str(p): sha(p.read_bytes()) for p in (original, formatted, overlay, executable, Path(__file__), ROOT / "source-manifest.json")}
    save_new(runner.out / "cleanup-launch-inputs.json", pins)
    previous = runner.env["MCPHUB_E2E_SCHEDULER"]
    try:
        runner.env["MCPHUB_E2E_SCHEDULER"] = ""
        row = runner.run("cleanup-owned", executable, [], 180, ROOT)
    finally:
        runner.env["MCPHUB_E2E_SCHEDULER"] = previous
        started = (runner.out / "cleanup-owned.launched.json").is_file()
        runner.receipt.update(native_task_operations=True if started else None,
                              native_task_operations_basis="Observed launch of the reviewed single-fixture conditional cleanup; actual mutation counters require reconciled events" if started else "Cleanup launch attempted; outcome unresolved")
    runner.require_pass(row)
    stdout = [json.loads(line) for line in (runner.out / "cleanup-owned.stdout.log").read_bytes().splitlines()]
    durable = [json.loads(line) for line in (runner.out / "ownedevents.log").read_bytes().splitlines()]
    if len(stdout) != 6 or durable != stdout[:-1] or (runner.out / "cleanup-owned.stderr.log").read_bytes():
        raise RuntimeError("Cleanup event streams differ")
    expected_operations = ["cleanup_intent", "provenance_verified", "delete_attempt", "delete_result", "absence_observation"]
    if [event.get("operation") for event in durable] != expected_operations or any(event.get("name") != NAME for event in stdout):
        raise RuntimeError("Cleanup touched an unexpected name or operation sequence")
    final = stdout[-1]
    if final.get("final") is not True or final.get("completed") is not True or final.get("delete_attempted") is not True or type(final.get("deleted_tasks")) is not int or final["deleted_tasks"] != 1 or final.get("absence_confirmed") is not True or "error" in final:
        raise RuntimeError("Original fixture cleanup is not confirmed")
    if durable[2].get("verified_export_sha256") != "5bbf10501bf82e822aa692bc99ac27017249e75fbee34ab59eafa4fb032231ce" or durable[2].get("owner_sid_matches") is not True or durable[2].get("disabled") is not True or durable[3].get("error") != "" or durable[3].get("confirmed_successful_return") is not True or durable[4].get("absent") is not True:
        raise RuntimeError("Cleanup predicates/results are not confirmed")
    if any(sha(Path(p).read_bytes()) != value for p, value in pins.items()):
        raise RuntimeError("Cleanup inputs drifted")
    after = runner.admit("published", COMMIT)
    runner.receipt.update(source_after=after, source_head=after["head"], source_tree=after["tree"], completed=True, state="complete", cleanup_result=final, stdout_matches_durable_observations=True, native_delete_attempts=1, native_absence_observations=1)


if __name__ == "__main__":
    runner, failure = Runner("native-retained-cleanup"), None
    try:
        if Path(__file__).resolve().parent != ROOT or len(sys.argv) != 1:
            raise RuntimeError("Use the cleanup wrapper from the approved root")
        cleanup(runner)
    except BaseException as error:
        failure = error
    finally:
        runner.finish(failure)
    if failure is not None or not runner.receipt["completed"]:
        raise SystemExit(1)
