"""Closed frontend-to-daemon value protocol for the saved-field sampler."""

from __future__ import annotations

import hashlib
import json
import re
from collections.abc import Mapping
from dataclasses import asdict, dataclass
from typing import Any, ClassVar

from .cst_saved_field_port import AbsoluteInvocationBudget

FRONTEND_REQUEST_SCHEMA = "mcphub.cst.saved_field.frontend_request.v1"
FRONTEND_RESULT_SCHEMA = "mcphub.cst.saved_field.frontend_result.v1"
FRONTEND_PROTOCOL_V1 = "FrontendDaemonProtocolV1"
FRONTEND_FRAME_MAX = 1_114_112
FRONTEND_REQUEST_MAX = 131_072
FRONTEND_SAFE_FAILURE_IDS = frozenset(
    {
        "cst_saved_field.activation_failed",
        "cst_saved_field.authorized_copy_changed",
        "cst_saved_field.broker_protocol_invalid",
        "cst_saved_field.broker_unauthorized",
        "cst_saved_field.broker_unavailable",
        "cst_saved_field.broker_worker_protocol_invalid",
        "cst_saved_field.bundle_not_authorized",
        "cst_saved_field.capability_unavailable",
        "cst_saved_field.containment_configuration_invalid",
        "cst_saved_field.containment_quarantined",
        "cst_saved_field.containment_residual_process",
        "cst_saved_field.containment_settle_failed",
        "cst_saved_field.containment_startup_invalid",
        "cst_saved_field.cst_unavailable",
        "cst_saved_field.daemon_unavailable",
        "cst_saved_field.deadline_exceeded",
        "cst_saved_field.field_identity_mismatch",
        "cst_saved_field.frame_ambiguous",
        "cst_saved_field.frame_missing",
        "cst_saved_field.frame_selector_mismatch",
        "cst_saved_field.frontend_protocol_invalid",
        "cst_saved_field.frontend_unavailable",
        "cst_saved_field.internal_error",
        "cst_saved_field.metadata_unavailable",
        "cst_saved_field.not_authorized",
        "cst_saved_field.path_identity_ambiguous",
        "cst_saved_field.path_namespace_invalid",
        "cst_saved_field.point_sample_failed",
        "cst_saved_field.policy_disabled",
        "cst_saved_field.policy_invalid",
        "cst_saved_field.policy_revision_changed",
        "cst_saved_field.resource_busy",
        "cst_saved_field.resource_limit_exceeded",
        "cst_saved_field.response_too_large",
        "cst_saved_field.session_ownership_ambiguous",
        "cst_saved_field.session_settle_failed",
        "cst_saved_field.source_changed",
        "cst_saved_field.source_identity_mismatch",
        "cst_saved_field.vendor_record_invalid",
        "cst_saved_field.vendor_status_unverified",
        "cst_saved_field.workspace_invalid",
        "cst_saved_field.workspace_settle_failed",
    }
)
_HEX32 = re.compile(r"[0-9a-f]{32}\Z")
_HEX64 = re.compile(r"[0-9a-f]{64}\Z")
_ENTRY = re.compile(r"[a-z0-9][a-z0-9._-]{0,63}\Z")


class FrontendProtocolFailure(RuntimeError):
    failure_id = "cst_saved_field.frontend_protocol_invalid"


def _fail() -> FrontendProtocolFailure:
    return FrontendProtocolFailure(FrontendProtocolFailure.failure_id)


def _closed(value: object, expected: set[str]) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != expected:
        raise _fail()
    return value


def _canonical(value: object) -> bytes:
    try:
        return json.dumps(
            value, ensure_ascii=False, allow_nan=False, sort_keys=True, separators=(",", ":")
        ).encode()
    except (TypeError, ValueError) as exc:
        raise _fail() from exc


def canonical_sha256(value: object) -> str:
    return hashlib.sha256(_canonical(value)).hexdigest()


def encode_frame(value: object, *, maximum: int) -> bytes:
    raw = _canonical(value)
    if not raw or len(raw) > maximum:
        raise _fail()
    return raw


def _pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise _fail()
        result[key] = value
    return result


def decode_frame(raw: bytes, *, maximum: int) -> dict[str, Any]:
    if not isinstance(raw, bytes) or not raw or len(raw) > maximum:
        raise _fail()
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_pairs)
    except (UnicodeError, json.JSONDecodeError, TypeError, FrontendProtocolFailure) as exc:
        raise _fail() from exc
    if not isinstance(value, dict) or encode_frame(value, maximum=maximum) != raw:
        raise _fail()
    return value


@dataclass(frozen=True, slots=True)
class FrontendDaemonRequestV1:
    correlation_id: str
    challenge_nonce: str
    launch_capability: str
    entry_id: str
    request_sha256: str
    request: Mapping[str, object]
    schema: ClassVar[str] = FRONTEND_REQUEST_SCHEMA

    def __post_init__(self) -> None:
        if (
            not _HEX32.fullmatch(self.correlation_id)
            or not _HEX64.fullmatch(self.challenge_nonce)
            or not _HEX64.fullmatch(self.launch_capability)
            or not _ENTRY.fullmatch(self.entry_id)
            or not _HEX64.fullmatch(self.request_sha256)
            or canonical_sha256(dict(self.request)) != self.request_sha256
        ):
            raise _fail()

    def to_wire(self) -> dict[str, object]:
        return {
            "schema": self.schema,
            "correlation_id": self.correlation_id,
            "challenge_nonce": self.challenge_nonce,
            "launch_capability": self.launch_capability,
            "entry_id": self.entry_id,
            "request_sha256": self.request_sha256,
            "request": dict(self.request),
        }

    @classmethod
    def from_wire(cls, value: object) -> FrontendDaemonRequestV1:
        item = _closed(
            value,
            {
                "schema",
                "correlation_id",
                "challenge_nonce",
                "launch_capability",
                "entry_id",
                "request_sha256",
                "request",
            },
        )
        if item["schema"] != cls.schema or not isinstance(item["request"], dict):
            raise _fail()
        return cls(
            item["correlation_id"],
            item["challenge_nonce"],
            item["launch_capability"],
            item["entry_id"],
            item["request_sha256"],
            item["request"],
        )


@dataclass(frozen=True, slots=True)
class FrontendDaemonResultV1:
    correlation_id: str
    entry_id: str
    request_sha256: str
    budget: AbsoluteInvocationBudget
    ok: bool
    text: str | None
    failure_id: str | None
    schema: ClassVar[str] = FRONTEND_RESULT_SCHEMA

    def __post_init__(self) -> None:
        if (
            not _HEX32.fullmatch(self.correlation_id)
            or not _ENTRY.fullmatch(self.entry_id)
            or not _HEX64.fullmatch(self.request_sha256)
            or not isinstance(self.budget, AbsoluteInvocationBudget)
            or type(self.ok) is not bool
            or self.ok != (self.text is not None and self.failure_id is None)
            or (self.text is not None and len(self.text.encode()) > FRONTEND_FRAME_MAX)
            or (self.failure_id is not None and self.failure_id not in FRONTEND_SAFE_FAILURE_IDS)
        ):
            raise _fail()

    def to_wire(self) -> dict[str, object]:
        return {
            "schema": self.schema,
            "correlation_id": self.correlation_id,
            "entry_id": self.entry_id,
            "request_sha256": self.request_sha256,
            "budget": self.budget.to_wire(),
            "ok": self.ok,
            "text": self.text,
            "failure_id": self.failure_id,
        }

    @classmethod
    def from_wire(cls, value: object) -> FrontendDaemonResultV1:
        item = _closed(
            value,
            {
                "schema",
                "correlation_id",
                "entry_id",
                "request_sha256",
                "budget",
                "ok",
                "text",
                "failure_id",
            },
        )
        if item.pop("schema") != cls.schema:
            raise _fail()
        item["budget"] = AbsoluteInvocationBudget.from_wire(item["budget"])
        return cls(**item)


@dataclass(frozen=True, slots=True)
class DaemonResponseReceiptV1:
    correlation_id: str
    response_frame_written: bool
    terminal_frame_written: bool
    flush_complete: bool
    ack_received: bool
    disconnect_complete: bool
    server_handle_closed: bool

    def __post_init__(self) -> None:
        if not _HEX32.fullmatch(self.correlation_id) or any(
            type(value) is not bool
            for value in (
                self.response_frame_written,
                self.terminal_frame_written,
                self.flush_complete,
                self.ack_received,
                self.disconnect_complete,
                self.server_handle_closed,
            )
        ):
            raise _fail()

    def to_wire(self) -> dict[str, object]:
        return asdict(self)

    @classmethod
    def from_wire(cls, value: object) -> DaemonResponseReceiptV1:
        item = _closed(
            value,
            {
                "correlation_id",
                "response_frame_written",
                "terminal_frame_written",
                "flush_complete",
                "ack_received",
                "disconnect_complete",
                "server_handle_closed",
            },
        )
        return cls(**item)

    @property
    def complete(self) -> bool:
        return bool(_HEX32.fullmatch(self.correlation_id)) and all(asdict(self).values())


@dataclass(frozen=True, slots=True)
class FrontendTransportReceiptV1:
    correlation_id: str
    response_frame_complete: bool
    terminal_frame_complete: bool
    eof_or_cancel: bool
    client_handle_closed: bool

    def __post_init__(self) -> None:
        if not _HEX32.fullmatch(self.correlation_id) or any(
            type(value) is not bool
            for value in (
                self.response_frame_complete,
                self.terminal_frame_complete,
                self.eof_or_cancel,
                self.client_handle_closed,
            )
        ):
            raise _fail()

    def to_wire(self) -> dict[str, object]:
        return asdict(self)

    @classmethod
    def from_wire(cls, value: object) -> FrontendTransportReceiptV1:
        item = _closed(
            value,
            {
                "correlation_id",
                "response_frame_complete",
                "terminal_frame_complete",
                "eof_or_cancel",
                "client_handle_closed",
            },
        )
        return cls(**item)

    @property
    def complete(self) -> bool:
        return bool(_HEX32.fullmatch(self.correlation_id)) and all(asdict(self).values())
