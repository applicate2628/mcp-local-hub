from __future__ import annotations

import io

import pytest


def test_worker_first_instruction_proof_precedes_request_read() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_worker import run_worker
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        BrokerWorkerResponseV1,
        WorkerSettlementV1,
        decode_startup_proof_frame,
    )

    class Unreadable(io.BytesIO):
        read_started = False

        def read(self, size=-1):
            self.read_started = True
            raise AssertionError("request read")

    source = Unreadable()
    diagnostics = io.BytesIO()

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
        )

    with pytest.raises(AssertionError, match="request read"):
        run_worker(
            source,
            io.BytesIO(),
            application,
            diagnostics=diagnostics,
            startup_observation={
                "exact_job": True,
                "exactly_three_inherited_std_handles": True,
                "no_console": True,
                "breakaway_denied": True,
                "breakaway_created": False,
                "escaped_process_settled": True,
            },
        )
    assert source.read_started is True
    proof = decode_startup_proof_frame(io.BytesIO(diagnostics.getvalue()))
    assert proof.complete is True


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
