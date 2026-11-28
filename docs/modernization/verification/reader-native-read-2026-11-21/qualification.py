"""Isolated PR627 preparation/qualification. Root reviews before any invocation.

Preparation reads only the 13 named existing archive inputs, and writes this new
D root. Test execution is a separate explicit phase. No downloads or installation.
The process lifecycle matches the earlier direct-child capture contract: a
timeout terminates only the retained Popen child and does not prove tree settlement.
"""
import sys
sys.dont_write_bytecode = True

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import tarfile
import tomllib

ROOT = Path(r"D:\dev\ChatGPT\mcp-reader-native-read-20261121-ef942c6e-v2")
REGISTRY = "registry+https://github.com/rust-lang/crates.io-index"
TARGET = "x86_64-pc-windows-msvc"


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def file_hash(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def utc():
    return datetime.now(timezone.utc).isoformat()


def regular(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or getattr(info, "st_file_attributes", 0) & 0x400:
        raise RuntimeError("Expected regular non-reparse file: " + str(path))


def no_reparse_chain(path):
    for item in (path, *path.parents):
        if getattr(item.lstat(), "st_file_attributes", 0) & 0x400:
            raise RuntimeError("Reparse point in admitted path: " + str(item))


def write_new(path, raw):
    path.parent.mkdir(parents=True, exist_ok=True)
    no_reparse_chain(path.parent)
    with path.open("xb") as stream:
        stream.write(raw)


def save_receipt(path, record):
    no_reparse_chain(path.parent)
    if path.exists():
        regular(path)
    path.write_text(json.dumps(record, indent=2) + "\n", encoding="utf-8")


def source_snapshot(plan):
    source = ROOT / "source-pr627"
    no_reparse_chain(source)
    seen = {}
    for row in plan["source_files"]:
        path = source / row["path"]
        no_reparse_chain(path.parent)
        regular(path)
        raw = path.read_bytes()
        blob = hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()
        if len(raw) != row["bytes"] or blob != row["git_blob"] or digest(raw) != row["sha256"]:
            raise RuntimeError("Source pin mismatch: " + row["path"])
        seen[row["path"]] = digest(raw)
    actual = set()
    for path in source.rglob("*"):
        if getattr(path.lstat(), "st_file_attributes", 0) & 0x400:
            raise RuntimeError("Reparse point in source")
        if path.is_file():
            actual.add(path.relative_to(source).as_posix())
    if actual != set(seen):
        raise RuntimeError("Unexpected or missing source files")
    crate = source / plan["subtree"]
    lock = tomllib.loads((crate / "Cargo.lock").read_text(encoding="utf-8"))
    locked = {(p["name"], p["version"], p["source"], p["checksum"])
              for p in lock["package"] if "source" in p}
    expected = {(p["name"], p["version"], REGISTRY, p["sha256"]) for p in plan["archives"]}
    if locked != expected or len(locked) != 13:
        raise RuntimeError("The complete registry lock differs from admitted archives")
    for parent in (crate, *crate.parents):
        if any((parent / ".cargo" / name).exists() for name in ("config", "config.toml")):
            raise RuntimeError("Unexpected ancestor Cargo configuration: " + str(parent))
    return seen


def tools_snapshot(plan):
    seen = {}
    for name, row in plan["tools"].items():
        path = Path(row["path"])
        regular(path)
        value = file_hash(path)
        if path.stat().st_size != row["bytes"] or value != row["sha256"]:
            raise RuntimeError("Installed tool/input changed: " + name)
        seen[name] = value
    if Path(sys.executable).resolve() != Path(plan["tools"]["python"]["path"]).resolve():
        raise RuntimeError("Use the exact existing Python interpreter")
    return seen


def safe_member(member, prefix):
    raw = member.name.rstrip("/")
    parts = raw.split("/")
    if (not raw or "\\" in raw or ":" in raw or parts[0] != prefix
            or any(p in ("", ".", "..") or p.endswith((".", " ")) for p in parts)):
        raise RuntimeError("Unsafe archive path: " + member.name)
    reserved = {"CON", "PRN", "AUX", "NUL", *[f"COM{i}" for i in range(1, 10)],
                *[f"LPT{i}" for i in range(1, 10)]}
    if any(p.split(".")[0].upper() in reserved for p in parts):
        raise RuntimeError("Windows reserved archive name")
    if not (member.isfile() or member.isdir()) or member.issparse():
        raise RuntimeError("Non-regular archive entry refused")
    return PurePosixPath(*parts)


def prepare(plan, record):
    archives, vendor = ROOT / "archive-inputs", ROOT / "vendor"
    archives.mkdir(exist_ok=False)
    vendor.mkdir(exist_ok=False)
    no_reparse_chain(archives)
    no_reparse_chain(vendor)
    receipt = ROOT / "preparation/receipt.json"
    record["archives"] = {}
    record["vendor"] = {}
    for row in plan["archives"]:
        src = Path(plan["archive_source"]) / row["filename"]
        regular(src)
        raw = src.read_bytes()
        if len(raw) != row["bytes"] or digest(raw) != row["sha256"]:
            raise RuntimeError("Existing archive differs: " + row["filename"])
        dest = archives / row["filename"]
        write_new(dest, raw)
        if file_hash(dest) != row["sha256"]:
            raise RuntimeError("Private archive copy differs")
        package = row["name"] + "-" + row["version"]
        files, casefolded = {}, set()
        with tarfile.open(dest, "r:gz") as tf:
            members = tf.getmembers()
            regulars = [m for m in members if m.isfile()]
            if len(regulars) != row["file_count"] or sum(m.size for m in regulars) != row["expanded_bytes"]:
                raise RuntimeError("Archive layout differs from reviewed input")
            for member in members:
                relative = safe_member(member, package)
                key = relative.as_posix().casefold()
                if key in casefolded:
                    raise RuntimeError("Duplicate/case-colliding archive entry")
                casefolded.add(key)
                target = vendor / Path(*relative.parts)
                if member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                    continue
                member_path = PurePosixPath(*relative.parts[1:]).as_posix()
                if member_path == ".cargo-checksum.json" or len(relative.parts) < 2:
                    raise RuntimeError("Unexpected checksum/root file in archive")
                with tf.extractfile(member) as stream:
                    data = stream.read()
                if len(data) != member.size:
                    raise RuntimeError("Short archive member")
                write_new(target, data)
                files[member_path] = digest(data)
        checksum = {"files": files, "package": row["sha256"]}
        checksum_path = vendor / package / ".cargo-checksum.json"
        write_new(checksum_path, (json.dumps(checksum, sort_keys=True, indent=2) + "\n").encode())
        if file_hash(checksum_path) != row["vendor_manifest_sha256"]:
            raise RuntimeError("Generated vendor manifest differs from archive-derived pin")
        record["archives"][row["filename"]] = row["sha256"]
        record["vendor"][package] = {"manifest_sha256": file_hash(checksum_path), "files": len(files)}
        save_receipt(receipt, record)


def verify_vendor(plan, prepared):
    vendor = ROOT / "vendor"
    no_reparse_chain(vendor)
    expected_dirs = {p["name"] + "-" + p["version"] for p in plan["archives"]}
    if {p.name for p in vendor.iterdir()} != expected_dirs:
        raise RuntimeError("Vendor package set differs")
    for row in plan["archives"]:
        archive = ROOT / "archive-inputs" / row["filename"]
        regular(archive)
        if file_hash(archive) != row["sha256"]:
            raise RuntimeError("Private archive changed")
        name = row["name"] + "-" + row["version"]
        root = vendor / name
        no_reparse_chain(root)
        path = root / ".cargo-checksum.json"
        regular(path)
        if (file_hash(path) != row["vendor_manifest_sha256"]
                or prepared["vendor"][name]["manifest_sha256"] != row["vendor_manifest_sha256"]):
            raise RuntimeError("Vendor checksum manifest changed")
        expected = json.loads(path.read_bytes())
        actual = {}
        for path in root.rglob("*"):
            if getattr(path.lstat(), "st_file_attributes", 0) & 0x400:
                raise RuntimeError("Reparse point in vendor")
            if path.is_file() and path != root / ".cargo-checksum.json":
                regular(path)
                actual[path.relative_to(root).as_posix()] = file_hash(path)
        if expected["package"] != row["sha256"] or actual != expected["files"]:
            raise RuntimeError("Vendor content differs: " + name)


def execution_environment(plan, runtime, target):
    for leaf in ("tmp", "profile", "appdata", "localappdata", "cargo-home", "rustup-home", "fixtures"):
        (runtime / leaf).mkdir(exist_ok=False)
    config = ('[source.crates-io]\nreplace-with = "qualified-vendor"\n'
              '[source.qualified-vendor]\ndirectory = ' + json.dumps((ROOT / "vendor").as_posix()) +
              '\n[net]\noffline = true\n')
    write_new(runtime / "cargo-home/config.toml", config.encode())
    essential = {"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SYSTEMDRIVE", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"}
    env = {k: v for k, v in os.environ.items() if k.upper() in essential}
    rust = Path(plan["tools"]["cargo"]["path"]).parent
    msvc, sdk = Path(plan["msvc_root"]), Path(plan["sdk_root"])
    version = plan["sdk_version"]
    system = Path(next(v for k, v in env.items() if k.upper() == "SYSTEMROOT"))
    env.update({"PATH": os.pathsep.join(map(str, (rust, msvc / "bin/Hostx64/x64", sdk / "bin" / version / "x64", system / "System32"))),
                "CARGO_HOME": str(runtime / "cargo-home"), "CARGO_TARGET_DIR": str(target),
                "CARGO_NET_OFFLINE": "true", "CARGO_INCREMENTAL": "0", "CARGO_BUILD_JOBS": "4", "CARGO_TERM_COLOR": "never",
                "RUSTC": plan["tools"]["rustc"]["path"], "RUSTDOC": plan["tools"]["rustdoc"]["path"], "RUSTFMT": plan["tools"]["rustfmt"]["path"],
                "CARGO_TARGET_X86_64_PC_WINDOWS_MSVC_LINKER": plan["tools"]["link"]["path"],
                "RUSTUP_HOME": str(runtime / "rustup-home"), "RUSTUP_AUTO_INSTALL": "0", "RUSTUP_OFFLINE": "1",
                "TEMP": str(runtime / "tmp"), "TMP": str(runtime / "tmp"), "TMPDIR": str(runtime / "tmp"),
                "USERPROFILE": str(runtime / "profile"), "APPDATA": str(runtime / "appdata"), "LOCALAPPDATA": str(runtime / "localappdata"),
                "PYTHONDONTWRITEBYTECODE": "1", "MCPHUB_RX04_FIXTURES": str(runtime / "fixtures"),
                "LIB": os.pathsep.join(map(str, (msvc / "lib/x64", sdk / "Lib" / version / "ucrt/x64", sdk / "Lib" / version / "um/x64"))),
                "INCLUDE": os.pathsep.join(map(str, (msvc / "include", sdk / "Include" / version / "ucrt", sdk / "Include" / version / "shared", sdk / "Include" / version / "um")))})
    return env


def execute(argv, cwd, env, directory, record):
    out, err = directory / "stdout.log", directory / "stderr.log"
    command = {"argv": argv, "cwd": str(cwd), "started_utc": utc(), "settled": False, "timeout_seconds": 600}
    record["command"] = command
    receipt = directory / "receipt.json"
    save_receipt(receipt, record)
    child = None
    try:
        with out.open("xb") as stdout, err.open("xb") as stderr:
            child = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                                     stdout=stdout, stderr=stderr, creationflags=subprocess.CREATE_NO_WINDOW)
            command["pid"] = child.pid
            save_receipt(receipt, record)
            try:
                command["exit_code"] = child.wait(timeout=600)
                command["settled"] = True
            except BaseException:
                command["interrupted_or_timed_out"] = True
                if child.poll() is None:
                    child.kill()  # Retained direct-child handle only; no recovered PID lookup.
                command["exit_code"] = child.wait(timeout=15)
                command["settled"] = True
                record["descendant_settlement_unknown"] = True
                raise
    finally:
        command["finished_utc"] = utc()
        command["streams"] = {p.name: {"bytes": p.stat().st_size, "sha256": file_hash(p)} for p in (out, err) if p.exists()}
        save_receipt(receipt, record)
    if command["exit_code"] != 0:
        raise RuntimeError("Command failed; preserved stdout/stderr; do not claim test PASS")
    return out.read_text(encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("prepare", "format", "test"))
    parser.add_argument("manifest_sha256")
    parser.add_argument("--run-id")
    args = parser.parse_args()
    if (os.name != "nt" or not sys.flags.isolated or not sys.dont_write_bytecode or sys.flags.optimize
            or Path(__file__).resolve().parent != ROOT or not re.fullmatch(r"[0-9a-f]{64}", args.manifest_sha256)):
        raise RuntimeError("Use pinned Python -I -B, exact D root and reviewed manifest pin")
    no_reparse_chain(ROOT)
    manifest = ROOT / "manifest.json"
    regular(manifest)
    if file_hash(manifest) != args.manifest_sha256:
        raise RuntimeError("Manifest differs from reviewed pin")
    plan = json.loads(manifest.read_bytes())
    if plan["root"] != str(ROOT) or file_hash(Path(__file__)) != plan["driver_sha256"]:
        raise RuntimeError("Driver/root differs from manifest")
    if args.phase == "prepare":
        if args.run_id:
            raise RuntimeError("Prepare has one fresh fixed receipt directory")
        directory = ROOT / "preparation"
    else:
        if not args.run_id or not re.fullmatch(r"[a-z][a-z0-9-]{0,47}", args.run_id):
            raise RuntimeError("A fresh run id is required")
        directory = ROOT / "runs" / args.run_id
    directory.mkdir(parents=True, exist_ok=False)
    no_reparse_chain(directory)
    record = {"schema": 1, "phase": args.phase, "commit": plan["commit"], "manifest_sha256": args.manifest_sha256,
              "source_role": plan["source_role"], "source_overlay": plan["source_overlay"],
              "driver_sha256": plan["driver_sha256"], "started_utc": utc(), "completed": False,
              "downloads": False, "installation": False,
              "whole_process_tree_deadline_enforced": False, "whole_process_tree_settlement_proven": False,
              "scope": "PR627 reader experiment only; no original native-owner/FFI or Product qualification"}
    receipt = directory / "receipt.json"
    save_receipt(receipt, record)
    try:
        before = source_snapshot(plan)
        tool_before = tools_snapshot(plan)
        record["source_before"] = before
        record["tools_before"] = tool_before
        save_receipt(receipt, record)
        if args.phase == "prepare":
            prepare(plan, record)
            verify_vendor(plan, record)
        else:
            prepared_path = ROOT / "preparation/receipt.json"
            regular(prepared_path)
            prepared = json.loads(prepared_path.read_bytes())
            if not prepared["completed"] or prepared["manifest_sha256"] != args.manifest_sha256:
                raise RuntimeError("Matching completed preparation required")
            verify_vendor(plan, prepared)
            runtime = directory / "runtime"
            runtime.mkdir(exist_ok=False)
            env = execution_environment(plan, runtime, directory / "target")
            record["environment"] = env
            crate = ROOT / "source-pr627" / plan["subtree"]
            if args.phase == "format":
                argv = [plan["tools"]["rustfmt"]["path"], "--check", "--edition", "2021", "src/lib.rs", "src/windows.rs", "src/read_tests.rs"]
            else:
                argv = [plan["tools"]["cargo"]["path"], "test", "--offline", "--locked", "--lib", "--target", TARGET, "--", "--test-threads=1"]
            output = execute(argv, crate, env, directory, record)
            if args.phase == "test":
                found = re.findall(r"(?m)^test (\S+) \.\.\. (\S+)\s*$", output)
                summary = re.findall(r"test result: (\w+)\. (\d+) passed; (\d+) failed; (\d+) ignored; (\d+) measured; (\d+) filtered out", output)
                record["test_results"] = found
                record["test_summaries"] = summary
                if (Counter(name for name, status in found if status == "ok") != Counter(plan["expected_test_names"])
                        or any(status != "ok" for _, status in found)
                        or summary != [("ok", "16", "0", "0", "0", "0")]):
                    raise RuntimeError("Exact 16-test oracle failed; no skip/filtered/empty acceptance")
            verify_vendor(plan, prepared)
        record["source_after"] = source_snapshot(plan)
        record["tools_after"] = tools_snapshot(plan)
        if record["source_after"] != before or record["tools_after"] != tool_before:
            raise RuntimeError("Source or selected tools changed during phase")
        record["completed"] = True
    except BaseException as error:
        record.update(error_type=type(error).__name__, error=str(error))
        raise
    finally:
        record["finished_utc"] = utc()
        save_receipt(receipt, record)
        print(json.dumps({"phase": args.phase, "completed": record["completed"], "receipt": str(receipt)}))


if __name__ == "__main__":
    main()
