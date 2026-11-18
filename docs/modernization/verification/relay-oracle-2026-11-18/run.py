"""Run finite controls against the real relay probe; no downloads or installations."""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import sys
from datetime import datetime, timezone

p = argparse.ArgumentParser()
p.add_argument("phase", choices=("baseline", "fixed"))
p.add_argument("--check-existing", action="store_true",
               help="Validate retained JSON logs without running Go again")
for name in ("source", "work", "module-cache", "build-cache", "goroot", "go", "cc"):
    p.add_argument("--" + name, required=True)
a = p.parse_args()
source, work = Path(a.source), Path(a.work)
phase = work / a.phase
previous = json.loads((phase / "summary.json").read_text(encoding="utf-8")) if a.check_existing else None
if not a.check_existing:
    phase.mkdir(exist_ok=False)
env = {k: v for k, v in os.environ.items() if not k.upper().startswith("MCPHUB_")}
for name in ("tmp", "appdata", "localappdata", "profile", "gopath"):
    (work / name).mkdir(exist_ok=True)
env.update({
    "GOROOT": a.goroot, "GOENV": "off", "GOTOOLCHAIN": "local",
    "GOPROXY": "off", "GOSUMDB": "off", "GOWORK": "off", "GOFLAGS": "",
    "GOCACHE": a.build_cache, "GOMODCACHE": a.module_cache,
    "GOPATH": str(work / "gopath"), "CC": a.cc, "CGO_ENABLED": "1",
    "GOMAXPROCS": "4", "GOTELEMETRY": "off", "MCPHUB_NO_AUTO_GUI": "1",
    "MCPHUB_E2E_SCHEDULER": "none", "USERPROFILE": str(work / "profile"),
    "APPDATA": str(work / "appdata"), "LOCALAPPDATA": str(work / "localappdata"),
    "TEMP": str(work / "tmp"), "TMP": str(work / "tmp"),
    "TMPDIR": str(work / "tmp"), "GOTMPDIR": str(work / "tmp"),
})
env["PATH"] = str(Path(a.cc).parent) + os.pathsep + env.get("PATH", "")
records = json.loads((phase / "commands.json").read_text(encoding="utf-8")) if a.check_existing else []

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def execute(name, arguments, case_env):
    started = datetime.now(timezone.utc).isoformat()
    stdout, stderr = phase / (name + ".stdout.jsonl"), phase / (name + ".stderr.log")
    if a.check_existing:
        record, = (r for r in records if r["name"] == name)
        if record["stdout_sha256"] != digest(stdout) or record["stderr_sha256"] != digest(stderr):
            raise RuntimeError("retained log hash mismatch: " + name)
        return record, stdout.read_text(encoding="utf-8", errors="strict")
    with stdout.open("wb") as out, stderr.open("wb") as err:
        result = subprocess.run([a.go, *arguments], cwd=source, env=case_env,
                                stdout=out, stderr=err,
                                creationflags=subprocess.CREATE_NO_WINDOW)
    record = {"name": name, "started_utc": started,
              "finished_utc": datetime.now(timezone.utc).isoformat(),
              "arguments": arguments, "exit_code": result.returncode,
              "stdout_sha256": digest(stdout), "stderr_sha256": digest(stderr)}
    records.append(record)
    (phase / "commands.json").write_text(json.dumps(records, indent=2) + "\n", encoding="utf-8")
    return record, stdout.read_text(encoding="utf-8", errors="replace")

fixture = work / "probe-control.exe"
if a.phase == "baseline":
    record, _ = execute("build-control", ["build", "-o", str(fixture), str(work / "fixture")], env)
    if record["exit_code"] != 0:
        print("control build failed; source/logs retained", flush=True)
        sys.exit(1)
if not fixture.is_file():
    raise RuntimeError("previously built owned control is required")
cases = []
for mode, reason in (("clean", ""), ("extra-tail", "unexpected trailing relay response"),
                     ("wrong-init-version", "bad init"),
                     ("wrong-call-version", "wrong binary relay response")):
    case_dir = phase / mode
    if not a.check_existing:
        case_dir.mkdir()
    case_env = env | {"MCPHUB_RX_RELAY_BINARY": str(fixture),
                      "MCPHUB_RX_RELAY_EVIDENCE": str(case_dir),
                      "MCPHUB_RX_PROBE_CONTROL": mode}
    arguments = ["test", "-json", "-race", "-count=1", "-p=1", "-timeout=90s",
                 "-run=^TestRXRelayBinaryProbe$/^processes_1$", "./internal/daemon"]
    record, output = execute(mode, arguments, case_env)
    events = [json.loads(line) for line in output.splitlines() if line]
    has_result = (case_dir / "relay-binary-1.json").is_file()
    reject = a.phase == "fixed" and mode != "clean"
    target = "TestRXRelayBinaryProbe/processes_1"
    outcomes = {e.get("Test", "package"): e["Action"] for e in events
                if e.get("Action") in ("pass", "fail", "skip")}
    expected_action = "fail" if reject else "pass"
    expected_outcomes = {"TestRXRelayBinaryProbe": expected_action,
                         target: expected_action, "package": expected_action}
    diagnostics = []
    unexpected = []
    if reject:
        # test2json can split one diagnostic across multiple Output events.
        for line in "".join(e["Output"] for e in events if e.get("Action") == "output").splitlines():
                if re.match(r"^\s+relay_binary_probe_windows_test\.go:\d+: ", line):
                    diagnostics.append(line.strip())
                elif not (line.startswith("=== RUN   ") or
                          line.lstrip().startswith("--- FAIL: ") or
                          line == "FAIL" or line.startswith("FAIL\t")):
                    unexpected.append(line)
    clean_rejection = (len(diagnostics) == 1 and reason in diagnostics[0]
                       and not unexpected)
    matched = ((record["exit_code"] == 1 and clean_rejection and not has_result)
               if reject else (record["exit_code"] == 0 and has_result))
    skipped = sum(e.get("Action") == "skip" for e in events)
    matched = (matched and outcomes == expected_outcomes and skipped == 0
               and (phase / (mode + ".stderr.log")).stat().st_size == 0)
    case = {"mode": mode, "exit_code": record["exit_code"],
            "expected_rejection": reject, "expected_diagnostic_found": reason in output if reject else None,
            "measurement_receipt_written": has_result, "skip_events": skipped,
            "test_outcomes": outcomes, "diagnostics": diagnostics,
            "unexpected_output": unexpected,
            "matched_expected_control": matched}
    cases.append(case)
    print(json.dumps(case), flush=True)
summary = {"phase": a.phase, "fixture_sha256": digest(fixture),
           "fixture_source_sha256": digest(work / "fixture" / "main.go"),
           "probe_source_sha256": digest(source / "internal/daemon/relay_binary_probe_windows_test.go"),
           "go_version": subprocess.check_output([a.go, "version"], env=env, text=True).strip(),
           "cases": cases, "all_expected_controls_matched": all(c["matched_expected_control"] for c in cases)}
summary["runner_sha256"] = digest(Path(__file__))
if a.check_existing:
    for key in ("fixture_sha256", "fixture_source_sha256", "go_version"):
        if summary[key] != previous[key]:
            raise RuntimeError("control environment changed: " + key)
    summary["probe_source_sha256"] = previous["probe_source_sha256"]
    summary["rechecked_retained_logs"] = True
name = "summary-rechecked.json" if a.check_existing else "summary.json"
(phase / name).write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
sys.exit(0 if summary["all_expected_controls_matched"] else 1)
