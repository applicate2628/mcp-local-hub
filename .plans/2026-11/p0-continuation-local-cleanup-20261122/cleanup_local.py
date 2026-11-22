"""After Root review: python -B cleanup_local.py agents-released-by-root.

Move only six disposable task directories. Preserve the active base checkout,
published final artifacts, readback, one compressed inventory and one receipt.
"""
import datetime
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import zipfile

ROOT = Path("/workspace/scratch/e51f442c712e")
OUT = ROOT / "continuation-closeout"
DEST = ROOT / "trash" / "local-continuation-20261122"
NAMES = ("architecture-candidate", "architecture-results", "scheduler-inputs",
         "scheduler-stop-candidate", "product-candidate", "context")
ARTIFACTS = ("open-pr-integration-20261117.md", "continuation-scope-audit-20261122.md",
             "p0-continuation-closeout-20261122.json", "p0-continuation-evidence-20261122.zip")
READBACK_SHA = "d8b266bab6fb6069a38f963f5bcaea7c16880ab901db087b6d62c392cab1d0ee"


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def guard(path):
    for ancestor in (path, *path.parents):
        require(not ancestor.is_symlink(), "Symlink in operation path: " + str(ancestor))


def inventory(roots):
    rows = []
    device = ROOT.stat().st_dev
    def visit(path, relative):
        info = path.lstat()
        require(info.st_dev == device, "Different filesystem in inventory: " + str(path))
        row = {"path": relative}
        if stat.S_ISLNK(info.st_mode):
            row.update(type="symlink", target=os.readlink(path))
        elif stat.S_ISREG(info.st_mode):
            digest = hashlib.sha256()
            size = 0
            with path.open("rb") as stream:
                for block in iter(lambda: stream.read(1024 * 1024), b""):
                    digest.update(block)
                    size += len(block)
            require(size == info.st_size, "File changed while inventoried")
            row.update(type="file", bytes=size, sha256=digest.hexdigest())
        elif stat.S_ISDIR(info.st_mode):
            row["type"] = "directory"
        else:
            raise RuntimeError("Special file requires inspection: " + str(path))
        rows.append(row)
        if row["type"] == "directory":
            for child in sorted(path.iterdir(), key=lambda item: item.name):
                visit(child, relative + "/" + child.name)
    for name, path in roots:
        visit(path, name)
    return rows


def processes(label):
    pattern = re.compile("|".join(re.escape(str(ROOT / name)) + r"(?=$|[/\s'\";,)\x00])" for name in NAMES))
    result = {"label": label, "utc": now(), "pids_seen": 0, "unavailable_reads": 0, "matches": [],
              "excluded_pid": os.getpid(), "scope": "Visible /proc cmdline, cwd and fd references; not whole-machine settlement"}
    for process in Path("/proc").iterdir():
        if not process.name.isdigit() or int(process.name) == os.getpid():
            continue
        result["pids_seen"] += 1
        values = []
        for kind in ("cmdline", "cwd"):
            try:
                value = os.fsdecode((process / kind).read_bytes()) if kind == "cmdline" else os.readlink(process / kind)
                values.append((kind, value))
            except OSError:
                result["unavailable_reads"] += 1
        try:
            descriptors = list((process / "fd").iterdir())
        except (OSError, PermissionError):
            result["unavailable_reads"] += 1
            descriptors = []
        for descriptor in descriptors:
            try:
                values.append(("fd", os.readlink(descriptor)))
            except (OSError, PermissionError):
                result["unavailable_reads"] += 1
        for kind, value in values:
            for match in pattern.finditer(value):
                result["matches"].append({"pid": int(process.name), "kind": kind, "root": match.group()})
    return result


def main():
    require(os.name == "posix" and Path(__file__).resolve().parent == OUT, "Unexpected execution root")
    require(sys.argv[1:] == ["agents-released-by-root"], "Root must confirm all six directories are released")
    guard(OUT)
    receipt_path = OUT / "local-cleanup.json"
    inventory_path = OUT / "local-cleanup-inventory.jsonl.gz"
    require(not receipt_path.exists() and not inventory_path.exists(), "Prior cleanup evidence exists; inspect it")
    require(not DEST.exists() and not DEST.is_symlink(), "Trash destination already exists")
    receipt = {"schema": 1, "started_utc": now(), "completed": False, "agents_released_by_root": True,
               "runner_sha256": sha(Path(__file__).read_bytes()), "publication_readback_sha256": READBACK_SHA,
               "sources": [str(ROOT / name) for name in NAMES], "destination": str(DEST), "moves": [],
               "retained_checkout": str(ROOT / "mcphub-base"), "process_observations": [],
               "delete_attempted": False, "kill_requested": False, "trash_purged": False,
               "git_metadata_edited": False, "shared_cache_or_history_changed": False}
    with receipt_path.open("x", encoding="utf-8") as stream:
        json.dump(receipt, stream, indent=2)
    def save():
        receipt_path.write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
    try:
        record_raw = (OUT / "plan-publication-readback.json").read_bytes()
        require(sha(record_raw) == READBACK_SHA, "Final publication readback changed")
        record = json.loads(record_raw)
        require(record["pr"] == 664 and record["readback_complete"] and record["git_object_fetch_verified"]
                and record["parents_count"] == 1 and record["tree"] == record["tree_observed"], "Incomplete final publication proof")
        files = record["files"]
        require(len(files) == 4 and {row["path"] for row in files} == {".plans/2026-11/" + name for name in ARTIFACTS}, "Unexpected publication file set")
        for row in files:
            path = OUT / Path(row["path"]).name
            require(row["local_path"] == str(path) and row["readback_bytes_equal"], "Wrong published artifact location")
            raw = path.read_bytes()
            require(len(raw) == row["bytes"] and sha(raw) == row["sha256"], "Final artifact differs: " + path.name)
            require(hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest() == row["git_blob"], "Published Git blob differs")
        bundle = OUT / ARTIFACTS[3]
        with zipfile.ZipFile(bundle) as archive:
            manifest = json.loads(archive.read("manifest.json"))
            require(len(archive.namelist()) == len(set(archive.namelist())) and set(archive.namelist()) == set(manifest) | {"manifest.json"}, "Unexpected bundle entries")
            for name, row in manifest.items():
                raw = archive.read(name)
                require(len(raw) == row["bytes"] and sha(raw) == row["sha256"], "Bundle member differs: " + name)
        closeout = json.loads((OUT / ARTIFACTS[2]).read_bytes())
        require(closeout["evidence"]["sha256"] == sha(bundle.read_bytes()), "Closeout bundle identity differs")
        published_arch = {row["path"]: row for row in closeout["publications"]["architecture"]["record"]["files"]}
        for relative in ("internal/gui/server.go", "architecture/baseline.yaml"):
            require(sha((ROOT / "architecture-candidate" / relative).read_bytes()) == published_arch[relative]["sha256"], "Linked worktree has an unpublished source change")
        require((ROOT / "mcphub-base").is_dir(), "Retained working checkout is missing")
        for name in NAMES:
            guard(ROOT / name)
            require((ROOT / name).is_dir(), "Source directory is missing: " + name)
        guard(DEST)
        require(all((ROOT / name).stat().st_dev == ROOT.stat().st_dev for name in NAMES), "Cross-volume source")
        receipt.update(published_head=record["head"], published_tree=record["tree"], bundle_payload_hashes_verified=len(manifest), final_artifacts_verified=4)
        observation = processes("before-inventory")
        receipt["process_observations"].append(observation)
        save()
        require(not observation["matches"], "A visible process still references a source directory")
        before = inventory([(name, ROOT / name) for name in NAMES])
        before_raw = encoded(before)
        with gzip.open(inventory_path, "xb") as stream:
            stream.write(encoded({"phase": "before", "entries": before}))
        receipt["inventory_before"] = {"entries": len(before), "sha256": sha(before_raw)}
        save()
        observation = processes("before-move")
        receipt["process_observations"].append(observation)
        save()
        require(not observation["matches"], "A visible process still references a source directory")
        DEST.mkdir(parents=True, exist_ok=False)
        guard(DEST)
        require(DEST.stat().st_dev == ROOT.stat().st_dev, "Cross-volume trash destination")
        for name in NAMES:
            source, target = ROOT / name, DEST / name
            require(not target.exists() and not target.is_symlink(), "Per-directory target already exists")
            source.rename(target)
            receipt["moves"].append({"source": str(source), "target": str(target)})
            save()
        after = inventory([(name, DEST / name) for name in NAMES])
        with gzip.open(inventory_path, "ab") as stream:
            stream.write(encoded({"phase": "after", "entries": after}))
        receipt.update(inventory_after={"entries": len(after), "sha256": sha(encoded(after))}, inventory_equal=before == after,
                       sources_absent=all(not (ROOT / name).exists() and not (ROOT / name).is_symlink() for name in NAMES),
                       retained_checkout_exists=(ROOT / "mcphub-base").is_dir(),
                       files=sum(row["type"] == "file" for row in after), directories=sum(row["type"] == "directory" for row in after),
                       symlinks=sum(row["type"] == "symlink" for row in after), bytes=sum(row.get("bytes", 0) for row in after))
        require(receipt["inventory_equal"] and receipt["sources_absent"] and receipt["retained_checkout_exists"], "Post-move verification differs; retain all evidence")
        receipt["completed"] = True
    except BaseException as error:
        receipt["error"] = type(error).__name__ + ": " + str(error)
        raise
    finally:
        if inventory_path.exists():
            raw = inventory_path.read_bytes()
            receipt["inventory_file"] = {"path": inventory_path.name, "bytes": len(raw), "sha256": sha(raw)}
        receipt["finished_utc"] = now()
        save()
        print(json.dumps({key: receipt.get(key) for key in ("completed", "error", "files", "directories", "symlinks", "bytes", "inventory_equal", "sources_absent", "retained_checkout_exists")}), flush=True)


if __name__ == "__main__":
    main()
