"""Freeze reviewed local base/candidate files and a JSON test plan, without Git.

Run from the task directory after review: python prepare_source.py [source-plan.json]
The plan has schema=2, base_commit, base_tree, files (relative Go path strings),
control_paths, native_scheduler_operations=false, and suites for
control/candidate/adjacent/published. Each suite has label, package, roots,
count, race, timeout_seconds, expected_exit_code, exact expected_events maps
for pass/fail/skip, and optional required_output [{test, any_of: [markers]}].
direct_timeout_seconds defaults to timeout_seconds+600; command_timeouts
default to check_recovery.DEFAULT_TIMEOUTS. These defaults are frozen in output.

Input trees are ./base and ./candidate. Existing outputs are never overwritten.
The product patch contains every changed path outside control_paths, including
candidate-only tests. This script does not clone, invoke tools or change inputs.
"""
import sys
sys.dont_write_bytecode = True

import copy
import difflib
import hashlib
import json
from pathlib import Path

from check_recovery import BASE, BASE_TREE, DEFAULT_TIMEOUTS, relative_source_path, sha, validate_manifest

ROOT = Path(__file__).resolve().parent


def blob(data):
    if data is None:
        return None
    return hashlib.sha1(b"blob " + str(len(data)).encode("ascii") + b"\0" + data).hexdigest()


def source_bytes(tree, relative):
    path = ROOT / tree / relative
    if not path.exists():
        return None
    if path.is_symlink() or not path.is_file() or not path.resolve().is_relative_to((ROOT / tree).resolve()):
        raise ValueError("Input is not an ordinary file inside its input tree: " + relative)
    raw = path.read_bytes()
    raw.decode("utf-8")
    if b"\0" in raw or b"\r" in raw or (raw and not raw.endswith(b"\n")):
        raise ValueError("Reviewed Go source must be UTF-8, LF-only, with a final newline: " + relative)
    return raw


def patch_for(path, before, after):
    if before is None and not after:
        raise ValueError("An empty new file cannot be represented by this text patch")
    return "".join(difflib.unified_diff(
        (before or b"").decode("utf-8").splitlines(True),
        (after or b"").decode("utf-8").splitlines(True),
        fromfile="/dev/null" if before is None else "a/" + path,
        tofile="/dev/null" if after is None else "b/" + path,
    ))


def prepare(plan_raw):
    plan = json.loads(plan_raw)
    if plan.get("schema") != 2 or plan.get("base_commit") != BASE or plan.get("base_tree") != BASE_TREE:
        raise ValueError("Plan must identify the reviewed c55d9ca base and exact tree")
    paths = plan.get("files")
    if not isinstance(paths, list) or not paths or any(not isinstance(p, str) for p in paths):
        raise ValueError("Plan files must be an explicit array of relative paths")
    paths = [relative_source_path(p) for p in paths]
    if len(set(paths)) != len(paths):
        raise ValueError("Duplicate source path")
    manifest = copy.deepcopy(plan)
    manifest["command_timeouts"] = {**DEFAULT_TIMEOUTS, **manifest.get("command_timeouts", {})}
    for suites in manifest.get("suites", {}).values():
        for suite in suites:
            suite.setdefault("direct_timeout_seconds", suite["timeout_seconds"] + 600)
            suite.setdefault("required_output", [])
    manifest["files"] = []
    pieces = {}
    for path in paths:
        before, after = source_bytes("base", path), source_bytes("candidate", path)
        manifest["files"].append({
            "path": path, "base_blob": blob(before), "base_sha256": None if before is None else sha(before),
            "candidate_blob": blob(after), "candidate_sha256": None if after is None else sha(after),
        })
        pieces[path] = patch_for(path, before, after)
    validate_manifest(manifest)
    controls = set(manifest["control_paths"])
    patches = {
        "control.patch": "".join(pieces[p] for p in paths if p in controls),
        "product.patch": "".join(pieces[p] for p in paths if p not in controls),
        "candidate.patch": "".join(pieces[p] for p in paths),
    }
    if any(not value for value in patches.values()):
        raise ValueError("Control, product and complete patches must all be nonempty")
    manifest.update(
        generator_sha256=sha(Path(__file__).read_bytes()),
        execution_runner_sha256=sha((ROOT / "check_recovery.py").read_bytes()),
        source_plan_sha256=sha(plan_raw),
        patch_sha256={name: sha(value.encode("utf-8")) for name, value in patches.items()},
    )
    return manifest, patches


def main():
    if sys.flags.optimize or len(sys.argv) > 2:
        raise SystemExit("Expected an optional reviewed source-plan.json path; do not use -O")
    plan_path = (ROOT / (sys.argv[1] if len(sys.argv) == 2 else "source-plan.json")).resolve()
    if not plan_path.is_relative_to(ROOT):
        raise SystemExit("Source plan must be within the task directory")
    manifest, patches = prepare(plan_path.read_bytes())
    outputs = {**patches, "source-manifest.json": json.dumps(manifest, indent=2, ensure_ascii=False) + "\n"}
    if any((ROOT / name).exists() for name in outputs):
        raise SystemExit("Frozen outputs already exist; preserve them before creating another reviewed attempt")
    for name, text in outputs.items():
        with (ROOT / name).open("x", encoding="utf-8", newline="\n") as stream:
            stream.write(text)
    print(json.dumps({
        "base_commit": BASE, "base_tree": BASE_TREE, "files": len(manifest["files"]),
        "control_paths": manifest["control_paths"], "source_plan_sha256": manifest["source_plan_sha256"],
        "source_manifest_sha256": sha((ROOT / "source-manifest.json").read_bytes()),
        "patch_sha256": manifest["patch_sha256"],
    }, indent=2))


if __name__ == "__main__":
    main()
