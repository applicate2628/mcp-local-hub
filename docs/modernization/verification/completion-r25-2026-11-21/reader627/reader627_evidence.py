"""Capture reviewed PR627 evidence, or verify an externally pinned public ZIP.

Verify mode reads bytes only: no extraction, imports from ZIP, or subprocesses.
It checks integrity/internal consistency, not execution authenticity, installed
tools, or the contents of excluded dependency archives/vendor files.
"""
import sys
sys.dont_write_bytecode = True
import argparse
from collections import Counter
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import stat
import tomllib
import zipfile

ROOT = PureWindowsPath(r"D:\dev\ChatGPT\mcp-r25-reader-20261121")
COMMIT = "b827398ef127a1556b7b2bdcdcec7fe03fa6651d"
MANIFEST_SHA = "d6ae499ac20b0b669a77c3f4a4e4a60605c4de546ed8f0f943955c5ca59737e5"
DRIVER_SHA = "f2686283d1b96f44a555c413421aa7f47546333747d49523615302a627a2f766"
SUBTREE = "experiments/rust-anchored-read"
PHASES = {"prepare": "preparation", "format": "runs/format-v1", "test": "runs/native-debug-v1"}
LIMIT = 4 * 1024 * 1024

def require(ok, message):
    if not ok:
        raise ValueError(message)

def sha(raw):
    return hashlib.sha256(raw).hexdigest()

def encoded(value):
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()

def safe(name):
    require(isinstance(name, str) and re.fullmatch(r"[A-Za-z0-9_./-]+", name), "Unsafe name")
    p = PurePosixPath(name)
    reserved = {"CON", "PRN", "AUX", "NUL"} | {s + str(i) for s in ("COM", "LPT") for i in range(1, 10)}
    require(not p.is_absolute() and str(p) == name and all(
        x not in (".", "..") and not x.endswith(".") and x.split(".")[0].upper() not in reserved
        for x in p.parts), "Noncanonical/reserved name")
    return name

def unique(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "Duplicate JSON key")
        result[key] = value
    return result

def decoded(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=unique,
                      parse_constant=lambda value: require(False, "Nonfinite JSON constant"))

def no_links(path):
    for p in (path, *path.parents):
        s = p.lstat()
        require(not stat.S_ISLNK(s.st_mode) and not getattr(s, "st_file_attributes", 0) & 0x400, "Link/reparse path")

def read(path):
    no_links(path)
    before = path.stat()
    require(stat.S_ISREG(before.st_mode) and before.st_size <= LIMIT, "Not a bounded regular file")
    raw = path.read_bytes()
    after = path.stat()
    require(all(getattr(after, k) == getattr(before, k) for k in ("st_dev", "st_ino", "st_size", "st_mtime_ns")) and len(raw) == before.st_size, "File changed while reading")
    return raw

def write(path, raw):
    no_links(path.parent)
    with path.open("xb") as stream:
        stream.write(raw)
    require(read(path) == raw, "Write readback differs")


def members(plan):
    names = {"manifest.json", "reader627.py", "reader627_evidence.py", "RUNTIME.md"}
    names.update("source-pr627/" + safe(x["path"]) for x in plan["source_files"])
    names.update("vendor/" + safe(x["name"] + "-" + x["version"]) + "/.cargo-checksum.json" for x in plan["archives"])
    names.update(p + "/receipt.json" for p in PHASES.values())
    names.update(PHASES[p] + "/" + s + ".log" for p in ("format", "test") for s in ("stdout", "stderr"))
    return names


def evidence(data, mapping):
    require(set(data) == set(mapping) and len(data) == 31, "Evidence membership differs")
    for name, row in mapping.items():
        safe(name)
        require(sha(data[name]) == row["public_sha256"] and len(data[name]) == row["public_bytes"], "Public member pin differs")
        require(re.fullmatch(r"[0-9a-f]{64}", row["raw_sha256"]) and type(row["raw_bytes"]) is int and row["raw_bytes"] >= 0 and type(row["changed"]) is bool, "Invalid original pin")
        counts = row["replacement_counts"]
        require(set(counts) == {"plain", "json_escaped"} and all(type(n) is int and n >= 0 for n in counts.values()), "Redaction counts")
        require(row["changed"] == bool(sum(counts.values())) == (row["raw_sha256"] != row["public_sha256"]), "Redaction identity")
        require(row["changed"] or row["raw_bytes"] == row["public_bytes"], "Unchanged size differs")
    plan = decoded(data["manifest.json"])
    require(mapping["manifest.json"]["raw_sha256"] == MANIFEST_SHA and plan["commit"] == COMMIT and
            plan["root"] == str(ROOT) and plan["subtree"] == SUBTREE, "Frozen manifest identity differs")
    require(set(data) == members(plan) and plan["driver_sha256"] == DRIVER_SHA == sha(data["reader627.py"]), "Source selection/driver differs")
    require(all(not mapping[n]["changed"] for n in data if n not in
                {"manifest.json", PHASES["format"] + "/receipt.json", PHASES["test"] + "/receipt.json"}), "Unexpected redaction")
    source = {x["path"]: x["sha256"] for x in plan["source_files"]}
    require(len(source) == len(plan["source_files"]) == 7 and sum(x["bytes"] for x in plan["source_files"]) == 39826, "Source count/size")
    for row in plan["source_files"]:
        raw = data["source-pr627/" + row["path"]]
        require(sha(raw) == row["sha256"] and len(raw) == row["bytes"] and
                hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest() == row["git_blob"], "Source SHA/blob differs")
    lock_raw = data["source-pr627/" + SUBTREE + "/Cargo.lock"]
    require(sha(lock_raw) == plan["cargo_lock_sha256"], "Cargo lock identity")
    locked = [(p["name"], p["version"], p["source"], p["checksum"]) for p in tomllib.loads(lock_raw.decode())["package"] if "source" in p]
    expected = [(p["name"], p["version"], "registry+https://github.com/rust-lang/crates.io-index", p["sha256"]) for p in plan["archives"]]
    require(len(locked) == len(set(locked)) == len(expected) == 13 and set(locked) == set(expected), "13 locked dependencies differ")
    vendor, archive_pins = {}, {}
    for row in plan["archives"]:
        package = row["name"] + "-" + row["version"]
        raw = data["vendor/" + package + "/.cargo-checksum.json"]
        checksum = decoded(raw)
        require(sha(raw) == row["vendor_manifest_sha256"] and checksum["package"] == row["sha256"] and
                set(checksum) == {"files", "package"} and len(checksum["files"]) == row["file_count"], "Vendor manifest/count differs")
        require(len(checksum["files"]) == len({n.casefold() for n in checksum["files"]}), "Vendor file collision")
        for name, digest in checksum["files"].items():
            require(safe(name) != ".cargo-checksum.json" and re.fullmatch(r"[0-9a-f]{64}", digest), "Vendor file entry differs")
        vendor[package] = {"manifest_sha256": sha(raw), "files": len(checksum["files"])}
        archive_pins[row["filename"]] = row["sha256"]
    require(sum(v["files"] for v in vendor.values()) == 797 and sum(r["bytes"] for r in plan["archives"]) == 3598476 and
            sum(r["expanded_bytes"] for r in plan["archives"]) == 24348685, "Dependency count/byte claims differ")
    tools = {k: v["sha256"] for k, v in plan["tools"].items()}
    require(len(tools) == 11, "Selected tool/input count differs")
    for phase, folder in PHASES.items():
        receipt = decoded(data[folder + "/receipt.json"])
        require(receipt["schema"] == 1 and receipt["phase"] == phase and receipt["commit"] == COMMIT and
                receipt["manifest_sha256"] == MANIFEST_SHA and receipt["driver_sha256"] == DRIVER_SHA and
                receipt["completed"] is True and not ({"error", "error_type", "descendant_settlement_unknown"} & set(receipt)), "Receipt identity/outcome differs")
        require(receipt["source_before"] == receipt["source_after"] == source and receipt["tools_before"] == receipt["tools_after"] == tools, "Pre/post equality differs")
        require(all(receipt[k] is False for k in ("downloads", "installation", "whole_process_tree_deadline_enforced", "whole_process_tree_settlement_proven")) and receipt["scope"] == "PR627 reader experiment only; no original native-owner/FFI or Product qualification", "Scope claims differ")
        if phase == "prepare":
            require(receipt["vendor"] == vendor and receipt["archives"] == archive_pins, "Preparation pins differ")
            continue
        command, env = receipt["command"], receipt["environment"]
        args = ["--check", "--edition", "2021", "src/lib.rs", "src/windows.rs", "src/read_tests.rs"] if phase == "format" else ["test", "--offline", "--locked", "--lib", "--target", "x86_64-pc-windows-msvc", "--", "--test-threads=1"]
        tool = "rustfmt" if phase == "format" else "cargo"
        require(command["argv"] == [plan["tools"][tool]["path"], *args] and command["cwd"] == str(ROOT / "source-pr627" / SUBTREE), "Command differs")
        require(command["exit_code"] == 0 and command["settled"] is True and not command.get("interrupted_or_timed_out") and command["timeout_seconds"] == 600, "Command did not finish cleanly")
        require(env["CARGO_NET_OFFLINE"] == "true" and env["RUSTUP_AUTO_INSTALL"] == "0" and env["RUSTUP_OFFLINE"] == "1", "Offline settings differ")
        for key in ("CARGO_HOME", "CARGO_TARGET_DIR", "RUSTUP_HOME", "TEMP", "TMP", "TMPDIR", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "MCPHUB_RX04_FIXTURES"):
            require(".." not in PureWindowsPath(env[key]).parts and PureWindowsPath(env[key]).is_relative_to(ROOT / folder), "Writable environment outside run")
        require(set(command["streams"]) == {"stdout.log", "stderr.log"}, "Stream selection differs")
        for stream, pin in command["streams"].items():
            raw = data[folder + "/" + stream]
            require(pin == {"bytes": len(raw), "sha256": sha(raw)}, "Raw stream pin differs")
        if phase == "format":
            require(not data[folder + "/stdout.log"] and not data[folder + "/stderr.log"], "Formatting output differs")
    names = ["windows::tests::" + n for n in re.findall(r"#\[test\]\s*fn (\w+)\(", data["source-pr627/" + SUBTREE + "/src/read_tests.rs"].decode())]
    output = data[PHASES["test"] + "/stdout.log"].decode().replace("\r\n", "\n")
    terminals = re.findall(r"(?m)^test (\S+) \.\.\. (\S+)[ \t]*$", output)
    summaries = re.findall(r"(?m)^test result: (\w+)\. (\d+) passed; (\d+) failed; (\d+) ignored; (\d+) measured; (\d+) filtered out; finished in [0-9.]+s$", output)
    require(len(names) == len(set(names)) == 15 and Counter(names) == Counter(plan["expected_test_names"]) and
            Counter(terminals) == Counter((n, "ok") for n in names) and summaries == [("ok", "15", "0", "0", "0", "0")] and output.splitlines().count("running 15 tests") == 1, "Exact 15-test oracle differs")
    test = decoded(data[PHASES["test"] + "/receipt.json"])
    require(test["test_results"] == [list(x) for x in terminals] and test["test_summaries"] == [list(x) for x in summaries], "Receipt/original stdout differs")
    return {"integrity": "PASS", "recorded_tests": 15, "source_files": 7, "vendor_manifests": 13, "vendor_file_hash_entries": 797,
            "runtime_authenticated": False, "excluded_vendor_bytes_verified": False, "installed_tools_verified": False}


def zip_bytes(data):
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for name, raw in sorted(data.items()):
            entry = zipfile.ZipInfo(safe(name), (1980, 1, 1, 0, 0, 0))
            entry.compress_type, entry.create_system, entry.external_attr = zipfile.ZIP_DEFLATED, 3, (stat.S_IFREG | 0o644) << 16
            archive.writestr(entry, raw)
    return buffer.getvalue()


def unzip(raw):
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        rows = archive.infolist()
        require(len(rows) == 31 and len({x.filename.casefold() for x in rows}) == 31 and sum(x.file_size for x in rows) <= LIMIT, "ZIP size/count/collision")
        for row in rows:
            safe(row.filename)
            require(row.filename == row.orig_filename and not row.is_dir() and
                    row.compress_type in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED) and
                    stat.S_ISREG(row.external_attr >> 16) and not row.flag_bits & 1, "ZIP name/type/compression/encryption")
        require(archive.testzip() is None, "ZIP CRC failure")
        return {x.filename: archive.read(x) for x in rows}


def pack(script_sha, runtime_sha):
    root = Path(str(ROOT))
    require(os.name == "nt" and sys.flags.isolated and not sys.flags.optimize and Path(__file__).absolute().parent == root, "Use reviewed Windows root with Python -I -B")
    manifest = read(root / "manifest.json")
    require(sha(manifest) == MANIFEST_SHA, "Original manifest pin differs")
    plan = decoded(manifest)
    data = {n: read(root / n) for n in members(plan)}
    require(sha(data["reader627_evidence.py"]) == script_sha and sha(data["RUNTIME.md"]) == runtime_sha, "Reviewed packaging input differs")
    require(sum(map(len, data.values())) <= LIMIT, "Capture too large")
    profile = str(PureWindowsPath(plan["archive_source"]).parents[3])
    literals = {"json_escaped": json.dumps(profile)[1:-1].encode(), "plain": profile.encode()}
    public, mapping = {}, {}
    for name, raw in data.items():
        value, counts = raw, {}
        for key, literal in literals.items():
            counts[key], value = value.count(literal), value.replace(literal, b"<USER_PROFILE>")
        public[name] = value
        mapping[name] = {"raw_sha256": sha(raw), "raw_bytes": len(raw), "public_sha256": sha(value), "public_bytes": len(value), "changed": raw != value, "replacement_counts": counts}
    summary = evidence(public, mapping)
    raw_zip, public_zip = zip_bytes(data), zip_bytes(public)
    require(unzip(raw_zip) == data and unzip(public_zip) == public, "ZIP roundtrip differs")
    index = {"schema": "r25-reader627-public-evidence-v1", "public_zip_sha256": sha(public_zip), "files": mapping, "summary": summary,
             "public_manifest_sha256": sha(public["manifest.json"]), "raw_manifest_sha256": MANIFEST_SHA,
             "not_recomputable_from_public": sorted(n for n in mapping if mapping[n]["changed"]),
             "redaction": "Only the two exact case-sensitive plain/JSON-escaped profile literals become <USER_PROFILE>; other input bytes unchanged."}
    destination = root / "evidence-v1"
    no_links(root)
    destination.mkdir(exist_ok=False)
    for name, raw in (("private-originals.zip", raw_zip), ("public.zip", public_zip), ("public-index.json", encoded(index))):
        write(destination / name, raw)
    require(all(read(root / n) == raw for n, raw in data.items()), "Original changed during capture")
    completion = {"state": "COMPLETE", "files": {n: {"sha256": sha(read(destination / n)), "bytes": (destination / n).stat().st_size} for n in ("private-originals.zip", "public.zip", "public-index.json")}}
    write(destination / "completion.json", encoded(completion))
    print(json.dumps(completion))


def verify(archive_path, index_path, archive_sha, index_sha):
    raw, index_raw = read(archive_path), read(index_path)
    require(sha(raw) == archive_sha and sha(index_raw) == index_sha, "External ZIP/index pins differ")
    index = decoded(index_raw)
    require(index["schema"] == "r25-reader627-public-evidence-v1" and index["public_zip_sha256"] == archive_sha and index["raw_manifest_sha256"] == MANIFEST_SHA, "Index identity differs")
    data = unzip(raw)
    require(sha(data["manifest.json"]) == index["public_manifest_sha256"] and data["reader627_evidence.py"] == read(Path(__file__).absolute()), "Public manifest/verifier differs")
    require(index["not_recomputable_from_public"] == sorted(n for n, r in index["files"].items() if r["changed"]), "Original-byte caveat differs")
    require(evidence(data, index["files"]) == index["summary"], "Summary differs")
    print(json.dumps(index["summary"], sort_keys=True))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="mode", required=True)
    capture = sub.add_parser("pack")
    capture.add_argument("script_sha256")
    capture.add_argument("runtime_sha256")
    check = sub.add_parser("verify")
    for name in ("archive", "index", "archive_sha256", "index_sha256"):
        check.add_argument(name)
    args = parser.parse_args()
    if args.mode == "pack":
        pack(args.script_sha256, args.runtime_sha256)
    else:
        verify(Path(args.archive), Path(args.index), args.archive_sha256, args.index_sha256)
