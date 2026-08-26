from __future__ import annotations

import os
import sys
from dataclasses import replace
from io import BytesIO
from pathlib import Path

import pytest

RUNTIME_IMAGE = Path(__file__).resolve().parents[1] / "native" / "cst-runtime" / "mcphub-cst-runtime.exe"


def _values():
    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        WorkerPreMainBootstrapV1,
        WorkerPreMainReceiptV1,
    )

    source = {"volume_serial": 11, "file_id": "source-id"}
    workspace = {"volume_serial": 22, "file_id": "workspace-id"}
    deadline = QpcDeadlineV1(10_000_000, 100, 600_000_100)
    bootstrap = WorkerPreMainBootstrapV1("1" * 32, deadline, 101, 202, 0x120089, 0x12019F, source, workspace)
    return (
        bootstrap,
        WorkerPreMainReceiptV1(
            bootstrap.correlation_id,
            deadline,
            True,
            True,
            False,
            bootstrap.source_access_mask,
            bootstrap.workspace_access_mask,
            source,
            workspace,
            bootstrap.checksum,
        ),
    )


def test_worker_pre_main_frames_are_closed_and_round_trip() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import encode_frame
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        decode_pre_main_bootstrap_frame,
        decode_pre_main_receipt_frame,
        encode_pre_main_bootstrap_frame,
        encode_pre_main_receipt_frame,
    )

    bootstrap, receipt = _values()
    assert decode_pre_main_bootstrap_frame(BytesIO(encode_pre_main_bootstrap_frame(bootstrap))) == bootstrap
    assert decode_pre_main_receipt_frame(BytesIO(encode_pre_main_receipt_frame(receipt))) == receipt
    for value, decoder in (
        (bootstrap.to_wire(), decode_pre_main_bootstrap_frame),
        (receipt.to_wire(), decode_pre_main_receipt_frame),
    ):
        value["unexpected"] = True
        with pytest.raises(Exception, match="broker_protocol_invalid"):
            decoder(BytesIO(encode_frame(value)))


@pytest.mark.parametrize(
    "receipt_mutation",
    [
        {"inherit_flags_cleared": False},
        {"capability_identities_verified": False},
        {"source_root_identity": {"volume_serial": 99, "file_id": "wrong"}},
    ],
)
def test_containment_rejects_unproved_native_pre_main(receipt_mutation) -> None:
    from mcphub_em_mcp.cst_saved_field_containment_windows import (
        ContainmentFailure,
        validate_worker_pre_main,
    )

    bootstrap, receipt = _values()
    with pytest.raises(ContainmentFailure) as raised:
        validate_worker_pre_main(
            bootstrap,
            replace(receipt, **receipt_mutation),
            inherited_handle_roles=("stdin", "stdout", "stderr", "source-root", "workspace-root"),
        )
    assert raised.value.quarantine is True


def test_protocol_rejects_non_exact_five_handle_receipt() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import BrokerProtocolFailure

    _, receipt = _values()
    with pytest.raises(BrokerProtocolFailure):
        replace(receipt, inherited_handle_roles=("stdin", "stdout", "stderr"))


def test_containment_accepts_exact_native_pre_main_only() -> None:
    from mcphub_em_mcp.cst_saved_field_containment_windows import validate_worker_pre_main

    bootstrap, receipt = _values()
    assert (
        validate_worker_pre_main(
            bootstrap,
            receipt,
            inherited_handle_roles=("stdin", "stdout", "stderr", "source-root", "workspace-root"),
        )
        is receipt
    )


def test_production_worker_stays_unavailable_without_native_receipt(monkeypatch) -> None:
    from mcphub_em_mcp import cst_saved_field_broker_worker as worker

    monkeypatch.setattr(worker, "compose_default_off_runtime", lambda: None)
    assert worker.main() == 78


@pytest.mark.skipif(sys.platform != "win32", reason="exact Win32 HANDLE_LIST fixture")
def test_native_worker_emits_bound_pre_main_receipt_before_python(tmp_path: Path) -> None:
    import ctypes
    import msvcrt
    from ctypes import wintypes

    from mcphub_em_mcp.cst_saved_field_broker_protocol import QpcDeadlineV1
    from mcphub_em_mcp.cst_saved_field_broker_worker_protocol import (
        WorkerPreMainBootstrapV1,
        decode_pre_main_receipt_frame,
        decode_startup_proof_frame,
        encode_pre_main_bootstrap_frame,
    )

    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    handle_t = wintypes.HANDLE
    size_t = ctypes.c_size_t

    class SA(ctypes.Structure):
        _fields_ = [
            ("nLength", wintypes.DWORD),
            ("lpSecurityDescriptor", wintypes.LPVOID),
            ("bInheritHandle", wintypes.BOOL),
        ]

    class SI(ctypes.Structure):
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

    class SIEX(ctypes.Structure):
        _fields_ = [("StartupInfo", SI), ("lpAttributeList", wintypes.LPVOID)]

    class PI(ctypes.Structure):
        _fields_ = [
            ("hProcess", handle_t),
            ("hThread", handle_t),
            ("dwProcessId", wintypes.DWORD),
            ("dwThreadId", wintypes.DWORD),
        ]

    class FILE_ID_128(ctypes.Structure):
        _fields_ = [("Identifier", ctypes.c_ubyte * 16)]

    class FILE_ID_INFO(ctypes.Structure):
        _fields_ = [("VolumeSerialNumber", ctypes.c_uint64), ("FileId", FILE_ID_128)]

    kernel.CreateJobObjectW.restype = handle_t
    kernel.CreateFileW.restype = handle_t
    kernel.InitializeProcThreadAttributeList.argtypes = [
        wintypes.LPVOID,
        wintypes.DWORD,
        wintypes.DWORD,
        ctypes.POINTER(size_t),
    ]
    kernel.UpdateProcThreadAttribute.argtypes = [
        wintypes.LPVOID,
        wintypes.DWORD,
        size_t,
        wintypes.LPVOID,
        size_t,
        wintypes.LPVOID,
        ctypes.POINTER(size_t),
    ]

    def directory(path: Path) -> int:
        sa = SA(ctypes.sizeof(SA), None, True)
        value = kernel.CreateFileW(str(path), 0x120089, 7, ctypes.byref(sa), 3, 0x02000000, None)
        assert value not in (None, -1)
        return int(value)

    def identity(value: int) -> dict[str, object]:
        info = FILE_ID_INFO()
        assert kernel.GetFileInformationByHandleEx(
            handle_t(value), 18, ctypes.byref(info), ctypes.sizeof(info)
        )
        return {"volume_serial": info.VolumeSerialNumber, "file_id": bytes(info.FileId.Identifier).hex()}

    source_dir, workspace_dir = tmp_path / "source", tmp_path / "workspace"
    source_dir.mkdir()
    workspace_dir.mkdir()
    source_handle, workspace_handle = directory(source_dir), directory(workspace_dir)
    stdin_r, stdin_w = os.pipe()
    stdout_r, stdout_w = os.pipe()
    stderr_r, stderr_w = os.pipe()
    for fd in (stdin_r, stdout_w, stderr_w):
        os.set_inheritable(fd, True)
    child_handles = [msvcrt.get_osfhandle(fd) for fd in (stdin_r, stdout_w, stderr_w)] + [
        source_handle,
        workspace_handle,
    ]
    job = kernel.CreateJobObjectW(None, None)
    size = size_t()
    kernel.InitializeProcThreadAttributeList(None, 2, 0, ctypes.byref(size))
    storage = ctypes.create_string_buffer(size.value)
    siex = SIEX()
    siex.lpAttributeList = ctypes.cast(storage, wintypes.LPVOID)
    assert kernel.InitializeProcThreadAttributeList(siex.lpAttributeList, 2, 0, ctypes.byref(size))
    job_value = handle_t(job)
    handles = (handle_t * 5)(*child_handles)
    assert kernel.UpdateProcThreadAttribute(
        siex.lpAttributeList, 0, 0x0002000D, ctypes.byref(job_value), ctypes.sizeof(job_value), None, None
    )
    assert kernel.UpdateProcThreadAttribute(
        siex.lpAttributeList, 0, 0x00020002, handles, ctypes.sizeof(handles), None, None
    )
    siex.StartupInfo.cb = ctypes.sizeof(SIEX)
    siex.StartupInfo.dwFlags = 0x100
    siex.StartupInfo.hStdInput, siex.StartupInfo.hStdOutput, siex.StartupInfo.hStdError = child_handles[:3]
    command = ctypes.create_unicode_buffer(f'"{RUNTIME_IMAGE}" --role=worker')
    process = PI()
    assert kernel.CreateProcessW(
        str(RUNTIME_IMAGE),
        command,
        None,
        None,
        True,
        0x08080000,
        None,
        str(RUNTIME_IMAGE.parent),
        ctypes.byref(siex),
        ctypes.byref(process),
    )
    os.close(stdin_r)
    os.close(stdout_w)
    os.close(stderr_w)
    frequency, tick = ctypes.c_int64(), ctypes.c_int64()
    assert kernel.QueryPerformanceFrequency(ctypes.byref(frequency))
    assert kernel.QueryPerformanceCounter(ctypes.byref(tick))
    bootstrap = WorkerPreMainBootstrapV1(
        "a" * 32,
        QpcDeadlineV1(frequency.value, tick.value, tick.value + 60 * frequency.value),
        source_handle,
        workspace_handle,
        0x120089,
        0x12019F,
        identity(source_handle),
        identity(workspace_handle),
    )
    os.write(stdin_w, encode_pre_main_bootstrap_frame(bootstrap))
    os.close(stdin_w)
    assert kernel.WaitForSingleObject(process.hProcess, 5000) == 0
    proof = decode_startup_proof_frame(BytesIO(os.read(stderr_r, 2048)))
    receipt = decode_pre_main_receipt_frame(BytesIO(os.read(stdout_r, 4096)))
    assert proof.complete and receipt.validates(bootstrap) and receipt.python_initialized is False
    for fd in (stdout_r, stderr_r):
        os.close(fd)
    for value in (process.hThread, process.hProcess, job, source_handle, workspace_handle):
        kernel.CloseHandle(handle_t(value))
