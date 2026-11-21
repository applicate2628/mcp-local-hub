"""One read-only native observation of the retained R21 fixture."""
import sys
sys.dont_write_bytecode = True
import json
from pathlib import Path
from check_recovery import Runner, ROOT, SOURCE, GO, GOFMT, sha, save_new

HARNESS_SHA = "2188d225edcc42d8fb8e82502f4e06bb504ebe04e07f7bef628f8b0e3121f6e7"


def observe(runner):
    runner.begin()
    runner.receipt["source_before"] = runner.admit("candidate")
    original = ROOT / "native-observer.go"
    raw = original.read_bytes()
    if sha(raw) != HARNESS_SHA or len(runner.files) != 9:
        raise RuntimeError("Observer or source manifest differs")
    formatted = ROOT / "native-observer-formatted.go"
    with formatted.open("xb") as stream:
        stream.write(raw)
    runner.require_pass(runner.run("observer-gofmt", GOFMT, ["-w", str(formatted)], 60, ROOT))
    virtual = SOURCE / "cmd/r21-native-observer/main.go"
    overlay = ROOT / "native-observer-overlay.json"
    executable = ROOT / "native-observer.exe"
    if virtual.parent.exists() or executable.exists():
        raise RuntimeError("Observer destination already exists")
    save_new(overlay, {"Replace": {str(virtual): str(formatted)}})
    runner.require_pass(runner.run("observer-build", GO, ["build", "-overlay", str(overlay), "-o", str(executable), "./cmd/r21-native-observer"], 900))
    if virtual.parent.exists():
        raise RuntimeError("Observer overlay materialized source files")
    pins = {str(p): sha(p.read_bytes()) for p in (original, formatted, overlay, executable, Path(__file__), ROOT / "source-manifest.json")}
    save_new(runner.out / "observer-launch-inputs.json", pins)
    previous = runner.env["MCPHUB_E2E_SCHEDULER"]
    try:
        runner.env["MCPHUB_E2E_SCHEDULER"] = ""
        runner.receipt.update(native_read_launch_attempted=True, native_task_mutations=False)
        row = runner.run("observer-read", executable, [], 120, ROOT)
    finally:
        runner.env["MCPHUB_E2E_SCHEDULER"] = previous
        started = (runner.out / "observer-read.launched.json").is_file()
        runner.receipt.update(native_task_operations=True if started else None,
                              native_task_operations_basis="Observed launch of the reviewed ExportXML/Status-only observer; no native mutation path" if started else "Read-only observer launch attempted; outcome unresolved")
    runner.require_pass(row)
    if any(sha(Path(p).read_bytes()) != value for p, value in pins.items()):
        raise RuntimeError("Observer inputs drifted")
    after = runner.admit("candidate")
    runner.receipt.update(source_after=after, source_head=after["head"], source_tree=after["tree"], completed=True, state="complete")


if __name__ == "__main__":
    runner, failure = Runner("native-observer"), None
    try:
        if Path(__file__).resolve().parent != ROOT or len(sys.argv) != 1:
            raise RuntimeError("Use the observer wrapper from the approved root")
        observe(runner)
    except BaseException as error:
        failure = error
    finally:
        runner.finish(failure)
    if failure is not None or not runner.receipt["completed"]:
        raise SystemExit(1)
