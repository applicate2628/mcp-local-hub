"""Fixed broker-owned worker entry point for one saved-field invocation."""

from __future__ import annotations

import json
from collections.abc import Callable
from dataclasses import dataclass
from typing import BinaryIO, Protocol, cast

from pydantic import ValidationError

from .cst_saved_field import SavedFieldFailure, SavedFieldRequestV1, sample_saved_field
from .cst_saved_field_broker_protocol import decode_one_frame, encode_frame
from .cst_saved_field_broker_worker_protocol import (
    BROKER_WORKER_REQUEST_MAX,
    BROKER_WORKER_RESPONSE_MAX,
    BrokerWorkerRequestV1,
    BrokerWorkerResponseV1,
    WorkerCapabilityReceiptV1,
    WorkerPreMainBootstrapV1,
    WorkerPreMainReceiptV1,
    WorkerSettlementV1,
)

WorkerApplication = Callable[[BrokerWorkerRequestV1], BrokerWorkerResponseV1]


class WorkerTransactionPort(Protocol):
    def execute(
        self, request: BrokerWorkerRequestV1
    ) -> tuple[str | None, str | None, WorkerSettlementV1, WorkerCapabilityReceiptV1]: ...


class SettledSavedFieldApplicationPort(Protocol):
    """Provisioned application port with an authoritative worker-local receipt."""

    def worker_settlement(self) -> WorkerSettlementV1: ...

    def worker_capability_receipt(self, correlation_id: str) -> WorkerCapabilityReceiptV1: ...


class SavedFieldWorkerTransactionV1:
    """Own one application call and derive its worker response from settled state."""

    def __init__(self, *, project_bundle: str, application_port: object) -> None:
        if not isinstance(project_bundle, str) or not project_bundle or application_port is None:
            raise RuntimeError("cst_saved_field.cst_unavailable")
        self._project_bundle = project_bundle
        self._application_port = application_port

    def execute(
        self, request: BrokerWorkerRequestV1
    ) -> tuple[str | None, str | None, WorkerSettlementV1, WorkerCapabilityReceiptV1]:
        body = {"project_bundle": self._project_bundle, **dict(request.request)}
        text: str | None = None
        failure_id: str | None = None
        try:
            application_request = SavedFieldRequestV1.model_validate(body)
            value = sample_saved_field(application_request, cast(object, self._application_port))
            text = json.dumps(
                value,
                ensure_ascii=False,
                allow_nan=False,
                sort_keys=True,
                separators=(",", ":"),
            )
        except ValidationError:
            failure_id = "cst_saved_field.invalid_request"
        except SavedFieldFailure as exc:
            failure_id = exc.failure_id
        except Exception:
            failure_id = "cst_saved_field.activation_failed"
        settlement_owner = cast(SettledSavedFieldApplicationPort, self._application_port)
        settlement = settlement_owner.worker_settlement()
        if not isinstance(settlement, WorkerSettlementV1) or not settlement.complete:
            raise RuntimeError("cst_saved_field.containment_settle_failed")
        capability_receipt = settlement_owner.worker_capability_receipt(request.correlation_id)
        if not isinstance(capability_receipt, WorkerCapabilityReceiptV1) or not capability_receipt.complete:
            raise RuntimeError("cst_saved_field.containment_settle_failed")
        return text, failure_id, settlement, capability_receipt


class BrokerWorkerApplication:
    """Authorize one broker request and execute its single settlement-owning transaction."""

    def __init__(
        self,
        *,
        authorize: Callable[[BrokerWorkerRequestV1], None],
        transaction: WorkerTransactionPort,
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
    ) -> None:
        self._authorize = authorize
        self._transaction = transaction
        self._qpc_frequency = qpc_frequency
        self._qpc_counter = qpc_counter

    def _check_deadline(self, request: BrokerWorkerRequestV1) -> None:
        if (
            self._qpc_frequency() != request.deadline.qpc_frequency
            or self._qpc_counter() >= request.deadline.deadline_tick
        ):
            raise RuntimeError("cst_saved_field.deadline_exceeded")

    def __call__(self, request: BrokerWorkerRequestV1) -> BrokerWorkerResponseV1:
        self._check_deadline(request)
        self._authorize(request)
        self._check_deadline(request)
        text, failure_id, settlement, capability_receipt = self._transaction.execute(request)
        self._check_deadline(request)
        if not settlement.complete:
            raise RuntimeError("cst_saved_field.containment_settle_failed")
        ok = text is not None and failure_id is None
        return BrokerWorkerResponseV1(
            request.correlation_id,
            request.policy_revision,
            request.request_sha256,
            request.deadline,
            ok,
            text,
            failure_id,
            settlement,
            capability_receipt,
        )


def run_worker(
    source: BinaryIO,
    destination: BinaryIO,
    application: WorkerApplication,
    *,
    bootstrap: WorkerPreMainBootstrapV1,
    pre_main_receipt: WorkerPreMainReceiptV1,
) -> int:
    if not isinstance(bootstrap, WorkerPreMainBootstrapV1) or not isinstance(
        pre_main_receipt, WorkerPreMainReceiptV1
    ):
        return 78
    if not pre_main_receipt.validates(bootstrap):
        return 78
    request = BrokerWorkerRequestV1.from_wire(decode_one_frame(source, maximum=BROKER_WORKER_REQUEST_MAX))
    response = application(request)
    destination.write(encode_frame(response.to_wire(), maximum=BROKER_WORKER_RESPONSE_MAX))
    destination.flush()
    return 0


@dataclass(frozen=True, slots=True)
class WorkerCompositionV1:
    """Broker-supplied owner dependencies for one contained worker."""

    authorize: Callable[[BrokerWorkerRequestV1], None]
    project_bundle: str
    application_port: object
    qpc_frequency: Callable[[], int]
    qpc_counter: Callable[[], int]
    source: BinaryIO
    destination: BinaryIO
    bootstrap: WorkerPreMainBootstrapV1
    pre_main_receipt: WorkerPreMainReceiptV1


def compose_application(composition: WorkerCompositionV1) -> BrokerWorkerApplication:
    if not isinstance(composition, WorkerCompositionV1):
        raise RuntimeError("cst_saved_field.cst_unavailable")
    return BrokerWorkerApplication(
        authorize=composition.authorize,
        transaction=SavedFieldWorkerTransactionV1(
            project_bundle=composition.project_bundle,
            application_port=composition.application_port,
        ),
        qpc_frequency=composition.qpc_frequency,
        qpc_counter=composition.qpc_counter,
    )


def compose_default_off_runtime() -> WorkerCompositionV1 | None:
    return None


def _run_composed_worker(composition: WorkerCompositionV1) -> int:
    return run_worker(
        composition.source,
        composition.destination,
        compose_application(composition),
        bootstrap=composition.bootstrap,
        pre_main_receipt=composition.pre_main_receipt,
    )


def main() -> int:
    composition = compose_default_off_runtime()
    if composition is None:
        return 78
    return _run_composed_worker(composition)


if __name__ == "__main__":
    raise SystemExit(main())
