"""Private closed broker-to-worker protocol for one saved-field invocation."""

from __future__ import annotations

import zlib
from collections.abc import Mapping
from dataclasses import asdict, dataclass
from typing import ClassVar

from .cst_saved_field_broker_protocol import (
    PUBLIC_TEXT_MAX,
    SAFE_FAILURE_IDS,
    QpcDeadlineV1,
    _canonical,
    _closed,
    _fail,
    canonical_sha256,
    decode_one_frame,
    encode_frame,
)

BROKER_WORKER_REQUEST_SCHEMA = "mcphub.cst.saved_field.broker_worker_request.v1"
BROKER_WORKER_RESPONSE_SCHEMA = "mcphub.cst.saved_field.broker_worker_response.v1"
BROKER_WORKER_PROTOCOL_V1 = "BrokerWorkerProtocolV1"
BROKER_WORKER_REQUEST_MAX = 131_072
BROKER_WORKER_RESPONSE_MAX = 1_114_112
WORKER_STARTUP_PROOF_SCHEMA = "mcphub.cst.saved_field.worker_startup_proof.v1"
WORKER_STARTUP_PROOF_MAX = 512
WORKER_PRE_MAIN_BOOTSTRAP_SCHEMA = "mcphub.cst.saved_field.worker_pre_main_bootstrap.v1"
WORKER_PRE_MAIN_RECEIPT_SCHEMA = "mcphub.cst.saved_field.worker_pre_main_receipt.v1"
WORKER_CAPABILITY_RECEIPT_SCHEMA = "mcphub.cst.saved_field.worker_capability_receipt.v1"
WORKER_PRE_MAIN_FRAME_MAX = 2_048
WORKER_HANDLE_ROLES = (
    "stdin",
    "stdout",
    "stderr",
    "source-root",
    "workspace-root",
)
JOB_PROCESS_MAX = 16


def _capability_identity(value: object) -> dict[str, object]:
    item = _closed(value, {"volume_serial", "file_id"})
    if (
        type(item["volume_serial"]) is not int
        or item["volume_serial"] < 0
        or not isinstance(item["file_id"], str)
        or not item["file_id"]
        or len(item["file_id"].encode("utf-8")) > 128
    ):
        raise _fail()
    return item


@dataclass(frozen=True, slots=True)
class WorkerPreMainBootstrapV1:
    """Broker-authored numeric locators and expected directory identities."""

    correlation_id: str
    deadline: QpcDeadlineV1
    source_root_locator: int
    workspace_root_locator: int
    source_access_mask: int
    workspace_access_mask: int
    source_root_identity: Mapping[str, object]
    workspace_root_identity: Mapping[str, object]
    inherited_handle_roles: tuple[str, ...] = WORKER_HANDLE_ROLES
    checksum: int | None = None
    schema: ClassVar[str] = WORKER_PRE_MAIN_BOOTSTRAP_SCHEMA

    def __post_init__(self) -> None:
        if (
            not isinstance(self.correlation_id, str)
            or len(self.correlation_id) != 32
            or any(ch not in "0123456789abcdef" for ch in self.correlation_id)
            or not isinstance(self.deadline, QpcDeadlineV1)
            or type(self.source_root_locator) is not int
            or type(self.workspace_root_locator) is not int
            or self.source_root_locator <= 0
            or self.workspace_root_locator <= 0
            or self.source_root_locator == self.workspace_root_locator
            or type(self.source_access_mask) is not int
            or type(self.workspace_access_mask) is not int
            or self.source_access_mask <= 0
            or self.workspace_access_mask <= 0
            or self.inherited_handle_roles != WORKER_HANDLE_ROLES
        ):
            raise _fail()
        object.__setattr__(self, "source_root_identity", _capability_identity(self.source_root_identity))
        object.__setattr__(
            self,
            "workspace_root_identity",
            _capability_identity(self.workspace_root_identity),
        )
        expected = zlib.crc32(_canonical(self._wire_without_checksum())) & 0xFFFFFFFF
        if self.checksum is None:
            object.__setattr__(self, "checksum", expected)
        elif type(self.checksum) is not int or self.checksum != expected:
            raise _fail()

    def _wire_without_checksum(self) -> dict[str, object]:
        return {
            "schema": self.schema,
            "correlation_id": self.correlation_id,
            "deadline": self.deadline.to_wire(),
            "source_root_locator": self.source_root_locator,
            "workspace_root_locator": self.workspace_root_locator,
            "source_access_mask": self.source_access_mask,
            "workspace_access_mask": self.workspace_access_mask,
            "source_root_identity": dict(self.source_root_identity),
            "workspace_root_identity": dict(self.workspace_root_identity),
            "inherited_handle_roles": list(self.inherited_handle_roles),
        }

    def to_wire(self) -> dict[str, object]:
        return {**self._wire_without_checksum(), "checksum": self.checksum}

    @classmethod
    def from_wire(cls, value: object) -> WorkerPreMainBootstrapV1:
        item = _closed(
            value,
            {
                "schema",
                "correlation_id",
                "deadline",
                "source_root_locator",
                "workspace_root_locator",
                "source_access_mask",
                "workspace_access_mask",
                "source_root_identity",
                "workspace_root_identity",
                "inherited_handle_roles",
                "checksum",
            },
        )
        if item.pop("schema") != cls.schema:
            raise _fail()
        if not isinstance(item["inherited_handle_roles"], list):
            raise _fail()
        item["inherited_handle_roles"] = tuple(item["inherited_handle_roles"])
        item["deadline"] = QpcDeadlineV1.from_wire(item["deadline"])
        return cls(**item)


@dataclass(frozen=True)
class WorkerPreMainReceiptV1:
    """Native-bootstrap observation; Python may validate but never author it."""

    correlation_id: str
    deadline: QpcDeadlineV1
    inherit_flags_cleared: bool
    capability_identities_verified: bool
    python_initialized: bool
    source_access_mask: int
    workspace_access_mask: int
    source_root_identity: Mapping[str, object]
    workspace_root_identity: Mapping[str, object]
    bootstrap_checksum: int
    inherited_handle_roles: tuple[str, ...] = WORKER_HANDLE_ROLES
    schema: ClassVar[str] = WORKER_PRE_MAIN_RECEIPT_SCHEMA

    def __post_init__(self) -> None:
        if (
            not isinstance(self.correlation_id, str)
            or len(self.correlation_id) != 32
            or not isinstance(self.deadline, QpcDeadlineV1)
            or type(self.inherit_flags_cleared) is not bool
            or type(self.capability_identities_verified) is not bool
            or self.python_initialized is not False
            or type(self.source_access_mask) is not int
            or type(self.workspace_access_mask) is not int
            or type(self.bootstrap_checksum) is not int
            or type(self.inherited_handle_roles) is not tuple
            or self.inherited_handle_roles != WORKER_HANDLE_ROLES
        ):
            raise _fail()
        object.__setattr__(self, "source_root_identity", _capability_identity(self.source_root_identity))
        object.__setattr__(
            self,
            "workspace_root_identity",
            _capability_identity(self.workspace_root_identity),
        )

    @property
    def complete(self) -> bool:
        return self.inherit_flags_cleared and self.capability_identities_verified

    def validates(self, bootstrap: WorkerPreMainBootstrapV1) -> bool:
        return (
            self.complete
            and self.correlation_id == bootstrap.correlation_id
            and self.deadline == bootstrap.deadline
            and self.source_access_mask == bootstrap.source_access_mask
            and self.workspace_access_mask == bootstrap.workspace_access_mask
            and self.bootstrap_checksum == bootstrap.checksum
            and dict(self.source_root_identity) == dict(bootstrap.source_root_identity)
            and dict(self.workspace_root_identity) == dict(bootstrap.workspace_root_identity)
        )

    def to_wire(self) -> dict[str, object]:
        return {
            "schema": self.schema,
            "correlation_id": self.correlation_id,
            "deadline": self.deadline.to_wire(),
            "inherit_flags_cleared": self.inherit_flags_cleared,
            "capability_identities_verified": self.capability_identities_verified,
            "python_initialized": self.python_initialized,
            "source_access_mask": self.source_access_mask,
            "workspace_access_mask": self.workspace_access_mask,
            "source_root_identity": dict(self.source_root_identity),
            "workspace_root_identity": dict(self.workspace_root_identity),
            "inherited_handle_roles": list(self.inherited_handle_roles),
            "bootstrap_checksum": self.bootstrap_checksum,
        }

    @classmethod
    def from_wire(cls, value: object) -> WorkerPreMainReceiptV1:
        item = _closed(
            value,
            {
                "schema",
                "correlation_id",
                "deadline",
                "inherit_flags_cleared",
                "capability_identities_verified",
                "python_initialized",
                "source_access_mask",
                "workspace_access_mask",
                "source_root_identity",
                "workspace_root_identity",
                "inherited_handle_roles",
                "bootstrap_checksum",
            },
        )
        if item.pop("schema") != cls.schema or not isinstance(item["inherited_handle_roles"], list):
            raise _fail()
        item["inherited_handle_roles"] = tuple(item["inherited_handle_roles"])
        item["deadline"] = QpcDeadlineV1.from_wire(item["deadline"])
        return cls(**item)


def encode_pre_main_bootstrap_frame(value: WorkerPreMainBootstrapV1) -> bytes:
    return encode_frame(value.to_wire(), maximum=WORKER_PRE_MAIN_FRAME_MAX)


def decode_pre_main_bootstrap_frame(source) -> WorkerPreMainBootstrapV1:
    return WorkerPreMainBootstrapV1.from_wire(decode_one_frame(source, maximum=WORKER_PRE_MAIN_FRAME_MAX))


def encode_pre_main_receipt_frame(value: WorkerPreMainReceiptV1) -> bytes:
    return encode_frame(value.to_wire(), maximum=WORKER_PRE_MAIN_FRAME_MAX)


def decode_pre_main_receipt_frame(source) -> WorkerPreMainReceiptV1:
    return WorkerPreMainReceiptV1.from_wire(decode_one_frame(source, maximum=WORKER_PRE_MAIN_FRAME_MAX))


@dataclass(frozen=True, slots=True)
class WorkerStartupProofV1:
    exact_job: bool
    exactly_three_inherited_std_handles: bool
    no_console: bool
    breakaway_denied: bool
    breakaway_created: bool
    escaped_process_settled: bool

    def __post_init__(self) -> None:
        if any(type(value) is not bool for value in asdict(self).values()):
            raise _fail()

    @property
    def complete(self) -> bool:
        return (
            self.exact_job
            and self.exactly_three_inherited_std_handles
            and self.no_console
            and self.breakaway_denied
            and not self.breakaway_created
            and self.escaped_process_settled
        )

    def to_wire(self) -> dict[str, object]:
        return {"schema": WORKER_STARTUP_PROOF_SCHEMA, **asdict(self)}

    @classmethod
    def from_wire(cls, value: object) -> WorkerStartupProofV1:
        item = _closed(value, {"schema", *cls.__dataclass_fields__})
        if item.pop("schema") != WORKER_STARTUP_PROOF_SCHEMA:
            raise _fail()
        return cls(**item)


def encode_startup_proof_frame(proof: WorkerStartupProofV1) -> bytes:
    return encode_frame(proof.to_wire(), maximum=WORKER_STARTUP_PROOF_MAX)


def decode_startup_proof_frame(source) -> WorkerStartupProofV1:
    return WorkerStartupProofV1.from_wire(decode_one_frame(source, maximum=WORKER_STARTUP_PROOF_MAX))


@dataclass(frozen=True, slots=True)
class WorkerSettlementV1:
    source_handles_closed: bool
    vendor_path_lease_settled: bool
    session_settled: bool
    workspace_settled: bool
    source_unchanged: bool
    output_sealed: bool
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
            self.source_handles_closed
            and self.vendor_path_lease_settled
            and self.session_settled
            and self.workspace_settled
            and self.source_unchanged
            and self.output_sealed
            and self.owned_remaining == 0
        )

    @classmethod
    def from_wire(cls, value: object) -> WorkerSettlementV1:
        item = _closed(value, set(cls.__dataclass_fields__))
        return cls(**item)


@dataclass(frozen=True, slots=True)
class WorkerCapabilityReceiptV1:
    """Worker-local capability-use/close observations; no broker-owned facts."""

    correlation_id: str
    descendant_opens_handle_relative: bool
    source_capability_closed: bool
    workspace_capability_closed: bool
    new_authority_opened: bool
    schema: ClassVar[str] = WORKER_CAPABILITY_RECEIPT_SCHEMA

    def __post_init__(self) -> None:
        if (
            not isinstance(self.correlation_id, str)
            or len(self.correlation_id) != 32
            or any(character not in "0123456789abcdef" for character in self.correlation_id)
            or any(
                type(value) is not bool
                for value in (
                    self.descendant_opens_handle_relative,
                    self.source_capability_closed,
                    self.workspace_capability_closed,
                    self.new_authority_opened,
                )
            )
        ):
            raise _fail()

    @property
    def complete(self) -> bool:
        return (
            self.descendant_opens_handle_relative
            and self.source_capability_closed
            and self.workspace_capability_closed
            and not self.new_authority_opened
        )

    def to_wire(self) -> dict[str, object]:
        return {"schema": self.schema, **asdict(self)}

    @classmethod
    def from_wire(cls, value: object) -> WorkerCapabilityReceiptV1:
        item = _closed(value, {"schema", *cls.__dataclass_fields__})
        if item.pop("schema") != cls.schema:
            raise _fail()
        return cls(**item)


@dataclass(frozen=True, slots=True)
class BrokerWorkerRequestV1:
    correlation_id: str
    policy_revision: str
    entry_id: str
    manifest_sha256: str
    request_sha256: str
    request: Mapping[str, object]
    deadline: QpcDeadlineV1
    schema: ClassVar[str] = BROKER_WORKER_REQUEST_SCHEMA

    def __post_init__(self) -> None:
        from .cst_saved_field_broker_protocol import BrokerRequestV1

        BrokerRequestV1(
            self.correlation_id,
            "0" * 64,
            self.policy_revision,
            self.entry_id,
            self.manifest_sha256,
            self.request_sha256,
            self.request,
            self.deadline,
        )

    def to_wire(self) -> dict[str, object]:
        return {
            "schema": self.schema,
            "correlation_id": self.correlation_id,
            "policy_revision": self.policy_revision,
            "entry_id": self.entry_id,
            "manifest_sha256": self.manifest_sha256,
            "request_sha256": self.request_sha256,
            "request": dict(self.request),
            "deadline": self.deadline.to_wire(),
        }

    @classmethod
    def from_wire(cls, value: object) -> BrokerWorkerRequestV1:
        item = _closed(
            value,
            {
                "schema",
                "correlation_id",
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
            item["policy_revision"],
            item["entry_id"],
            item["manifest_sha256"],
            item["request_sha256"],
            item["request"],
            QpcDeadlineV1.from_wire(item["deadline"]),
        )


@dataclass(frozen=True, slots=True)
class BrokerWorkerResponseV1:
    correlation_id: str
    policy_revision: str
    request_sha256: str
    deadline: QpcDeadlineV1
    ok: bool
    text: str | None
    failure_id: str | None
    settlement: WorkerSettlementV1
    capability_receipt: WorkerCapabilityReceiptV1
    schema: ClassVar[str] = BROKER_WORKER_RESPONSE_SCHEMA

    def __post_init__(self) -> None:
        probe = {
            "correlation_id": self.correlation_id,
            "policy_revision": self.policy_revision,
            "request_sha256": self.request_sha256,
        }
        if (
            any(len(value) not in {32, 64} for value in probe.values())
            or type(self.ok) is not bool
            or self.ok != (self.text is not None and self.failure_id is None)
            or (self.text is not None and not isinstance(self.text, str))
            or (self.text is not None and len(self.text.encode("utf-8")) > PUBLIC_TEXT_MAX)
            or (self.failure_id is not None and self.failure_id not in SAFE_FAILURE_IDS)
            or not self.capability_receipt.complete
            or self.capability_receipt.correlation_id != self.correlation_id
            or (self.ok and not self.settlement.complete)
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
            "capability_receipt": self.capability_receipt.to_wire(),
        }

    @classmethod
    def from_wire(cls, value: object) -> BrokerWorkerResponseV1:
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
                "capability_receipt",
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
            WorkerSettlementV1.from_wire(item["settlement"]),
            WorkerCapabilityReceiptV1.from_wire(item["capability_receipt"]),
        )


def validate_worker_response(
    request: BrokerWorkerRequestV1, response: BrokerWorkerResponseV1
) -> BrokerWorkerResponseV1:
    if (
        response.correlation_id != request.correlation_id
        or response.policy_revision != request.policy_revision
        or response.request_sha256 != request.request_sha256
        or response.deadline != request.deadline
        or canonical_sha256(dict(request.request)) != request.request_sha256
    ):
        raise _fail()
    return response
