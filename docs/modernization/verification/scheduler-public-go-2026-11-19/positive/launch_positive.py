"""Plan by default; apply one explicitly approved positive Scheduler fixture."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


RUN_ID = "027ca2dd03624a318f469ad87b259bff"
FIXTURE_SHA256 = "b3cf12165b57df5f8ab43e41fc00b57ecdc824c650203ab3d4732386beebc514"
PROBE_SHA256 = "b97b069c42cc56ef632962864495b10482f886e94a12ea2a0335a9ba4a7d3bfc"
SOURCE_SHA256 = "e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa"
POWERSHELL_SHA256 = "3247bcfd60f6dd25f34cb74b5889ab10ef1b3ec72b4d4b3d95b5b25b534560b8"


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def now():
    return datetime.now(timezone.utc).isoformat()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    root = Path(r"D:\dev\ChatGPT")
    prep = root / "mcp-r17-public-go-positive-prepare-20261119"
    fixture = prep / "Invoke-R17PublicGoPositiveFixture.ps1"
    probe = root / "mcp-r17-hidden-work-20261119/public-go-positive-empty-input/probe.exe"
    source = root / "mcp-r17-resume-20261119/scheduler_com_windows-1b870e04.go"
    run = root / "mcp-r17-native-qualification-20261119" / RUN_ID
    plan = {
        "mode": "APPLY" if args.apply else "PLAN_ONLY", "run_id": RUN_ID,
        "fixture_sha256": FIXTURE_SHA256, "probe_sha256": PROBE_SHA256,
        "source_sha256": SOURCE_SHA256, "powershell_sha256": POWERSHELL_SHA256,
        "named_tasks": ["\\mcp-r17-" + RUN_ID + "-" + kind for kind in ("visible", "hidden")],
        "effects": "Two CREATE-only disabled, inert tasks; five public Go reads; fresh guarded cleanup even after a semantic failure",
        "execution_policy": "RemoteSigned for this PowerShell process and its inherited process environment only",
        "whole_process_tree_deadline_enforced": False,
    }
    if not args.apply:
        print(json.dumps(plan, indent=2))
        return 0
    if os.name != "nt" or root.resolve(strict=True) != root:
        raise RuntimeError("Apply requires the admitted Windows work root")
    for path in (prep, fixture, probe, source):
        if not path.resolve(strict=True).is_relative_to(root):
            raise RuntimeError("An input resolves outside the admitted work root")
    powershell = Path(os.environ["SystemRoot"]) / "System32/WindowsPowerShell/v1.0/powershell.exe"
    expected = {fixture: FIXTURE_SHA256, probe: PROBE_SHA256, source: SOURCE_SHA256, powershell: POWERSHELL_SHA256}
    if any(digest(path) != sha for path, sha in expected.items()):
        raise RuntimeError("An admitted file changed; no fixture process started")
    if run.exists():
        raise RuntimeError("The one-shot native run directory already exists")
    intent = prep / "launch-intent.json"
    result = prep / "launch-result.json"
    runtime = prep / "launch-runtime"
    if intent.exists() or result.exists() or runtime.exists():
        raise RuntimeError("This launch has already been attempted; no automatic retry")
    plan["started_utc"] = now()
    plan["launcher_sha256"] = digest(Path(__file__))
    with intent.open("x", encoding="utf-8", newline="\n") as handle:
        json.dump(plan, handle, indent=2)
        handle.write("\n")

    outcome = {"run_id": RUN_ID, "started_utc": plan["started_utc"], "start_attempted": False,
               "settled": False, "exit_code": None, "error": None}
    try:
        for name in ("tmp", "profile", "appdata", "localappdata"):
            (runtime / name).mkdir(parents=True)
        env = {key: value for key, value in os.environ.items()
               if key.upper() in {"SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "OS",
                                  "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS"}}
        env.update({"PATH": str(powershell.parent) + os.pathsep + str(powershell.parent.parent.parent),
                    "TEMP": str(runtime / "tmp"), "TMP": str(runtime / "tmp"),
                    "USERPROFILE": str(runtime / "profile"), "APPDATA": str(runtime / "appdata"),
                    "LOCALAPPDATA": str(runtime / "localappdata"),
                    "PSModuleAnalysisCachePath": str(runtime / "localappdata/module-analysis-cache")})
        command = [str(powershell), "-ExecutionPolicy", "RemoteSigned", "-NoProfile", "-NonInteractive",
                   "-File", str(fixture), "-SourceFile", str(source), "-ProbePath", str(probe),
                   "-ProbeSHA256", PROBE_SHA256, "-RunId", RUN_ID, "-Apply"]
        with (prep / "launch.stdout.log").open("xb") as stdout, (prep / "launch.stderr.log").open("xb") as stderr:
            outcome["start_attempted"] = True
            with subprocess.Popen(command, cwd=root, env=env, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr) as process:
                outcome["exit_code"] = process.wait()
                outcome["settled"] = True
        outcome["stdout_sha256"] = digest(prep / "launch.stdout.log")
        outcome["stderr_sha256"] = digest(prep / "launch.stderr.log")
    except Exception as error:
        outcome["error"] = str(error)
    finally:
        outcome["finished_utc"] = now()
        with result.open("x", encoding="utf-8", newline="\n") as handle:
            json.dump(outcome, handle, indent=2)
            handle.write("\n")
    print(json.dumps({"launch_result_written": True, "settled": outcome["settled"],
                      "exit_code": outcome["exit_code"], "launcher_error": outcome["error"] is not None}))
    return outcome["exit_code"] if outcome["settled"] and outcome["error"] is None else 1


if __name__ == "__main__":
    sys.exit(main())
