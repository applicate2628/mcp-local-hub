from __future__ import annotations

from pathlib import Path

PACKAGE = Path(__file__).parents[1] / "src" / "mcphub_em_mcp"


def test_worker_protocol_owner_exists_without_import_error() -> None:
    path = PACKAGE / "cst_saved_field_broker_worker_protocol.py"
    assert path.is_file(), "P10-WORKER-PROTOCOL-MISSING"


def test_sr_c5_02_breakaway_truth_is_exact_conjunction() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import WorkerStartupProofV1

    assert WorkerStartupProofV1(True, True, True, True, False, True).complete is True
    assert WorkerStartupProofV1(True, True, True, True, True, True).complete is False


def test_worker_protocol_declares_nested_settlement_and_unchanged_qpc() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import BrokerWorkerRequestV1

    deadline = QpcDeadlineV1(100, 20, 6_020)
    body = {"field": "E"}
    from mcphub_em_mcp.cst_saved_field_broker_protocol import canonical_sha256

    value = BrokerWorkerRequestV1(
        "1" * 32,
        "2" * 64,
        "line10-e",
        "3" * 64,
        canonical_sha256(body),
        body,
        deadline,
    )
    assert value.deadline is deadline
