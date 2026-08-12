from __future__ import annotations

import importlib.util
import io
import os
import sys
import threading
import time
from dataclasses import replace

import pytest


def _containment():
    name = "mcphub_em_mcp.cst_saved_field_containment_windows"
    assert importlib.util.find_spec(name) is not None, "Windows containment module is missing"
    return __import__(name, fromlist=["*"])


def _deadline(module):
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1

    frequency = module._default_qpc_frequency()
    admitted = module._default_qpc_counter()
    return QpcDeadlineV1(frequency, admitted, admitted + 60 * frequency)


def _evidence(module):
    identity = module.WorkerIdentityV1(
        4242,
        123456,
        "S-1-5-80-333",
        0,
        os.path.abspath(sys.executable),
        None,
        3131,
    )
    return module.KernelContainmentEvidenceV1(
        identity,
        identity,
        True,
        ("stdin", "stdout", "stderr"),
        module._SETTLEMENT_EVENTS,
        0,
    )


def test_saved_field_createprocess_tuple() -> None:
    module = _containment()
    spec = module.build_create_process_spec(sys.executable)
    resolved = str(module.Path(sys.executable).resolve(strict=True))
    assert spec.application_name == resolved
    assert spec.command_line == (f'"{resolved}" -I -s -E -m mcphub_em_mcp.cst_saved_field_broker_worker')
    assert spec.current_directory == str(module.Path(resolved).parent)
    assert spec.inherit_handles is True
    assert spec.startf_use_std_handles is True
    assert spec.handle_list_roles == ("stdin", "stdout", "stderr")
    assert spec.attribute_roles == ("job_list", "handle_list")
    assert spec.creation_flags == (
        module.EXTENDED_STARTUPINFO_PRESENT | module.CREATE_UNICODE_ENVIRONMENT | module.CREATE_NO_WINDOW
    )
    assert spec.shell is False
    assert spec.path_search is False
    assert spec.breakaway is False


@pytest.mark.parametrize(
    "field",
    [
        "application_name",
        "command_line",
        "current_directory",
        "inherit_handles",
        "startf_use_std_handles",
        "handle_list_roles",
        "attribute_roles",
        "creation_flags",
        "shell",
        "path_search",
        "breakaway",
    ],
)
def test_createprocess_tuple_mutation_is_rejected(field: str) -> None:
    module = _containment()
    spec = module.build_create_process_spec(sys.executable)
    value = getattr(spec, field)
    if isinstance(value, bool):
        invalid = not value
    elif isinstance(value, int):
        invalid = value ^ 1
    elif isinstance(value, tuple):
        invalid = (*value, "extra")
    else:
        invalid = f"{value}.decoy"
    with pytest.raises(module.ContainmentFailure):
        module.validate_create_process_spec(replace(spec, **{field: invalid}), sys.executable)


def test_admission_gate_one_active_one_waiter_and_quarantine_linearization() -> None:
    module = _containment()
    gate = module.SamplerAdmissionGate("a" * 64)
    first = gate.acquire_and_seal("a" * 64, wait_seconds=0.0)
    first.authorize_start()
    waiter_ready = threading.Event()
    waiter_done = threading.Event()
    result: list[str] = []

    def waiter() -> None:
        waiter_ready.set()
        try:
            lease = gate.acquire_and_seal("a" * 64, wait_seconds=1.0)
            lease.authorize_start()
        except module.AdmissionFailure as exc:
            result.append(exc.failure_id)
        finally:
            waiter_done.set()

    thread = threading.Thread(target=waiter)
    thread.start()
    assert waiter_ready.wait(1.0)
    assert gate.waiter_count == 1
    with pytest.raises(module.AdmissionFailure) as second_waiter:
        gate.acquire_and_seal("a" * 64, wait_seconds=0.01)
    assert second_waiter.value.failure_id == "cst_saved_field.resource_busy"

    gate.quarantine_and_release(first)
    assert waiter_done.wait(1.0)
    thread.join(1.0)
    assert result == ["cst_saved_field.containment_quarantined"]
    assert gate.active_count == 0
    assert gate.waiter_count == 0
    with pytest.raises(module.AdmissionFailure) as later:
        gate.acquire_and_seal("a" * 64, wait_seconds=0.0)
    assert later.value.failure_id == "cst_saved_field.containment_quarantined"


def test_admission_generation_drift_denies_before_work() -> None:
    module = _containment()
    gate = module.SamplerAdmissionGate("a" * 64)
    lease = gate.acquire_and_seal("a" * 64, wait_seconds=0.0)
    gate._test_only_advance_generation()
    with pytest.raises(module.AdmissionFailure) as raised:
        lease.authorize_start()
    assert raised.value.failure_id == "cst_saved_field.policy_revision_changed"
    gate.release(lease)


class _Kernel:
    def __init__(self, module, *, active_after_exit: int = 0, settle: bool = True) -> None:
        self.module = module
        self.active_after_exit = active_after_exit
        self.settle = settle
        self.trace: list[str] = []
        self.foreign_alive = True

    def invoke(
        self,
        spec,
        request_frame,
        *,
        startup_validator,
        startup_deadline: float,
        response_deadline: float,
        absolute_deadline: float,
        cleanup_deadline: float,
    ):
        assert startup_deadline <= absolute_deadline
        assert response_deadline == absolute_deadline - 2.0
        assert cleanup_deadline == absolute_deadline + 10.0
        self.trace.extend(["job_configured", "createprocess", "first_instruction_in_job"])
        assert spec.creation_flags & self.module.CREATE_NO_WINDOW
        proof = self.module.FirstInstructionProof(True, True, True, True, False, True)
        startup_validator(proof)
        request_frame = request_frame()
        if self.active_after_exit:
            self.trace.extend(["helper_signal", "exit_record", "process_handle_close", "query_active"])
            self.trace.extend(["terminate_job", "active_zero"] if self.settle else ["settlement_failed"])
            return self.module.KernelInvocationResult(
                response_frame=b"",
                worker_signaled=True,
                exit_recorded=True,
                process_reference_closed=True,
                active_zero=self.settle,
                readers_joined=self.settle,
                handles_closed=self.settle,
                residual_process=True,
                timed_out=False,
                containment_evidence=_evidence(self.module),
            )
        self.trace.extend(
            ["helper_signal", "exit_record", "process_handle_close", "query_active", "active_zero"]
        )
        return self.module.KernelInvocationResult(
            response_frame=request_frame,
            worker_signaled=True,
            exit_recorded=True,
            process_reference_closed=True,
            active_zero=True,
            readers_joined=True,
            handles_closed=True,
            residual_process=False,
            timed_out=False,
            first_instruction_proof=proof,
            containment_evidence=_evidence(self.module),
        )


def test_saved_field_normal_residual_routes_termination() -> None:
    module = _containment()
    kernel = _Kernel(module, active_after_exit=1)
    invocation = module.WindowsContainedInvocation(kernel=kernel, executable=sys.executable)
    with pytest.raises(module.ContainmentFailure) as raised:
        invocation.invoke(b"request", deadline=_deadline(module))
    assert raised.value.failure_id == "cst_saved_field.containment_residual_process"
    assert kernel.trace == [
        "job_configured",
        "createprocess",
        "first_instruction_in_job",
        "helper_signal",
        "exit_record",
        "process_handle_close",
        "query_active",
        "terminate_job",
        "active_zero",
    ]
    assert kernel.foreign_alive is True


def test_unproved_settlement_is_quarantine_worthy() -> None:
    module = _containment()
    kernel = _Kernel(module, active_after_exit=1, settle=False)
    invocation = module.WindowsContainedInvocation(kernel=kernel, executable=sys.executable)
    with pytest.raises(module.ContainmentFailure) as raised:
        invocation.invoke(b"request", deadline=_deadline(module))
    assert raised.value.failure_id == "cst_saved_field.containment_settle_failed"
    assert raised.value.quarantine is True
    assert kernel.foreign_alive is True


class _ResultKernel:
    def __init__(self, result) -> None:
        self.result = result

    def invoke(
        self,
        _spec,
        request_factory,
        *,
        startup_validator,
        startup_deadline,
        response_deadline,
        absolute_deadline,
        cleanup_deadline,
    ):
        assert startup_deadline <= absolute_deadline
        assert response_deadline == absolute_deadline - 2.0
        assert cleanup_deadline - absolute_deadline == 10.0
        module = _containment()
        proof = module.FirstInstructionProof(True, True, True, True, False, True)
        startup_validator(proof)
        request_factory()
        return replace(
            self.result,
            first_instruction_proof=self.result.first_instruction_proof or proof,
            containment_evidence=self.result.containment_evidence or _evidence(module),
        )


@pytest.mark.parametrize(
    ("changes", "failure_id", "quarantine"),
    [
        ({"stderr_overflow": True}, "cst_saved_field.broker_worker_protocol_invalid", False),
        ({"exit_code": 7}, "cst_saved_field.broker_worker_protocol_invalid", False),
        ({"exit_code": 0xC0000005}, "cst_saved_field.broker_worker_protocol_invalid", False),
        ({"timed_out": True}, "cst_saved_field.deadline_exceeded", False),
        (
            {"residual_process": True},
            "cst_saved_field.containment_residual_process",
            False,
        ),
        ({"readers_joined": False}, "cst_saved_field.containment_settle_failed", True),
        ({"worker_signaled": False}, "cst_saved_field.containment_settle_failed", True),
        ({"exit_recorded": False}, "cst_saved_field.containment_settle_failed", True),
        (
            {"process_reference_closed": False},
            "cst_saved_field.containment_settle_failed",
            True,
        ),
        ({"active_zero": False}, "cst_saved_field.containment_settle_failed", True),
        ({"handles_closed": False}, "cst_saved_field.containment_settle_failed", True),
    ],
)
def test_saved_field_timeout_settlement(
    changes: dict[str, object], failure_id: str, quarantine: bool
) -> None:
    module = _containment()
    baseline = module.KernelInvocationResult(
        response_frame=b"CANARY-PRIVATE-RESPONSE",
        worker_signaled=True,
        exit_recorded=True,
        process_reference_closed=True,
        active_zero=True,
        readers_joined=True,
        handles_closed=True,
        residual_process=False,
        timed_out=False,
    )
    invocation = module.WindowsContainedInvocation(
        kernel=_ResultKernel(replace(baseline, **changes)), executable=sys.executable
    )
    with pytest.raises(module.ContainmentFailure) as raised:
        invocation.invoke(b"request", deadline=_deadline(module))
    assert raised.value.failure_id == failure_id
    assert raised.value.quarantine is quarantine
    assert "CANARY" not in str(raised.value)


def test_saved_field_quarantine_all_routes() -> None:
    module = _containment()
    result = module.KernelInvocationResult(
        response_frame=b"",
        worker_signaled=True,
        exit_recorded=True,
        process_reference_closed=True,
        active_zero=False,
        readers_joined=True,
        handles_closed=False,
        residual_process=True,
        timed_out=False,
    )
    for route in ("frame", "factory"):
        gate = module.SamplerAdmissionGate("a" * 64)
        runner = module.ContainedSamplerRunner(
            gate=gate,
            invocation=module.WindowsContainedInvocation(
                kernel=_ResultKernel(result), executable=sys.executable
            ),
        )
        with pytest.raises(module.ContainmentFailure):
            if route == "frame":
                runner.invoke(
                    b"request",
                    deadline=_deadline(module),
                    revision="a" * 64,
                    wait_seconds=0.0,
                )
            else:
                runner.invoke_after_admission(
                    lambda _started: b"request",
                    deadline=_deadline(module),
                    revision="a" * 64,
                    wait_seconds=0.0,
                )
        assert gate.active_count == 0
        with pytest.raises(module.AdmissionFailure) as later:
            runner.invoke(
                b"later",
                deadline=_deadline(module),
                revision="a" * 64,
                wait_seconds=0.0,
            )
        assert later.value.failure_id == "cst_saved_field.containment_quarantined"


@pytest.mark.skipif(os.name != "nt", reason="safe synthetic Win32 containment probe")
def test_saved_field_worker_reference_order() -> None:
    module = _containment()
    from mcphub_em_mcp import cst_saved_field_broker_worker_protocol as protocol
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1, canonical_sha256

    body = {"field": "E", "synthetic_probe": True}
    request = protocol.BrokerWorkerRequestV1(
        correlation_id="0" * 32,
        policy_revision="a" * 64,
        entry_id="synthetic",
        manifest_sha256="b" * 64,
        request_sha256=canonical_sha256(body),
        request=body,
        deadline=QpcDeadlineV1(10, 0, 600),
    )
    request_frame = protocol.encode_frame(request.to_wire(), maximum=protocol.BROKER_WORKER_REQUEST_MAX)
    spec = module.build_create_process_spec(sys.executable)
    started = time.monotonic()
    result = module.CtypesWindowsKernel().invoke(
        spec,
        request_frame,
        startup_deadline=started + 5.0,
        response_deadline=started + 58.0,
        absolute_deadline=started + 60.0,
        cleanup_deadline=started + 70.0,
    )
    proof = result.first_instruction_proof
    assert proof is not None
    assert proof.exact_job is True
    assert proof.exactly_three_inherited_std_handles is True
    assert proof.no_console is True
    assert proof.escaped_process_settled is True
    assert result.stderr_overflow is False
    assert b"CANARY" not in result.response_frame
    if proof.breakaway_created:
        assert proof.breakaway_denied is False
        with pytest.raises(module.ContainmentFailure) as raised:
            module.WindowsContainedInvocation._validate_startup(proof)  # noqa: SLF001
        assert raised.value.quarantine is True
        assert result.exit_code == 78
        assert result.response_frame == b""
        return
    assert proof.breakaway_denied is True
    module.WindowsContainedInvocation._validate_startup(proof)  # noqa: SLF001
    response = protocol.decode_one_frame(
        io.BytesIO(result.response_frame), maximum=protocol.BROKER_WORKER_RESPONSE_MAX
    )
    assert response["failure_id"] == "cst_saved_field.cst_unavailable"
    assert response["settlement"]["owned_remaining"] == 0


def test_saved_field_reader_cancellation() -> None:
    module = _containment()
    blocked = threading.Event()
    cancelled: list[int | None] = []

    def operation() -> None:
        blocked.wait()

    def cancel(thread: threading.Thread) -> None:
        cancelled.append(thread.native_id)
        blocked.set()

    worker = module._BoundedIoWorker(operation, cancel)
    worker.start()
    started = time.monotonic()
    assert worker.settle(started + 0.02, started + 0.2) is True
    assert time.monotonic() - started < 0.2
    assert worker.cancelled is True
    assert len(cancelled) == 1 and cancelled[0] is not None


def test_saved_field_shutdown_and_restart() -> None:
    module = _containment()
    first = module.SamplerAdmissionGate("a" * 64)
    lease = first.acquire_and_seal("a" * 64, wait_seconds=0.0)
    first.quarantine_and_release(lease)
    with pytest.raises(module.AdmissionFailure):
        first.acquire_and_seal("a" * 64, wait_seconds=0.0)
    restarted = module.SamplerAdmissionGate("b" * 64)
    fresh = restarted.acquire_and_seal("b" * 64, wait_seconds=0.0)
    restarted.release(fresh)
    assert restarted.active_count == 0
