"""Verify all 566 historical command captures as DATA after reconstruction.

Reads only reconstructed evidence directories. Does not read a live Source,
execute any preserved file, run a test, or grant qualification to a new run.
"""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath

INDEX_SHA256 = "1064ec8e060b826c9bc38a64b98ed5292042073d9a1bc8a9a60a3e895db9bb08"
MAIN_SHA256 = "6cd606a5ec4e6e1a3e74d4b8324786cc98dffe8cd587f4a41c89a751ea7f0c4a"
ROOT_RECEIVING_SHA256 = "f02bf04c80d0e84a5989297f742662ea874bbf7743c78ab3da947305004849d5"


def require(ok, message):
    if not ok:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def relative(value):
    require(isinstance(value, str) and value and "\\" not in value and ":" not in value,
            "plain relative evidence path required")
    path = PurePosixPath(value)
    require(not path.is_absolute() and ".." not in path.parts and
            "." not in path.parts and str(path) == value, "unclean evidence path")
    return Path(*path.parts)


def closed(command):
    require(command["quiescence_state"] == "complete" and
            command["captures_measured"] is True,
            "historical command capture closure differs")
    native = command.get("native")
    if native:
        require(native["direct_reaped"] is True and native["active_zero"] is True and
                native["handles_closed"] is True and not native.get("issues") and
                command["original_readers_joined"] is True,
                "original native command closure differs")
        for name in ("stdout", "stderr"):
            stream = command["io"][name]
            require(stream["worker_complete"] is True and stream["eof"] is True and
                    stream["pipe_close_returned"] is True and
                    stream["capture_close_returned"] is True and not stream.get("error"),
                    "original reader closure differs")
    else:
        require(command.get("completion_receiving") is not None,
                "original owner receiving missing")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--new-root", type=Path, required=True)
    parser.add_argument("--prior-full-root", type=Path, required=True)
    parser.add_argument("--prior-api-root", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    args = parser.parse_args()
    require(not args.receipt.exists() and args.receipt.parent.is_dir(),
            "DATA receipt must be a new file")
    capsule = Path(__file__).resolve().parent
    index_raw = (capsule / "capture-index.json").read_bytes()
    require(digest(index_raw) == INDEX_SHA256, "capture inventory changed")
    index = json.loads(index_raw)
    require(index["capture_count"] == 566 and index["command_count"] == 283 and
            index["classification"] == "FAIL/CLOSED", "fixed historical scope differs")
    roots = {"this": args.new_root.resolve(strict=True),
             "prior_full": args.prior_full_root.resolve(strict=True),
             "prior_api": args.prior_api_root.resolve(strict=True)}
    cache = {}

    def read(location, expected, size=None):
        key = (location["dependency"], location["original_role_path"])
        if key not in cache:
            root = roots[key[0]]
            path = root / relative(key[1])
            require(path.is_file() and not path.is_symlink() and
                    path.resolve().is_relative_to(root), "ordinary reconstructed file required")
            raw = path.read_bytes()
            cache[key] = (raw, digest(raw))
        raw, actual = cache[key]
        require(actual == expected and (size is None or len(raw) == size),
                "reconstructed original differs: " + key[1])
        return raw

    main_raw = read({"dependency": "prior_full", "original_role_path": "full/receipt.json"},
                    MAIN_SHA256)
    main_receipt = json.loads(main_raw)
    require(main_receipt["status"] == "FAIL" and
            main_receipt["literal_pre_push_commands_passed"] is False and
            main_receipt["all_original_invocations_closed"] is True and
            main_receipt["commands_quiescent"] is True,
            "historical full outcome was changed")
    root_audit = json.loads(read(
        {"dependency": "prior_full", "original_role_path": "root-receiving/result.json"},
        ROOT_RECEIVING_SHA256))
    require(root_audit["classification"] == "FAIL/CLOSED" and
            root_audit["commands_checked"] == 283 and
            root_audit["command_captures_hashed"] == 566 and
            root_audit["package_receipts_hashed"] == 65 and
            root_audit["original_owner_receipts_hashed"] == 130,
            "independent original root receiving differs")
    commands = {}
    for number, command in enumerate(main_receipt["commands"]):
        commands[(MAIN_SHA256, number)] = command
    packages = index["package_receipts"]
    require(len(packages) == 65, "package inventory differs")
    original_received = []
    for command in main_receipt["commands"]:
        if len(command.get("argv", [])) > 1 and command["argv"][1] == "test":
            original_received.extend(command["completion_receiving"]["received"])
    require({(row["package"], row["receipt_sha256"]) for row in original_received} ==
            {(row["package"], row["receipt_sha256"]) for row in packages} and
            len(original_received) == 65, "original package receiving relation differs")
    for row in packages:
        raw = read(row["content"], row["receipt_sha256"], row["receipt_bytes"])
        package = json.loads(raw)
        require(package["phase"] == row["phase"] and
                package["native_custody_settled"] is True and
                package["commands_quiescent"] is True and
                len(package["commands"]) == row["commands"],
                "closed package metadata differs")
        for number, command in enumerate(package["commands"]):
            key = (row["receipt_sha256"], number)
            require(key not in commands, "duplicate original command key")
            commands[key] = command
    require(len(commands) == 283, "original command count differs")
    for command in commands.values():
        closed(command)
    captures = index["captures"]
    require(len(captures) == 566 and
            len({row["closed_relative_path"] for row in captures}) == 566,
            "original capture paths overlap or are missing")
    seen, hashes, total, empty = set(), set(), 0, 0
    for row in captures:
        key = (row["receipt_sha256"], row["command_index"])
        stream = row["stream"]
        require(stream in ("stdout", "stderr") and (*key, stream) not in seen,
                "duplicate command stream")
        seen.add((*key, stream))
        command = commands[key]
        require(command[stream + "_sha256"] == row["sha256"] and
                command["exit_code"] == row["exit_code"] and
                command[stream].replace("\\", "/").endswith(
                    "/" + row["closed_relative_path"]), "original capture binding differs")
        raw = read(row["content"], row["sha256"], row["bytes"])
        total += len(raw)
        empty += not raw
        hashes.add(row["sha256"])
    require(len(seen) == 2 * len(commands) and len(hashes) == 285 and
            empty == 277 and total == 46234475,
            "complete original capture totals differ")
    result = {
        "schema": "closed-full-v6-all-captures-data-verification-v1",
        "status": "EXACT_CLOSED_FULL_V6_CAPTURE_DATA_VERIFIED",
        "classification": "FAIL/CLOSED",
        "capture_index_sha256": INDEX_SHA256,
        "main_receipt_sha256": MAIN_SHA256,
        "root_receiving_sha256": ROOT_RECEIVING_SHA256,
        "commands": 283,
        "captures": 566,
        "distinct_payloads": len(hashes),
        "empty_captures": empty,
        "total_original_capture_bytes": total,
        "package_receipts": 65,
        "original_native_owner_receipts_previously_received": 130,
        "new_native_or_test_execution": False,
        "runtime_sources_imported": False,
        "qualification_inherited": False,
    }
    with args.receipt.open("xb") as stream:
        stream.write((json.dumps(result, indent=2) + "\n").encode("utf-8"))
    print(json.dumps(result))


if __name__ == "__main__":
    main()
