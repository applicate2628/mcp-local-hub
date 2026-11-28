"""Reconstruct original native-owner JSON bytes from pinned package proof text.

DATA only. Preserve lexical numbers, key order, escapes and values. Remove only
four spaces of embedding indentation and append one LF; verify each original SHA.
"""
import argparse
import hashlib
import json
import re
from pathlib import Path, PurePosixPath

INDEX_SHA256 = "8100488ccc5fc9d5e524e44b191e6a22a87d2d679923da824721ab3f6a3d16eb"
MAIN_SHA256 = "6cd606a5ec4e6e1a3e74d4b8324786cc98dffe8cd587f4a41c89a751ea7f0c4a"


def require(ok, message):
    if not ok:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def relative(value):
    require(isinstance(value, str) and value and "\\" not in value and ":" not in value,
            "plain relative role path required")
    path = PurePosixPath(value)
    require(not path.is_absolute() and ".." not in path.parts and
            "." not in path.parts and str(path) == value, "unclean role path")
    return Path(*path.parts)


def ordinary(path, root):
    require(path.is_file() and not path.is_symlink() and
            path.resolve().is_relative_to(root.resolve()), "ordinary bounded evidence file required")
    return path.read_bytes()


def raw_field(text, wanted):
    decoder = json.JSONDecoder()
    size = len(text)

    def skip_space(position):
        while position < size and text[position] in " \t\r\n":
            position += 1
        return position

    position = skip_space(0)
    require(position < size and text[position] == "{", "JSON object required")
    position += 1
    while position < size:
        position = skip_space(position)
        if text[position] == "}":
            break
        key, position = decoder.raw_decode(text, position)
        require(isinstance(key, str), "JSON member name required")
        position = skip_space(position)
        require(text[position] == ":", "JSON member separator missing")
        start = skip_space(position + 1)
        # The decoded value is discarded. Return the original lexical span;
        # no numeric value, string escape or key order is reserialized.
        _, end = decoder.raw_decode(text, start)
        if key == wanted:
            return text[start:end]
        position = skip_space(end)
        if text[position] == ",":
            position += 1
            continue
        require(text[position] == "}", "JSON object separator missing")
        break
    raise ValueError("required JSON member absent: " + wanted)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--captures-root", type=Path, required=True)
    parser.add_argument("--prior-full-root", type=Path, required=True)
    parser.add_argument("--prior-api-root", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--output-originals", type=Path,
                        help="Optional new absent directory for the 130 original owner JSON files.")
    args = parser.parse_args()
    roots = {"this": args.captures_root.resolve(strict=True),
             "prior_full": args.prior_full_root.resolve(strict=True),
             "prior_api": args.prior_api_root.resolve(strict=True)}
    require(not args.receipt.exists() and args.receipt.parent.is_dir() and
            not any(args.receipt.resolve().is_relative_to(root) for root in roots.values()),
            "receipt must be a new file outside input directories")
    if args.output_originals is not None:
        require(not args.output_originals.exists() and not args.output_originals.is_symlink() and
                args.output_originals.parent.is_dir() and
                not any(args.output_originals.resolve().is_relative_to(root) for root in roots.values()),
                "original output directory must be new and outside inputs")
    capsule = Path(__file__).resolve().parent
    raw_index = (capsule / "owner-reconstruction-index.json").read_bytes()
    require(digest(raw_index) == INDEX_SHA256, "owner reconstruction inventory changed")
    index = json.loads(raw_index)
    require(index["owner_receipts"] == 130 and len(index["files"]) == 130,
            "fixed original owner inventory differs")
    main_raw = ordinary(roots["prior_full"] / "full" / "receipt.json", roots["prior_full"])
    require(digest(main_raw) == MAIN_SHA256, "original main receipt differs")
    main_receipt = json.loads(main_raw)
    require(main_receipt["status"] == "FAIL" and main_receipt["commands_quiescent"] is True and
            main_receipt["all_original_invocations_closed"] is True, "original full result differs")
    expected = []
    for command in main_receipt["commands"]:
        if len(command.get("argv", [])) > 1 and command["argv"][1] == "test":
            for package in command["completion_receiving"]["received"]:
                for owner in package["original_owners"]:
                    expected.append((package["receipt_sha256"], owner["receipt_sha256"], owner["kind"]))
    actual = [(row["package_receipt_sha256"], row["original_sha256"], row["native_kind"])
              for row in index["files"]]
    require(len(expected) == 130 and len(set(actual)) == 130 and
            set(actual) == set(expected), "original native-owner relation differs")
    cache, prepared, rows, destinations = {}, [], [], set()
    for item in index["files"]:
        location = item["package_content"]
        key = (location["dependency"], location["original_role_path"])
        if key not in cache:
            root = roots[key[0]]
            raw = ordinary(root / relative(key[1]), root)
            cache[key] = (raw, digest(raw))
        raw, actual_sha = cache[key]
        require(actual_sha == item["package_receipt_sha256"] and
                len(raw) == item["package_receipt_bytes"], "package receipt bytes differ")
        field = item["proof_field"]
        require(field in ("inventory_owner", "owner"), "unexpected proof member")
        record = raw_field(raw.decode("utf-8"), field)
        record_object = json.loads(record)
        require(record_object["sha256"] == item["original_sha256"], "owner record binding differs")
        proof = raw_field(record, "proof")
        match = re.search(r"\n( *)\}$", proof)
        require(match is not None and len(match[1]) == item["embedding_indent_spaces"] == 4 and
                item["rule"] == "dedent_parent_proof_plus_LF",
                "fixed lexical indentation recipe differs")
        prefix = match[1]
        lines = proof.split("\n")
        require(all(line.startswith(prefix) for line in lines[1:]),
                "inconsistent embedded proof indentation")
        restored = (lines[0] + "\n" + "\n".join(line[len(prefix):] for line in lines[1:]) + "\n").encode("utf-8")
        require(len(restored) == item["original_bytes"] and
                digest(restored) == item["original_sha256"], "original owner bytes not reconstructed")
        role = relative(item["original_role_path"])
        require(role not in destinations, "duplicate owner output role")
        destinations.add(role)
        prepared.append((role, restored))
        rows.append({"original_role_path": item["original_role_path"],
                     "package_receipt_sha256": item["package_receipt_sha256"],
                     "proof_field": field, "bytes": len(restored),
                     "sha256": item["original_sha256"]})
    require(len(cache) == 65 and sum(len(raw) for _, raw in prepared) == 348272,
            "fixed owner byte total differs")
    if args.output_originals is not None:
        args.output_originals.mkdir(parents=False, exist_ok=False)
        for role, raw in prepared:
            target = args.output_originals / role
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open("xb") as stream:
                stream.write(raw)
            require(target.read_bytes() == raw, "new owner file readback differs")
    result = {
        "schema": "closed-full-v6-original-owner-byte-replay-v1",
        "status": "ALL_130_ORIGINAL_OWNER_BYTES_RECONSTRUCTED",
        "historical_classification": "FAIL/CLOSED",
        "owner_index_sha256": INDEX_SHA256,
        "main_receipt_sha256": MAIN_SHA256,
        "package_receipts": len(cache), "original_owner_receipts": len(rows),
        "original_owner_bytes": 348272, "files": rows,
        "numeric_lexemes_reserialized": False,
        "operator_role_values_included": False,
        "original_owner_files_read_by_replayer": False,
        "native_closure_re_received": False,
        "new_native_or_test_execution": False,
        "qualification_inherited": False,
        "new_original_files_written": args.output_originals is not None,
    }
    with args.receipt.open("xb") as stream:
        stream.write((json.dumps(result, indent=2) + "\n").encode("utf-8"))
    print(json.dumps({key: result[key] for key in
                      ("status", "original_owner_receipts", "original_owner_bytes",
                       "new_original_files_written", "historical_classification")}))


if __name__ == "__main__":
    main()
