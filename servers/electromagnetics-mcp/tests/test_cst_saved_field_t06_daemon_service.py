from __future__ import annotations

import json

import pytest


def _snapshot():
    from mcphub_em_mcp.cst_saved_field_policy import AuthorityEntry, AuthoritySnapshot, RootIdentityV1

    entry = AuthorityEntry(
        "line10-e",
        r"C:\\authorized",
        RootIdentityV1(7, "ab" * 8),
        "Line10.cst",
        "1" * 64,
        "2" * 64,
        "3" * 64,
    )
    return AuthoritySnapshot("4" * 64, (entry,), {})


class _Enrollment:
    def __init__(self) -> None:
        self.calls: list[tuple[object, ...]] = []

    def consume_frontend(self, correlation, capability, **evidence):
        self.calls.append((correlation, capability, evidence))
        return evidence == {"exact_32_and_eof": True, "frontend_challenge_consumed": True}

    def shutdown(self, correlation=None):
        self.calls.append(("shutdown", correlation))


class _Transport:
    def startup_proof(self):
        from mcphub_em_mcp.cst_saved_field_broker_client_windows import BrokerStartupProofV1

        return BrokerStartupProofV1(True, True, True, True, True)

    def challenge(self, deadline):
        from mcphub_em_mcp.cst_saved_field_broker_protocol import BrokerChallengeV1

        # Challenge issuance may have latency; the original admitted triple is unchanged.
        return BrokerChallengeV1(
            "5" * 64,
            deadline.admitted_tick + 2,
            deadline.admitted_tick + 2 + 5 * deadline.qpc_frequency,
            deadline.qpc_frequency,
        )

    def exchange(self, request):
        from mcphub_em_mcp.cst_saved_field_broker_protocol import (
            BrokerResponseV1,
            BrokerSettlementV1,
        )

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
        from mcphub_em_mcp.cst_saved_field_broker_client_windows import BrokerCancelReceiptV1

        return BrokerCancelReceiptV1(True, True, True, 0)


def _request(service):
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import (
        FrontendDaemonRequestV1,
        canonical_sha256,
    )

    correlation = "a" * 32
    challenge = service.issue_challenge(correlation)
    body = {"field": "E", "points": [[0.0, 0.0, 0.0]]}
    return FrontendDaemonRequestV1(
        correlation,
        challenge,
        (b"c" * 32).hex(),
        "line10-e",
        canonical_sha256(body),
        body,
    )


def _receipt(correlation: str, *, complete: bool = True):
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import DaemonResponseReceiptV1

    return DaemonResponseReceiptV1(
        correlation,
        True,
        True,
        True,
        complete,
        complete,
        complete,
    )


def _service(*, events, transport=None):
    from mcphub_em_mcp.cst_saved_field_daemon_service_windows import WindowsCstDaemonService

    return WindowsCstDaemonService(
        snapshot=_snapshot(),
        enrollment=_Enrollment(),
        broker_transport=transport or _Transport(),
        qpc_frequency=lambda: 100,
        qpc_counter=iter((10, 12, 13, 14, 15)).__next__,
        broker_correlation=lambda: "b" * 32,
        random_bytes=lambda count: b"n" * count,
        event_sink=events.append,
    )


def test_t06_red_daemon_auth_resolves_entry_and_preserves_original_qpc() -> None:
    events = []
    transport = _Transport()
    service = _service(events=events, transport=transport)
    request = _request(service)
    result, receipt = service.exchange(
        request,
        exact_capability_eof=True,
        response_writer=lambda value: _receipt(value.correlation_id),
    )
    assert result.ok and receipt.complete
    assert result.budget.to_wire() == {
        "qpc_frequency": 100,
        "admitted_tick": 10,
        "deadline_tick": 6010,
    }
    assert transport.request.deadline.to_wire() == result.budget.to_wire()
    assert transport.request.entry_id == "line10-e"
    assert transport.request.manifest_sha256 == "3" * 64
    assert service.enrollment.calls[0][1] == b"c" * 32
    serialized = json.dumps(transport.request.to_wire(), sort_keys=True)
    assert all(word not in serialized for word in ("project_bundle", "root", "path", "handle", "bytes"))
    assert [event.disposition for event in events] == ["released"]


def test_t06_red_daemon_receipt_alone_gates_release_and_quarantines_first() -> None:
    events = []
    service = _service(events=events)
    request = _request(service)
    with pytest.raises(RuntimeError, match="containment_settle_failed"):
        service.exchange(
            request,
            exact_capability_eof=True,
            response_writer=lambda value: _receipt(value.correlation_id, complete=False),
        )
    assert service.quarantined is True
    assert [event.disposition for event in events] == ["quarantined"]
    with pytest.raises(RuntimeError, match="containment_quarantined"):
        service.issue_challenge("d" * 32)


def test_t06_shutdown_during_response_cannot_return_success_or_double_settle() -> None:
    events = []
    service = _service(events=events)
    request = _request(service)

    def shutdown_before_close(value):
        service.shutdown()
        return _receipt(value.correlation_id)

    with pytest.raises(RuntimeError, match="containment_settle_failed"):
        service.exchange(
            request,
            exact_capability_eof=True,
            response_writer=shutdown_before_close,
        )
    assert service.quarantined is True
    assert [event.disposition for event in events] == ["quarantined"]


@pytest.mark.parametrize("entry_id", ["missing", "other-valid-id"])
def test_t06_red_denial_does_zero_broker_work_and_terminalizes_challenge(entry_id: str) -> None:
    events = []
    transport = _Transport()
    service = _service(events=events, transport=transport)
    request = _request(service)
    request = request.__class__(
        request.correlation_id,
        request.challenge_nonce,
        request.launch_capability,
        entry_id,
        request.request_sha256,
        request.request,
    )
    with pytest.raises(RuntimeError, match="not_authorized"):
        service.exchange(
            request,
            exact_capability_eof=True,
            response_writer=lambda value: _receipt(value.correlation_id),
        )
    assert not hasattr(transport, "request")
    assert service.challenge_state(request.correlation_id) == "CONSUMED"
    assert events == []


def test_t06_red_source_has_daemon_only_dependency_boundary() -> None:
    from pathlib import Path

    source = (
        Path(__file__).parents[1] / "src" / "mcphub_em_mcp" / "cst_saved_field_daemon_service_windows.py"
    ).read_text()
    for forbidden in (
        "cst_saved_field_source",
        "cst_saved_field_safety",
        "cst_saved_field_containment_windows",
        "cst_saved_field_vendor",
        "cst_saved_field_broker_worker",
    ):
        assert forbidden not in source
