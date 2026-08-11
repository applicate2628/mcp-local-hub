from __future__ import annotations

import hashlib
import json
import os
import re
import unicodedata
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path, PureWindowsPath
from types import MappingProxyType
from typing import Any, Literal, Protocol

POLICY_SCHEMA = "mcphub.cst.saved_field_authority.v2"
POLICY_FILE_MAX = 1_048_576
POLICY_ENTRY_MAX = 128
PATH_SCALAR_MAX = 4_096
PATH_UTF8_MAX = 16_384
RELATIVE_SCALAR_MAX = 1_024
RELATIVE_UTF8_MAX = 4_096
RELATIVE_DEPTH_MAX = 32
ENTRY_ID_RE = re.compile(r"[a-z0-9][a-z0-9._-]{0,63}\Z")
LOWER_SHA256_RE = re.compile(r"[0-9a-f]{64}\Z")
FILE_ID_RE = re.compile(r"[0-9a-f]{32}\Z")
_RESERVED = {
    "CON",
    "PRN",
    "AUX",
    "NUL",
    "CLOCK$",
    "CONIN$",
    "CONOUT$",
    *(f"COM{digit}" for digit in range(1, 10)),
    *(f"LPT{digit}" for digit in range(1, 10)),
}
_SUPERSCRIPT_DIGITS = str.maketrans({"¹": "1", "²": "2", "³": "3"})
_FORBIDDEN_COMPONENT = frozenset('<>:"/\\|?*')


@dataclass
class PolicyFailure(Exception):
    failure_id: str
    stage: str

    def __str__(self) -> str:
        return f"{self.failure_id}: {self.stage}"


@dataclass(frozen=True, slots=True)
class ObjectIdentityEvidence:
    canonical_path: str
    volume_serial: int
    file_id: str
    link_count: int
    reparse: bool
    streams: tuple[str, ...]
    long_name_exact: bool
    short_name_present: bool


@dataclass(slots=True)
class HeldDirectoryCapability:
    path: Path
    evidence: ObjectIdentityEvidence
    restricted: bool
    handle: int
    _close_handle: Any
    _closed: bool = False

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        if self._close_handle(self.handle) is False:
            raise OSError("verified directory handle close failed")


class WindowsPathProvider(Protocol):
    def prove(self, path: Path, *, kind: Literal["file", "directory"]) -> ObjectIdentityEvidence: ...


class PolicyPlatform(Protocol):
    def read_verified_file(
        self, path: Path, *, maximum: int
    ) -> tuple[ObjectIdentityEvidence, bytes, bool]: ...

    def prove_file(self, path: Path) -> ObjectIdentityEvidence: ...

    def prove_directory(self, path: Path) -> ObjectIdentityEvidence: ...

    def prove_restricted_directory(self, path: Path) -> tuple[ObjectIdentityEvidence, bool]: ...

    def hold_restricted_directory(self, path: Path) -> HeldDirectoryCapability: ...

    def revalidate_held_directory(
        self, held: HeldDirectoryCapability
    ) -> tuple[ObjectIdentityEvidence, bool]: ...

    def create_restricted_child(
        self, held: HeldDirectoryCapability, name: str
    ) -> HeldDirectoryCapability: ...

    def delete_restricted_tree(self, held: HeldDirectoryCapability) -> None: ...

    def access_is_restricted(self, path: Path) -> bool: ...

    def drive_is_local(self, drive: str) -> bool: ...


class WindowsPolicyPlatform:
    """Concrete Win32 identity, locality, owner, and access-control proof."""

    def __init__(self) -> None:
        if os.name != "nt":
            raise OSError("Windows policy proof is unavailable on this platform")

    def prove_file(self, path: Path) -> ObjectIdentityEvidence:
        return self._prove(path, kind="file")

    def read_verified_file(self, path: Path, *, maximum: int) -> tuple[ObjectIdentityEvidence, bytes, bool]:
        import ctypes
        from ctypes import wintypes

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.CreateFileW.restype = wintypes.HANDLE
        handle = kernel32.CreateFileW(
            str(path),
            0x80000000 | 0x00020000,
            0x00000001 | 0x00000002 | 0x00000004,
            None,
            3,
            0x00200000,
            None,
        )
        invalid = ctypes.c_void_p(-1).value
        if handle in {None, invalid}:
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            evidence = _evidence_from_handle(kernel32, handle, str(path), "file")
            restricted = _owner_only_access_handle(handle)
            content = bytearray()
            buffer = ctypes.create_string_buffer(min(1_048_576, maximum + 1))
            while True:
                count = wintypes.DWORD()
                if not kernel32.ReadFile(handle, buffer, len(buffer), ctypes.byref(count), None):
                    raise ctypes.WinError(ctypes.get_last_error())
                if count.value == 0:
                    break
                content.extend(buffer.raw[: count.value])
                if len(content) > maximum:
                    raise OSError("verified file exceeds limit")
            return evidence, bytes(content), restricted
        finally:
            if not kernel32.CloseHandle(handle):
                raise ctypes.WinError(ctypes.get_last_error())

    def prove_directory(self, path: Path) -> ObjectIdentityEvidence:
        return self._prove(path, kind="directory")

    def prove_restricted_directory(self, path: Path) -> tuple[ObjectIdentityEvidence, bool]:
        held = self.hold_restricted_directory(path)
        try:
            return held.evidence, held.restricted
        finally:
            held.close()

    def hold_restricted_directory(self, path: Path) -> HeldDirectoryCapability:
        import ctypes
        from ctypes import wintypes

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.CreateFileW.restype = wintypes.HANDLE
        handle = kernel32.CreateFileW(
            str(path),
            0x001200A7,
            0x00000001 | 0x00000002,
            None,
            3,
            0x00200000 | 0x02000000,
            None,
        )
        invalid = ctypes.c_void_p(-1).value
        if handle in {None, invalid}:
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            evidence = _evidence_from_handle(kernel32, handle, str(path), "directory")
            restricted = _owner_only_access_handle(handle)
        except BaseException:
            kernel32.CloseHandle(handle)
            raise
        return HeldDirectoryCapability(
            Path(path),
            evidence,
            restricted,
            int(handle),
            lambda raw: bool(kernel32.CloseHandle(wintypes.HANDLE(raw))),
        )

    def create_restricted_child(self, held: HeldDirectoryCapability, name: str) -> HeldDirectoryCapability:
        _validate_component(name, stage="workspace_child")
        handle = _open_relative_native(
            held.handle,
            name,
            directory=True,
            create=True,
            delete_access=True,
        )
        child_path = Path(held.path) / name
        import ctypes
        from ctypes import wintypes

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.CloseHandle.argtypes = (wintypes.HANDLE,)
        kernel32.CloseHandle.restype = wintypes.BOOL
        try:
            _harden_owner_access_handle(handle)
            evidence = _evidence_from_handle(kernel32, handle, str(child_path), "directory")
            restricted = _owner_only_access_handle(handle)
            if evidence.reparse or evidence.link_count != 1 or evidence.streams != () or not restricted:
                raise OSError("new workspace child failed identity or access proof")
        except BaseException:
            try:
                _delete_tree_from_handle(handle)
            finally:
                kernel32.CloseHandle(wintypes.HANDLE(handle))
            raise
        return HeldDirectoryCapability(
            child_path,
            evidence,
            restricted,
            handle,
            lambda raw: bool(kernel32.CloseHandle(wintypes.HANDLE(raw))),
        )

    def delete_restricted_tree(self, held: HeldDirectoryCapability) -> None:
        if held._closed:
            return
        _delete_tree_from_handle(held.handle)
        held.close()

    def revalidate_held_directory(self, held: HeldDirectoryCapability) -> tuple[ObjectIdentityEvidence, bool]:
        import ctypes

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        return (
            _evidence_from_handle(kernel32, held.handle, str(held.path), "directory"),
            _owner_only_access_handle(held.handle),
        )

    def _prove(self, path: Path, *, kind: Literal["file", "directory"]) -> ObjectIdentityEvidence:
        import ctypes
        from ctypes import wintypes

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.CreateFileW.restype = wintypes.HANDLE
        access = 0x0080  # FILE_READ_ATTRIBUTES
        share = 0x00000001 | 0x00000002 | 0x00000004
        flags = 0x00200000  # FILE_FLAG_OPEN_REPARSE_POINT
        if kind == "directory":
            flags |= 0x02000000  # FILE_FLAG_BACKUP_SEMANTICS
        handle = kernel32.CreateFileW(str(path), access, share, None, 3, flags, None)
        invalid = ctypes.c_void_p(-1).value
        if handle in {None, invalid}:
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            return _evidence_from_handle(kernel32, handle, str(path), kind)
        finally:
            kernel32.CloseHandle(handle)

    def drive_is_local(self, drive: str) -> bool:
        import ctypes

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        if kernel32.GetDriveTypeW(f"{drive}\\") != 3:  # DRIVE_FIXED
            return False
        target = ctypes.create_unicode_buffer(32_768)
        size = kernel32.QueryDosDeviceW(drive, target, len(target))
        return bool(size) and target.value.startswith("\\Device\\HarddiskVolume")

    def access_is_restricted(self, path: Path) -> bool:
        return _owner_only_access(path)


def _open_relative_native(
    parent_handle: int,
    name: str,
    *,
    directory: bool,
    create: bool = False,
    delete_access: bool = False,
) -> int:
    import ctypes
    from ctypes import wintypes

    class UnicodeString(ctypes.Structure):
        _fields_ = [
            ("Length", wintypes.USHORT),
            ("MaximumLength", wintypes.USHORT),
            ("Buffer", wintypes.LPWSTR),
        ]

    class ObjectAttributes(ctypes.Structure):
        _fields_ = [
            ("Length", wintypes.ULONG),
            ("RootDirectory", wintypes.HANDLE),
            ("ObjectName", ctypes.POINTER(UnicodeString)),
            ("Attributes", wintypes.ULONG),
            ("SecurityDescriptor", wintypes.LPVOID),
            ("SecurityQualityOfService", wintypes.LPVOID),
        ]

    class IoStatusBlock(ctypes.Structure):
        _fields_ = [("Status", ctypes.c_void_p), ("Information", ctypes.c_size_t)]

    buffer = ctypes.create_unicode_buffer(name)
    unicode_name = UnicodeString(
        len(name.encode("utf-16-le")),
        len(buffer) * 2,
        ctypes.cast(buffer, wintypes.LPWSTR),
    )
    attributes = ObjectAttributes(
        ctypes.sizeof(ObjectAttributes),
        wintypes.HANDLE(parent_handle),
        ctypes.pointer(unicode_name),
        0x00000040,
        None,
        None,
    )
    io_status = IoStatusBlock()
    handle = wintypes.HANDLE()
    ntdll = ctypes.WinDLL("ntdll", use_last_error=True)
    ntdll.NtCreateFile.restype = ctypes.c_long
    desired = 0x00100080 | (0x00000027 if directory else 0)
    if delete_access:
        desired |= 0x00010000 | 0x00020000 | 0x00040000 | 0x00080000
    status = int(
        ntdll.NtCreateFile(
            ctypes.byref(handle),
            desired,
            ctypes.byref(attributes),
            ctypes.byref(io_status),
            None,
            0x00000080,
            0x00000001 | 0x00000002,
            2 if create else 1,
            0x00000020 | (0x00000001 if directory else 0x00000040) | 0x00200000,
            None,
            0,
        )
    )
    if status < 0:
        error = ntdll.RtlNtStatusToDosError(status)
        raise OSError(int(error), "relative NtCreateFile failed")
    return int(handle.value)


def _native_directory_rows(handle: int):
    import ctypes
    from ctypes import wintypes

    class IoStatusBlock(ctypes.Structure):
        _fields_ = [("Status", ctypes.c_void_p), ("Information", ctypes.c_size_t)]

    ntdll = ctypes.WinDLL("ntdll", use_last_error=True)
    ntdll.NtQueryDirectoryFile.restype = ctypes.c_long
    storage = ctypes.create_string_buffer(64 * 1024)
    io_status = IoStatusBlock()
    restart = True
    while True:
        status = int(
            ntdll.NtQueryDirectoryFile(
                wintypes.HANDLE(handle),
                None,
                None,
                None,
                ctypes.byref(io_status),
                storage,
                len(storage),
                1,
                False,
                None,
                restart,
            )
        )
        restart = False
        if ctypes.c_uint32(status).value == 0x80000006:
            return
        if status < 0:
            error = ntdll.RtlNtStatusToDosError(status)
            raise OSError(int(error), "relative NtQueryDirectoryFile failed")
        used = int(io_status.Information)
        if used == 0:
            return
        offset = 0
        while offset < used:
            next_offset = int.from_bytes(storage[offset : offset + 4], "little")
            attributes = int.from_bytes(storage[offset + 56 : offset + 60], "little")
            name_length = int.from_bytes(storage[offset + 60 : offset + 64], "little")
            name = bytes(storage[offset + 64 : offset + 64 + name_length]).decode("utf-16-le")
            if name not in {".", ".."}:
                yield name, bool(attributes & 0x00000010), bool(attributes & 0x00000400)
            if next_offset == 0:
                break
            offset += next_offset


def _mark_native_delete(handle: int) -> None:
    import ctypes
    from ctypes import wintypes

    class FileDispositionInfoEx(ctypes.Structure):
        _fields_ = [("Flags", wintypes.DWORD)]

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.SetFileInformationByHandle.argtypes = (
        wintypes.HANDLE,
        ctypes.c_int,
        ctypes.c_void_p,
        wintypes.DWORD,
    )
    kernel32.SetFileInformationByHandle.restype = wintypes.BOOL
    disposition = FileDispositionInfoEx(0x00000001 | 0x00000002 | 0x00000010)
    if not kernel32.SetFileInformationByHandle(
        wintypes.HANDLE(handle),
        21,
        ctypes.byref(disposition),
        ctypes.sizeof(disposition),
    ):
        raise ctypes.WinError(ctypes.get_last_error())


def _delete_tree_from_handle(root_handle: int) -> None:
    import ctypes
    from ctypes import wintypes

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.CloseHandle.argtypes = (wintypes.HANDLE,)
    kernel32.CloseHandle.restype = wintypes.BOOL
    for name, is_directory, is_reparse in _native_directory_rows(root_handle):
        if is_reparse:
            raise OSError("workspace contains a reparse point")
        child = _open_relative_native(
            root_handle,
            name,
            directory=is_directory,
            delete_access=True,
        )
        try:
            if is_directory:
                _delete_tree_from_handle(child)
            else:
                _mark_native_delete(child)
        finally:
            if not kernel32.CloseHandle(wintypes.HANDLE(child)):
                raise ctypes.WinError(ctypes.get_last_error())
    _mark_native_delete(root_handle)


def _evidence_from_handle(kernel32: Any, handle: int, raw: str, kind: str) -> ObjectIdentityEvidence:
    import ctypes
    from ctypes import wintypes

    class FileIdInfo(ctypes.Structure):
        _fields_ = [("VolumeSerialNumber", ctypes.c_uint64), ("FileId", ctypes.c_ubyte * 16)]

    class FileStandardInfo(ctypes.Structure):
        _fields_ = [
            ("AllocationSize", ctypes.c_int64),
            ("EndOfFile", ctypes.c_int64),
            ("NumberOfLinks", wintypes.DWORD),
            ("DeletePending", wintypes.BOOLEAN),
            ("Directory", wintypes.BOOLEAN),
        ]

    class FileAttributeTagInfo(ctypes.Structure):
        _fields_ = [("FileAttributes", wintypes.DWORD), ("ReparseTag", wintypes.DWORD)]

    def query(info_class: int, value: Any) -> None:
        if not kernel32.GetFileInformationByHandleEx(
            handle, info_class, ctypes.byref(value), ctypes.sizeof(value)
        ):
            raise ctypes.WinError(ctypes.get_last_error())

    file_id = FileIdInfo()
    standard = FileStandardInfo()
    attributes = FileAttributeTagInfo()
    query(18, file_id)
    query(1, standard)
    query(9, attributes)
    if bool(standard.Directory) != (kind == "directory"):
        raise OSError("object kind changed")

    final_size = kernel32.GetFinalPathNameByHandleW(handle, None, 0, 0)
    if not final_size:
        raise ctypes.WinError(ctypes.get_last_error())
    final_buffer = ctypes.create_unicode_buffer(final_size + 1)
    if not kernel32.GetFinalPathNameByHandleW(handle, final_buffer, len(final_buffer), 0):
        raise ctypes.WinError(ctypes.get_last_error())
    canonical = final_buffer.value
    if canonical.startswith("\\\\?\\"):
        canonical = canonical[4:]

    streams: list[str] = []
    stream_buffer = ctypes.create_string_buffer(65_536)
    if kernel32.GetFileInformationByHandleEx(handle, 7, stream_buffer, len(stream_buffer)):
        offset = 0
        while True:
            next_offset = int.from_bytes(stream_buffer.raw[offset : offset + 4], "little")
            name_bytes = int.from_bytes(stream_buffer.raw[offset + 4 : offset + 8], "little")
            name = stream_buffer.raw[offset + 24 : offset + 24 + name_bytes].decode("utf-16-le")
            streams.append(name)
            if next_offset == 0:
                break
            offset += next_offset
            if offset >= len(stream_buffer):
                raise OSError("invalid stream inventory")
    elif ctypes.get_last_error() != 38:  # ERROR_HANDLE_EOF means a valid empty stream result.
        raise ctypes.WinError(ctypes.get_last_error())

    long_size = kernel32.GetLongPathNameW(raw, None, 0)
    short_size = kernel32.GetShortPathNameW(raw, None, 0)
    if not long_size or not short_size:
        raise ctypes.WinError(ctypes.get_last_error())
    long_buffer = ctypes.create_unicode_buffer(long_size + 1)
    short_buffer = ctypes.create_unicode_buffer(short_size + 1)
    if not kernel32.GetLongPathNameW(raw, long_buffer, len(long_buffer)):
        raise ctypes.WinError(ctypes.get_last_error())
    if not kernel32.GetShortPathNameW(raw, short_buffer, len(short_buffer)):
        raise ctypes.WinError(ctypes.get_last_error())
    return ObjectIdentityEvidence(
        canonical_path=canonical,
        volume_serial=int(file_id.VolumeSerialNumber),
        file_id=bytes(file_id.FileId).hex(),
        link_count=int(standard.NumberOfLinks),
        reparse=bool(attributes.FileAttributes & 0x00000400),
        streams=tuple(streams),
        long_name_exact=long_buffer.value == raw,
        short_name_present=short_buffer.value.casefold() != raw.casefold(),
    )


def _owner_only_access(path: Path) -> bool:
    if os.name != "nt":
        return False
    import ctypes
    from ctypes import wintypes

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.CreateFileW.restype = wintypes.HANDLE
    handle = kernel32.CreateFileW(
        str(path),
        0x00020000 | 0x0080,
        0x00000001 | 0x00000002 | 0x00000004,
        None,
        3,
        0x00200000 | 0x02000000,
        None,
    )
    invalid = ctypes.c_void_p(-1).value
    if handle in {None, invalid}:
        return False
    try:
        return _owner_only_access_handle(handle)
    finally:
        kernel32.CloseHandle(handle)


def _sddl_access_is_restricted(sddl: str, owner_sid: str) -> bool:
    owner_match = re.search(r"O:(.*?)(?=[GDS]:)", sddl)
    if owner_match is None or owner_match.group(1) != owner_sid or "D:" not in sddl:
        return False
    allowed = {owner_sid, "SY", "BA"}
    observed: set[str] = set()
    for raw_ace in re.findall(r"\(([^()]*)\)", sddl.split("D:", 1)[1]):
        fields = raw_ace.split(";")
        if len(fields) != 6:
            return False
        ace_type, flags, _rights, _object_guid, _inherit_guid, sid = fields
        if ace_type not in {"A", "D"} or "ID" in flags:
            return False
        if ace_type == "A":
            if sid not in allowed:
                return False
            observed.add(sid)
    return observed == allowed


def _owner_only_access_handle(handle: int) -> bool:
    if os.name != "nt":
        return False
    import ctypes
    from ctypes import wintypes

    advapi32 = ctypes.WinDLL("advapi32", use_last_error=True)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.GetCurrentProcess.restype = wintypes.HANDLE
    advapi32.OpenProcessToken.argtypes = (
        wintypes.HANDLE,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.HANDLE),
    )
    advapi32.OpenProcessToken.restype = wintypes.BOOL
    advapi32.GetTokenInformation.argtypes = (
        wintypes.HANDLE,
        ctypes.c_int,
        ctypes.c_void_p,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.DWORD),
    )
    advapi32.GetTokenInformation.restype = wintypes.BOOL
    advapi32.EqualSid.argtypes = (ctypes.c_void_p, ctypes.c_void_p)
    advapi32.EqualSid.restype = wintypes.BOOL
    advapi32.ConvertSidToStringSidW.argtypes = (
        ctypes.c_void_p,
        ctypes.POINTER(wintypes.LPWSTR),
    )
    advapi32.ConvertSidToStringSidW.restype = wintypes.BOOL
    advapi32.ConvertSecurityDescriptorToStringSecurityDescriptorW.argtypes = (
        ctypes.c_void_p,
        wintypes.DWORD,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.LPWSTR),
        ctypes.POINTER(wintypes.DWORD),
    )
    advapi32.ConvertSecurityDescriptorToStringSecurityDescriptorW.restype = wintypes.BOOL
    kernel32.LocalFree.argtypes = (ctypes.c_void_p,)
    kernel32.LocalFree.restype = ctypes.c_void_p
    kernel32.CloseHandle.argtypes = (wintypes.HANDLE,)
    kernel32.CloseHandle.restype = wintypes.BOOL
    owner = ctypes.c_void_p()
    dacl = ctypes.c_void_p()
    descriptor = ctypes.c_void_p()
    status = advapi32.GetSecurityInfo(
        handle,
        6,
        0x00000001 | 0x00000004,
        ctypes.byref(owner),
        None,
        ctypes.byref(dacl),
        None,
        ctypes.byref(descriptor),
    )
    if status != 0 or not owner.value or not dacl.value or not descriptor.value:
        return False
    token = wintypes.HANDLE()
    owner_text = wintypes.LPWSTR()
    descriptor_text = wintypes.LPWSTR()
    try:
        if not advapi32.OpenProcessToken(kernel32.GetCurrentProcess(), 0x0008, ctypes.byref(token)):
            return False
        needed = wintypes.DWORD()
        advapi32.GetTokenInformation(token, 1, None, 0, ctypes.byref(needed))
        token_buffer = ctypes.create_string_buffer(needed.value)
        if not advapi32.GetTokenInformation(token, 1, token_buffer, len(token_buffer), ctypes.byref(needed)):
            return False
        token_sid = ctypes.c_void_p.from_buffer(token_buffer).value
        if not token_sid or not advapi32.EqualSid(owner, token_sid):
            return False
        if not advapi32.ConvertSidToStringSidW(owner, ctypes.byref(owner_text)):
            return False
        if not advapi32.ConvertSecurityDescriptorToStringSecurityDescriptorW(
            descriptor, 1, 0x00000001 | 0x00000004, ctypes.byref(descriptor_text), None
        ):
            return False
        return _sddl_access_is_restricted(descriptor_text.value or "", owner_text.value or "")
    finally:
        if owner_text:
            kernel32.LocalFree(owner_text)
        if descriptor_text:
            kernel32.LocalFree(descriptor_text)
        if token:
            kernel32.CloseHandle(token)
        kernel32.LocalFree(descriptor)


def _harden_owner_access_handle(handle: int) -> None:
    import ctypes
    from ctypes import wintypes

    advapi32 = ctypes.WinDLL("advapi32", use_last_error=True)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.GetCurrentProcess.restype = wintypes.HANDLE
    token = wintypes.HANDLE()
    owner_text = wintypes.LPWSTR()
    descriptor = ctypes.c_void_p()
    owner = ctypes.c_void_p()
    dacl = ctypes.c_void_p()
    advapi32.OpenProcessToken.argtypes = (
        wintypes.HANDLE,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.HANDLE),
    )
    advapi32.OpenProcessToken.restype = wintypes.BOOL
    advapi32.GetTokenInformation.argtypes = (
        wintypes.HANDLE,
        ctypes.c_int,
        ctypes.c_void_p,
        wintypes.DWORD,
        ctypes.POINTER(wintypes.DWORD),
    )
    advapi32.GetTokenInformation.restype = wintypes.BOOL
    advapi32.ConvertSidToStringSidW.argtypes = (
        ctypes.c_void_p,
        ctypes.POINTER(wintypes.LPWSTR),
    )
    advapi32.ConvertSidToStringSidW.restype = wintypes.BOOL
    try:
        if not advapi32.OpenProcessToken(kernel32.GetCurrentProcess(), 0x0008, ctypes.byref(token)):
            raise ctypes.WinError(ctypes.get_last_error())
        needed = wintypes.DWORD()
        advapi32.GetTokenInformation(token, 1, None, 0, ctypes.byref(needed))
        token_buffer = ctypes.create_string_buffer(needed.value)
        if not advapi32.GetTokenInformation(token, 1, token_buffer, len(token_buffer), ctypes.byref(needed)):
            raise ctypes.WinError(ctypes.get_last_error())
        token_sid = ctypes.c_void_p.from_buffer(token_buffer).value
        if not token_sid or not advapi32.ConvertSidToStringSidW(token_sid, ctypes.byref(owner_text)):
            raise ctypes.WinError(ctypes.get_last_error())
        sddl = f"O:{owner_text.value}D:P(A;OICI;FA;;;{owner_text.value})(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
        if not advapi32.ConvertStringSecurityDescriptorToSecurityDescriptorW(
            sddl, 1, ctypes.byref(descriptor), None
        ):
            raise ctypes.WinError(ctypes.get_last_error())
        owner_defaulted = wintypes.BOOL()
        dacl_present = wintypes.BOOL()
        dacl_defaulted = wintypes.BOOL()
        if not advapi32.GetSecurityDescriptorOwner(
            descriptor, ctypes.byref(owner), ctypes.byref(owner_defaulted)
        ) or not advapi32.GetSecurityDescriptorDacl(
            descriptor,
            ctypes.byref(dacl_present),
            ctypes.byref(dacl),
            ctypes.byref(dacl_defaulted),
        ):
            raise ctypes.WinError(ctypes.get_last_error())
        status = advapi32.SetSecurityInfo(
            wintypes.HANDLE(handle),
            1,
            0x00000001 | 0x00000004 | 0x80000000,
            owner,
            None,
            dacl,
            None,
        )
        if status != 0:
            raise OSError(int(status), "SetSecurityInfo failed")
    finally:
        if descriptor:
            kernel32.LocalFree(descriptor)
        if owner_text:
            kernel32.LocalFree(owner_text)
        if token:
            kernel32.CloseHandle(token)


def _namespace_failure(stage: str) -> PolicyFailure:
    return PolicyFailure("cst_saved_field.path_namespace_invalid", stage)


def _identity_failure(stage: str) -> PolicyFailure:
    return PolicyFailure("cst_saved_field.path_identity_ambiguous", stage)


def _validate_component(component: str, *, stage: str) -> None:
    if (
        not component
        or component in {".", ".."}
        or component.endswith((".", " "))
        or component != unicodedata.normalize("NFC", component)
        or "~" in component
        or any(character in _FORBIDDEN_COMPONENT or ord(character) < 32 for character in component)
        or len(component.encode("utf-16-le")) // 2 > 255
    ):
        raise _namespace_failure(stage)
    comparison_key = component.casefold().split(".", 1)[0].rstrip(". ").translate(_SUPERSCRIPT_DIGITS)
    if comparison_key.upper() in _RESERVED:
        raise _namespace_failure(stage)


def validate_windows_path_lexical(
    raw: str,
    *,
    absolute: bool,
    role: str,
) -> tuple[str, ...]:
    if not isinstance(raw, str):
        raise _namespace_failure(role)
    scalar_limit = PATH_SCALAR_MAX if absolute else RELATIVE_SCALAR_MAX
    byte_limit = PATH_UTF8_MAX if absolute else RELATIVE_UTF8_MAX
    if not raw or len(raw) > scalar_limit or len(raw.encode("utf-8")) > byte_limit:
        raise _namespace_failure(role)
    if absolute:
        if (
            len(raw) < 4
            or raw[0] not in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
            or raw[1:3] != ":\\"
            or raw.count(":") != 1
            or "/" in raw
        ):
            raise _namespace_failure(role)
        components = tuple(raw[3:].split("\\"))
    else:
        if raw.startswith(("\\", "/")) or ":" in raw or "\\" in raw:
            raise _namespace_failure(role)
        components = tuple(raw.split("/"))
        if len(components) > RELATIVE_DEPTH_MAX:
            raise _namespace_failure(role)
    if not components:
        raise _namespace_failure(role)
    for component in components:
        _validate_component(component, stage=role)
    return components


def _validate_evidence(raw: str, evidence: ObjectIdentityEvidence, *, role: str) -> None:
    expected_streams = () if role == "root" else ("::$DATA",)
    if (
        evidence.canonical_path != raw
        or not isinstance(evidence.volume_serial, int)
        or isinstance(evidence.volume_serial, bool)
        or not 0 <= evidence.volume_serial <= (1 << 64) - 1
        or FILE_ID_RE.fullmatch(evidence.file_id) is None
        or evidence.link_count != 1
        or evidence.reparse
        or evidence.streams != expected_streams
        or not evidence.long_name_exact
        or evidence.short_name_present
    ):
        raise _identity_failure(role)


def prove_windows_path(
    raw: str,
    *,
    provider: WindowsPathProvider,
    kind: Literal["file", "directory"],
    role: str,
) -> ObjectIdentityEvidence:
    validate_windows_path_lexical(raw, absolute=True, role=role)
    try:
        evidence = provider.prove(Path(raw), kind=kind)
    except PolicyFailure:
        raise
    except Exception as exc:
        raise _identity_failure(role) from exc
    _validate_evidence(raw, evidence, role=role)
    return evidence


@dataclass(frozen=True, slots=True)
class RootIdentityV1:
    volume_serial: int
    file_id: str


@dataclass(frozen=True, slots=True)
class AuthorityEntry:
    entry_id: str
    root: str
    root_identity: RootIdentityV1
    project_relative: str
    project_sha256: str
    mesh_sha256: str
    bundle_manifest_sha256: str


@dataclass(frozen=True, slots=True)
class AuthorizedBundleDescriptor:
    entry_id: str
    policy_revision: str
    root: str
    root_identity: RootIdentityV1
    project_relative: str
    project_sha256: str
    mesh_sha256: str
    bundle_manifest_sha256: str


@dataclass(frozen=True, slots=True)
class AuthoritySnapshot:
    revision: str
    entries: tuple[AuthorityEntry, ...]
    _projects: Mapping[str, AuthorityEntry]

    def authorize(self, project_bundle: str) -> AuthorizedBundleDescriptor:
        validate_windows_path_lexical(project_bundle, absolute=True, role="project")
        entry = self._projects.get(project_bundle)
        if entry is None:
            raise PolicyFailure("cst_saved_field.not_authorized", "lexical_authority")
        return AuthorizedBundleDescriptor(
            entry_id=entry.entry_id,
            policy_revision=self.revision,
            root=entry.root,
            root_identity=entry.root_identity,
            project_relative=entry.project_relative,
            project_sha256=entry.project_sha256,
            mesh_sha256=entry.mesh_sha256,
            bundle_manifest_sha256=entry.bundle_manifest_sha256,
        )


@dataclass(frozen=True, slots=True)
class PolicyLoadResult:
    enabled: bool
    snapshot: AuthoritySnapshot | None
    failure_id: str | None


def _canonical_json(value: object) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def canonical_disabled_policy(entries: list[dict[str, object]]) -> bytes:
    return _canonical_json({"enabled": False, "entries": entries, "schema": POLICY_SCHEMA})


def _require_exact_keys(value: Mapping[str, Any], expected: frozenset[str], stage: str) -> None:
    if frozenset(value) != expected:
        raise PolicyFailure("cst_saved_field.policy_invalid", stage)


def _parse_identity(value: object) -> RootIdentityV1:
    if not isinstance(value, dict):
        raise PolicyFailure("cst_saved_field.policy_invalid", "root_identity")
    _require_exact_keys(value, frozenset({"volume_serial", "file_id"}), "root_identity")
    volume = value["volume_serial"]
    file_id = value["file_id"]
    if (
        not isinstance(volume, int)
        or isinstance(volume, bool)
        or not 0 <= volume <= (1 << 64) - 1
        or not isinstance(file_id, str)
        or FILE_ID_RE.fullmatch(file_id) is None
    ):
        raise PolicyFailure("cst_saved_field.policy_invalid", "root_identity")
    return RootIdentityV1(volume, file_id)


def _parse_entry(value: object, platform: PolicyPlatform) -> AuthorityEntry:
    if not isinstance(value, dict):
        raise PolicyFailure("cst_saved_field.policy_invalid", "entry")
    _require_exact_keys(
        value,
        frozenset(
            {
                "entry_id",
                "root",
                "root_identity",
                "project_relative",
                "project_sha256",
                "mesh_sha256",
                "bundle_manifest_sha256",
            }
        ),
        "entry",
    )
    entry_id = value["entry_id"]
    root = value["root"]
    relative = value["project_relative"]
    hashes = (
        value["project_sha256"],
        value["mesh_sha256"],
        value["bundle_manifest_sha256"],
    )
    if (
        not isinstance(entry_id, str)
        or ENTRY_ID_RE.fullmatch(entry_id) is None
        or not isinstance(root, str)
        or not isinstance(relative, str)
        or any(not isinstance(item, str) or LOWER_SHA256_RE.fullmatch(item) is None for item in hashes)
    ):
        raise PolicyFailure("cst_saved_field.policy_invalid", "entry")
    validate_windows_path_lexical(root, absolute=True, role="root")
    components = validate_windows_path_lexical(relative, absolute=False, role="project_relative")
    if not components[-1].lower().endswith(".cst"):
        raise PolicyFailure("cst_saved_field.policy_invalid", "project_relative")
    drive = root[:2]
    if not platform.drive_is_local(drive):
        raise PolicyFailure("cst_saved_field.policy_invalid", "root_locality")
    root_path = Path(root)
    evidence, restricted = platform.prove_restricted_directory(root_path)
    _validate_evidence(root, evidence, role="root")
    identity = _parse_identity(value["root_identity"])
    if (evidence.volume_serial, evidence.file_id) != (identity.volume_serial, identity.file_id):
        raise PolicyFailure("cst_saved_field.policy_invalid", "root_identity")
    if not restricted:
        raise PolicyFailure("cst_saved_field.policy_invalid", "root_access")
    return AuthorityEntry(
        entry_id=entry_id,
        root=root,
        root_identity=identity,
        project_relative=relative,
        project_sha256=hashes[0],
        mesh_sha256=hashes[1],
        bundle_manifest_sha256=hashes[2],
    )


def _disabled(failure_id: str) -> PolicyLoadResult:
    return PolicyLoadResult(enabled=False, snapshot=None, failure_id=failure_id)


def load_authority_snapshot(raw_path: str | None, platform: PolicyPlatform) -> PolicyLoadResult:
    if raw_path is None or raw_path == "":
        return _disabled("cst_saved_field.policy_disabled")
    try:
        validate_windows_path_lexical(raw_path, absolute=True, role="policy")
        if not platform.drive_is_local(raw_path[:2]):
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_locality")
        path = Path(raw_path)
        evidence, raw, restricted = platform.read_verified_file(path, maximum=POLICY_FILE_MAX)
        _validate_evidence(raw_path, evidence, role="policy")
        if not restricted:
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_access")
        if len(raw) > POLICY_FILE_MAX:
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_read")
        document = json.loads(raw.decode("utf-8"))
        if not isinstance(document, dict):
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_schema")
        _require_exact_keys(document, frozenset({"schema", "enabled", "entries"}), "policy_schema")
        if _canonical_json(document) != raw:
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_canonical")
        if document["schema"] != POLICY_SCHEMA or type(document["enabled"]) is not bool:
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_schema")
        values = document["entries"]
        if not isinstance(values, list) or len(values) > POLICY_ENTRY_MAX:
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_entries")
        if not document["enabled"]:
            return _disabled("cst_saved_field.policy_disabled")
        if not values:
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_entries")
        entries = tuple(_parse_entry(value, platform) for value in values)
        ids = [entry.entry_id for entry in entries]
        projects = [
            str(PureWindowsPath(entry.root) / PureWindowsPath(entry.project_relative)) for entry in entries
        ]
        root_projects = [
            (entry.root_identity.volume_serial, entry.root_identity.file_id, entry.project_relative)
            for entry in entries
        ]
        if len(ids) != len(set(ids)) or len(root_projects) != len(set(root_projects)):
            raise PolicyFailure("cst_saved_field.policy_invalid", "policy_duplicates")
        revision = hashlib.sha256(raw).hexdigest()
        return PolicyLoadResult(
            enabled=True,
            snapshot=AuthoritySnapshot(
                revision=revision,
                entries=entries,
                _projects=MappingProxyType(dict(zip(projects, entries, strict=True))),
            ),
            failure_id=None,
        )
    except (PolicyFailure, OSError, UnicodeError, json.JSONDecodeError, ValueError, TypeError):
        return _disabled("cst_saved_field.policy_invalid")
