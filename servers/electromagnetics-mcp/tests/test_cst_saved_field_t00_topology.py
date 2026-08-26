from __future__ import annotations

import ast
from pathlib import Path

REPO = Path(__file__).parents[3]
PACKAGE = Path(__file__).parents[1] / "src" / "mcphub_em_mcp"
GO_API = REPO / "internal" / "api"

EXPECTED_ENDPOINTS = {
    r"\\.\pipe\mcp-local-hub-cst-saved-field-enrollment-v1",
    r"\\.\pipe\mcp-local-hub-cst-saved-field-frontend-v1",
    r"\\.\pipe\mcp-local-hub-cst-saved-field-v1",
}
PROTOCOL_OWNERS = {
    "cst_saved_field_hub_enrollment_windows.py": "HubEnrollmentProtocolV1",
    "cst_saved_field_frontend_protocol.py": "FrontendDaemonProtocolV1",
    "cst_saved_field_broker_protocol.py": "BrokerProtocolV1",
    "cst_saved_field_broker_worker_protocol.py": "BrokerWorkerProtocolV1",
}


def _imports(path: Path) -> set[str]:
    tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    result: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.module:
            result.add(node.module.rsplit(".", 1)[-1])
        elif isinstance(node, ast.Import):
            result.update(alias.name.rsplit(".", 1)[-1] for alias in node.names)
    return result


def _endpoint_owners() -> dict[str, list[str]]:
    owners: dict[str, list[str]] = {}
    prefix = r"\\.\pipe\mcp-local-hub-cst-saved-field-"
    for path in sorted(PACKAGE.glob("*.py")):
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if (
                isinstance(node, ast.Constant)
                and isinstance(node.value, str)
                and node.value.startswith(prefix)
            ):
                owners.setdefault(node.value, []).append(path.name)
    return owners


def _require_imports(errors: list[str], owner: str, required: set[str], forbidden: set[str]) -> None:
    path = PACKAGE / owner
    if not path.is_file():
        errors.append(f"missing-owner:{owner}")
        return
    imported = _imports(path)
    missing = sorted(required - imported)
    extra = sorted(forbidden & imported)
    if missing:
        errors.append(f"missing-edge:{owner}:{','.join(missing)}")
    if extra:
        errors.append(f"forbidden-edge:{owner}:{','.join(extra)}")


def test_t00_accepted_three_endpoint_four_schema_topology() -> None:
    errors: list[str] = []

    endpoint_owners = _endpoint_owners()
    actual_endpoints = set(endpoint_owners)
    if actual_endpoints != EXPECTED_ENDPOINTS:
        errors.append(
            "endpoint-set:"
            f"missing={sorted(EXPECTED_ENDPOINTS - actual_endpoints)}:"
            f"extra={sorted(actual_endpoints - EXPECTED_ENDPOINTS)}"
        )
    duplicated = {endpoint: owners for endpoint, owners in endpoint_owners.items() if len(owners) != 1}
    if duplicated:
        errors.append(f"endpoint-owner-count:{duplicated}")

    for owner, protocol in PROTOCOL_OWNERS.items():
        path = PACKAGE / owner
        if not path.is_file():
            errors.append(f"missing-schema-owner:{owner}:{protocol}")
        elif protocol not in path.read_text(encoding="utf-8"):
            errors.append(f"missing-schema-token:{owner}:{protocol}")

    supervisor_address = GO_API / "supervisor_ipc_address_windows.go"
    address_source = supervisor_address.read_text(encoding="utf-8")
    if address_source.count("func SupervisorIPCAddress(") != 1 or "mcphub-supervisor-" not in address_source:
        errors.append("supervisor-status-endpoint-owner")
    status_owners = [*sorted(GO_API.glob("supervisor_ipc*.go")), GO_API / "supervisor_cst_identity.go"]
    status_sources = "\n".join(path.read_text(encoding="utf-8") for path in status_owners)
    if "GET_CURRENT_CST_TASK_IDENTITY_V1" not in status_sources:
        errors.append("supervisor-status-cst-identity-opcode")

    for go_owner in (
        REPO / "internal" / "daemon" / "launch_capability_windows.go",
        GO_API / "hub_enrollment_client_windows.go",
    ):
        if not go_owner.is_file():
            errors.append(f"missing-owner:{go_owner.relative_to(REPO).as_posix()}")

    _require_imports(
        errors,
        "cst.py",
        {"cst_saved_field_daemon_client_windows"},
        {
            "cst_saved_field_broker_client_windows",
            "cst_saved_field_broker_protocol",
            "cst_saved_field_broker_worker",
            "cst_saved_field_containment_windows",
            "cst_saved_field_vendor",
        },
    )
    _require_imports(
        errors,
        "cst_saved_field_daemon_service_windows.py",
        {
            "cst_saved_field_hub_enrollment_windows",
            "cst_saved_field_frontend_protocol",
            "cst_saved_field_broker_client_windows",
        },
        {
            "cst_saved_field_broker_worker",
            "cst_saved_field_containment_windows",
            "cst_saved_field_transfer",
            "cst_saved_field_vendor",
        },
    )
    _require_imports(
        errors,
        "cst_saved_field_broker_service_windows.py",
        {
            "cst_saved_field_broker_protocol",
            "cst_saved_field_broker_worker_protocol",
            "cst_saved_field_containment_windows",
            "cst_saved_field_vendor_isolation_windows",
        },
        {"cst_saved_field_frontend_protocol"},
    )

    assert not errors, "T00-TOPOLOGY-RED:" + "|".join(errors)
