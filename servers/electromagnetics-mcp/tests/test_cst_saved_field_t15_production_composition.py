from __future__ import annotations

import ast
import json
from io import BytesIO
from pathlib import Path

import pytest


@pytest.mark.parametrize(
    "missing",
    [
        "worker_signaled",
        "exit_recorded",
        "process_reference_closed",
        "active_job_zero",
        "readers_joined",
        "handles_closed",
        "pipe_closed",
    ],
)
def test_containment_receipt_is_the_only_broker_settlement_source(missing: str) -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerProtocolFailure,
        BrokerRequestV1,
        QpcDeadlineV1,
        canonical_sha256,
    )
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import (
        ContainedWorkerBrokerApplicationV1,
    )
    from mcphub_em_mcp.cst_saved_field_containment_windows import (
        SOURCE_ROOT_ACCESS,
        SOURCE_ROOT_SHARE,
        WORKSPACE_ROOT_ACCESS,
        WORKSPACE_ROOT_SHARE,
        BrokerCapabilityReceiptV1,
        ContainedInvocationReceiptV1,
        WorkerCapabilitySetV1,
    )
    from mcphub_em_mcp.cst_saved_field_policy import AuthorityEntry, RootIdentityV1
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import (
        BROKER_ACCOUNT,
        BROKER_SERVICE,
        VendorIsolationProofV1,
    )

    class Invocation:
        def invoke(self, request_frame, *, deadline, correlation_id, capabilities):
            del request_frame, deadline
            assert correlation_id == request.correlation_id
            assert capabilities.receipt is not None and capabilities.receipt.complete
            facts = {
                "worker_signaled": True,
                "exit_recorded": True,
                "process_reference_closed": True,
                "active_job_zero": True,
                "readers_joined": True,
                "handles_closed": True,
                "pipe_closed": True,
            }
            facts[missing] = False
            return ContainedInvocationReceiptV1(response_frame=b"{}", **facts)

    source_identity = {"volume_serial": 1, "file_id": "1" * 32}
    workspace_identity = {"volume_serial": 2, "file_id": "2" * 32}

    class Workspace:
        capability = type(
            "Capability",
            (),
            {
                "restricted": True,
                "path": r"C:\workspace",
                "evidence": type("Evidence", (), workspace_identity)(),
            },
        )()

        def settle(self):
            return None

    class WorkspacePolicy:
        def create_child(self, correlation_id):
            assert correlation_id == request.correlation_id
            return Workspace()

    deadline = QpcDeadlineV1(100, 10, 6010)
    request = BrokerRequestV1(
        "a" * 32,
        "b" * 64,
        "c" * 64,
        "entry",
        "e" * 64,
        canonical_sha256({}),
        {},
        deadline,
    )
    capability_receipt = BrokerCapabilityReceiptV1(
        request.correlation_id,
        SOURCE_ROOT_ACCESS,
        SOURCE_ROOT_SHARE,
        WORKSPACE_ROOT_ACCESS,
        WORKSPACE_ROOT_SHARE,
        source_identity,
        workspace_identity,
        SOURCE_ROOT_ACCESS,
        WORKSPACE_ROOT_ACCESS,
        True,
        True,
        True,
        ("Directory", "Directory"),
    )
    application = ContainedWorkerBrokerApplicationV1(
        invocation=Invocation(),  # type: ignore[arg-type]
        vendor_isolation=VendorIsolationProofV1(
            service_name=BROKER_SERVICE,
            token_user=BROKER_ACCOUNT,
            workspace_owner=BROKER_ACCOUNT,
            protected_dacl=True,
            daemon_access_denied=True,
            session_id=0,
        ),
        capability_opener=lambda *_args, **_kwargs: WorkerCapabilitySetV1(
            41,
            42,
            SOURCE_ROOT_ACCESS,
            WORKSPACE_ROOT_ACCESS,
            source_identity,
            workspace_identity,
            _owner=type(
                "Owner",
                (),
                {"handles": {41, 42}, "close_all": lambda self: self.handles.clear()},
            )(),
            receipt=capability_receipt,
        ),
    )
    with pytest.raises(BrokerProtocolFailure, match="containment_settle_failed"):
        application(
            request,
            AuthorityEntry(
                "entry",
                r"C:\approved",
                RootIdentityV1(1, "1" * 32),
                "model.cst",
                "2" * 64,
                "3" * 64,
                "e" * 64,
            ),
            WorkspacePolicy(),
        )


def test_production_roots_require_provisioned_owner_compositions() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_service_windows import BrokerServiceCompositionV1
    from mcphub_em_mcp.cst_saved_field_broker_worker import WorkerCompositionV1
    from mcphub_em_mcp.cst_saved_field_daemon_service_windows import DaemonServiceCompositionV1

    assert BrokerServiceCompositionV1.__dataclass_fields__["serve"].type
    assert WorkerCompositionV1.__dataclass_fields__["project_bundle"].type
    assert WorkerCompositionV1.__dataclass_fields__["application_port"].type
    assert DaemonServiceCompositionV1.__dataclass_fields__["serve"].type


def test_worker_main_builds_real_application_from_owner_dependencies(monkeypatch) -> None:
    from mcphub_em_mcp import cst_saved_field_broker_worker as module

    calls: list[str] = []

    def run_worker(source, destination, application, **options):
        assert isinstance(application, module.BrokerWorkerApplication)
        assert source is composition.source and destination is composition.destination
        assert options == {
            "bootstrap": composition.bootstrap,
            "pre_main_receipt": composition.pre_main_receipt,
        }
        calls.append("run")
        return 0

    monkeypatch.setattr(module, "run_worker", run_worker)
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        WorkerPreMainBootstrapV1,
        WorkerPreMainReceiptV1,
    )

    deadline = QpcDeadlineV1(100, 10, 6010)
    source_identity = {"volume_serial": 1, "file_id": "1" * 32}
    workspace_identity = {"volume_serial": 2, "file_id": "2" * 32}
    bootstrap = WorkerPreMainBootstrapV1(
        "a" * 32,
        deadline,
        41,
        42,
        0x00120089,
        0x0012019F,
        source_identity,
        workspace_identity,
    )
    pre_main_receipt = WorkerPreMainReceiptV1(
        bootstrap.correlation_id,
        deadline,
        True,
        True,
        False,
        bootstrap.source_access_mask,
        bootstrap.workspace_access_mask,
        source_identity,
        workspace_identity,
        bootstrap.checksum,
    )
    composition = module.WorkerCompositionV1(
        authorize=lambda request: calls.append(request.entry_id),
        project_bundle=r"C:\approved\model.cst",
        application_port=object(),
        qpc_frequency=lambda: 100,
        qpc_counter=lambda: 10,
        source=BytesIO(),
        destination=BytesIO(),
        bootstrap=bootstrap,
        pre_main_receipt=pre_main_receipt,
    )
    monkeypatch.setattr(module, "compose_default_off_runtime", lambda: composition)
    assert module.main() == 0
    assert calls == ["run"]


def test_worker_transaction_owns_application_and_settlement() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1, canonical_sha256
    from mcphub_em_mcp.cst_saved_field_broker_worker import SavedFieldWorkerTransactionV1
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        BrokerWorkerRequestV1,
        WorkerSettlementV1,
    )
    from mcphub_em_mcp.cst_saved_field_port import (
        AcquisitionSettlement,
        ApplicationSettlement,
        FieldFrameCandidate,
        PreparedSavedFieldSource,
        VendorSampleBatch,
    )

    trace: list[str] = []
    worker_settlement = WorkerSettlementV1(True, True, True, True, True, True, 0)

    class Port:
        def prepare_source(self, request):
            trace.append("source")
            return PreparedSavedFieldSource(
                object(),
                (
                    FieldFrameCandidate(
                        "E",
                        1,
                        1,
                        3e9,
                        "frame",
                        "tree",
                        "field.sct",
                        None,
                        "mm",
                        "V/m",
                        "exp(+jwt)",
                        "verified",
                        "f" * 64,
                        3e9,
                        3e9,
                        "Efield3D",
                        {0: True},
                    ),
                ),
                "model.cst",
                "a" * 64,
                "mesh.slim",
                "b" * 64,
            )

        def open_owned_session(self, source):
            del source
            trace.append("session")
            return object(), AcquisitionSettlement("transfer", True, 1, 0, True, True, 0)

        def activate_and_sample(self, owned, source, frame, points_project, tolerance):
            del owned, source, frame, points_project, tolerance
            trace.append("vendor")
            return VendorSampleBatch(
                (((1.0, 2.0, 3.0, 4.0, 5.0, 6.0), 0),),
                {
                    "project_unit": "mm",
                    "field_unit": "V/m",
                    "time_dependence": "exp(+jwt)",
                    "time_dependence_status": "verified",
                },
                3e9,
                3e9,
                "Efield3D",
                True,
            )

        def product_version(self, owned):
            del owned
            return "synthetic-adapter"

        def settle(self, source, owned, acquisition, causal_failure):
            del source, owned, causal_failure
            trace.append("settle")
            return ApplicationSettlement(True, True, True, 0, True, True, acquisition)

        def emit(self, event, fields):
            assert event == "cst_saved_field.session_settled"
            assert fields["owned_remaining"] == 0

        def worker_settlement(self):
            return worker_settlement

        def worker_capability_receipt(self, correlation_id):
            from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import WorkerCapabilityReceiptV1

            return WorkerCapabilityReceiptV1(correlation_id, True, True, True, False)

    body = {
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
    }
    request = BrokerWorkerRequestV1(
        "c" * 32,
        "d" * 64,
        "entry",
        "e" * 64,
        canonical_sha256(body),
        body,
        QpcDeadlineV1(100, 10, 6010),
    )
    text, failure_id, settlement, capability_receipt = SavedFieldWorkerTransactionV1(
        project_bundle=r"C:\approved\model.cst", application_port=Port()
    ).execute(request)
    assert failure_id is None and settlement is worker_settlement
    assert capability_receipt.complete
    assert json.loads(text or "")["schema"] == "mcphub.cst.saved_field_sample.v1"
    assert trace == ["source", "session", "vendor", "settle"]


def test_daemon_main_composes_owner_then_runs_listener(monkeypatch) -> None:
    from mcphub_em_mcp import cst_saved_field_daemon_service_windows as module

    trace: list[str] = []

    class Service:
        def shutdown(self):
            trace.append("shutdown")

    def from_policy(**owners):
        assert owners["raw_policy_path"] is None
        assert owners["broker_transport"] is broker_transport
        trace.append("compose")
        return Service()

    monkeypatch.setattr(module.WindowsCstDaemonService, "from_policy", from_policy)
    broker_transport = object()
    composition = module.DaemonServiceCompositionV1(
        None,
        object(),  # type: ignore[arg-type]
        object(),  # type: ignore[arg-type]
        broker_transport,  # type: ignore[arg-type]
        lambda: 100,
        lambda: 10,
        lambda: "a" * 32,
        lambda count: b"x" * count,
        lambda event: event,
        lambda service: trace.append("serve") or 17,
    )
    assert module._run_composed_service(composition) == 17
    assert trace == ["compose", "serve", "shutdown"]


def test_broker_main_composes_containment_owner_then_runs_listener(monkeypatch) -> None:
    from mcphub_em_mcp import cst_saved_field_broker_service_windows as module
    from mcphub_em_mcp.cst_saved_field_vendor_isolation_windows import (
        BROKER_ACCOUNT,
        BROKER_SERVICE,
        VendorIsolationProofV1,
    )

    trace: list[str] = []

    class Service:
        def shutdown(self):
            trace.append("shutdown")

    def from_policy(**owners):
        assert isinstance(owners["application"], module.ContainedWorkerBrokerApplicationV1)
        trace.append("compose")
        return Service()

    monkeypatch.setattr(module.BrokerRuntimeServiceV1, "from_policy", from_policy)
    composition = module.BrokerServiceCompositionV1(
        None,
        object(),  # type: ignore[arg-type]
        {},
        "S-1-5-80-101",
        "daemon.exe",
        "worker.exe",
        VendorIsolationProofV1(BROKER_SERVICE, BROKER_ACCOUNT, BROKER_ACCOUNT, True, True, 0),
        lambda: 100,
        lambda: 10,
        lambda service: trace.append("serve") or 19,
        kernel=object(),  # type: ignore[arg-type]
        random_bytes=lambda count: b"x" * count,
    )
    assert module._run_composed_service(composition) == 19
    assert trace == ["compose", "serve", "shutdown"]


def test_no_unavailable_transport_or_literal_broker_settlement_success() -> None:
    root = Path("src/mcphub_em_mcp")
    client = (root / "cst_saved_field_broker_client_windows.py").read_text(encoding="utf-8")
    assert "UnavailableBrokerTransport" not in client

    broker_path = root / "cst_saved_field_broker_service_windows.py"
    tree = ast.parse(broker_path.read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        if isinstance(node.func, ast.Name) and node.func.id == "BrokerSettlementV1":
            assert not any(isinstance(arg, ast.Constant) and arg.value is True for arg in node.args)
