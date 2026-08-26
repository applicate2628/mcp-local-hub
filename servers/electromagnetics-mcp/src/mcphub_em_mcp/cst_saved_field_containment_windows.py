"""Broker-owned Windows atomic worker containment with an import-safe surface."""

from __future__ import annotations

import hashlib
import os
import sys
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

from .cst_saved_field_broker_client_windows import (
    AdmissionFailure,  # noqa: F401 - compatibility surface for containment callers
    SamplerAdmissionGate,
)
from .cst_saved_field_broker_protocol import QpcDeadlineV1
from .cst_saved_field_broker_worker_protocol import (
    JOB_PROCESS_MAX,
    WORKER_HANDLE_ROLES,
    WORKER_PRE_MAIN_FRAME_MAX,
    WorkerPreMainBootstrapV1,
    WorkerPreMainReceiptV1,
    WorkerStartupProofV1,
    decode_pre_main_receipt_frame,
    encode_pre_main_bootstrap_frame,
)

EXTENDED_STARTUPINFO_PRESENT = 0x00080000
CREATE_UNICODE_ENVIRONMENT = 0x00000400
CREATE_NO_WINDOW = 0x08000000
STARTF_USESTDHANDLES = 0x00000100
PROC_THREAD_ATTRIBUTE_HANDLE_LIST = 0x00020002
PROC_THREAD_ATTRIBUTE_JOB_LIST = 0x0002000D
JOB_OBJECT_LIMIT_ACTIVE_PROCESS = 0x00000008
JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000
SOURCE_ROOT_ACCESS = 0x00120089
WORKSPACE_ROOT_ACCESS = 0x0012019F
SOURCE_ROOT_SHARE = 0x00000001
WORKSPACE_ROOT_SHARE = 0x00000001 | 0x00000002

_SETTLEMENT_EVENTS = (
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
)
_UNAVAILABLE_SETTLEMENT_EVENTS = tuple(event for event in _SETTLEMENT_EVENTS if event != "request_started")


class ContainmentFailure(RuntimeError):
    def __init__(self, failure_id: str, *, quarantine: bool = False) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id
        self.quarantine = quarantine


class WorkerInheritanceEpoch:
    """Broker-wide owner for every interval containing inheritable handles."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._owner: int | None = None

    def acquire(self) -> _WorkerInheritanceEpochLease:
        owner = threading.get_ident()
        if self._owner == owner:
            raise ContainmentFailure("cst_saved_field.inheritance_epoch_reentered", quarantine=True)
        self._lock.acquire()
        self._owner = owner
        return _WorkerInheritanceEpochLease(self, owner)

    def _release(self, owner: int) -> None:
        if self._owner != owner:
            raise ContainmentFailure("cst_saved_field.inheritance_epoch_owner_invalid", quarantine=True)
        self._owner = None
        self._lock.release()


@dataclass(slots=True)
class _WorkerInheritanceEpochLease:
    epoch: WorkerInheritanceEpoch
    owner: int
    released: bool = False

    def release(self) -> None:
        if not self.released:
            self.epoch._release(self.owner)
            self.released = True


BROKER_WORKER_INHERITANCE_EPOCH = WorkerInheritanceEpoch()


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


@dataclass(frozen=True, slots=True)
class WorkerIdentityV1:
    """Kernel-read identity of the exact CreateProcessW process handle."""

    pid: int
    creation_time_100ns: int
    token_user_sid: str
    session_id: int
    image_path: str
    package_identity: str | None
    parent_pid: int

    def __post_init__(self) -> None:
        if (
            type(self.pid) is not int
            or self.pid <= 0
            or type(self.creation_time_100ns) is not int
            or self.creation_time_100ns <= 0
            or not isinstance(self.token_user_sid, str)
            or not self.token_user_sid.upper().startswith("S-")
            or type(self.session_id) is not int
            or self.session_id < 0
            or not isinstance(self.image_path, str)
            or not os.path.isabs(self.image_path)
            or (self.package_identity is not None and not isinstance(self.package_identity, str))
            or type(self.parent_pid) is not int
            or self.parent_pid <= 0
        ):
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)


@dataclass(frozen=True, slots=True)
class KernelContainmentEvidenceV1:
    """Broker-owned, exact-handle containment and settlement observations."""

    created_worker: WorkerIdentityV1
    pre_request_worker: WorkerIdentityV1
    job_member_before_request: bool
    inherited_handle_roles: tuple[str, ...]
    settlement_events: tuple[str, ...]
    foreign_process_operations: int

    def validate(self) -> None:
        if (
            self.created_worker != self.pre_request_worker
            or self.job_member_before_request is not True
            or self.inherited_handle_roles != WORKER_HANDLE_ROLES
            or self.settlement_events not in {_SETTLEMENT_EVENTS, _UNAVAILABLE_SETTLEMENT_EVENTS}
            or self.foreign_process_operations != 0
        ):
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)


def _default_qpc_frequency() -> int:
    if os.name != "nt":
        return 1_000_000_000
    import ctypes

    value = ctypes.c_int64()
    if not ctypes.windll.kernel32.QueryPerformanceFrequency(ctypes.byref(value)) or value.value <= 0:
        raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
    return int(value.value)


def _default_qpc_counter() -> int:
    if os.name != "nt":
        return time.perf_counter_ns()
    import ctypes

    value = ctypes.c_int64()
    if not ctypes.windll.kernel32.QueryPerformanceCounter(ctypes.byref(value)):
        raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
    return int(value.value)


def _read_windows_worker_identity(process_handle: int, pid: int) -> WorkerIdentityV1:
    """Read only the exact process handle returned by CreateProcessW."""

    if os.name != "nt":
        raise OSError("Windows worker identity is unavailable on this platform")
    import ctypes
    from ctypes import wintypes

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    advapi32 = ctypes.WinDLL("advapi32", use_last_error=True)
    ntdll = ctypes.WinDLL("ntdll")
    advapi32.OpenProcessToken.argtypes = (
        wintypes.HANDLE,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.HANDLE),
    )
    advapi32.OpenProcessToken.restype = wintypes.BOOL
    advapi32.GetTokenInformation.argtypes = (
        wintypes.HANDLE,
        ctypes.c_int,
        wintypes.LPVOID,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.DWORD),
    )
    advapi32.GetTokenInformation.restype = wintypes.BOOL
    advapi32.ConvertSidToStringSidW.argtypes = (
        wintypes.LPVOID,
        ctypes.POINTER(wintypes.LPWSTR),
    )
    advapi32.ConvertSidToStringSidW.restype = wintypes.BOOL
    kernel32.LocalFree.argtypes = (wintypes.HLOCAL,)
    kernel32.LocalFree.restype = wintypes.HLOCAL
    ntdll.NtQueryInformationProcess.restype = ctypes.c_long
    handle = wintypes.HANDLE(process_handle)

    class FileTime(ctypes.Structure):
        _fields_ = [("low", wintypes.DWORD), ("high", wintypes.DWORD)]

    creation, exit_time, kernel_time, user_time = FileTime(), FileTime(), FileTime(), FileTime()
    if not kernel32.GetProcessTimes(
        handle,
        ctypes.byref(creation),
        ctypes.byref(exit_time),
        ctypes.byref(kernel_time),
        ctypes.byref(user_time),
    ):
        raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
    creation_time = (int(creation.high) << 32) | int(creation.low)

    session = wintypes.DWORD()
    if not kernel32.ProcessIdToSessionId(wintypes.DWORD(pid), ctypes.byref(session)):
        raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)

    image = ctypes.create_unicode_buffer(32_768)
    image_length = wintypes.DWORD(len(image))
    if not kernel32.QueryFullProcessImageNameW(handle, 0, image, ctypes.byref(image_length)):
        raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)

    class ProcessBasicInformation(ctypes.Structure):
        _fields_ = [
            ("reserved1", wintypes.LPVOID),
            ("peb", wintypes.LPVOID),
            ("reserved2_0", wintypes.LPVOID),
            ("reserved2_1", wintypes.LPVOID),
            ("unique_pid", ctypes.c_size_t),
            ("parent_pid", ctypes.c_size_t),
        ]

    basic = ProcessBasicInformation()
    returned = wintypes.ULONG()
    status = ntdll.NtQueryInformationProcess(
        handle,
        0,
        ctypes.byref(basic),
        ctypes.sizeof(basic),
        ctypes.byref(returned),
    )
    if status != 0 or int(basic.unique_pid) != pid:
        raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)

    token = wintypes.HANDLE()
    if not advapi32.OpenProcessToken(handle, 0x0008, ctypes.byref(token)):
        raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
    sid_text = wintypes.LPWSTR()
    try:
        required = wintypes.DWORD()
        advapi32.GetTokenInformation(token, 1, None, 0, ctypes.byref(required))
        if required.value == 0:
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        token_user = ctypes.create_string_buffer(required.value)
        if not advapi32.GetTokenInformation(
            token,
            1,
            token_user,
            required.value,
            ctypes.byref(required),
        ):
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        sid_pointer = ctypes.cast(token_user, ctypes.POINTER(ctypes.c_void_p))[0]
        if not advapi32.ConvertSidToStringSidW(sid_pointer, ctypes.byref(sid_text)):
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        user_sid = sid_text.value
    finally:
        if sid_text.value:
            kernel32.LocalFree(ctypes.cast(sid_text, wintypes.HLOCAL))
        kernel32.CloseHandle(token)

    package_identity = None
    package_length = wintypes.UINT()
    package_result = kernel32.GetPackageFullName(handle, ctypes.byref(package_length), None)
    if package_result == 122:
        package_buffer = ctypes.create_unicode_buffer(package_length.value)
        if kernel32.GetPackageFullName(handle, ctypes.byref(package_length), package_buffer) != 0:
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        package_identity = package_buffer.value
    elif package_result != 15_700:
        raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)

    return WorkerIdentityV1(
        pid=pid,
        creation_time_100ns=creation_time,
        token_user_sid=user_sid,
        session_id=int(session.value),
        image_path=os.path.abspath(image.value),
        package_identity=package_identity,
        parent_pid=int(basic.parent_pid),
    )


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
        command_line=f'"{resolved}" --role=worker',
        current_directory=str(Path(resolved).parent),
        inherit_handles=True,
        startf_use_std_handles=True,
        handle_list_roles=WORKER_HANDLE_ROLES,
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
class BrokerCapabilityReceiptV1:
    correlation_id: str
    source_access: int
    source_share: int
    workspace_access: int
    workspace_share: int
    source_identity: dict[str, object]
    workspace_identity: dict[str, object]
    source_granted_access: int
    workspace_granted_access: int
    originals_non_inheritable: bool
    duplicates_inheritable: bool
    duplicates_unprotected: bool
    object_types: tuple[str, str]

    @property
    def complete(self) -> bool:
        return (
            bool(self.correlation_id)
            and self.source_access == SOURCE_ROOT_ACCESS
            and self.source_share == SOURCE_ROOT_SHARE
            and self.workspace_access == WORKSPACE_ROOT_ACCESS
            and self.workspace_share == WORKSPACE_ROOT_SHARE
            and self.source_granted_access == SOURCE_ROOT_ACCESS
            and self.workspace_granted_access == WORKSPACE_ROOT_ACCESS
            and self.originals_non_inheritable is True
            and self.duplicates_inheritable is True
            and self.duplicates_unprotected is True
            and self.object_types == ("Directory", "Directory")
        )


@dataclass(frozen=True, slots=True)
class WorkerCapabilitySetV1:
    """Two broker-owned directory handles transferred exactly once to a worker."""

    source_root_handle: int
    workspace_root_handle: int
    source_access_mask: int
    workspace_access_mask: int
    source_root_identity: dict[str, object]
    workspace_root_identity: dict[str, object]
    _owner: _CapabilityHandleOwner | None = field(default=None, repr=False, compare=False)
    receipt: BrokerCapabilityReceiptV1 | None = None
    _original_owner: _CapabilityHandleOwner | None = field(default=None, repr=False, compare=False)
    _epoch: _WorkerInheritanceEpochLease | None = field(default=None, repr=False, compare=False)

    def __post_init__(self) -> None:
        if (
            type(self.source_root_handle) is not int
            or type(self.workspace_root_handle) is not int
            or self.source_root_handle <= 0
            or self.workspace_root_handle <= 0
            or self.source_root_handle == self.workspace_root_handle
            or type(self.source_access_mask) is not int
            or type(self.workspace_access_mask) is not int
            or self.source_access_mask <= 0
            or self.workspace_access_mask <= 0
            or set(self.source_root_identity) != {"volume_serial", "file_id"}
            or set(self.workspace_root_identity) != {"volume_serial", "file_id"}
            or (self.receipt is not None and not self.receipt.complete)
        ):
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")

    def bootstrap(self, correlation_id: str, deadline: QpcDeadlineV1) -> WorkerPreMainBootstrapV1:
        return WorkerPreMainBootstrapV1(
            correlation_id,
            deadline,
            self.source_root_handle,
            self.workspace_root_handle,
            self.source_access_mask,
            self.workspace_access_mask,
            self.source_root_identity,
            self.workspace_root_identity,
        )

    def revoke_parent_handles(self) -> None:
        if self._owner is None:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        self._owner.close_all()
        if self._epoch is None:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        self._epoch.release()

    def settle(self) -> None:
        failure: BaseException | None = None
        for owner in (self._owner, self._original_owner):
            if owner is None:
                continue
            try:
                owner.close_all()
            except BaseException as exc:
                failure = failure or exc
        if self._epoch is not None:
            try:
                self._epoch.release()
            except BaseException as exc:
                failure = failure or exc
        if failure is not None:
            raise ContainmentFailure(
                "cst_saved_field.containment_settle_failed", quarantine=True
            ) from failure

    @property
    def parent_handles_closed(self) -> bool:
        return self._owner is not None and not self._owner.handles


@dataclass(slots=True)
class _CapabilityHandleOwner:
    handles: set[int]
    closer: Callable[[int], object]

    def close_all(self) -> None:
        failures: list[BaseException] = []
        for handle in tuple(self.handles):
            try:
                if self.closer(handle) is False:
                    raise OSError("capability handle close failed")
            except BaseException as exc:
                failures.append(exc)
            else:
                self.handles.remove(handle)
        if failures:
            raise ContainmentFailure(
                "cst_saved_field.containment_settle_failed", quarantine=True
            ) from failures[0]


def duplicate_worker_capabilities(
    source_capability: object,
    workspace_capability: object,
    *,
    duplicator: Callable[[int, int], int] | None = None,
    closer: Callable[[int], object] | None = None,
) -> WorkerCapabilitySetV1:
    """Create broker-owned inheritable duplicates and retain their sole close owner."""

    source_handle = getattr(source_capability, "handle", None)
    workspace_handle = getattr(workspace_capability, "handle", None)
    source_evidence = getattr(source_capability, "evidence", None)
    workspace_evidence = getattr(workspace_capability, "evidence", None)
    if not all(
        (
            type(source_handle) is int,
            type(workspace_handle) is int,
            source_evidence is not None,
            workspace_evidence is not None,
        )
    ):
        raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
    if duplicator is None or closer is None:
        if os.name != "nt":
            raise OSError("Windows capability duplication is unavailable on this platform")
        import ctypes
        from ctypes import wintypes

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.GetCurrentProcess.restype = wintypes.HANDLE
        kernel32.DuplicateHandle.argtypes = (
            wintypes.HANDLE,
            wintypes.HANDLE,
            wintypes.HANDLE,
            ctypes.POINTER(wintypes.HANDLE),
            wintypes.DWORD,
            wintypes.BOOL,
            wintypes.DWORD,
        )
        kernel32.DuplicateHandle.restype = wintypes.BOOL
        current = kernel32.GetCurrentProcess()

        def duplicate(raw: int, access: int) -> int:
            duplicated = wintypes.HANDLE()
            if not kernel32.DuplicateHandle(
                current,
                wintypes.HANDLE(raw),
                current,
                ctypes.byref(duplicated),
                access,
                True,
                0,
            ):
                raise ctypes.WinError(ctypes.get_last_error())
            return int(duplicated.value)

        def close(raw: int) -> bool:
            return bool(kernel32.CloseHandle(wintypes.HANDLE(raw)))

        duplicator, closer = duplicate, close
    owner = _CapabilityHandleOwner(set(), closer)
    try:
        source_duplicate = duplicator(source_handle, 0x00120089)
        owner.handles.add(source_duplicate)
        workspace_duplicate = duplicator(workspace_handle, 0x0012019F)
        owner.handles.add(workspace_duplicate)
        return WorkerCapabilitySetV1(
            source_duplicate,
            workspace_duplicate,
            0x00120089,
            0x0012019F,
            {
                "volume_serial": source_evidence.volume_serial,
                "file_id": source_evidence.file_id,
            },
            {
                "volume_serial": workspace_evidence.volume_serial,
                "file_id": workspace_evidence.file_id,
            },
            owner,
        )
    except BaseException:
        owner.close_all()
        raise


def open_broker_worker_capabilities(
    source_path: str | Path,
    workspace_path: str | Path,
    *,
    correlation_id: str,
    expected_source_identity: dict[str, object],
    expected_workspace_identity: dict[str, object] | None = None,
    epoch: WorkerInheritanceEpoch = BROKER_WORKER_INHERITANCE_EPOCH,
) -> WorkerCapabilitySetV1:
    """Open and duplicate the two exact broker root roles inside one epoch."""

    if os.name != "nt" or not correlation_id:
        raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
    import ctypes
    from ctypes import wintypes

    from .cst_saved_field_policy import _evidence_from_handle, _owner_only_access_handle

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    ntdll = ctypes.WinDLL("ntdll")
    kernel32.CreateFileW.restype = wintypes.HANDLE
    kernel32.GetCurrentProcess.restype = wintypes.HANDLE
    kernel32.DuplicateHandle.argtypes = (
        wintypes.HANDLE,
        wintypes.HANDLE,
        wintypes.HANDLE,
        ctypes.POINTER(wintypes.HANDLE),
        wintypes.DWORD,
        wintypes.BOOL,
        wintypes.DWORD,
    )
    kernel32.DuplicateHandle.restype = wintypes.BOOL
    ntdll.NtQueryObject.argtypes = (
        wintypes.HANDLE,
        wintypes.ULONG,
        wintypes.LPVOID,
        wintypes.ULONG,
        ctypes.POINTER(wintypes.ULONG),
    )
    ntdll.NtQueryObject.restype = wintypes.LONG
    invalid = ctypes.c_void_p(-1).value

    class ObjectBasicInformation(ctypes.Structure):
        _fields_ = [
            ("Attributes", wintypes.ULONG),
            ("GrantedAccess", wintypes.ULONG),
            ("HandleCount", wintypes.ULONG),
            ("PointerCount", wintypes.ULONG),
            ("PagedPoolCharge", wintypes.ULONG),
            ("NonPagedPoolCharge", wintypes.ULONG),
            ("Reserved", wintypes.ULONG * 3),
            ("NameInfoSize", wintypes.ULONG),
            ("TypeInfoSize", wintypes.ULONG),
            ("SecurityDescriptorSize", wintypes.ULONG),
            ("CreationTime", ctypes.c_int64),
        ]

    class UnicodeString(ctypes.Structure):
        _fields_ = [
            ("Length", wintypes.USHORT),
            ("MaximumLength", wintypes.USHORT),
            ("Buffer", wintypes.LPWSTR),
        ]

    def object_type(raw: int) -> str:
        required = wintypes.ULONG()
        ntdll.NtQueryObject(wintypes.HANDLE(raw), 2, None, 0, ctypes.byref(required))
        if required.value == 0:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        buffer = ctypes.create_string_buffer(required.value)
        if ntdll.NtQueryObject(wintypes.HANDLE(raw), 2, buffer, len(buffer), ctypes.byref(required)) != 0:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        name = ctypes.cast(buffer, ctypes.POINTER(UnicodeString)).contents
        if not name.Buffer or not name.Length:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        return ctypes.wstring_at(name.Buffer, name.Length // ctypes.sizeof(ctypes.c_wchar))

    def close(raw: int) -> bool:
        return bool(kernel32.CloseHandle(wintypes.HANDLE(raw)))

    lease = epoch.acquire()
    originals = _CapabilityHandleOwner(set(), close)
    duplicates = _CapabilityHandleOwner(set(), close)
    try:
        observations: list[tuple[object, bool, int, str]] = []
        for path, access, share, expected in (
            (source_path, SOURCE_ROOT_ACCESS, SOURCE_ROOT_SHARE, expected_source_identity),
            (
                workspace_path,
                WORKSPACE_ROOT_ACCESS,
                WORKSPACE_ROOT_SHARE,
                expected_workspace_identity,
            ),
        ):
            canonical = os.path.abspath(os.fspath(path))
            handle = kernel32.CreateFileW(
                canonical,
                access,
                share,
                None,
                3,
                0x02000000 | 0x00200000,
                None,
            )
            if handle in {None, invalid}:
                raise ctypes.WinError(ctypes.get_last_error())
            raw = int(handle)
            originals.handles.add(raw)
            evidence = _evidence_from_handle(kernel32, raw, canonical, "directory")
            restricted = _owner_only_access_handle(raw)
            identity = {"volume_serial": evidence.volume_serial, "file_id": evidence.file_id}
            canonical_key = os.path.normcase(os.path.abspath(canonical)).casefold()
            final_key = os.path.normcase(os.path.abspath(evidence.canonical_path)).casefold()
            if (
                evidence.reparse
                or not restricted
                or (expected is not None and identity != expected)
                or final_key != canonical_key
            ):
                raise ContainmentFailure("cst_saved_field.broker_unauthorized")
            flags = wintypes.DWORD()
            if not kernel32.GetHandleInformation(wintypes.HANDLE(raw), ctypes.byref(flags)):
                raise ctypes.WinError(ctypes.get_last_error())
            if flags.value & 1:
                raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
            duplicate = wintypes.HANDLE()
            if not kernel32.DuplicateHandle(
                kernel32.GetCurrentProcess(),
                wintypes.HANDLE(raw),
                kernel32.GetCurrentProcess(),
                ctypes.byref(duplicate),
                access,
                True,
                0,
            ):
                raise ctypes.WinError(ctypes.get_last_error())
            duplicate_raw = int(duplicate.value)
            duplicates.handles.add(duplicate_raw)
            basic = ObjectBasicInformation()
            if (
                ntdll.NtQueryObject(
                    wintypes.HANDLE(duplicate_raw), 0, ctypes.byref(basic), ctypes.sizeof(basic), None
                )
                != 0
            ):
                raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
            duplicate_flags = wintypes.DWORD()
            if not kernel32.GetHandleInformation(
                wintypes.HANDLE(duplicate_raw), ctypes.byref(duplicate_flags)
            ):
                raise ctypes.WinError(ctypes.get_last_error())
            duplicate_evidence = _evidence_from_handle(kernel32, duplicate_raw, canonical, "directory")
            if (
                basic.GrantedAccess != access
                or duplicate_flags.value != 1
                or duplicate_evidence.volume_serial != evidence.volume_serial
                or duplicate_evidence.file_id != evidence.file_id
            ):
                raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
            observed_type = object_type(duplicate_raw)
            if observed_type != "File":
                raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
            observations.append((evidence, restricted, duplicate_raw, observed_type))
        source, workspace = observations
        receipt = BrokerCapabilityReceiptV1(
            correlation_id,
            SOURCE_ROOT_ACCESS,
            SOURCE_ROOT_SHARE,
            WORKSPACE_ROOT_ACCESS,
            WORKSPACE_ROOT_SHARE,
            {"volume_serial": source[0].volume_serial, "file_id": source[0].file_id},
            {"volume_serial": workspace[0].volume_serial, "file_id": workspace[0].file_id},
            SOURCE_ROOT_ACCESS,
            WORKSPACE_ROOT_ACCESS,
            True,
            True,
            True,
            ("Directory", "Directory"),
        )
        if not receipt.complete:
            raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
        return WorkerCapabilitySetV1(
            source[2],
            workspace[2],
            SOURCE_ROOT_ACCESS,
            WORKSPACE_ROOT_ACCESS,
            receipt.source_identity,
            receipt.workspace_identity,
            _owner=duplicates,
            receipt=receipt,
            _original_owner=originals,
            _epoch=lease,
        )
    except BaseException:
        failure: BaseException | None = None
        for owner in (duplicates, originals):
            try:
                owner.close_all()
            except BaseException as exc:
                failure = failure or exc
        try:
            lease.release()
        except BaseException as exc:
            failure = failure or exc
        if failure is not None:
            raise ContainmentFailure(
                "cst_saved_field.containment_settle_failed", quarantine=True
            ) from failure
        raise


def validate_worker_pre_main(
    bootstrap: WorkerPreMainBootstrapV1,
    receipt: WorkerPreMainReceiptV1,
    *,
    inherited_handle_roles: tuple[str, ...],
) -> WorkerPreMainReceiptV1:
    """Validate native/broker observations without manufacturing receipt facts."""

    if (
        not isinstance(bootstrap, WorkerPreMainBootstrapV1)
        or not isinstance(receipt, WorkerPreMainReceiptV1)
        or inherited_handle_roles != WORKER_HANDLE_ROLES
        or not receipt.validates(bootstrap)
    ):
        raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
    return receipt


@dataclass(frozen=True, slots=True)
class KernelInvocationResult:
    response_frame: bytes
    worker_signaled: bool
    exit_recorded: bool
    process_reference_closed: bool
    active_zero: bool
    readers_joined: bool
    handles_closed: bool
    pipe_closed: bool
    residual_process: bool
    timed_out: bool
    exit_code: int = 0
    stderr_overflow: bool = False
    first_instruction_proof: FirstInstructionProof | None = None
    containment_evidence: KernelContainmentEvidenceV1 | None = None
    pre_main_receipt: WorkerPreMainReceiptV1 | None = None
    application_available: bool = True


@dataclass(frozen=True, slots=True)
class ContainedInvocationReceiptV1:
    """Containment-owned response and causal kernel settlement evidence."""

    response_frame: bytes
    worker_signaled: bool
    exit_recorded: bool
    process_reference_closed: bool
    active_job_zero: bool
    readers_joined: bool
    handles_closed: bool
    pipe_closed: bool
    pre_main_receipt: WorkerPreMainReceiptV1 | None = None
    application_available: bool = True

    @property
    def complete(self) -> bool:
        return (
            type(self.response_frame) is bytes
            and self.worker_signaled is True
            and self.exit_recorded is True
            and self.process_reference_closed is True
            and self.active_job_zero is True
            and self.readers_joined is True
            and self.handles_closed is True
            and self.pipe_closed is True
            and isinstance(self.pre_main_receipt, WorkerPreMainReceiptV1)
        )


class WindowsKernel(Protocol):
    def invoke(
        self,
        spec: CreateProcessSpec,
        request_frame: bytes | Callable[[], bytes],
        *,
        bootstrap: WorkerPreMainBootstrapV1,
        capabilities: WorkerCapabilitySetV1,
        startup_validator: Callable[[FirstInstructionProof], None] | None = None,
        startup_deadline: float,
        response_deadline: float,
        absolute_deadline: float,
        cleanup_deadline: float,
    ) -> KernelInvocationResult: ...


class WindowsContainedInvocation:
    def __init__(
        self,
        *,
        kernel: WindowsKernel,
        executable: str,
        qpc_frequency: Callable[[], int] = _default_qpc_frequency,
        qpc_counter: Callable[[], int] = _default_qpc_counter,
        monotonic: Callable[[], float] = time.monotonic,
    ) -> None:
        self._kernel = kernel
        self._executable = executable
        self._qpc_frequency = qpc_frequency
        self._qpc_counter = qpc_counter
        self._monotonic = monotonic

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
    def _validate_result(
        cls,
        result: KernelInvocationResult,
        *,
        bootstrap: WorkerPreMainBootstrapV1,
    ) -> ContainedInvocationReceiptV1:
        settled = (
            result.worker_signaled
            and result.exit_recorded
            and result.process_reference_closed
            and result.active_zero
            and result.readers_joined
            and result.handles_closed
            and result.pipe_closed
        )
        if not settled:
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)
        if result.residual_process:
            raise ContainmentFailure("cst_saved_field.containment_residual_process")
        if result.timed_out:
            raise ContainmentFailure("cst_saved_field.deadline_exceeded")
        if result.stderr_overflow or result.exit_code not in {0, 78}:
            raise ContainmentFailure("cst_saved_field.broker_worker_protocol_invalid")
        if result.first_instruction_proof is None:
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        cls._validate_startup(result.first_instruction_proof)
        if result.containment_evidence is None:
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)
        result.containment_evidence.validate()
        validate_worker_pre_main(
            bootstrap,
            result.pre_main_receipt,
            inherited_handle_roles=result.containment_evidence.inherited_handle_roles,
        )
        if result.application_available != (result.exit_code == 0):
            raise ContainmentFailure("cst_saved_field.broker_worker_protocol_invalid", quarantine=True)
        return ContainedInvocationReceiptV1(
            response_frame=result.response_frame,
            worker_signaled=result.worker_signaled,
            exit_recorded=result.exit_recorded,
            process_reference_closed=result.process_reference_closed,
            active_job_zero=result.active_zero,
            readers_joined=result.readers_joined,
            handles_closed=result.handles_closed,
            pipe_closed=result.pipe_closed,
            pre_main_receipt=result.pre_main_receipt,
            application_available=result.application_available,
        )

    def invoke_after_startup(
        self,
        request_factory: Callable[[], bytes],
        *,
        deadline: QpcDeadlineV1,
        correlation_id: str,
        capabilities: WorkerCapabilitySetV1,
    ) -> ContainedInvocationReceiptV1:
        frequency = self._qpc_frequency()
        current_tick = self._qpc_counter()
        try:
            remaining = deadline.remaining(
                current_frequency=frequency,
                current_tick=current_tick,
            )
        except ValueError as exc:
            raise ContainmentFailure("cst_saved_field.deadline_exceeded") from exc
        started = self._monotonic()
        absolute_deadline = started + remaining

        def check_deadline() -> None:
            if self._qpc_frequency() != frequency or self._qpc_counter() >= deadline.deadline_tick:
                raise ContainmentFailure("cst_saved_field.deadline_exceeded")

        check_deadline()
        with PinnedExecutable.open(self._executable, check_deadline=check_deadline) as executable:
            identity = executable.revalidate()
            check_deadline()
            spec = validate_create_process_spec(
                build_create_process_spec(identity.final_path), identity.final_path
            )
            bootstrap = capabilities.bootstrap(correlation_id, deadline)
            result = self._kernel.invoke(
                spec,
                request_factory,
                bootstrap=bootstrap,
                capabilities=capabilities,
                startup_validator=self._validate_startup,
                startup_deadline=min(started + 5.0, absolute_deadline),
                response_deadline=max(started, absolute_deadline - 2.0),
                absolute_deadline=absolute_deadline,
                cleanup_deadline=absolute_deadline + 10.0,
            )
            executable.revalidate()
            check_deadline()
        return self._validate_result(result, bootstrap=bootstrap)

    def invoke(
        self,
        request_frame: bytes,
        *,
        deadline: QpcDeadlineV1,
        correlation_id: str,
        capabilities: WorkerCapabilitySetV1,
    ) -> ContainedInvocationReceiptV1:
        return self.invoke_after_startup(
            lambda: request_frame,
            deadline=deadline,
            correlation_id=correlation_id,
            capabilities=capabilities,
        )


class ContainedSamplerRunner:
    """Bind one admission lease to one contained invocation and settle it once."""

    def __init__(self, *, gate: SamplerAdmissionGate, invocation: WindowsContainedInvocation) -> None:
        self._gate = gate
        self._invocation = invocation

    def invoke(
        self,
        request_frame: bytes,
        *,
        deadline: QpcDeadlineV1,
        correlation_id: str,
        capabilities: WorkerCapabilitySetV1,
        revision: str,
        wait_seconds: float,
    ) -> bytes:
        lease = self._gate.acquire_and_seal(revision, wait_seconds=wait_seconds)
        try:
            lease.authorize_start()
        except BaseException:
            self._gate.release(lease)
            raise
        try:
            response = self._invocation.invoke(
                request_frame,
                deadline=deadline,
                correlation_id=correlation_id,
                capabilities=capabilities,
            )
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
        return response.response_frame

    def invoke_after_admission(
        self,
        request_factory,
        *,
        deadline: QpcDeadlineV1,
        correlation_id: str,
        capabilities: WorkerCapabilitySetV1,
        revision: str,
        wait_seconds: float,
        response_consumer=None,
    ):
        """Seal/recheck admission before descriptor construction or source work."""
        lease = self._gate.acquire_and_seal(revision, wait_seconds=wait_seconds)
        try:
            lease.authorize_start()
        except BaseException:
            self._gate.release(lease)
            raise
        try:
            factory_error: BaseException | None = None

            def create_frame() -> bytes:
                nonlocal factory_error
                try:
                    built = request_factory(deadline)
                    return built[0] if isinstance(built, tuple) else built
                except BaseException as exc:
                    factory_error = exc
                    raise

            receipt = self._invocation.invoke_after_startup(
                create_frame,
                deadline=deadline,
                correlation_id=correlation_id,
                capabilities=capabilities,
            )
            response = receipt.response_frame
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
        bootstrap: WorkerPreMainBootstrapV1,
        capabilities: WorkerCapabilitySetV1,
        startup_validator: Callable[[FirstInstructionProof], None] | None = None,
        startup_deadline: float,
        response_deadline: float,
        absolute_deadline: float,
        cleanup_deadline: float,
    ) -> KernelInvocationResult:
        return _invoke_atomic_job_process(
            spec,
            request_frame,
            bootstrap=bootstrap,
            capabilities=capabilities,
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
    bootstrap: WorkerPreMainBootstrapV1,
    capabilities: WorkerCapabilitySetV1,
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

    if not isinstance(capabilities, WorkerCapabilitySetV1) or capabilities.parent_handles_closed:
        raise ContainmentFailure("cst_saved_field.containment_configuration_invalid")
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
    created_worker: WorkerIdentityV1 | None = None
    pre_request_worker: WorkerIdentityV1 | None = None
    settlement_events: list[str] = []
    worker_signaled = False
    exit_recorded = False
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
        settlement_events.append("job_configured")

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
        capability_handles = (
            handle_t(capabilities.source_root_handle),
            handle_t(capabilities.workspace_root_handle),
        )
        handle_array = (handle_t * 5)(*child_ends, *capability_handles)
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
        settlement_events.append("attributes_bound")
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
        for child_handle in child_ends:
            ledger.close_one(int(child_handle.value), close)
        capabilities.revoke_parent_handles()
        settlement_events.append("capability_handles_revoked")
        settlement_events.append("process_created")
        ledger.own(int(process.hProcess))
        ledger.own(int(process.hThread))
        created_worker = _read_windows_worker_identity(int(process.hProcess), int(process.dwProcessId))
        broker_identity = _read_windows_worker_identity(
            int(kernel32.GetCurrentProcess()),
            os.getpid(),
        )
        if (
            os.path.normcase(created_worker.image_path) != os.path.normcase(spec.application_name)
            or created_worker.parent_pid != os.getpid()
            or created_worker.token_user_sid != broker_identity.token_user_sid
            or created_worker.session_id != broker_identity.session_id
            or created_worker.package_identity != broker_identity.package_identity
        ):
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        settlement_events.append("identity_bound")
        in_exact_job = wintypes.BOOL()
        checked(
            kernel32.IsProcessInJob(process.hProcess, handle_t(job), ctypes.byref(in_exact_job)),
            "IsProcessInJob",
        )
        exact_job = bool(in_exact_job.value)
        if not exact_job:
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        settlement_events.append("job_membership_verified")
        ledger.close_one(int(process.hThread), close)

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
        pre_request_worker = _read_windows_worker_identity(
            int(process.hProcess),
            int(process.dwProcessId),
        )
        if pre_request_worker != created_worker:
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        bootstrap_frame = encode_pre_main_bootstrap_frame(bootstrap)
        bootstrap_buffer = ctypes.create_string_buffer(bootstrap_frame)
        bootstrap_written = wintypes.DWORD()
        checked(
            kernel32.WriteFile(
                stdin_write,
                bootstrap_buffer,
                len(bootstrap_frame),
                ctypes.byref(bootstrap_written),
                None,
            ),
            "WriteFile(worker bootstrap)",
        )
        if bootstrap_written.value != len(bootstrap_frame):
            raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
        settlement_events.append("bootstrap_written")
        pre_main_receipt = None
        receipt_deadline = min(absolute_deadline, startup_deadline)
        while time.monotonic() < receipt_deadline:
            raw = bytes(stdout_data)
            if len(raw) >= 4:
                receipt_length = int.from_bytes(raw[:4], "big")
                if receipt_length == 0 or receipt_length + 4 > WORKER_PRE_MAIN_FRAME_MAX:
                    raise ContainmentFailure("cst_saved_field.containment_startup_invalid", quarantine=True)
                if len(raw) >= receipt_length + 4:
                    try:
                        pre_main_receipt = decode_pre_main_receipt_frame(
                            __import__("io").BytesIO(raw[: receipt_length + 4])
                        )
                    except BaseException as exc:
                        raise ContainmentFailure(
                            "cst_saved_field.containment_startup_invalid", quarantine=True
                        ) from exc
                    del stdout_data[: receipt_length + 4]
                    break
            time.sleep(0.005)
        validate_worker_pre_main(
            bootstrap,
            pre_main_receipt,
            inherited_handle_roles=spec.handle_list_roles,
        )
        settlement_events.append("pre_main_receipt_validated")
        factory_values: list[bytes] = []
        factory_errors: list[BaseException] = []

        def build_request() -> None:
            try:
                factory_values.append(request_frame() if callable(request_frame) else request_frame)
            except BaseException as exc:
                factory_errors.append(exc)

        application_available = pre_main_receipt.python_initialized
        writer_joined = True
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

        if application_available:
            settlement_events.append("request_started")
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
        if worker_signaled:
            settlement_events.append("worker_signaled")
        timed_out = timed_out or not worker_signaled
        if timed_out:
            kernel32.TerminateJobObject(handle_t(job), 1)
            cleanup_ms = max(0, int((cleanup_deadline - time.monotonic()) * 1000))
            worker_signaled = kernel32.WaitForSingleObject(process.hProcess, cleanup_ms) == 0
        checked(kernel32.GetExitCodeProcess(process.hProcess, ctypes.byref(exit_code)), "GetExitCodeProcess")
        exit_recorded = True
        settlement_events.append("exit_recorded")
        ledger.close_one(int(process.hProcess), close)
        process_closed = True
        settlement_events.append("process_reference_closed")
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
        if active_zero:
            settlement_events.append("job_active_zero")
        reader_results = tuple(worker.settle(time.monotonic(), cleanup_deadline) for worker in reader_workers)
        readers_joined = writer_joined and all(reader_results)
        if readers_joined:
            settlement_events.append("readers_joined")
        if readers_joined:
            for parent_reader in (stdout_read, stderr_read):
                ledger.close_one(int(parent_reader.value), close)
        if active_zero and readers_joined:
            ledger.close_one(int(job), close)
        handles_closed = not ledger.handles
        pipe_handles = {
            int(stdin_read.value),
            int(stdin_write.value),
            int(stdout_read.value),
            int(stdout_write.value),
            int(stderr_read.value),
            int(stderr_write.value),
        }
        pipe_closed = not any(handle in ledger.handles for handle in pipe_handles)
        if handles_closed:
            settlement_events.append("handles_closed")
        if created_worker is None or pre_request_worker is None:
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True)
        return KernelInvocationResult(
            response_frame=bytes(stdout_data),
            worker_signaled=worker_signaled,
            exit_recorded=exit_recorded,
            process_reference_closed=process_closed,
            active_zero=active_zero,
            readers_joined=readers_joined,
            handles_closed=handles_closed,
            pipe_closed=pipe_closed,
            residual_process=residual,
            timed_out=timed_out,
            exit_code=int(exit_code.value),
            stderr_overflow=len(stderr_data) > 65_536,
            first_instruction_proof=first_instruction_proof,
            containment_evidence=KernelContainmentEvidenceV1(
                created_worker=created_worker,
                pre_request_worker=pre_request_worker,
                job_member_before_request=exact_job,
                inherited_handle_roles=spec.handle_list_roles,
                settlement_events=tuple(settlement_events),
                foreign_process_operations=0,
            ),
            pre_main_receipt=pre_main_receipt,
            application_available=application_available,
        )
    finally:
        if startup.lpAttributeList:
            kernel32.DeleteProcThreadAttributeList(startup.lpAttributeList)
        failures = ledger.settle(close)
        capability_failure: BaseException | None = None
        try:
            capabilities.settle()
        except BaseException as exc:
            capability_failure = exc
        if failures or capability_failure is not None:
            raise ContainmentFailure("cst_saved_field.containment_settle_failed", quarantine=True) from (
                failures[0] if failures else capability_failure
            )
