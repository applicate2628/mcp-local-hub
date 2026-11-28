"""Verify the preserved evidence without executing Go or changing any file."""
import hashlib
import json
from pathlib import Path
import tarfile


def main():
    root = Path(__file__).resolve().parent
    manifest = json.loads((root / "probe-evidence-manifest.json").read_text())
    archive = root / manifest["archive"]["name"]
    raw = archive.read_bytes()
    assert len(raw) == manifest["archive"]["bytes"]
    assert hashlib.sha256(raw).hexdigest() == manifest["archive"]["sha256"]
    with tarfile.open(archive) as tar:
        members = tar.getmembers()
        assert all(m.isfile() for m in members)
        assert len(members) == len({m.name for m in members}) == len(manifest["files"])
        data = {m.name: tar.extractfile(m).read() for m in members}
    assert set(data) == {entry["path"] for entry in manifest["files"]}
    for entry in manifest["files"]:
        body = data[entry["path"]]
        assert len(body) == entry["bytes"]
        assert hashlib.sha256(body).hexdigest() == entry["sha256"], entry["path"]
    for phase in ("prepare", "format", "control", "candidate", "build", "vet"):
        prefix = "evidence/" + phase + "/"
        receipt = json.loads(data[prefix + "receipt.json"])
        assert receipt["completed"]
        assert not receipt.get("retain_workspace_descendants_unresolved")
        assert receipt["head_before"] == receipt["head_after"] == manifest["source"]
        runner = "evidence/prepare/qualify_probe.py" if phase == "prepare" else "qualify_probe.py"
        assert hashlib.sha256(data[runner]).hexdigest() == receipt["runner_sha256"]
        for row in receipt["commands"]:
            assert row["exit_code"] == row["expected_exit"] and not row.get("timed_out")
            for stream in ("stdout", "stderr"):
                raw = data[prefix + row["label"] + "." + stream]
                assert hashlib.sha256(raw).hexdigest() == row[stream + "_sha256"]
        if phase in ("candidate", "build", "vet"):
            for path, digest in receipt["inputs_before"].items():
                assert hashlib.sha256(data["candidate/" + path]).hexdigest() == digest
        if phase == "candidate":
            tests = receipt["tests"]
            assert len(tests["pass"]) == 14 and not tests["fail"] and not tests["skip"]
            assert {"TestVersionProbeOutput", "TestVersionProbeCancellation", "TestProbeMissingPath", "PACKAGE:mcp-local-hub/internal/perftools"}.issubset(tests["pass"])
        if phase == "control":
            tests = receipt["tests"]
            expected = {"TestVersionProbeOutput/" + name for name in ("stdout-overflow", "stderr-overflow", "combined-overflow")}
            expected.update(("TestVersionProbeOutput", "PACKAGE:mcp-local-hub/internal/perftools"))
            assert set(tests["fail"]) == expected and len(tests["pass"]) == 4 and not tests["skip"]
            assert data[prefix + "tests.stdout"].count(b"overflow accepted:") == 3
    print("Verified 209 evidence files, exact inputs and all six recorded phase results.")


if __name__ == "__main__":
    main()
