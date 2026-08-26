"""Closed daemon-to-broker protocol for the saved-field sampler."""

from __future__ import annotations

import hashlib
import json
import re
from collections.abc import Mapping
from dataclasses import asdict, dataclass
from typing import Any, BinaryIO, ClassVar

from .cst_saved_field_port import AbsoluteInvocationBudget

BROKER_CHALLENGE_SCHEMA = "mcphub.cst.saved_field.broker_challenge.v1"
BROKER_REQUEST_SCHEMA = "mcphub.cst.saved_field.broker_request.v1"
BROKER_RESPONSE_SCHEMA = "mcphub.cst.saved_field.broker_response.v1"
BROKER_PROTOCOL_V1 = "BrokerProtocolV1"
BROKER_FRAME_MAX = 1_114_112
BROKER_REQUEST_MAX = 131_072
PUBLIC_TEXT_MAX = 1_048_576
NONCE_BYTES = 32
NONCE_LIFETIME_SECONDS = 5
SAFE_FAILURE_IDS = frozenset(
    {
        "cst_saved_field.activation_failed",
        "cst_saved_field.authorized_copy_changed",
        "cst_saved_field.broker_protocol_invalid",
        "cst_saved_field.broker_unauthorized",
        "cst_saved_field.broker_unavailable",
        "cst_saved_field.broker_worker_protocol_invalid",
        "cst_saved_field.bundle_not_authorized",
        "cst_saved_field.containment_configuration_invalid",
        "cst_saved_field.containment_quarantined",
        "cst_saved_field.containment_residual_process",
        "cst_saved_field.containment_settle_failed",
        "cst_saved_field.containment_startup_invalid",
        "cst_saved_field.cst_unavailable",
        "cst_saved_field.deadline_exceeded",
        "cst_saved_field.field_identity_mismatch",
        "cst_saved_field.frame_ambiguous",
        "cst_saved_field.frame_missing",
        "cst_saved_field.frame_selector_mismatch",
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
_HEX_32 = re.compile(r"[0-9a-f]{32}\Z")
_HEX_64 = re.compile(r"[0-9a-f]{64}\Z")
_ENTRY = re.compile(r"[a-z0-9][a-z0-9._-]{0,63}\Z")


class BrokerProtocolFailure(RuntimeError):
    failure_id = "cst_saved_field.broker_protocol_invalid"

    def __init__(self, failure_id: str = "cst_saved_field.broker_protocol_invalid") -> None:
        if failure_id not in SAFE_FAILURE_IDS:
            failure_id = "cst_saved_field.broker_protocol_invalid"
        super().__init__(failure_id)
        self.failure_id = failure_id


def _fail() -> BrokerProtocolFailure:
    return BrokerProtocolFailure(BrokerProtocolFailure.failure_id)


def _closed(value: object, expected: set[str]) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != expected:
        raise _fail()
    return value


def _canonical(value: object) -> bytes:
    try:
        return json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise _fail() from exc


def canonical_sha256(value: object) -> str:
    return hashlib.sha256(_canonical(value)).hexdigest()


def encode_frame(value: object, *, maximum: int = BROKER_FRAME_MAX) -> bytes:
    payload = _canonical(value)
    if not payload or len(payload) > maximum:
        raise _fail()
    return len(payload).to_bytes(4, "big") + payload


def decode_one_frame(source: BinaryIO, *, maximum: int = BROKER_FRAME_MAX) -> dict[str, Any]:
    header = source.read(4)
    if len(header) != 4:
        raise _fail()
    size = int.from_bytes(header, "big")
    if size <= 0 or size > maximum:
        raise _fail()
    payload = source.read(size)
    if len(payload) != size or source.read(1) != b"":
        raise _fail()
    try:
        value = json.loads(payload, object_pairs_hook=_unique_pairs)
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise _fail() from exc
    if not isinstance(value, dict) or _canonical(value) != payload:
        raise _fail()
    return value


def _unique_pairs(pairs: list[tuple[str, object]]) -> dict[str, object]:
    value: dict[str, object] = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate member")
        value[key] = item
    return value


class QpcDeadlineV1(AbsoluteInvocationBudget):
    """Wire-compatible protocol name for the neutral absolute budget."""

    @classmethod
    def from_wire(cls, value: object) -> QpcDeadlineV1:
        try:
            parsed = AbsoluteInvocationBudget.from_wire(value)
            return cls(parsed.qpc_frequency, parsed.admitted_tick, parsed.deadline_tick)
        except ValueError as exc:
            raise _fail() from exc

    def remaining(self, *, current_frequency: int, current_tick: int) -> float:
        try:
            return super().remaining(current_frequency=current_frequency, current_tick=current_tick)
        except ValueError as exc:
            raise _fail() from exc


@dataclass(frozen=True, slots=True)
class BrokerChallengeV1:
    nonce: str
    issued_tick: int
    expires_tick: int
    qpc_frequency: int
    schema: ClassVar[str] = BROKER_CHALLENGE_SCHEMA

    def __post_init__(self) -> None:
        if (
            not _HEX_64.fullmatch(self.nonce)
            or type(self.qpc_frequency) is not int
            or self.qpc_frequency <= 0
            or type(self.issued_tick) is not int
            or self.issued_tick < 0
            or type(self.expires_tick) is not int
            or self.expires_tick <= self.issued_tick
            or self.expires_tick > self.issued_tick + NONCE_LIFETIME_SECONDS * self.qpc_frequency
        ):
            raise _fail()

    def to_wire(self) -> dict[str, object]:
        return {"schema": self.schema, **asdict(self)}

    @classmethod
    def from_wire(cls, value: object) -> BrokerChallengeV1:
        item = _closed(value, {"schema", "nonce", "issued_tick", "expires_tick", "qpc_frequency"})
        if item["schema"] != cls.schema:
            raise _fail()
        return cls(item["nonce"], item["issued_tick"], item["expires_tick"], item["qpc_frequency"])


@dataclass(frozen=True, slots=True)
class BrokerRequestV1:
    correlation_id: str
    nonce: str
    policy_revision: str
    entry_id: str
    manifest_sha256: str
    request_sha256: str
    request: Mapping[str, object]
    deadline: QpcDeadlineV1
    schema: ClassVar[str] = BROKER_REQUEST_SCHEMA

    def __post_init__(self) -> None:
        if (
            not _HEX_32.fullmatch(self.correlation_id)
            or not _HEX_64.fullmatch(self.nonce)
            or not _HEX_64.fullmatch(self.policy_revision)
            or not _ENTRY.fullmatch(self.entry_id)
            or not _HEX_64.fullmatch(self.manifest_sha256)
            or not _HEX_64.fullmatch(self.request_sha256)
            or canonical_sha256(dict(self.request)) != self.request_sha256
        ):
            raise _fail()

    def to_wire(self) -> dict[str, object]:
        return {
            "schema": self.schema,
            "correlation_id": self.correlation_id,
            "nonce": self.nonce,
            "policy_revision": self.policy_revision,
            "entry_id": self.entry_id,
            "manifest_sha256": self.manifest_sha256,
            "request_sha256": self.request_sha256,
            "request": dict(self.request),
            "deadline": self.deadline.to_wire(),
        }

    @classmethod
    def from_wire(cls, value: object) -> BrokerRequestV1:
        item = _closed(
            value,
            {
                "schema",
                "correlation_id",
                "nonce",
                "policy_revision",
                "entry_id",
                "manifest_sha256",
                "request_sha256",
                "request",
                "deadline",
            },
        )
        if item["schema"] != cls.schema or not isinstance(item["request"], dict):
            raise _fail()
        return cls(
            item["correlation_id"],
            item["nonce"],
            item["policy_revision"],
            item["entry_id"],
            item["manifest_sha256"],
            item["request_sha256"],
            item["request"],
            QpcDeadlineV1.from_wire(item["deadline"]),
        )


@dataclass(frozen=True, slots=True)
class BrokerSettlementV1:
    worker_signaled: bool
    worker_exit_recorded: bool
    worker_reference_closed: bool
    job_active_zero: bool
    readers_joined: bool
    handles_closed: bool
    pipe_closed: bool
    workspace_settled: bool
    session_settled: bool
    source_unchanged: bool
    owned_remaining: int

    def __post_init__(self) -> None:
        for name, value in asdict(self).items():
            if name == "owned_remaining":
                if type(value) is not int or value < 0:
                    raise _fail()
            elif type(value) is not bool:
                raise _fail()

    @property
    def complete(self) -> bool:
        return (
            self.worker_signaled
            and self.worker_exit_recorded
            and self.worker_reference_closed
            and self.job_active_zero
            and self.readers_joined
            and self.handles_closed
            and self.pipe_closed
            and self.workspace_settled
            and self.session_settled
            and self.source_unchanged
            and self.owned_remaining == 0
        )

    @classmethod
    def from_wire(cls, value: object) -> BrokerSettlementV1:
        expected = set(cls.__dataclass_fields__)
        item = _closed(value, expected)
        return cls(**item)


@dataclass(frozen=True, slots=True)
class BrokerResponseV1:
    correlation_id: str
    policy_revision: str
    request_sha256: str
    deadline: QpcDeadlineV1
    ok: bool
    text: str | None
    failure_id: str | None
    settlement: BrokerSettlementV1
    schema: ClassVar[str] = BROKER_RESPONSE_SCHEMA

    def __post_init__(self) -> None:
        if (
            not _HEX_32.fullmatch(self.correlation_id)
            or not _HEX_64.fullmatch(self.policy_revision)
            or not _HEX_64.fullmatch(self.request_sha256)
            or type(self.ok) is not bool
            or self.ok != (self.text is not None and self.failure_id is None)
            or (self.text is not None and not isinstance(self.text, str))
            or (self.text is not None and len(self.text.encode("utf-8")) > PUBLIC_TEXT_MAX)
            or (self.failure_id is not None and self.failure_id not in SAFE_FAILURE_IDS)
        ):
            raise _fail()

    def to_wire(self) -> dict[str, object]:
        return {
            "schema": self.schema,
            "correlation_id": self.correlation_id,
            "policy_revision": self.policy_revision,
            "request_sha256": self.request_sha256,
            "deadline": self.deadline.to_wire(),
            "ok": self.ok,
            "text": self.text,
            "failure_id": self.failure_id,
            "settlement": asdict(self.settlement),
        }

    @classmethod
    def from_wire(cls, value: object) -> BrokerResponseV1:
        item = _closed(
            value,
            {
                "schema",
                "correlation_id",
                "policy_revision",
                "request_sha256",
                "deadline",
                "ok",
                "text",
                "failure_id",
                "settlement",
            },
        )
        if item["schema"] != cls.schema:
            raise _fail()
        return cls(
            item["correlation_id"],
            item["policy_revision"],
            item["request_sha256"],
            QpcDeadlineV1.from_wire(item["deadline"]),
            item["ok"],
            item["text"],
            item["failure_id"],
            BrokerSettlementV1.from_wire(item["settlement"]),
        )


def validate_response(request: BrokerRequestV1, response: BrokerResponseV1) -> BrokerResponseV1:
    if (
        response.correlation_id != request.correlation_id
        or response.policy_revision != request.policy_revision
        or response.request_sha256 != request.request_sha256
        or response.deadline != request.deadline
        or not response.settlement.complete
    ):
        raise _fail()
    return response
