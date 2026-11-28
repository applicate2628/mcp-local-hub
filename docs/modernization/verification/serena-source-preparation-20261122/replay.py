"""Replay A5 evidence as data only; never import or execute project/runner code.

Workspace: python replay.py --root /path/to/serena-next > audit.json
Bundle: python replay.py --root /unpacked --layout bundle --payload /transfer.b64
        --v2-control /v2-control.b64 [--source-root /source] [--evidence-zip /raw.zip]
Reads frozen controls/payload and evidence. Omitted full source/ZIP checks are
reported NOT_RUN in bundle mode. No output or payload reconstruction is hidden.
The only output is JSON on stdout. Uses Python's standard library only.
"""
import argparse
import base64
import datetime
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import zipfile
import zlib


BASE = "75dbc6406123e22468f168b861d5e0622c9d5769"
BASE_TREE = "cb3eec77c23f9400be962d91c9f23834b84f7643"
TREE = "8928e54342bc700b07f977a1d8f22bbaf986ad04"
COMMIT = "4c3eb47d002ea729fe80370a75506a1d0c9650ac"
IDENTITY = "8fc6780873e8d2580ea889c572535c5fb8a430dd"
QUALIFIER = "a483536d689df8c5713fddbdfd221b85953cf0ce4f9d578a31c4cdbfc6b9a26e"
ARCHIVE = {"path": "a5-prep/master-download/master.zip", "bytes": 17461518,
           "sha256": "2e688e6e1e84cc062b3213a50f59f8cad61fac730a2afe44ecaba7f2937d09ac"}
PHASE_PINS = {
    "": ("d9190906266880964ec171b99c1ba6d25988067bb63006d70c93216fc06b921a",
         "864fcab7c3e4090172dadf827a2bde05b568bcdfd75b0ff975ae8802042e986e"),
    "-v2": ("92a51464012c9d58adb3e6e670a2baf04afa1dbcce6c067c78b5aa9100d2bf39",
            "7fd1ca879c303b747775837f667fafb93d281b3f630363245533c96558f2cda6"),
    "-v3": ("87fbadc69f3d0974f8204b0b693860ddd1ac7834401623fad986b70ba77c211e",
            "66860c29629d649f1c07022f97e1209edf9b01d247c6ded97938c23f36e36cef"),
}
WIN_ROOT = r"D:\dev\ChatGPT\mcp-residual-next-20261122-dcb40fba"
GIT = r"C:\Program Files\Git\cmd\git.exe"
GO = r"C:\msys64\ucrt64\bin\go.exe"
GIT_SHA = "177bc8a9af84fa3a400c694c975fda731d048279c074e1a93947ce8b067ebb48"
HELPER_SHA = "23af5b3688704590970bbb4682424ca36c528b1812b851adda3c6f2c6843800c"
TOOLS = {
    GIT: GIT_SHA,
    GO: "0176dbac02ff27ef54c6bc5b316c7c2a2a89f7c8cf5fd314a0e03d6d2b924113",
    r"C:\msys64\ucrt64\bin\gofmt.exe": "95368e4aafefa20f962a345ab02f53ffc035ed69349612abbf385b52f4fb25c5",
    r"C:\msys64\ucrt64\bin\gcc.exe": "f6ff9378c1edb591131304518eac6456708d28720284105930d0d687136bcdd3",
}
GIT_PREFIX = [GIT, "--no-pager", "--no-optional-locks", "--no-replace-objects",
              "--literal-pathspecs", "-c", "core.fsmonitor=false", "-c",
              "core.untrackedCache=false", "-c", "protocol.allow=never"]
ZIP_PIN = {"bytes": 963852, "sha256": "f4154b8e666f1cab452fc35d1083444c884413dee86dcdcb140b8d73e50a455b"}
# These six root records plus the pinned export receipt's 192 member rows bind
# every extracted byte, including when the original transport ZIP is omitted.
ROOT_RECORD_PINS = {
    "actual-commit.raw": (1082, "c10b920e17c488e3ef581de679a1a514b438f13d1a6c576c62211b0efae4f11b"),
    "git-cat-file.launched.json": (824, "8a3d779d986668121893bbd74f402cb859b01f1e3b6104e5fe28a939462df6fc"),
    "git-cat-file.receipt.json": (1282, "f08b7f72869f4abca3390ca87dc23278394a138b960e0d271765dea3e70818a7"),
    "git-cat-file.stderr.log": (0, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"),
    "receipt-initial.json": (631, "c4a6a021933649d8f26d26142825d238e7a1f8e923a7c2d3246a7b2aa2ab0619"),
    "receipt.json": (75005, "1ece40b870c2e1415e2386c660f952af220e489d5a32849d9bfd20eadfd9f7bf"),
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def digest(raw):
    return {"bytes": len(raw), "sha256": sha(raw)}


def oid(kind, raw):
    return hashlib.sha1(kind.encode() + b" " + str(len(raw)).encode() + b"\0" + raw).hexdigest()


def unique(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key: " + key)
        result[key] = value
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique)


def valid_path(name):
    p = PurePosixPath(name)
    return (bool(name) and name.isascii() and not p.is_absolute() and p.as_posix() == name
            and not any(c in name for c in "\\:\0\r\n")
            and all(c not in (".", "..") and c.lower() != ".git" and not c.endswith((".", " ")) for c in p.parts))


def tree_oid(rows):
    root = {}
    for row in rows:
        parts = row["path"].split("/")
        branch = root
        for part in parts[:-1]:
            branch = branch.setdefault(part, {})
        require(parts[-1] not in branch, "duplicate or colliding tree path")
        branch[parts[-1]] = row["blob"]
    def visit(branch):
        raw = b""
        for name in sorted(branch, key=lambda n: (n + ("/" if isinstance(branch[n], dict) else "")).encode()):
            value = branch[name]
            mode, obj = ("40000", visit(value)) if isinstance(value, dict) else ("100644", value)
            raw += mode.encode() + b" " + name.encode() + b"\0" + bytes.fromhex(obj)
        return oid("tree", raw)
    return visit(root)


def physical_summary(rows):
    table = [[x["path"], x["bytes"], x["sha256"]] for x in rows]
    return {"files": len(rows), "bytes": sum(x["bytes"] for x in rows),
            "inventory_sha256": sha(json.dumps(table, separators=(",", ":")).encode())}


def expected_commands(suffix):
    work = WIN_ROOT + "\\a5-work" + suffix
    source, helper = work + "\\checkout", work + "\\bin\\source-prepare-helper.exe"
    prefix = GIT_PREFIX + ["-C", source]
    specs = []
    def git(label, args, stdout=b"", stdin=None, expected_exit=0, timeout=60):
        specs.append((label, prefix + args, source, timeout, expected_exit, stdin, stdout))
    specs.append(("git-version", [GIT, "--version"], WIN_ROOT, 60, 0, None, b"git version 2.56.1.windows.1\n"))
    specs.append(("git-init", GIT_PREFIX + ["init", "--initial-branch=serena-source-only",
                  r"--template=C:\Program Files\Git\ucrt64\share\git-core\templates", source],
                  WIN_ROOT, 60, 0, None, ("Initialized empty Git repository in " + source.replace("\\", "/") + "/.git/\n").encode()))
    for name in ["core.hooksPath", "commit.gpgSign", "init.templateDir"]:
        git("git-config-" + name.lower().replace(".", "-"), ["config", "--get", name], expected_exit=1)
    git("git-config-crlf", ["config", "--local", "core.autocrlf", "false"])
    git("git-import-base-blobs", ["hash-object", "-w", "--no-filters", "--stdin-paths"], "base-oids", "base-paths.txt", timeout=300 if suffix == "-v3" else 60)
    git("git-index-base", ["update-index", "-z", "--index-info"], stdin="base-index.bin")
    git("git-tree-base", ["write-tree"], (BASE_TREE + "\n").encode())
    git("git-import-base-commit", ["hash-object", "-w", "-t", "commit", "--stdin"], (BASE + "\n").encode(), "base-commit.raw")
    git("git-import-delta-blobs", ["hash-object", "-w", "--no-filters", "--stdin-paths"], "delta-oids", "delta-paths.txt")
    git("git-index-composed", ["update-index", "-z", "--index-info"], stdin="composed-index.bin")
    git("git-tree-composed", ["write-tree"], (TREE + "\n").encode())
    git("git-create-source-commit", ["commit-tree", TREE, "-p", BASE, "-F", WIN_ROOT + "\\a5-prep\\inputs\\COMMIT_MESSAGE.txt"], (COMMIT + "\n").encode())
    git("git-bind-source-ref", ["update-ref", "refs/heads/serena-source-only", COMMIT, "0" * 40])
    git("git-bind-head", ["symbolic-ref", "HEAD", "refs/heads/serena-source-only"])
    git("git-read-head", ["rev-parse", "--verify", "HEAD"], (COMMIT + "\n").encode())
    git("git-read-tree", ["rev-parse", "--verify", COMMIT + "^{tree}"], (TREE + "\n").encode())
    git("git-read-parent", ["rev-list", "--parents", "-n", "1", "HEAD"], (COMMIT + " " + BASE + "\n").encode())
    git("git-read-index", ["ls-files", "--stage", "-z", "--cached"], "index")
    git("git-read-leaves", ["ls-tree", "-r", "-z", "--full-tree", "HEAD"], "leaves")
    git("git-clean-status", ["status", "--porcelain=v1", "--untracked-files=all"])
    git("git-object-closure", ["fsck", "--strict", "--no-reflogs"])
    specs.append(("build-source-helper", [GO, "build", "-mod=readonly", "-buildvcs=false", "-o", helper, "./tools/windows-product-validate"], source, 900, 0, None, b""))
    specs.append(("prepare-source", [helper, "--prepare-source", work + "\\export", "--source-checkout", source,
                  "--source-snapshot-output", work + "\\source-snapshot.json", "--expected-commit", COMMIT], source, 600, 0, None, "prepared"))
    specs.append(("verify-source-readback", [helper, "--source-snapshot", work + "\\source-snapshot.json", "--source-root", work + "\\export"], source, 300, 0, None, "readback"))
    git("git-clean-after-preparation", ["status", "--porcelain=v1", "--untracked-files=all"])
    return specs


def main(root, layout, payload_path, v2_control_path, source_root, zip_path):
    step = root / "next-step"
    evidence = root / ("evidence/native-source" if layout == "bundle" else "execution-evidence")
    names = {p.relative_to(evidence).as_posix() for p in evidence.rglob("*") if p.is_file()}
    require(len(names) == 198 and all(valid_path(n) for n in names), "extracted member set")
    for p in evidence.rglob("*"):
        require(not p.is_symlink(), "evidence symlink")
    if zip_path is not None:
        require(digest(zip_path.read_bytes()) == ZIP_PIN, "evidence ZIP changed")
        with zipfile.ZipFile(zip_path) as z:
            require(len(z.namelist()) == 198 and set(z.namelist()) == names, "ZIP member set")
            require(all(not z.getinfo(n).is_dir() for n in names), "ZIP directories")
            for name in names:
                require((evidence / name).read_bytes() == z.read(name), "extracted bytes: " + name)
    def read(name):
        return (evidence / name).read_bytes()
    require({n for n in names if not n.startswith("records/")} == set(ROOT_RECORD_PINS), "top-level evidence set")
    for name, (size, expected_hash) in ROOT_RECORD_PINS.items():
        require(digest(read(name)) == {"bytes": size, "sha256": expected_hash}, "frozen root record: " + name)
    exported = decode(read("receipt.json"))
    require(exported["completed"] is True and exported["latest_phase"] == "a5-source-preparation-v3", "export not complete")
    require(sha((step / "export_source_v3.py").read_bytes()) == exported["exporter_sha256"] == "44f5d5f105fe05e1eba8c4f23ed71170221e5ef2dc2e9807d665cb96f006cfc4", "exporter pin")
    require(len(exported["files"]) == 192 and len({r["path"] for r in exported["files"]}) == 192, "export manifest")
    require({"records/" + r["path"] for r in exported["files"]} == {n for n in names if n.startswith("records/")}, "export manifest set")
    for row in exported["files"]:
        require(digest(read("records/" + row["path"])) == {k: row[k] for k in ("bytes", "sha256")}, "export record digest")
    for flag in ["go_or_project_executed", "network_started", "source_modified", "prior_receipts_modified", "source_qualification_reissued"]:
        require(exported[flag] is False, "export scope: " + flag)

    payload_raw = payload_path.read_bytes()
    require(sha(payload_raw) == "5f758e275c3e60c67d555450aca9b9a7e29ab7c2988dd2c1c38a9fd171340cf5", "original payload pin")
    payload = decode(zlib.decompress(base64.b64decode(payload_raw, validate=True)))
    require(len(payload) == len({r["path"] for r in payload}) == 123, "payload file set")
    inputs = {}
    for row in payload:
        require(valid_path(row["path"]), "payload path")
        raw = base64.b64decode(row["base64"], validate=True)
        require(digest(raw) == {k: row[k] for k in ("bytes", "sha256")}, "payload member changed")
        inputs[row["path"]] = raw
    def inp(name):
        return inputs["a5-prep/inputs/" + name]
    plans = {}
    for suffix, pins in PHASE_PINS.items():
        runner_name = "run_source" + suffix.replace("-", "_") + ".py"
        plan_name = "prepare-plan" + suffix + ".json"
        runner, plan_raw = (step / runner_name).read_bytes(), (step / plan_name).read_bytes()
        require((sha(runner), sha(plan_raw)) == pins, "frozen control pin: " + suffix)
        plan = decode(plan_raw)
        plans[suffix] = plan
        require(plan["runner_sha256"] == pins[0] and plan["qualification_owner_sha256"] == QUALIFIER, "plan owners")
        require((plan["base_commit"], plan["base_tree"], plan["composed_tree"]) == (BASE, BASE_TREE, TREE), "plan source")
        require(plan["master_archive"] == ARCHIVE, "plan archive")
        require(len(plan["input_files"]) == len({r["path"] for r in plan["input_files"]}) == 121, "input count")
        for row in plan["input_files"]:
            require(digest(inputs[row["path"]]) == {k: row[k] for k in ("bytes", "sha256")}, "plan input digest")
            if layout == "bundle":
                relative = row["path"].removeprefix("a5-prep/inputs/")
                path = root / ("candidate" if relative.startswith("deltas/") or relative in ("manifest.json", "source-files.json") else "next-step") / relative
                require(path.read_bytes() == inputs[row["path"]], "published input differs: " + relative)
        require(plan["input_files"] == plans[""]["input_files"], "cross-attempt input change")
        for key in ["product_or_serena_execution", "native_scheduler_operations", "installs", "tests", "dependency_downloads"]:
            require(plan[key] is False, "plan scope")
    require(inputs["a5-prep/run_source.py"] == (step / "run_source.py").read_bytes(), "payload runner")
    require(inputs["a5-prep/prepare-plan.json"] == (step / "prepare-plan.json").read_bytes(), "payload plan")
    for suffix, control_sha in [("-v2", "a54583d1b5ccd1036dcfe5d938fec316af8e981740b6d4bb3df421782ff711e5"), ("-v3", "8bd47471c1a9d56ebe6f5e5a343e82d74e995905b51ef10b3159229da2760d94")]:
        control_raw = (v2_control_path if suffix == "-v2" else step / "v3-control.b64").read_bytes()
        require(sha(control_raw) == control_sha, "versioned control payload")
        control = decode(zlib.decompress(base64.b64decode(control_raw, validate=True)))
        expected_paths = {"a5-prep/run_source" + suffix.replace("-", "_") + ".py", "a5-prep/prepare-plan" + suffix + ".json"}
        require(len(control) == 2 and {r["path"] for r in control} == expected_paths, "versioned control set")
        for row in control:
            raw = base64.b64decode(row["base64"], validate=True)
            require(raw == (step / PurePosixPath(row["path"]).name).read_bytes() and digest(raw) == {k: row[k] for k in ("bytes", "sha256")}, "versioned control bytes")

    base, full, manifest = decode(inp("base-files.json")), decode(inp("source-files.json")), decode(inp("manifest.json"))
    for rows, count, tree in [(base, 3270, BASE_TREE), (full, 3323, TREE)]:
        require(len(rows) == len({r["path"].lower() for r in rows}) == count, "inventory count")
        require(rows == sorted(rows, key=lambda r: r["path"]), "inventory order")
        require(all(valid_path(r["path"]) and r["mode"] == "100644" for r in rows), "inventory path or mode")
        require(tree_oid(rows) == tree, "independent Git tree")
    by_base, by_full = {r["path"]: r for r in base}, {r["path"]: r for r in full}
    deltas = {r["path"]: r for r in manifest["files"]}
    changed = {n for n in by_full if n not in by_base or by_base[n]["blob"] != by_full[n]["blob"]}
    require(len(deltas) == 109 and set(deltas) == changed and not (set(by_base) - set(by_full)), "delta selection")
    require(len(set(by_full) - set(by_base)) == 53, "addition count")
    for name, row in deltas.items():
        raw = inp("deltas/" + name)
        require(digest(raw) == {k: by_full[name][k] for k in ("bytes", "sha256")} and oid("blob", raw) == by_full[name]["blob"] == row["output"]["blob"], "delta bytes")
    if source_root is not None:
        require(source_root.is_dir() and not source_root.is_symlink(), "full source root")
        for p in source_root.rglob("*"):
            require(not p.is_symlink(), "local source symlink")
        require({p.relative_to(source_root).as_posix() for p in source_root.rglob("*") if p.is_file()} == set(by_full), "candidate local file set")
        for row in full:
            raw = (source_root / row["path"]).read_bytes()
            require(digest(raw) == {k: row[k] for k in ("bytes", "sha256")} and oid("blob", raw) == row["blob"], "local source bytes: " + row["path"])
    for name, rows in [("base-index.bin", base), ("composed-index.bin", full)]:
        require(inp(name) == b"".join((r["mode"] + " " + r["blob"] + "\t" + r["path"]).encode() + b"\0" for r in rows), "index input")
    for name, paths in [("base-paths.txt", [r["path"] for r in base]), ("delta-paths.txt", sorted(deltas))]:
        require(inp(name) == "".join(json.dumps(p) + "\n" for p in paths).encode(), "paths input")
    require(inp("base-shallow.txt") == (BASE + "\n").encode(), "shallow boundary")
    require(oid("commit", inp("base-commit.raw")) == BASE and oid("commit", inp("identity-source-commit.raw")) == IDENTITY, "raw input commit identity")
    require(inp("base-commit.raw").startswith(("tree " + BASE_TREE + "\n").encode()), "base commit tree")

    raw_snapshot = read("records/source/source-snapshot.json")
    snapshot = decode(raw_snapshot)
    require(snapshot == {"schema": "reused-graphify-source-snapshot-v1", "files": {r["path"]: {"sha256": r["sha256"]} for r in full}}, "complete snapshot table")
    # Go json.Marshal struct field order; all path/hash strings here are ASCII and contain no HTML escapes.
    canonical = {"schema": "mcphub-publisher-source-closure-v1", "version": 1,
                 "files": [{"path": r["path"], "sha256": r["sha256"]} for r in full]}
    require(all(not any(c in r["path"] for c in "<>&") for r in full), "canonical escaping assumption")
    epoch = sha(json.dumps(canonical, separators=(",", ":")).encode())
    source_input = {"schema": "windows-publisher-source-input-v1", "source_snapshot_sha256": sha(raw_snapshot), "source_epoch": epoch, "file_count": 3323}
    prepared = {"schema": "windows-publisher-source-preparation-v1", "git_commit": COMMIT,
                "git_tree": TREE, "dirty_selected_source": False, "source_input": source_input}
    raw_commit = read("actual-commit.raw")
    require(raw_commit == read("records/source/actual-commit.raw") and oid("commit", raw_commit) == COMMIT, "actual raw commit identity")
    header, message = raw_commit.split(b"\n\n", 1)
    require(message == inp("COMMIT_MESSAGE.txt"), "commit message")
    require(header.splitlines()[:2] == [("tree " + TREE).encode(), ("parent " + BASE).encode()], "actual tree/parent")
    require(len(header.splitlines()) == 4, "unexpected commit headers or parent")
    actual_date = None
    identity = decode(inp("identity.json"))
    for role in ("author", "committer"):
        match = re.search(rb"(?m)^" + role.encode() + rb" (.+) <([^<>\r\n]+)> (\d+) ([+-]\d{4})$", header)
        require(match is not None, "actual identity syntax")
        require((match[1].decode(), match[2].decode()) == (identity[role]["name"], identity[role]["email"]) and match[4] == b"+0000", "actual author identity")
        date = datetime.datetime.fromtimestamp(int(match[3]), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        require(actual_date in (None, date), "different author/committer dates")
        actual_date = date

    outputs = {
        "base-oids": "".join(r["blob"] + "\n" for r in base).encode(),
        "delta-oids": "".join(deltas[n]["output"]["blob"] + "\n" for n in sorted(deltas)).encode(),
        "index": b"".join((r["mode"] + " " + r["blob"] + " 0\t" + r["path"]).encode() + b"\0" for r in full),
        "leaves": b"".join((r["mode"] + " blob " + r["blob"] + "\t" + r["path"]).encode() + b"\0" for r in full),
        "prepared": json.dumps(prepared, separators=(",", ":")).encode() + b"\n",
        "readback": json.dumps(source_input, separators=(",", ":")).encode() + b"\n",
    }
    attempts, exceptions, final_receipt = {}, [], None
    for suffix, pins in PHASE_PINS.items():
        phase = "a5-source-preparation" + suffix
        rel = "records/evidence/" + phase + "/"
        receipt_raw, initial_raw = read(rel + "receipt.json"), read(rel + "receipt-initial.json")
        receipt, initial = decode(receipt_raw), decode(initial_raw)
        require((receipt["runner_sha256"], receipt["plan_sha256"]) == pins and receipt["phase"] == phase, "attempt pins")
        require((receipt["base_commit"], receipt["base_tree"], receipt["source_tree"]) == (BASE, BASE_TREE, TREE), "attempt source")
        require(initial["commands"] == [] and initial["completed"] is False and initial["source_issuance"] == "NOT_RUN", "initial state")
        for key in initial:
            if key not in ("commands", "completed", "source_commit_created", "source_issuance"):
                require(initial[key] == receipt[key], "initial receipt identity: " + key)
        for key in ("product_or_serena_execution", "installs", "native_scheduler_operations"):
            require(receipt[key] is False, "attempt scope")
        require(receipt["tests"] == "NOT_RUN" and receipt["tools_sha256"] == TOOLS, "attempt tools/tests")
        env = receipt["environment"]
        for key, value in {"GOPROXY": "off", "GOSUMDB": "off", "GOAUTH": "off", "GOVCS": "*:off", "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly", "GOENV": "off", "GOWORK": "off", "GIT_NO_LAZY_FETCH": "1", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "MCPHUB_E2E_SCHEDULER": "none"}.items():
            require(env[key] == value, "private environment: " + key)
        runtime = WIN_ROOT + "\\runtime\\" + phase
        for key, tail in {"USERPROFILE": "profile", "APPDATA": "appdata", "LOCALAPPDATA": "localappdata", "TEMP": "tmp", "TMP": "tmp", "GOTMPDIR": "tmp", "GOPATH": "gopath", "XDG_CACHE_HOME": "xdg-cache", "XDG_CONFIG_HOME": "xdg-config", "XDG_DATA_HOME": "xdg-data", "GIT_CONFIG_GLOBAL": "absent-global-config"}.items():
            require(env[key] == runtime + "\\" + tail, "D-only private path: " + key)
        specs = expected_commands(suffix)[:1 if suffix == "" else 7 if suffix == "-v2" else 27]
        require(len(receipt["commands"]) == len(specs), "attempt command count")
        stream_rows = []
        for command, spec in zip(receipt["commands"], specs):
            label, argv, cwd, timeout, expected_exit, stdin, stdout = spec
            is_timeout = suffix == "-v2" and label == "git-import-base-blobs"
            require((command["label"], command["argv"], command["cwd"], command["direct_timeout_seconds"]) == (label, argv, cwd, timeout), "exact command contract: " + phase + "/" + label)
            require(command["settled"] is True and command["whole_process_tree_deadline_enforced"] is False, "direct child settlement scope")
            require(command["exit_code"] == (1 if is_timeout else expected_exit) and command["oracle_pass"] is (not is_timeout) and command["timed_out"] is is_timeout, "command outcome")
            require(bool(command.get("process_tree_unresolved")) is is_timeout, "process tree unresolved state")
            if label != "build-source-helper":
                executable_sha = GIT_SHA if argv[0] == GIT else HELPER_SHA
                require(command["executable_sha256"] == command["executable_hash_after"] == executable_sha, "per-command executable pin")
            require(decode(read(rel + label + ".receipt.json")) == command, "individual command receipt")
            started, launched = decode(read(rel + label + ".started.json")), decode(read(rel + label + ".launched.json"))
            require(started["settled"] is False and launched["settled"] is False and "pid" not in started and launched["pid"] == command["pid"], "launch ownership record")
            for key in started:
                if key not in ("settled", "timed_out"):
                    require(started[key] == command[key] and launched[key] == command[key], "launch identity")
            if stdin:
                require(read(rel + label + ".stdin.bin") == inp(stdin) and command["stdin"] == digest(inp(stdin)), "exact stdin")
            else:
                require("stdin" not in command, "unexpected stdin")
            expected_stdout = outputs[stdout] if isinstance(stdout, str) else stdout
            for stream, expected_bytes in [("stdout", expected_stdout), ("stderr", b"")]:
                path = "evidence/" + phase + "/" + label + "." + stream + ".log"
                raw = read("records/" + path)
                require(raw == expected_bytes, "stream oracle bytes: " + path)
                recorded, captured = command[stream], digest(raw)
                exact = recorded == captured
                require(exact or (is_timeout and stream == "stdout"), "unexpected stream difference")
                row = {"path": path, "recorded": recorded, "captured": captured, "exact_match": exact,
                       "recorded_prefix_matches": len(raw) >= recorded["bytes"] and sha(raw[:recorded["bytes"]]) == recorded["sha256"]}
                require(row["recorded_prefix_matches"], "recorded stream prefix mismatch")
                if not exact:
                    prefix_path = "recorded-stream-prefixes/" + phase + "/" + label + ".stdout.bin"
                    prefix = read("records/" + prefix_path)
                    require(prefix == raw[:recorded["bytes"]] and digest(prefix) == recorded and len(prefix) == 36864 and len(raw) == 134070, "sole recorded-prefix exception")
                    row["recorded_prefix_file"] = prefix_path
                    exceptions.append(row)
                stream_rows.append(row)
        exported_attempt = exported["attempts"][phase]
        require(exported_attempt == {"recorded_completed": receipt["completed"], "recorded_source_issuance": receipt["source_issuance"],
                "recorded_source_commit_created": receipt["source_commit_created"], "recorded_process_tree_unresolved": suffix == "-v2", "streams": stream_rows}, "exported stream summary")
        if suffix == "":
            require(receipt["completed"] is False and receipt["source_commit_created"] is False and receipt["source_issuance"] == "NOT_RUN" and "mingw64" in receipt["error"] and receipt["error_type"] == "FileNotFoundError", "v1 retained failure")
        elif suffix == "-v2":
            require(receipt["completed"] is False and receipt["source_commit_created"] is False and receipt["source_issuance"] == "NOT_RUN" and receipt["error_type"] == "TimeoutExpired", "v2 retained failure")
        else:
            require(receipt["completed"] is True and receipt["source_commit_created"] is True and receipt["source_issuance"] == "PASS" and "error" not in receipt, "v3 final success")
            final_receipt = receipt
        attempts[phase] = {"receipt": digest(receipt_raw), "commands": len(specs), "matching_streams": sum(x["exact_match"] for x in stream_rows), "total_streams": len(stream_rows),
                           "completed": receipt["completed"], "source_issuance": receipt["source_issuance"], "process_tree_unresolved": suffix == "-v2",
                           "commands_checked": [{"label": c["label"], "exit_code": c["exit_code"], "oracle_pass": c["oracle_pass"], "direct_timeout_seconds": c["direct_timeout_seconds"], "seconds": c["seconds"]} for c in receipt["commands"]]}

    require(len(exceptions) == 1, "recorded-prefix exception count")
    require(final_receipt["source_commit"] == COMMIT and final_receipt["source_preparation"] == prepared and final_receipt["source_input_readback"] == source_input and final_receipt["helper_sha256"] == HELPER_SHA, "final source identity")
    for role in ("AUTHOR", "COMMITTER"):
        for key, value in [("NAME", identity[role.lower()]["name"]), ("EMAIL", identity[role.lower()]["email"]), ("DATE", actual_date)]:
            require(final_receipt["commit_metadata"]["GIT_" + role + "_" + key] == value, "commit metadata receipt")
    require(final_receipt["identity_source_commit"] == IDENTITY and final_receipt["history_boundary"] == BASE, "identity source and history boundary")
    require(decode(read("records/evidence/a5-source-preparation-v3/source-commit-created.json")) == {key: final_receipt[key] for key in ("source_commit", "source_tree", "commit_metadata", "identity_source_commit", "history_boundary")}, "early commit-created receipt")
    for key in ["composed_physical_before_commit", "source_before_helper", "source_after_helper_build", "export", "source_after"]:
        require(final_receipt[key] == physical_summary(full), "full physical inventory receipt: " + key)
    require(final_receipt["base_physical"] == physical_summary(base), "base physical inventory")
    require(exported["source_before"] == exported["source_after"] == physical_summary(full), "export physical coverage")
    require(exported["snapshot"] == {**digest(raw_snapshot), "matches_issued_hash": True}, "export snapshot")
    require(exported["actual_commit"] == {"oid": COMMIT, **digest(raw_commit), "tree": TREE, "parent": BASE}, "export commit")
    operation = decode(read("git-cat-file.receipt.json"))
    require(exported["native_operations"] == [operation], "single exporter native operation")
    source = WIN_ROOT + "\\a5-work-v3\\checkout"
    require(operation["argv"] == GIT_PREFIX + ["-C", source, "cat-file", "commit", COMMIT] and operation["cwd"] == source, "exporter read-only argv")
    require(operation["settled"] is True and operation["timed_out"] is False and operation["oracle_pass"] is True and operation["exit_code"] == 0 and operation["whole_process_tree_deadline_enforced"] is False, "exporter operation outcome")
    require(operation["stdout"] == digest(raw_commit) and operation["stderr"] == digest(b"") and read("git-cat-file.stderr.log") == b"", "exporter streams")
    require(operation["executable_sha256"] == operation["executable_hash_after"] == GIT_SHA, "exporter Git pin")
    fetch_raw = read("records/a5-prep/master-download/receipt.json")
    fetch = decode(fetch_raw)
    require(fetch["completed"] is True and fetch["source_commit"] == BASE and fetch["archive"] == ARCHIVE and sha(fetch_raw) == final_receipt["master_archive"]["fetch_receipt_sha256"], "retained archive receipt")
    require(fetch["script_sha256"] == sha((step / "fetch_master.py").read_bytes()) and fetch["url"] == plans["-v3"]["source_url"], "archive fetch source")
    require(final_receipt["master_archive"]["zip_metadata"] == fetch["zip_metadata"] == [{"kind": "directory", "create_system": 0, "external_attr": 16, "upper_mode": 0, "count": 187}, {"kind": "file", "create_system": 0, "external_attr": 0, "upper_mode": 0, "count": 3270}], "recorded archive metadata")

    return {"schema": "independent-a5-data-audit-v1", "result": "PASS", "replay_sha256": sha(Path(__file__).read_bytes()), "layout": layout,
            "audit_scope": "Data-only replay; no subprocess, project import, Git, Go, Rust, helper, network, or native execution",
            "evidence_zip": {**ZIP_PIN, "rehashed": "PASS" if zip_path is not None else "NOT_RUN"}, "frozen_extracted_members_checked": 198, "export_manifest_records": 192,
            "payload_files": 123, "unchanged_execution_inputs_per_attempt": 121, "delta_files": 109,
            "additions": 53, "modifications": 56, "deletions": 0,
            "independent_base_tree": BASE_TREE, "independent_source_tree": TREE,
            "full_source_rehash": "PASS" if source_root is not None else "NOT_RUN", "local_candidate_files_rehashed": 3323 if source_root is not None else 0, "physical_inventory": physical_summary(full),
            "actual_commit": {"oid": COMMIT, **digest(raw_commit), "tree": TREE, "parent": BASE, "date": actual_date},
            "source_snapshot": digest(raw_snapshot), "source_input": source_input,
            "attempts": attempts, "recorded_stream_exceptions": exceptions,
            "original_failures_preserved": True, "whole_process_tree_deadline_qualification": False,
            "source_issuance_supported": True, "tests": "NOT_RUN", "product_or_serena_execution": False,
            "limitations": ["This is a consistency and content audit of retained execution evidence, not a new runtime qualification.",
                            "The retained master ZIP itself is not in the evidence ZIP; its acquisition and runner checks are bound by the captured receipt and frozen runner.",
                            "Outer subprocess deadlines cover the retained direct child; the v2 timed-out attempt retains process_tree_unresolved=true.",
                            "Native checkout/export coverage is evidenced by inventoried runner/exporter receipts; this replay always matches the complete emitted snapshot to the frozen inventory and rehashes full local source only when supplied.",
                            "No Rust, Go test, Product, or Serena operational qualification follows from Source preparation."]}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--layout", choices=("workspace", "bundle"), default="workspace")
    parser.add_argument("--payload", type=Path)
    parser.add_argument("--v2-control", type=Path)
    parser.add_argument("--source-root", type=Path)
    parser.add_argument("--evidence-zip", type=Path)
    args = parser.parse_args()
    root = args.root.resolve()
    if args.layout == "bundle":
        require(args.payload is not None and args.v2_control is not None, "Bundle replay requires the exact recreated --payload and --v2-control")
    else:
        args.payload = args.payload or root / "next-step/transfer.b64"
        args.v2_control = args.v2_control or root / "next-step/v2-control.b64"
        args.source_root = args.source_root or root / "candidate/source"
        args.evidence_zip = args.evidence_zip or root / "a5-source-evidence-v3.zip"
    print(json.dumps(main(root, args.layout, args.payload, args.v2_control, args.source_root, args.evidence_zip), indent=2, ensure_ascii=False))
