"""Independently replay the A3 evidence without Go, network or project execution.

Usage: python audit.py --archive raw-evidence.zip --git-repo REPOSITORY --output audit-recheck.json
The immutable Git base must already exist. Only the new output JSON is written.
Archived Python is inspected as data, never imported or executed.
"""
import sys
sys.dont_write_bytecode = True
import argparse
from collections import Counter, defaultdict
import copy
import datetime
import hashlib
import io
import json
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import subprocess
import zipfile

ARCHIVE_SHA = "43eb0e795b1a51eba75435f4bc697acb9994f1aa513dcc1a6c2721409fabced8"
ARCHIVE_BYTES = 3820388
BASE_HEAD = "65fea0b567a80f8793ccd417e9503c674e340f72"
BASE_TREE = "67991d80a424d61d2eecff9b72f7ca20ae31febb"
CANDIDATE_TREE = "217c889965369081667b782fec606a21dd912a95"
FINAL_TREE = "d398dc284d85722bc3ffafa5eaafdacf88eaf8c4"
V1_PLAN = "7a1a2dee75252fd0caa5360f1a30cb464a5b2aa2e43efffb15a1a155ab209b2c"
V2_PLAN = "f99660c7981f3f2c8f545fd75295878dbf3e6cc94dd4a79be182eb3aa7b1d9ab"
FINAL_PLAN = "e8b693645a6fe9023eadc505a6070e12ef8eed5f2ca008cd6aa7efe9a81c9b72"
RUNNER_SHA = "a483536d689df8c5713fddbdfd221b85953cf0ce4f9d578a31c4cdbfc6b9a26e"
BASELINE_SHA = "f7a7ad049c2e27b0df8fca68e1cb772909d3eed504fbb6ccf07ffdc0f0a0dd7a"
FINAL_BASELINE_SHA = "5b856cb5d791a2edd9cf8af861533a8537b274c79efdd369aaf16ac97b254ae2"
REMOTE = PureWindowsPath(r"D:\dev\ChatGPT\mcp-residual-next-20261122-dcb40fba")
TOOLS = {
    r"C:\msys64\ucrt64\bin\go.exe": "0176dbac02ff27ef54c6bc5b316c7c2a2a89f7c8cf5fd314a0e03d6d2b924113",
    r"C:\msys64\ucrt64\bin\gofmt.exe": "95368e4aafefa20f962a345ab02f53ffc035ed69349612abbf385b52f4fb25c5",
    r"C:\msys64\ucrt64\bin\gcc.exe": "f6ff9378c1edb591131304518eac6456708d28720284105930d0d687136bcdd3",
}
GUI_PATHS = ["internal/gui/client_install_prefs.go", "internal/gui/client_install_prefs_test.go", "internal/gui/server.go"]
ROOTS = [
    "TestClientInstallPrefs_UsesServerAPI", "TestClientInstallPrefs_ServersKeepIndependentDependencies",
    "TestClientInstallPrefs_GetDefault", "TestClientInstallPrefs_PostPersistsAndReturnsSnapshot",
    "TestClientInstallPrefs_PostUnknownClientIs400", "TestClientInstallPrefs_PostEmptySetIs400",
    "TestClientInstallPrefs_MethodNotAllowed", "TestClientInstallPrefs_CrossOriginRejected",
]
STEPS = ["first_initial_read", "second_initial_read", "first_mutation", "second_after_first_mutation",
         "second_mutation", "first_after_second_mutation"]
CONTROL_FAILURES = {
    "first_initial_read": "selected=[vscode], want [cursor]",
    "second_after_first_mutation": "selected=[claude-code], want [vscode]",
    "first_after_second_mutation": "selected=[codex-cli], want [claude-code]",
}
REMOVED = {
    "24d9c30a73077273cecb80e7e46f46eb1e787d5d4579b9e25488b8f661056a9d": ("mutable_global", "clientInstallPrefsViewFn"),
    "2b533332b231b11e1941ec5d908bd5b5961849a9243135a0d222411590c436fd": ("api_construction", "clientInstallPrefsSetNames"),
    "3c49a37917a4fa67c001fbf39954303f9f54f3ea27af7c6ae38d8180b4198846": ("api_construction", "clientInstallPrefsView"),
    "bc262b12501bc20b069a110fa95df448fe5019a08dd3daa01959b28664eb9c15": ("mutable_global", "clientInstallPrefsSetFn"),
}
SERVER_BUDGET = "e7f29b64ccfbb97e6553b041f6a4c1d9952da9e14de4033f9da9820e976d1d88"
BASE_COUNTERS = {"expired": 0, "grown": 42, "new": 387, "stale": 128, "unowned": 0, "workers": 72}
TRANSFER_PINS = {
    "a3-source-readback.zip": (1253960, "4e7324f4b2720d48fcfcb38f73f53145e595d91d779dd9e497f0153fb38107d5"),
    "a3-gui-independence.zip": (11615, "d1944a83af5f2114e05ae644774faacea0277647d1b51ec78b3dd7fbf3e303ae"),
    "a3-base-architecture.zip": (192408, "0740c247059d772e022309c5700fc0bf768fae667a6470fcc168fb18e2059bba"),
    "a3-candidate-architecture.zip": (192229, "91969e0de86265d6a9eaf196d6e4e00ec8c89fee7091dc3c289d0a5f3a4a82a1"),
    "a3-prune-inputs.zip": (183276, "88fad0852131f0735593b42f4703ea6be5d8695862a3cb023a5505af5efa27bd"),
    "a3-pruned-architecture.zip": (8096, "054346413d0d3e2dc9cda40711b8e18ee808a95d7db15ce9d56bf575f2cdb2ba"),
}


def need(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def encode(value):
    return (json.dumps(value, indent=2, ensure_ascii=False) + "\n").encode()


def safe_name(name):
    path = PurePosixPath(name)
    need(name and not path.is_absolute() and path.as_posix() == name and
         all(part not in (".", "..", ".git") for part in path.parts) and
         "\\" not in name and ":" not in name, "Unsafe relative artifact path")
    return path


def simple(path, raw):
    return {"path": path, "bytes": len(raw), "sha256": sha(raw)}


def rich(path, raw):
    safe_name(path)
    return {"path": path, "mode": "100644", "git_blob": hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest(),
            "bytes": len(raw), "sha256": sha(raw)}


def rows(contents):
    return [rich(path, contents[path]) for path in sorted(contents)]


def simplify(values):
    return [{key: value[key] for key in ("path", "bytes", "sha256")} for value in values]


def inventory(values):
    data = [[value["path"], value["bytes"], value["sha256"]] for value in values]
    return {"files": len(data), "bytes": sum(value[1] for value in data),
            "inventory_sha256": sha(json.dumps(data, separators=(",", ":"), ensure_ascii=True).encode())}


def tree(values):
    root = {}
    folded = set()
    for value in values:
        path = safe_name(value["path"])
        need(value["mode"] == "100644" and value["path"].casefold() not in folded, "Git mode or folded name differs")
        folded.add(value["path"].casefold())
        node = root
        for name in path.parts[:-1]:
            node = node.setdefault(name, {})
            need(isinstance(node, dict), "Git file/directory collision")
        need(path.name not in node, "Duplicate Git path")
        node[path.name] = bytes.fromhex(value["git_blob"])

    def digest(node):
        body = b""
        for name, value in sorted(node.items(), key=lambda item: item[0].encode() + (b"/" if isinstance(item[1], dict) else b"")):
            body += (b"40000" if isinstance(value, dict) else b"100644") + b" " + name.encode() + b"\0"
            body += digest(value) if isinstance(value, dict) else value
        return hashlib.sha1(b"tree " + str(len(body)).encode() + b"\0" + body).digest()
    return digest(root).hex()


def git_base(repository):
    def git(*arguments, data=None):
        return subprocess.run(["git", *arguments], cwd=repository, input=data, check=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout
    need(git("rev-parse", BASE_HEAD + "^{tree}").decode().strip() == BASE_TREE, "Wrong immutable Git base")
    entries = []
    for record in git("ls-tree", "-r", "-z", "--full-tree", BASE_HEAD).split(b"\0"):
        if record:
            header, name = record.split(b"\t", 1)
            mode, kind, oid = header.decode().split()
            need(mode == "100644" and kind == "blob", "Unexpected immutable Git entry type")
            entries.append((name.decode(), oid))
    stream = io.BytesIO(git("cat-file", "--batch", data="".join(oid + "\n" for _, oid in entries).encode()))
    contents = {}
    for path, oid in entries:
        found, kind, size = stream.readline().decode().split()
        raw = stream.read(int(size))
        need(found == oid and kind == "blob" and stream.read(1) == b"\n" and rich(path, raw)["git_blob"] == oid,
             "Git framing or full blob identity differs")
        contents[path] = raw
    need(not stream.read() and len(contents) == 3283 and sum(map(len, contents.values())) == 55719013,
         "Incomplete immutable Git base")
    need(tree(rows(contents)) == BASE_TREE, "Independent reconstructed base Git root differs")
    return contents


def unpack(raw):
    with zipfile.ZipFile(io.BytesIO(raw)) as zipped:
        names = zipped.namelist()
        need(len(names) == len(set(names)), "Duplicate archive member")
        for entry in zipped.infolist():
            safe_name(entry.filename)
            need(not entry.is_dir() and not entry.flag_bits & 1, "Unexpected archive directory/encryption")
        return {name: zipped.read(name) for name in names}


def check_rows(files, values):
    need(len(values) == len({value["path"] for value in values}), "Duplicate inventory path")
    for value in values:
        need(simple(value["path"], files[value["path"]]) == value, "Artifact content differs: " + value["path"])


def test_expectation(control):
    package = "PACKAGE:mcp-local-hub/internal/gui"
    children = [ROOTS[1] + "/" + step for step in STEPS]
    if not control:
        return {"pass": ROOTS + children + [package], "fail": [], "skip": []}
    failed = [ROOTS[1] + "/" + step for step in CONTROL_FAILURES]
    return {"pass": [name for name in children if name not in failed], "fail": [ROOTS[1], package] + failed, "skip": []}


def test_events(raw, receipt, control):
    actions = Counter()
    terminal = {name: [] for name in ("pass", "fail", "skip")}
    runs = []
    output = defaultdict(str)
    for line in raw.splitlines():
        event = json.loads(line)
        need(isinstance(event, dict) and event.get("Package") == "mcp-local-hub/internal/gui", "Unexpected test event/package")
        action = event["Action"]
        need(action in ("start", "run", "output", "pass", "fail", "skip", "pause", "cont"), "Unknown test action")
        name = event.get("Test", "PACKAGE:mcp-local-hub/internal/gui")
        actions[action] += 1
        if action in terminal:
            terminal[action].append(name)
        if action == "run":
            runs.append(name)
        if action == "output":
            output[name] += event.get("Output", "")
    expected = test_expectation(control)
    for action in terminal:
        need(Counter(terminal[action]) == Counter(expected[action]), "Exact raw terminal set differs: " + action)
    named = [name for values in expected.values() for name in values if not name.startswith("PACKAGE:")]
    need(Counter(runs) == Counter(named) and actions["start"] == 1, "Tests/package did not start exactly once")
    need(receipt["tests"] == terminal, "Raw events and command receipt disagree")
    need(b"WARNING: DATA RACE" not in raw and b"race detected during execution of test" not in raw, "Race failure is not an admitted control")
    if control:
        for step, cause in CONTROL_FAILURES.items():
            text = output[ROOTS[1] + "/" + step]
            need(cause in text and "status=" not in text and "want an active override" not in text,
                 "Control did not reach its exact shared-selection assertion")
    return {"counts": {key: len(value) for key, value in terminal.items()}, "terminals": terminal,
            "named_events": len(named), "leaf_cases": len(named) - (1 if control else 1),
            "exact_runs_and_terminals_verified": True, "event_actions": dict(actions),
            "failed_outputs": {name: output[name] for name in terminal["fail"]},
            "test_output_sha256": {name: sha(value.encode()) for name, value in sorted(output.items())}}


def verification_line(counters):
    return "architecture verification: " + " ".join(key + "=" + str(counters[key]) for key in sorted(counters))


def baseline_blocks(raw):
    markers = list(re.finditer(rb"^    - fingerprint: ([0-9a-f]{64})\n", raw, re.M))
    need(markers, "Baseline has no complete fingerprint blocks")
    blocks = [(match[1].decode(), raw[match.start():markers[i + 1].start() if i + 1 < len(markers) else len(raw)])
              for i, match in enumerate(markers)]
    need(len(blocks) == len({key for key, _ in blocks}), "Duplicate baseline fingerprint")
    header = raw[:markers[0].start()]
    need(header == b"schema_version: 1\ngenerated_from: 1b875e3ad9f0ca7d4bc306edb6c22431b66da6c7\nentries:\n", "Baseline header changed")
    return header, blocks


def debt_counters(report, baseline, workers):
    """Replay Verify's six counts for this pinned flat baseline/empty registry.

    This deliberately rejects other YAML representations; it is not a general
    YAML parser or substitute architecture scanner. The actual scans are inputs.
    """
    need(workers == b"schema_version: 1\nentries: []\n", "Worker registry is outside the reviewed empty-registry case")
    _, blocks = baseline_blocks(baseline)
    known = {}
    for fingerprint, raw in blocks:
        entry = {}
        for key in ("kind", "owner", "work_package", "remove_by", "reason", "max_metric"):
            values = re.findall(rb"^      " + key.encode() + rb": (.*)$", raw, re.M)
            need(len(values) == (0 if key == "max_metric" and not values else 1), "Missing/duplicate baseline field")
            if not values:
                continue
            value = values[0].decode().strip()
            need(value not in ("|", ">", "|-", ">-"), "Unreviewed multiline baseline field")
            if value.startswith('"'):
                value = json.loads(value)
            elif value.startswith("'"):
                need(value.endswith("'"), "Malformed baseline scalar")
                value = value[1:-1].replace("''", "'")
            entry[key] = int(value) if key == "max_metric" else value
        known[fingerprint] = entry
    current = {entry["fingerprint"]: entry for entry in report["violations"]}
    need(len(current) == len(report["violations"]), "Duplicate report fingerprint")
    unowned, expired, stale, grown = set(), [], [], []
    day = datetime.date(2026, 11, 22)
    for fingerprint, entry in known.items():
        if any(not str(entry.get(key, "")).strip() for key in ("owner", "work_package", "remove_by", "reason")):
            unowned.add(fingerprint)
        try:
            deadline = datetime.datetime.strptime(entry["remove_by"], "%Y-%m-%d").date()
            need(deadline.isoformat() == entry["remove_by"], "Uncanonical baseline date")
            if deadline < day:
                expired.append(fingerprint)
        except (KeyError, ValueError):
            unowned.add(fingerprint)
        if fingerprint not in current:
            stale.append(fingerprint)
            continue
        metric = current[fingerprint].get("metric", 0)
        if metric > 0:
            maximum = entry.get("max_metric", 0)
            if maximum <= 0:
                unowned.add(fingerprint)
            elif metric > maximum:
                grown.append(fingerprint)
    new = sorted(set(current) - set(known))
    worker_problems = sorted(fp for fp, value in current.items() if value["kind"] == "worker" and
                             (fp not in known or known[fp]["kind"] != "worker"))
    counts = dict(expired=len(expired), grown=len(grown), new=len(new), stale=len(stale), unowned=len(unowned), workers=len(worker_problems))
    return counts, {"expired": sorted(expired), "grown": sorted(grown), "new": new,
                    "stale": sorted(stale), "unowned": sorted(unowned), "workers": worker_problems}


def expected_plans(base, candidate, final, inputs):
    common = {"schema": 1, "runner_sha256": RUNNER_SHA, "source_head": BASE_HEAD, "base_tree": BASE_TREE,
              "native_scheduler_operations": False, "inputs_directory": "inputs", "input_files": simplify(inputs)}

    def plan(name, source, directory, commands):
        values = rows(source)
        return {**common, "phase": name, "source_tree": tree(values),
                "source_note": "source_head identifies the immutable base; source_tree and the complete source_files inventory identify these physical bytes. No candidate commit is claimed.",
                "source_directory": directory, "source_files": simplify(values), "commands": commands}

    def go(label, arguments, expected=0, required=(), timeout=360):
        return {"label": label, "tool": "go", "args": arguments, "timeout_seconds": timeout,
                "expected_exit_code": expected, "required_output": list(required)}

    def tests(control):
        arguments = ["test", "-tags=test_state_path_env", "-race", "-count=1", "-parallel=1", "-timeout=180s", "-json"]
        if control:
            arguments += ["-overlay", str(REMOTE / "inputs/a3-control/overlay.json")]
        arguments += ["-run", "^(" + "|".join([ROOTS[1]] if control else ROOTS) + ")$", "./internal/gui"]
        command = go("a3-shared-owner-control" if control else "a3-client-prefs", arguments, int(control),
                     CONTROL_FAILURES.values() if control else (), 240)
        command["required_tests"] = {key: values for key, values in test_expectation(control).items() if values}
        return command

    def architecture(counters):
        flags = ["--root", ".", "--policy", "architecture/policy.yaml"]
        return [go("arch-scan", ["run", "./tools/archcheck", "scan", *flags],
                   required=['"schema_version": 1', '"module": "mcp-local-hub"']),
                go("arch-verify", ["run", "./tools/archcheck", "verify", *flags,
                   "--owners", "architecture/owners.yaml", "--baseline", "architecture/baseline.yaml",
                   "--workers", "architecture/workers.yaml"], 1, [verification_line(counters)])]

    formatting = {"label": "a3-gofmt-check", "tool": "gofmt",
                  "args": ["-l", *GUI_PATHS, *[str(REMOTE / "inputs/a3-control" / path) for path in (GUI_PATHS[0], GUI_PATHS[2])]],
                  "timeout_seconds": 60, "expected_exit_code": 0}
    result = {
        "a3-gui-independence": plan("a3-gui-independence", candidate, "architecture-source", [formatting, tests(True), tests(False)]),
        "a3-base-architecture": plan("a3-base-architecture", base, "architecture-base-source", architecture(BASE_COUNTERS)),
        "a3-candidate-architecture": plan("a3-candidate-architecture", candidate, "architecture-source", architecture({**BASE_COUNTERS, "stale": 132})),
        "a3-build-vet-optional": plan("a3-build-vet-optional", candidate, "architecture-source",
                                      [go(verb, [verb, "./..."], timeout=900) for verb in ("build", "vet")]),
    }
    commands = [architecture(BASE_COUNTERS)[1]]
    commands += [{"label": verb, "tool": "go", "args": [verb, "./..."], "timeout_seconds": 900, "expected_exit_code": 0}
                 for verb in ("build", "vet")]
    result["a3-pruned-architecture"] = plan("a3-pruned-architecture", final, "architecture-final-source", commands)
    return result


def phase_evidence(files, plan, plan_path):
    name = plan["phase"]
    prefix = "evidence/" + name + "/"
    receipt = json.loads(files[prefix + "receipt.json"])
    initial = json.loads(files[prefix + "receipt-initial.json"])
    need(initial["completed"] is False and initial["commands"] == [] and initial["started_utc"] == receipt["started_utc"],
         "Initial phase receipt differs")
    for key in ("phase", "manifest_sha256", "runner_sha256", "source_head", "source_tree", "native_scheduler_operations"):
        need(initial[key] == receipt[key], "Phase receipt identity changed")
    need(receipt["completed"] is True and receipt["native_scheduler_operations"] is False and
         receipt["manifest_sha256"] == sha(files[plan_path]) and receipt["runner_sha256"] == RUNNER_SHA and
         receipt["source_head"] == BASE_HEAD and receipt["source_tree"] == plan["source_tree"] and
         receipt["tools_sha256"] == TOOLS and not receipt.get("error"), "Phase identity, completion or tool pin differs")
    for kind in ("source", "inputs"):
        values = plan["source_files"] if kind == "source" else plan["input_files"]
        need(receipt[kind + "_before"] == receipt[kind + "_after"] == inventory(values), "Full before/after invariant failed")
    environment = receipt["environment"]
    for key, value in {"GOOS": "windows", "GOARCH": "amd64", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
                       "GOVCS": "*:off", "GOFLAGS": "-mod=readonly", "GOENV": "off", "GOWORK": "off", "GOAUTH": "off",
                       "GOTELEMETRY": "off", "MCPHUB_E2E_SCHEDULER": "none", "MCPHUB_NO_AUTO_GUI": "1"}.items():
        need(environment[key] == value, "Unexpected admission environment: " + key)
    for key in ("APPDATA", "LOCALAPPDATA", "GOPATH", "GOTMPDIR", "TEMP", "TMP", "USERPROFILE", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"):
        path = PureWindowsPath(environment[key])
        need(path.is_absolute() and path.is_relative_to(REMOTE / "runtime" / name), "Mutable runtime path escapes owned phase")
    for key in ("GOCACHE", "GOMODCACHE"):
        need(PureWindowsPath(environment[key]).is_relative_to(PureWindowsPath(r"D:\dev\ChatGPT")), "Cache escapes admitted D-root")
    need(len(receipt["commands"]) == len(plan["commands"]), "Missing or extra phase command")
    commands, streams = [], {}
    for command, phase_row in zip(plan["commands"], receipt["commands"]):
        label = command["label"]
        row = json.loads(files[prefix + label + ".receipt.json"])
        started = json.loads(files[prefix + label + ".started.json"])
        launched = json.loads(files[prefix + label + ".launched.json"])
        need(row == phase_row, "Command and phase receipts disagree")
        executable = str(PureWindowsPath(r"C:\msys64\ucrt64\bin") / (command["tool"] + ".exe"))
        expected_argv = [executable, *command["args"]]
        need(row["argv"] == started["argv"] == launched["argv"] == expected_argv, "Literal argv differs")
        need(row["cwd"] == started["cwd"] == launched["cwd"] == str(REMOTE / plan["source_directory"]), "Literal cwd differs")
        need(type(row["pid"]) is int and row["pid"] > 0 and row["pid"] == launched["pid"], "Original direct-child PID differs")
        need(row["direct_timeout_seconds"] == started["direct_timeout_seconds"] == launched["direct_timeout_seconds"] == command["timeout_seconds"], "Deadline differs")
        need(row["settled"] is True and row["timed_out"] is False and not row.get("error_type") and
             not row.get("process_tree_unresolved"), "Command did not settle")
        raw = {}
        for stream in ("stdout", "stderr"):
            raw[stream] = files[prefix + label + "." + stream + ".log"]
            need(row[stream] == {"bytes": len(raw[stream]), "sha256": sha(raw[stream])}, "Per-command raw hash differs")
        output = raw["stdout"] + raw["stderr"]
        oracle = row["exit_code"] == command["expected_exit_code"] and all(text.encode() in output for text in command.get("required_output", []))
        record = {"label": label, "argv": row["argv"], "cwd": row["cwd"], "pid": row["pid"],
                  "started_utc": row["started_utc"], "finished_utc": row["finished_utc"], "exit_code": row["exit_code"],
                  "stdout": row["stdout"], "stderr": row["stderr"], "settled": True, "timed_out": False,
                  "whole_process_tree_deadline_enforced": row["whole_process_tree_deadline_enforced"]}
        if command["args"][0] == "test":
            need(not raw["stderr"], "Unexpected test stderr")
            tests = test_events(raw["stdout"], row, label == "a3-shared-owner-control")
            record["tests"] = tests
            for action, required in command.get("required_tests", {}).items():
                observed = Counter(tests["terminals"][action])
                oracle = oracle and all(observed[value] >= count for value, count in Counter(required).items())
            if command["expected_exit_code"] == 0:
                oracle = oracle and not tests["terminals"]["fail"]
        elif label in ("a3-gofmt-check", "build", "vet"):
            need(not output and row["exit_code"] == 0, "Format/build/vet emitted output or failed")
        elif label == "arch-scan":
            need(not raw["stderr"] and row["exit_code"] == 0, "Architecture scan did not succeed cleanly")
        elif label == "arch-verify":
            expected = command["required_output"][0].encode() + b"\nexit status 1\n"
            need(not raw["stdout"] and raw["stderr"] == expected and row["exit_code"] == 1, "Exact architecture failure differs")
        else:
            need(False, "Unreviewed command label")
        need(oracle and row["oracle_pass"] is True, "Declared command oracle does not recompute from raw evidence")
        record["runner_oracle_pass_recomputed"] = True
        commands.append(record)
        streams[label] = raw
    return {"phase": name, "plan_path": plan_path, "plan_sha256": sha(files[plan_path]), "completed": True,
            "started_utc": receipt["started_utc"], "finished_utc": receipt["finished_utc"],
            "source_tree": receipt["source_tree"], "physical_source": receipt["source_before"],
            "inputs": receipt["inputs_before"], "full_before_after_invariants": True, "commands": commands}, streams


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--git-repo", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--transfer-dir", type=Path, help="Optionally also rehash the six original transport ZIPs")
    args = parser.parse_args()
    need(not args.output.exists(), "Output already exists; preserve it and choose a fresh filename")
    archive_raw = args.archive.read_bytes()
    need(len(archive_raw) == ARCHIVE_BYTES and sha(archive_raw) == ARCHIVE_SHA, "Publication evidence archive identity differs")
    files = unpack(archive_raw)
    package = json.loads(files["PACKAGE_FILES.json"])
    need(len(files) == 112 and package["file_count"] == 111 and
         set(files) == {value["path"] for value in package["files"]} | {"PACKAGE_FILES.json"}, "Incomplete archive manifest")
    check_rows(files, package["files"])
    need(package["base_commit"] == BASE_HEAD and package["qualified_source_tree"] == FINAL_TREE, "Archive scope differs")
    need(not any(name.startswith(("architecture-source/", "architecture-base-source/", "architecture-final-source/")) or
                 name.endswith("/source.zip") for name in files), "Full source copy unexpectedly included")
    transfers = json.loads(files["TRANSFER_ARCHIVES.json"])
    need({value["path"] for value in transfers["archives"]} == set(TRANSFER_PINS), "Missing original transport ZIP provenance")
    transferred = set()
    for value in transfers["archives"]:
        need((value["bytes"], value["sha256"]) == TRANSFER_PINS[value["path"]], "Original transport identity differs")
        check_rows(files, value["members"])
        transferred.update(item["path"] for item in value["members"])
        if args.transfer_dir:
            raw = (args.transfer_dir / value["path"]).read_bytes()
            need(len(raw) == value["bytes"] and sha(raw) == value["sha256"], "Original ZIP changed")
            members = unpack(raw)
            need(set(members) == {item["path"] for item in value["members"]}, "Original ZIP member set differs")
            need(all(data == files[name] for name, data in members.items()), "Transport/extracted artifact differs")
    need(len(transferred) == 72, "Unexpected count of original transported evidence files")
    need(sha(files["qualify_go.py"]) == RUNNER_SHA, "Runner pin differs")

    source1 = json.loads(files["a3-prep/source-plan.json"])
    source2 = json.loads(files["a3-prep/source-plan-v2.json"])
    need(sha(files["a3-prep/source-plan.json"]) == V1_PLAN and sha(files["a3-prep/source-plan-v2.json"]) == V2_PLAN, "Source-plan pin differs")
    for key in source1:
        if key != "preparation_files":
            need(source1[key] == source2[key], "Retry changed the source/test/architecture contract")
    for source in (source1, source2):
        need(source["schema"] == 1 and source["project_execution"] is False and source["native_scheduler_operations"] is False,
             "Unadmitted source preparation scope")
        check_rows(files, source["preparation_files"] + source["input_files"])
    base = git_base(args.git_repo)
    base_rows = rows(base)
    need(base_rows == source2["base_files"], "Remote/prepared base inventory differs from every independently read Git blob")
    candidate = dict(base)
    for path in GUI_PATHS:
        candidate[path] = files["inputs/a3/" + path]
    candidate_rows = rows(candidate)
    need(candidate_rows == source2["candidate_files"] and tree(candidate_rows) == CANDIDATE_TREE, "Candidate is not the exact three-file source")
    need(sorted(path for path in base if base[path] != candidate[path]) == sorted(GUI_PATHS), "Unrelated candidate source change")
    old_baseline = base["architecture/baseline.yaml"]
    new_baseline = files["a3-prune/baseline.yaml"]
    need(sha(old_baseline) == BASELINE_SHA and sha(new_baseline) == FINAL_BASELINE_SHA, "Baseline identities differ")
    header, blocks = baseline_blocks(old_baseline)
    kept = [(fingerprint, raw) for fingerprint, raw in blocks if fingerprint not in REMOVED]
    removed_blocks = [(fingerprint, raw) for fingerprint, raw in blocks if fingerprint in REMOVED]
    need(len(blocks) == 2401 and len(kept) == 2397 and {key for key, _ in removed_blocks} == set(REMOVED), "Wrong baseline prune scope")
    need(header + b"".join(raw for _, raw in kept) == new_baseline and
         b"".join(raw for _, raw in removed_blocks) == files["a3-prune/removed-blocks.txt"], "Unrelated baseline byte changed")
    need(baseline_blocks(new_baseline) == (header, kept), "Baseline retained block order or header differs")
    final = dict(candidate)
    final["architecture/baseline.yaml"] = new_baseline
    final_rows = rows(final)
    need(tree(final_rows) == FINAL_TREE, "Full final source Git tree differs")
    input_rows = [rich(value["path"][len("inputs/"):], files[value["path"]]) for value in source2["input_files"]]
    expected = expected_plans(base, candidate, final, input_rows)
    for name, plan in expected.items():
        path = "a3-prune/qualification-plan.json" if name == "a3-pruned-architecture" else "a3-prep/plans/" + name + ".json"
        need(files[path] == encode(plan), "Entire phase plan differs from independent derivation: " + name)
        if name == "a3-pruned-architecture":
            need(sha(files[path]) == FINAL_PLAN, "Final plan pin differs")
        else:
            need(files[path] == files["a3-prep/remote-plans/" + name + ".json"] and
                 sha(files[path]) == source2["qualification_plan_sha256"][name + ".json"], "Local/remote phase plan mismatch")

    manifest_raw = files["a3-prep/source-results-v2/manifest.json"]
    manifest = json.loads(manifest_raw)
    materialized = json.loads(files["a3-prep/source-results-v2/receipt.json"])
    initial = json.loads(files["a3-prep/source-results-v2/receipt-initial.json"])
    need(initial["completed"] is False and initial["plan_sha256"] == V2_PLAN and
         initial["started_utc"] == materialized["started_utc"], "Initial retry receipt differs")
    need(manifest == {"schema": 1, "base_commit": BASE_HEAD, "base_directory": "architecture-base-source", "base_tree": BASE_TREE,
                     "base_files": base_rows, "candidate_directory": "architecture-source", "candidate_tree": CANDIDATE_TREE,
                     "candidate_files": candidate_rows, "input_files": input_rows,
                     "qualification_plan_sha256": source2["qualification_plan_sha256"]}, "Entire remote source manifest differs")
    need(materialized["completed"] is True and materialized["project_execution"] is False and materialized["native_scheduler_operations"] is False and
         materialized["plan_sha256"] == V2_PLAN and materialized["manifest"] == {"bytes": len(manifest_raw), "sha256": sha(manifest_raw)},
         "Retry source receipt does not bind the exact manifest")
    need(materialized["base_readback"] == materialized["zip_inventory"] == inventory(base_rows) and
         materialized["candidate_readback"] == inventory(candidate_rows) and materialized["zip_reconstructed_git_tree"] == BASE_TREE,
         "Retry full materialized readback differs")
    prior_pin = source2["prior_attempt_receipt"]
    prior_raw = files[prior_pin["path"]]
    check_rows(files, [prior_pin])
    prior = json.loads(prior_raw)
    retained = source2["retained_archive"]
    need(retained == {"path": "a3-prep/source-results/source.zip", "bytes": 22469879,
                      "sha256": "e4c4b4fe80673eb5cf93d0f752159564266514b14d03049bbf823fd935b2c089",
                      "url": "https://codeload.github.com/applicate2628/mcp-local-hub/zip/" + BASE_HEAD}, "Retained archive pin differs")
    need(prior["completed"] is False and prior["project_execution"] is False and prior["native_scheduler_operations"] is False and
         prior["plan_sha256"] == V1_PLAN and prior["archive"] == retained and prior["error_type"] == "RuntimeError" and
         prior["error"] == "Unadmitted archive directory", "Initial preparation failure differs")
    need(materialized["archive"] == retained and materialized["archive_reused"] is True and
         materialized["prior_attempt_receipt"] == prior_pin, "Retry did not preserve its original archive/failure")
    metadata = json.loads(files["a3-prep/source-results/archive-metadata.json"])
    need(metadata["archive_sha256"] == retained["sha256"] and metadata["archive_bytes"] == retained["bytes"] and
         metadata["failed_receipt_sha256"] == sha(prior_raw) and metadata["failed_receipt_bytes"] == len(prior_raw) and metadata["members"] == 3475,
         "Metadata diagnosis is not bound to the preserved failure/archive")
    expected_directories = {""}
    for name in base:
        expected_directories.update(str(parent) for parent in PurePosixPath(name).parents if str(parent) != ".")
    prefix = "mcp-local-hub-" + BASE_HEAD + "/"
    actual_directories = set()
    for entry in metadata["rejected_directories"]:
        need(entry["name"].startswith(prefix) and entry["name"].endswith("/"), "Unexpected archive diagnostic path")
        actual_directories.add(entry["name"][len(prefix):-1])
        need({key: value for key, value in entry.items() if key != "name"} ==
             {"external_attr": "0x10", "mode": "0o0", "create_system": 0, "is_dir": True, "flag_bits": 0,
              "expected_directory": True, "file_size": 0, "compress_size": 0}, "Directory metadata differs from narrow DOS diagnosis")
    need(actual_directories == expected_directories and len(actual_directories) == len(metadata["rejected_directories"]) == 192,
         "Observed directory set differs from independent Git parents")
    need(metadata["directory_modes"] == [{"create_system": 0, "mode": "0o0", "dos_attrs": "0x10", "count": 192}] and
         metadata["file_modes"] == [{"create_system": 0, "mode": "0o0", "dos_attrs": "0x0", "count": 3283}] and
         materialized["archive_entry_encoding"] == {"create_system": 0, "directory_external_attr": 16, "file_external_attr": 0,
                                                    "directories": 192, "files": 3283}, "DOS representation/census differs")

    pruned = json.loads(files["a3-prune/receipt.json"])
    prune_initial = json.loads(files["a3-prune/receipt-initial.json"])
    need(sha(files["a3-prune/materialize.py"]) == "9b6105428583df6c05627542f82595e655151735c0784e26b998de0e2fcdc0d9" and
         sha(files["a3-prune/prune.py"]) == "aaee5be8ef622cc387d99aa88b11c4cb1138e409ae7b6ba4299959c1d06ee567", "Final materializer/pruner changed")
    need(pruned["completed"] is True and pruned["project_execution"] is False and pruned["native_scheduler_operations"] is False and
         pruned["script_sha256"] == sha(files["a3-prune/materialize.py"]) and pruned["source_tree"] == FINAL_TREE and
         pruned["plan_sha256"] == FINAL_PLAN, "Final materialization identity differs")
    need(prune_initial["completed"] is False and prune_initial["started_utc"] == pruned["started_utc"] and
         prune_initial["script_sha256"] == pruned["script_sha256"], "Initial final-materialization receipt differs")
    need(pruned["source_before"] == pruned["source_after"] == inventory(candidate_rows) and
         pruned["inputs_before"] == pruned["inputs_after"] == inventory(input_rows) and
         pruned["final_source"] == inventory(final_rows), "Final materialization full before/after inventory differs")
    need(pruned["baseline_before_sha256"] == BASELINE_SHA and pruned["baseline_after_sha256"] == FINAL_BASELINE_SHA and
         pruned["entries_before"] == 2401 and pruned["entries_after"] == 2397 and
         pruned["removed_fingerprints"] == sorted(REMOVED) and pruned["unrelated_blocks_preserved"] is True, "Final prune scope differs")
    analysis = json.loads(files["a3-prune/analysis.json"])
    need(analysis == {"baseline_before_sha256": BASELINE_SHA, "baseline_after_sha256": FINAL_BASELINE_SHA,
                      "bytes_before": len(old_baseline), "bytes_after": len(new_baseline), "entries_before": 2401, "entries_after": 2397,
                      "removed_fingerprints": sorted(REMOVED), "unrelated_blocks_preserved_byte_for_byte": True,
                      "header_preserved_byte_for_byte": True, "source_tree": FINAL_TREE, "source_inventory": inventory(final_rows),
                      "qualification_plan_sha256": FINAL_PLAN, "pruner_sha256": sha(files["a3-prune/prune.py"]),
                      "removed_blocks_sha256": sha(files["a3-prune/removed-blocks.txt"])}, "Prune analysis does not recompute exactly")

    # Reconstruct the semantic control, preserving its API and frozen assertions.
    control_prefs = candidate[GUI_PATHS[0]].decode()
    anchor = "\nfunc registerClientInstallPrefsRoutes(s *Server) {\n"
    need(control_prefs.count(anchor) == 1 and control_prefs.count("s.clientInstallPrefs.") == 3, "Control anchors changed")
    control_prefs = control_prefs.replace(anchor, "\n// Negative control only: every request uses the most recently constructed Server.\nvar clientInstallPrefsLastServerForControl *Server\n" + anchor)
    control_prefs = control_prefs.replace("s.clientInstallPrefs.", "clientInstallPrefsLastServerForControl.clientInstallPrefs.")
    control_server = candidate[GUI_PATHS[2]].decode()
    assignment = "\ts.clientInstallPrefs = s.api\n"
    need(control_server.count(assignment) == 1, "Control constructor anchor changed")
    control_server = control_server.replace(assignment, assignment + "\tclientInstallPrefsLastServerForControl = s\n")
    need(files["inputs/a3-control/" + GUI_PATHS[0]] == control_prefs.encode() and
         files["inputs/a3-control/" + GUI_PATHS[2]] == control_server.encode(), "Control contains an unreviewed mutation")
    overlay = json.loads(files["inputs/a3-control/overlay.json"])
    need(overlay == {"Replace": {str(REMOTE / "architecture-source" / path): str(REMOTE / "inputs/a3-control" / path)
                                for path in (GUI_PATHS[0], GUI_PATHS[2])}}, "Control overlay escapes its exact two files")
    test_source = candidate[GUI_PATHS[1]].decode()
    need(set(re.findall(r"^func (Test\w+)\(t \*testing.T\)", test_source, re.M)) == set(ROOTS), "Frozen eight roots changed")
    step_names = re.findall(r'^\s*\{"([^"]+)", (?:first|second), http.Method(?:Get|Post),', test_source, re.M)
    need([name.replace(" ", "_") for name in step_names] == STEPS, "Frozen interleaving changed")

    phases, raw_streams = [], {}
    for name in ("a3-gui-independence", "a3-base-architecture", "a3-candidate-architecture", "a3-pruned-architecture"):
        path = "a3-prune/qualification-plan.json" if name == "a3-pruned-architecture" else "a3-prep/plans/" + name + ".json"
        phase, streams = phase_evidence(files, expected[name], path)
        phases.append(phase)
        raw_streams[name] = streams
    need(not any(name.startswith("evidence/a3-build-vet-optional/") for name in files), "Optional pre-prune phase unexpectedly executed")

    expectations = source2["architecture_expectations"]
    reports, indexes, report_roots = {}, {}, {}
    for kind, directory in (("base", "architecture-base-source"), ("candidate", "architecture-source")):
        raw = raw_streams["a3-" + kind + "-architecture"]["arch-scan"]["stdout"]
        report = json.loads(raw)
        report_roots[kind] = report.pop("root")
        need(report_roots[kind] == (REMOTE / directory).as_posix(), "Scan used a different exact physical root")
        need(sha(encode(report)) == expectations[kind + "_report_without_root_sha256"] and
             len(report["violations"]) == expectations[kind + "_violation_count"], "Complete unfiltered report differs")
        need(dict(Counter(value["kind"] for value in report["violations"])) == report["summary"], "Scan summary does not match its findings")
        need(report["schema_version"] == 1 and report["module"] == "mcp-local-hub", "Unexpected report schema/module")
        reports[kind] = report
        indexes[kind] = {value["fingerprint"]: value for value in report["violations"]}
        need(len(indexes[kind]) == len(report["violations"]), "Duplicate architecture fingerprint")
    before, after = indexes["base"], indexes["candidate"]
    need(set(before) - set(after) == set(REMOVED) and not set(after) - set(before), "Architecture delta is not exactly four removals")
    need({key: before[key] for key in REMOVED} == expectations["removed"], "Removed finding identity differs")
    for fingerprint, (kind, symbol) in REMOVED.items():
        value = before[fingerprint]
        need(value["kind"] == kind and value["location"]["symbol"] == symbol and value["location"]["path"] == GUI_PATHS[0],
             "Removal belongs to a different owner")
    line_changes = {}
    for fingerprint, old in before.items():
        if fingerprint not in after:
            continue
        revised = copy.deepcopy(old)
        new = after[fingerprint]
        old_line = old["location"].get("line", 0)
        new_line = new["location"].get("line", 0)
        if old_line != new_line:
            path = old["location"]["path"]
            need(path in GUI_PATHS and old_line > 0 and new_line > 0 and
                 base[path].splitlines()[old_line - 1] == candidate[path].splitlines()[new_line - 1],
                 "Surviving diagnostic moved to different source text")
            revised["location"]["line"] = new_line
            line_changes[fingerprint] = [old_line, new_line]
        if fingerprint == SERVER_BUDGET:
            need(old["metric"] == base[GUI_PATHS[2]].count(b"\n") == 1265 and
                 new["metric"] == candidate[GUI_PATHS[2]].count(b"\n") == 1267 and old["evidence"] == "production_advisory_lines",
                 "Server size metric differs")
            revised["metric"] = 1267
            revised["message"] = "file has 1267 lines and exceeds production_advisory_lines"
        need(revised == new, "Unreviewed surviving architecture finding delta")
    need(line_changes == expectations["surviving_line_changes"] and len(line_changes) == 9, "Exact surviving line shifts differ")
    budget_block = dict(blocks)[SERVER_BUDGET]
    need(re.findall(rb"^      max_metric: (\d+)$", budget_block, re.M) == [b"1406"], "Server baseline cap changed")
    actual_counters, debt_fingerprints = {}, {}
    for kind, report, baseline, wanted in (
        ("base", reports["base"], old_baseline, BASE_COUNTERS),
        ("candidate_unpruned", reports["candidate"], old_baseline, {**BASE_COUNTERS, "stale": 132}),
        ("candidate_pruned", reports["candidate"], new_baseline, BASE_COUNTERS),
    ):
        counters, identities = debt_counters(report, baseline, base["architecture/workers.yaml"])
        need(counters == wanted, "Independent six-counter replay differs: " + kind)
        need(set(identities["workers"]) <= set(identities["new"]), "Worker debt is not the expected subset of New")
        actual_counters[kind], debt_fingerprints[kind] = counters, identities
    need(set(debt_fingerprints["candidate_unpruned"]["stale"]) - set(debt_fingerprints["candidate_pruned"]["stale"]) == set(REMOVED),
         "Prune removed other stale debt")
    need(SERVER_BUDGET not in debt_fingerprints["candidate_pruned"]["grown"], "Server metric unexpectedly increased grown debt")

    # Bind the earlier report used to predict the exact delta to immutable Git.
    historical_raw = base[expectations["historical_archive"]]
    need(sha(historical_raw) == expectations["historical_archive_sha256"], "Historical raw archive changed")
    with zipfile.ZipFile(io.BytesIO(historical_raw)) as historical:
        scan_raw = historical.read("evidence/architecture-owner-green/arch-scan.stdout.log")
        historical_plan = json.loads(historical.read("plans/architecture-owner-green.json"))
        verify_raw = historical.read("evidence/architecture-owner-green/arch-verify.stderr.log")
    need(sha(scan_raw) == expectations["historical_scan_sha256"] and
         verify_raw == verification_line(BASE_COUNTERS).encode() + b"\nexit status 1\n", "Historical prediction source differs")
    historical_report = json.loads(scan_raw)
    historical_report.pop("root")
    need(historical_report == reports["base"], "Actual exact-base scan differs from historical prediction")
    historical_paths = set()
    for value in historical_plan["source_files"]:
        need(simple(value["path"], base[value["path"]]) == value, "Historical qualified source differs from immutable base")
        historical_paths.add(value["path"])
    need(sorted(set(base) - historical_paths) == expectations["historical_source_exact_match_except_report_files"],
         "Historical comparison has an unrelated source addition")

    # The checker repair changes only the expected slash representation.
    checker2 = files["a3-prep/check_evidence_v2.py"]
    checker3 = files["a3-prep/check_evidence_v3.py"]
    old_line = b'        expected_root = str(c.REMOTE / (c.BASE_DIRECTORY if kind == "base" else c.CANDIDATE_DIRECTORY))\n'
    new_line = b'        expected_root = (c.REMOTE / (c.BASE_DIRECTORY if kind == "base" else c.CANDIDATE_DIRECTORY)).as_posix()\n'
    need(checker2.count(old_line) == 1 and checker2.replace(old_line, new_line) == checker3 and
         sha(checker3) == "9286e0d131979c739da1a20279effe4b4aab8d83e6df0b17509a857afdc24b73", "Checker v3 contains a broader change")
    need(b"reportRoot = filepath.ToSlash(filepath.Clean(reportRoot))" in base["internal/archguard/scan.go"], "Production root normalization changed")
    need(report_roots["base"] != str(REMOTE / "architecture-base-source") and
         report_roots["base"] == (REMOTE / "architecture-base-source").as_posix(), "Preserved v2 failure cannot be reproduced by its exact predicate")
    revision = json.loads(files["a3-local-checks/checker-revision.json"])
    need([value["version"] for value in revision["checks"]] == ["v2", "v3"], "Missing checker failure/success replay")
    for check in revision["checks"]:
        version = check["version"]
        stdout = files["a3-local-checks/" + version + "-architecture.stdout.log"]
        stderr = files["a3-local-checks/" + version + "-architecture.stderr.log"]
        need(check["script_sha256"] == sha(files["a3-prep/check_evidence_" + version + ".py"]) and
             check["stdout_sha256"] == sha(stdout) and check["stderr_sha256"] == sha(stderr) and
             check["argv"][-3:] == ["architecture", "residual-next-20261122-dcb40fba/a3-evidence", V2_PLAN], "Checker replay hashes/arguments differ")
        if version == "v2":
            need(check["exit_code"] == 1 and not stdout and b"RuntimeError: Scan used a different physical root" in stderr,
                 "Preserved checker failure changed")
        else:
            need(check["exit_code"] == 0 and not stderr, "Corrected checker replay failed")
            replay = json.loads(stdout)
            need(replay["source"]["base"] == inventory(base_rows) and replay["source"]["candidate"] == inventory(candidate_rows) and
                 replay["architecture"]["base_findings"] == 2660 and replay["architecture"]["candidate_findings"] == 2656 and
                 replay["architecture"]["removed_fingerprints"] == sorted(REMOVED) and replay["architecture"]["architecture_gate"] == "FAILED" and
                 replay["architecture"]["baseline_edit_performed"] is False and replay["build_vet"] == "NOT_CHECKED", "v3 replay overstates its scope")

    control_source = dict(candidate)
    control_source[GUI_PATHS[0]] = control_prefs.encode()
    control_source[GUI_PATHS[2]] = control_server.encode()
    result = {
        "schema": 1, "audit_kind": "independent_local_full_Git_blob_replay_raw_events_and_six_debt_counters",
        "audit_script_sha256": sha(Path(__file__).read_bytes()), "project_execution": False, "remote_execution": False,
        "archive": {"filename": "raw-evidence.zip", "bytes": len(archive_raw), "sha256": sha(archive_raw), "members": len(files),
                    "payload_files": len(package["files"]), "all_payload_hashes_verified": True,
                    "package_manifest_sha256": sha(files["PACKAGE_FILES.json"]), "full_base_and_retained_download_included": False},
        "transfers": {"archive_pins": [{"path": name, "bytes": value[0], "sha256": value[1]} for name, value in TRANSFER_PINS.items()],
                      "all_72_payload_contents_verified": True, "original_zip_bytes_rehashed_in_this_replay": bool(args.transfer_dir)},
        "source": {"base_commit": BASE_HEAD, "base_tree": BASE_TREE, "base": inventory(base_rows),
                   "all_3283_immutable_blobs_rehashed_locally": True, "declared_git_mode": "100644",
                   "candidate_tree": CANDIDATE_TREE, "candidate": inventory(candidate_rows), "candidate_replacements": GUI_PATHS,
                   "qualified_tree": FINAL_TREE, "final": inventory(final_rows), "final_extra_change": "architecture/baseline.yaml",
                   "all_remote_manifest_rows_and_local_derived_plans_match": True,
                   "base_commit_is_not_a_candidate_publication_commit": True},
        "initial_preparation": {"completed": False, "plan_sha256": V1_PLAN, "receipt": prior_pin,
                                "error": prior["error"], "project_execution": False, "source_directories_created_before_rejection": False,
                                "retained_archive": retained, "retained_download_rehashed_locally": False,
                                "metadata": {"directories": 192, "files": 3283, "create_system": 0,
                                             "directory_external_attr": "0x10", "file_external_attr": "0x0", "unix_mode": 0}},
        "source_retry": {"completed": True, "plan_sha256": V2_PLAN, "project_execution": False, "archive_reused": True,
                         "original_receipt_preserved": True, "git_mode_authority": "independently read immutable Git tree, not missing ZIP Unix bits",
                         "complete_manifest_sha256": sha(manifest_raw), "receipt": materialized},
        "final_materialization": {"completed": True, "project_execution": False, "receipt": pruned,
                                  "independently_recomputed_final_tree": FINAL_TREE, "final_plan_sha256": FINAL_PLAN},
        "baseline": {"before": simple("architecture/baseline.yaml", old_baseline), "after": simple("architecture/baseline.yaml", new_baseline),
                     "entries_before": len(blocks), "entries_after": len(kept), "exact_removed_fingerprints": sorted(REMOVED),
                     "all_2397_remaining_blocks_and_header_byte_identical": True, "kept_blocks_sha256": sha(b"".join(raw for _, raw in kept)),
                     "removed_blocks_sha256": sha(files["a3-prune/removed-blocks.txt"])},
        "control": {"semantics": "all three preference calls dispatch to the most recently constructed Server through one package-global pointer",
                    "historical_whole_head_replay": False, "frozen_test_assertions_unchanged": True, "fake_stores_retained": True,
                    "only_two_overlay_sources": [GUI_PATHS[0], GUI_PATHS[2]], "effective_tree": tree(rows(control_source)),
                    "effective_inventory": inventory(rows(control_source))},
        "phases": phases, "command_count": sum(len(phase["commands"]) for phase in phases),
        "raw_command_stream_pairs_verified": sum(len(phase["commands"]) for phase in phases), "all_test_skips": 0,
        "architecture": {"base_findings": 2660, "candidate_findings": 2656,
                         "base_summary": reports["base"]["summary"], "candidate_summary": reports["candidate"]["summary"],
                         "normalized_complete_report_sha256": {kind: sha(encode(report)) for kind, report in reports.items()},
                         "removed": {key: before[key] for key in sorted(REMOVED)}, "surviving_line_changes": line_changes,
                         "server_file_budget": {"fingerprint": SERVER_BUDGET, "before": 1265, "after": 1267, "baseline_max": 1406},
                         "counters_recomputed_from_actual_scans_and_exact_baselines": actual_counters,
                         "debt_fingerprints": debt_fingerprints, "workers_are_a_subset_of_new": True,
                         "global_gate": "FAILED", "historical_scan_prediction_checked_against_actual_exact_base_scan": True,
                         "final_verify_runs_its_own_scan_but_no_final_full_JSON_scan_was_exported": True},
        "checker_repair": {"original_v2_failed": True, "only_expected_root_slash_representation_changed": True,
                           "exact_v2_false_and_v3_true_predicates_replayed": True, "v3_sha256": sha(checker3),
                           "preserved_local_replay_receipt": revision, "new_Go_execution_for_checker_repair": False},
        "publication_candidate_files": [simple(path, final[path]) for path in [*GUI_PATHS, "architecture/baseline.yaml"]],
        "limits": {"full_Go_test_suite": "NOT_RUN", "native_scheduler_or_installed_Product_qualification": "NOT_RUN",
                   "real_preference_storage_independence": "NOT_ESTABLISHED_BY_FAKE_DEPENDENCY_TESTS",
                   "complete_process_tree_settlement_independently_proved": False,
                   "pre_prune_optional_build_vet": "NOT_RUN; final pruned build/vet executed instead",
                   "publication_commit_or_external_review": "NOT_ESTABLISHED_BY_THIS_ARCHIVE"},
    }
    need(result["command_count"] == 10, "Unexpected command total")
    with args.output.open("xb") as stream:
        stream.write(encode(result))
    print(json.dumps({"verified": True, "archive_members": len(files), "all_Git_base_blobs": len(base),
                      "qualified_tree": FINAL_TREE, "final_source": inventory(final_rows), "commands": result["command_count"],
                      "test_skips": 0, "architecture_gate": "FAILED", "counters": actual_counters,
                      "output_bytes": args.output.stat().st_size, "output_sha256": sha(args.output.read_bytes())}, indent=2))


if __name__ == "__main__":
    main()
