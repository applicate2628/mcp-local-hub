"""Reconstruct frozen source bytes as DATA using explicit operator role values.

This program does not import or execute the preserved sources, contact a network,
run Git, change a gate, or supply any historical qualification.
"""
import argparse
import hashlib
import json
import re
from pathlib import Path, PurePosixPath

BINDINGS_SHA256 = "5e129d10d56d669674fd1831c361c06a337983f27b6212a62c0013a8afcf908b"
SCHEMA = "original-v6-source-preservation-bindings-v1"


def require(ok, message):
    if not ok:
        raise ValueError(message)


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def relative(value):
    require(isinstance(value, str) and "\\" not in value and ":" not in value,
            "plain relative POSIX artifact path required")
    path = PurePosixPath(value)
    require(value and not path.is_absolute() and ".." not in path.parts
            and "." not in path.parts and str(path) == value,
            "unclean artifact path refused")
    return Path(*path.parts)


def role_bytes(role_values, name, transform):
    text = role_values[name]
    if transform == "double_backslashes":
        text = text.replace("\\", "\\\\")
    else:
        require(transform == "literal", "unsupported role transformation")
    return text.encode("utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--roles-json", required=True, type=Path,
                        help="Operator-owned JSON containing all original role values named by the manifest.")
    parser.add_argument("--output", required=True, type=Path,
                        help="A new, absent output directory under the operator-approved work area.")
    args = parser.parse_args()
    capsule = Path(__file__).resolve().parent
    binding_raw = (capsule / "source-bindings.json").read_bytes()
    require(sha256(binding_raw) == BINDINGS_SHA256, "source bindings changed")
    bindings = json.loads(binding_raw)
    require(bindings["schema"] == SCHEMA and
            type(bindings["source_file_count"]) is int and
            bindings["source_file_count"] > 0 and
            len(bindings["files"]) == bindings["source_file_count"],
            "fixed preservation inventory differs")
    require(isinstance(bindings["required_roles"], dict),
            "explicit required roles missing")
    role_names = set(bindings["required_roles"])
    markers = {}
    for item in bindings["files"]:
        for substitution in item["substitutions"]:
            marker = substitution["marker"]
            pair = (substitution["role"], substitution["transform"])
            require(isinstance(marker, str) and
                    re.fullmatch(r"@@RECOVERY_[A-Z0-9_]+@@", marker) and
                    pair[0] in role_names and
                    pair[1] in ("literal", "double_backslashes"),
                    "unsupported publication marker or role")
            require(marker not in markers or markers[marker] == pair,
                    "conflicting publication marker meanings")
            markers[marker] = pair
    roles = json.loads(args.roles_json.read_bytes())
    require(isinstance(roles, dict) and set(roles) == role_names,
            "supply exactly the role keys listed in source-bindings.json")
    for name, value in roles.items():
        require(isinstance(value, str) and value and
                not any(c in value for c in ("\0", "\r", "\n")),
                "nonempty single-line role value required: " + name)
        require(not any(marker in value for marker in markers),
                "role values must not contain preservation markers")

    prepared = []
    seen = set()
    for item in bindings["files"]:
        published = capsule / relative(item["published_path"])
        require(not published.is_symlink() and published.is_file(),
                "ordinary published source file required")
        raw = published.read_bytes()
        require(len(raw) == item["published_bytes"] and
                sha256(raw) == item["published_sha256"],
                "published source bytes changed: " + item["published_path"])
        for substitution in item["substitutions"]:
            marker = substitution["marker"]
            require(marker in markers and
                    markers[marker] == (substitution["role"], substitution["transform"]),
                    "unexpected marker or role transformation")
            token = marker.encode("ascii")
            require(type(substitution["occurrences"]) is int and
                    substitution["occurrences"] > 0 and
                    raw.count(token) == substitution["occurrences"],
                    "literal substitution count differs")
            raw = raw.replace(token, role_bytes(
                roles, substitution["role"], substitution["transform"]))
        require(not any(marker.encode("ascii") in raw for marker in markers),
                "unreconstructed preservation marker")
        require(len(raw) == item["original_bytes"] and
                sha256(raw) == item["original_sha256"],
                "role values do not reconstruct the frozen original: " +
                item["original_role_path"])
        restored = relative(item["original_role_path"])
        require(restored not in seen, "duplicate restored artifact")
        seen.add(restored)
        prepared.append((restored, raw, item["original_sha256"]))

    # Validate every byte before creating any destination. Existing output
    # directories and files are never overwritten or cleaned up.
    require(not args.output.exists() and not args.output.is_symlink(),
            "output must be a new absent directory")
    args.output.mkdir(parents=False, exist_ok=False)
    receipt = []
    for restored, raw, expected in prepared:
        target = args.output / restored
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("xb") as stream:
            stream.write(raw)
        actual = target.read_bytes()
        require(actual == raw and sha256(actual) == expected,
                "written source bytes differ")
        receipt.append({"path": restored.as_posix(),
                        "bytes": len(actual), "sha256": expected})
    result = {
        "schema": "original-v6-exact-data-reconstruction-v1",
        "status": "EXACT_DATA_RECONSTRUCTED",
        "source_bindings_sha256": BINDINGS_SHA256,
        "files": receipt,
        "role_values_included": False,
        "sources_imported_or_executed": False,
        "qualification_inherited": False,
        "existing_public_inputs_manifest": bindings.get("public_exact_inputs_manifest"),
    }
    with (args.output / "reconstruction-receipt.json").open("xb") as stream:
        stream.write((json.dumps(result, indent=2) + "\n").encode("utf-8"))
    print(json.dumps({"status": result["status"], "files": len(receipt),
                      "sources_executed": False}))


if __name__ == "__main__":
    main()
