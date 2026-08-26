from __future__ import annotations

from pathlib import Path

PACKAGE = Path(__file__).parents[1] / "src" / "mcphub_em_mcp"


def test_broker_protocol_owner_exists_without_import_error() -> None:
    path = PACKAGE / "cst_saved_field_broker_protocol.py"
    assert path.is_file(), "P10-BROKER-PROTOCOL-MISSING"


def test_broker_protocol_declares_closed_qpc_bound_frames() -> None:
    path = PACKAGE / "cst_saved_field_broker_protocol.py"
    text = path.read_text(encoding="utf-8") if path.is_file() else ""
    required = (
        "BrokerChallengeV1",
        "BrokerRequestV1",
        "BrokerResponseV1",
        "BrokerSettlementV1",
        "qpc_frequency",
        "admitted_tick",
        "deadline_tick",
        "correlation_id",
        "policy_revision",
        "request_sha256",
    )
    missing = tuple(name for name in required if name not in text)
    assert missing == (), f"P10-BROKER-PROTOCOL-SURFACE:{missing}"
