"""Bounded, helper-owned transfer of an authorized CST result bundle.

The public records in this module are deliberately neutral: they carry no CST
objects and perform no ambient configuration lookup.  The caller is expected to
run the transfer in the contained helper process.
"""

from __future__ import annotations

import hashlib
import os
import shutil
import stat
import time
import uuid
from collections.abc import Callable, Iterator
from contextlib import AbstractContextManager
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Literal

from .cst_saved_field_policy import (
    HeldDirectoryCapability,
    PolicyPlatform,
    WindowsPathIdentityV1,
    validate_windows_path_lexical,
)
from .cst_saved_field_port import (
    AuthorizedVendorPathLease,
    SealedVendorOutput,
    VendorPathLeaseSettlement,
)

MANIFEST_SCHEMA = "sha256-canonical-file-list-v2"


class _RelativeFileOwner(AbstractContextManager["_RelativeFileOwner"]):
    def open(self, relative: str, *, create: bool = False) -> int:
        raise NotImplementedError

    def ensure_parent(self, relative: str) -> None:
        raise NotImplementedError

    def set_modified_ns(self, descriptor: int, modified_ns: int) -> None:
        raise NotImplementedError

    def iter_directory(self, relative: str, *, budget: TransferBudget) -> Iterator[tuple[str, bool]]:
        raise NotImplementedError


def _convert_owned_native_handle(
    handle: int,
    flags: int,
    *,
    converter: Callable[[int, int], int],
    close_native: Callable[[int], object],
) -> int:
    """Transfer one native HANDLE to the CRT or close it on conversion failure."""
    try:
        return converter(handle, flags)
    except BaseException:
        close_native(handle)
        raise


class _PortableRelativeFileOwner(_RelativeFileOwner):
    def __init__(self, root: Path) -> None:
        self.root = root

    def __exit__(self, *_args: object) -> None:
        return None

    def open(self, relative: str, *, create: bool = False) -> int:
        target = self.root.joinpath(*PurePosixPath(relative).parts)
        if create:
            return os.open(
                target,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_BINARY", 0),
                stat.S_IRUSR | stat.S_IWUSR,
            )
        return os.open(target, os.O_RDONLY | getattr(os, "O_BINARY", 0))

    def ensure_parent(self, relative: str) -> None:
        self.root.joinpath(*PurePosixPath(relative).parts[:-1]).mkdir(parents=True, exist_ok=True)

    def set_modified_ns(self, descriptor: int, modified_ns: int) -> None:
        current = os.fstat(descriptor)
        os.utime(descriptor, ns=(current.st_atime_ns, modified_ns))

    def iter_directory(self, relative: str, *, budget: TransferBudget) -> Iterator[tuple[str, bool]]:
        target = self.root.joinpath(*PurePosixPath(relative).parts)
        with os.scandir(target) as entries:
            for entry in entries:
                budget.check_time()
                if entry.is_symlink():
                    raise OSError("directory entry is a symbolic link")
                yield entry.name, entry.is_dir(follow_symlinks=False)


class _WindowsRelativeFileOwner(_RelativeFileOwner):
    """Open children relative to one held directory handle through NtCreateFile."""

    def __init__(
        self,
        root: Path,
        *,
        root_handle: int | None = None,
        own_root: bool = True,
        component_hook: Callable[[str], None] | None = None,
    ) -> None:
        import ctypes
        from ctypes import wintypes

        self._ctypes = ctypes
        self._wintypes = wintypes
        self._kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        self._ntdll = ctypes.WinDLL("ntdll", use_last_error=True)
        self._kernel32.CreateFileW.restype = wintypes.HANDLE
        self._own_root = own_root
        self._component_hook = component_hook or (lambda _relative: None)
        self._root = (
            wintypes.HANDLE(root_handle)
            if root_handle is not None
            else self._kernel32.CreateFileW(
                str(root),
                0x00100081,
                0x00000001 | 0x00000002 | 0x00000004,
                None,
                3,
                0x00200000 | 0x02000000,
                None,
            )
        )
        if self._root in {None, ctypes.c_void_p(-1).value}:
            raise ctypes.WinError(ctypes.get_last_error())

    def __exit__(self, *_args: object) -> None:
        if self._own_root and self._root and not self._kernel32.CloseHandle(self._root):
            raise self._ctypes.WinError(self._ctypes.get_last_error())
        self._root = None

    def _open_native(self, parent, name: str, *, directory: bool, create: bool = False) -> int:
        ctypes = self._ctypes
        wintypes = self._wintypes

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
            parent,
            ctypes.pointer(unicode_name),
            0x00000040,
            None,
            None,
        )
        io_status = IoStatusBlock()
        handle = wintypes.HANDLE()
        status = self._ntdll.NtCreateFile(
            ctypes.byref(handle),
            0x00120196 if create else (0x00100081 if directory else 0x00120089),
            ctypes.byref(attributes),
            ctypes.byref(io_status),
            None,
            0x00000080,
            0x00000001 | 0x00000002 | 0x00000004,
            (3 if directory and create else 2) if create else 1,
            0x00000020 | (0x00000001 if directory else 0x00000040) | 0x00200000,
            None,
            0,
        )
        if status < 0:
            error = self._ntdll.RtlNtStatusToDosError(status)
            raise OSError(int(error), "relative NtCreateFile failed")
        return int(handle.value)

    def _reject_reparse(self, handle: int) -> None:
        ctypes = self._ctypes
        wintypes = self._wintypes

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

        info = ByHandleFileInformation()
        if (
            not self._kernel32.GetFileInformationByHandle(wintypes.HANDLE(handle), ctypes.byref(info))
            or info.attributes & 0x00000400
        ):
            raise OSError("relative component is a reparse point")

    def open(self, relative: str, *, create: bool = False) -> int:
        import msvcrt

        parts = PurePosixPath(relative).parts
        if not parts or any(part in {"", ".", ".."} for part in parts):
            raise OSError("invalid relative path")
        parent = int(self._root.value if hasattr(self._root, "value") else self._root)
        owned_directories: list[int] = []
        try:
            traversed: list[str] = []
            for part in parts[:-1]:
                child = self._open_native(parent, part, directory=True)
                try:
                    self._reject_reparse(child)
                except BaseException:
                    self._kernel32.CloseHandle(self._wintypes.HANDLE(child))
                    raise
                owned_directories.append(child)
                parent = child
                traversed.append(part)
                self._component_hook("/".join(traversed))
            handle = self._open_native(parent, parts[-1], directory=False, create=create)
            try:
                self._reject_reparse(handle)
            except BaseException:
                self._kernel32.CloseHandle(self._wintypes.HANDLE(handle))
                raise
        finally:
            for directory_handle in reversed(owned_directories):
                self._kernel32.CloseHandle(self._wintypes.HANDLE(directory_handle))
        flags = (os.O_WRONLY if create else os.O_RDONLY) | getattr(os, "O_BINARY", 0)
        return _convert_owned_native_handle(
            handle,
            flags,
            converter=msvcrt.open_osfhandle,
            close_native=lambda raw: self._kernel32.CloseHandle(self._wintypes.HANDLE(raw)),
        )

    def ensure_parent(self, relative: str) -> None:
        parts = PurePosixPath(relative).parts[:-1]
        parent = int(self._root.value if hasattr(self._root, "value") else self._root)
        owned: list[int] = []
        try:
            traversed: list[str] = []
            for part in parts:
                child = self._open_native(parent, part, directory=True, create=True)
                try:
                    self._reject_reparse(child)
                except BaseException:
                    self._kernel32.CloseHandle(self._wintypes.HANDLE(child))
                    raise
                owned.append(child)
                parent = child
                traversed.append(part)
                self._component_hook("/".join(traversed))
        finally:
            for handle in reversed(owned):
                self._kernel32.CloseHandle(self._wintypes.HANDLE(handle))

    def set_modified_ns(self, descriptor: int, modified_ns: int) -> None:
        import msvcrt

        value = modified_ns // 100 + 116_444_736_000_000_000
        timestamp = self._wintypes.FILETIME(value & 0xFFFFFFFF, value >> 32)
        native = self._wintypes.HANDLE(msvcrt.get_osfhandle(descriptor))
        if not self._kernel32.SetFileTime(native, None, None, self._ctypes.byref(timestamp)):
            raise self._ctypes.WinError(self._ctypes.get_last_error())

    def iter_directory(self, relative: str, *, budget: TransferBudget) -> Iterator[tuple[str, bool]]:
        """Enumerate one directory through the same held RootDirectory capability."""

        ctypes = self._ctypes
        wintypes = self._wintypes

        class IoStatusBlock(ctypes.Structure):
            _fields_ = [("Status", ctypes.c_void_p), ("Information", ctypes.c_size_t)]

        parts = PurePosixPath(relative).parts
        if not parts or any(part in {"", ".", ".."} for part in parts):
            raise OSError("invalid relative directory")
        parent = int(self._root.value if hasattr(self._root, "value") else self._root)
        owned: list[int] = []
        try:
            traversed: list[str] = []
            for part in parts:
                child = self._open_native(parent, part, directory=True)
                try:
                    self._reject_reparse(child)
                except BaseException:
                    self._kernel32.CloseHandle(wintypes.HANDLE(child))
                    raise
                owned.append(child)
                parent = child
                traversed.append(part)
                self._component_hook("/".join(traversed))

            io_status = IoStatusBlock()
            storage = ctypes.create_string_buffer(64 * 1024)
            restart = True
            self._ntdll.NtQueryDirectoryFile.restype = ctypes.c_long
            while True:
                budget.check_time()
                status = int(
                    self._ntdll.NtQueryDirectoryFile(
                        wintypes.HANDLE(parent),
                        None,
                        None,
                        None,
                        ctypes.byref(io_status),
                        storage,
                        len(storage),
                        1,  # FileDirectoryInformation
                        False,
                        None,
                        restart,
                    )
                )
                restart = False
                if ctypes.c_uint32(status).value == 0x80000006:  # STATUS_NO_MORE_FILES
                    break
                if status < 0:
                    error = self._ntdll.RtlNtStatusToDosError(status)
                    raise OSError(int(error), "relative NtQueryDirectoryFile failed")
                used = int(io_status.Information)
                offset = 0
                while offset < used:
                    budget.check_time()
                    next_offset = int.from_bytes(storage[offset : offset + 4], "little")
                    attributes = int.from_bytes(storage[offset + 56 : offset + 60], "little")
                    name_length = int.from_bytes(storage[offset + 60 : offset + 64], "little")
                    name = bytes(storage[offset + 64 : offset + 64 + name_length]).decode(
                        "utf-16-le", errors="strict"
                    )
                    if name not in {".", ".."}:
                        if attributes & 0x00000400:
                            raise OSError("directory entry is a reparse point")
                        yield name, bool(attributes & 0x00000010)
                    if next_offset == 0:
                        break
                    offset += next_offset
                if used == 0:
                    break
        finally:
            for handle in reversed(owned):
                self._kernel32.CloseHandle(wintypes.HANDLE(handle))


def _relative_file_owner(
    root: Path, capability: HeldDirectoryCapability | object | None = None
) -> _RelativeFileOwner:
    if os.name == "nt":
        if capability is not None:
            return _WindowsRelativeFileOwner(root, root_handle=capability.handle, own_root=False)
        return _WindowsRelativeFileOwner(root)
    return _PortableRelativeFileOwner(root)


@dataclass(frozen=True, slots=True)
class WorkspaceSettlement:
    stage: str
    child_created: bool
    permission_set: bool
    identity_proven: bool
    initialized: bool
    lease_transferred: bool
    child_removed: bool


class TransferFailure(RuntimeError):
    def __init__(
        self,
        failure_id: str,
        stage: str,
        *,
        workspace_settlement: WorkspaceSettlement | None = None,
    ) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id
        self.stage = stage
        self.workspace_settlement = workspace_settlement


@dataclass(frozen=True, slots=True)
class ManifestRowV2:
    path: str
    type: Literal["regular"]
    stream: Literal["::$DATA"]
    size: int
    sha256: str
    modified_ns: int


@dataclass(frozen=True, slots=True)
class ManifestV2:
    schema: str
    rows: tuple[ManifestRowV2, ...]
    aggregate_sha256: str

    def __post_init__(self) -> None:
        paths = tuple(row.path for row in self.rows)
        if (
            self.schema != MANIFEST_SCHEMA
            or type(self.rows) is not tuple
            or paths != tuple(sorted(paths, key=lambda value: value.encode("utf-8")))
            or len(paths) != len(set(paths))
            or any(
                not isinstance(row, ManifestRowV2)
                or row.type != "regular"
                or row.stream != "::$DATA"
                or type(row.size) is not int
                or row.size < 0
                or len(row.sha256) != 64
                or any(character not in "0123456789abcdef" for character in row.sha256)
                or type(row.modified_ns) is not int
                or row.modified_ns < 0
                for row in self.rows
            )
            or len(self.aggregate_sha256) != 64
            or self.aggregate_sha256 != _canonical_aggregate(self.rows)
        ):
            raise TransferFailure("cst_saved_field.authorized_copy_changed", "manifest")


@dataclass(frozen=True, slots=True)
class TransferBudget:
    max_depth: int = 32
    max_entries: int = 20_000
    max_files: int = 10_000
    max_file_bytes: int = 8 * 1024**3
    max_total_bytes: int = 16 * 1024**3
    absolute_deadline: float | None = None

    def check_time(self) -> None:
        if self.absolute_deadline is not None and time.monotonic() >= self.absolute_deadline:
            raise TransferFailure("cst_saved_field.resource_limit_exceeded", "budget")


def _sha256_handle(handle, budget: TransferBudget) -> str:
    digest = hashlib.sha256()
    while True:
        budget.check_time()
        block = handle.read(1024 * 1024)
        if not block:
            break
        digest.update(block)
    return digest.hexdigest()


def _canonical_aggregate(rows: tuple[ManifestRowV2, ...]) -> str:
    digest = hashlib.sha256()
    for row in rows:
        record = f"{row.path}\0{row.type}\0{row.stream}\0{row.size}\0{row.sha256}\0{row.modified_ns}\n"
        digest.update(record.encode("utf-8"))
    return digest.hexdigest()


def _source_members(project: Path, budget: TransferBudget, owner: _RelativeFileOwner) -> tuple[str, ...]:
    project = Path(os.path.abspath(project))
    result_relative = (PurePosixPath(project.stem) / "Result").as_posix()
    members: list[str] = [project.name]
    entry_count = 1
    directories = [result_relative]
    while directories:
        budget.check_time()
        relative_directory = directories.pop()
        _validate_relative(relative_directory, budget=budget)
        try:
            entries = owner.iter_directory(relative_directory, budget=budget)
        except OSError as exc:
            raise TransferFailure("cst_saved_field.source_changed", "enumerate") from exc
        for name, is_directory in entries:
            budget.check_time()
            entry_count += 1
            if entry_count > budget.max_entries:
                raise TransferFailure("cst_saved_field.resource_limit_exceeded", "enumerate")
            relative = (PurePosixPath(relative_directory) / name).as_posix()
            _validate_relative(relative, budget=budget)
            if is_directory:
                directories.append(relative)
            else:
                if len(members) >= budget.max_files:
                    raise TransferFailure("cst_saved_field.resource_limit_exceeded", "enumerate")
                members.append(relative)
    return tuple(members)


def _validate_relative(relative: str, *, budget: TransferBudget) -> None:
    pure = PurePosixPath(relative)
    if pure.is_absolute() or any(part in {"", ".", ".."} for part in pure.parts):
        raise TransferFailure("cst_saved_field.source_changed", "enumerate")
    if len(pure.parts) > budget.max_depth:
        raise TransferFailure("cst_saved_field.resource_limit_exceeded", "enumerate")


def inventory_manifest_v2(
    project: Path,
    *,
    enumeration_order: str = "native",
    budget: TransferBudget | None = None,
    root_capability: HeldDirectoryCapability | object | None = None,
) -> ManifestV2:
    """Inventory every regular default-stream file in the project result bundle."""

    limits = budget or TransferBudget()
    limits.check_time()
    rows: list[ManifestRowV2] = []
    total = 0
    identities: set[tuple[int, int]] = set()
    with _relative_file_owner(Path(project).parent, root_capability) as owner:
        members = list(_source_members(Path(project), limits, owner))
        if enumeration_order == "reverse":
            members.reverse()
        if len(members) > limits.max_entries or len(members) > limits.max_files:
            raise TransferFailure("cst_saved_field.resource_limit_exceeded", "enumerate")
        for relative in members:
            limits.check_time()
            _validate_relative(relative, budget=limits)
            descriptor = owner.open(relative)
            try:
                before = os.fstat(descriptor)
                if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
                    raise TransferFailure("cst_saved_field.source_changed", "enumerate")
                identity = (before.st_dev, before.st_ino)
                if identity in identities:
                    raise TransferFailure("cst_saved_field.source_changed", "enumerate")
                identities.add(identity)
                if before.st_size > limits.max_file_bytes:
                    raise TransferFailure("cst_saved_field.resource_limit_exceeded", "enumerate")
                total += before.st_size
                if total > limits.max_total_bytes:
                    raise TransferFailure("cst_saved_field.resource_limit_exceeded", "enumerate")
                with os.fdopen(descriptor, "rb", closefd=False) as handle:
                    digest = _sha256_handle(handle, limits)
                after = os.fstat(descriptor)
            finally:
                os.close(descriptor)
            stable = (
                before.st_dev,
                before.st_ino,
                before.st_size,
                before.st_mtime_ns,
                before.st_nlink,
            ) == (
                after.st_dev,
                after.st_ino,
                after.st_size,
                after.st_mtime_ns,
                after.st_nlink,
            )
            if not stable:
                raise TransferFailure("cst_saved_field.source_changed", "read")
            rows.append(
                ManifestRowV2(
                    path=relative,
                    type="regular",
                    stream="::$DATA",
                    size=before.st_size,
                    sha256=digest,
                    modified_ns=before.st_mtime_ns,
                )
            )
    ordered = tuple(sorted(rows, key=lambda row: row.path.encode("utf-8")))
    return ManifestV2(MANIFEST_SCHEMA, ordered, _canonical_aggregate(ordered))


@dataclass(slots=True)
class WorkspaceLease:
    path: Path
    capability: HeldDirectoryCapability | object | None = None
    _delete_capability: Callable[[object], None] | None = None
    _settled: bool = False

    @property
    def settled(self) -> bool:
        return self._settled

    def settle(self) -> None:
        if self._settled:
            return
        if self.capability is not None and self._delete_capability is not None:
            self._delete_capability(self.capability)
            self._settled = True
            return
        if self.capability is not None:
            self.capability.close()
        if self.path.exists():
            shutil.rmtree(self.path)
        self._settled = True


@dataclass(slots=True)
class TrustedWorkspacePolicy:
    """The sole factory for one disposable child beneath an injected root."""

    root: Path
    identity: WindowsPathIdentityV1 | None = None
    platform: PolicyPlatform | None = None
    root_capability: HeldDirectoryCapability | object | None = None

    @classmethod
    def from_verified_root(cls, root: Path, platform: PolicyPlatform) -> TrustedWorkspacePolicy:
        held = platform.hold_restricted_directory(Path(root))
        evidence = held.evidence
        if evidence.reparse or evidence.link_count != 1 or evidence.streams != () or not held.restricted:
            held.close()
            raise TransferFailure("cst_saved_field.workspace_invalid", "root_identity")
        return cls(Path(root), evidence, platform, held)

    @classmethod
    def from_windows_root(cls, raw: str, platform: PolicyPlatform) -> TrustedWorkspacePolicy:
        validate_windows_path_lexical(raw, absolute=True, role="workspace_root")
        if not platform.drive_is_local(raw[:2]):
            raise TransferFailure("cst_saved_field.workspace_invalid", "root_locality")
        return cls.from_verified_root(Path(raw), platform)

    def close(self) -> None:
        held = self.root_capability
        self.root_capability = None
        if held is not None:
            held.close()

    def create_child(self, correlation_id: str, *, exact_path: Path | None = None) -> WorkspaceLease:
        if not correlation_id or any(character not in "0123456789abcdef" for character in correlation_id):
            raise TransferFailure("cst_saved_field.workspace_invalid", "correlation")
        root = Path(self.root)
        if not root.is_absolute():
            raise TransferFailure("cst_saved_field.workspace_invalid", "root")
        owned_root = root
        if self.platform is not None:
            if self.root_capability is None:
                raise TransferFailure("cst_saved_field.workspace_invalid", "root_identity")
            revalidate = getattr(self.platform, "revalidate_held_directory", None)
            if callable(revalidate):
                current, restricted = revalidate(self.root_capability)
            else:
                current, restricted = self.root_capability.evidence, self.root_capability.restricted
            if current != self.identity or not restricted:
                raise TransferFailure("cst_saved_field.workspace_invalid", "root_identity")
            owned_root = Path(self.root_capability.path)
        elif not root.is_dir():
            raise TransferFailure("cst_saved_field.workspace_invalid", "root")
        child_name = (
            exact_path.name
            if exact_path is not None
            else f"cst-saved-field-{correlation_id}-{uuid.uuid4().hex}"
        )
        child = exact_path or owned_root / child_name
        if child.parent != owned_root:
            raise TransferFailure("cst_saved_field.workspace_invalid", "identity")
        if self.platform is not None:
            create_child = getattr(self.platform, "create_restricted_child", None)
            delete_tree = getattr(self.platform, "delete_restricted_tree", None)
            if not callable(create_child) or not callable(delete_tree):
                raise TransferFailure("cst_saved_field.workspace_invalid", "capability_owner")
            try:
                held_child = create_child(self.root_capability, child_name)
                evidence, restricted = held_child.evidence, held_child.restricted
                if evidence.reparse or evidence.link_count != 1:
                    raise TransferFailure("cst_saved_field.workspace_invalid", "identity")
                if not restricted:
                    raise TransferFailure("cst_saved_field.workspace_invalid", "access")
                return WorkspaceLease(
                    Path(held_child.path),
                    held_child,
                    delete_tree,
                )
            except BaseException:
                if "held_child" in locals():
                    delete_tree(held_child)
                raise
        return create_workspace_lease(child)


def create_workspace_lease(path: Path, *, fail_stage: str | None = None) -> WorkspaceLease:
    """Create exactly one child transactionally; never remove its siblings."""

    child = Path(path)
    if child.exists():
        raise TransferFailure("cst_saved_field.workspace_invalid", "create")
    created = False
    permission_set = False
    identity_proven = False
    initialized = False
    stage = "create"
    try:
        if fail_stage == "create":
            raise TransferFailure("cst_saved_field.workspace_invalid", "create")
        os.mkdir(child)
        created = True
        stage = "permission"
        if fail_stage == stage:
            raise TransferFailure("cst_saved_field.workspace_invalid", stage)
        child.chmod(0o700)
        permission_set = True
        stage = "identity"
        if fail_stage == stage:
            raise TransferFailure("cst_saved_field.workspace_invalid", stage)
        identity_proven = Path(os.path.abspath(child)) == child.absolute()
        if not identity_proven:
            raise TransferFailure("cst_saved_field.workspace_invalid", stage)
        stage = "initialize"
        if fail_stage == stage:
            raise TransferFailure("cst_saved_field.workspace_invalid", stage)
        initialized = not any(child.iterdir())
        if not initialized:
            raise TransferFailure("cst_saved_field.workspace_invalid", stage)
        stage = "before_transfer"
        if fail_stage == stage:
            raise TransferFailure("cst_saved_field.workspace_invalid", stage)
        return WorkspaceLease(child)
    except Exception as exc:
        if created and child.exists():
            shutil.rmtree(child)
        settlement = WorkspaceSettlement(
            stage=stage,
            child_created=created,
            permission_set=permission_set,
            identity_proven=identity_proven,
            initialized=initialized,
            lease_transferred=False,
            child_removed=not child.exists(),
        )
        raise TransferFailure(
            "cst_saved_field.workspace_invalid",
            stage,
            workspace_settlement=settlement,
        ) from exc


class _SnapshotVendorPathLease:
    """One non-copyable lease whose terminal receipt gates snapshot deletion."""

    __slots__ = ("_inner", "_settlement")

    def __init__(self, inner: AuthorizedVendorPathLease) -> None:
        self._inner = inner
        self._settlement: VendorPathLeaseSettlement | None = None

    def __copy__(self):
        raise TypeError("AuthorizedVendorPathLease is non-copyable")

    def __deepcopy__(self, _memo):
        raise TypeError("AuthorizedVendorPathLease is non-copyable")

    @property
    def settled(self) -> bool:
        return self._settlement is not None and self._settlement.complete

    def hold_ancestor(self, relative: str) -> str:
        return self._inner.hold_ancestor(relative)

    def hold_read_input(self, relative: str) -> str:
        return self._inner.hold_read_input(relative)

    def prepare_output(self, relative: str) -> str:
        return self._inner.prepare_output(relative)

    def seal_output(self, relative: str) -> SealedVendorOutput:
        return self._inner.seal_output(relative)

    def create_clean_input(
        self, source_relative: str, destination_relative: str, expected_sha256: str
    ) -> str:
        return self._inner.create_clean_input(
            source_relative,
            destination_relative,
            expected_sha256,
        )

    def revalidate_all(self) -> None:
        self._inner.revalidate_all()

    def settle(self) -> VendorPathLeaseSettlement:
        if self._settlement is None or not self._settlement.complete:
            self._settlement = self._inner.settle()
        return self._settlement


VendorLeaseFactory = Callable[[object], AuthorizedVendorPathLease]


@dataclass(slots=True)
class AuthorizedWorkspaceSnapshot:
    manifest: ManifestV2
    _path: Path
    _lease: WorkspaceLease
    capability: HeldDirectoryCapability | object | None = None
    budget: TransferBudget | None = None
    _vendor_lease_factory: VendorLeaseFactory | None = None
    _vendor_lease: _SnapshotVendorPathLease | None = None
    _settled: bool = False

    @property
    def settled(self) -> bool:
        return self._settled and self._lease.settled

    def settle(self) -> None:
        if not self._settled:
            if self._vendor_lease is not None and not self._vendor_lease.settled:
                raise TransferFailure("cst_saved_field.workspace_settle_failed", "vendor_lease")
            self._lease.settle()
            self._settled = True

    def create_vendor_path_lease(self) -> AuthorizedVendorPathLease:
        if self._settled or self._vendor_lease is not None or self._vendor_lease_factory is None:
            raise TransferFailure("cst_saved_field.vendor_isolation_unavailable", "vendor_lease")
        inner = self._vendor_lease_factory(self.capability)
        self._vendor_lease = _SnapshotVendorPathLease(inner)
        return self._vendor_lease


BoundaryHook = Callable[[str, str | None], None]


class AuthorizedBundleTransfer:
    """Copy an exact manifest from held source handles into one owned workspace."""

    _BOUNDARIES = (
        "enumerate",
        "pre_open",
        "read",
        "copy",
        "source_close",
        "destination_enumerate",
        "pre_commit",
    )

    def __init__(
        self,
        expected: ManifestV2,
        *,
        boundary_hook: BoundaryHook | None = None,
        budget: TransferBudget | None = None,
    ) -> None:
        if expected.schema != MANIFEST_SCHEMA:
            raise TransferFailure("cst_saved_field.source_changed", "manifest")
        self._expected = expected
        self._hook = boundary_hook or (lambda _stage, _relative: None)
        self._budget = budget or TransferBudget()

    def _verify_source(
        self,
        project: Path,
        stage: str,
        source_capability: HeldDirectoryCapability | object | None,
    ) -> None:
        self._hook(stage, None)
        if (
            inventory_manifest_v2(project, budget=self._budget, root_capability=source_capability)
            != self._expected
        ):
            raise TransferFailure("cst_saved_field.source_changed", stage)

    def execute(
        self,
        project: Path,
        workspace: Path,
        *,
        workspace_lease: WorkspaceLease | None = None,
        source_capability: HeldDirectoryCapability | object | None = None,
        vendor_lease_factory: VendorLeaseFactory | None = None,
        on_vendor_start: Callable[[AuthorizedWorkspaceSnapshot], None] | None = None,
    ) -> AuthorizedWorkspaceSnapshot:
        project = Path(project)
        lease = workspace_lease or TrustedWorkspacePolicy(Path(workspace).parent).create_child(
            "0", exact_path=Path(workspace)
        )
        if lease.path != Path(workspace):
            raise TransferFailure("cst_saved_field.workspace_invalid", "identity")
        try:
            if len(self._expected.rows) > self._budget.max_files:
                raise TransferFailure("cst_saved_field.resource_limit_exceeded", "enumerate")
            self._verify_source(project, "enumerate", source_capability)
            self._verify_source(project, "pre_open", source_capability)
            source_parent = Path(os.path.abspath(project)).parent
            with (
                _relative_file_owner(source_parent, source_capability) as source_owner,
                _relative_file_owner(lease.path, lease.capability) as destination_owner,
            ):
                for row in self._expected.rows:
                    self._budget.check_time()
                    destination_owner.ensure_parent(row.path)
                    self._verify_source(project, "read", source_capability)
                    self._verify_source(project, "copy", source_capability)
                    try:
                        source_fd = source_owner.open(row.path)
                    except OSError as exc:
                        raise TransferFailure("cst_saved_field.source_changed", "pre_open") from exc
                    try:
                        before = os.fstat(source_fd)
                        with os.fdopen(source_fd, "rb", closefd=False) as source_handle:
                            destination_fd = destination_owner.open(row.path, create=True)
                            try:
                                with os.fdopen(destination_fd, "wb", closefd=False) as destination_handle:
                                    digest = hashlib.sha256()
                                    count = 0
                                    while block := source_handle.read(1024 * 1024):
                                        self._budget.check_time()
                                        count += len(block)
                                        if count > self._budget.max_file_bytes:
                                            raise TransferFailure(
                                                "cst_saved_field.resource_limit_exceeded",
                                                "copy",
                                            )
                                        digest.update(block)
                                        destination_handle.write(block)
                                        self._budget.check_time()
                                    destination_handle.flush()
                                    os.fsync(destination_fd)
                                destination_owner.set_modified_ns(destination_fd, row.modified_ns)
                            finally:
                                os.close(destination_fd)
                        after = os.fstat(source_fd)
                        if (
                            before.st_dev,
                            before.st_ino,
                            before.st_size,
                            before.st_mtime_ns,
                            digest.hexdigest(),
                        ) != (
                            after.st_dev,
                            after.st_ino,
                            after.st_size,
                            after.st_mtime_ns,
                            row.sha256,
                        ):
                            raise TransferFailure("cst_saved_field.source_changed", "copy")
                    finally:
                        os.close(source_fd)
            self._verify_source(project, "source_close", source_capability)
            self._verify_source(project, "destination_enumerate", source_capability)
            destination_project = lease.path / project.name
            copied = inventory_manifest_v2(
                destination_project,
                budget=self._budget,
                root_capability=lease.capability,
            )
            if copied != self._expected:
                raise TransferFailure("cst_saved_field.authorized_copy_changed", "destination_enumerate")
            self._verify_source(project, "pre_commit", source_capability)
            snapshot = AuthorizedWorkspaceSnapshot(
                manifest=copied,
                _path=lease.path,
                _lease=lease,
                capability=lease.capability,
                budget=self._budget,
                _vendor_lease_factory=vendor_lease_factory,
            )
            if on_vendor_start is not None:
                on_vendor_start(snapshot)
            return snapshot
        except Exception as exc:
            stage = exc.stage if isinstance(exc, TransferFailure) else "transfer"
            try:
                lease.settle()
            except Exception as settlement_error:
                settlement = WorkspaceSettlement(
                    stage=stage,
                    child_created=True,
                    permission_set=True,
                    identity_proven=True,
                    initialized=True,
                    lease_transferred=False,
                    child_removed=lease.settled,
                )
                raise TransferFailure(
                    "cst_saved_field.workspace_settle_failed",
                    stage,
                    workspace_settlement=settlement,
                ) from settlement_error
            settlement = WorkspaceSettlement(
                stage=stage,
                child_created=True,
                permission_set=True,
                identity_proven=True,
                initialized=True,
                lease_transferred=False,
                child_removed=lease.settled,
            )
            if isinstance(exc, TransferFailure):
                raise TransferFailure(
                    exc.failure_id,
                    exc.stage,
                    workspace_settlement=settlement,
                ) from exc
            raise TransferFailure(
                "cst_saved_field.authorized_copy_changed",
                stage,
                workspace_settlement=settlement,
            ) from exc
