from __future__ import annotations

import json

import pytest


def _peer(**changes: object):
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import EnrollmentPeerIdentityV1

    values = {
        "pid": 4312,
        "creation_time": "2026-08-12T08:00:00.0000000Z",
        "image_path": r"C:\Program Files\mcp-local-hub\mcphub.exe",
        "package_identity": "mcphub/0.4.28/b87dc8dd",
        "parent_pid": 902,
        "token_user_sid": "S-1-5-21-1000",
        "session_id": 1,
    }
    values.update(changes)
    return EnrollmentPeerIdentityV1(**values)


def _status(**changes: object):
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import SupervisorTaskIdentityV1

    values = {
        "task": "cst",
        "generation": 7,
        "pid": 4312,
        "creation_time": "2026-08-12T08:00:00.0000000Z",
        "image_path": r"C:\Program Files\mcp-local-hub\mcphub.exe",
        "package_identity": "mcphub/0.4.28/b87dc8dd",
        "parent_pid": 902,
        "token_user_sid": "S-1-5-21-1000",
        "session_id": 1,
    }
    values.update(changes)
    return SupervisorTaskIdentityV1(**values)


class _Clock:
    def __init__(self) -> None:
        self.value = 100.0

    def __call__(self) -> float:
        return self.value


def _server(status=None):
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import HubEnrollmentServerV1

    clock = _Clock()
    current = _status() if status is None else status
    random_values = iter((b"n" * 32, b"m" * 32, b"q" * 32, b"r" * 32))
    server = HubEnrollmentServerV1(
        query_supervisor=lambda timeout: current if timeout == 5.0 else None,
        random_bytes=lambda count: next(random_values),
        monotonic=clock,
    )
    return server, clock


def _enroll_frame(challenge: str, correlation: str, capability: bytes, generation: int = 7) -> bytes:
    import hashlib

    return json.dumps(
        {
            "version": 1,
            "op": "enroll",
            "challenge": challenge,
            "correlation": correlation,
            "task": "cst",
            "generation": generation,
            "capability_sha256": hashlib.sha256(capability).hexdigest(),
        },
        separators=(",", ":"),
    ).encode()


def test_t04_red_authentication_matrix_is_independent_of_frame_claims() -> None:
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import EnrollmentFailure

    fields = {
        "pid": 4313,
        "creation_time": "2026-08-12T08:00:01Z",
        "image_path": r"C:\tmp\mcphub.exe",
        "package_identity": "mcphub/other",
        "parent_pid": 903,
        "token_user_sid": "S-1-5-21-2000",
        "session_id": 2,
    }
    for field, value in fields.items():
        server, _ = _server()
        with pytest.raises(EnrollmentFailure, match="identity_mismatch"):
            server.issue_challenge(_peer(**{field: value}))
    server, _ = _server(_status(task="other"))
    with pytest.raises(EnrollmentFailure, match="identity_mismatch"):
        server.issue_challenge(_peer())
    server, _ = _server(_status(generation=0))
    with pytest.raises(EnrollmentFailure, match="identity_mismatch"):
        server.issue_challenge(_peer())


def test_t04_red_nonce_and_capability_ledgers_are_independent_one_use() -> None:
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import EnrollmentFailure

    capability = b"c" * 32
    correlation = "a" * 32
    server, _ = _server()
    challenge = server.issue_challenge(_peer())
    receipt = server.exchange(_peer(), _enroll_frame(challenge, correlation, capability))
    assert receipt == {
        "version": 1,
        "correlation": correlation,
        "state": "ENROLLED",
        "channel_settled": True,
    }
    assert server.channel_state(challenge) == "CONSUMED"
    assert server.capability_state(correlation) == "ENROLLED"
    with pytest.raises(EnrollmentFailure, match="replay"):
        server.exchange(_peer(), _enroll_frame(challenge, correlation, capability))
    assert server.capability_state(correlation) == "ENROLLED"
    assert server.consume_frontend(
        correlation,
        capability,
        exact_32_and_eof=True,
        frontend_challenge_consumed=True,
    )
    assert server.capability_state(correlation) == "CONSUMED"
    assert not server.consume_frontend(
        correlation,
        capability,
        exact_32_and_eof=True,
        frontend_challenge_consumed=True,
    )


@pytest.mark.parametrize(
    "terminalizer",
    ["ack_loss", "post_ack_failure", "child_exit", "disconnect", "service_stop", "shutdown", "restart"],
)
def test_t04_red_all_post_enrollment_exits_cancel_without_stranding(terminalizer: str) -> None:
    capability = b"d" * 32
    correlation = "b" * 32
    server, _ = _server()
    challenge = server.issue_challenge(_peer())
    server.exchange(_peer(), _enroll_frame(challenge, correlation, capability))
    getattr(server, terminalizer)(correlation)
    assert server.capability_state(correlation) == "CANCELLED"
    assert server.outstanding_capability_count == 0
    assert server.outstanding_channel_count == 0


def test_t04_red_fresh_authenticated_cancel_and_expiry_are_terminal() -> None:
    capability = b"e" * 32
    correlation = "c" * 32
    server, clock = _server()
    first = server.issue_challenge(_peer())
    server.exchange(_peer(), _enroll_frame(first, correlation, capability))
    server.issue_challenge(_peer())
    cancel = json.dumps(
        {"version": 1, "op": "cancel", "correlation": correlation}, separators=(",", ":")
    ).encode()
    assert server.exchange(_peer(), cancel)["state"] == "CANCELLED"
    assert server.outstanding_capability_count == 0

    second = server.issue_challenge(_peer())
    server.exchange(_peer(), _enroll_frame(second, "d" * 32, capability))
    clock.value += 5.001
    server.expire()
    assert server.capability_state("d" * 32) == "CANCELLED"
    assert server.outstanding_capability_count == 0


def test_t04_red_frame_failures_cancel_channel_and_never_authorize_digest() -> None:
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import EnrollmentFailure

    server, _ = _server()
    challenge = server.issue_challenge(_peer())
    with pytest.raises(EnrollmentFailure, match="frame_invalid"):
        server.exchange(_peer(), b'{"version":1,"version":1}')
    assert server.channel_state(challenge) == "CANCELLED"
    assert server.outstanding_channel_count == 0
    assert server.outstanding_capability_count == 0


def test_t04_red_descriptor_is_exact_numeric_local_high_and_one_of_three() -> None:
    from mcphub_em_mcp.cst_saved_field_hub_enrollment_windows import (
        DescriptorFailure,
        build_enrollment_descriptor,
    )
    from mcphub_em_mcp.cst_saved_field_policy import EXACT_ENDPOINTS

    descriptor = build_enrollment_descriptor(
        daemon_service_sid="S-1-5-80-111",
        policy_owner_sid="S-1-5-21-1000",
    )
    assert descriptor.endpoint == EXACT_ENDPOINTS[0]
    assert descriptor.endpoint in EXACT_ENDPOINTS and len(EXACT_ENDPOINTS) == 3
    assert descriptor.first_instance and descriptor.remote_clients_rejected and descriptor.message_mode
    assert descriptor.dacl_protected and descriptor.sacl_integrity_sid == "S-1-16-12288"
    assert descriptor.sacl_no_write_up and descriptor.audit_success_failure
    assert descriptor.aces == (
        ("ALLOW", "S-1-5-18", 0x1F01FF, 0),
        ("ALLOW", "S-1-5-80-111", 0x1F01FF, 0),
        ("ALLOW", "S-1-5-21-1000", 0x0012019F, 0),
    )
    descriptor.verify_readback(descriptor)
    with pytest.raises(DescriptorFailure):
        build_enrollment_descriptor(
            daemon_service_sid=r"NT SERVICE\McpLocalHubCstDaemon",
            policy_owner_sid="S-1-5-21-1000",
        )
    with pytest.raises(DescriptorFailure):
        descriptor.verify_readback(
            descriptor.__class__(**{**descriptor.as_dict(), "aces": tuple(reversed(descriptor.aces))})
        )


def test_t04_red_source_has_no_bypass_detach_or_frontend_fallback() -> None:
    from pathlib import Path

    source = (
        Path(__file__).parents[1] / "src" / "mcphub_em_mcp" / "cst_saved_field_hub_enrollment_windows.py"
    ).read_text()
    assert "test_bypass" not in source
    assert "detached" not in source
    assert "frontend_enroll" not in source
    assert "digest_only" not in source
