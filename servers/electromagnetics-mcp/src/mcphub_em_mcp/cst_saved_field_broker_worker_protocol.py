"""Private closed broker-to-worker protocol for one saved-field invocation."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import asdict, dataclass
from typing import ClassVar

from .cst_saved_field_broker_protocol import (
    PUBLIC_TEXT_MAX,
    SAFE_FAILURE_IDS,
    QpcDeadlineV1,
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
JOB_PROCESS_MAX = 16


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
