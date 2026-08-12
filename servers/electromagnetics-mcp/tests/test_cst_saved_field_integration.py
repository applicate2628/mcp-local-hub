from __future__ import annotations

import json
import threading
from dataclasses import replace

import pytest

from mcphub_em_mcp import cst_saved_field as core
from mcphub_em_mcp.cst_saved_field_port import (
    AcquisitionSettlement,
    ApplicationSettlement,
    FieldFrameCandidate,
    OwnedSessionAcquisitionError,
    PreparedSavedFieldSource,
    VendorFailure,
    VendorSampleBatch,
)


def _request() -> core.SavedFieldRequestV1:
    return core.SavedFieldRequestV1.model_validate(
        {
            "project_bundle": r"C:\approved\model.cst",
            "expected_project_sha256": "1" * 64,
            "field": "E",
            "result": {
                "port": 1,
                "mode": 1,
                "frequency_hz": 3e9,
                "frequency_tolerance_hz": 0.0,
                "frame_selector": "frame-e1",
                "expected_field_sha256": "3" * 64,
                "expected_mesh_sha256": "2" * 64,
                "adaptive_pass": "pass-4",
            },
            "points": [
                {"id": "p1", "xyz": [1.0, 2.0, 3.0]},
                {"id": "p2", "xyz": [4.0, 5.0, 6.0]},
            ],
            "coordinate_unit": "mm",
            "allow_solve": False,
            "max_points": 2,
        }
    )


def _candidate() -> FieldFrameCandidate:
    return FieldFrameCandidate(
        field="E",
        port=1,
        mode=1,
        frequency_hz=3e9,
        frame_id="frame-e1",
        tree_path="2D/3D Results/E-Field/e1",
        payload_relative="Result/saved/e1.sct",
        adaptive_pass="pass-4",
        project_unit="mm",
        field_unit="V/m",
        time_dependence="exp(+jwt)",
        time_dependence_status="verified",
        field_sha256="3" * 64,
        initial_frequency_hz=3e9,
        post_registration_frequency_hz=3e9,
        activation_type="Efield3D",
        status_policy={-1: True},
    )


_ACQUIRED = AcquisitionSettlement(
    stage="session_transfer",
    transfer_committed=True,
    handles_received=1,
    close_attempts=0,
    close_succeeded=False,
    absence_proven=False,
    safely_attributed_remaining=0,
)


class _Runtime:
    def __init__(self) -> None:
        self.trace: list[str] = []
        self.events: list[tuple[str, object]] = []
        self.source_unchanged = True
        self.source_changed_role = None

    def prepare_source(self, request):
        self.trace.append("prepare")
        assert request.project_bundle == r"C:\approved\model.cst"
        return PreparedSavedFieldSource(
            capability=object(),
            candidates=(_candidate(),),
            project_relative="model.cst",
            project_sha256="1" * 64,
            mesh_relative="model/Result/3d.slim",
            mesh_sha256="2" * 64,
        )

    def open_owned_session(self, source):
        self.trace.append("open")
        assert source.capability is not None
        return object(), _ACQUIRED

    def activate_and_sample(self, owned, source, frame, points_project, tolerance):
        self.trace.append("activate")
        assert owned is not None and source.capability is not None
        assert frame.frame_id == "frame-e1"
        assert points_project == ((1.0, 2.0, 3.0), (4.0, 5.0, 6.0))
        assert tolerance == 0.0
        return VendorSampleBatch(
            rows=(
                ((1.0, 2.0, 3.0, 4.0, 5.0, 6.0), -1),
                ((0.0, 0.0, 0.0, 0.0, 0.0, 0.0), -1),
            ),
            metadata={
                "project_unit": "mm",
                "field_unit": "V/m",
                "time_dependence": "exp(+jwt)",
                "time_dependence_status": "verified",
            },
            initial_frequency_hz=3e9,
            post_registration_frequency_hz=3e9,
            activation_type="Efield3D",
            generated_header=True,
        )

    def product_version(self, owned):
        self.trace.append("version")
        assert owned is not None
        return "CST Studio Suite 2026"

    def settle(self, source, owned, acquisition, causal_failure):
        self.trace.append("settle")
        return ApplicationSettlement(
            workspace_settled=True,
            session_settled=True,
            source_unchanged=self.source_unchanged,
            owned_remaining=0,
            cache_cleared=owned is not None,
            closed_without_save=owned is not None,
            acquisition=acquisition,
            source_changed_role=self.source_changed_role,
        )

    def emit(self, event, fields):
        self.events.append((event, fields))


def test_saved_field_restart_replay() -> None:
    first_runtime = _Runtime()
    second_runtime = _Runtime()
    first = core.sample_saved_field(_request(), first_runtime)
    second = core.sample_saved_field(_request(), second_runtime)
    assert first == second
    assert [point["id"] for point in first["points"]] == ["p1", "p2"]
    assert first["points"][1]["zero_ambiguous"] is True
    assert first_runtime.trace == ["prepare", "open", "activate", "version", "settle"]
    assert len(first_runtime.events) == 1
    assert first_runtime.events[0][1]["acquisition"] == {
        field: getattr(_ACQUIRED, field) for field in _ACQUIRED.__dataclass_fields__
    }


def test_saved_field_cleanup_all_paths() -> None:
    for stage in ("activate", "session_identity", "session_transfer", "post_transfer"):
        runtime = _Runtime()
        receipt = replace(
            _ACQUIRED,
            stage=stage,
            transfer_committed=False,
            close_attempts=1,
            close_succeeded=True,
            absence_proven=True,
        )

        def fail(_source, receipt=receipt, stage=stage):
            raise OwnedSessionAcquisitionError(
                VendorFailure(
                    "cst_saved_field.session_ownership_ambiguous",
                    stage,
                    "fixed safe error",
                ),
                receipt,
            )

        runtime.open_owned_session = fail
        with pytest.raises(core.SavedFieldFailure) as raised:
            core.sample_saved_field(_request(), runtime)
        assert raised.value.failure_id == "cst_saved_field.session_ownership_ambiguous"
        assert runtime.events[0][1]["acquisition"] == {
            field: getattr(receipt, field) for field in receipt.__dataclass_fields__
        }
        assert runtime.trace == ["prepare", "settle"]


@pytest.mark.parametrize("role", ["project", "mesh", "field"])
def test_p07_post_snapshot_source_mutation_is_source_changed(role: str) -> None:
    runtime = _Runtime()
    runtime.source_unchanged = False
    runtime.source_changed_role = role
    with pytest.raises(core.SavedFieldFailure) as raised:
        core.sample_saved_field(_request(), runtime)
    assert raised.value.failure_id == "cst_saved_field.source_changed"
    assert raised.value.stage == f"source_post_hash:{role}"
    assert runtime.trace[-1] == "settle"
    assert len(runtime.events) == 1


def test_saved_field_settlement_blocked_window() -> None:
    entered = threading.Event()
    release = threading.Event()
    runtime = _Runtime()
    original = runtime.activate_and_sample

    def blocked(*args, **kwargs):
        entered.set()
        assert release.wait(2.0)
        return original(*args, **kwargs)

    runtime.activate_and_sample = blocked  # type: ignore[method-assign]
    result: list[object] = []
    worker = threading.Thread(target=lambda: result.append(core.sample_saved_field(_request(), runtime)))
    worker.start()
    assert entered.wait(1.0)
    assert "settle" not in runtime.trace
    assert runtime.events == []
    release.set()
    worker.join(2.0)
    assert not worker.is_alive()
    assert len(result) == 1
    assert runtime.trace[-1] == "settle"
    assert len(runtime.events) == 1


@pytest.mark.asyncio
async def test_named_daemon_broker_worker_integration_has_one_return_route() -> None:
    from io import BytesIO

    from mcphub_em_mcp import cst
    from mcphub_em_mcp.cst_saved_field_broker_client_windows import (
        BrokerCancelReceiptV1,
        BrokerStartupProofV1,
    )
    from mcphub_em_mcp.cst_saved_field_broker_protocol import decode_one_frame, encode_frame
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import (
        BrokerPeerIdentityV1,
        BrokerRuntimeServiceV1,
        ContainedWorkerBrokerApplicationV1,
    )
    from mcphub_em_mcp.cst_saved_field_broker_worker import BrokerWorkerApplication
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        BROKER_WORKER_REQUEST_MAX,
        BrokerWorkerRequestV1,
        WorkerSettlementV1,
    )
    from mcphub_em_mcp.cst_saved_field_daemon_client_windows import WindowsDaemonClient
    from mcphub_em_mcp.cst_saved_field_daemon_service_windows import WindowsCstDaemonService
    from mcphub_em_mcp.cst_saved_field_frontend_protocol import (
        DaemonResponseReceiptV1,
        FrontendTransportReceiptV1,
    )
    from mcphub_em_mcp.cst_saved_field_policy import (
        AuthorityEntry,
        AuthoritySnapshot,
        RootIdentityV1,
    )
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import (
        BROKER_ACCOUNT,
        BROKER_SERVICE,
        VendorIsolationProofV1,
    )
    from mcphub_em_mcp.strict_fastmcp import strict_fastmcp

    trace: list[str] = []
    entry = AuthorityEntry(
        "line10-e",
        r"C:\approved",
        RootIdentityV1(1, "1" * 32),
        "model.cst",
        "2" * 64,
        "3" * 64,
        "4" * 64,
    )
    snapshot = AuthoritySnapshot(
        "5" * 64,
        (entry,),
        {r"C:\approved\model.cst": entry},
    )

    class Transaction:
        def execute(self, request):
            trace.append("worker:source-transfer-vendor-settlement")
            assert "project_bundle" not in request.request
            return (
                json.dumps(
                    {"schema": "mcphub.cst.saved_field_sample.v1", "points": []},
                    sort_keys=True,
                    separators=(",", ":"),
                ),
                None,
                WorkerSettlementV1(True, True, True, True, True, True, 0),
            )

    worker = BrokerWorkerApplication(
        authorize=lambda request: trace.append("worker:authorize-policy-entry"),
        transaction=Transaction(),
        qpc_frequency=lambda: 100,
        qpc_counter=lambda: 11,
    )

    class ContainedInvocation:
        def invoke(self, request_frame, *, deadline):
            trace.append("broker:worker-start")
            request = BrokerWorkerRequestV1.from_wire(
                decode_one_frame(BytesIO(request_frame), maximum=BROKER_WORKER_REQUEST_MAX)
            )
            assert request.deadline == deadline
            response = worker(request)
            trace.append("broker:worker-settled")
            return encode_frame(response.to_wire())

    application = ContainedWorkerBrokerApplicationV1(
        invocation=ContainedInvocation(),  # type: ignore[arg-type]
        vendor_isolation=VendorIsolationProofV1(
            BROKER_SERVICE,
            BROKER_ACCOUNT,
            BROKER_ACCOUNT,
            True,
            True,
            0,
        ),
    )
    daemon_sid = "S-1-5-80-101"
    daemon_image = r"C:\Program Files\mcp-local-hub\cst-daemon.exe"
    peer = BrokerPeerIdentityV1(
        101,
        daemon_sid,
        daemon_sid,
        True,
        True,
        0,
        True,
        True,
        daemon_image,
    )
    broker = BrokerRuntimeServiceV1(
        snapshot=snapshot,
        daemon_service_sid=daemon_sid,
        daemon_image=daemon_image,
        workspace_policy=object(),
        application=application,
        qpc_frequency=lambda: 100,
        qpc_counter=iter((11, 12)).__next__,
        random_bytes=lambda count: b"n" * count,
    )

    class BrokerTransport:
        def startup_proof(self):
            return BrokerStartupProofV1(True, True, True, True, True)

        def challenge(self, deadline):
            trace.append("broker:challenge")
            return broker.issue_challenge(peer, deadline)

        def exchange(self, request):
            trace.append("broker:authorize-policy-nonce")
            response = broker.exchange(peer, request)
            trace.append("broker:response-settled")
            return response

        def cancel_and_settle(self, correlation_id, deadline):
            del correlation_id, deadline
            return BrokerCancelReceiptV1(True, True, True, 0)

    transport = BrokerTransport()

    class Enrollment:
        def consume_frontend(self, correlation, capability, **proof):
            trace.append("enrollment:capability-consumed")
            return correlation == "7" * 32 and capability == b"c" * 32 and all(proof.values())

        def shutdown(self):
            trace.append("enrollment:shutdown")

    daemon = WindowsCstDaemonService(
        snapshot=snapshot,
        enrollment=Enrollment(),
        broker_transport=transport,
        qpc_frequency=lambda: 100,
        qpc_counter=iter((10, 13)).__next__,
        broker_correlation=lambda: "6" * 32,
        random_bytes=lambda count: b"d" * count,
        event_sink=lambda receipt: trace.append(f"daemon:admission-{receipt.disposition}"),
    )

    class FrontendTransport:
        def startup_proof(self, timeout):
            return timeout == 5.0

        def challenge(self, correlation, timeout):
            assert timeout == 5.0
            return daemon.issue_challenge(correlation)

        def exchange(self, request, timeout):
            assert timeout == 5.0

            def write_response(result):
                trace.append("daemon:response-write-flush-ack-close")
                return DaemonResponseReceiptV1(result.correlation_id, True, True, True, True, True, True)

            result, daemon_receipt = daemon.exchange(
                request,
                exact_capability_eof=True,
                response_writer=write_response,
            )
            assert daemon_receipt.complete
            trace.append("frontend:response-read-eof-close")
            return result, FrontendTransportReceiptV1(result.correlation_id, True, True, True, True)

        def cancel(self, correlation, timeout):
            return timeout == 5.0 and daemon.challenge_state(correlation) != "ISSUED"

    client = WindowsDaemonClient(
        transport=FrontendTransport(),
        capability=bytearray(b"c" * 32),
        correlation=lambda: "7" * 32,
        qpc_frequency=lambda: 100,
        qpc_counter=lambda: 10,
    )
    server = strict_fastmcp("broker-integration")
    assert cst._compose_saved_field_tool(server, snapshot, client) is True
    result = await server.call_tool(
        "cst_sample_saved_field",
        {
            "project_bundle": r"C:\approved\model.cst",
            "expected_project_sha256": None,
            "field": "E",
            "result": {
                "port": 1,
                "mode": 1,
                "frequency_hz": 3e9,
                "frequency_tolerance_hz": 0.0,
                "frame_selector": None,
                "expected_field_sha256": None,
                "expected_mesh_sha256": None,
                "adaptive_pass": None,
            },
            "points": [{"id": "p1", "xyz": [0.0, 0.0, 0.0]}],
            "coordinate_unit": "mm",
            "allow_solve": False,
            "max_points": 1,
        },
    )
    assert result.isError is False and result.structuredContent is None and len(result.content) == 1
    assert trace == [
        "enrollment:capability-consumed",
        "broker:challenge",
        "broker:authorize-policy-nonce",
        "broker:worker-start",
        "worker:authorize-policy-entry",
        "worker:source-transfer-vendor-settlement",
        "broker:worker-settled",
        "broker:response-settled",
        "daemon:response-write-flush-ack-close",
        "daemon:admission-released",
        "frontend:response-read-eof-close",
    ]
