from __future__ import annotations

import pytest


def _request(nonce: str, deadline=None):
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerRequestV1,
        QpcDeadlineV1,
        canonical_sha256,
    )

    body = {"field": "E", "points": [[0.0, 0.0, 0.0]]}
    return BrokerRequestV1(
        correlation_id="1" * 32,
        nonce=nonce,
        policy_revision="2" * 64,
        entry_id="line10-e",
        manifest_sha256="3" * 64,
        request_sha256=canonical_sha256(body),
        request=body,
        deadline=deadline or QpcDeadlineV1(10_000_000, 100, 600_000_100),
    )


def test_pipe_descriptor_is_single_local_remote_rejecting_and_narrow() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import build_broker_descriptor

    descriptor = build_broker_descriptor(broker_service_sid="S-1-5-80-333", daemon_service_sid="S-1-5-80-222")
    assert descriptor.endpoint == r"\\.\pipe\mcp-local-hub-cst-saved-field-v1"
    assert descriptor.first_instance and descriptor.overlapped and descriptor.remote_clients_rejected
    assert descriptor.instances == 1
    assert ("DENY", "S-1-5-7", 0x001F01FF, 0) in descriptor.aces
    assert ("DENY", "S-1-5-2", 0x001F01FF, 0) in descriptor.aces
    assert ("ALLOW", "S-1-5-80-222", 0x00100083, 0) in descriptor.aces
    assert descriptor.sacl_integrity_sid == "S-1-16-12288"


def test_impersonation_reverts_before_parse_and_unproved_revert_quarantines() -> None:
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import (
        DAEMON_ACCOUNT,
        DAEMON_SERVICE,
        AuthenticatedPipeSession,
        BrokerIsolationFailure,
        PeerTokenProofV1,
    )

    peer = PeerTokenProofV1(
        DAEMON_SERVICE,
        DAEMON_ACCOUNT,
        True,
        True,
        True,
        True,
        0,
        True,
        True,
        True,
    )

    class Impersonation:
        active = False
        revert_ok = True

        def impersonate(self):
            self.active = True
            return True

        def revert(self):
            self.active = False
            return self.revert_ok

    impersonation = Impersonation()
    privileged: list[str] = []
    session = AuthenticatedPipeSession(
        peer=peer,
        impersonation=impersonation,
        privileged_counter=lambda: privileged.append("authorized"),
    )
    parsed: list[bool] = []
    request = _request("4" * 64)
    assert session.authenticate_then(lambda: (parsed.append(impersonation.active), request)[1]) is request
    assert parsed == [False] and privileged == ["authorized"]

    impersonation.revert_ok = False
    parsed.clear()
    with pytest.raises(BrokerIsolationFailure) as raised:
        session.authenticate_then(lambda: (parsed.append(True), request)[1])
    assert raised.value.quarantine is True
    assert parsed == []


def test_nonce_is_256_bit_one_use_and_expiry_is_monotonic() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import BrokerProtocolFailure, QpcDeadlineV1
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import BrokerNonceLedgerV1

    ledger = BrokerNonceLedgerV1(random_bytes=lambda count: b"x" * count)
    deadline = QpcDeadlineV1(100, 1_000, 7_000)
    challenge = ledger.issue(deadline, issued_tick=1_000)
    assert challenge.nonce == (b"x" * 32).hex()
    request = _request(challenge.nonce, deadline)
    ledger.consume(request, current_tick=1_499, current_frequency=100)
    with pytest.raises(BrokerProtocolFailure):
        ledger.consume(request, current_tick=1_499, current_frequency=100)

    deadline = QpcDeadlineV1(100, 2_000, 8_000)
    challenge = ledger.issue(deadline, issued_tick=2_000)
    with pytest.raises(BrokerProtocolFailure):
        ledger.consume(_request(challenge.nonce, deadline), current_tick=2_500, current_frequency=100)
