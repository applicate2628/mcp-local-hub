"""Verify recorded evidence and candidate bytes without executing archived code."""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path, PurePosixPath
import zipfile

ARCHIVE_SHA256 = "b58362201753892911c53c115ef7c5e329c1f15f5ef9bdb4e8f9b92f7fd6652c"
SOURCE = "610fb30348c10be7b645b6ce43c862397b190f60"
TREE = "bf5d77bdc359174290600dad5bf98c28309f4870"
SKIP = "TestExecuteInstallToRollbackBackendCompatibility/unsupported_platform"
PHASES = ("source-prepare", "prune-admit", "prune-control", "prune-candidate",
          "prune-adjacent", "prune-build-vet")
EXPECTED_TESTS = {
    "prune-control": (0, 18, 0, 6, 13, 1),
    "prune-candidate": (18, 0, 0, 6, 13, 0),
    "prune-adjacent": (106, 0, 1, 31, 91, 0),
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def verify(archive, repository):
    require(sha(archive.read_bytes()) == ARCHIVE_SHA256, "archive SHA-256 differs")
    with zipfile.ZipFile(archive) as bundle:
        names = bundle.namelist()
        require(len(names) == len(set(names)), "duplicate archive entries")
        require(bundle.testzip() is None, "ZIP integrity failure")
        manifest = json.loads(bundle.read("manifest.json"))
        declared = [row["path"] for row in manifest["files"]]
        require(len(declared) == len(set(declared)), "duplicate manifest entries")
        require(set(names) == set(declared) | {"manifest.json"}, "archive file set differs")
        for row in manifest["files"]:
            path = PurePosixPath(row["path"])
            require(not path.is_absolute() and ".." not in path.parts, "unsafe archive path")
            data = bundle.read(row["path"])
            require(len(data) == row["bytes"] and sha(data) == row["sha256"], row["path"])

        scope_raw = bundle.read("inputs/scopes/prune/manifest.json")
        scope = json.loads(scope_raw)
        summary = json.loads(bundle.read("summary.json"))
        require((summary["source"], summary["tree"]) == (SOURCE, TREE), "summary source differs")
        require(summary["full_default_test"] == summary["full_tagged_test"] == "NOT_RUN",
                "full test scope changed")
        require(summary["native_scheduler_operations"] is False, "native scope changed")
        require(summary["candidate_files"] == scope["files"], "candidate manifest differs")
        wrapper_sha = sha(bundle.read("inputs/qualify_scope.py"))
        owner_sha = sha(bundle.read("inputs/capture-owner.py"))
        observed_test_names = {}

        for phase in PHASES:
            receipt = json.loads(bundle.read(phase + "/receipt.json"))
            require((receipt["base"], receipt["base_tree"]) == (SOURCE, TREE), phase + " source")
            final = receipt["source_final"]
            require((final["head"], final["tree"]) == (SOURCE, TREE)
                    and not final["status"] and not final["staged"], phase + " dirty source")
            require(receipt["scope_wrapper_sha256"] == wrapper_sha, phase + " wrapper")
            require(receipt["reused_capture_owner_sha256"] == owner_sha, phase + " capture owner")
            if phase == "prune-adjacent":
                require(receipt["state"] == "failed" and receipt["completed"] is False
                        and receipt["error"] == "Qualification selected an unexpected skipped test: prune-adjacent",
                        "adjacent failure was lost")
            else:
                require(receipt["state"] == "complete" and receipt["completed"] is True,
                        phase + " incomplete")
            if phase.startswith("prune-"):
                require(receipt["scope_manifest_sha256"] == sha(scope_raw), phase + " manifest")
            require(sha(bundle.read(phase + "/overlay.json")) == receipt["overlay_sha256"],
                    phase + " overlay")
            for command in receipt["commands"]:
                label = command["label"]
                require(command["settled"] and not command["timed_out"], label + " direct process")
                streams = {}
                for stream in ("stdout", "stderr"):
                    data = bundle.read(phase + "/" + label + "." + stream + ".log")
                    require(len(data) == command[stream]["bytes"]
                            and sha(data) == command[stream]["sha256"], label + " " + stream)
                    streams[stream] = data
                require(command["exit_code"] == (1 if phase == "prune-control" else 0),
                        label + " exit")
                if phase not in EXPECTED_TESTS:
                    continue
                events = [json.loads(line) for line in streams["stdout"].splitlines()]
                terminal = {action: [event.get("Test", "PACKAGE:" + event.get("Package", ""))
                                     for event in events if event.get("Action") == action]
                            for action in ("pass", "fail", "skip")}
                require(terminal == command["tests"], label + " recorded events")
                runs = Counter(e["Test"] for e in events if e.get("Action") == "run")
                ended = Counter(name for group in terminal.values() for name in group
                                if not name.startswith("PACKAGE:"))
                require(runs == ended and all(count == 1 for count in runs.values()),
                        label + " missing or repeated terminal events")
                counts = tuple(sum(not name.startswith("PACKAGE:") for name in terminal[action])
                               for action in ("pass", "fail", "skip"))
                roots = sum("/" not in name for name in ended)
                leaves = sum(not any(other.startswith(name + "/") for other in ended) for name in ended)
                require(counts + (roots, leaves, command["exit_code"]) == EXPECTED_TESTS[phase],
                        label + " counts")
                packages = [(action, name) for action, group in terminal.items() for name in group
                            if name.startswith("PACKAGE:")]
                require(packages == [("fail" if phase == "prune-control" else "pass",
                                      "PACKAGE:mcp-local-hub/internal/api")], label + " package")
                require(terminal["skip"] == ([SKIP] if phase == "prune-adjacent" else []),
                        label + " skip")
                observed_test_names[phase] = set(ended)
        require(observed_test_names["prune-control"] == observed_test_names["prune-candidate"],
                "control and candidate selected different tests")

        for row in scope["files"]:
            data = (repository / row["path"]).read_bytes()
            git_blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
            require(sha(data) == row["candidate_sha256"] and git_blob == row["candidate_git_blob"],
                    "candidate differs: " + row["path"])
    print("Evidence bytes and four candidate files verified; adjacent phase remains FAILED (one platform SKIP).")
    print("Full default/tagged tests and native scheduler qualification remain NOT_RUN.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, default=Path(__file__).with_name("evidence.zip"))
    parser.add_argument("--repository", type=Path, default=Path(__file__).resolve().parents[4])
    args = parser.parse_args()
    verify(args.archive, args.repository)
