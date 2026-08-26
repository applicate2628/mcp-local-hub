from __future__ import annotations

import importlib
import inspect
from pathlib import Path

import pytest

PACKAGE_ROOT = Path(__file__).resolve().parents[1]
REPOSITORY_ROOT = PACKAGE_ROOT.parents[1]


def test_w00_red_package_owned_native_runtime_is_materialized() -> None:
    runtime_root = PACKAGE_ROOT / "native" / "cst-runtime"
    runtime_image = runtime_root / "mcphub-cst-runtime.exe"

    assert runtime_root.is_dir(), "W00 gap: package-owned native/cst-runtime subtree is absent"
    assert runtime_image.is_file(), "W00 gap: package-owned mcphub-cst-runtime.exe is absent"
    assert runtime_image.stat().st_size > 0, "W00 gap: native runtime image is empty"


@pytest.mark.parametrize(
    ("module_name", "entrypoint"),
    [
        ("mcphub_em_mcp.cst_saved_field_daemon_service_windows", "main"),
        ("mcphub_em_mcp.cst_saved_field_broker_service_windows", "main"),
        ("mcphub_em_mcp.cst_saved_field_broker_worker", "main"),
    ],
)
def test_w00_red_production_roots_do_not_require_python_object_injection(
    module_name: str, entrypoint: str
) -> None:
    root = getattr(importlib.import_module(module_name), entrypoint)

    assert not inspect.signature(root).parameters, (
        f"W00 gap: {module_name}.{entrypoint} still exposes injected Python composition "
        "instead of a fixed package-owned production root"
    )


@pytest.mark.parametrize(
    ("module_name", "receipt_name"),
    [
        ("mcphub_em_mcp.cst_saved_field_broker_client_windows", "BrokerExchangeReceiptV1"),
        ("mcphub_em_mcp.cst_saved_field_broker_worker_protocol", "WorkerPreMainReceiptV1"),
        ("mcphub_em_mcp.cst_saved_field_broker_worker_protocol", "WorkerCapabilityReceiptV1"),
        ("mcphub_em_mcp.cst_saved_field_containment_windows", "BrokerCapabilityReceiptV1"),
    ],
)
def test_w00_red_cross_process_receipt_owner_types_exist(module_name: str, receipt_name: str) -> None:
    module = importlib.import_module(module_name)

    assert hasattr(module, receipt_name), (
        f"W00 gap: {module_name} has no authoritative owner-local {receipt_name}"
    )


def test_w00_red_go_launch_owner_requires_exact_direct_image_receipt() -> None:
    launch_sources = [
        REPOSITORY_ROOT / "internal" / "daemon" / "host.go",
        REPOSITORY_ROOT / "internal" / "daemon" / "launch_capability.go",
        REPOSITORY_ROOT / "internal" / "daemon" / "launch_capability_windows.go",
        REPOSITORY_ROOT / "internal" / "cli" / "daemon.go",
    ]
    combined = "\n".join(path.read_text(encoding="utf-8") for path in launch_sources)

    assert "cst-direct-v1" in combined, (
        "W00 gap: the existing Go spawn owner has no exact cst-direct-v1 image receipt"
    )
