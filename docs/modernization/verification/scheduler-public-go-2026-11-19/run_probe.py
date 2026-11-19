"""Build and run the separately reviewed, read-only Go Scheduler probe.

Uses an existing exact checkout and installed offline tools.  No task mutation,
test suite, package installation, source checkout, or policy change is performed.
The compiler and probe are waited through their creator-owned Popen objects.
This runner does not claim an independently enforced whole-process-tree deadline.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import time


HEAD = "1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3"
TREE = "f4b20c7ff7c0e0224f056cd9d8ec6b15013dff2a"
SOURCES = {
    "internal/scheduler/scheduler.go": "f13244c94d13ab424e55a8fee74669c17790a83ef93e7f10e0b954813001a825",
    "internal/scheduler/scheduler_windows.go": "d2d286ea3c43ea1337dbf1ec7125557d58539f7868410ad1edf38fdcde35588c",
    "internal/scheduler/scheduler_com_windows.go": "e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa",
}
STAGE = Path("docs/modernization/verification/scheduler-public-go-2026-11-19")


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("root", "source", "work", "go", "git", "goroot", "gocache", "gomodcache"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--expected-probe-sha256", required=True)
    args = parser.parse_args()
    if os.name != "nt":
        raise RuntimeError("Windows execution is required")

    root = args.root.resolve(strict=True)
    source = args.source.resolve(strict=True)
    work = args.work.resolve()
    payload = Path(__file__).with_name("probe.go").resolve(strict=True)
    for path in (source, work, payload, args.gocache.resolve(strict=True), args.gomodcache.resolve(strict=True)):
        if not path.is_relative_to(root) or path == root:
            raise RuntimeError("Working files and existing caches must remain inside the approved root")
    gofmt = args.go.with_name("gofmt.exe")
    for path in (args.go, gofmt, args.git):
        if not path.is_file():
            raise RuntimeError("Required existing executable is missing")
    if not args.goroot.is_dir():
        raise RuntimeError("Required existing GOROOT is missing")
    stage = source / STAGE
    if work.exists() or stage.exists():
        raise RuntimeError("One-shot evidence or staging directory already exists")
    if not stage.parent.resolve(strict=True).is_relative_to(source):
        raise RuntimeError("Staging parent resolves outside the source checkout")
    payload_bytes = payload.read_bytes()
    payload_sha256 = hashlib.sha256(payload_bytes).hexdigest()
    if payload_sha256 != args.expected_probe_sha256:
        raise RuntimeError("Probe differs from the separately reviewed input")

    system_root = os.environ.get("SystemRoot") or os.environ.get("WINDIR")
    if not system_root:
        raise RuntimeError("Windows system root is unavailable")
    powershell = Path(system_root) / "System32/WindowsPowerShell/v1.0/powershell.exe"
    if not powershell.is_file():
        raise RuntimeError("System Windows PowerShell is unavailable")
    runtime = work / "runtime"
    env = {key: value for key, value in os.environ.items()
           if key.upper() in {"SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "OS",
                              "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS"}}
    env.update({
        "GOROOT": str(args.goroot), "GOENV": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off",
        "GOSUMDB": "off", "GOVCS": "*:off", "GOWORK": "off", "GOFLAGS": "-mod=readonly",
        "GOTELEMETRY": "off", "CGO_ENABLED": "0", "GOMAXPROCS": "4",
        "GOCACHE": str(args.gocache), "GOMODCACHE": str(args.gomodcache),
        "GOPATH": str(runtime / "gopath"), "USERPROFILE": str(runtime / "profile"),
        "APPDATA": str(runtime / "appdata"), "LOCALAPPDATA": str(runtime / "localappdata"),
        "TEMP": str(runtime / "tmp"), "TMP": str(runtime / "tmp"),
        "TMPDIR": str(runtime / "tmp"), "GOTMPDIR": str(runtime / "tmp"),
        "PSModuleAnalysisCachePath": str(runtime / "localappdata/module-analysis-cache"),
        "PATH": os.pathsep.join((str(powershell.parent), str(Path(system_root) / "System32"), str(args.go.parent))),
    })
    if shutil.which("powershell.exe", path=env["PATH"]) != str(powershell):
        raise RuntimeError("Unexpected PowerShell resolution")

    def git(*command):
        completed = subprocess.run([str(args.git), "-C", str(source), *command], env=env,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)
        return completed.stdout.decode("utf-8").strip()

    if git("rev-parse", "HEAD") != HEAD or git("rev-parse", "HEAD^{tree}") != TREE:
        raise RuntimeError("Source commit/tree differs from the reviewed input")
    if git("status", "--porcelain=v1", "--untracked-files=all"):
        raise RuntimeError("Source checkout is not clean")
    if {name: sha256(source / name) for name in SOURCES} != SOURCES:
        raise RuntimeError("Production source digest mismatch")

    work.mkdir()
    for name in ("tmp", "profile", "appdata", "localappdata", "gopath"):
        (runtime / name).mkdir(parents=True)
    with (work / "probe.go").open("xb") as target:
        target.write(payload_bytes)
    receipt = {
        "schema": 1, "started_utc": utc_now(), "source_head": HEAD, "source_tree": TREE,
        "source_sha256_before": SOURCES, "source_clean_before": True,
        "probe_sha256": payload_sha256, "runner_sha256": sha256(Path(__file__)),
        "windows_version": list(os.sys.getwindowsversion()[:3]),
        "executables": {"go_sha256": sha256(args.go), "gofmt_sha256": sha256(gofmt), "powershell_sha256": sha256(powershell)},
        "environment": {key: env[key] for key in ("GOENV", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GOVCS",
                                                  "GOWORK", "GOFLAGS", "GOTELEMETRY", "CGO_ENABLED", "GOMAXPROCS")},
        "scheduler_override_present": "MCPHUB_E2E_SCHEDULER" in env,
        "all_working_paths_inside_approved_root": True,
        "commands": [], "staging_disposition": "not_created", "error": None,
        "whole_process_tree_deadline_enforced": False,
    }

    def checkpoint():
        path = work / ("checkpoint-%02d.json" % len(list(work.glob("checkpoint-*.json"))))
        with path.open("x", encoding="utf-8", newline="\n") as handle:
            json.dump(receipt, handle, indent=2)
            handle.write("\n")

    def run(label, command, public_arguments, cwd):
        stdout, stderr = work / (label + ".stdout.log"), work / (label + ".stderr.log")
        record = {"name": label, "arguments": public_arguments, "started_utc": utc_now(),
                  "process_start_attempted": True, "settled": False}
        receipt["commands"].append(record)
        checkpoint()
        print("START " + label, flush=True)
        started = time.monotonic()
        with stdout.open("xb") as out, stderr.open("xb") as err:
            with subprocess.Popen(command, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=out, stderr=err) as process:
                record["exit_code"] = process.wait()
                record["settled"] = True
        record.update({"finished_utc": utc_now(), "seconds": round(time.monotonic() - started, 6),
                       "settled": True, "stdout_bytes": stdout.stat().st_size,
                       "stderr_bytes": stderr.stat().st_size,
                       "stdout_sha256": sha256(stdout), "stderr_sha256": sha256(stderr)})
        checkpoint()
        print(json.dumps(record), flush=True)
        if record["exit_code"] != 0:
            raise RuntimeError(label + " returned a nonzero exit code")
        return stdout

    def settle_stage():
        if receipt["staging_disposition"] != "created":
            return
        if any(command["name"] in {"gofmt", "build"} and command.get("process_start_attempted")
               and not command.get("settled") for command in receipt["commands"]):
            raise RuntimeError("Staging reader settlement is unconfirmed; staging retained outside trash")
        if sorted(path.name for path in stage.iterdir()) != ["probe.go"]:
            raise RuntimeError("Staging directory acquired unexpected contents; retained outside trash")
        if sha256(stage / "probe.go") != receipt["probe_sha256"] or sha256(work / "probe.go") != receipt["probe_sha256"]:
            raise RuntimeError("Staging or retained probe identity changed; retained outside trash")
        trash_base = root / "trash"
        if not trash_base.resolve().is_relative_to(root):
            raise RuntimeError("Trash parent resolves outside the approved root; staging retained")
        trash_base.mkdir(exist_ok=True)
        if not trash_base.resolve(strict=True).is_relative_to(root):
            raise RuntimeError("Trash parent changed outside the approved root; staging retained")
        trash = trash_base / ("r17-public-go-" + receipt["started_utc"].replace(":", "").replace("+", "_"))
        trash.mkdir(exist_ok=False)
        stage.rename(trash / "staged-probe")
        receipt["staging_disposition"] = "moved_to_trash_with_verified_retained_copy"
        receipt["staging_absent_after_move"] = not stage.exists()
        receipt["trash_copy_sha256"] = sha256(trash / "staged-probe/probe.go")

    checkpoint()
    try:
        version = run("go-version", [str(args.go), "version"], ["go", "version"], source)
        receipt["go_version"] = version.read_text(encoding="utf-8").strip()
        stage.mkdir()
        receipt["staging_disposition"] = "created_unverified"
        with (stage / "probe.go").open("xb") as target:
            target.write(payload_bytes)
        if sha256(stage / "probe.go") != receipt["probe_sha256"]:
            raise RuntimeError("Staged probe does not match reviewed input; retained outside trash")
        receipt["staging_disposition"] = "created"
        formatting = run("gofmt", [str(gofmt), "-d", str(stage / "probe.go")],
                         ["gofmt", "-d", "<source>/" + STAGE.as_posix() + "/probe.go"], source)
        if formatting.stat().st_size:
            raise RuntimeError("Probe needs gofmt; source was not modified")
        executable = work / "probe.exe"
        run("build", [str(args.go), "build", "-mod=readonly", "-trimpath", "-tags=r17_public_scheduler_probe", "-o", str(executable), str(stage / "probe.go")],
            ["go", "build", "-mod=readonly", "-trimpath", "-tags=r17_public_scheduler_probe", "-o", "<evidence>/probe.exe", "<source>/" + STAGE.as_posix() + "/probe.go"], source)
        receipt["probe_executable_sha256"] = sha256(executable)
        settle_stage()
        receipt["source_clean_before_probe"] = not git("status", "--porcelain=v1", "--untracked-files=all")
        if not receipt["source_clean_before_probe"]:
            raise RuntimeError("Checkout did not return to clean state after the build")
        run("probe", [str(executable)], ["<evidence>/probe.exe"], work)
    except Exception as error:
        receipt["error"] = str(error)
    finally:
        try:
            settle_stage()
        except Exception as error:
            receipt["staging_error"] = str(error)
        receipt["source_unchanged"] = False
        try:
            receipt["source_head_after"] = git("rev-parse", "HEAD")
            receipt["source_tree_after"] = git("rev-parse", "HEAD^{tree}")
            receipt["source_clean_after"] = not git("status", "--porcelain=v1", "--untracked-files=all")
            receipt["source_sha256_after"] = {name: sha256(source / name) for name in SOURCES}
            receipt["source_unchanged"] = (receipt["source_head_after"] == HEAD and receipt["source_tree_after"] == TREE
                                           and receipt["source_clean_after"] and receipt["source_sha256_after"] == SOURCES)
        except Exception as error:
            receipt["source_inspection_error"] = str(error)
        receipt["finished_utc"] = utc_now()
        with (work / "execution.json").open("x", encoding="utf-8", newline="\n") as handle:
            json.dump(receipt, handle, indent=2)
            handle.write("\n")
    print(json.dumps({"execution_receipt_written": True, "error": receipt["error"],
                      "source_unchanged": receipt["source_unchanged"],
                      "staging_disposition": receipt["staging_disposition"]}), flush=True)
    return 0 if receipt["error"] is None and receipt["source_unchanged"] and "staging_error" not in receipt else 1


if __name__ == "__main__":
    raise SystemExit(main())
