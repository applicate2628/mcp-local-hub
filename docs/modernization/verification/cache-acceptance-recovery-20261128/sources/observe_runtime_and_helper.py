"""Recheck preserved runtime bytes and observe the exact canonical helper timeout."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

def sha(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()

def save(path, value):
    with Path(path).open("x", encoding="utf-8", newline="\n") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())

def verify_runtime(root, manifest, output):
    expected = json.loads(Path(manifest).read_bytes())
    base = Path(root)
    actual_files, actual_dirs = {}, []
    for current, dirs, files in os.walk(base, followlinks=False):
        for name in dirs:
            path = Path(current) / name
            if path.is_symlink() or path.is_junction():
                raise RuntimeError("runtime directory contains a reparse point")
            actual_dirs.append(path.relative_to(base).as_posix())
        for name in files:
            path = Path(current) / name
            if path.is_symlink() or path.is_junction() or not path.is_file():
                raise RuntimeError("runtime file is not ordinary")
            actual_files[path.relative_to(base).as_posix()] = {
                "bytes": path.stat().st_size, "sha256": sha(path)}
    matches = sorted(actual_dirs) == sorted(expected["directories"]) and actual_files == expected["files"]
    record = {"schema": "preserved-runtime-current-readback-v1",
              "status": "PASS" if matches else "FAIL",
              "manifest_sha256": sha(manifest), "files": len(actual_files),
              "directories": len(actual_dirs), "bytes": sum(v["bytes"] for v in actual_files.values()),
              "complete_manifest_equal": matches, "runtime_root": str(base),
              "pwsh_sha256": sha(base / "pwsh.exe")}
    save(output, record)
    print(json.dumps(record))
    if not matches:
        raise RuntimeError("preserved runtime differs from the closed copy inventory")

def observe_helper(binary, expected_pin, output_root):
    base = Path(output_root)
    base.mkdir()
    if sha(binary) != expected_pin:
        raise RuntimeError("original API test image differs")
    canonical = base / "absent-canonical" / "mcphub.exe"
    paths = [Path(binary), base / "npm-package" / "node_modules" / "platform" / "api.test.exe",
             base / "direct-product" / "api.test.exe"]
    observations = []
    for index, path in enumerate(paths):
        if index:
            path.parent.mkdir(parents=True)
            with Path(binary).open("rb") as source, path.open("xb") as target:
                shutil.copyfileobj(source, target)
            if sha(path) != expected_pin:
                raise RuntimeError("copied image differs")
        env = os.environ.copy()
        env["MCPHUB_TEST_CANONICAL_MANIFEST_DIR_HELPER"] = str(canonical)
        argv = [str(path), "-test.run=^TestCanonicalManifestDirHelperProcess$"]
        started = time.monotonic()
        deadline = started + 10.0
        startup = subprocess.STARTUPINFO()
        startup.dwFlags = subprocess.STARTF_USESHOWWINDOW
        startup.wShowWindow = subprocess.SW_HIDE
        original = subprocess.Popen(argv, env=env, stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    startupinfo=startup, creationflags=subprocess.CREATE_NO_WINDOW)
        created_after = time.monotonic() - started
        timed_out = False
        try:
            out, err = original.communicate(timeout=max(0.0, deadline - time.monotonic()))
        except subprocess.TimeoutExpired:
            timed_out = True
            original.kill()
            out, err = original.communicate()
        elapsed = time.monotonic() - started
        row = {"context": ["original", "npm-layout", "direct-layout"][index],
               "binary_sha256": expected_pin, "creation_seconds": created_after,
               "total_seconds": elapsed, "deadline_seconds": 10.0,
               "deadline_exceeded": timed_out, "original_exit_code": original.returncode,
               "stdout": out.decode("utf-8", "replace"), "stderr": err.decode("utf-8", "replace"),
               "original_reaped": original.returncode is not None,
               "stdout_closed": original.stdout.closed, "stderr_closed": original.stderr.closed,
               "canonical_parent_created": canonical.parent.exists()}
        observations.append(row)
    record = {"schema": "canonical-helper-existing-deadline-observation-v1",
              "observations": observations, "product_claim": False,
              "source_changed": False}
    save(base / "observations.json", record)
    print(json.dumps(record))
    if any(v["deadline_exceeded"] or v["original_exit_code"] != 0 or v["canonical_parent_created"]
           for v in observations):
        raise SystemExit(1)

if __name__ == "__main__":
    if sys.argv[1] == "runtime":
        verify_runtime(*sys.argv[2:])
    elif sys.argv[1] == "helper":
        observe_helper(*sys.argv[2:])
    else:
        raise SystemExit("unknown observation")
