"""Creation-time Windows Job ownership for one direct process and its descendants.

This module owns native process and Job handles. A caller owns each binary file
descriptor only after ``take_*_fd`` transfers it. No PID-based operation exists.
"""

from __future__ import annotations

import ctypes
from ctypes import wintypes
from dataclasses import dataclass
from functools import cmp_to_key
import math
import os
import subprocess
import threading
import time
from typing import Callable, Literal, Mapping, Sequence


WINDOWS_JOB_MODULE_CONTRACT_V1 = "orchestrarium.windows-job.module.v1"


class WindowsJobError(RuntimeError):
    def __init__(self, failure_id: str, stage: str) -> None:
        super().__init__(f"{failure_id}:{stage}")
        self.failure_id = failure_id
        self.stage = stage


class WindowsInheritanceCoordinatorV1:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self._poisoned = False

    @property
    def poisoned(self) -> bool:
        return self._poisoned

    def poison(self) -> None:
        self._poisoned = True


SIZE_T = ctypes.c_size_t
ULONG_PTR = wintypes.WPARAM


class SECURITY_ATTRIBUTES(ctypes.Structure):
    _fields_ = [
        ("nLength", wintypes.DWORD),
        ("lpSecurityDescriptor", ctypes.c_void_p),
        ("bInheritHandle", wintypes.BOOL),
    ]


class STARTUPINFOW(ctypes.Structure):
    _fields_ = [
        ("cb", wintypes.DWORD), ("lpReserved", wintypes.LPWSTR),
        ("lpDesktop", wintypes.LPWSTR), ("lpTitle", wintypes.LPWSTR),
        ("dwX", wintypes.DWORD), ("dwY", wintypes.DWORD),
        ("dwXSize", wintypes.DWORD), ("dwYSize", wintypes.DWORD),
        ("dwXCountChars", wintypes.DWORD), ("dwYCountChars", wintypes.DWORD),
        ("dwFillAttribute", wintypes.DWORD), ("dwFlags", wintypes.DWORD),
        ("wShowWindow", wintypes.WORD), ("cbReserved2", wintypes.WORD),
        ("lpReserved2", ctypes.POINTER(ctypes.c_ubyte)),
        ("hStdInput", wintypes.HANDLE), ("hStdOutput", wintypes.HANDLE),
        ("hStdError", wintypes.HANDLE),
    ]


class STARTUPINFOEXW(ctypes.Structure):
    _fields_ = [("StartupInfo", STARTUPINFOW), ("lpAttributeList", ctypes.c_void_p)]


class PROCESS_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("hProcess", wintypes.HANDLE), ("hThread", wintypes.HANDLE),
        ("dwProcessId", wintypes.DWORD), ("dwThreadId", wintypes.DWORD),
    ]


class JOBOBJECT_BASIC_LIMIT_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("PerProcessUserTimeLimit", ctypes.c_longlong),
        ("PerJobUserTimeLimit", ctypes.c_longlong),
        ("LimitFlags", wintypes.DWORD),
        ("MinimumWorkingSetSize", SIZE_T),
        ("MaximumWorkingSetSize", SIZE_T),
        ("ActiveProcessLimit", wintypes.DWORD),
        ("Affinity", ULONG_PTR),
        ("PriorityClass", wintypes.DWORD),
        ("SchedulingClass", wintypes.DWORD),
    ]


class IO_COUNTERS(ctypes.Structure):
    _fields_ = [(name, ctypes.c_ulonglong) for name in (
        "ReadOperationCount", "WriteOperationCount", "OtherOperationCount",
        "ReadTransferCount", "WriteTransferCount", "OtherTransferCount",
    )]


class JOBOBJECT_EXTENDED_LIMIT_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("BasicLimitInformation", JOBOBJECT_BASIC_LIMIT_INFORMATION),
        ("IoInfo", IO_COUNTERS), ("ProcessMemoryLimit", SIZE_T),
        ("JobMemoryLimit", SIZE_T), ("PeakProcessMemoryUsed", SIZE_T),
        ("PeakJobMemoryUsed", SIZE_T),
    ]


class JOBOBJECT_BASIC_ACCOUNTING_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("TotalUserTime", ctypes.c_longlong),
        ("TotalKernelTime", ctypes.c_longlong),
        ("ThisPeriodTotalUserTime", ctypes.c_longlong),
        ("ThisPeriodTotalKernelTime", ctypes.c_longlong),
        ("TotalPageFaultCount", wintypes.DWORD),
        ("TotalProcesses", wintypes.DWORD),
        ("ActiveProcesses", wintypes.DWORD),
        ("TotalTerminatedProcesses", wintypes.DWORD),
    ]


class WindowsKernelV1:
    WAIT_OBJECT_0 = 0
    WAIT_TIMEOUT = 258
    INFINITE = 0xFFFFFFFF
    HANDLE_FLAG_INHERIT = 1
    STARTF_USESTDHANDLES = 0x100
    CREATE_SUSPENDED = 0x4
    CREATE_UNICODE_ENVIRONMENT = 0x400
    EXTENDED_STARTUPINFO_PRESENT = 0x80000
    JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x2000
    PROC_THREAD_ATTRIBUTE_HANDLE_LIST = 0x00020002
    PROC_THREAD_ATTRIBUTE_JOB_LIST = 0x0002000D
    STILL_ACTIVE = 259
    GENERIC_READ = 0x80000000
    GENERIC_WRITE = 0x40000000
    FILE_READ_ATTRIBUTES = 0x00000080
    FILE_SHARE_READ = 0x00000001
    FILE_SHARE_WRITE = 0x00000002
    OPEN_EXISTING = 3
    FILE_ATTRIBUTE_NORMAL = 0x00000080
    FILE_ATTRIBUTE_DIRECTORY = 0x00000010
    FILE_ATTRIBUTE_REPARSE_POINT = 0x00000400
    FILE_FLAG_BACKUP_SEMANTICS = 0x02000000
    FILE_FLAG_OPEN_REPARSE_POINT = 0x00200000
    INVALID_HANDLE_VALUE = ctypes.c_void_p(-1).value

    def __init__(self) -> None:
        if os.name != "nt":
            raise WindowsJobError("WJOB-UNAVAILABLE", "acquire")
        self.k32 = ctypes.WinDLL("kernel32", use_last_error=True)
        self._bind()

    def _bind(self) -> None:
        k = self.k32
        k.CreatePipe.argtypes = [ctypes.POINTER(wintypes.HANDLE), ctypes.POINTER(wintypes.HANDLE), ctypes.POINTER(SECURITY_ATTRIBUTES), wintypes.DWORD]
        k.CreatePipe.restype = wintypes.BOOL
        k.SetHandleInformation.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.DWORD]
        k.SetHandleInformation.restype = wintypes.BOOL
        k.CloseHandle.argtypes = [wintypes.HANDLE]
        k.CloseHandle.restype = wintypes.BOOL
        k.CreateJobObjectW.argtypes = [ctypes.c_void_p, wintypes.LPCWSTR]
        k.CreateJobObjectW.restype = wintypes.HANDLE
        k.SetInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
        k.SetInformationJobObject.restype = wintypes.BOOL
        k.QueryInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(wintypes.DWORD)]
        k.QueryInformationJobObject.restype = wintypes.BOOL
        k.InitializeProcThreadAttributeList.argtypes = [ctypes.c_void_p, wintypes.DWORD, wintypes.DWORD, ctypes.POINTER(SIZE_T)]
        k.InitializeProcThreadAttributeList.restype = wintypes.BOOL
        k.UpdateProcThreadAttribute.argtypes = [ctypes.c_void_p, wintypes.DWORD, SIZE_T, ctypes.c_void_p, SIZE_T, ctypes.c_void_p, ctypes.c_void_p]
        k.UpdateProcThreadAttribute.restype = wintypes.BOOL
        k.DeleteProcThreadAttributeList.argtypes = [ctypes.c_void_p]
        k.DeleteProcThreadAttributeList.restype = None
        k.CreateProcessW.argtypes = [wintypes.LPCWSTR, wintypes.LPWSTR, ctypes.c_void_p, ctypes.c_void_p, wintypes.BOOL, wintypes.DWORD, ctypes.c_void_p, wintypes.LPCWSTR, ctypes.POINTER(STARTUPINFOW), ctypes.POINTER(PROCESS_INFORMATION)]
        k.CreateProcessW.restype = wintypes.BOOL
        k.CreateFileW.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD, ctypes.c_void_p, wintypes.DWORD, wintypes.DWORD, wintypes.HANDLE]
        k.CreateFileW.restype = wintypes.HANDLE
        k.IsProcessInJob.argtypes = [wintypes.HANDLE, wintypes.HANDLE, ctypes.POINTER(wintypes.BOOL)]
        k.IsProcessInJob.restype = wintypes.BOOL
        k.ResumeThread.argtypes = [wintypes.HANDLE]
        k.ResumeThread.restype = wintypes.DWORD
        k.TerminateJobObject.argtypes = [wintypes.HANDLE, wintypes.UINT]
        k.TerminateJobObject.restype = wintypes.BOOL
        k.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
        k.WaitForSingleObject.restype = wintypes.DWORD
        k.GetExitCodeProcess.argtypes = [wintypes.HANDLE, ctypes.POINTER(wintypes.DWORD)]
        k.GetExitCodeProcess.restype = wintypes.BOOL
        k.CompareStringOrdinal.argtypes = [wintypes.LPCWSTR, ctypes.c_int, wintypes.LPCWSTR, ctypes.c_int, wintypes.BOOL]
        k.CompareStringOrdinal.restype = ctypes.c_int

    def serialize_environment(
        self, items: Sequence[tuple[str, str]]
    ) -> tuple[str, bool]:
        """Preserve native strings and stable ordinal order, without caller policy."""
        def compare(left: tuple[str, str], right: tuple[str, str]) -> int:
            try:
                left_units = len(left[0].encode("utf-16-le", "surrogatepass")) // 2
                right_units = len(right[0].encode("utf-16-le", "surrogatepass")) // 2
                result = self.k32.CompareStringOrdinal(
                    left[0], left_units, right[0], right_units, True
                )
            except Exception:
                # Preserve the private context, without exposing native input text.
                raise WindowsJobError("WJOB-REQUEST", "environment-serialization") from None
            if result not in (1, 2, 3):
                raise WindowsJobError("WJOB-REQUEST", "environment-serialization")
            return result - 2

        ordered = sorted(items, key=cmp_to_key(compare))
        duplicates = any(
            compare(ordered[index - 1], ordered[index]) == 0
            for index in range(1, len(ordered))
        )
        return "\0".join(f"{name}={value}" for name, value in ordered) + "\0\0", duplicates

    def close(self, handle: int | None) -> bool:
        return not handle or bool(self.k32.CloseHandle(handle))


@dataclass(frozen=True)
class WindowsJobCreateProcessV1:
    attempted: bool
    created: bool
    win32_last_error: int | None


@dataclass(frozen=True)
class WindowsJobClosureV1:
    direct_exit_code: int | None
    direct_reaped: bool
    active_zero: bool
    job_terminated: bool
    handles_closed: bool
    issues: tuple[str, ...]

    @property
    def complete(self) -> bool:
        return self.direct_reaped and self.active_zero and self.handles_closed and not self.issues


class WindowsJobProcessV1:
    def __init__(self, api: WindowsKernelV1) -> None:
        self._api = api
        self._handles: dict[str, int | None] = {
            name: None for name in (
                "stdin_child", "stdin_parent", "stdout_child", "stdout_parent",
                "stderr_child", "stderr_parent", "job", "process", "thread",
            )
        }
        self._fds: dict[str, int | None] = {name: None for name in ("stdin", "stdout", "stderr")}
        self._attribute_buffer: ctypes.Array | None = None
        self._attribute_initialized = False
        self._pid = 0
        self._exit_code: int | None = None
        self._job_terminated = False
        self._issues: list[str] = []
        self._closure: WindowsJobClosureV1 | None = None
        self._process_creation: WindowsJobCreateProcessV1 | None = None
        self._physical_closure: WindowsJobClosureV1 | None = None
        self._original_binding: dict | None = None
        self._handle_close_attempted: set[str] = set()
        self._fd_close_attempted: set[str] = set()

    @property
    def pid(self) -> int:
        return self._pid

    def _close_handle(self, name: str) -> None:
        handle = self._handles[name]
        if handle is None:
            return
        if name in self._handle_close_attempted:
            return
        self._handle_close_attempted.add(name)
        if not self._api.close(handle):
            self._issues.append("WJOB-HANDLE-CLOSE")
            return  # Keep the actual unreceived handle; never retry CloseHandle.
        self._handles[name] = None

    def original_binding(self) -> dict:
        """DATA marker for this actual object and its original native handles."""
        if self._original_binding is None:
            self._original_binding = {
                "schema": "ordinary-original-native-owner-binding-v1",
                "object_id": id(self), "pid": self._pid,
                "process": self._handles["process"], "job": self._handles["job"],
            }
        return dict(self._original_binding)

    def physical_binding(self, original_closure: WindowsJobClosureV1) -> dict:
        if original_closure is not self._closure:
            raise WindowsJobError("WJOB-ORIGINAL-CLOSURE", "physical-receiving")
        binding = self.original_binding()
        current = {name: self._handles[name] for name in ("process", "job")}
        physically_closed = self._physical_closure is not None and self._physical_closure.complete
        if not original_closure.direct_reaped and not physically_closed:
            if not binding["process"] or current["process"] != binding["process"]:
                raise WindowsJobError("WJOB-ORIGINAL-HANDLE", "physical-receiving")
        if not original_closure.handles_closed and not physically_closed:
            if not binding["job"] or current["job"] != binding["job"]:
                raise WindowsJobError("WJOB-ORIGINAL-HANDLE", "physical-receiving")
        return {"original_owner_binding": binding, "retained_native_handles_before": current,
                "original_closure_bound": True}

    def _take_fd(self, name: str) -> int | None:
        fd = self._fds[name]
        self._fds[name] = None
        return fd

    def take_stdin_fd(self) -> int | None:
        return self._take_fd("stdin")

    def take_stdout_fd(self) -> int | None:
        return self._take_fd("stdout")

    def take_stderr_fd(self) -> int | None:
        return self._take_fd("stderr")

    def _active_processes(self) -> int:
        job = self._handles["job"]
        if job is None:
            raise WindowsJobError("WJOB-JOB-CLOSED", "settlement")
        accounting = JOBOBJECT_BASIC_ACCOUNTING_INFORMATION()
        returned = wintypes.DWORD()
        if not self._api.k32.QueryInformationJobObject(
            job, 1, ctypes.byref(accounting), ctypes.sizeof(accounting), ctypes.byref(returned)
        ):
            raise WindowsJobError("WJOB-JOB-QUERY", "settlement")
        return int(accounting.ActiveProcesses)

    def poll(self) -> int | None:
        handle = self._handles["process"]
        if handle is None:
            return self._exit_code
        result = self._api.k32.WaitForSingleObject(handle, 0)
        if result == self._api.WAIT_TIMEOUT:
            return None
        if result != self._api.WAIT_OBJECT_0:
            raise WindowsJobError("WJOB-DIRECT-WAIT", "execution")
        code = wintypes.DWORD()
        if not self._api.k32.GetExitCodeProcess(handle, ctypes.byref(code)):
            raise WindowsJobError("WJOB-DIRECT-EXIT", "execution")
        self._exit_code = ctypes.c_int32(code.value).value
        return self._exit_code

    def wait(self, deadline: float) -> int | None:
        if not math.isfinite(deadline):
            raise WindowsJobError("WJOB-DEADLINE", "execution")
        if self._handles["process"] is None:
            return self._exit_code
        while True:
            code = self.poll()
            if code is not None:
                return code
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            handle = self._handles["process"]
            assert handle is not None
            result = self._api.k32.WaitForSingleObject(
                handle, max(1, min(0xFFFFFFFE, int(remaining * 1000)))
            )
            if result not in (self._api.WAIT_OBJECT_0, self._api.WAIT_TIMEOUT):
                raise WindowsJobError("WJOB-DIRECT-WAIT", "execution")

    def terminate_job(self) -> bool:
        job = self._handles["job"]
        if job is None:
            return False
        terminated = bool(self._api.k32.TerminateJobObject(job, 1))
        self._job_terminated = self._job_terminated or terminated
        if not terminated:
            self._issues.append("WJOB-JOB-TERMINATE")
        return terminated

    def _settle_original_handles(self, deadline: float, *, terminate: bool = False) -> WindowsJobClosureV1:
        if not math.isfinite(deadline):
            raise WindowsJobError("WJOB-DEADLINE", "settlement")
        if terminate:
            self.terminate_job()
        active_zero = self._handles["job"] is None and self._pid == 0
        try:
            while self._handles["job"] is not None:
                if self._active_processes() == 0:
                    active_zero = True
                    break
                now = time.monotonic()
                if not self._job_terminated and now >= deadline - 0.25:
                    self.terminate_job()
                if now >= deadline:
                    self._issues.append("WJOB-JOB-NONEMPTY")
                    break
                time.sleep(min(0.01, max(0.0, deadline - now)))
        except WindowsJobError as exc:
            self._issues.append(exc.failure_id)
            self.terminate_job()
        try:
            self.wait(deadline)
        except WindowsJobError as exc:
            self._issues.append(exc.failure_id)
        # A pending original wait must keep the same process/Job handles.
        # Deadline and other failures remain failures; they are never a result.
        if (self._exit_code is None and self._pid != 0) or not active_zero or self._issues:
            return WindowsJobClosureV1(self._exit_code, self._exit_code is not None or self._pid == 0,
                                       active_zero, self._job_terminated, False, tuple(self._issues))
        for name, fd in self._fds.items():
            if fd is not None and name not in self._fd_close_attempted:
                self._fd_close_attempted.add(name)
                try:
                    os.close(fd)
                except OSError:
                    self._issues.append("WJOB-FD-CLOSE")
                else:
                    self._fds[name] = None
        for name in ("thread", "process", "stdin_child", "stdin_parent", "stdout_child", "stdout_parent", "stderr_child", "stderr_parent"):
            self._close_handle(name)
        if self._attribute_initialized and self._attribute_buffer is not None:
            try:
                self._api.k32.DeleteProcThreadAttributeList(
                    ctypes.cast(self._attribute_buffer, ctypes.c_void_p)
                )
            except BaseException:
                self._issues.append("WJOB-ATTRIBUTE-CLOSE")
            self._attribute_initialized = False
        self._close_handle("job")
        return WindowsJobClosureV1(
            self._exit_code, self._exit_code is not None or self._pid == 0, active_zero,
            self._job_terminated, all(value is None for value in self._handles.values())
            and all(value is None for value in self._fds.values()),
            tuple(self._issues),
        )

    def settle(self, deadline: float, *, terminate: bool = False) -> WindowsJobClosureV1:
        if self._closure is not None:
            return self._closure
        self.original_binding()
        self._closure = self._settle_original_handles(deadline, terminate=terminate)
        return self._closure

    def receive_failed_closure(self, original_closure: WindowsJobClosureV1,
                               deadline: float) -> WindowsJobClosureV1:
        """Physical receiving only; the original qualification snapshot stays fixed."""
        self.physical_binding(original_closure)
        if original_closure.complete:
            return original_closure
        if self._physical_closure is not None and self._physical_closure.complete:
            return self._physical_closure
        # This narrow repair covers only a pending direct wait after positive
        # active-zero and no API error. Sticky native/close errors stay UNKNOWN.
        if (original_closure.direct_reaped or not original_closure.active_zero
                or original_closure.handles_closed or original_closure.issues or self._issues):
            return self._physical_closure or original_closure
        self._physical_closure = self._settle_original_handles(deadline)
        return self._physical_closure

    def close(self) -> WindowsJobClosureV1:
        return self.settle(time.monotonic() + 5.0, terminate=True)


class WindowsJobOwnerV1:
    @classmethod
    def launch(
        cls,
        *,
        executable: str,
        argv: Sequence[str],
        cwd: str | None,
        environment: Mapping[str, str] | None,
        stdin: Literal["pipe", "null"] = "pipe",
        stdout: Literal["pipe", "null"] = "pipe",
        stderr: Literal["pipe", "null"] = "pipe",
        create_gate: Callable[[Callable[[], bool]], bool] | None = None,
        before_resume: Callable[[], None] | None = None,
        coordinator: WindowsInheritanceCoordinatorV1 | None = None,
        api: WindowsKernelV1 | None = None,
    ) -> WindowsJobProcessV1:
        if os.name != "nt":
            raise WindowsJobError("WJOB-UNAVAILABLE", "launch")
        import msvcrt
        if not os.path.isabs(executable) or not argv or argv[0] != executable:
            raise WindowsJobError("WJOB-REQUEST", "launch")
        if any(mode not in ("pipe", "null") for mode in (stdin, stdout, stderr)):
            raise WindowsJobError("WJOB-REQUEST", "launch")
        if coordinator is None:
            coordinator = WindowsInheritanceCoordinatorV1()
        if coordinator.poisoned:
            raise WindowsJobError("WJOB-INHERITANCE-POISONED", "launch")
        process = WindowsJobProcessV1(api if api is not None else WindowsKernelV1())
        k = process._api.k32
        h = process._handles
        sa = SECURITY_ATTRIBUTES(ctypes.sizeof(SECURITY_ATTRIBUTES), None, False)
        attr_pointer: ctypes.c_void_p | None = None
        try:
            for name, mode in (("stdin", stdin), ("stdout", stdout), ("stderr", stderr)):
                if mode == "null":
                    access = process._api.GENERIC_READ if name == "stdin" else process._api.GENERIC_WRITE
                    child = k.CreateFileW(
                        "NUL", access,
                        process._api.FILE_SHARE_READ | process._api.FILE_SHARE_WRITE,
                        None, process._api.OPEN_EXISTING, process._api.FILE_ATTRIBUTE_NORMAL, None,
                    )
                    if not child or child == process._api.INVALID_HANDLE_VALUE:
                        raise WindowsJobError("WJOB-STDIO", "handle-preparation")
                    h[f"{name}_child"] = int(child)
                    continue
                read = wintypes.HANDLE()
                write = wintypes.HANDLE()
                if not k.CreatePipe(ctypes.byref(read), ctypes.byref(write), ctypes.byref(sa), 0):
                    raise WindowsJobError("WJOB-STDIO", "handle-preparation")
                if name == "stdin":
                    h["stdin_child"], h["stdin_parent"] = read.value, write.value
                else:
                    h[f"{name}_parent"], h[f"{name}_child"] = read.value, write.value
            job = k.CreateJobObjectW(None, None)
            if not job:
                raise WindowsJobError("WJOB-JOB-CREATE", "handle-preparation")
            h["job"] = int(job)
            limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION()
            limits.BasicLimitInformation.LimitFlags = process._api.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
            if not k.SetInformationJobObject(job, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
                raise WindowsJobError("WJOB-JOB-LIMIT", "handle-preparation")
            size = SIZE_T()
            k.InitializeProcThreadAttributeList(None, 2, 0, ctypes.byref(size))
            if not size.value:
                raise WindowsJobError("WJOB-ATTRIBUTE", "handle-preparation")
            process._attribute_buffer = ctypes.create_string_buffer(size.value)
            attr_pointer = ctypes.cast(process._attribute_buffer, ctypes.c_void_p)
            if not k.InitializeProcThreadAttributeList(attr_pointer, 2, 0, ctypes.byref(size)):
                raise WindowsJobError("WJOB-ATTRIBUTE", "handle-preparation")
            process._attribute_initialized = True
            job_value = wintypes.HANDLE(job)
            if not k.UpdateProcThreadAttribute(
                attr_pointer, 0, process._api.PROC_THREAD_ATTRIBUTE_JOB_LIST,
                ctypes.byref(job_value), ctypes.sizeof(job_value), None, None,
            ):
                raise WindowsJobError("WJOB-ATTRIBUTE", "handle-preparation")
            child_handles = (wintypes.HANDLE * 3)(
                h["stdin_child"], h["stdout_child"], h["stderr_child"]
            )
            if not k.UpdateProcThreadAttribute(
                attr_pointer, 0, process._api.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
                child_handles, ctypes.sizeof(child_handles), None, None,
            ):
                raise WindowsJobError("WJOB-ATTRIBUTE", "handle-preparation")
            startup = STARTUPINFOEXW()
            startup.StartupInfo.cb = ctypes.sizeof(startup)
            startup.StartupInfo.dwFlags = process._api.STARTF_USESTDHANDLES
            startup.StartupInfo.hStdInput = h["stdin_child"]
            startup.StartupInfo.hStdOutput = h["stdout_child"]
            startup.StartupInfo.hStdError = h["stderr_child"]
            startup.lpAttributeList = attr_pointer
            info = PROCESS_INFORMATION()
            command = ctypes.create_unicode_buffer(subprocess.list2cmdline(tuple(argv)))
            env_block = None
            if environment is not None:
                block_text, _duplicates = process._api.serialize_environment(
                    tuple(environment.items())
                )
                env_block = ctypes.create_unicode_buffer(block_text)
            child_names = ("stdin_child", "stdout_child", "stderr_child")
            with coordinator.lock:
                if coordinator.poisoned:
                    raise WindowsJobError("WJOB-INHERITANCE-POISONED", "handle-preparation")
                enabled: list[str] = []
                try:
                    for name in child_names:
                        if not k.SetHandleInformation(
                            h[name], process._api.HANDLE_FLAG_INHERIT,
                            process._api.HANDLE_FLAG_INHERIT,
                        ):
                            raise WindowsJobError("WJOB-INHERITANCE", "handle-preparation")
                        enabled.append(name)
                    created_once = False

                    def create_once() -> bool:
                        nonlocal created_once
                        if created_once:
                            raise WindowsJobError("WJOB-CREATE-REENTRY", "process-create")
                        created_once = True
                        created = bool(k.CreateProcessW(
                            executable, command, None, None, True,
                            process._api.CREATE_SUSPENDED
                            | process._api.CREATE_UNICODE_ENVIRONMENT
                            | process._api.EXTENDED_STARTUPINFO_PRESENT,
                            env_block, cwd, ctypes.byref(startup.StartupInfo), ctypes.byref(info),
                        ))
                        # Read the thread-local Win32 error before any restoration
                        # or cleanup API can replace it. Never add ambient text.
                        last_error = None if created else wintypes.DWORD(ctypes.get_last_error()).value
                        process._process_creation = WindowsJobCreateProcessV1(True, created, last_error)
                        if created:
                            h["process"], h["thread"] = int(info.hProcess), int(info.hThread)
                            process._pid = int(info.dwProcessId)
                        return created

                    created = create_once() if create_gate is None else create_gate(create_once)
                    if not created_once or not created:
                        raise WindowsJobError("WJOB-PROCESS-CREATE", "process-create")
                finally:
                    restored = True
                    for name in enabled:
                        if not k.SetHandleInformation(h[name], process._api.HANDLE_FLAG_INHERIT, 0):
                            restored = False
                    if not restored:
                        coordinator.poison()
                        raise WindowsJobError("WJOB-INHERITANCE-POISONED", "handle-preparation")
            for name in child_names:
                process._close_handle(name)
            in_job = wintypes.BOOL()
            if not k.IsProcessInJob(h["process"], h["job"], ctypes.byref(in_job)) or not in_job.value:
                raise WindowsJobError("WJOB-MEMBERSHIP", "tree-verification")
            if before_resume is not None:
                before_resume()
            if k.ResumeThread(h["thread"]) != 1:
                raise WindowsJobError("WJOB-RESUME", "process-resume")
            process._close_handle("thread")
            for name, flags in (("stdin", os.O_WRONLY), ("stdout", os.O_RDONLY), ("stderr", os.O_RDONLY)):
                handle = h[f"{name}_parent"]
                if handle is None:
                    continue
                try:
                    process._fds[name] = msvcrt.open_osfhandle(handle, flags | os.O_BINARY)
                    h[f"{name}_parent"] = None
                except OSError as exc:
                    raise WindowsJobError("WJOB-FD-TRANSFER", "handle-preparation") from exc
            if process._issues:
                raise WindowsJobError(process._issues[0], "handle-preparation")
            return process
        except BaseException as exc:
            closure = process.close()
            try:
                exc.windows_job_closure = closure
                exc.windows_job_creation = process._process_creation
                if not closure.complete:
                    exc.windows_job_owner = process
            except (AttributeError, TypeError):
                pass
            raise


def windows_job_module_contract_v1() -> tuple[str, type[WindowsJobOwnerV1], type[WindowsJobError]]:
    return WINDOWS_JOB_MODULE_CONTRACT_V1, WindowsJobOwnerV1, WindowsJobError


__all__ = (
    "WindowsJobCreateProcessV1", "WindowsJobClosureV1", "WindowsJobError", "WindowsJobOwnerV1",
    "WindowsJobProcessV1", "WindowsInheritanceCoordinatorV1", "WindowsKernelV1",
)
