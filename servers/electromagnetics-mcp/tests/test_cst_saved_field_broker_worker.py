from __future__ import annotations

import io

import pytest


def test_worker_capability_receipt_is_closed_and_fail_closed() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import BrokerProtocolFailure
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import WorkerCapabilityReceiptV1

    receipt = WorkerCapabilityReceiptV1("a" * 32, True, True, True, False)
    assert receipt.complete
    assert WorkerCapabilityReceiptV1.from_wire(receipt.to_wire()) == receipt
    with pytest.raises(BrokerProtocolFailure):
        WorkerCapabilityReceiptV1.from_wire({**receipt.to_wire(), "broker_owned_fact": True})
    assert not WorkerCapabilityReceiptV1("a" * 32, True, True, False, False).complete


def test_worker_first_instruction_proof_precedes_request_read() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1
    from mcphub_em_mcp.cst_saved_field_broker_worker import run_worker
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        BrokerWorkerResponseV1,
        WorkerCapabilityReceiptV1,
        WorkerPreMainBootstrapV1,
        WorkerPreMainReceiptV1,
        WorkerSettlementV1,
    )

    class Unreadable(io.BytesIO):
        read_started = False

        def read(self, size=-1):
            self.read_started = True
            raise AssertionError("request read")

    source = Unreadable()
    identity = {"volume_serial": 1, "file_id": "native-id"}
    deadline = QpcDeadlineV1(10, 0, 600)
    bootstrap = WorkerPreMainBootstrapV1("1" * 32, deadline, 101, 202, 0x120089, 0x12019F, identity, identity)
    receipt = WorkerPreMainReceiptV1(
        bootstrap.correlation_id,
        deadline,
        True,
        True,
        False,
        bootstrap.source_access_mask,
        bootstrap.workspace_access_mask,
        identity,
        identity,
        bootstrap.checksum,
    )

    def application(_request):
        return BrokerWorkerResponseV1(
            "1" * 32,
            "2" * 64,
            "3" * 64,
            None,
            False,
            None,
            "cst_saved_field.activation_failed",
            WorkerSettlementV1(False, False, False, False, False, False, 0),
            WorkerCapabilityReceiptV1("1" * 32, True, True, True, False),
        )

    with pytest.raises(AssertionError, match="request read"):
        run_worker(
            source,
            io.BytesIO(),
            application,
            bootstrap=bootstrap,
            pre_main_receipt=receipt,
        )
    assert source.read_started is True


@pytest.mark.parametrize(
    ("denied", "created", "settled", "complete"),
    [
        (True, False, True, True),
        (True, True, True, False),
        (False, False, True, False),
        (False, True, True, False),
        (True, True, False, False),
    ],
)
def test_sr_c5_02_breakaway_truth_table(denied: bool, created: bool, settled: bool, complete: bool) -> None:
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import WorkerStartupProofV1

    proof = WorkerStartupProofV1(True, True, True, denied, created, settled)
    assert proof.complete is complete
