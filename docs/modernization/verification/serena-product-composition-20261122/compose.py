#!/usr/bin/env python3
"""Materialize pinned candidate source files; never execute project code."""

import argparse
import collections
import difflib
import hashlib
import json
from pathlib import Path, PurePosixPath
import subprocess


BASE = "75dbc6406123e22468f168b861d5e0622c9d5769"
REFS = {
    "643": "daaa04c14bc3944babd4efbc3a125eaf495e6534",
    "650": "2a273c10f6e5c0651e89860f88ef0b6a0e3748ef",
    "654": "d01fb6e73013d6a47628b1a9cbecd2d5f3b8e32c",
    "655": "1a5a7dd04fe732df5c539860dde813c8c703586d",
    "657": "f20bc4ae5a369e7c0be0f7ad4a0f0ba9ea758a57",
    "637": "1ebe00c5dbac448488d980072e689b1126875dde",
    "646": "1df32782e7ade719e1d7f8c6153b4455a6ea740a",
    "647": "dca92972e90c5280e6c217409acfc6fd0dfb38ea",
    "649": "73caa97a860f1eaec205ec84c1502c47ce05cdc0",
    "653": "90fd6f69ea4a59856375e29b34cebf4953cb2ff4",
}
CORE = ["643", "650", "654", "655", "657"]
EXTENDED = CORE + ["637", "646", "647", "649", "653"]
BUILD_ENV = "internal/api/serena_build_environment.go"
RUNTIME = "internal/api/serena_prepared_runtime.go"
EXPECTED_RUNTIME = "0ea18f0a12369ee77853dfa768e0bce60e13f483fd94c91de497047ad0696980"
EXPECTED_BUILDER = "e811e4549672ec4cc3ea72e59b3fa6f0235191137e41007a3600edf0f6354fa8"
NOTES = {
    "copy": "Exact source blob; its preimage equals the pinned master.",
    "runtime": "Retain #643 transient generation custody and #655 finalizer-before-work-cancel ordering; disjoint base intervals.",
    "builder": "Retain master version 0.4.65 and its already common relative inventory guard; add #657 command-test mode and ASCII diagnostic.",
    "environment": "Retain both Windows policy bodies under one condition, constraint first then PATHEXT/Git; keep the common runtime import once. Projection entries remain sorted by the existing final sorter.",
    "inputs": "Retain #637 qualified-PATH discovery and #643 original probe cleanup deadline; disjoint base intervals.",
    "qualification": "Retain #643 original qualification cleanup deadline and #649 USERPROFILE bound to the held qualification root; disjoint base intervals.",
}
RESOLVED = {
    RUNTIME: "runtime",
    "build.ps1": "builder",
    BUILD_ENV: "environment",
    "internal/api/serena_prepared_inputs.go": "inputs",
    "internal/api/serena_qualification_resources.go": "qualification",
}


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def blob(raw):
    return hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()


class SourceConflict(Exception):
    pass


def compose(base, variants, path):
    """Apply independent edits; the only divergent insertion admitted is named."""
    lines = base.splitlines(keepends=True)
    edits = {}
    for source, raw in variants:
        changed = raw.splitlines(keepends=True)
        matcher = difflib.SequenceMatcher(a=lines, b=changed, autojunk=False)
        for op, start, end, a, b in matcher.get_opcodes():
            if op == "equal":
                continue
            replacement = changed[a:b]
            key = (start, end)
            if key not in edits:
                edits[key] = (replacement, [source])
                continue
            previous, owners = edits[key]
            if previous == replacement:
                owners.append(source)
                continue
            if path == BUILD_ENV and key == (214, 214) and owners == ["643"] and source == "647":
                opening = b'\tif runtime.GOOS == "windows" {\n'
                closing = b"\t}\n"
                if previous[0] != opening or replacement[0] != opening or previous[-1] != closing or replacement[-1] != closing:
                    raise SourceConflict("the reviewed Windows policy blocks changed")
                edits[key] = (previous[:-1] + replacement[1:], owners + [source])
                continue
            raise SourceConflict(f"different edits at base interval {key}: {owners} and {source}")
    ordered = sorted(edits)
    for left, right in zip(ordered, ordered[1:]):
        a, b = left
        c, d = right
        if b > c or (a == b and c <= a < d):
            raise SourceConflict(f"overlapping base intervals {left} and {right}")
    result = list(lines)
    for start, end in reversed(ordered):
        result[start:end] = edits[(start, end)][0]
    return b"".join(result)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    repo, output = args.repository.resolve(), args.output.resolve()
    if repo == output or repo in output.parents or output in repo.parents:
        raise SystemExit("candidate output and repository must be separate")

    def git(*argv):
        return subprocess.check_output([
            "git", "--no-replace-objects", "--no-optional-locks", "-C", str(repo), *argv
        ])

    def tree(ref):
        result = {}
        for row in git("ls-tree", "-r", "-z", ref).split(b"\0"):
            if not row:
                continue
            metadata, name = row.split(b"\t", 1)
            mode, kind, oid = metadata.decode().split()
            result[name.decode()] = (mode, kind, oid)
        return result

    trees = {BASE: tree(BASE)}
    sources, by_path = {}, collections.defaultdict(list)
    for source, ref in REFS.items():
        ancestor = git("merge-base", BASE, ref).decode().strip()
        trees.setdefault(ancestor, tree(ancestor))
        trees[ref] = tree(ref)
        rows = git("diff", "--name-status", ancestor, ref, "--", "*.go",
                   "build.ps1", "scripts/test-windows-product-build.ps1").decode().splitlines()
        paths = []
        for row in rows:
            status, path = row.split("\t")
            if status not in {"A", "M"}:
                raise SystemExit(f"unsupported source change {source}: {row}")
            parts = PurePosixPath(path)
            if parts.is_absolute() or ".." in parts.parts:
                raise SystemExit(f"unsafe repository path {path}")
            if path != "build.ps1" and trees[BASE].get(path) != trees[ancestor].get(path):
                raise SystemExit(f"master preimage changed: {source} {path}")
            paths.append(path)
            by_path[path].append(source)
        sources[source] = {"ref": ref, "merge_base": ancestor, "path_count": len(paths)}

    core_paths = sorted(path for path, owners in by_path.items() if set(owners) & set(CORE))
    extended_paths = sorted(by_path)
    if len(core_paths) != 37 or sum(p.endswith(".go") for p in core_paths) != 35 or len(extended_paths) != 74:
        raise SystemExit("the reviewed source path sets changed")

    needed = set()
    for path, owners in by_path.items():
        base_entry = trees[BASE].get(path)
        if base_entry:
            needed.add(base_entry[2])
        for source in owners:
            needed.add(trees[REFS[source]][path][2])
    builder_ancestor = sources["657"]["merge_base"]
    needed.add(trees[builder_ancestor]["build.ps1"][2])
    ordered = sorted(needed)
    batch = subprocess.check_output(
        ["git", "--no-replace-objects", "--no-optional-locks", "-C", str(repo), "cat-file", "--batch"],
        input=("\n".join(ordered) + "\n").encode(),
    )
    blobs, offset = {}, 0
    for oid in ordered:
        end = batch.index(b"\n", offset)
        header = batch[offset:end].decode().split()
        if len(header) != 3 or header[0] != oid or header[1] != "blob":
            raise SystemExit(f"required Git blob not received: {header}")
        length = int(header[2])
        raw = batch[end + 1:end + 1 + length]
        if len(raw) != length or blob(raw) != oid or batch[end + 1 + length:end + 2 + length] != b"\n":
            raise SystemExit(f"Git blob content mismatch: {oid}")
        blobs[oid] = raw
        offset = end + length + 2
    if offset != len(batch):
        raise SystemExit("unexpected trailing Git batch output")

    def content(ref, path):
        entry = trees[ref].get(path)
        return blobs[entry[2]] if entry else b""

    manifest = {
        "kind": "source-only candidate composition",
        "repository": "applicate2628/mcp-local-hub",
        "base": {"ref": BASE, "tree": git("rev-parse", BASE + "^{tree}").decode().strip()},
        "generator": {"file": "compose.py", "sha256": sha(Path(__file__).read_bytes())},
        "source_refs": sources,
        "sets": {
            "core": {"root": "core", "sources": CORE, "path_count": 37, "go_path_count": 35, "paths": core_paths},
            "extended": {"root": "extended", "sources": EXTENDED, "path_count": 74, "go_path_count": 72, "paths": extended_paths},
        },
        "resolution_notes": NOTES,
        "files": [],
        "blocked_files": [],
        "qualification": {
            "project_code_executed": False,
            "tests": "NOT_RUN",
            "builds": "NOT_RUN",
            "source_issuance": "NOT_RUN",
            "publisher_or_product_issued": False,
            "limit": "Candidate changed paths only; not a complete publisher Source or an installation admission.",
        },
    }
    pending = {}
    for path in extended_paths:
        original = content(BASE, path)
        base_entry = trees[BASE].get(path)
        entry = {
            "path": path,
            "base": {"blob": base_entry[2], "sha256": sha(original)} if base_entry else None,
            "inputs": [],
            "outputs": [],
        }
        for source in by_path[path]:
            source_entry = trees[REFS[source]][path]
            if source_entry[0] not in {"100644", "100755"} or source_entry[1] != "blob":
                raise SystemExit(f"source is not a regular file: {source} {path}")
            raw = content(REFS[source], path)
            entry["inputs"].append({"source": source, "blob": source_entry[2], "sha256": sha(raw)})
        for name, selected in [("core", CORE), ("extended", EXTENDED)]:
            owners = [source for source in by_path[path] if source in selected]
            if not owners:
                continue
            variants = [(source, content(REFS[source], path)) for source in owners]
            try:
                if path == "build.ps1":
                    raw = compose(content(builder_ancestor, path),
                                  [("master", original), ("657", variants[0][1])], path)
                    if sha(raw) != EXPECTED_BUILDER:
                        raise SourceConflict("the previously reviewed builder hash changed")
                elif len(variants) == 1:
                    raw = variants[0][1]
                else:
                    raw = compose(original, variants, path)
                if path == RUNTIME and sha(raw) != EXPECTED_RUNTIME:
                    raise SourceConflict("the previously reviewed runtime hash changed")
            except SourceConflict as failure:
                manifest["blocked_files"].append({"set": name, "path": path, "reason": str(failure)})
                continue
            resolution = RESOLVED.get(path, "copy") if len(owners) > 1 or path == "build.ps1" else "copy"
            outcome = {"sets": [name], "bytes": len(raw), "blob": blob(raw), "sha256": sha(raw), "resolution": resolution}
            same = next((v for v in entry["outputs"] if v["sha256"] == outcome["sha256"]), None)
            if same:
                same["sets"].append(name)
            else:
                entry["outputs"].append(outcome)
            pending[f"{name}/{path}"] = raw
        manifest["files"].append(entry)

    script = Path(__file__).read_bytes()
    pending["compose.py"] = script
    pending["manifest.json"] = (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode()
    # Refuse to overwrite any prior or unexpected artifact. Exact replay is read-only.
    expected_files = set(pending)
    if output.exists():
        actual_files = {p.relative_to(output).as_posix() for p in output.rglob("*") if p.is_file()}
        if actual_files - expected_files:
            raise SystemExit("output contains unknown files")
    for relative, raw in pending.items():
        target = output / relative
        if target.exists() and (target.is_symlink() or target.read_bytes() != raw):
            raise SystemExit(f"output preimage differs: {relative}")
    for relative, raw in pending.items():
        target = output / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        if not target.exists():
            with target.open("xb") as stream:
                stream.write(raw)
        if target.read_bytes() != raw:
            raise SystemExit(f"persisted output mismatch: {relative}")
    print(json.dumps({
        "candidate_files": len(pending) - 2,
        "core_files": sum(k.startswith("core/") for k in pending),
        "extended_files": sum(k.startswith("extended/") for k in pending),
        "blocked_files": manifest["blocked_files"],
        "manifest_sha256": sha(pending["manifest.json"]),
        "project_execution": False,
    }))


if __name__ == "__main__":
    main()
