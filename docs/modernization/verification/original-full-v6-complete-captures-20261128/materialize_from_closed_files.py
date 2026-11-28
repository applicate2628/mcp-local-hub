"""Materialize only pinned public evidence payloads from an already closed run.

DATA only: read exact files, verify hashes, substitute explicit publication roles,
write new payload files. No subprocess, runtime-source import, test, cache read,
directory scan, Git operation, receipt reclassification, or file overwrite.
"""
import argparse
import hashlib
import json
import re
from pathlib import Path, PurePosixPath

BINDINGS_SHA256 = "d834d994d3091d0d3db1e027f4d0dc8eb81bac023b46be27f1b429209de8c3e0"
SOURCE_MAP_SHA256 = "d265267f66aa492cd3d79d04d018df78068a903302c6ecc496ff076762066d1b"
MAIN_SHA256 = "6cd606a5ec4e6e1a3e74d4b8324786cc98dffe8cd587f4a41c89a751ea7f0c4a"


def require(ok, message):
    if not ok:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def relative(value):
    require(isinstance(value, str) and value and "\\" not in value and ":" not in value,
            "plain relative POSIX path required")
    path = PurePosixPath(value)
    require(not path.is_absolute() and ".." not in path.parts and
            "." not in path.parts and str(path) == value,
            "unclean relative path refused")
    return Path(*path.parts)


def beneath(path, root):
    try:
        path.resolve().relative_to(root.resolve())
        return True
    except ValueError:
        return False


def ordinary(path, root):
    require(beneath(path, root) and path.is_file() and not path.is_symlink(),
            "ordinary bounded input required")
    return path.read_bytes()


def role_value(roles, substitution):
    text = roles[substitution["role"]]
    transform = substitution["transform"]
    if transform == "quadruple_backslashes":
        text = text.replace("\\", "\\\\\\\\")
    elif transform == "double_backslashes":
        text = text.replace("\\", "\\\\")
    else:
        require(transform == "literal", "unknown role transform")
    return text.encode("utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--closed-root", type=Path, required=True)
    parser.add_argument("--roles-json", type=Path, required=True)
    parser.add_argument("--manifest-sha256", required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    args = parser.parse_args()
    capsule = Path(__file__).resolve().parent
    closed = args.closed_root.resolve(strict=True)
    require(closed.is_dir(), "closed input root must exist")
    require(not args.receipt.exists() and args.receipt.parent.is_dir() and
            not beneath(args.receipt, closed) and not beneath(args.receipt, capsule),
            "receipt must be a new file outside inputs and capsule")
    manifest_raw = ordinary(capsule / "artifact-manifest.json", capsule)
    require(digest(manifest_raw) == args.manifest_sha256,
            "reviewed artifact manifest changed")
    manifest = json.loads(manifest_raw)
    artifact_rows = {row["path"]: row for row in manifest["files"]}
    require(len(artifact_rows) == len(manifest["files"]), "duplicate artifact path")
    binding_raw = ordinary(capsule / "source-bindings.json", capsule)
    map_raw = ordinary(capsule / "closed-source-map.json", capsule)
    require(digest(binding_raw) == BINDINGS_SHA256 and
            digest(map_raw) == SOURCE_MAP_SHA256, "bounded generation inputs changed")
    bindings, source_map = json.loads(binding_raw), json.loads(map_raw)
    require(bindings["source_file_count"] == 333 and len(bindings["files"]) == 333 and
            source_map["schema"] == "closed-full-v6-bounded-source-map-v1" and
            source_map["closed_main_sha256"] == MAIN_SHA256,
            "fixed evidence scope differs")
    main_raw = ordinary(closed / relative(source_map["closed_main_relative_path"]), closed)
    require(digest(main_raw) == MAIN_SHA256, "original closed main receipt differs")
    original_main = json.loads(main_raw)
    require(original_main["status"] == "FAIL" and
            original_main["commands_quiescent"] is True and
            original_main["all_original_invocations_closed"] is True and
            original_main["literal_pre_push_commands_passed"] is False,
            "original FAIL/CLOSED classification unavailable")
    roles = json.loads(args.roles_json.read_bytes())
    require(set(roles) == set(bindings["required_roles"]),
            "supply exactly the original role keys")
    for role, value in roles.items():
        require(isinstance(value, str) and value and
                not any(c in value for c in ("\0", "\r", "\n", "@@RECOVERY_")),
                "invalid explicit role value: " + role)
    original_roles = [row["original_role_path"] for row in bindings["files"]]
    require(set(source_map["files"]) == set(original_roles) and
            len(original_roles) == len(set(original_roles)),
            "bounded source map is not exact")
    payload_paths = {row["published_path"] for row in bindings["files"]}
    require(len(payload_paths) == 333, "payload destinations overlap")
    for name, row in artifact_rows.items():
        path = capsule / relative(name)
        if name in payload_paths:
            require(not path.exists() and not path.is_symlink(),
                    "payload already exists; no overwrite is permitted")
        else:
            raw = ordinary(path, capsule)
            require(len(raw) == row["bytes"] and digest(raw) == row["sha256"],
                    "prepared static artifact changed: " + name)
    prepared = []
    for item in bindings["files"]:
        source = closed / relative(source_map["files"][item["original_role_path"]])
        original = ordinary(source, closed)
        require(len(original) == item["original_bytes"] and
                digest(original) == item["original_sha256"], "closed original differs")
        raw = original
        if item.get("data_transform") is not None:
            require(item["data_transform"] == "nul_paths_as_lf_lines" and
                    item["path_count"] == 3509 and raw.endswith(b"\0") and
                    raw.count(b"\0") == 3509 and b"\n" not in raw and b"\r" not in raw,
                    "fixed original NUL path list differs")
            for entry in raw[:-1].split(b"\0"):
                relative(entry.decode("utf-8"))
            raw = raw.replace(b"\0", b"\n")
        for substitution in item["substitutions"]:
            marker = substitution["marker"]
            require(re.fullmatch(r"@@RECOVERY_[A-Z0-9_]+@@", marker),
                    "unsupported role marker")
            token = marker.encode("ascii")
            value = role_value(roles, substitution)
            require(token not in raw and
                    raw.count(value) == substitution["occurrences"],
                    "publication substitution inventory changed")
            raw = raw.replace(value, token)
        require(len(raw) == item["published_bytes"] and
                digest(raw) == item["published_sha256"],
                "forward projection differs from reviewed public bytes")
        row = artifact_rows[item["published_path"]]
        require(row["bytes"] == len(raw) and row["sha256"] == digest(raw),
                "manifest and binding disagree")
        target = capsule / relative(item["published_path"])
        require(beneath(target, capsule), "payload destination escapes capsule")
        prepared.append((target, raw, item))
    # Every input and every output byte is checked before the first payload write.
    results = []
    for target, raw, item in prepared:
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("xb") as stream:
            stream.write(raw)
        actual = target.read_bytes()
        require(actual == raw and digest(actual) == item["published_sha256"],
                "new payload readback differs")
        results.append({
            "original_role_path": item["original_role_path"],
            "original_sha256": item["original_sha256"],
            "published_path": item["published_path"],
            "bytes": len(actual),
            "sha256": digest(actual),
        })
    result = {
        "schema": "closed-full-v6-data-materialization-v1",
        "status": "EXACT_PUBLIC_PAYLOADS_MATERIALIZED",
        "original_classification": "FAIL/CLOSED",
        "bindings_sha256": BINDINGS_SHA256,
        "source_map_sha256": SOURCE_MAP_SHA256,
        "artifact_manifest_sha256": args.manifest_sha256,
        "original_main_sha256": MAIN_SHA256,
        "payloads": results,
        "role_values_included": False,
        "original_files_modified": False,
        "runtime_sources_imported": False,
        "new_native_or_test_commands": False,
        "qualification_inherited": False,
    }
    with args.receipt.open("xb") as stream:
        stream.write((json.dumps(result, indent=2) + "\n").encode("utf-8"))
    print(json.dumps({"status": result["status"], "payloads": len(results),
                      "published_bytes": sum(row["bytes"] for row in results)}))


if __name__ == "__main__":
    main()
