"""Fixed broker-owned worker entry point for one saved-field invocation."""

from __future__ import annotations

import os
import sys
from collections.abc import Callable
from typing import BinaryIO, Protocol

from .cst_saved_field_broker_protocol import decode_one_frame, encode_frame
from .cst_saved_field_broker_worker_protocol import (
    BROKER_WORKER_REQUEST_MAX,
    BROKER_WORKER_RESPONSE_MAX,
    BrokerWorkerRequestV1,
    BrokerWorkerResponseV1,
    WorkerSettlementV1,
    WorkerStartupProofV1,
    encode_startup_proof_frame,
)

WorkerApplication = Callable[[BrokerWorkerRequestV1], BrokerWorkerResponseV1]


class WorkerTransactionPort(Protocol):
    def execute(
        self, request: BrokerWorkerRequestV1
    ) -> tuple[str | None, str | None, WorkerSettlementV1]: ...


class BrokerWorkerApplication:
    """Authorize one broker request and execute its single settlement-owning transaction."""

    def __init__(
        self,
        *,
        authorize: Callable[[BrokerWorkerRequestV1], None],
        transaction: WorkerTransactionPort,
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
    ) -> None:
        self._authorize = authorize
        self._transaction = transaction
        self._qpc_frequency = qpc_frequency
        self._qpc_counter = qpc_counter

    def _check_deadline(self, request: BrokerWorkerRequestV1) -> None:
        if (
            self._qpc_frequency() != request.deadline.qpc_frequency
            or self._qpc_counter() >= request.deadline.deadline_tick
        ):
            raise RuntimeError("cst_saved_field.deadline_exceeded")

    def __call__(self, request: BrokerWorkerRequestV1) -> BrokerWorkerResponseV1:
        self._check_deadline(request)
        self._authorize(request)
        self._check_deadline(request)
        text, failure_id, settlement = self._transaction.execute(request)
        self._check_deadline(request)
        if not settlement.complete:
            raise RuntimeError("cst_saved_field.containment_settle_failed")
        ok = text is not None and failure_id is None
        return BrokerWorkerResponseV1(
            request.correlation_id,
            request.policy_revision,
            request.request_sha256,
            request.deadline,
            ok,
            text,
            failure_id,
            settlement,
        )


def _first_instruction_observation() -> dict[str, bool]:
    if os.name != "nt":
        return {
            "exact_job": False,
            "exactly_three_inherited_std_handles": False,
            "no_console": False,
            "breakaway_denied": False,
            "breakaway_created": False,
            "escaped_process_settled": False,
        }
    import ctypes
    from ctypes import wintypes

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.GetCurrentProcess.restype = wintypes.HANDLE
    kernel32.IsProcessInJob.argtypes = (
        wintypes.HANDLE,
        wintypes.HANDLE,
        ctypes.POINTER(wintypes.BOOL),
    )
    kernel32.IsProcessInJob.restype = wintypes.BOOL
    kernel32.GetStdHandle.argtypes = (wintypes.DWORD,)
    kernel32.GetStdHandle.restype = wintypes.HANDLE
    kernel32.GetHandleInformation.argtypes = (
        wintypes.HANDLE,
        ctypes.POINTER(wintypes.DWORD),
    )
    kernel32.GetHandleInformation.restype = wintypes.BOOL
    kernel32.GetConsoleWindow.restype = wintypes.HWND
    current = kernel32.GetCurrentProcess()
    in_job = wintypes.BOOL()
    exact_job = bool(kernel32.IsProcessInJob(current, None, ctypes.byref(in_job))) and bool(in_job.value)
    stdio = True
    for identifier in (-10, -11, -12):
        handle = kernel32.GetStdHandle(wintypes.DWORD(identifier))
        flags = wintypes.DWORD()
        if handle in (None, 0, -1) or not kernel32.GetHandleInformation(handle, ctypes.byref(flags)):
            stdio = False
            break
        stdio = stdio and bool(flags.value & 1)
    denied, created, settled = _breakaway_observation()
    return {
        "exact_job": exact_job,
        "exactly_three_inherited_std_handles": stdio,
        "no_console": not bool(kernel32.GetConsoleWindow()),
        "breakaway_denied": denied,
        "breakaway_created": created,
        "escaped_process_settled": settled,
    }


def _breakaway_observation() -> tuple[bool, bool, bool]:
    if os.name != "nt":
        return False, False, False
    import ctypes
    from ctypes import wintypes

    class StartupInfoW(ctypes.Structure):
        _fields_ = [
            ("cb", wintypes.DWORD),
            ("lpReserved", wintypes.LPWSTR),
            ("lpDesktop", wintypes.LPWSTR),
            ("lpTitle", wintypes.LPWSTR),
            ("dwX", wintypes.DWORD),
            ("dwY", wintypes.DWORD),
            ("dwXSize", wintypes.DWORD),
            ("dwYSize", wintypes.DWORD),
            ("dwXCountChars", wintypes.DWORD),
            ("dwYCountChars", wintypes.DWORD),
            ("dwFillAttribute", wintypes.DWORD),
            ("dwFlags", wintypes.DWORD),
            ("wShowWindow", wintypes.WORD),
            ("cbReserved2", wintypes.WORD),
            ("lpReserved2", ctypes.POINTER(ctypes.c_ubyte)),
            ("hStdInput", wintypes.HANDLE),
            ("hStdOutput", wintypes.HANDLE),
            ("hStdError", wintypes.HANDLE),
        ]

    class ProcessInformation(ctypes.Structure):
        _fields_ = [
            ("hProcess", wintypes.HANDLE),
            ("hThread", wintypes.HANDLE),
            ("dwProcessId", wintypes.DWORD),
            ("dwThreadId", wintypes.DWORD),
        ]

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.CreateProcessW.argtypes = (
        wintypes.LPCWSTR,
        wintypes.LPWSTR,
        wintypes.LPVOID,
        wintypes.LPVOID,
        wintypes.BOOL,
        wintypes.DWORD,
        wintypes.LPVOID,
        wintypes.LPCWSTR,
        ctypes.POINTER(StartupInfoW),
        ctypes.POINTER(ProcessInformation),
    )
    kernel32.CreateProcessW.restype = wintypes.BOOL
    startup = StartupInfoW(cb=ctypes.sizeof(StartupInfoW))
    process = ProcessInformation()
    executable = os.path.realpath(sys.executable)
    command = ctypes.create_unicode_buffer(f'"{executable}" -I -s -E -c "pass"')
    created = bool(
        kernel32.CreateProcessW(
            executable,
            command,
            None,
            None,
            False,
            0x08000000 | 0x01000000,
            None,
            os.path.dirname(executable),
            ctypes.byref(startup),
            ctypes.byref(process),
        )
    )
    if not created:
        return ctypes.get_last_error() == 5, False, True
    terminated = waited = exit_recorded = thread_closed = process_closed = False
    try:
        terminated = bool(kernel32.TerminateProcess(process.hProcess, 1))
        waited = kernel32.WaitForSingleObject(process.hProcess, 5_000) == 0
        exit_code = wintypes.DWORD()
        exit_recorded = bool(kernel32.GetExitCodeProcess(process.hProcess, ctypes.byref(exit_code)))
    finally:
        thread_closed = bool(kernel32.CloseHandle(process.hThread))
        process_closed = bool(kernel32.CloseHandle(process.hProcess))
    return False, True, all((terminated, waited, exit_recorded, thread_closed, process_closed))


def _unavailable(request: BrokerWorkerRequestV1) -> BrokerWorkerResponseV1:
    return BrokerWorkerResponseV1(
        request.correlation_id,
        request.policy_revision,
        request.request_sha256,
        request.deadline,
        False,
        None,
        "cst_saved_field.cst_unavailable",
        WorkerSettlementV1(True, True, True, True, True, True, 0),
    )


def run_worker(
    source: BinaryIO,
    destination: BinaryIO,
    application: WorkerApplication = _unavailable,
    *,
    diagnostics: BinaryIO | None = None,
    startup_observation: dict[str, bool] | None = None,
) -> int:
    observation = dict(startup_observation or _first_instruction_observation())
    proof = WorkerStartupProofV1(
        exact_job=observation.get("exact_job") is True,
        exactly_three_inherited_std_handles=(observation.get("exactly_three_inherited_std_handles") is True),
        no_console=observation.get("no_console") is True,
        breakaway_denied=observation.get("breakaway_denied") is True,
        breakaway_created=observation.get("breakaway_created") is True,
        escaped_process_settled=observation.get("escaped_process_settled") is True,
    )
    if diagnostics is not None:
        diagnostics.write(encode_startup_proof_frame(proof))
        diagnostics.flush()
    if not proof.complete:
        return 78
    request = BrokerWorkerRequestV1.from_wire(decode_one_frame(source, maximum=BROKER_WORKER_REQUEST_MAX))
    response = application(request)
    destination.write(encode_frame(response.to_wire(), maximum=BROKER_WORKER_RESPONSE_MAX))
    destination.flush()
    return 0


def main() -> int:
    return run_worker(sys.stdin.buffer, sys.stdout.buffer, diagnostics=sys.stderr.buffer)


if __name__ == "__main__":
    raise SystemExit(main())
