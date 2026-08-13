from __future__ import annotations

import inspect
import sys
from dataclasses import replace

import pytest


def _identity(module):
    return module.WorkerIdentityV1(
        pid=4242,
        creation_time_100ns=123456,
        token_user_sid="S-1-5-80-333",
        session_id=0,
        image_path=sys.executable,
        package_identity=None,
        parent_pid=3131,
    )


def _proof(module):
    return module.FirstInstructionProof(True, True, True, True, False, True)


def _evidence(module):
    identity = _identity(module)
    return module.KernelContainmentEvidenceV1(
        created_worker=identity,
        pre_request_worker=identity,
        job_member_before_request=True,
        inherited_handle_roles=module.WORKER_HANDLE_ROLES,
        settlement_events=(
            "job_configured",
            "attributes_bound",
            "process_created",
            "identity_bound",
            "job_membership_verified",
            "capability_handles_revoked",
            "bootstrap_written",
            "pre_main_receipt_validated",
            "request_started",
            "worker_signaled",
            "exit_recorded",
            "process_reference_closed",
            "job_active_zero",
            "readers_joined",
            "handles_closed",
        ),
        foreign_process_operations=0,
    )


def _capabilities(module):
    return module.WorkerCapabilitySetV1(
        101,
        202,
        0x120089,
        0x12019F,
        {"volume_serial": 11, "file_id": "source-id"},
        {"volume_serial": 22, "file_id": "workspace-id"},
    )


def _receipt(module, bootstrap):
    return module.WorkerPreMainReceiptV1(
        bootstrap.correlation_id,
        bootstrap.deadline,
        True,
        True,
        False,
        bootstrap.source_access_mask,
        bootstrap.workspace_access_mask,
        bootstrap.source_root_identity,
        bootstrap.workspace_root_identity,
        bootstrap.checksum,
    )


class _Kernel:
    def __init__(self, module, result) -> None:
        self.module = module
        self.result = result
        self.deadlines = None

    def invoke(
        self,
        spec,
        request_factory,
        *,
        bootstrap,
        capabilities,
        startup_validator,
        startup_deadline,
        response_deadline,
        absolute_deadline,
        cleanup_deadline,
    ):
        assert spec.attribute_roles == ("job_list", "handle_list")
        assert spec.handle_list_roles == self.module.WORKER_HANDLE_ROLES
        self.deadlines = (
            startup_deadline,
            response_deadline,
            absolute_deadline,
            cleanup_deadline,
        )
        startup_validator(_proof(self.module))
        request_factory()
        return replace(
            self.result,
            pre_main_receipt=_receipt(self.module, bootstrap),
        )


def test_t08_red_atomic_job_and_handle_list_precede_request_without_suspended_gap() -> None:
    from mcphub_em_mcp import cst_saved_field_containment_windows as module

    spec = module.build_create_process_spec(sys.executable)
    assert spec.creation_flags == (
        module.EXTENDED_STARTUPINFO_PRESENT | module.CREATE_UNICODE_ENVIRONMENT | module.CREATE_NO_WINDOW
    )
    assert spec.attribute_roles == ("job_list", "handle_list")
    assert spec.handle_list_roles == module.WORKER_HANDLE_ROLES
    source = inspect.getsource(module._invoke_atomic_job_process)
    order = tuple(
        source.index(marker)
        for marker in (
            "PROC_THREAD_ATTRIBUTE_JOB_LIST",
            "PROC_THREAD_ATTRIBUTE_HANDLE_LIST",
            "CreateProcessW(",
            "IsProcessInJob(",
            "def build_request",
        )
    )
    assert order == tuple(sorted(order))
    for forbidden in ("CREATE_SUSPENDED", "AssignProcessToJobObject", "ResumeThread"):
        assert forbidden not in source


def test_t08_red_qpc_deadline_is_injected_unchanged_and_cleanup_is_separate() -> None:
    from mcphub_em_mcp import cst_saved_field_containment_windows as module
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1

    result = module.KernelInvocationResult(
        response_frame=b"ok",
        worker_signaled=True,
        exit_recorded=True,
        process_reference_closed=True,
        active_zero=True,
        readers_joined=True,
        handles_closed=True,
        pipe_closed=True,
        residual_process=False,
        timed_out=False,
        first_instruction_proof=_proof(module),
        containment_evidence=_evidence(module),
    )
    kernel = _Kernel(module, result)
    invocation = module.WindowsContainedInvocation(
        kernel=kernel,
        executable=sys.executable,
        qpc_frequency=lambda: 10,
        qpc_counter=lambda: 110,
        monotonic=lambda: 50.0,
    )
    deadline = QpcDeadlineV1(10, 100, 700)
    assert (
        invocation.invoke(
            b"request",
            deadline=deadline,
            correlation_id="1" * 32,
            capabilities=_capabilities(module),
        ).response_frame
        == b"ok"
    )
    assert kernel.deadlines == (55.0, 107.0, 109.0, 119.0)


@pytest.mark.parametrize(
    "mutation",
    (
        {"pre_request_worker": "identity"},
        {"job_member_before_request": False},
        {"inherited_handle_roles": ("stdin", "stdout", "stderr", "job")},
        {"foreign_process_operations": 1},
        {"settlement_events": ("job_configured", "request_started")},
    ),
)
def test_t08_red_identity_membership_handles_foreign_and_settlement_fail_closed(mutation) -> None:
    from mcphub_em_mcp import cst_saved_field_containment_windows as module

    evidence = _evidence(module)
    if mutation.get("pre_request_worker") == "identity":
        mutation = {"pre_request_worker": replace(evidence.created_worker, parent_pid=9999)}
    result = module.KernelInvocationResult(
        response_frame=b"CANARY",
        worker_signaled=True,
        exit_recorded=True,
        process_reference_closed=True,
        active_zero=True,
        readers_joined=True,
        handles_closed=True,
        pipe_closed=True,
        residual_process=False,
        timed_out=False,
        first_instruction_proof=_proof(module),
        containment_evidence=replace(evidence, **mutation),
    )
    with pytest.raises(module.ContainmentFailure) as raised:
        capabilities = _capabilities(module)
        bootstrap = capabilities.bootstrap("1" * 32, module.QpcDeadlineV1(10, 100, 700))
        result = replace(result, pre_main_receipt=_receipt(module, bootstrap))
        module.WindowsContainedInvocation._validate_result(result, bootstrap=bootstrap)
    assert raised.value.quarantine is True
    assert "CANARY" not in str(raised.value)
