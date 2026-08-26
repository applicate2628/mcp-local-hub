from __future__ import annotations

import json
from io import BytesIO


def test_daemon_client_sends_authority_only_request_and_preserves_qpc() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_client_windows import (
        BrokerExchangeReceiptV1,
        BrokerStartupProofV1,
        WindowsBrokerClient,
    )
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerChallengeV1,
        BrokerResponseV1,
        BrokerSettlementV1,
    )

    class Transport:
        request = None

        def startup_proof(self):
            return BrokerStartupProofV1(True, True, True, True, True)

        def challenge(self, deadline):
            return BrokerChallengeV1(
                "4" * 64,
                deadline.admitted_tick,
                deadline.admitted_tick + 5 * deadline.qpc_frequency,
                deadline.qpc_frequency,
            )

        def exchange(self, request):
            self.request = request
            response = BrokerResponseV1(
                request.correlation_id,
                request.policy_revision,
                request.request_sha256,
                request.deadline,
                True,
                json.dumps({"schema": "mcphub.cst.saved_field_sample.v1"}),
                None,
                BrokerSettlementV1(*([True] * 10), 0),
            )
            return response, BrokerExchangeReceiptV1(request.correlation_id, True, True, True, True, True)

        def cancel_and_settle(self, correlation_id, deadline):
            raise AssertionError((correlation_id, deadline))

    transport = Transport()
    client = WindowsBrokerClient(
        transport=transport,
        qpc_frequency=lambda: 100,
        qpc_counter=lambda: 10,
        correlation=lambda: "1" * 32,
    )
    response = client.invoke(
        policy_revision="2" * 64,
        entry_id="line10-e",
        manifest_sha256="3" * 64,
        request={"field": "E", "points": [[0.0, 0.0, 0.0]]},
    )
    assert response.ok is True
    wire = transport.request.to_wire()
    serialized = json.dumps(wire, sort_keys=True)
    for forbidden in ("project_bundle", "root", "path", "handle", "bytes"):
        assert forbidden not in serialized
    assert wire["deadline"] == {"qpc_frequency": 100, "admitted_tick": 10, "deadline_tick": 6_010}


def test_broker_startup_proof_is_all_required_and_fail_closed() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_client_windows import BrokerStartupProofV1

    assert BrokerStartupProofV1(True, True, True, True, True).complete is True
    assert BrokerStartupProofV1(True, True, True, True, False).complete is False


def test_fixed_broker_transport_derives_complete_local_receipt() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_client_windows import (
        BrokerStartupProofV1,
        WindowsNamedPipeBrokerTransport,
    )
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerChallengeV1,
        BrokerRequestV1,
        BrokerResponseV1,
        BrokerSettlementV1,
        QpcDeadlineV1,
        canonical_sha256,
        encode_frame,
    )

    deadline = QpcDeadlineV1(100, 10, 6010)
    challenge = BrokerChallengeV1("4" * 64, 10, 510, 100)
    request = BrokerRequestV1(
        "1" * 32, "4" * 64, "2" * 64, "line10-e", "3" * 64, canonical_sha256({}), {}, deadline
    )
    response = BrokerResponseV1(
        request.correlation_id,
        request.policy_revision,
        request.request_sha256,
        deadline,
        True,
        "{}",
        None,
        BrokerSettlementV1(*([True] * 10), 0),
    )
    incoming = BytesIO(
        encode_frame(challenge.to_wire())
        + encode_frame(response.to_wire())
        + encode_frame({"op": "terminal", "correlation_id": request.correlation_id})
    )

    class Channel:
        closed = False

        def read(self, count=-1):
            return incoming.read(count)

        def write(self, value):
            return len(value)

        def flush(self):
            return None

        def close(self):
            self.closed = True

    channel = Channel()
    transport = WindowsNamedPipeBrokerTransport(
        startup=BrokerStartupProofV1(True, True, True, True, True),
        connector=lambda endpoint: channel,
    )
    assert transport.challenge(deadline) == challenge
    actual, receipt = transport.exchange(request)
    assert actual == response
    assert receipt.complete and channel.closed
