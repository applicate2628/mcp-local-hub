"""One read-only observation of the two approved names after owner settlement."""

from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess


ROOT = Path(r"D:\dev\ChatGPT")
PREP = ROOT / "mcp-r17-public-go-positive-prepare-20261119"
RUN_ID = "027ca2dd03624a318f469ad87b259bff"
POWERSHELL_SHA256 = "3247bcfd60f6dd25f34cb74b5889ab10ef1b3ec72b4d4b3d95b5b25b534560b8"
SCRIPT = r"""
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$service = New-Object -ComObject 'Schedule.Service'
$service.Connect()
$folder = $service.GetFolder('\')
$tasks = @($folder.GetTasks(1))
$counts = @('visible', 'hidden') | ForEach-Object {
    $name = 'mcp-r17-027ca2dd03624a318f469ad87b259bff-' + $_
    [pscustomobject]@{ kind = $_; count = @($tasks | Where-Object { $_.Name -ceq $name }).Count }
}
[ordered]@{ observed_utc = [DateTime]::UtcNow.ToString('o'); get_tasks_flag = 1; query_only = $true; counts = @($counts) } | ConvertTo-Json -Depth 4
"""


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    if os.name != "nt" or ROOT.resolve(strict=True) != ROOT:
        raise RuntimeError("Expected the admitted Windows work root")
    if not PREP.resolve(strict=True).is_relative_to(ROOT):
        raise RuntimeError("Observation path escaped the admitted root")
    launch = json.loads((PREP / "launch-result.json").read_bytes())
    if (launch["run_id"] != RUN_ID or launch["settled"] is not True
            or launch["exit_code"] != 0 or launch["error"] is not None):
        raise RuntimeError("Successful owner settlement must be established first")
    powershell = Path(os.environ["SystemRoot"]) / "System32/WindowsPowerShell/v1.0/powershell.exe"
    if digest(powershell) != POWERSHELL_SHA256:
        raise RuntimeError("System PowerShell changed")
    runtime = PREP / "launch-runtime"
    for name in ("tmp", "profile", "appdata", "localappdata"):
        if not (runtime / name).resolve(strict=True).is_relative_to(ROOT):
            raise RuntimeError("Runtime path escaped the admitted root")
    env = {key: value for key, value in os.environ.items()
           if key.upper() in {"SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "OS",
                              "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS"}}
    env.update({"PATH": str(powershell.parent) + os.pathsep + str(powershell.parent.parent.parent),
                "TEMP": str(runtime / "tmp"), "TMP": str(runtime / "tmp"),
                "USERPROFILE": str(runtime / "profile"), "APPDATA": str(runtime / "appdata"),
                "LOCALAPPDATA": str(runtime / "localappdata"),
                "PSModuleAnalysisCachePath": str(runtime / "localappdata/module-analysis-cache")})
    record = {"run_id": RUN_ID, "observer_sha256": digest(Path(__file__)),
              "started_utc": datetime.now(timezone.utc).isoformat(), "settled": False,
              "exit_code": None, "all_absent": False, "error_type": None,
              "scheduler_mutations": 0, "execution_policy_override": False}
    with (PREP / "absence-observation-intent.json").open("x", encoding="utf-8") as handle:
        json.dump(record, handle, indent=2)
    stdout = PREP / "absence-observation.json"
    stderr = PREP / "absence-observation.stderr.log"
    try:
        with stdout.open("xb") as out, stderr.open("xb") as err:
            with subprocess.Popen([str(powershell), "-NoProfile", "-NonInteractive", "-Command", SCRIPT],
                                  cwd=ROOT, env=env, stdin=subprocess.DEVNULL, stdout=out, stderr=err) as process:
                record["exit_code"] = process.wait()
                record["settled"] = True
        record["stdout_sha256"] = digest(stdout)
        record["stderr_sha256"] = digest(stderr)
        if record["exit_code"] == 0:
            value = json.loads(stdout.read_bytes())
            record["all_absent"] = (value["query_only"] is True and value["get_tasks_flag"] == 1
                                    and value["counts"] == [{"kind": "visible", "count": 0},
                                                            {"kind": "hidden", "count": 0}])
    except Exception as error:
        record["error_type"] = type(error).__name__
    finally:
        record["finished_utc"] = datetime.now(timezone.utc).isoformat()
        with (PREP / "absence-observation-execution.json").open("x", encoding="utf-8", newline="\n") as handle:
            json.dump(record, handle, indent=2)
            handle.write("\n")
    print(json.dumps(record))
    return 0 if record["settled"] and record["exit_code"] == 0 and record["all_absent"] and record["error_type"] is None else 1


if __name__ == "__main__":
    raise SystemExit(main())
