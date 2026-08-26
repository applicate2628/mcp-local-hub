from __future__ import annotations

import json
from pathlib import Path

import pytest

PACKAGE_ROOT = Path(__file__).resolve().parents[1]
RUNTIME_ROOT = PACKAGE_ROOT / "native" / "cst-runtime"
RUNTIME_IMAGE = RUNTIME_ROOT / "mcphub-cst-runtime.exe"
RUNTIME_MANIFEST = RUNTIME_ROOT / "cst-native-runtime-manifest-v1.json"
PE_VERIFIER = RUNTIME_ROOT / "verify_cst_native_pe.py"


def _require_native_contract_files() -> None:
    missing = [
        path.relative_to(PACKAGE_ROOT).as_posix()
        for path in (RUNTIME_IMAGE, RUNTIME_MANIFEST, PE_VERIFIER)
        if not path.is_file()
    ]
    assert not missing, f"W01 gap: package-owned native contract files absent: {missing}"


def _manifest() -> dict[str, object]:
    _require_native_contract_files()
    value = json.loads(RUNTIME_MANIFEST.read_text(encoding="utf-8"))
    assert isinstance(value, dict), "W01 gap: native runtime manifest is not an object"
    return value


def test_w01_red_independent_pe_verifier_admits_exact_pre_entry_image() -> None:
    _require_native_contract_files()
    verifier_source = PE_VERIFIER.read_text(encoding="utf-8")
    for contract in (
        "AMD64",
        "KERNEL32.dll",
        "delay_import",
        "tls",
        "clr",
        "bound_import",
        "mcphub_cst_entry",
        "CETCOMPAT",
        "HIGHENTROPYVA",
        "NXCOMPAT",
    ):
        assert contract.casefold() in verifier_source.casefold(), (
            f"W01 gap: independent PE verifier omits {contract}"
        )


def test_w01_red_native_manifest_binds_deterministic_image_and_disassembly() -> None:
    manifest = _manifest()
    assert manifest.get("schema") == "mcphub.cst.native-runtime-manifest.v1"
    assert manifest.get("unsigned_builds_byte_identical") is True
    assert manifest.get("direct_imports") == ["KERNEL32.dll"]
    assert manifest.get("entry_symbol") == "mcphub_cst_entry"
    assert manifest.get("pre_revocation_disassembly_sha256")
    assert manifest.get("runtime_image_sha256")


@pytest.mark.parametrize(
    ("role", "handles"),
    [
        ("frontend", ["stdin", "stdout", "stderr", "capability-read"]),
        ("worker", ["stdin", "stdout", "stderr", "source-root", "workspace-root"]),
    ],
)
def test_w01_red_native_manifest_declares_exact_role_handle_tuple(role: str, handles: list[str]) -> None:
    manifest = _manifest()
    roles = manifest.get("roles")
    assert isinstance(roles, dict)
    assert roles.get(role) == {
        "inherited_handles": handles,
        "revoked_before_package_code": True,
    }
