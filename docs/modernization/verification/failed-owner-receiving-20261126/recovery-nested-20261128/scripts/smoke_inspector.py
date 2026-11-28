#!/usr/bin/env python3
"""Focused local checks; retained fixtures are data only, never executed."""
import base64
import contextlib
import gzip
import hashlib
import io
import json
from pathlib import Path
import sys
import zipfile

sys.dont_write_bytecode = True
from inspect_nested import main


def sha(data):
    return hashlib.sha256(data).hexdigest()


def zipped(name, data):
    output = io.BytesIO()
    with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        item = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
        item.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(item, data)
    return output.getvalue()


def run(folder):
    folder.mkdir(exist_ok=False)
    target = b"exact historical receiver smoke fixture\r\n"
    encoded = base64.b64encode(gzip.compress(target, mtime=0)).decode("ascii")
    inner = zipped("payload.json", json.dumps({"content_base64": encoded}).encode())
    outer = zipped("inner.zip", inner)
    oversized = zipped("large.bin", b"x" * 1025)
    oversized_outer = zipped("inner.zip", oversized)
    corrupt = b"this is not a ZIP archive\n"
    fixtures = {"nested.zip": outer, "oversize.zip": oversized_outer, "corrupt.zip": corrupt}
    for name, content in fixtures.items():
        (folder / name).write_bytes(content)

    def execute(name, members, extra=None, limit=1024):
        content = fixtures[name]
        declared = {"path": "{FIXTURE_ROOT}/" + name, "bytes": len(content),
                    "sha256": sha(content), "members": members}
        declared.update(extra or {})
        spec = {"targets": {"receiver": sha(target)}, "roots": {"{FIXTURE_ROOT}": str(folder)},
                "inputs": [declared], "seen_archive_sha256": [sha(corrupt)],
                "limits": {"member_bytes": limit}}
        spec_path = folder / (name + ".spec.json")
        spec_path.write_text(json.dumps(spec), encoding="utf-8")
        output = folder / (name + ".out")
        with contextlib.redirect_stdout(io.StringIO()):
            assert main([str(spec_path), str(output)]) == 0
        report = json.loads((output / "report.json").read_text(encoding="utf-8"))
        return report, output, spec_path

    report, output, spec = execute("nested.zip", [{"name": "inner.zip", "bytes": len(inner), "sha256": sha(inner)}])
    hits = [hit for hit in report["hits"] if hit["targets"]]
    assert report["complete_for_declared_scope"], report
    assert len(hits) == 1 and (output / hits[0]["artifact"]).read_bytes() == target
    second = folder / "deterministic-repeat.out"
    with contextlib.redirect_stdout(io.StringIO()):
        main([str(spec), str(second)])
    assert (output / "report.json").read_bytes() == (second / "report.json").read_bytes()
    try:
        main([str(spec), str(output)])
        raise AssertionError("existing output directory was accepted")
    except FileExistsError:
        pass

    report, _, _ = execute("oversize.zip", [{"name": "inner.zip"}])
    assert not report["complete_for_declared_scope"] and not report["hits"]
    assert any("member_bytes" in row["reason"] for row in report["limitations"])
    assert not any(row["source"].endswith("large.bin") for row in report["readset"])
    report, _, _ = execute("corrupt.zip", [], {"inspect_root": True})
    assert not report["complete_for_declared_scope"]
    assert any("BadZipFile" in row["reason"] for row in report["errors"])
    assert report["archives"][0]["prior_deduplication_override"] == "explicit inspect_root"
    print(json.dumps({"status": "PASS", "checks": ["nested ZIP / JSON / base64 / GZIP exact bytes",
          "recursive member size skip", "corrupt ZIP and explicit-root dedup override",
          "deterministic reports", "exclusive output directory"]}, sort_keys=True))


if __name__ == "__main__":
    run(Path(sys.argv[1]).absolute())
