"""Move only verified redundant transfer files to fresh trash; never delete."""
import argparse
import base64
import datetime
import gzip
import hashlib
import json
import os
from pathlib import Path
import zipfile

ARCHIVES = {
    "r22/evidence-redacted.zip": "ab979b12c68d9d311516dec82d0cd65cdfb901bf3de5f106debd2f1b71fec654",
    "go-linux-build/evidence-redacted.zip": "e2dce14e2374c37c9be44281af2b1d1397d48d58b61cd00740038e07c8680f61",
    "rust/public-rust.zip": "73d01303df3c480cad4ffb6db3520f44df7cf83281c7bc8786bbc70eedc3222f",
}
PAYLOAD_NAMES = {"internal/api/install.go", "internal/api/install_task_rollback.go",
                 "internal/api/install_scheduler_snapshot_test.go", "internal/api/install_task_mutation_failure_test.go"}


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def decode(raw):
    return base64.b64decode(b"".join(raw.split()), validate=True)


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("local", "remote"), required=True)
    parser.add_argument("--readback", type=Path, required=True)
    args = parser.parse_args()
    root = Path(r"D:\dev\ChatGPT") if args.mode == "remote" else Path(__file__).resolve().parent.parent
    if args.mode == "remote" and os.name != "nt":
        raise RuntimeError("Remote mode requires the authorized Windows workspace")
    readback = json.loads(args.readback.read_bytes())
    assert readback["status"] == "complete" and readback["all_checks_passed"]
    observed = {row["path"]: row for row in readback["archives"]}
    for name, expected in ARCHIVES.items():
        assert observed[name]["sha256"] == expected and observed[name]["crc_valid"]
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    trash = root / "trash" / ("r22-r23-transfers-" + stamp)
    record_dir = root / ("mcp-r23-current-outcome-20261121/cleanup" if args.mode == "remote" else "completion-docs/cleanup")
    record_dir.mkdir(parents=True, exist_ok=False)
    receipt_path = record_dir / "receipt.json"
    receipt = {"schema": 1, "mode": args.mode, "started_utc": now(), "published_evidence_commit": readback["commit"],
               "publication_readback_sha256": digest(args.readback.read_bytes()), "trash_relative": str(trash.relative_to(root)),
               "status": "preparing", "moved": [], "preserved_checks": [], "trash_emptied": False, "delete_calls": 0}
    plan, preserve = [], {}

    def record():
        receipt_path.write_text(json.dumps(receipt, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    def kept(path, raw=None):
        assert path.is_relative_to(root) and not path.is_relative_to(trash) and not path.is_symlink()
        actual = path.read_bytes()
        if raw is not None:
            assert actual == raw, str(path)
        preserve[path] = {"path": str(path.relative_to(root)), "bytes": len(actual), "sha256": digest(actual)}
        return actual

    def add(path, reason):
        assert path.is_relative_to(root) and path.is_file() and not path.is_symlink()
        assert path not in preserve and all(old[0] != path for old in plan)
        raw = path.read_bytes()
        plan.append((path, {"source": str(path.relative_to(root)), "bytes": len(raw), "sha256": digest(raw), "reason": reason}))

    try:
        if args.mode == "local":
            r22 = root / "r22-packaging/public-r22/evidence-redacted.zip"
            golinux = root / "go-linux-package/evidence-redacted.zip"
            rust = root / "consumer-rust-review/public-rust.zip"
            transfers = root / "r22-packaging/download"
            for path, expected in [(r22, ARCHIVES["r22/evidence-redacted.zip"]), (golinux, ARCHIVES["go-linux-build/evidence-redacted.zip"]), (rust, ARCHIVES["rust/public-rust.zip"])]:
                assert digest(kept(path)) == expected
            go_parts = [root / "go-linux-package" / ("part-" + str(offset) + ".b64") for offset in (0, 128, 256)]
            assert decode(b"".join(p.read_bytes() for p in go_parts)) == kept(golinux)
            for p in go_parts:
                add(p, "Combined Base64 equals retained and independently published Go/Linux archive")
            linux_root = root / "linux-percent-execution"
            for mode in ("control", "candidate"):
                parts = [linux_root / (mode + "-" + str(offset) + ".b64") for offset in range(0, 897, 128)]
                compressed = decode(b"".join(p.read_bytes() for p in parts))
                kept(linux_root / (mode + ".test.gz"), compressed)
                kept(linux_root / (mode + ".test"), gzip.decompress(compressed))
                for p in parts:
                    add(p, "Combined Base64 equals retained gzip and test executable")
            formatted = root / "formatted-transfer.json"
            payload_path = root / "r23-scheduler/candidate-payload.json.gz.b64"
            payload_root = root / "r23-scheduler/candidate"
            formatted_sources = {"linux.go": linux_root / "source/linux.go", "linux_exec_percent_test.go": linux_root / "source/linux_exec_percent_test.go",
                                 "install_task_mutation_failure_test.go": root / "r23-scheduler/formatted-readback/internal/api/install_task_mutation_failure_test.go"}
        else:
            r22root = root / "mcp-r22-completion-20261121"
            r23root = root / "mcp-r23-current-outcome-20261121"
            r22 = r22root / "public-r22/evidence-redacted.zip"
            golinux = r23root / "public-go-linux/evidence-redacted.zip"
            transfers = r22root / "transfer-r22"
            assert digest(kept(r22)) == ARCHIVES["r22/evidence-redacted.zip"]
            assert digest(kept(golinux)) == ARCHIVES["go-linux-build/evidence-redacted.zip"]
            whole = transfers / "archive-whole.b64"
            assert decode(whole.read_bytes()) == kept(r22)
            add(whole, "Base64 equals retained and independently published R22 archive")
            go_transfer = r23root / "transfer-go-linux.b64"
            assert decode(go_transfer.read_bytes()) == kept(golinux)
            add(go_transfer, "Base64 equals retained and independently published Go/Linux archive")
            for mode in ("control", "candidate"):
                directory = r22root / "linux-percent-work" / ("compile-" + mode)
                p = directory / "autostart.test.gz.b64"
                compressed = decode(p.read_bytes())
                kept(directory / "autostart.test.gz", compressed)
                kept(directory / "autostart.test", gzip.decompress(compressed))
                add(p, "Base64 equals retained gzip and original test executable")
            formatted = r23root / "formatted-transfer.json"
            payload_path = r23root / "candidate-payload.json.gz.b64"
            payload_root = r23root / "retained-original-inputs"
            payload_root.mkdir(exist_ok=False)
            formatted_sources = None

        manifest = json.loads(kept(transfers / "transfer-manifest.json"))
        assert len(manifest["chunks"]) == 35
        decoded = []
        for row in manifest["chunks"]:
            p = transfers / row["name"]
            assert p.parent == transfers and p.name.endswith(".b64")
            raw = p.read_bytes()
            assert len(raw) == row["base64_bytes"] and digest(raw) == row["base64_sha256"]
            part = decode(raw)
            assert len(part) == row["decoded_bytes"] and digest(part) == row["decoded_sha256"]
            decoded.append(part)
            add(p, "Manifest-verified chunk; combined bytes equal retained published R22 archive")
        assert b"".join(decoded) == kept(r22)

        data = json.loads(formatted.read_bytes())
        assert set(data) == {"linux.go", "linux_exec_percent_test.go", "install_task_mutation_failure_test.go"}
        with zipfile.ZipFile(golinux) as zipped:
            for name, row in data.items():
                raw = decode(row["base64"].encode())
                assert len(raw) == row["bytes"] and digest(raw) == row["sha256"]
                if formatted_sources is not None:
                    kept(formatted_sources[name], raw)
                candidates = [n for n in zipped.namelist() if n.endswith("/" + name)]
                assert any(zipped.read(n) == raw for n in candidates), name
        add(formatted, "All three decoded formatted sources retained byte-for-byte in verified public archive")
        payload = json.loads(gzip.decompress(decode(payload_path.read_bytes())))
        assert set(payload) == PAYLOAD_NAMES
        for name, text in payload.items():
            path = payload_root / name
            raw = text.encode("utf-8")
            if args.mode == "remote":
                path.parent.mkdir(parents=True, exist_ok=True)
                with path.open("xb") as stream:
                    stream.write(raw)
            kept(path, raw)
        add(payload_path, "All four original pre-format source files preserved exactly outside trash")
        assert len(plan) == (56 if args.mode == "local" else 41)
        receipt["planned_files"] = len(plan)
        receipt["preserved_checks"] = list(preserve.values())
        receipt["status"] = "moving"
        record()
        trash.mkdir(parents=True, exist_ok=False)
        for source, row in plan:
            assert digest(source.read_bytes()) == row["sha256"]
            target = trash / source.relative_to(root)
            target.parent.mkdir(parents=True, exist_ok=True)
            assert not target.exists()
            source.rename(target)
            assert not source.exists() and digest(target.read_bytes()) == row["sha256"]
            receipt["moved"].append({**row, "target": str(target.relative_to(root)), "source_absent": True, "target_hash_verified": True})
            record()
        for path, row in preserve.items():
            assert digest(path.read_bytes()) == row["sha256"]
        receipt["status"] = "complete"
        receipt["preserved_rechecked_after_moves"] = True
    except BaseException as exc:
        receipt["status"] = "failed"
        receipt["error"] = type(exc).__name__ + ": " + str(exc)
        raise
    finally:
        receipt["finished_utc"] = now()
        receipt["runner_sha256"] = digest(Path(__file__).read_bytes())
        record()
    print(json.dumps({"status": receipt["status"], "moved_files": len(receipt["moved"]), "preserved_files_checked": len(preserve), "receipt": str(receipt_path)}))


if __name__ == "__main__":
    main()
