"""Check this capsule's exact bytes and Git metadata without executing Source."""
import hashlib
import json
import sys
from pathlib import Path, PurePosixPath

TARGET_COMMIT = "3ebe335a2f666a5279d7fafc31a6f4dc3feedff4"
TARGET_TREE = "67c13b8fc1317b4fd60e3135a3eb8a376fe01817"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def object_hash(kind, raw):
    return hashlib.sha1(kind.encode("ascii") + b" " + str(len(raw)).encode("ascii") + b"\0" + raw).hexdigest()


def tree_hash(node):
    entries = []
    for name, value in node.items():
        name = name.encode("utf-8")
        if isinstance(value, dict):
            entries.append((name + b"/", b"40000 " + name + b"\0" + bytes.fromhex(tree_hash(value))))
        else:
            mode, blob = value
            entries.append((name, mode.encode("ascii") + b" " + name + b"\0" + bytes.fromhex(blob)))
    return object_hash("tree", b"".join(raw for _, raw in sorted(entries)))


def main():
    root = Path(sys.argv[1]) if len(sys.argv) == 2 else Path(__file__).resolve().parent
    require(len(sys.argv) <= 2, "usage: verify_capsule.py [capsule-directory]")
    manifest = json.loads((root / "manifest.json").read_bytes())
    require(manifest["target_commit"] == TARGET_COMMIT and manifest["target_tree"] == TARGET_TREE,
            "different target metadata")
    entries = manifest["files"]
    names = {row["path"] for row in entries}
    require(len(entries) == len(names) and "manifest.json" not in names, "duplicate/self-referential manifest entry")
    for row in entries:
        name = row["path"]
        require(PurePosixPath(name).name == name and name not in ("", ".", ".."), "unexpected capsule member path")
        file = root / name
        require(file.is_file() and not file.is_symlink(), "ordinary capsule member required")
        raw = file.read_bytes()
        require(len(raw) == row["bytes"] and hashlib.sha256(raw).hexdigest() == row["sha256"],
                "capsule bytes changed: " + name)
    patch = (root / "source.patch").read_bytes()
    require(len(patch) == 3471196 and hashlib.sha256(patch).hexdigest()
            == "10af6f1b1b179c04299c13024ca75b4dc868fb945574a3e941b34ac343d7c03e", "patch identity differs")
    raw_commit = (root / "source.commit").read_bytes()
    require(len(raw_commit) == 276 and object_hash("commit", raw_commit) == TARGET_COMMIT
            and raw_commit.startswith(("tree " + TARGET_TREE + "\n").encode("ascii")), "raw commit identity differs")
    files = json.loads((root / "source-files.json").read_bytes())
    require(len(files) == len({row["path"] for row in files}) == 3508
            and sum(row["bytes"] for row in files) == 57144526, "target census differs")
    tree = {}
    for row in files:
        name = row["path"]
        parts = name.split("/")
        require(not name.startswith("/") and not any(part in ("", ".", "..") for part in parts)
                and row["mode"] == "100644" and row["type"] == "blob", "target inventory row differs")
        node = tree
        for part in parts[:-1]:
            require(part not in node or isinstance(node[part], dict), "file/directory collision")
            node = node.setdefault(part, {})
        require(parts[-1] not in node, "file/directory collision")
        node[parts[-1]] = (row["mode"], row["blob"])
    require(tree_hash(tree) == TARGET_TREE, "inventory tree identity differs")
    print(json.dumps({"status": "CAPSULE_BYTES_AND_METADATA_VERIFIED", "target_commit": TARGET_COMMIT,
        "target_tree": TARGET_TREE, "target_files": len(files), "target_bytes": 57144526,
        "capsule_members_hashed": len(entries), "git_reconstruction_executed_by_this_checker": False,
        "source_code_executed": False, "qualification_expanded": False}))


if __name__ == "__main__":
    main()
