from __future__ import annotations

import importlib
import inspect
from dataclasses import fields, is_dataclass

import pytest


def _owner_type(module_name: str, type_name: str):
    module = importlib.import_module(module_name)
    value = getattr(module, type_name, None)
    assert value is not None, f"W01 gap: {module_name} does not own {type_name}"
    return value


@pytest.mark.parametrize(
    "module_name",
    [
        "mcphub_em_mcp.cst_saved_field_daemon_service_windows",
        "mcphub_em_mcp.cst_saved_field_broker_service_windows",
        "mcphub_em_mcp.cst_saved_field_broker_worker",
    ],
)
def test_w01_red_production_entrypoints_are_fixed_non_injected_roots(module_name: str) -> None:
    root = importlib.import_module(module_name).main
    assert inspect.signature(root).parameters == {}, (
        f"W01 gap: {module_name}.main still accepts injected Python composition"
    )


@pytest.mark.parametrize(
    ("module_name", "owner_name"),
    [
        ("mcphub_em_mcp.cst_saved_field_daemon_service_windows", "WindowsNamedPipeDaemonTransport"),
        ("mcphub_em_mcp.cst_saved_field_broker_client_windows", "WindowsNamedPipeBrokerTransport"),
        ("mcphub_em_mcp.cst_saved_field_containment_windows", "WindowsContainedInvocation"),
    ],
)
def test_w01_red_production_roots_materialize_real_local_transport_owners(
    module_name: str, owner_name: str
) -> None:
    _owner_type(module_name, owner_name)


@pytest.mark.parametrize(
    ("module_name", "type_name", "required_fields"),
    [
        (
            "mcphub_em_mcp.cst_saved_field_broker_worker_protocol",
            "WorkerPreMainBootstrapV1",
            {
                "source_root_locator",
                "workspace_root_locator",
                "source_root_identity",
                "workspace_root_identity",
            },
        ),
        (
            "mcphub_em_mcp.cst_saved_field_broker_worker_protocol",
            "WorkerPreMainReceiptV1",
            {"inherited_handle_roles", "inherit_flags_cleared", "capability_identities_verified"},
        ),
        (
            "mcphub_em_mcp.cst_saved_field_broker_client_windows",
            "BrokerExchangeReceiptV1",
            {
                "response_frame_complete",
                "terminal_frame_complete",
                "flush_complete",
                "eof_or_cancel",
                "handle_closed",
            },
        ),
        (
            "mcphub_em_mcp.cst_saved_field_frontend_protocol",
            "DaemonResponseReceiptV1",
            {
                "response_frame_written",
                "terminal_frame_written",
                "flush_complete",
                "ack_received",
                "disconnect_complete",
                "server_handle_closed",
            },
        ),
    ],
)
def test_w01_red_owner_local_receipts_are_closed_serializable_wire_values(
    module_name: str, type_name: str, required_fields: set[str]
) -> None:
    owner_type = _owner_type(module_name, type_name)
    assert is_dataclass(owner_type), f"W01 gap: {type_name} is not a typed dataclass wire value"
    assert required_fields <= {item.name for item in fields(owner_type)}
    assert callable(getattr(owner_type, "from_wire", None))
    assert callable(getattr(owner_type, "to_wire", None))


def test_w01_red_worker_contract_declares_exact_five_handle_tuple() -> None:
    receipt_type = _owner_type(
        "mcphub_em_mcp.cst_saved_field_broker_worker_protocol", "WorkerPreMainReceiptV1"
    )
    assert getattr(receipt_type, "inherited_handle_roles", None) == (
        "stdin",
        "stdout",
        "stderr",
        "source-root",
        "workspace-root",
    )


def test_w01_red_no_fake_settlement_or_default_on_composition() -> None:
    daemon = importlib.import_module("mcphub_em_mcp.cst_saved_field_daemon_service_windows")
    broker = importlib.import_module("mcphub_em_mcp.cst_saved_field_broker_service_windows")
    for module in (daemon, broker):
        source = inspect.getsource(module)
        assert "UnavailableBrokerTransport" not in source
        assert "settlement=BrokerSettlementV1(True" not in source
    assert hasattr(daemon, "compose_default_off_runtime"), (
        "W01 gap: no fixed default-off composition owner proving zero listener/work on absent policy"
    )


def test_w01_red_production_topology_is_exactly_three_endpoints_and_four_schemas() -> None:
    daemon = importlib.import_module("mcphub_em_mcp.cst_saved_field_daemon_service_windows")
    topology = getattr(daemon, "CST_SAVED_FIELD_PRODUCTION_TOPOLOGY_V1", None)
    assert topology is not None, "W01 gap: no closed production topology contract"
    assert topology["endpoints"] == ("enrollment", "frontend", "broker")
    assert topology["schemas"] == (
        "hub-enrollment-v1",
        "frontend-v1",
        "broker-v1",
        "broker-worker-v1",
    )
