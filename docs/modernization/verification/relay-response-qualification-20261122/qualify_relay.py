"""Recover finished Relay evidence, then run only missing scoped gates.

No downloads, installation, Git writes, old script execution, or system tasks.
The retained Popen object owns each direct child. A timeout is a failure with
unresolved descendant state; this runner never kills by rediscovered PID.
"""
import sys
sys.dont_write_bytecode = True
import collections
import ctypes
import datetime
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import time

ROOT = Path(r"D:\dev\ChatGPT\mcp-relay-20261122-e21a1792")
OLD = Path(r"D:\dev\ChatGPT\mcp-p0-gates-20261121-247107d5")
BASE_SOURCE = OLD / "preserved-input/source-preserved"
SOURCE = ROOT / "source"
BASE = "858812988833a84368c60fba1540ae601155fa71"
TREE = "dba685c090952a93a80a1e82403c4646ad8aaed1"
INPUT = OLD / "manifests/relay-v2-candidate/manifest.json"
INPUT_SHA = "c5ebbe5c118f8fd79ea1fa776ef53756ae8def31ed3c9e59506397753b940b4e"
GO = Path(r"C:\msys64\ucrt64\bin\go.exe")
GIT = Path(r"C:\Program Files\Git\cmd\git.exe")
TOOLS = {
    GO: "0176dbac02ff27ef54c6bc5b316c7c2a2a89f7c8cf5fd314a0e03d6d2b924113",
    GO.with_name("gofmt.exe"): "95368e4aafefa20f962a345ab02f53ffc035ed69349612abbf385b52f4fb25c5",
    GO.with_name("gcc.exe"): "f6ff9378c1edb591131304518eac6456708d28720284105930d0d687136bcdd3",
    GIT: "177bc8a9af84fa3a400c694c975fda731d048279c074e1a93947ce8b067ebb48",
}
HISTORY = ["relay-format-v2-control", "relay-compile-v2-control",
           "relay-v2-control-body", "relay-v2-control-errors",
           "relay-v2-control-framing", "relay-v2-control-redirect",
           "relay-format-v2-prime", "relay-compile-v2-prime",
           "relay-v2-prime-control", "relay-format-v2-candidate",
           "relay-compile-v2-candidate", "relay-v2-contract"]


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("x", encoding="utf-8", newline="\n") as out:
        json.dump(value, out, indent=2, ensure_ascii=False)
        out.write("\n")


def checked(path):
    for item in (path, *path.parents):
        info = item.lstat()
        if item.is_symlink() or getattr(info, "st_file_attributes", 0) & 0x400:
            raise RuntimeError("Reparse path is not admitted: " + str(item))
    return path


def input_manifest():
    raw = checked(INPUT).read_bytes()
    if sha(raw) != INPUT_SHA:
        raise RuntimeError("Historical candidate manifest differs")
    data = json.loads(raw)
    if data["source_head"] != BASE or data["source_tree"] != TREE:
        raise RuntimeError("Unexpected source identity")
    return data


def recover():
    destination = ROOT / "history"
    destination.mkdir(exist_ok=False)
    summary = {"observed_utc": utc(), "source": BASE, "tree": TREE,
               "method": "Read retained evidence; no historical command rerun", "phases": []}
    for phase in HISTORY:
        directory = checked(OLD / "evidence" / phase)
        receipt = json.loads((directory / "receipt.json").read_bytes())
        if receipt["state"] != "complete" or not receipt["completed"]:
            raise RuntimeError("Historical phase did not complete: " + phase)
        if receipt["base"] != BASE or receipt["base_tree"] != TREE:
            raise RuntimeError("Unexpected historical base: " + phase)
        if receipt["runner_sha256"] != receipt["runner_sha256_after"] or receipt["manifest_sha256"] != receipt["manifest_sha256_after"] or receipt["tools_sha256"] != receipt["tools_sha256_after"]:
            raise RuntimeError("Historical executable/input drift: " + phase)
        if receipt.get("source_before") != receipt.get("source_after") or receipt.get("source_after") != receipt.get("source_final"):
            raise RuntimeError("Historical source drift: " + phase)
        row = {"phase": phase, "started_moscow": receipt["started_moscow"],
               "finished_moscow": receipt["finished_moscow"], "commands": []}
        for command in receipt["commands"]:
            label = command["label"]
            if not command["settled"] or command["timed_out"] or command.get("process_tree_unresolved"):
                raise RuntimeError("Historical command did not settle: " + label)
            for stream in ("stdout", "stderr"):
                raw = checked(directory / (label + "." + stream + ".log")).read_bytes()
                if sha(raw) != command[stream]["sha256"] or len(raw) != command[stream]["bytes"]:
                    raise RuntimeError("Historical log drift: " + phase + "/" + label)
                target = destination / phase / (label + "." + stream + ".log")
                target.parent.mkdir(parents=True, exist_ok=True)
                with target.open("xb") as out:
                    out.write(raw)
                if stream == "stdout" and "tests" in command:
                    events = [json.loads(line) for line in raw.splitlines()]
                    terminal = {action: [e.get("Test", "PACKAGE:" + e.get("Package", "")) for e in events if e.get("Action") == action] for action in ("pass", "fail", "skip")}
                    if terminal != command["tests"]:
                        raise RuntimeError("Historical events differ from receipt: " + label)
            oracle = json.loads((directory / (label + ".oracle.json")).read_bytes())
            if oracle.get("matched") is not True:
                raise RuntimeError("Historical oracle did not match: " + label)
            save(destination / phase / (label + ".receipt.json"), command)
            save(destination / phase / (label + ".oracle.json"), oracle)
            row["commands"].append({"label": label, "exit": command["exit_code"],
                "seconds": command["seconds"], "tests": {k: len(v) for k, v in command.get("tests", {}).items()},
                "source_unchanged": command.get("source_before") == command.get("source_after")})
        save(destination / phase / "receipt.json", receipt)
        summary["phases"].append(row)
    save(ROOT / "history-summary.json", summary)
    print(json.dumps(summary), flush=True)


def environment(phase):
    runtime = ROOT / "runtime" / phase
    runtime.mkdir(parents=True, exist_ok=False)
    for name in ("tmp", "profile", "appdata", "localappdata", "gopath", "telemetry", "xdg", "empty-hooks"):
        (runtime / name).mkdir()
    (runtime / "telemetry/mode").write_text("off\n", encoding="ascii")
    system = os.environ["SYSTEMROOT"]
    env = {"SYSTEMROOT": system, "WINDIR": system, "COMSPEC": str(Path(system) / "System32/cmd.exe"),
           "PATH": os.pathsep.join([str(GO.parent), str(Path(system) / "System32"), str(GIT.parent)]),
           "GOROOT": r"C:\msys64\ucrt64\lib\go", "GOENV": "off", "GOTOOLCHAIN": "local",
           "GOPROXY": "off", "GOSUMDB": "off", "GOVCS": "*:off", "GOWORK": "off", "GOFLAGS": "-mod=readonly",
           "GOCACHE": str(ROOT / "gocache"), "GOMODCACHE": r"D:\dev\ChatGPT\mcp-pr616-qualification-20261114\gomodcache",
           "GOPATH": str(runtime / "gopath"), "CC": str(GO.with_name("gcc.exe")), "CGO_ENABLED": "1",
           "GOMAXPROCS": "4", "GOTELEMETRY": "off", "TEST_TELEMETRY_DIR": str(runtime / "telemetry"),
           "MCPHUB_NO_AUTO_GUI": "1", "MCPHUB_E2E_SCHEDULER": "none",
           "USERPROFILE": str(runtime / "profile"), "APPDATA": str(runtime / "appdata"),
           "LOCALAPPDATA": str(runtime / "localappdata"), "GIT_OPTIONAL_LOCKS": "0",
           "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": str(runtime / "no-config"),
           "GIT_TERMINAL_PROMPT": "0", "GIT_NO_REPLACE_OBJECTS": "1", "PYTHONDONTWRITEBYTECODE": "1"}
    for key in ("TEMP", "TMP", "TMPDIR", "GOTMPDIR"):
        env[key] = str(runtime / "tmp")
    for key in ("XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"):
        env[key] = str(runtime / "xdg")
    old = {key: os.environ.get(key) for key in ("TEMP", "TMP")}
    try:
        os.environ.update(TEMP=env["TEMP"], TMP=env["TMP"])
        buf = ctypes.create_unicode_buffer(32768)
        size = ctypes.windll.kernel32.GetTempPath2W(len(buf), buf)
        if not size or Path(buf.value) != Path(env["TEMP"]):
            raise RuntimeError("Native temporary path is outside this phase")
    finally:
        for key, value in old.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
    for tool, expected in TOOLS.items():
        if sha(checked(tool).read_bytes()) != expected:
            raise RuntimeError("Preinstalled tool changed: " + str(tool))
    checked(Path(env["GOMODCACHE"]))
    return env


def execute(phase, label, argv, cwd, env, timeout):
    out = ROOT / "evidence" / phase
    out.mkdir(parents=True, exist_ok=True)
    row = {"label": label, "argv": [str(x) for x in argv], "cwd": str(cwd), "started_utc": utc(),
           "timeout_seconds": timeout, "settled": False, "timed_out": False,
           "whole_process_tree_deadline_enforced": False}
    save(out / (label + ".started.json"), row)
    started = time.monotonic()
    with (out / (label + ".stdout.log")).open("xb") as stdout, (out / (label + ".stderr.log")).open("xb") as stderr:
        child = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr)
        row["pid"] = child.pid
        try:
            row["exit_code"] = child.wait(timeout=timeout)
            row["settled"] = True
        except subprocess.TimeoutExpired:
            row["timed_out"] = True
            row["process_tree_unresolved"] = True
            child.terminate()
            try:
                row["exit_code"] = child.wait(timeout=15)
                row["settled"] = True
            except subprocess.TimeoutExpired:
                row["termination_unresolved"] = True
    row.update(seconds=time.monotonic() - started, finished_utc=utc())
    for stream in ("stdout", "stderr"):
        raw = (out / (label + "." + stream + ".log")).read_bytes()
        row[stream] = {"bytes": len(raw), "sha256": sha(raw)}
    save(out / (label + ".receipt.json"), row)
    print(json.dumps(row), flush=True)
    if row.get("exit_code") != 0 or row["timed_out"] or not row["settled"]:
        raise RuntimeError("Command failed: " + label)
    return (out / (label + ".stdout.log")).read_bytes()


def prepare():
    manifest = input_manifest()
    env = environment("prepare")
    command = [GIT, "-c", "core.fsmonitor=false", "-c", "core.autocrlf=false", "-C", BASE_SOURCE]
    identity = execute("prepare", "source-identity", [*command, "rev-parse", "HEAD", "HEAD^{tree}"], ROOT, env, 60).decode().splitlines()
    if identity != [BASE, TREE]:
        raise RuntimeError("Source ref changed")
    raw_tree = execute("prepare", "source-tree", [*command, "ls-tree", "-rz", "--full-tree", "HEAD"], ROOT, env, 60)
    SOURCE.mkdir(exist_ok=False)
    files = {}
    for record in raw_tree.split(b"\0"):
        if not record:
            continue
        meta, raw_path = record.split(b"\t", 1)
        mode, kind, digest = meta.decode().split()
        name = raw_path.decode("utf-8")
        relative = PurePosixPath(name)
        if mode not in ("100644", "100755") or kind != "blob" or relative.is_absolute() or any(x in (".", "..", ".git") for x in relative.parts):
            raise RuntimeError("Unsupported source entry: " + name)
        raw = checked(BASE_SOURCE / name).read_bytes()
        if hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest() != digest:
            raise RuntimeError("Worktree bytes differ from pinned tree: " + name)
        target = SOURCE / name
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("xb") as out:
            out.write(raw)
        files[name] = {"base_blob": digest, "sha256": sha(raw), "bytes": len(raw)}
    for item in manifest["files"]:
        name = item["path"]
        current = files.get(name)
        if (current["sha256"] if current else None) != item["source_sha256"]:
            raise RuntimeError("Overlay source differs: " + name)
        raw = checked(INPUT.parent / "candidate" / name).read_bytes()
        if sha(raw) != item["candidate_sha256"] or len(raw) != item["candidate_bytes"]:
            raise RuntimeError("Overlay bytes differ: " + name)
        target = SOURCE / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(raw)
        files[name] = {"base_blob": item["source_git_blob"], "sha256": sha(raw), "bytes": len(raw), "overlay": True}
    save(ROOT / "composition.json", {"source": BASE, "tree": TREE, "input_sha256": INPUT_SHA, "files": files})
    save(ROOT / "input-manifest.json", manifest)
    print(json.dumps({"prepared_files": len(files), "overlays": sum(x.get("overlay", False) for x in files.values())}), flush=True)


def verify_source():
    manifest = json.loads((ROOT / "composition.json").read_bytes())
    files = {p.relative_to(SOURCE).as_posix(): p for p in SOURCE.rglob("*") if p.is_file()}
    if files.keys() != manifest["files"].keys():
        raise RuntimeError("Composed source inventory changed")
    for name, path in files.items():
        if sha(checked(path).read_bytes()) != manifest["files"][name]["sha256"]:
            raise RuntimeError("Composed source changed: " + name)
    return sha((ROOT / "composition.json").read_bytes())


def qualify(phase):
    if phase not in ("neighbors", "build", "vet"):
        raise RuntimeError("Unknown bounded phase")
    manifest = input_manifest()
    selected = manifest["phases"]["relay-v2-" + phase]["commands"][1]
    before = verify_source()
    env = environment(phase)
    save(ROOT / "evidence" / phase / "input.json", {"composition_sha256": before, "environment": env,
         "runner_sha256": sha(Path(__file__).read_bytes()), "tools": {str(k): v for k, v in TOOLS.items()}})
    names = [SOURCE / item["path"] for item in manifest["files"]]
    if execute(phase, "format", [GO.with_name("gofmt.exe"), "-l", *names], SOURCE, env, 60):
        raise RuntimeError("Formatter reports unformatted source")
    args = selected["args"][:]
    index = args.index("-overlay")
    del args[index:index + 2]
    raw = execute(phase, phase, [GO, *args], SOURCE, env, selected["timeout_seconds"])
    summary = {"phase": phase, "composition_sha256": before, "source_unchanged": verify_source() == before,
               "finished_utc": utc(), "full_go_tests": "NOT_RUN"}
    if phase == "neighbors":
        events = [json.loads(line) for line in raw.splitlines()]
        tests = {action: [e.get("Test", "PACKAGE:" + e.get("Package", "")) for e in events if e.get("Action") == action] for action in ("pass", "fail", "skip")}
        counts = collections.Counter(tests["pass"])
        roots = {k: v for k, v in counts.items() if "/" not in k and not k.startswith("PACKAGE:")}
        if tests["fail"] or tests["skip"] or roots != selected["required_root_pass_counts"] or counts["PACKAGE:mcp-local-hub/internal/daemon"] != 1:
            raise RuntimeError("Neighbor terminal events did not match the frozen selection")
        summary["tests"] = tests
    save(ROOT / "evidence" / phase / "summary.json", summary)
    print(json.dumps(summary), flush=True)


if __name__ == "__main__":
    if os.name != "nt" or len(sys.argv) != 2 or Path(__file__).resolve().parent != ROOT:
        raise SystemExit("Run one fixed phase from the authorized Windows workspace")
    checked(ROOT)
    mode = sys.argv[1]
    if mode == "recover":
        recover()
    elif mode == "prepare":
        prepare()
    else:
        qualify(mode)
