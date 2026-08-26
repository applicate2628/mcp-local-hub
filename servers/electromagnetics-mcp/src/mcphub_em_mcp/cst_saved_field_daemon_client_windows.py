"""Thin saved-field frontend client; owns no admission, source, broker, or CST work."""

from __future__ import annotations

import os
import re
import threading
from collections.abc import Callable, Mapping
from typing import BinaryIO, Protocol

from .cst_saved_field_frontend_protocol import (
    FRONTEND_FRAME_MAX,
    FRONTEND_REQUEST_MAX,
    FrontendDaemonRequestV1,
    FrontendDaemonResultV1,
    FrontendTransportReceiptV1,
    canonical_sha256,
    decode_frame,
    encode_frame,
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
    """Frontend owner for the fixed local daemon pipe and its observed receipt."""

    def __init__(self, connector: Callable[[str], BinaryIO] | None = None) -> None:
        self._connector = connector or _open_fixed_frontend_pipe
        self._channel: BinaryIO | None = None
        self._correlation: str | None = None

    @staticmethod
    def _write(channel: BinaryIO, value: object) -> None:
        raw = encode_frame(value, maximum=FRONTEND_REQUEST_MAX) + b"\n"
        if channel.write(raw) != len(raw):
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable")

    @staticmethod
    def _read(channel: BinaryIO) -> dict[str, object]:
        raw = channel.readline(FRONTEND_FRAME_MAX + 2)
        if not raw.endswith(b"\n") or len(raw) > FRONTEND_FRAME_MAX + 1:
            raise DaemonClientFailure("cst_saved_field.frontend_protocol_invalid")
        return decode_frame(raw[:-1], maximum=FRONTEND_FRAME_MAX)

    def startup_proof(self, timeout: float) -> bool:
        return (
            timeout == FRONTEND_OPERATION_TIMEOUT_SECONDS
            and os.name == "nt"
            and (self._connector is not _open_fixed_frontend_pipe or _fixed_pipe_ready(timeout))
        )

    def challenge(self, correlation: str, timeout: float) -> str:
        if timeout != FRONTEND_OPERATION_TIMEOUT_SECONDS or self._channel is not None:
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable")
        try:
            channel = self._connector(FRONTEND_ENDPOINT_V1)
            self._write(channel, {"op": "challenge", "correlation_id": correlation})
            channel.flush()
            frame = self._read(channel)
            if set(frame) != {"op", "correlation_id", "challenge_nonce"}:
                raise DaemonClientFailure("cst_saved_field.frontend_protocol_invalid")
            if frame["op"] != "challenge" or frame["correlation_id"] != correlation:
                raise DaemonClientFailure("cst_saved_field.frontend_protocol_invalid")
            challenge = frame["challenge_nonce"]
            if not isinstance(challenge, str):
                raise DaemonClientFailure("cst_saved_field.frontend_protocol_invalid")
            self._channel = channel
            self._correlation = correlation
            return challenge
        except DaemonClientFailure:
            raise
        except Exception as exc:
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable") from exc

    def exchange(
        self, request: FrontendDaemonRequestV1, timeout: float
    ) -> tuple[FrontendDaemonResultV1, FrontendTransportReceiptV1]:
        channel = self._channel
        if channel is None or self._correlation != request.correlation_id:
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable")
        response_complete = terminal_complete = eof = closed = False
        result: FrontendDaemonResultV1 | None = None
        try:
            self._write(channel, {"op": "invoke", "request": request.to_wire()})
            channel.flush()
            result = FrontendDaemonResultV1.from_wire(self._read(channel))
            response_complete = True
            terminal = self._read(channel)
            terminal_complete = terminal == {
                "op": "terminal",
                "correlation_id": request.correlation_id,
            }
            self._write(channel, {"op": "ack", "correlation_id": request.correlation_id})
            channel.flush()
            eof = channel.read(1) == b""
        except DaemonClientFailure:
            raise
        except Exception as exc:
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable") from exc
        finally:
            try:
                channel.close()
                closed = True
            finally:
                self._channel = None
                self._correlation = None
        if result is None:
            raise DaemonClientFailure("cst_saved_field.daemon_unavailable")
        return result, FrontendTransportReceiptV1(
            request.correlation_id,
            response_complete,
            terminal_complete,
            eof,
            closed,
        )

    def cancel(self, correlation: str, timeout: float) -> bool:
        channel = self._channel
        self._channel = None
        self._correlation = None
        if channel is None or timeout != FRONTEND_OPERATION_TIMEOUT_SECONDS:
            return False
        try:
            self._write(channel, {"op": "cancel", "correlation_id": correlation})
            channel.flush()
            return self._read(channel) == {"op": "cancelled", "correlation_id": correlation}
        except Exception:
            return False
        finally:
            channel.close()


def _open_fixed_frontend_pipe(endpoint: str) -> BinaryIO:
    if os.name != "nt" or endpoint != FRONTEND_ENDPOINT_V1:
        raise DaemonClientFailure("cst_saved_field.daemon_unavailable")
    return open(endpoint, "r+b", buffering=0)


def _fixed_pipe_ready(timeout: float) -> bool:
    import ctypes

    return bool(
        ctypes.WinDLL("kernel32", use_last_error=True).WaitNamedPipeW(
            FRONTEND_ENDPOINT_V1,
            max(1, int(timeout * 1000)),
        )
    )


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
