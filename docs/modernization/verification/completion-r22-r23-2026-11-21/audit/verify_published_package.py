"""Fetch immutable published bytes and verify them without running project code."""
import argparse
import concurrent.futures
import datetime
import hashlib
import io
import json
from pathlib import Path
import time
import urllib.request
import zipfile

REPO = "applicate2628/mcp-local-hub"
PREFIX = "docs/modernization/verification/completion-r22-r23-2026-11-21/"
BOARD = "docs/modernization/plans/rust-independent-execution-plan-2026-11-12.md"
CODE = [
    ("610fb30348c10be7b645b6ce43c862397b190f60", "internal/autostart/linux.go", "6c7e1bc9d5811e39c1926779ccb7b339b6847d82"),
    ("610fb30348c10be7b645b6ce43c862397b190f60", "internal/autostart/linux_exec_percent_test.go", "fe201c084d0573eb76547e3dfb78eb3a049ac964"),
    ("610fb30348c10be7b645b6ce43c862397b190f60", "internal/api/install.go", "c034fcf8d85a9492114cb1c3325481d36dbacfa9"),
    ("610fb30348c10be7b645b6ce43c862397b190f60", "internal/api/install_task_rollback.go", "512c7e1af623491fc6abf44697b7794714f7c3aa"),
    ("610fb30348c10be7b645b6ce43c862397b190f60", "internal/api/install_scheduler_snapshot_test.go", "a7930cc1ddced0064b6a71a5ab8af8518a947990"),
    ("610fb30348c10be7b645b6ce43c862397b190f60", "internal/api/install_task_mutation_failure_test.go", "937c44fbd41f8638f8eae700396372c2d81548ae"),
    ("2eb7c114103ef5c60939cd114110ebe667901166", "experiments/rust-process-owner/README.md", "57ad9d27dcc1154ec128e16e8faf43689997e778"),
]


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def blob(raw):
    return hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def fetch(commit, path):
    url = f"https://raw.githubusercontent.com/{REPO}/{commit}/{path}"
    for attempt in range(3):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "mcp-evidence-readback"}), timeout=45) as response:
                return response.read()
        except Exception:
            if attempt == 2:
                raise
            time.sleep(attempt + 1)
    raise AssertionError("unreachable")


def save_new(path, raw):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("xb") as stream:
        stream.write(raw)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--commit", required=True)
    parser.add_argument("--staged", type=Path, required=True)
    parser.add_argument("--board", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    assert len(args.commit) == 40 and all(c in "0123456789abcdef" for c in args.commit)
    args.output.mkdir(parents=True, exist_ok=False)
    receipt = {"schema": 1, "started_utc": utc(), "repository": REPO, "commit": args.commit,
               "mode": "independent HTTP download from immutable GitHub commit", "files": [], "archives": [], "code_files": [],
               "project_code_executed": False, "status": "running"}
    receipt_path = args.output / "publication-readback.json"
    try:
        manifest_bytes = fetch(args.commit, PREFIX + "PUBLIC-FILES.json")
        assert manifest_bytes == (args.staged / "PUBLIC-FILES.json").read_bytes()
        manifest = json.loads(manifest_bytes)
        save_new(args.output / "package/PUBLIC-FILES.json", manifest_bytes)
        receipt["manifest"] = {"bytes": len(manifest_bytes), "sha256": sha(manifest_bytes), "git_blob": blob(manifest_bytes)}

        def verify_one(row):
            path = row["path"]
            assert not Path(path).is_absolute() and ".." not in Path(path).parts
            raw = fetch(args.commit, PREFIX + path)
            assert len(raw) == row["bytes"] and sha(raw) == row["sha256"] and blob(raw) == row["git_blob"], path
            assert raw == (args.staged / path).read_bytes(), path
            save_new(args.output / "package" / path, raw)
            return {"path": path, "bytes": len(raw), "sha256": sha(raw), "git_blob": blob(raw), "exact_staged_match": True}

        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            for item in pool.map(verify_one, manifest["files"]):
                receipt["files"].append(item)
        assert len(receipt["files"]) == len(manifest["files"])
        board = fetch(args.commit, BOARD)
        assert board == args.board.read_bytes()
        save_new(args.output / "board.md", board)
        receipt["board"] = {"repository_path": BOARD, "bytes": len(board), "sha256": sha(board), "git_blob": blob(board), "exact_staged_match": True}

        for path, manifest_name, key, expected in [
            ("r22/evidence-redacted.zip", "MANIFEST.json", "members", 5538),
            ("go-linux-build/evidence-redacted.zip", "MANIFEST.json", "members", 1187),
            ("rust/public-rust.zip", "PUBLIC-HASHES.json", "files", 48),
            ("audit/go-inventory/public-inventory.zip", "PUBLIC-HASHES.json", "files", 67),
            ("audit/pr-body-originals.zip", "MANIFEST.json", "files", 8),
        ]:
            raw = (args.output / "package" / path).read_bytes()
            with zipfile.ZipFile(io.BytesIO(raw)) as zipped:
                names = zipped.namelist()
                assert len(names) == len(set(names)) == expected + 1
                assert zipped.testzip() is None
                rows = json.loads(zipped.read(manifest_name))[key]
                assert len(rows) == expected
                assert set(names) == {r["path"] for r in rows} | {manifest_name}
                for row in rows:
                    member = zipped.read(row["path"])
                    byte_key = "public_bytes" if "public_bytes" in row else "bytes"
                    hash_key = "public_sha256" if "public_sha256" in row else "sha256"
                    assert len(member) == row[byte_key] and sha(member) == row[hash_key], (path, row["path"])
                if path != "audit/pr-body-originals.zip":
                    outer = Path(path).parent / manifest_name
                    assert zipped.read(manifest_name) == (args.output / "package" / outer).read_bytes()
            receipt["archives"].append({"path": path, "bytes": len(raw), "sha256": sha(raw), "members": len(names), "public_member_hashes_verified": expected, "crc_valid": True})

        linux_manifest = json.loads((args.output / "package/linux-execution/MANIFEST.json").read_bytes())
        for row in linux_manifest["files"]:
            raw = (args.output / "package/linux-execution" / row["path"]).read_bytes()
            assert len(raw) == row["public_bytes"] and sha(raw) == row["public_sha256"]
        receipt["linux_execution_files_verified"] = len(linux_manifest["files"])
        for commit, path, expected_blob in CODE:
            raw = fetch(commit, path)
            assert blob(raw) == expected_blob, path
            save_new(args.output / "code" / commit / path, raw)
            receipt["code_files"].append({"commit": commit, "path": path, "bytes": len(raw), "sha256": sha(raw), "git_blob": blob(raw)})
        receipt["status"] = "complete"
        receipt["all_checks_passed"] = True
    except Exception as exc:
        receipt["status"] = "failed"
        receipt["error"] = type(exc).__name__ + ": " + str(exc)
        raise
    finally:
        receipt["ended_utc"] = utc()
        receipt["runner_sha256"] = sha(Path(__file__).read_bytes())
        receipt_path.write_text(json.dumps(receipt, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"status": receipt["status"], "package_files_including_manifest": len(receipt["files"]) + 1,
                      "board_verified": True, "archives": receipt["archives"], "code_files_verified": len(receipt["code_files"])}))


if __name__ == "__main__":
    main()
