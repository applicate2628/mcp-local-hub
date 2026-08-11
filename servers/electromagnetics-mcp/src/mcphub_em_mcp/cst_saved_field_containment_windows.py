"""Broker-owned Windows atomic worker containment with an import-safe surface."""

from __future__ import annotations

import hashlib
import os
import sys
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

from .cst_saved_field_broker_client_windows import (
    AdmissionFailure,  # noqa: F401 - compatibility surface for containment callers
    SamplerAdmissionGate,
)
from .cst_saved_field_broker_worker_protocol import (
    JOB_PROCESS_MAX,
    WorkerStartupProofV1,
)

EXTENDED_STARTUPINFO_PRESENT = 0x00080000
CREATE_UNICODE_ENVIRONMENT = 0x00000400
CREATE_NO_WINDOW = 0x08000000
STARTF_USESTDHANDLES = 0x00000100
PROC_THREAD_ATTRIBUTE_HANDLE_LIST = 0x00020002
PROC_THREAD_ATTRIBUTE_JOB_LIST = 0x0002000D
JOB_OBJECT_LIMIT_ACTIVE_PROCESS = 0x00000008
JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000


class ContainmentFailure(RuntimeError):
    def __init__(self, failure_id: str, *, quarantine: bool = False) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id
        self.quarantine = quarantine


@dataclass(slots=True)
class _NativeHandleLedger:
    """Own every native handle immediately, including partial allocation failures."""

    handles: list[int]

    def own(self, handle: int) -> int:
        if not handle:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        self.handles.append(int(handle))
        return int(handle)

    def close_one(self, handle: int, closer: Callable[[int], None]) -> None:
        closer(handle)
        self.handles.remove(int(handle))

    def settle(self, closer: Callable[[int], None]) -> tuple[BaseException, ...]:
        failures: list[BaseException] = []
        for handle in tuple(reversed(self.handles)):
            try:
                closer(handle)
            except BaseException as exc:
                failures.append(exc)
            else:
                self.handles.remove(handle)
        return tuple(failures)


@dataclass(slots=True)
class _BoundedIoWorker:
    """One blocking native I/O call with owned cancellation and bounded join."""

    operation: Callable[[], None]
    cancel: Callable[[threading.Thread], None]
    thread: threading.Thread | None = None
    cancelled: bool = False

    def start(self) -> None:
        if self.thread is not None:
            raise RuntimeError("I/O worker already started")
        self.thread = threading.Thread(target=self.operation, daemon=True)
        self.thread.start()

    def settle(self, operation_deadline: float, cleanup_deadline: float) -> bool:
        if self.thread is None:
            return True
        self.thread.join(max(0.0, operation_deadline - time.monotonic()))
        if self.thread.is_alive():
            self.cancelled = True
            self.cancel(self.thread)
            self.thread.join(max(0.0, cleanup_deadline - time.monotonic()))
        return not self.thread.is_alive()


@dataclass(frozen=True, slots=True)
class ExecutableIdentity:
    final_path: str
    volume_serial: int
    file_id: str
    sha256: str
    version: str


class PinnedExecutable:
    """Hold the exact worker executable without write/delete sharing until launch settles."""

    def __init__(
        self,
        handle: int,
        identity: ExecutableIdentity,
        identity_reader: Callable[[int], ExecutableIdentity],
        closer: Callable[[int], object],
        check_deadline: Callable[[], None] = lambda: None,
    ) -> None:
        self._handle = handle
        self.identity = identity
        self._identity_reader = identity_reader
        self._closer = closer
        self._check_deadline = check_deadline

    @classmethod
    def open(
        cls,
        path: str | Path,
        *,
        identity_reader: Callable[[int], ExecutableIdentity] | None = None,
        opener: Callable[[str], int] | None = None,
        closer: Callable[[int], object] | None = None,
        check_deadline: Callable[[], None] = lambda: None,
    ) -> PinnedExecutable:
        if opener is None or identity_reader is None or closer is None:
            opener, identity_reader, closer = _executable_pin_primitives(check_deadline)
        check_deadline()
        handle = opener(os.path.abspath(os.fspath(path)))
        try:
            check_deadline()
            identity = identity_reader(handle)
            check_deadline()
        except BaseException:
            closer(handle)
            raise
        return cls(handle, identity, identity_reader, closer, check_deadline)

    def revalidate(self) -> ExecutableIdentity:
        self._check_deadline()
        if self._handle is None or self._identity_reader(self._handle) != self.identity:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        self._check_deadline()
        return self.identity

    def __enter__(self) -> PinnedExecutable:
        return self

    def __exit__(self, *_args: object) -> None:
        handle = self._handle
        self._handle = None
        if handle is not None and self._closer(handle) is False:
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)


def _executable_pin_primitives(check_deadline: Callable[[], None] = lambda: None):
    if os.name != "nt":

        def opener(path: str) -> int:
            return os.open(path, os.O_RDONLY)

        def reader(handle: int) -> ExecutableIdentity:
            info = os.fstat(handle)
            position = os.lseek(handle, 0, os.SEEK_CUR)
            os.lseek(handle, 0, os.SEEK_SET)
            digest = hashlib.sha256()
            while True:
                check_deadline()
                block = os.read(handle, 1024 * 1024)
                if not block:
                    break
                digest.update(block)
            os.lseek(handle, position, os.SEEK_SET)
            return ExecutableIdentity(
                final_path=os.path.abspath(f"/proc/self/fd/{handle}"),
                volume_serial=int(info.st_dev),
                file_id=f"{int(info.st_ino):032x}",
                sha256=digest.hexdigest(),
                version=sys.version.split()[0],
            )

        return opener, reader, os.close

    import ctypes
    from ctypes import wintypes

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.CreateFileW.restype = wintypes.HANDLE

    class FileTime(ctypes.Structure):
        _fields_ = [("low", wintypes.DWORD), ("high", wintypes.DWORD)]

    class ByHandleFileInformation(ctypes.Structure):
        _fields_ = [
            ("attributes", wintypes.DWORD),
            ("creation", FileTime),
            ("access", FileTime),
            ("write", FileTime),
            ("volume_serial", wintypes.DWORD),
            ("size_high", wintypes.DWORD),
            ("size_low", wintypes.DWORD),
            ("links", wintypes.DWORD),
            ("index_high", wintypes.DWORD),
            ("index_low", wintypes.DWORD),
        ]

    def checked_handle(path: str) -> int:
        handle = kernel32.CreateFileW(
            path,
            0x80000000,
            0x00000001,
            None,
            3,
            0x00200000,
            None,
        )
        if handle in {None, ctypes.c_void_p(-1).value}:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        return int(handle)

    def read_identity(raw: int) -> ExecutableIdentity:
        handle = wintypes.HANDLE(raw)
        info = ByHandleFileInformation()
        if not kernel32.GetFileInformationByHandle(handle, ctypes.byref(info)):
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        if info.attributes & 0x00000400 or info.links != 1:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        needed = kernel32.GetFinalPathNameByHandleW(handle, None, 0, 0)
        if not needed:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        buffer = ctypes.create_unicode_buffer(needed + 1)
        if not kernel32.GetFinalPathNameByHandleW(handle, buffer, len(buffer), 0):
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        final_path = buffer.value
        if final_path.startswith("\\\\?\\"):
            final_path = final_path[4:]
        zero = ctypes.c_int64(0)
        if not kernel32.SetFilePointerEx(handle, zero, None, 0):
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        digest = hashlib.sha256()
        block = ctypes.create_string_buffer(1024 * 1024)
        count = wintypes.DWORD()
        while True:
            check_deadline()
            if not kernel32.ReadFile(handle, block, len(block), ctypes.byref(count), None):
                raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
            if count.value == 0:
                break
            digest.update(block.raw[: count.value])
        if not kernel32.SetFilePointerEx(handle, zero, None, 0):
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        return ExecutableIdentity(
            final_path=final_path,
            volume_serial=int(info.volume_serial),
            file_id=f"{int(info.index_high):08x}{int(info.index_low):08x}".rjust(32, "0"),
            sha256=digest.hexdigest(),
            version=sys.version.split()[0],
        )

    def close(raw: int) -> bool:
        return bool(kernel32.CloseHandle(wintypes.HANDLE(raw)))

    return checked_handle, read_identity, close


@dataclass(frozen=True, slots=True)
class CreateProcessSpec:
    application_name: str
    command_line: str
    current_directory: str
    inherit_handles: bool
    startf_use_std_handles: bool
    handle_list_roles: tuple[str, ...]
    attribute_roles: tuple[str, ...]
    creation_flags: int
    shell: bool
    path_search: bool
    breakaway: bool


def build_create_process_spec(executable: str) -> CreateProcessSpec:
    resolved = os.path.abspath(executable)
    if not os.path.isabs(resolved) or not os.path.basename(resolved):
        raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
    return CreateProcessSpec(
        application_name=resolved,
        command_line=f'"{resolved}" -I -s -E -m mcphub_em_mcp.cst_saved_field_broker_worker',
        current_directory=str(Path(resolved).parent),
        inherit_handles=True,
        startf_use_std_handles=True,
        handle_list_roles=("stdin", "stdout", "stderr"),
        attribute_roles=("job_list", "handle_list"),
        creation_flags=(EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT | CREATE_NO_WINDOW),
        shell=False,
        path_search=False,
        breakaway=False,
    )


def validate_create_process_spec(spec: CreateProcessSpec, executable: str) -> CreateProcessSpec:
    if spec != build_create_process_spec(executable):
        raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
    return spec


FirstInstructionProof = WorkerStartupProofV1


@dataclass(frozen=True, slots=True)
class KernelInvocationResult:
    response_frame: bytes
    worker_signaled: bool
    exit_recorded: bool
    process_reference_closed: bool
    active_zero: bool
    readers_joined: bool
    handles_closed: bool
    residual_process: bool
    timed_out: bool
    exit_code: int = 0
    stderr_overflow: bool = False
    first_instruction_proof: FirstInstructionProof | None = None


class WindowsKernel(Protocol):
    def invoke(
        self,
        spec: CreateProcessSpec,
        request_frame: bytes | Callable[[], bytes],
        *,
        startup_validator: Callable[[FirstInstructionProof], None] | None = None,
        startup_deadline: float,
        response_deadline: float,
        absolute_deadline: float,
        cleanup_deadline: float,
    ) -> KernelInvocationResult: ...


class WindowsContainedInvocation:
    def __init__(self, *, kernel: WindowsKernel, executable: str) -> None:
        self._kernel = kernel
        self._executable = executable

    @staticmethod
    def _validate_startup(proof: FirstInstructionProof) -> None:
        if not (
            proof.exact_job
            and proof.exactly_three_inherited_std_handles
            and proof.no_console
            and proof.breakaway_denied
            and not proof.breakaway_created
            and proof.escaped_process_settled
        ):
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)

    @classmethod
    def _validate_result(cls, result: KernelInvocationResult) -> bytes:
        settled = (
            result.worker_signaled
            and result.exit_recorded
            and result.process_reference_closed
            and result.active_zero
            and result.readers_joined
            and result.handles_closed
        )
        if not settled:
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)
        if result.residual_process:
            raise ContainmentFailure("cst_saved_field.containment_residual_process")
        if result.timed_out:
            raise ContainmentFailure("cst_saved_field.deadline_exceeded")
        if result.stderr_overflow or result.exit_code != 0:
            raise ContainmentFailure("cst_saved_field.broker_worker_protocol_invalid")
        if result.first_instruction_proof is None:
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        cls._validate_startup(result.first_instruction_proof)
        return result.response_frame

    def invoke_after_startup(
        self, request_factory: Callable[[], bytes], *, start: float | None = None
    ) -> bytes:
        started = time.monotonic() if start is None else start
        absolute_deadline = started + 60.0

        def check_deadline() -> None:
            if time.monotonic() >= absolute_deadline:
                raise ContainmentFailure("cst_saved_field.deadline_exceeded")

        check_deadline()
        with PinnedExecutable.open(self._executable, check_deadline=check_deadline) as executable:
            identity = executable.revalidate()
            check_deadline()
            spec = validate_create_process_spec(
                build_create_process_spec(identity.final_path), identity.final_path
            )
            result = self._kernel.invoke(
                spec,
                request_factory,
                startup_validator=self._validate_startup,
                startup_deadline=started + 5.0,
                response_deadline=started + 58.0,
                absolute_deadline=absolute_deadline,
                cleanup_deadline=started + 70.0,
            )
            executable.revalidate()
            check_deadline()
        return self._validate_result(result)

    def invoke(self, request_frame: bytes, *, start: float | None = None) -> bytes:
        return self.invoke_after_startup(lambda: request_frame, start=start)


class ContainedSamplerRunner:
    """Bind one admission lease to one contained invocation and settle it once."""

    def __init__(self, *, gate: SamplerAdmissionGate, invocation: WindowsContainedInvocation) -> None:
        self._gate = gate
        self._invocation = invocation

    def invoke(
        self,
        request_frame: bytes,
        *,
        revision: str,
        wait_seconds: float,
        start: float | None = None,
    ) -> bytes:
        lease = self._gate.acquire_and_seal(revision, wait_seconds=wait_seconds)
        try:
            lease.authorize_start()
        except BaseException:
            self._gate.release(lease)
            raise
        try:
            response = self._invocation.invoke(request_frame, start=start)
        except ContainmentFailure as exc:
            if exc.quarantine:
                self._gate.quarantine_and_release(lease)
            else:
                self._gate.release(lease)
            raise
        except BaseException as exc:
            self._gate.quarantine_and_release(lease)
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True) from exc
        self._gate.release(lease)
        return response

    def invoke_after_admission(
        self,
        request_factory,
        *,
        revision: str,
        wait_seconds: float,
        response_consumer=None,
    ):
        """Seal/recheck admission before descriptor construction or source work."""
        lease = self._gate.acquire_and_seal(revision, wait_seconds=wait_seconds)
        try:
            lease.authorize_start()
            started = time.monotonic()
        except BaseException:
            self._gate.release(lease)
            raise
        try:
            factory_error: BaseException | None = None

            def create_frame() -> bytes:
                nonlocal factory_error
                try:
                    built = request_factory(started)
                    return built[0] if isinstance(built, tuple) else built
                except BaseException as exc:
                    factory_error = exc
                    raise

            response = self._invocation.invoke_after_startup(create_frame, start=started)
        except ContainmentFailure as exc:
            if exc.quarantine:
                self._gate.quarantine_and_release(lease)
            else:
                self._gate.release(lease)
            raise
        except BaseException as exc:
            if factory_error is exc:
                self._gate.release(lease)
                raise
            self._gate.quarantine_and_release(lease)
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True) from exc
        if response_consumer is not None:
            try:
                response = response_consumer(response)
            except ContainmentFailure as exc:
                if exc.quarantine:
                    self._gate.quarantine_and_release(lease)
                else:
                    self._gate.release(lease)
                raise
            except BaseException as exc:
                self._gate.quarantine_and_release(lease)
                raise ContainmentFailure(
                    "cst_saved_field.containment_settle_failed", quarantine=True
                ) from exc
        self._gate.release(lease)
        return response


class CtypesWindowsKernel:
    """ctypes CreateProcessW backend; constructed only on Windows.

    Process creation uses ``PROC_THREAD_ATTRIBUTE_JOB_LIST`` and
    ``PROC_THREAD_ATTRIBUTE_HANDLE_LIST`` in the same STARTUPINFOEXW call.  There
    is intentionally no subprocess/shell/PATH or create-suspended fallback.
    """

    def __init__(self) -> None:
        if os.name != "nt":
            raise OSError("Windows containment is unavailable on this platform")

    def invoke(
        self,
        spec: CreateProcessSpec,
        request_frame: bytes | Callable[[], bytes],
        *,
        startup_validator: Callable[[FirstInstructionProof], None] | None = None,
        startup_deadline: float,
        response_deadline: float,
        absolute_deadline: float,
        cleanup_deadline: float,
    ) -> KernelInvocationResult:
        return _invoke_atomic_job_process(
            spec,
            request_frame,
            startup_validator=startup_validator,
            startup_deadline=startup_deadline,
            response_deadline=response_deadline,
            absolute_deadline=absolute_deadline,
            cleanup_deadline=cleanup_deadline,
        )


def _invoke_atomic_job_process(
    spec: CreateProcessSpec,
    request_frame: bytes | Callable[[], bytes],
    *,
    startup_validator: Callable[[FirstInstructionProof], None] | None = None,
    startup_deadline: float,
    response_deadline: float,
    absolute_deadline: float,
    cleanup_deadline: float,
) -> KernelInvocationResult:
    """Create the worker already assigned to its Job and explicit handle list."""

    if os.name != "nt":
        raise OSError("Windows containment is unavailable on this platform")
    import ctypes
    from ctypes import wintypes

    validate_create_process_spec(spec, spec.application_name)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    handle_t = wintypes.HANDLE
    size_t = ctypes.c_size_t
    ulong_ptr = ctypes.c_size_t
    kernel32.CreateJobObjectW.restype = handle_t
    kernel32.OpenThread.restype = handle_t

    class SecurityAttributes(ctypes.Structure):
        _fields_ = [
            ("nLength", wintypes.DWORD),
            ("lpSecurityDescriptor", wintypes.LPVOID),
            ("bInheritHandle", wintypes.BOOL),
        ]

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
            ("hStdInput", handle_t),
            ("hStdOutput", handle_t),
            ("hStdError", handle_t),
        ]

    class StartupInfoExW(ctypes.Structure):
        _fields_ = [("StartupInfo", StartupInfoW), ("lpAttributeList", wintypes.LPVOID)]

    class ProcessInformation(ctypes.Structure):
        _fields_ = [
            ("hProcess", handle_t),
            ("hThread", handle_t),
            ("dwProcessId", wintypes.DWORD),
            ("dwThreadId", wintypes.DWORD),
        ]

    class IoCounters(ctypes.Structure):
        _fields_ = [
            (name, ctypes.c_uint64)
            for name in (
                "ReadOperationCount",
                "WriteOperationCount",
                "OtherOperationCount",
                "ReadTransferCount",
                "WriteTransferCount",
                "OtherTransferCount",
            )
        ]

    class BasicLimitInformation(ctypes.Structure):
        _fields_ = [
            ("PerProcessUserTimeLimit", ctypes.c_int64),
            ("PerJobUserTimeLimit", ctypes.c_int64),
            ("LimitFlags", wintypes.DWORD),
            ("MinimumWorkingSetSize", size_t),
            ("MaximumWorkingSetSize", size_t),
            ("ActiveProcessLimit", wintypes.DWORD),
            ("Affinity", ulong_ptr),
            ("PriorityClass", wintypes.DWORD),
            ("SchedulingClass", wintypes.DWORD),
        ]

    class ExtendedLimitInformation(ctypes.Structure):
        _fields_ = [
            ("BasicLimitInformation", BasicLimitInformation),
            ("IoInfo", IoCounters),
            ("ProcessMemoryLimit", size_t),
            ("JobMemoryLimit", size_t),
            ("PeakProcessMemoryUsed", size_t),
            ("PeakJobMemoryUsed", size_t),
        ]

    class BasicAccountingInformation(ctypes.Structure):
        _fields_ = [
            ("TotalUserTime", ctypes.c_int64),
            ("TotalKernelTime", ctypes.c_int64),
            ("ThisPeriodTotalUserTime", ctypes.c_int64),
            ("ThisPeriodTotalKernelTime", ctypes.c_int64),
            ("TotalPageFaultCount", wintypes.DWORD),
            ("TotalProcesses", wintypes.DWORD),
            ("ActiveProcesses", wintypes.DWORD),
            ("TotalTerminatedProcesses", wintypes.DWORD),
        ]

    kernel32.InitializeProcThreadAttributeList.argtypes = [
        wintypes.LPVOID,
        wintypes.DWORD,
        wintypes.DWORD,
        ctypes.POINTER(size_t),
    ]
    kernel32.InitializeProcThreadAttributeList.restype = wintypes.BOOL
    kernel32.UpdateProcThreadAttribute.argtypes = [
        wintypes.LPVOID,
        wintypes.DWORD,
        size_t,
        wintypes.LPVOID,
        size_t,
        wintypes.LPVOID,
        ctypes.POINTER(size_t),
    ]
    kernel32.UpdateProcThreadAttribute.restype = wintypes.BOOL
    kernel32.DeleteProcThreadAttributeList.argtypes = [wintypes.LPVOID]
    kernel32.DeleteProcThreadAttributeList.restype = None

    def checked(ok: int, operation: str) -> None:
        if not ok:
            raise ContainmentFailure(
                "cst_saved_field.containment_configuration_invalid"
            ) from ctypes.WinError(ctypes.get_last_error())

    def close(handle: int | None) -> None:
        if handle and not kernel32.CloseHandle(handle_t(handle)):
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)

    job = kernel32.CreateJobObjectW(None, None)
    if not job:
        checked(0, "CreateJobObjectW")
    ledger = _NativeHandleLedger([int(job)])
    attribute_buffer = None
    startup = StartupInfoExW()
    process = ProcessInformation()
    reader_workers: list[_BoundedIoWorker] = []
    stdout_data = bytearray()
    stderr_data = bytearray()
    stderr_updated = threading.Event()
    timed_out = False
    residual = False
    exit_code = wintypes.DWORD(0)
    exact_job = False
    worker_signaled = False
    process_closed = False
    active_zero = False
    try:
        limits = ExtendedLimitInformation()
        limits.BasicLimitInformation.LimitFlags = (
            JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | JOB_OBJECT_LIMIT_ACTIVE_PROCESS
        )
        limits.BasicLimitInformation.ActiveProcessLimit = JOB_PROCESS_MAX
        checked(
            kernel32.SetInformationJobObject(handle_t(job), 9, ctypes.byref(limits), ctypes.sizeof(limits)),
            "SetInformationJobObject",
        )

        security = SecurityAttributes(ctypes.sizeof(SecurityAttributes), None, True)
        stdin_read, stdin_write = handle_t(), handle_t()
        stdout_read, stdout_write = handle_t(), handle_t()
        stderr_read, stderr_write = handle_t(), handle_t()
        for read_end, write_end in (
            (stdin_read, stdin_write),
            (stdout_read, stdout_write),
            (stderr_read, stderr_write),
        ):
            checked(
                kernel32.CreatePipe(
                    ctypes.byref(read_end), ctypes.byref(write_end), ctypes.byref(security), 0
                ),
                "CreatePipe",
            )
            ledger.own(int(read_end.value))
            ledger.own(int(write_end.value))
        parent_ends = (stdin_write, stdout_read, stderr_read)
        child_ends = (stdin_read, stdout_write, stderr_write)
        for parent_handle in parent_ends:
            checked(kernel32.SetHandleInformation(parent_handle, 1, 0), "SetHandleInformation")
        attribute_size = size_t()
        kernel32.InitializeProcThreadAttributeList(None, 2, 0, ctypes.byref(attribute_size))
        attribute_buffer = ctypes.create_string_buffer(attribute_size.value)
        startup.lpAttributeList = ctypes.cast(attribute_buffer, wintypes.LPVOID)
        checked(
            kernel32.InitializeProcThreadAttributeList(
                startup.lpAttributeList, 2, 0, ctypes.byref(attribute_size)
            ),
            "InitializeProcThreadAttributeList",
        )
        job_value = handle_t(job)
        checked(
            kernel32.UpdateProcThreadAttribute(
                startup.lpAttributeList,
                0,
                PROC_THREAD_ATTRIBUTE_JOB_LIST,
                ctypes.byref(job_value),
                ctypes.sizeof(job_value),
                None,
                None,
            ),
            "UpdateProcThreadAttribute(JOB_LIST)",
        )
        handle_array = (handle_t * 3)(*child_ends)
        checked(
            kernel32.UpdateProcThreadAttribute(
                startup.lpAttributeList,
                0,
                PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
                handle_array,
                ctypes.sizeof(handle_array),
                None,
                None,
            ),
            "UpdateProcThreadAttribute(HANDLE_LIST)",
        )
        startup.StartupInfo.cb = ctypes.sizeof(StartupInfoExW)
        startup.StartupInfo.dwFlags = STARTF_USESTDHANDLES
        startup.StartupInfo.hStdInput = stdin_read
        startup.StartupInfo.hStdOutput = stdout_write
        startup.StartupInfo.hStdError = stderr_write
        command_line = ctypes.create_unicode_buffer(spec.command_line)
        checked(
            kernel32.CreateProcessW(
                spec.application_name,
                command_line,
                None,
                None,
                True,
                spec.creation_flags,
                None,
                spec.current_directory,
                ctypes.byref(startup),
                ctypes.byref(process),
            ),
            "CreateProcessW",
        )
        ledger.own(int(process.hProcess))
        ledger.own(int(process.hThread))
        in_exact_job = wintypes.BOOL()
        checked(
            kernel32.IsProcessInJob(process.hProcess, handle_t(job), ctypes.byref(in_exact_job)),
            "IsProcessInJob",
        )
        exact_job = bool(in_exact_job.value)
        ledger.close_one(int(process.hThread), close)
        for child_handle in child_ends:
            ledger.close_one(int(child_handle.value), close)

        def read_pipe(handle: handle_t, target: bytearray, maximum: int) -> None:
            buffer = ctypes.create_string_buffer(8192)
            count = wintypes.DWORD()
            while len(target) <= maximum:
                if not kernel32.ReadFile(handle, buffer, len(buffer), ctypes.byref(count), None):
                    return
                target.extend(buffer.raw[: count.value])
                if target is stderr_data:
                    stderr_updated.set()

        def cancel_sync(worker_thread: threading.Thread) -> None:
            if worker_thread.native_id is None:
                return
            worker_handle = kernel32.OpenThread(0x0001, False, worker_thread.native_id)
            if not worker_handle:
                raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)
            try:
                checked(kernel32.CancelSynchronousIo(worker_handle), "CancelSynchronousIo")
            finally:
                close(int(worker_handle))

        reader_workers = [
            _BoundedIoWorker(lambda: read_pipe(stdout_read, stdout_data, 1_114_112), cancel_sync),
            _BoundedIoWorker(lambda: read_pipe(stderr_read, stderr_data, 65_536), cancel_sync),
        ]
        for worker in reader_workers:
            worker.start()
        from .cst_saved_field_broker_worker_protocol import (
            WORKER_STARTUP_PROOF_MAX,
            decode_startup_proof_frame,
        )

        proof_deadline = min(absolute_deadline, startup_deadline)
        first_instruction_proof = None
        while time.monotonic() < proof_deadline:
            raw = bytes(stderr_data)
            if len(raw) >= 4:
                proof_length = int.from_bytes(raw[:4], "big")
                if proof_length == 0 or proof_length + 4 > WORKER_STARTUP_PROOF_MAX:
                    raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
                if len(raw) >= proof_length + 4:
                    try:
                        observed = decode_startup_proof_frame(
                            __import__("io").BytesIO(raw[: proof_length + 4])
                        )
                    except BaseException as exc:
                        raise ContainmentFailure(
                            "cst_saved_field.containment_startup_invalid", quarantine=True
                        ) from exc
                    first_instruction_proof = FirstInstructionProof(
                        exact_job=exact_job and observed.exact_job,
                        exactly_three_inherited_std_handles=(observed.exactly_three_inherited_std_handles),
                        no_console=observed.no_console,
                        breakaway_denied=observed.breakaway_denied,
                        breakaway_created=observed.breakaway_created,
                        escaped_process_settled=observed.escaped_process_settled,
                    )
                    break
            stderr_updated.wait(min(0.01, max(0.0, proof_deadline - time.monotonic())))
            stderr_updated.clear()
        if first_instruction_proof is None:
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        if startup_validator is not None:
            startup_validator(first_instruction_proof)
        factory_values: list[bytes] = []
        factory_errors: list[BaseException] = []

        def build_request() -> None:
            try:
                factory_values.append(request_frame() if callable(request_frame) else request_frame)
            except BaseException as exc:
                factory_errors.append(exc)

        factory_worker = _BoundedIoWorker(build_request, cancel_sync)
        factory_worker.start()
        factory_joined = factory_worker.settle(absolute_deadline, cleanup_deadline)
        if factory_worker.cancelled:
            kernel32.TerminateJobObject(handle_t(job), 1)
        if not factory_joined:
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)
        if factory_worker.cancelled:
            raise ContainmentFailure("cst_saved_field.deadline_exceeded")
        if factory_errors:
            raise factory_errors[0]
        if len(factory_values) != 1:
            raise ContainmentFailure("cst_saved_field.broker_worker_protocol_invalid")
        request_frame = factory_values[0]
        if time.monotonic() >= absolute_deadline:
            raise ContainmentFailure("cst_saved_field.deadline_exceeded")
        request_buffer = ctypes.create_string_buffer(request_frame)
        write_failures: list[str] = []

        def write_request() -> None:
            written = wintypes.DWORD()
            try:
                if not kernel32.WriteFile(
                    stdin_write,
                    request_buffer,
                    len(request_frame),
                    ctypes.byref(written),
                    None,
                ):
                    write_failures.append("write_failed")
                elif written.value != len(request_frame):
                    write_failures.append("write_short")
            except BaseException:
                write_failures.append("write_exception")

        writer = _BoundedIoWorker(write_request, cancel_sync)
        writer.start()
        writer_joined = writer.settle(absolute_deadline, cleanup_deadline)
        if writer.cancelled or not writer_joined:
            timed_out = True
            kernel32.TerminateJobObject(handle_t(job), 1)
        if int(stdin_write.value) in ledger.handles:
            ledger.close_one(int(stdin_write.value), close)
        if write_failures and not timed_out:
            raise ContainmentFailure("cst_saved_field.broker_worker_protocol_invalid")
        remaining_ms = max(0, int((response_deadline - time.monotonic()) * 1000))
        wait_result = kernel32.WaitForSingleObject(process.hProcess, remaining_ms)
        worker_signaled = wait_result == 0
        timed_out = timed_out or not worker_signaled
        if timed_out:
            kernel32.TerminateJobObject(handle_t(job), 1)
            cleanup_ms = max(0, int((cleanup_deadline - time.monotonic()) * 1000))
            worker_signaled = kernel32.WaitForSingleObject(process.hProcess, cleanup_ms) == 0
        checked(kernel32.GetExitCodeProcess(process.hProcess, ctypes.byref(exit_code)), "GetExitCodeProcess")
        ledger.close_one(int(process.hProcess), close)
        process_closed = True
        accounting = BasicAccountingInformation()
        checked(
            kernel32.QueryInformationJobObject(
                handle_t(job), 1, ctypes.byref(accounting), ctypes.sizeof(accounting), None
            ),
            "QueryInformationJobObject",
        )
        accounting_grace = min(absolute_deadline, time.monotonic() + 0.1)
        while worker_signaled and accounting.ActiveProcesses != 0 and time.monotonic() < accounting_grace:
            time.sleep(0.005)
            checked(
                kernel32.QueryInformationJobObject(
                    handle_t(job), 1, ctypes.byref(accounting), ctypes.sizeof(accounting), None
                ),
                "QueryInformationJobObject",
            )
        residual = accounting.ActiveProcesses != 0
        if residual:
            kernel32.TerminateJobObject(handle_t(job), 1)
            while time.monotonic() < cleanup_deadline:
                checked(
                    kernel32.QueryInformationJobObject(
                        handle_t(job), 1, ctypes.byref(accounting), ctypes.sizeof(accounting), None
                    ),
                    "QueryInformationJobObject",
                )
                if accounting.ActiveProcesses == 0:
                    break
                time.sleep(0.01)
        active_zero = accounting.ActiveProcesses == 0
        reader_results = tuple(worker.settle(time.monotonic(), cleanup_deadline) for worker in reader_workers)
        readers_joined = writer_joined and all(reader_results)
        if readers_joined:
            for parent_reader in (stdout_read, stderr_read):
                ledger.close_one(int(parent_reader.value), close)
        if active_zero and readers_joined:
            ledger.close_one(int(job), close)
        handles_closed = not ledger.handles
        return KernelInvocationResult(
            response_frame=bytes(stdout_data),
            worker_signaled=worker_signaled,
            exit_recorded=worker_signaled,
            process_reference_closed=process_closed,
            active_zero=active_zero,
            readers_joined=readers_joined,
            handles_closed=handles_closed,
            residual_process=residual,
            timed_out=timed_out,
            exit_code=int(exit_code.value),
            stderr_overflow=len(stderr_data) > 65_536,
            first_instruction_proof=first_instruction_proof,
        )
    finally:
        if startup.lpAttributeList:
            kernel32.DeleteProcThreadAttributeList(startup.lpAttributeList)
        failures = ledger.settle(close)
        if failures:
            raise ContainmentFailure(
                "cst_saved_field.containment_settle_failed", quarantine=True
            ) from failures[0]
