from __future__ import annotations

import ast
import json
from pathlib import Path

import pytest


def _request() -> dict[str, object]:
    return {
        "expected_project_sha256": None,
        "field": "E",
        "result": {
            "port": 1,
            "mode": 1,
            "frequency_hz": 3e9,
            "frequency_tolerance_hz": 0.0,
            "frame_selector": "frame-e1",
            "expected_field_sha256": None,
            "expected_mesh_sha256": None,
            "adaptive_pass": None,
        },
        "points": [{"id": "p1", "xyz": [1.0, 2.0, 3.0]}],
        "coordinate_unit": "mm",
        "allow_solve": False,
        "max_points": 1,
    }


class _Transport:
    def __init__(self, *, result=None, receipt=None, fail_at: str | None = None) -> None:
        self.result = result
        self.receipt = receipt
        self.fail_at = fail_at
        self.calls: list[tuple[str, object]] = []

    def startup_proof(self, timeout: float) -> bool:
        self.calls.append(("startup", timeout))
        return self.fail_at != "startup"

    def challenge(self, correlation: str, timeout: float) -> str:
        self.calls.append(("challenge", (correlation, timeout)))
        if self.fail_at == "challenge":
            raise OSError("CANARY challenge")
        return "2" * 64

    def exchange(self, request, timeout: float):
        self.calls.append(("exchange", (request, timeout)))
        if self.fail_at == "exchange":
            raise OSError("CANARY exchange")
        return self.result, self.receipt

    def cancel(self, correlation: str, timeout: float) -> bool:
        self.calls.append(("cancel", (correlation, timeout)))
        return self.fail_at != "cancel"


def _result(*, deadline: int = 1_000_000, request_sha256: str = "3" * 64):
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import FrontendDaemonResultV1
    from mcphub_em_mcp.cst_saved_field_port import AbsoluteInvocationBudget

    return FrontendDaemonResultV1(
        correlation_id="1" * 32,
        entry_id="fixture",
        request_sha256=request_sha256,
        budget=AbsoluteInvocationBudget(10_000, deadline - 600_000, deadline),
        ok=True,
        text='{"schema":"ok"}',
        failure_id=None,
    )


def _receipt(**changes: object):
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import FrontendTransportReceiptV1

    values = {
        "correlation_id": "1" * 32,
        "response_frame_complete": True,
        "terminal_frame_complete": True,
        "eof_or_cancel": True,
        "client_handle_closed": True,
    }
    values.update(changes)
    return FrontendTransportReceiptV1(**values)


def test_t05_red_frontend_request_has_capability_and_no_authority_locator() -> None:
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import (
        FrontendDaemonRequestV1,
        canonical_sha256,
    )

    request = _request()
    value = FrontendDaemonRequestV1(
        correlation_id="1" * 32,
        challenge_nonce="2" * 64,
        launch_capability="4" * 64,
        entry_id="fixture",
        request_sha256=canonical_sha256(request),
        request=request,
    )
    wire = value.to_wire()
    assert wire["launch_capability"] == "4" * 64
    forbidden = {"project_bundle", "source_path", "manifest_sha256", "policy_revision"}
    assert not (forbidden & set(wire["request"]))


def test_t05_red_capability_reader_is_exact_once_eof_and_closes_every_exit() -> None:
    from mcphub_em_mcp.cst_saved_field_daemon_client_windows import (
        DaemonClientFailure,
        read_inherited_launch_capability,
    )

    events: list[str] = []
    chunks = iter((b"c" * 32, b""))
    got = read_inherited_launch_capability(
        "77",
        open_handle=lambda locator: events.append(f"open:{locator}") or 9,
        read_handle=lambda handle, count: events.append(f"read:{handle}:{count}") or next(chunks),
        close_handle=lambda handle: events.append(f"close:{handle}") or True,
    )
    assert got == b"c" * 32
    assert events == ["open:77", "read:9:33", "read:9:1", "close:9"]
    partial = iter((b"c" * 7, b"c" * 25, b""))
    assert (
        read_inherited_launch_capability(
            "77",
            open_handle=lambda _locator: 9,
            read_handle=lambda _handle, _count: next(partial),
            close_handle=lambda _handle: True,
        )
        == b"c" * 32
    )
    with pytest.raises(DaemonClientFailure):
        read_inherited_launch_capability(
            "77",
            open_handle=lambda _locator: 9,
            read_handle=lambda _handle, _count: b"x" * 33,
            close_handle=lambda handle: events.append(f"bad-close:{handle}") or True,
        )
    assert events[-1] == "bad-close:9"


def test_t05_red_client_one_use_challenge_receipt_deadline_and_cancel() -> None:
    from mcphub_em_mcp.cst_saved_field_daemon_client_windows import (
        DaemonClientFailure,
        WindowsDaemonClient,
    )
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import canonical_sha256

    result = _result(request_sha256=canonical_sha256(_request()))
    transport = _Transport(result=result, receipt=_receipt())
    client = WindowsDaemonClient(
        transport=transport,
        capability=bytearray(b"c" * 32),
        correlation=lambda: "1" * 32,
        qpc_frequency=lambda: 10_000,
        qpc_counter=lambda: 500_000,
    )
    response = client.invoke(entry_id="fixture", request=_request())
    assert response.text == '{"schema":"ok"}'
    assert [name for name, _ in transport.calls] == ["challenge", "exchange"]
    with pytest.raises(DaemonClientFailure, match="unavailable"):
        client.invoke(entry_id="fixture", request=_request())

    failed = _Transport(fail_at="exchange")
    client = WindowsDaemonClient(
        transport=failed,
        capability=bytearray(b"d" * 32),
        correlation=lambda: "1" * 32,
        qpc_frequency=lambda: 10_000,
        qpc_counter=lambda: 500_000,
    )
    with pytest.raises(DaemonClientFailure):
        client.invoke(entry_id="fixture", request=_request())
    assert [name for name, _ in failed.calls] == ["challenge", "exchange", "cancel"]


@pytest.mark.parametrize(
    "receipt_change",
    [
        {"response_frame_complete": False},
        {"terminal_frame_complete": False},
        {"eof_or_cancel": False},
        {"client_handle_closed": False},
    ],
)
def test_t05_red_incomplete_frontend_receipt_suppresses_publication(receipt_change) -> None:
    from mcphub_em_mcp.cst_saved_field_daemon_client_windows import (
        DaemonClientFailure,
        WindowsDaemonClient,
    )
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import canonical_sha256

    transport = _Transport(
        result=_result(request_sha256=canonical_sha256(_request())),
        receipt=_receipt(**receipt_change),
    )
    client = WindowsDaemonClient(
        transport=transport,
        capability=bytearray(b"e" * 32),
        correlation=lambda: "1" * 32,
        qpc_frequency=lambda: 10_000,
        qpc_counter=lambda: 500_000,
    )
    with pytest.raises(DaemonClientFailure):
        client.invoke(entry_id="fixture", request=_request())


def test_t05_red_deadline_and_redaction_are_fail_closed() -> None:
    from mcphub_em_mcp.cst_saved_field_daemon_client_windows import (
        WindowsDaemonClient,
    )
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import canonical_sha256

    client = WindowsDaemonClient(
        transport=_Transport(
            result=_result(deadline=600_000, request_sha256=canonical_sha256(_request())),
            receipt=_receipt(),
        ),
        capability=bytearray(b"f" * 32),
        correlation=lambda: "1" * 32,
        qpc_frequency=lambda: 10_000,
        qpc_counter=lambda: 600_000,
    )
    with pytest.raises(Exception) as raised:
        client.invoke(entry_id="fixture", request=_request())
    assert "CANARY" not in str(raised.value)
    assert "C:\\" not in str(raised.value)


def test_t05_red_daemon_failure_id_cannot_carry_raw_text() -> None:
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import (
        FrontendDaemonResultV1,
        FrontendProtocolFailure,
    )
    from mcphub_em_mcp.cst_saved_field_port import AbsoluteInvocationBudget

    with pytest.raises(FrontendProtocolFailure):
        FrontendDaemonResultV1(
            correlation_id="1" * 32,
            entry_id="fixture",
            request_sha256="3" * 64,
            budget=AbsoluteInvocationBudget(10_000, 400_000, 1_000_000),
            ok=False,
            text=None,
            failure_id=r"cst_saved_field.CANARY_C:\\secret_S-1-5-18",
        )


def test_t05_red_cst_frontend_imports_daemon_only_and_six_names_stay_exact() -> None:
    from mcphub_em_mcp import cst, hfss

    source_path = Path(cst.__file__)
    tree = ast.parse(source_path.read_text(encoding="utf-8"))
    imports = {
        node.module.rsplit(".", 1)[-1]
        for node in ast.walk(tree)
        if isinstance(node, ast.ImportFrom) and node.module
    }
    assert "cst_saved_field_daemon_client_windows" in imports
    assert not (
        {
            "cst_saved_field_broker_client_windows",
            "cst_saved_field_broker_protocol",
            "cst_saved_field_broker_worker",
            "cst_saved_field_containment_windows",
            "cst_saved_field_vendor",
        }
        & imports
    )
    assert set(cst.mcp._tool_manager._tools) == {"cst_solve", "cst_export_mesh", "cst_export_results"}
    assert set(hfss.mcp._tool_manager._tools) == {
        "hfss_solve",
        "hfss_export_mesh",
        "hfss_export_sparams",
    }


def test_t05_red_daemon_wire_corpus_is_bounded_canonical_and_closed() -> None:
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import (
        FrontendProtocolFailure,
        decode_frame,
        encode_frame,
    )

    frame = encode_frame({"a": 1}, maximum=7)
    assert decode_frame(frame, maximum=7) == {"a": 1}
    with pytest.raises(FrontendProtocolFailure):
        decode_frame(b'{"a":1,"a":2}', maximum=20)
    with pytest.raises(FrontendProtocolFailure):
        decode_frame(json.dumps({"a": 1}).encode(), maximum=20)
