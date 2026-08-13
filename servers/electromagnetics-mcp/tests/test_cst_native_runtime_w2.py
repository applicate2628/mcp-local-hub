from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import struct
import subprocess
import sys
from pathlib import Path

import pytest

PACKAGE_ROOT = Path(__file__).resolve().parents[1]
RUNTIME_ROOT = PACKAGE_ROOT / "native" / "cst-runtime"
IMAGE = RUNTIME_ROOT / "mcphub-cst-runtime.exe"
MANIFEST = RUNTIME_ROOT / "cst-native-runtime-manifest-v1.json"
VERIFIER = RUNTIME_ROOT / "verify_cst_native_pe.py"
CLOSURE_BUILDER = RUNTIME_ROOT / "build_package_load_closure.py"


def _verifier_module():
    spec = importlib.util.spec_from_file_location("cst_native_pe_verifier", VERIFIER)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _closure_module():
    spec = importlib.util.spec_from_file_location("cst_package_closure", CLOSURE_BUILDER)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _directory_offset(image: bytearray, index: int) -> int:
    pe = struct.unpack_from("<I", image, 0x3C)[0]
    optional = pe + 24
    assert struct.unpack_from("<H", image, optional)[0] == 0x20B
    return optional + 112 + index * 8


def test_w02_pe_loader_closure_independent_facts_match_manifest() -> None:
    module = _verifier_module()
    facts = module.inspect_image(IMAGE)
    manifest = json.loads(MANIFEST.read_text(encoding="utf-8"))
    assert facts == manifest["pe"]
    assert facts["machine"] == "AMD64"
    assert facts["entry_symbol"] == "mcphub_cst_entry"
    assert facts["direct_imports"] == ["KERNEL32.dll"]
    assert facts["delay_import"] is facts["tls"] is facts["clr"] is facts["bound_import"] is False
    assert facts["dependent_load_flags"] == 0x800
    assert facts["relocations"] is True
    assert facts["mitigations"] == ["CETCOMPAT", "HIGHENTROPYVA", "DYNAMICBASE", "NXCOMPAT"]


@pytest.mark.parametrize("mutation", ["machine", "kernel32", "tls", "relocations", "cet"])
def test_w02_pe_loader_closure_self_falsifies_mutations(tmp_path: Path, mutation: str) -> None:
    image = bytearray(IMAGE.read_bytes())
    pe = struct.unpack_from("<I", image, 0x3C)[0]
    if mutation == "machine":
        struct.pack_into("<H", image, pe + 4, 0x14C)
    elif mutation == "kernel32":
        marker = image.index(b"KERNEL32.dll")
        image[marker] = ord("X")
    elif mutation == "tls":
        struct.pack_into("<II", image, _directory_offset(image, 9), 0x1000, 40)
    elif mutation == "relocations":
        struct.pack_into("<II", image, _directory_offset(image, 5), 0, 0)
    else:
        debug_rva, debug_size = struct.unpack_from("<II", image, _directory_offset(image, 6))
        module = _verifier_module()
        original = module.inspect_image
        assert original(IMAGE)["mitigations"][0] == "CETCOMPAT"
        # Locate the type-20 debug record and clear its extended characteristics payload.
        optional = pe + 24
        section_count = struct.unpack_from("<H", image, pe + 6)[0]
        section_table = optional + struct.unpack_from("<H", image, pe + 20)[0]
        debug_offset = None
        for index in range(section_count):
            off = section_table + index * 40
            vsize, rva, raw_size, raw = struct.unpack_from("<IIII", image, off + 8)
            if rva <= debug_rva < rva + max(vsize, raw_size):
                debug_offset = raw + debug_rva - rva
                break
        assert debug_offset is not None
        for item in range(debug_size // 28):
            off = debug_offset + item * 28
            if struct.unpack_from("<I", image, off + 12)[0] == 20:
                payload = struct.unpack_from("<I", image, off + 24)[0]
                struct.pack_into("<I", image, payload, 0)
                break
        else:
            raise AssertionError("CET debug record missing")
    candidate = tmp_path / "mutant.exe"
    candidate.write_bytes(image)
    result = subprocess.run(
        [sys.executable, str(VERIFIER), str(candidate)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 78
    assert result.stdout.startswith("native_loader_invalid:")


def test_w02_deterministic_manifest_is_publication_safe_and_default_off() -> None:
    manifest = json.loads(MANIFEST.read_text(encoding="utf-8"))
    assert manifest["unsigned_builds_byte_identical"] is True
    assert manifest["package_load"] == {
        "state": "default-off",
        "required_receipt": "ProvisionedPackageIdentityV1",
        "failure_id": "native_loader_invalid",
    }
    assert manifest["signed_structure"] == {"status": "target-unfulfilled", "phase": "X1/X2"}
    assert manifest["package_load_closure"]["state"] == "unprovisioned"
    assert manifest["package_load_closure"]["ordered_package_rows"] == []
    assert manifest["package_load_closure"]["target_os_rows"] == []
    expected_inputs = {
        "source_sha256": hashlib.sha256((RUNTIME_ROOT / "mcphub_cst_runtime.c").read_bytes()).hexdigest(),
        "build_script_sha256": hashlib.sha256((RUNTIME_ROOT / "build.ps1").read_bytes()).hexdigest(),
        "verifier_sha256": hashlib.sha256(VERIFIER.read_bytes()).hexdigest(),
        "closure_builder_sha256": hashlib.sha256(CLOSURE_BUILDER.read_bytes()).hexdigest(),
    }
    assert manifest["input_hashes"] == expected_inputs
    serialized = MANIFEST.read_text(encoding="utf-8")
    assert str(PACKAGE_ROOT.parent.parent) not in serialized
    assert "<MSVC>" in serialized and "<WindowsSDK>" in serialized


def test_w02_package_closure_traversal_is_deterministic_and_fail_closed(tmp_path: Path) -> None:
    module = _closure_module()
    package = tmp_path / "package"
    package.mkdir()
    (package / IMAGE.name).write_bytes(IMAGE.read_bytes())
    system = Path(os.environ["SYSTEMROOT"]) / "System32" / "kernel32.dll"
    first = module.build_closure(package, (IMAGE.name,), {"KERNEL32.dll": system})
    second = module.build_closure(package, (IMAGE.name,), {"kernel32.DLL": system})
    assert first == second
    assert [row["name"] for row in first["package_rows"]] == [IMAGE.name]
    assert [row["name"].casefold() for row in first["system32_rows"]] == ["kernel32.dll"]
    with pytest.raises(module.ClosureError, match="unresolved dependency"):
        module.build_closure(package, (IMAGE.name,), {})


def test_w02_package_closure_rejects_ambiguous_package_basename(tmp_path: Path) -> None:
    module = _closure_module()
    package = tmp_path / "package"
    (package / "one").mkdir(parents=True)
    (package / "two").mkdir()
    (package / "one" / "kernel32.dll").write_bytes(IMAGE.read_bytes())
    (package / "two" / "KERNEL32.DLL").write_bytes(IMAGE.read_bytes())
    (package / IMAGE.name).write_bytes(IMAGE.read_bytes())
    with pytest.raises(module.ClosureError, match="ambiguous package dependency"):
        module.build_closure(package, (IMAGE.name,), {})


@pytest.mark.skipif(sys.platform != "win32", reason="native Windows entry contract")
@pytest.mark.parametrize(
    ("role", "environment_names"),
    [
        ("frontend", ("MCPHUB_CST_LAUNCH_HANDLE",)),
        ("worker", ("MCPHUB_CST_SOURCE_HANDLE", "MCPHUB_CST_WORKSPACE_HANDLE")),
    ],
)
def test_w02_postrevocation_absent_package_receipt_fails_closed(
    role: str, environment_names: tuple[str, ...]
) -> None:
    import msvcrt

    descriptors: list[int] = []
    environment = os.environ.copy()
    try:
        for name in environment_names:
            read_fd, write_fd = os.pipe()
            descriptors.extend((read_fd, write_fd))
            os.set_inheritable(read_fd, True)
            environment[name] = str(msvcrt.get_osfhandle(read_fd))
        result = subprocess.run(
            [str(IMAGE), f"--role={role}"],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            env=environment,
            close_fds=False,
            timeout=5,
            check=False,
        )
        assert result.returncode == 78
        assert result.stdout == b""
        if role == "worker":
            from io import BytesIO

            from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
                decode_startup_proof_frame,
            )

            assert decode_startup_proof_frame(BytesIO(result.stderr)).complete is True
        else:
            assert result.stderr == b""
    finally:
        for descriptor in descriptors:
            os.close(descriptor)
