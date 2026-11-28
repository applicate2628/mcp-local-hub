"""Offline, manifest-pinned unit qualification; only direct-child waits are bounded.

Invocation: check_recovery.py {control,candidate,adjacent,build-vet}
            check_recovery.py published <exact-single-parent-commit>
No phase performs native scheduler operations by design of its reviewed suites.
The environment switch is not a runtime sandbox or proof of process-tree exit.
"""
import sys
sys.dont_write_bytecode = True

from collections import Counter
import datetime as dt
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import time

ROOT = Path(r"<RECOVERY_ROOT>")
SOURCE = ROOT / "source"
OLD = Path(r"<INSTALL_SNAPSHOT_ROOT>\source")
BASE = "c55d9ca619f6f9301ba1113615370fc6218d608f"
BASE_TREE = "06a85c6efe53f5a4a8babbcadb7818752bb7acee"
GO = Path(r"C:\msys64\ucrt64\bin\go.exe")
GOFMT = GO.with_name("gofmt.exe")
GIT = Path(r"C:\Program Files\Git\cmd\git.exe")
PATCHES = ("control.patch", "product.patch", "candidate.patch")
TEST_PHASES = ("control", "candidate", "adjacent", "published")
DEFAULT_TIMEOUTS = {"git": 180, "format": 60, "build": 900, "vet": 900, "termination": 15}
RESERVED_LABELS = {
    "clone", "checkout-base", "check-control", "apply-control", "check-product",
    "apply-product", "gofmt-check", "go-version", "diff-check", "build", "vet",
    "stage-exact-files", "fetch-exact-commit", "checkout-detached",
}


def sha(data):
    return hashlib.sha256(data).hexdigest()


def utc():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def moscow():
    return dt.datetime.now(dt.timezone(dt.timedelta(hours=3))).isoformat()


def save_new(path, value):
    with path.open("x", encoding="utf-8", newline="\n") as stream:
        json.dump(value, stream, indent=2, ensure_ascii=False)
        stream.write("\n")


def relative_source_path(value):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_./-]+", value):
        raise ValueError("Invalid source-relative path")
    path = PurePosixPath(value)
    if path.is_absolute() or str(path) != value or any(p in {".", "..", ".git"} for p in path.parts):
        raise ValueError("Source path escapes or aliases the reviewed tree")
    if not value.endswith(".go"):
        raise ValueError("This qualification accepts reviewed Go source paths only")
    return value


def validate_suite(suite):
    if not isinstance(suite, dict):
        raise ValueError("Suite must be an object")
    label = suite.get("label", "")
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,63}", label) or label in RESERVED_LABELS or label.startswith("observation-"):
        raise ValueError("Invalid or reserved suite label")
    package = suite.get("package", "")
    if not re.fullmatch(r"mcp-local-hub/internal/[A-Za-z0-9_/-]+", package) or ".." in package.split("/"):
        raise ValueError("Suite package must be a repository internal package")
    roots = suite.get("roots")
    if not isinstance(roots, list) or not roots or len(set(roots)) != len(roots) or any(not isinstance(n, str) or not re.fullmatch(r"Test[A-Za-z0-9_]+", n) for n in roots):
        raise ValueError("Suite roots must be unique named top-level Go tests")
    for key, low, high in (("count", 1, 20), ("timeout_seconds", 1, 1800), ("direct_timeout_seconds", 1, 3600)):
        value = suite.get(key)
        if type(value) is not int or not low <= value <= high:
            raise ValueError("Invalid suite " + key)
    if suite["direct_timeout_seconds"] <= suite["timeout_seconds"]:
        raise ValueError("Direct-child deadline must leave time for Go's test timeout")
    if type(suite.get("race")) is not bool or type(suite.get("expected_exit_code")) is not int or suite["expected_exit_code"] not in (0, 1):
        raise ValueError("Invalid race or exit oracle")
    expected = suite.get("expected_events")
    if not isinstance(expected, dict) or set(expected) != {"pass", "fail", "skip"}:
        raise ValueError("Exact pass/fail/skip event maps are required")
    terminal_names = set()
    for action, counts in expected.items():
        if not isinstance(counts, dict):
            raise ValueError("Event counts must be objects")
        for name, count in counts.items():
            if not isinstance(name, str) or type(count) is not int or count < 1:
                raise ValueError("Invalid event count")
            if name in terminal_names:
                raise ValueError("One test cannot have two terminal outcomes")
            terminal_names.add(name)
            if name.startswith("PACKAGE:"):
                if name != "PACKAGE:" + package or count != 1:
                    raise ValueError("Package event oracle must match exactly once")
            elif name.split("/", 1)[0] not in roots or count != suite["count"]:
                raise ValueError("Test event is outside the selected roots or repeat count")
    if not set(roots).issubset(terminal_names):
        raise ValueError("Every selected root requires a terminal-event oracle")
    package_action = "fail" if suite["expected_exit_code"] else "pass"
    if expected[package_action].get("PACKAGE:" + package) != 1:
        raise ValueError("Missing package terminal-event oracle")
    required = suite.get("required_output", [])
    if not isinstance(required, list):
        raise ValueError("Required-output oracle must be an array")
    for item in required:
        if not isinstance(item, dict) or item.get("test") not in terminal_names:
            raise ValueError("Required-output test is outside the event oracle")
        markers = item.get("any_of")
        if not isinstance(markers, list) or not markers or any(not isinstance(m, str) or not m or len(m) > 500 for m in markers):
            raise ValueError("Required-output markers must be bounded nonempty strings")
    if suite["expected_exit_code"] == 1 and not required:
        raise ValueError("Negative control requires a behavioral failure marker")


def validate_manifest(manifest):
    if manifest.get("schema") != 2 or manifest.get("base_commit") != BASE or manifest.get("base_tree") != BASE_TREE:
        raise ValueError("Unexpected source manifest version or base identity")
    files = manifest.get("files")
    if not isinstance(files, list) or not 1 <= len(files) <= 64:
        raise ValueError("Expected a bounded explicit source-file list")
    paths = []
    for item in files:
        if not isinstance(item, dict):
            raise ValueError("Manifest file entry must be an object")
        paths.append(relative_source_path(item.get("path")))
        for prefix in ("base", "candidate"):
            digest, blob = item.get(prefix + "_sha256"), item.get(prefix + "_blob")
            if (digest is None) != (blob is None):
                raise ValueError("Incomplete file identity")
            if digest is not None and (not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest) or not isinstance(blob, str) or not re.fullmatch(r"[0-9a-f]{40}", blob)):
                raise ValueError("Invalid source digest")
        if item["base_sha256"] == item["candidate_sha256"]:
            raise ValueError("Manifest contains an unchanged or absent file")
    if len(set(paths)) != len(paths):
        raise ValueError("Duplicate source path")
    controls = manifest.get("control_paths")
    if not isinstance(controls, list) or not controls or len(set(controls)) != len(controls) or not set(controls).issubset(paths):
        raise ValueError("Invalid control-test subset")
    if any(not p.startswith("internal/api/") or not p.endswith("_test.go") for p in controls):
        raise ValueError("Control may change only reviewed API test files")
    if any(item["candidate_sha256"] is None for item in files if item["path"] in controls):
        raise ValueError("Control tests cannot be deleted")
    if manifest.get("native_scheduler_operations") is not False:
        raise ValueError("Native scheduler execution is outside this runner")
    suites = manifest.get("suites")
    if not isinstance(suites, dict) or set(suites) != set(TEST_PHASES):
        raise ValueError("All four test phases must be frozen in the manifest")
    for phase, selections in suites.items():
        if not isinstance(selections, list) or not selections:
            raise ValueError("Each test phase needs a nonempty suite list")
        labels = set()
        for suite in selections:
            validate_suite(suite)
            if suite["label"] in labels:
                raise ValueError("Duplicate suite label within phase")
            labels.add(suite["label"])
            if phase == "control":
                if suite["package"] != "mcp-local-hub/internal/api" or suite["count"] != 1 or suite["race"] or suite["expected_exit_code"] != 1:
                    raise ValueError("Control must be one-pass non-race negative API tests")
            elif suite["expected_exit_code"] != 0:
                raise ValueError("Candidate, adjacent and published suites must pass")
    timeouts = manifest.get("command_timeouts")
    if not isinstance(timeouts, dict) or set(timeouts) != set(DEFAULT_TIMEOUTS):
        raise ValueError("Command deadlines must be frozen")
    if any(type(n) is not int or not 1 <= n <= 3600 for n in timeouts.values()):
        raise ValueError("Invalid command deadline")
    return files


def test_events(raw):
    events = []
    for line in raw.decode("utf-8").splitlines():
        event = json.loads(line)
        if not isinstance(event, dict):
            raise ValueError("Invalid Go JSON event")
        events.append(event)
    return events


def terminal_events(events):
    return {action: [e.get("Test", "PACKAGE:" + e.get("Package", "")) for e in events if e.get("Action") == action] for action in ("pass", "fail", "skip")}


def check_test_oracle(row, suite, events):
    if not row.get("settled") or row.get("timed_out") or row.get("error_type") or any(k in row for k in ("source_after_error", "stdout_capture_error", "stderr_capture_error")) or row.get("exit_code") != suite["expected_exit_code"]:
        raise RuntimeError("Test command did not settle with the required exit")
    observed = terminal_events(events)
    if row.get("tests") != observed:
        raise RuntimeError("Test receipt differs from raw events")
    if any(Counter(observed[a]) != Counter(suite["expected_events"][a]) for a in observed):
        raise RuntimeError("Unexpected test selection, skips or terminal-event counts")
    if any(e.get("Package") != suite["package"] for e in events):
        raise RuntimeError("Go JSON stream contains another package")
    for requirement in suite.get("required_output", []):
        output = "".join(e.get("Output", "") for e in events if e.get("Test") == requirement["test"])
        if not any(marker in output for marker in requirement["any_of"]):
            raise RuntimeError("Negative control failed for a different cause: " + requirement["test"])


def expected_status(manifest, state):
    if state in ("base", "published"):
        return set()
    result = set()
    for item in manifest["files"]:
        if state == "control" and item["path"] not in manifest["control_paths"]:
            continue
        code = "?? " if item["base_sha256"] is None else " D " if item["candidate_sha256"] is None else " M "
        result.add(code + item["path"])
    return result


class Runner:
    def __init__(self, phase):
        self.phase = phase
        self.out = ROOT / "evidence" / phase
        self.runtime = ROOT / "runtime" / phase
        self.receipt = {
            "schema": 2, "phase": phase, "base": BASE, "base_tree": BASE_TREE,
            "started_utc": utc(), "started_moscow": moscow(), "state": "preflight",
            "completed": False, "runner_sha256": sha(Path(__file__).read_bytes()),
            "commands": [], "observations": [], "native_task_operations": False,
            "native_task_operations_basis": "Reviewed frozen unit-test selection and MCPHUB_E2E_SCHEDULER=none; not syscall enforcement",
            "whole_process_tree_deadline_enforced": False,
            "direct_child_wait_bounded": True, "process_tree_settlement_observed": False,
        }
        self.observation_id = 0
        self.out_created = False
        self.env = None
        self.manifest = None

    def begin(self):
        if os.name != "nt" or sys.flags.optimize or Path(__file__).resolve().parent != ROOT:
            raise RuntimeError("Runner must be used unoptimized from the approved Windows task root")
        self.out.mkdir(parents=True, exist_ok=False)
        self.out_created = True
        self.runtime.mkdir(parents=True, exist_ok=False)
        for name in ("tmp", "profile", "appdata", "localappdata", "gopath", "empty-hooks", "xdg-config", "xdg-cache", "xdg-data"):
            (self.runtime / name).mkdir(exist_ok=False)
        env = {k: v for k, v in os.environ.items() if not k.upper().startswith(("MCPHUB_", "GIT_")) and k.upper() != "SSH_ASKPASS"}
        system = Path(next(v for k, v in env.items() if k.upper() == "SYSTEMROOT"))
        ps = system / "System32/WindowsPowerShell/v1.0/powershell.exe"
        env.update({
            "GOROOT": r"C:\msys64\ucrt64\lib\go", "GOENV": "off", "GOTOOLCHAIN": "local",
            "GOPROXY": "off", "GOSUMDB": "off", "GOVCS": "*:off", "GOWORK": "off",
            "GOFLAGS": "-mod=readonly", "GOCACHE": r"<WORK_ROOT>\mcp-pr616-qualification-20261114\gocache",
            "GOMODCACHE": r"<WORK_ROOT>\mcp-pr616-qualification-20261114\gomodcache",
            "GOPATH": str(self.runtime / "gopath"), "CC": str(GO.with_name("gcc.exe")),
            "CGO_ENABLED": "1", "GOMAXPROCS": "4", "GOTELEMETRY": "off",
            "MCPHUB_NO_AUTO_GUI": "1", "MCPHUB_E2E_SCHEDULER": "none",
            "USERPROFILE": str(self.runtime / "profile"), "APPDATA": str(self.runtime / "appdata"),
            "LOCALAPPDATA": str(self.runtime / "localappdata"), "TEMP": str(self.runtime / "tmp"),
            "TMP": str(self.runtime / "tmp"), "TMPDIR": str(self.runtime / "tmp"),
            "GOTMPDIR": str(self.runtime / "tmp"), "XDG_CONFIG_HOME": str(self.runtime / "xdg-config"),
            "XDG_CACHE_HOME": str(self.runtime / "xdg-cache"), "XDG_DATA_HOME": str(self.runtime / "xdg-data"),
            "PSModuleAnalysisCachePath": str(self.runtime / "localappdata/module-cache"),
            "PYTHONDONTWRITEBYTECODE": "1", "GIT_OPTIONAL_LOCKS": "0", "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": str(self.runtime / "no-global-config"), "GIT_TERMINAL_PROMPT": "0",
            "GCM_INTERACTIVE": "never",
        })
        env["PATH"] = os.pathsep.join((str(GO.parent), str(ps.parent), str(system / "System32"), str(GIT.parent), env.get("PATH", "")))
        self.env = env
        for path in (GO, GOFMT, GIT, Path(env["GOROOT"]), Path(env["CC"]), Path(env["GOCACHE"]), Path(env["GOMODCACHE"])):
            if not path.exists():
                raise RuntimeError("Missing pre-existing tool/cache: " + str(path))
        raw = (ROOT / "source-manifest.json").read_bytes()
        self.manifest = json.loads(raw)
        self.files = validate_manifest(self.manifest)
        self.paths = [item["path"] for item in self.files]
        if self.manifest.get("execution_runner_sha256") != self.receipt["runner_sha256"]:
            raise RuntimeError("Execution runner changed after manifest freeze")
        if self.manifest.get("generator_sha256") != sha((ROOT / "prepare_source.py").read_bytes()):
            raise RuntimeError("Source preparation script changed after manifest freeze")
        patches = {name: sha((ROOT / name).read_bytes()) for name in PATCHES}
        if patches != self.manifest.get("patch_sha256"):
            raise RuntimeError("Patch bytes differ from manifest")
        self.receipt.update(
            manifest_sha256=sha(raw), patch_sha256=patches,
            source_plan_sha256=self.manifest["source_plan_sha256"],
            command_timeouts=self.manifest["command_timeouts"],
            environment={k: env[k] for k in (
                "GOENV", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GOVCS", "GOWORK", "GOFLAGS",
                "GOTELEMETRY", "GOMAXPROCS", "CGO_ENABLED", "MCPHUB_NO_AUTO_GUI",
                "MCPHUB_E2E_SCHEDULER", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP",
                "GOCACHE", "GOMODCACHE", "GOTMPDIR", "XDG_CACHE_HOME",
            )},
            tools_sha256={str(p): sha(p.read_bytes()) for p in (GO, GOFMT, GIT, Path(env["CC"]))},
        )
        save_new(self.out / "receipt-initial.json", self.receipt)
        self.receipt["state"] = "running"

    def git_command(self, path, args):
        return [
            str(GIT), "-c", "core.autocrlf=false", "-c", "core.hooksPath=" + str(self.runtime / "empty-hooks"),
            "-c", "credential.helper=", "-c", "core.askPass=", "-c", "core.fsmonitor=false",
            "-c", "maintenance.auto=false", "-c", "gc.auto=0", "-C", str(path), *args,
        ]

    def execute(self, label, command, cwd, timeout, source_before=None, quiet=False):
        row = {
            "label": label, "executable": command[0], "args": command[1:], "cwd": str(cwd),
            "started_utc": utc(), "started_moscow": moscow(), "settled": False, "timed_out": False,
            "direct_timeout_seconds": timeout, "whole_process_tree_deadline_enforced": False,
            "runner_sha256": self.receipt["runner_sha256"], "manifest_sha256": self.receipt.get("manifest_sha256"),
        }
        if source_before is not None:
            row["source_before"] = source_before
        save_new(self.out / (label + ".started.json"), row)
        if not quiet:
            print("START " + label, flush=True)
        child = None
        started = time.monotonic()
        try:
            with (self.out / (label + ".stdout.log")).open("xb") as stdout, (self.out / (label + ".stderr.log")).open("xb") as stderr:
                child = subprocess.Popen(command, cwd=cwd, env=self.env, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr)
                row["pid"] = child.pid
                save_new(self.out / (label + ".launched.json"), {**row, "launched_utc": utc()})
                try:
                    row["exit_code"] = child.wait(timeout=timeout)
                    row["settled"] = True
                except subprocess.TimeoutExpired:
                    row["timed_out"] = True
                    row["error_type"] = "TimeoutExpired"
                    row["error"] = "Direct-child deadline expired; descendant state is unresolved"
        except BaseException as error:
            row.update(error_type=type(error).__name__, error=str(error))
        finally:
            if child is not None and not row["settled"]:
                # Popen owns the original Windows process handle. Never resolve
                # a PID again, call taskkill, or infer that children were killed.
                row["termination_method"] = "Popen.terminate on retained direct-child handle"
                row["process_tree_unresolved"] = True
                self.receipt["process_tree_unresolved"] = True
                try:
                    child.terminate()
                    row["exit_code"] = child.wait(timeout=self.manifest["command_timeouts"]["termination"])
                    row["settled"] = True
                except BaseException as error:
                    row["termination_error"] = type(error).__name__ + ": " + str(error)
                row["retain_runtime_and_logs"] = True
            row.update(seconds=time.monotonic() - started, finished_utc=utc(), finished_moscow=moscow())
            for stream in ("stdout", "stderr"):
                path = self.out / (label + "." + stream + ".log")
                if path.exists():
                    try:
                        raw = path.read_bytes()
                        row[stream] = {
                            "bytes": len(raw), "sha256": sha(raw), "captured_utc": utc(),
                            "hash_scope": "observed bytes; later writers unresolved" if row.get("process_tree_unresolved") else "bytes observed after direct-child exit",
                        }
                    except Exception as error:
                        row[stream + "_capture_error"] = type(error).__name__ + ": " + str(error)
        return row

    def git_at(self, path, *args):
        self.observation_id += 1
        label = "observation-%04d" % self.observation_id
        row = self.execute(label, self.git_command(path, args), path, self.manifest["command_timeouts"]["git"], quiet=True)
        self.receipt["observations"].append(row)
        save_new(self.out / (label + ".receipt.json"), row)
        self.require_pass(row)
        return (self.out / (label + ".stdout.log")).read_text(encoding="utf-8").rstrip("\r\n")

    def snapshot(self, path=SOURCE):
        result = {"exists": path.exists(), "sha256": {p: sha((path / p).read_bytes()) if (path / p).is_file() else None for p in self.paths}}
        if (path / ".git").exists():
            result.update(head=self.git_at(path, "rev-parse", "HEAD"), tree=self.git_at(path, "rev-parse", "HEAD^{tree}"))
            result["materialized"] = (path / "go.mod").is_file()
            if result["materialized"]:
                result.update(
                    parents=self.git_at(path, "show", "-s", "--format=%P", "HEAD").split(),
                    staged=self.git_at(path, "diff", "--cached", "--name-only"),
                    status=self.git_at(path, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching"),
                )
        return result

    def admit(self, state, expected=BASE, path=SOURCE):
        value = self.snapshot(path)
        if value.get("head") != expected or value.get("staged") != "" or set(value.get("status", "").splitlines()) != expected_status(self.manifest, state):
            raise RuntimeError("Unexpected head, index, tracked, untracked or ignored source state")
        if expected == BASE and value["tree"] != BASE_TREE:
            raise RuntimeError("Unexpected base tree")
        for item in self.files:
            candidate = state in {"candidate", "published"} or (state == "control" and item["path"] in self.manifest["control_paths"])
            if value["sha256"][item["path"]] != item["candidate_sha256" if candidate else "base_sha256"]:
                raise RuntimeError("Source byte mismatch: " + item["path"])
        if state == "published":
            if value["parents"] != [BASE] or set(self.git_at(path, "diff", "--name-only", BASE, expected).splitlines()) != set(self.paths):
                raise RuntimeError("Published commit is not the standalone single-parent change")
        return value

    @staticmethod
    def require_pass(row):
        if not row.get("settled") or row.get("timed_out") or row.get("error_type") or row.get("exit_code") != 0 or row.get("tests", {}).get("fail") or any(k in row for k in ("source_after_error", "stdout_capture_error", "stderr_capture_error")):
            raise RuntimeError(row["label"] + " failed")

    def run(self, label, executable, args, timeout, cwd=SOURCE):
        before = self.snapshot()
        command = self.git_command(cwd, args) if executable == GIT else [str(executable), *args]
        row = self.execute(label, command, cwd, timeout, before)
        self.receipt["commands"].append(row)
        try:
            if executable == GO and args and args[0] == "test" and row.get("settled") and not row.get("timed_out"):
                row["tests"] = terminal_events(test_events((self.out / (label + ".stdout.log")).read_bytes()))
        except Exception as error:
            row.update(event_error_type=type(error).__name__, event_error=str(error))
        try:
            row["source_after"] = self.snapshot()
        except Exception as error:
            row["source_after_error"] = type(error).__name__ + ": " + str(error)
        save_new(self.out / (label + ".receipt.json"), row)
        print(json.dumps({k: row.get(k) for k in ("label", "exit_code", "seconds", "settled", "timed_out", "process_tree_unresolved")}), flush=True)
        return row

    def git_run(self, label, args, cwd=SOURCE):
        row = self.run(label, GIT, args, self.manifest["command_timeouts"]["git"], cwd)
        self.require_pass(row)
        return row

    def format_check(self):
        paths = [p for p in self.paths if (SOURCE / p).is_file()]
        row = self.run("gofmt-check", GOFMT, ["-l", *paths], self.manifest["command_timeouts"]["format"])
        self.require_pass(row)
        if (self.out / "gofmt-check.stdout.log").read_text(encoding="utf-8").strip():
            raise RuntimeError("Source needs gofmt; no formatting mutation performed")

    def run_suites(self, phase):
        for suite in self.manifest["suites"][phase]:
            args = [
                "test", "-json", "-tags=test_state_path_env", *(["-race"] if suite["race"] else []),
                "-count=" + str(suite["count"]), "-timeout=" + str(suite["timeout_seconds"]) + "s",
                "-run", "^(" + "|".join(re.escape(n) for n in suite["roots"]) + ")$",
                "./" + suite["package"].removeprefix("mcp-local-hub/"),
            ]
            row = self.run(suite["label"], GO, args, suite["direct_timeout_seconds"])
            check_test_oracle(row, suite, test_events((self.out / (suite["label"] + ".stdout.log")).read_bytes()))

    def admit_retained_control(self):
        directory = ROOT / "evidence/control"
        raw = (directory / "receipt.json").read_bytes()
        prior = json.loads(raw)
        if prior.get("base") != BASE or not prior.get("completed") or not prior.get("negative_control_confirmed") or prior.get("process_tree_unresolved") or prior.get("runner_sha256") != self.receipt["runner_sha256"] or prior.get("manifest_sha256") != self.receipt["manifest_sha256"]:
            raise RuntimeError("Retained negative control is not admitted")
        for suite in self.manifest["suites"]["control"]:
            rows = [r for r in prior["commands"] if r["label"] == suite["label"]]
            if len(rows) != 1:
                raise RuntimeError("Retained control command is missing or duplicated")
            row = rows[0]
            for stream in ("stdout", "stderr"):
                saved = (directory / (suite["label"] + "." + stream + ".log")).read_bytes()
                if sha(saved) != row[stream]["sha256"] or len(saved) != row[stream]["bytes"]:
                    raise RuntimeError("Retained negative-control stream changed")
            check_test_oracle(row, suite, test_events((directory / (suite["label"] + ".stdout.log")).read_bytes()))
        self.receipt.update(retained_control_receipt_sha256=sha(raw), control_rerun=False)

    def finish(self, error=None):
        if error is not None:
            self.receipt.update(state="failed", error_type=type(error).__name__, error=str(error), completed=False)
        if self.manifest is not None and self.env is not None and hasattr(self, "paths"):
            try:
                self.receipt["source_final"] = self.snapshot()
                if self.receipt["completed"] and self.receipt["source_final"] != self.receipt.get("source_after"):
                    raise RuntimeError("Source drifted after phase admission")
            except Exception as failure:
                self.receipt.update(completed=False, state="failed", source_final_error=type(failure).__name__ + ": " + str(failure))
        self.receipt.update(finished_utc=utc(), finished_moscow=moscow())
        self.receipt.update(
            source_sha256_before=self.receipt.get("source_before", {}).get("sha256"),
            source_sha256_after=self.receipt.get("source_final", {}).get("sha256"),
        )
        if self.out_created:
            save_new(self.out / "receipt.json", self.receipt)
        print(json.dumps({k: self.receipt.get(k) for k in ("phase", "state", "completed", "error", "negative_control_confirmed", "source_head", "process_tree_unresolved")}), flush=True)


def qualify(runner, published_commit=None):
    phase = runner.phase
    if phase == "control":
        if SOURCE.exists():
            raise RuntimeError("Destination already exists")
        runner.receipt["clone_source"] = runner.admit("base", path=OLD)
        runner.git_run("clone", ["clone", "--single-branch", "--local", "--no-hardlinks", "--no-checkout", str(OLD), str(SOURCE)], ROOT)
        runner.git_run("checkout-base", ["checkout", "--detach", BASE])
        runner.receipt["source_before"] = runner.admit("base")
        runner.git_run("check-control", ["apply", "--check", str(ROOT / "control.patch")])
        runner.git_run("apply-control", ["apply", str(ROOT / "control.patch")])
        runner.receipt["control_source"] = runner.admit("control")
        declared = set()
        for path in runner.manifest["control_paths"]:
            declared.update(re.findall(r"^func (Test\w+)\(t \*testing.T\)", (SOURCE / path).read_text(encoding="utf-8"), re.M))
        selected = {name for suite in runner.manifest["suites"]["control"] for name in suite["roots"]}
        if not selected.issubset(declared):
            raise RuntimeError("A selected negative-control root is not in the reviewed control files")
    else:
        state = "published" if phase == "published" else "control" if phase == "candidate" else "candidate"
        runner.receipt["source_before"] = runner.admit(state, published_commit or BASE)
        if phase == "candidate":
            runner.admit_retained_control()
            runner.git_run("check-product", ["apply", "--check", str(ROOT / "product.patch")])
            runner.git_run("apply-product", ["apply", str(ROOT / "product.patch")])
            runner.receipt["candidate_source"] = runner.admit("candidate")
        elif phase == "published":
            raw = (ROOT / "publication-input.json").read_bytes()
            publication = json.loads(raw)
            if publication.get("schema") != 1 or publication.get("base") != BASE or publication.get("parents") != [BASE] or publication.get("commit") != published_commit or publication.get("tree") != runner.receipt["source_before"]["tree"] or publication.get("source_manifest_sha256") != runner.receipt["manifest_sha256"] or publication.get("execution_runner_sha256") != runner.receipt["runner_sha256"]:
                raise RuntimeError("Publication identities differ from the reviewed execution")
            runner.receipt["publication_input_sha256"] = sha(raw)
    runner.format_check()
    runner.require_pass(runner.run("go-version", GO, ["version"], runner.manifest["command_timeouts"]["format"]))
    if phase == "build-vet":
        for name in ("build", "vet"):
            runner.require_pass(runner.run(name, GO, [name, "./..."], runner.manifest["command_timeouts"][name]))
    else:
        runner.run_suites(phase)
        if phase == "control":
            runner.receipt["negative_control_confirmed"] = True
    runner.git_run("diff-check", ["diff", "--check"])
    state = "control" if phase == "control" else "published" if phase == "published" else "candidate"
    after = runner.admit(state, published_commit or BASE)
    runner.receipt.update(source_after=after, source_head=after["head"], source_tree=after["tree"], working_status=after["status"], completed=True, state="complete")


def main():
    phase = sys.argv[1] if len(sys.argv) > 1 else ""
    if phase not in {*TEST_PHASES, "build-vet"} or len(sys.argv) != (3 if phase == "published" else 2):
        raise SystemExit("Expected one phase, with one exact commit only for published")
    commit = sys.argv[2] if phase == "published" else None
    if commit is not None and not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise SystemExit("Published commit must be a full SHA")
    runner = Runner(phase)
    failure = None
    try:
        runner.begin()
        qualify(runner, commit)
    except BaseException as error:
        failure = error
    finally:
        runner.finish(failure)
    if failure is not None or not runner.receipt["completed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
