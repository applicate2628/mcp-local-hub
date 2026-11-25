#!/usr/bin/env python3
"""Reproduce the recorded source tree using pinned inputs and a private index.

Uses Python's standard library and Git only. Does not fetch, run application
code, alter refs, edit a worktree, or use the historical composition commits.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys


class ReplayError(Exception):
    pass


def relative_path(value):
    path = PurePosixPath(value)
    if path.is_absolute() or ".." in path.parts or "\\" in value or not value:
        raise ReplayError("Unsafe relative artifact or source path")
    return value


def object_id(value):
    if not re.fullmatch(r"[0-9a-f]{40}", value):
        raise ReplayError("Expected a complete SHA-1 Git object ID")
    return value


def run_replay(repository, work_dir, artifact_dir):
    manifest_bytes = (artifact_dir / "composition.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    if manifest["schema"] != "mcphub-integration-replay-v1":
        raise ReplayError("Unsupported composition schema")
    environment = os.environ.copy()
    for name in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
                 "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"):
        environment.pop(name, None)
    environment["GIT_INDEX_FILE"] = str(work_dir / "replay.index")

    def git(*arguments, data=None, check=True):
        result = subprocess.run(
            ["git", "-C", str(repository), "-c", "core.autocrlf=false",
             "-c", "merge.renormalize=false", *arguments], input=data,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment,
        )
        if check and result.returncode:
            detail = result.stderr.decode("utf-8", errors="replace")
            detail = detail.replace(str(repository), "<repository>")
            detail = detail.replace(str(work_dir), "<replay-work>")
            raise ReplayError("Git %s failed: %s" % (arguments[0], detail[:2000]))
        return result

    def artifact(record):
        path = artifact_dir / relative_path(record["file"])
        if not path.resolve().is_relative_to(artifact_dir):
            raise ReplayError("Artifact path leaves its evidence directory")
        content = path.read_bytes()
        if hashlib.sha256(content).hexdigest() != record["sha256"]:
            raise ReplayError("Artifact digest differs: " + record["file"])
        return content

    if git("rev-parse", "--show-object-format").stdout.strip() != b"sha1":
        raise ReplayError("This ledger records a SHA-1 Git repository")
    top = git("rev-parse", "--show-toplevel", check=False)
    if top.returncode == 0 and work_dir.is_relative_to(Path(top.stdout.decode().strip()).resolve()):
        raise ReplayError("The private replay directory must be outside the source worktree")
    if work_dir.is_relative_to(artifact_dir):
        raise ReplayError("The private replay directory must be outside the evidence files")
    for commit in manifest["required_input_commits"]:
        if git("cat-file", "-e", object_id(commit) + "^{commit}", check=False).returncode:
            raise ReplayError("Pinned input commit is unavailable locally: " + commit)

    owner_inputs = json.loads(artifact(manifest["owner_inputs"]))
    for owner in owner_inputs["owners"]:
        for entry in owner["files"]:
            path = relative_path(entry["path"])
            content = git("show", owner["head"] + ":" + path).stdout
            if hashlib.sha256(content).hexdigest() != entry["sha256"]:
                raise ReplayError("Owner code digest differs: " + owner["lane"] + ":" + path)
    # Validate this separate artifact, but never apply it to the result.
    artifact(manifest["negative_control"])
    work_dir.mkdir(parents=True, exist_ok=False)
    git("read-tree", object_id(manifest["initial"]["head"]))
    report = {
        "schema": "mcphub-integration-replay-verification-v1",
        "scope": "Source tree replay only; no build or native runtime execution",
        "composition_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
        "historical_target_commit": manifest["target"]["commit"],
        "expected_tree": manifest["target"]["tree"], "steps": [],
        "application_code_execution": "NONE", "network": "NONE",
        "refs_or_worktree_changes": "NONE",
        "owner_source_files_verified": sum(len(owner["files"]) for owner in owner_inputs["owners"]),
        "pinned_input_commits_verified": len(manifest["required_input_commits"]),
        "git_version": git("--version").stdout.decode().strip(),
        "python_version": ".".join(str(value) for value in sys.version_info[:3]),
    }

    for step in manifest["steps"]:
        before = git("write-tree").stdout.decode().strip()
        if before != step["before_tree"]:
            raise ReplayError("Pre-step tree differs: " + step["id"])
        if step["kind"] == "commit_delta":
            paths = [relative_path(path) for path in step["selected_paths"]]
            patch = git(
                "diff", "--binary", "--full-index", "--no-renames",
                "--no-ext-diff", "--no-textconv", "--no-color",
                "--src-prefix=a/", "--dst-prefix=b/", "--diff-algorithm=myers",
                object_id(step["input"]["base"]), object_id(step["input"]["head"]), "--", *paths,
            ).stdout if paths else b""
        elif step["kind"] == "patch":
            patch = artifact(step["patch"])
        else:
            raise ReplayError("Unknown replay step kind: " + step["kind"])
        applied = git("apply", "--3way", "--cached", "--whitespace=nowarn",
                      data=patch, check=False) if patch else None
        unmerged = git("ls-files", "--unmerged", "-z").stdout
        conflicts = sorted({item.split(b"\t", 1)[1].decode()
                            for item in unmerged.split(b"\0") if item})
        allowed = {entry["path"] for entry in step["resolutions"]}
        if set(conflicts) - allowed:
            raise ReplayError("Unrecorded conflicts at " + step["id"] + ": " + ", ".join(conflicts))
        if applied is not None and applied.returncode and not conflicts:
            raise ReplayError("Input patch failed without recorded conflicts: " + step["id"])
        for entry in step["resolutions"]:
            content = artifact(entry)
            if entry["mode"] not in ("100644", "100755"):
                raise ReplayError("Unsupported recorded source-file mode")
            blob = git("hash-object", "-w", "--stdin", data=content).stdout.decode().strip()
            if blob != object_id(entry["blob"]):
                raise ReplayError("Resolution blob differs: " + entry["path"])
            git("update-index", "--add", "--cacheinfo", entry["mode"],
                blob, relative_path(entry["path"]))
        if step.get("post_merge"):
            edit = step["post_merge"]
            if git("write-tree").stdout.decode().strip() != edit["before_tree"]:
                raise ReplayError("Pre-adjustment tree differs: " + step["id"])
            git("apply", "--cached", "--whitespace=nowarn", data=artifact(edit))
        actual = git("write-tree").stdout.decode().strip()
        if actual != step["expected_tree"]:
            raise ReplayError("Tree mismatch at %s: expected %s, got %s" %
                              (step["id"], step["expected_tree"], actual))
        report["steps"].append({"id": step["id"], "tree": actual,
                                "observed_conflicts": conflicts, "status": "PASS"})
        print(step["id"], actual, "PASS", flush=True)

    report["actual_tree"] = git("write-tree").stdout.decode().strip()
    if report["actual_tree"] != manifest["target"]["tree"]:
        raise ReplayError("Final tree differs")
    report["status"] = "PASS"
    (work_dir / "verification.json").write_text(
        json.dumps(report, indent=2) + "\n", encoding="utf-8",
    )
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", required=True, type=Path,
                        help="Existing repository containing the pinned input objects")
    parser.add_argument("--work-dir", required=True, type=Path,
                        help="New directory for the isolated index and verification report")
    args = parser.parse_args()
    try:
        report = run_replay(args.repository.resolve(), args.work_dir.resolve(),
                            Path(__file__).resolve().parent)
    except (ReplayError, OSError, ValueError, KeyError) as error:
        print("Replay failed:", error, file=sys.stderr)
        return 1
    print("FINAL", report["actual_tree"], "PASS (source replay only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
