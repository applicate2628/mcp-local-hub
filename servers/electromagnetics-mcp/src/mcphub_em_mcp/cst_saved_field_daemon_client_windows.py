"""Thin saved-field frontend client; owns no admission, source, broker, or CST work."""

from __future__ import annotations

import os
import re
import threading
from collections.abc import Callable, Mapping
from typing import Protocol

from .cst_saved_field_frontend_protocol import (
    FrontendDaemonRequestV1,
    FrontendDaemonResultV1,
    FrontendTransportReceiptV1,
    canonical_sha256,
)
from .cst_saved_field_policy import EXACT_ENDPOINTS

FRONTEND_ENDPOINT_V1 = EXACT_ENDPOINTS[1]
FRONTEND_OPERATION_TIMEOUT_SECONDS = 5.0
LAUNCH_CAPABILITY_HANDLE_ENV = "MCPHUB_CST_LAUNCH_HANDLE"
_ENTRY = re.compile(r"[a-z0-9][a-z0-9._-]{0,63}\Z")


class DaemonClientFailure(RuntimeError):
    def __init__(self, failure_id: str) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id

    def __str__(self) -> str:
        return self.failure_id


class DaemonTransport(Protocol):
    def startup_proof(self, timeout: float) -> bool: ...

    def challenge(self, correlation: str, timeout: float) -> str: ...

    def exchange(
        self, request: FrontendDaemonRequestV1, timeout: float
    ) -> tuple[FrontendDaemonResultV1, FrontendTransportReceiptV1]: ...

    def cancel(self, correlation: str, timeout: float) -> bool: ...


class WindowsNamedPipeDaemonTransport:
    """Fixed production descriptor; T06 composes the concrete overlapped channel."""

    def __init__(self, channel: DaemonTransport | None = None) -> None:
        self._channel = channel

    def startup_proof(self, timeout: float) -> bool:
        return (
            timeout == FRONTEND_OPERATION_TIMEOUT_SECONDS
            and os.name == "nt"
            and self._channel is not None
            and self._channel.startup_proof(timeout)
        )

    def challenge(self, correlation: str, timeout: float) -> str:
        if self._channel is None:
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable")
        return self._channel.challenge(correlation, timeout)

    def exchange(
        self, request: FrontendDaemonRequestV1, timeout: float
    ) -> tuple[FrontendDaemonResultV1, FrontendTransportReceiptV1]:
        if self._channel is None:
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable")
        return self._channel.exchange(request, timeout)

    def cancel(self, correlation: str, timeout: float) -> bool:
        return self._channel is not None and self._channel.cancel(correlation, timeout)


def _production_open_handle(locator: str) -> int:
    if os.name != "nt" or not locator.isascii() or not locator.isdecimal():
        raise DaemonClientFailure("cst_saved_field.capability_unavailable")
    import msvcrt

    raw = int(locator, 10)
    if raw <= 0:
        raise DaemonClientFailure("cst_saved_field.capability_unavailable")
    return msvcrt.open_osfhandle(raw, os.O_RDONLY)


def read_inherited_launch_capability(
    locator: str,
    *,
    open_handle: Callable[[str], int] = _production_open_handle,
    read_handle: Callable[[int, int], bytes] = os.read,
    close_handle: Callable[[int], object] = os.close,
) -> bytes:
    handle: int | None = None
    try:
        handle = open_handle(locator)
        value = bytearray()
        while len(value) <= 32:
            chunk = read_handle(handle, 33 - len(value))
            if not chunk:
                break
            value.extend(chunk)
        if len(value) != 32:
            raise DaemonClientFailure("cst_saved_field.capability_unavailable")
        return bytes(value)
    except DaemonClientFailure:
        raise
    except Exception as exc:
        raise DaemonClientFailure("cst_saved_field.capability_unavailable") from exc
    finally:
        if handle is not None:
            try:
                close_handle(handle)
            except Exception as exc:
                raise DaemonClientFailure("cst_saved_field.capability_unavailable") from exc


class FrontendChallengeLedger:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._state: dict[str, str] = {}

    def issue(self, correlation: str) -> None:
        with self._lock:
            if self._state:
                raise DaemonClientFailure("cst_saved_field.frontend_unavailable")
            self._state[correlation] = "ISSUED"

    def terminalize(self, correlation: str, state: str) -> None:
        with self._lock:
            if self._state.get(correlation) != "ISSUED" or state not in {"CONSUMED", "CANCELLED"}:
                raise DaemonClientFailure("cst_saved_field.frontend_unavailable")
            self._state[correlation] = state


class WindowsDaemonClient:
    def __init__(
        self,
        *,
        transport: DaemonTransport,
        capability: bytearray,
        correlation: Callable[[], str],
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
    ) -> None:
        if len(capability) != 32:
            raise DaemonClientFailure("cst_saved_field.capability_unavailable")
        self._transport = transport
        self._capability = capability
        self._correlation = correlation
        self._qpc_frequency = qpc_frequency
        self._qpc_counter = qpc_counter
        self._ledger = FrontendChallengeLedger()
        self._used = False

    def startup_ready(self) -> bool:
        return not self._used and self._transport.startup_proof(FRONTEND_OPERATION_TIMEOUT_SECONDS)

    def invoke(self, *, entry_id: str, request: Mapping[str, object]) -> FrontendDaemonResultV1:
        if self._used or _ENTRY.fullmatch(entry_id) is None:
            raise DaemonClientFailure("cst_saved_field.frontend_unavailable")
        correlation = self._correlation()
        self._ledger.issue(correlation)
        challenge_obtained = False
        try:
            challenge = self._transport.challenge(correlation, FRONTEND_OPERATION_TIMEOUT_SECONDS)
            challenge_obtained = True
            request_hash = canonical_sha256(dict(request))
            frame = FrontendDaemonRequestV1(
                correlation,
                challenge,
                bytes(self._capability).hex(),
                entry_id,
                request_hash,
                dict(request),
            )
            result, receipt = self._transport.exchange(frame, FRONTEND_OPERATION_TIMEOUT_SECONDS)
            if (
                not isinstance(result, FrontendDaemonResultV1)
                or result.correlation_id != correlation
                or result.entry_id != entry_id
                or result.request_sha256 != request_hash
                or not isinstance(receipt, FrontendTransportReceiptV1)
                or receipt.correlation_id != correlation
                or not receipt.complete
                or result.budget.qpc_frequency != self._qpc_frequency()
                or self._qpc_counter() >= result.budget.deadline_tick
            ):
                raise DaemonClientFailure("cst_saved_field.frontend_protocol_invalid")
            self._ledger.terminalize(correlation, "CONSUMED")
            return result
        except DaemonClientFailure:
            if challenge_obtained:
                self._transport.cancel(correlation, FRONTEND_OPERATION_TIMEOUT_SECONDS)
            self._ledger.terminalize(correlation, "CANCELLED")
            raise
        except Exception as exc:
            if challenge_obtained:
                self._transport.cancel(correlation, FRONTEND_OPERATION_TIMEOUT_SECONDS)
            self._ledger.terminalize(correlation, "CANCELLED")
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable") from exc
        finally:
            self._used = True
            self._capability[:] = b"\x00" * len(self._capability)


def inherited_daemon_client(
    *,
    correlation: Callable[[], str],
    qpc_frequency: Callable[[], int],
    qpc_counter: Callable[[], int],
    transport: DaemonTransport | None = None,
) -> WindowsDaemonClient | None:
    locator = os.environ.pop(LAUNCH_CAPABILITY_HANDLE_ENV, "")
    if not locator:
        return None
    try:
        capability = bytearray(read_inherited_launch_capability(locator))
        client = WindowsDaemonClient(
            transport=transport or WindowsNamedPipeDaemonTransport(),
            capability=capability,
            correlation=correlation,
            qpc_frequency=qpc_frequency,
            qpc_counter=qpc_counter,
        )
        if client.startup_ready():
            return client
        capability[:] = b"\x00" * len(capability)
        return None
    except DaemonClientFailure:
        return None
