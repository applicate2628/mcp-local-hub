from __future__ import annotations

import io
import json

import pytest


def test_t03_four_closed_protocol_schemas_and_frontend_authority_boundary() -> None:
    from mcphub_em_mcp import cst_saved_field_frontend_protocol as frontend

    request = {"field": "E"}
    request_hash = frontend.canonical_sha256(request)
    value = frontend.FrontendDaemonRequestV1(
        correlation_id="1" * 32,
        challenge_nonce="2" * 64,
        launch_capability="3" * 64,
        entry_id="line10-e",
        request_sha256=request_hash,
        request=request,
    )
    wire = value.to_wire()
    assert set(wire) == {
        "schema",
        "correlation_id",
        "challenge_nonce",
        "launch_capability",
        "entry_id",
        "request_sha256",
        "request",
    }
    assert not (
        {"source_path", "source_bytes", "source_handle", "manifest_sha256", "policy_revision"} & set(wire)
    )
    with pytest.raises(frontend.FrontendProtocolFailure):
        frontend.FrontendDaemonRequestV1.from_wire({**wire, "extra": True})


def test_t03_absolute_budget_is_one_unchanged_qpc_triple() -> None:
    from mcphub_em_mcp.cst_saved_field_port import AbsoluteInvocationBudget

    budget = AbsoluteInvocationBudget(10_000_000, 123, 600_000_123)
    assert AbsoluteInvocationBudget.from_wire(budget.to_wire()) == budget
    assert budget.remaining(current_frequency=10_000_000, current_tick=500_000_123) == 10.0
    assert budget.cleanup_deadline(termination_tick=700) == 100_000_700
    wire = budget.to_wire()
    wire["deadline_tick"] += 1
    with pytest.raises(ValueError):
        AbsoluteInvocationBudget.from_wire(wire)


def test_t03_policy_v1_requires_exact_three_endpoint_descriptors_and_manifest_v2() -> None:
    from mcphub_em_mcp import cst_saved_field_policy as policy

    assert policy.POLICY_SCHEMA == "mcphub.cst.saved_field_authority.v1"
    assert policy.MANIFEST_SCHEMA == "sha256-canonical-file-list-v2"
    assert policy.EXACT_ENDPOINTS == (
        r"\\.\pipe\mcp-local-hub-cst-saved-field-enrollment-v1",
        r"\\.\pipe\mcp-local-hub-cst-saved-field-frontend-v1",
        r"\\.\pipe\mcp-local-hub-cst-saved-field-v1",
    )


def test_t03_frame_ceiling_canonical_duplicate_and_trailing_guards() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerProtocolFailure,
        decode_one_frame,
        encode_frame,
    )

    canonical = encode_frame({"a": 1}, maximum=7)
    assert decode_one_frame(io.BytesIO(canonical), maximum=7) == {"a": 1}
    duplicate = b'{"a":1,"a":2}'
    with pytest.raises(BrokerProtocolFailure):
        decode_one_frame(io.BytesIO(len(duplicate).to_bytes(4, "big") + duplicate))
    noncanonical = json.dumps({"a": 1}).encode()
    with pytest.raises(BrokerProtocolFailure):
        decode_one_frame(io.BytesIO(len(noncanonical).to_bytes(4, "big") + noncanonical))
    with pytest.raises(BrokerProtocolFailure):
        decode_one_frame(io.BytesIO(canonical + b"x"), maximum=7)


def test_t03_neutral_port_imports_no_cst_or_windows_implementation() -> None:
    from pathlib import Path

    source = (Path(__file__).parents[1] / "src" / "mcphub_em_mcp" / "cst_saved_field_port.py").read_text()
    assert "import ctypes" not in source
    assert "import win32" not in source
    assert "from .cst" not in source
    assert "import cst" not in source
