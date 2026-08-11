from __future__ import annotations

import json


def test_daemon_client_sends_authority_only_request_and_preserves_qpc() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_client_windows import (
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
            return BrokerResponseV1(
                request.correlation_id,
                request.policy_revision,
                request.request_sha256,
                request.deadline,
                True,
                json.dumps({"schema": "mcphub.cst.saved_field_sample.v1"}),
                None,
                BrokerSettlementV1(*([True] * 10), 0),
            )

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
