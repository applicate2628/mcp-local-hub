#!/usr/bin/env python3
"""Restore and verify the exact P02 snapshot in a new bare object database."""

import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess


ENV = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
ENV.update(
    GIT_NO_REPLACE_OBJECTS="1",
    GIT_NO_LAZY_FETCH="1",
    GIT_ALLOW_PROTOCOL="",
    GIT_TERMINAL_PROMPT="0",
    GIT_CONFIG_NOSYSTEM="1",
    GIT_CONFIG_GLOBAL=os.devnull,
    GIT_OPTIONAL_LOCKS="0",
)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def git(repository, *arguments, data=None):
    result = subprocess.run(
        ["git", "-C", str(repository), *arguments], input=data,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV,
    )
    require(result.returncode == 0, result.stderr.decode("utf-8", "replace").strip())
    return result.stdout


def oid_bytes(objects):
    return "".join(oid + "\n" for oid in sorted(objects)).encode("ascii")


def oid_digest(objects):
    return hashlib.sha256(oid_bytes(objects)).hexdigest()


def object_hash(kind, content):
    header = (kind + " " + str(len(content)) + "\0").encode("ascii")
    return hashlib.sha1(header + content).hexdigest()


def tree_objects(repository, tree):
    kinds = {tree: "tree"}
    entries = git(repository, "ls-tree", "-r", "-t", "--format=%(objecttype) %(objectname)", tree)
    for line in entries.decode("ascii").splitlines():
        kind, oid = line.split()
        require(kind in ("tree", "blob"), "Snapshot contains an unsupported gitlink/type")
        kinds[oid] = kind
    return kinds


def import_pack(database, content, expected=None, specification=None):
    result = git(database, "index-pack", "--stdin", data=content).decode("ascii").split()
    pack_id = result[-1]
    require(re.fullmatch(r"[0-9a-f]{40}", pack_id), "Unexpected index-pack output")
    index = database / "objects" / "pack" / ("pack-" + pack_id + ".idx")
    output = git(database, "verify-pack", "-v", str(index)).decode("ascii")
    objects = {line.split()[0] for line in output.splitlines() if re.match(r"^[0-9a-f]{40} ", line)}
    if expected is not None:
        require(objects == expected, "Exported pack contains a different object set")
    if specification is not None:
        require(len(objects) == specification["object_count"], "Capsule pack object count differs")
        require(oid_digest(objects) == specification["object_ids_sha256"], "Capsule pack object IDs differ")
    return objects


def copy_exact_objects(repository, database, objects):
    if not objects:
        return set()
    pack = git(
        repository, "pack-objects", "--stdout", "--no-reuse-delta",
        "--no-reuse-object", "--window=0", "--threads=1", data=oid_bytes(objects),
    )
    # No --revs: only these named objects are exported, with no parent walk.
    return import_pack(database, pack, expected=objects)


def rehash_objects(database, objects, work_dir):
    request = work_dir / "target-objects.txt"
    request.write_bytes(oid_bytes(objects))
    counts = Counter()
    total_bytes = 0
    stderr_path = work_dir / "cat-file.stderr.txt"
    with request.open("rb") as source, stderr_path.open("wb") as errors:
        with subprocess.Popen(
            ["git", "-C", str(database), "cat-file", "--batch"],
            stdin=source, stdout=subprocess.PIPE, stderr=errors, env=ENV,
        ) as child:
            for oid in sorted(objects):
                fields = child.stdout.readline().decode("ascii").split()
                require(len(fields) == 3 and fields[0] == oid, "Target object is missing: " + oid)
                kind, size = fields[1], int(fields[2])
                digest = hashlib.sha1((kind + " " + str(size) + "\0").encode("ascii"))
                remaining = size
                while remaining:
                    chunk = child.stdout.read(min(remaining, 65536))
                    require(bool(chunk), "Truncated target object: " + oid)
                    digest.update(chunk)
                    remaining -= len(chunk)
                require(child.stdout.read(1) == b"\n", "Invalid cat-file object boundary")
                require(digest.hexdigest() == oid, "Target object content hash differs: " + oid)
                counts[kind] += 1
                total_bytes += size
            require(child.stdout.read() == b"", "Unexpected extra cat-file output")
            require(child.wait() == 0, "cat-file failed; inspect " + str(stderr_path))
    return dict(counts), total_bytes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", required=True, type=Path, help="Existing repository with the pinned public snapshots")
    parser.add_argument("--work-dir", required=True, type=Path, help="New directory outside the source repository and this capsule")
    arguments = parser.parse_args()
    repository = arguments.repository.resolve()
    work_dir = arguments.work_dir.resolve()
    capsule = Path(__file__).resolve().parent
    manifest_bytes = (capsule / "manifest.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    target = manifest["target"]
    coverage = manifest["coverage"]
    common = Path(git(repository, "rev-parse", "--path-format=absolute", "--git-common-dir").decode().strip()).resolve()
    for protected in (repository, common, capsule):
        require(work_dir != protected and protected not in work_dir.parents, "Work directory must be outside source/capsule storage")
    require(not work_dir.exists(), "Work directory already exists; choose a new owned directory")

    public = set()
    public_trees = set()
    require(len(manifest["public_snapshots"]) == coverage["public_snapshot_count"], "Public snapshot count differs")
    for snapshot in manifest["public_snapshots"]:
        commit, tree = snapshot["commit"], snapshot["tree"]
        raw = git(repository, "cat-file", "commit", commit)
        require(object_hash("commit", raw) == commit, "Public raw commit hash differs")
        require(raw.splitlines()[0] == ("tree " + tree).encode(), "Public root tree differs")
        kinds = tree_objects(repository, tree)
        objects = set(kinds) | {commit}
        require(len(objects) == snapshot["snapshot_object_count"], "Public snapshot object count differs")
        require(oid_digest(objects) == snapshot["snapshot_object_ids_sha256"], "Public snapshot object IDs differ")
        public.update(objects)
        public_trees.update(oid for oid, kind in kinds.items() if kind == "tree")
    require(len(public) == coverage["public_union_object_count"], "Public union count differs")
    require(oid_digest(public) == coverage["public_union_object_ids_sha256"], "Public union IDs differ")
    require(len(public_trees) == coverage["public_tree_object_count"], "Public tree count differs")
    require(oid_digest(public_trees) == coverage["public_tree_object_ids_sha256"], "Public tree IDs differ")

    work_dir.mkdir(parents=True)
    database = work_dir / "snapshot.git"
    git(work_dir, "init", "--bare", "--object-format=sha1", "--template=", str(database))
    imported = copy_exact_objects(repository, database, public_trees)
    private = set()
    for specification in manifest["packs"]:
        name = specification["path"]
        require(Path(name).name == name, "Capsule pack must be a local filename")
        content = (capsule / name).read_bytes()
        require(len(content) == specification["bytes"], "Capsule pack length differs")
        require(hashlib.sha256(content).hexdigest() == specification["sha256"], "Capsule pack SHA-256 differs")
        objects = import_pack(database, content, specification=specification)
        require(not objects & imported, "Capsule pack overlaps another declared contribution")
        private.update(objects)
        imported.update(objects)

    raw = git(database, "cat-file", "commit", target["commit"])
    expected_raw = (capsule / target["raw_commit_file"]).read_bytes()
    require(raw == expected_raw and len(raw) == target["raw_commit_bytes"], "Raw P02 header/body differs")
    require(hashlib.sha256(raw).hexdigest() == target["raw_commit_sha256"], "Raw P02 SHA-256 differs")
    require(object_hash("commit", raw) == target["commit"], "P02 commit identity differs")
    require(raw.splitlines()[0] == ("tree " + target["tree"]).encode(), "P02 root tree differs")
    required = set(tree_objects(database, target["tree"])) | {target["commit"]}
    require(len(required) == target["unique_object_count"], "P02 unique object count differs")
    require(oid_digest(required) == target["object_ids_sha256"], "P02 object set differs")
    require(private <= required, "Capsule includes objects outside P02")
    require(required <= public | private, "P02 needs an undeclared object")
    require(not private & public, "Private/public contributions overlap")
    require(len(required & public) == coverage["P02_objects_from_public_snapshots"], "Public P02 contribution differs")
    require(len(public_trees - required) == coverage["public_only_staging_tree_count"], "Public staging tree count differs")
    remaining = required - imported
    require(remaining <= public, "A missing P02 object is not in a public snapshot")
    imported.update(copy_exact_objects(repository, database, remaining))
    require(imported == required | public_trees, "Unexpected restored object set")

    actual = {line.decode().split()[0] for line in git(database, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").splitlines()}
    require(actual == imported, "New database contains undeclared objects")
    require(len(actual) == coverage["expected_restored_database_objects"], "Restored database count differs")
    counts, total_bytes = rehash_objects(database, required, work_dir)
    require(counts == target["unique_object_types"], "P02 object type counts differ")
    parent_availability = {}
    for parent in target["parents"]:
        result = subprocess.run(["git", "-C", str(database), "cat-file", "-e", parent], env=ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        parent_availability[parent] = result.returncode == 0
    require(not any(parent_availability.values()), "Unexpected parent history in snapshot capsule")
    require(not (database / "objects" / "info" / "alternates").exists(), "Output DB has alternates")
    require(not git(database, "for-each-ref"), "Output DB unexpectedly has refs")
    report = {
        "status": "PASS_SNAPSHOT_RESTORATION_ONLY",
        "manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
        "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "commit": target["commit"], "tree": target["tree"],
        "public_snapshots": len(manifest["public_snapshots"]),
        "public_union_objects": len(public),
        "target_objects": len(required), "target_types": counts,
        "rehash_target_payload_bytes": total_bytes,
        "target_object_ids_sha256": oid_digest(required),
        "target_from_public": len(required & public),
        "target_from_capsule": len(private),
        "database_objects": len(actual),
        "extra_declared_public_trees": len(actual - required),
        "parent_objects_present": parent_availability,
        "history_complete": False, "alternates": False, "branch_or_tag_refs_created": 0,
        "network_fetches": 0, "checkout_created": False,
        "Go_native_publication": "NOT_RUN",
    }
    (work_dir / "verification.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
