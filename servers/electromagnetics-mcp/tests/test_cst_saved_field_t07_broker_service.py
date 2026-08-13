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


def _peer(**changes):
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import BrokerPeerIdentityV1

    values = {
        "pid": 4312,
        "token_user_sid": "S-1-5-80-222",
        "service_sid": "S-1-5-80-222",
        "service_sid_enabled": True,
        "scm_pid_matches": True,
        "session_id": 0,
        "high_integrity": True,
        "prohibited_privileges_absent": True,
        "image_path": r"C:\Program Files\mcp-local-hub\cst-daemon.exe",
    }
    values.update(changes)
    return BrokerPeerIdentityV1(**values)


def _response(request):
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerResponseV1,
        BrokerSettlementV1,
    )

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


def _request(challenge, *, manifest="3" * 64):
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerRequestV1,
        QpcDeadlineV1,
        canonical_sha256,
    )

    body = {"field": "E", "points": [[0.0, 0.0, 0.0]]}
    return BrokerRequestV1(
        "a" * 32,
        challenge.nonce,
        "4" * 64,
        "line10-e",
        manifest,
        canonical_sha256(body),
        body,
        QpcDeadlineV1(100, 100, 6100),
    )


def _service(*, ticks=None, calls=None):
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import BrokerRuntimeServiceV1

    observed = [] if calls is None else calls
    counter = iter(ticks or (105, 106)).__next__
    workspace_policy = object()
    return BrokerRuntimeServiceV1(
        snapshot=_snapshot(),
        daemon_service_sid="S-1-5-80-222",
        daemon_image=r"C:\Program Files\mcp-local-hub\cst-daemon.exe",
        workspace_policy=workspace_policy,
        application=lambda request, entry, workspace: (
            observed.append((request, entry, workspace)),
            _response(request),
        )[1],
        qpc_frequency=lambda: 100,
        qpc_counter=counter,
        random_bytes=lambda count: b"n" * count,
    )


def test_t07_red_challenge_allows_latency_and_preserves_original_triple() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1

    calls = []
    service = _service(ticks=(105, 106), calls=calls)
    deadline = QpcDeadlineV1(100, 100, 6100)
    challenge = service.issue_challenge(_peer(), deadline)
    assert challenge.issued_tick == 105
    assert challenge.expires_tick == 605
    response = service.exchange(_peer(), _request(challenge))
    assert response.deadline is deadline or response.deadline == deadline
    assert len(calls) == 1
    assert calls[0][1].entry_id == "line10-e"
    assert calls[0][2] is not None
    assert service.nonce_state(challenge.nonce) == "CONSUMED"


def test_t07_red_nonce_consumes_on_policy_failure_and_replay_does_zero_work() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import BrokerProtocolFailure, QpcDeadlineV1

    calls = []
    service = _service(ticks=(105, 106, 107), calls=calls)
    challenge = service.issue_challenge(_peer(), QpcDeadlineV1(100, 100, 6100))
    request = _request(challenge, manifest="f" * 64)
    with pytest.raises(BrokerProtocolFailure, match="broker_unauthorized"):
        service.exchange(_peer(), request)
    assert service.nonce_state(challenge.nonce) == "CONSUMED"
    with pytest.raises(BrokerProtocolFailure, match="broker_protocol_invalid"):
        service.exchange(_peer(), request)
    assert calls == []


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("pid", 0),
        ("token_user_sid", "S-1-5-80-999"),
        ("service_sid", "S-1-5-80-999"),
        ("service_sid_enabled", False),
        ("scm_pid_matches", False),
        ("session_id", 1),
        ("high_integrity", False),
        ("prohibited_privileges_absent", False),
        ("image_path", r"C:\tmp\cst-daemon.exe"),
    ],
)
def test_t07_red_peer_identity_denies_before_nonce(field: str, value: object) -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import BrokerProtocolFailure, QpcDeadlineV1

    service = _service()
    with pytest.raises(BrokerProtocolFailure, match="broker_unauthorized"):
        service.issue_challenge(_peer(**{field: value}), QpcDeadlineV1(100, 100, 6100))
    assert service.outstanding_nonce_count == 0


def test_t07_red_descriptors_are_numeric_exact_three_and_read_back() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import (
        DescriptorFailure,
        build_broker_descriptor,
        build_frontend_descriptor,
        validate_sampler_descriptors,
    )
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import build_enrollment_descriptor

    enrollment = build_enrollment_descriptor(
        daemon_service_sid="S-1-5-80-222", policy_owner_sid="S-1-5-21-1000"
    )
    frontend = build_frontend_descriptor(daemon_service_sid="S-1-5-80-222", frontend_user_sid="S-1-5-21-1000")
    broker = build_broker_descriptor(broker_service_sid="S-1-5-80-333", daemon_service_sid="S-1-5-80-222")
    validate_sampler_descriptors((enrollment, frontend, broker))
    broker.verify_readback(broker)
    assert broker.aces[:2] == (
        ("DENY", "S-1-5-7", 0x001F01FF, 0),
        ("DENY", "S-1-5-2", 0x001F01FF, 0),
    )
    with pytest.raises(DescriptorFailure):
        build_broker_descriptor(
            broker_service_sid=r"NT SERVICE\McpLocalHubCstVendorBroker",
            daemon_service_sid="S-1-5-80-222",
        )
    with pytest.raises(DescriptorFailure):
        validate_sampler_descriptors((enrollment, frontend, broker, broker))


def test_t07_red_output_root_is_broker_only_and_injects_verified_policy() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import (
        OUTPUT_ROOT_ENV,
        load_output_workspace_policy,
    )

    calls = []
    marker = object()
    result = load_output_workspace_policy(
        {OUTPUT_ROOT_ENV: r"C:\broker-output"},
        object(),
        factory=lambda raw, platform: (calls.append((raw, platform)), marker)[1],
    )
    assert result is marker
    assert calls == [(r"C:\broker-output", calls[0][1])]

    from pathlib import Path

    production_hits = []
    for path in (Path(__file__).parents[1] / "src" / "mcphub_em_mcp").glob("cst_saved_field*.py"):
        if OUTPUT_ROOT_ENV in path.read_text():
            production_hits.append(path.name)
    assert production_hits == ["cst_saved_field_broker_service_windows.py"]


def test_t07_red_broker_service_has_no_frontend_or_mcp_dependency() -> None:
    from pathlib import Path

    source = (
        Path(__file__).parents[1] / "src" / "mcphub_em_mcp" / "cst_saved_field_broker_service_windows.py"
    ).read_text()
    for forbidden in ("fastmcp", "frontend_protocol", "daemon_client", "cst.py"):
        assert forbidden not in source.casefold()
