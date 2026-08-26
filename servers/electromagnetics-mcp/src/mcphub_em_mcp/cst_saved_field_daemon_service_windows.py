"""SCM-daemon owner for saved-field admission and frontend settlement."""

from __future__ import annotations

import re
import threading
from collections.abc import Callable
from dataclasses import dataclass
from typing import Literal, Protocol

from .cst_saved_field_broker_client_windows import (
    AdmissionLease,
    BrokerTransport,
    SamplerAdmissionGate,
    WindowsBrokerClient,
)
from .cst_saved_field_broker_protocol import BrokerProtocolFailure, QpcDeadlineV1
from .cst_saved_field_endpoints import FRONTEND_ENDPOINT_V1
from .cst_saved_field_frontend_protocol import (
    FRONTEND_SAFE_FAILURE_IDS,
    DaemonResponseReceiptV1,
    FrontendDaemonRequestV1,
    FrontendDaemonResultV1,
)
from .cst_saved_field_hub_enrollment_windows import HubEnrollmentServerV1
from .cst_saved_field_policy import (
    AuthorityEntry,
    AuthoritySnapshot,
    PolicyPlatform,
    load_authority_snapshot,
)
from .cst_saved_field_port import AbsoluteInvocationBudget

_HEX32 = re.compile(r"[0-9a-f]{32}\Z")
ChallengeState = Literal["ISSUED", "CONSUMED", "CANCELLED"]
CST_SAVED_FIELD_PRODUCTION_TOPOLOGY_V1 = {
    "endpoints": ("enrollment", "frontend", "broker"),
    "schemas": ("hub-enrollment-v1", "frontend-v1", "broker-v1", "broker-worker-v1"),
}


class WindowsNamedPipeDaemonTransport:
    """SCM-owned frontend listener marker for the fixed policy endpoint."""

    endpoint = FRONTEND_ENDPOINT_V1


class DaemonServiceFailure(RuntimeError):
    def __init__(self, failure_id: str) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id


class ResponseWriter(Protocol):
    def __call__(self, result: FrontendDaemonResultV1) -> DaemonResponseReceiptV1: ...


@dataclass(frozen=True, slots=True)
class AdmissionSettlementV1:
    correlation_id: str
    policy_revision: str
    generation: int
    disposition: Literal["released", "quarantined"]


class WindowsCstDaemonService:
    """Process-lifetime admission owner composed below the fixed SCM listener."""

    def __init__(
        self,
        *,
        snapshot: AuthoritySnapshot,
        enrollment: HubEnrollmentServerV1,
        broker_transport: BrokerTransport,
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
        broker_correlation: Callable[[], str],
        random_bytes: Callable[[int], bytes],
        event_sink: Callable[[AdmissionSettlementV1], object],
    ) -> None:
        if not isinstance(snapshot, AuthoritySnapshot) or not snapshot.entries:
            raise DaemonServiceFailure("cst_saved_field.policy_disabled")
        self.snapshot = snapshot
        self.enrollment = enrollment
        self._qpc_frequency = qpc_frequency
        self._qpc_counter = qpc_counter
        self._random_bytes = random_bytes
        self._event_sink = event_sink
        self._gate = SamplerAdmissionGate(snapshot.revision)
        self._broker = WindowsBrokerClient(
            transport=broker_transport,
            qpc_frequency=qpc_frequency,
            qpc_counter=qpc_counter,
            correlation=broker_correlation,
            gate=self._gate,
        )
        self._lock = threading.RLock()
        self._challenges: dict[str, str] = {}
        self._challenge_history: dict[str, ChallengeState] = {}
        self._active: tuple[str, AdmissionLease] | None = None
        self._quarantined = False

    @classmethod
    def from_policy(
        cls,
        *,
        raw_policy_path: str | None,
        policy_platform: PolicyPlatform,
        enrollment: HubEnrollmentServerV1,
        broker_transport: BrokerTransport,
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
        broker_correlation: Callable[[], str],
        random_bytes: Callable[[int], bytes],
        event_sink: Callable[[AdmissionSettlementV1], object],
    ) -> WindowsCstDaemonService:
        loaded = load_authority_snapshot(raw_policy_path, policy_platform)
        if not loaded.enabled or loaded.snapshot is None:
            raise DaemonServiceFailure(loaded.failure_id or "cst_saved_field.policy_invalid")
        return cls(
            snapshot=loaded.snapshot,
            enrollment=enrollment,
            broker_transport=broker_transport,
            qpc_frequency=qpc_frequency,
            qpc_counter=qpc_counter,
            broker_correlation=broker_correlation,
            random_bytes=random_bytes,
            event_sink=event_sink,
        )

    @property
    def quarantined(self) -> bool:
        with self._lock:
            return self._quarantined

    def challenge_state(self, correlation: str) -> ChallengeState | None:
        with self._lock:
            return self._challenge_history.get(correlation)

    def issue_challenge(self, correlation: str) -> str:
        with self._lock:
            if self._quarantined:
                raise DaemonServiceFailure("cst_saved_field.containment_quarantined")
            if _HEX32.fullmatch(correlation) is None or self._challenges:
                raise DaemonServiceFailure("cst_saved_field.frontend_unavailable")
            raw = self._random_bytes(32)
            if not isinstance(raw, bytes) or len(raw) != 32:
                raise DaemonServiceFailure("cst_saved_field.frontend_unavailable")
            nonce = raw.hex()
            self._challenges[correlation] = nonce
            self._challenge_history[correlation] = "ISSUED"
            return nonce

    def _consume_challenge(self, request: FrontendDaemonRequestV1) -> None:
        with self._lock:
            expected = self._challenges.pop(request.correlation_id, None)
            if expected != request.challenge_nonce:
                self._challenge_history[request.correlation_id] = "CANCELLED"
                raise DaemonServiceFailure("cst_saved_field.frontend_protocol_invalid")
            self._challenge_history[request.correlation_id] = "CONSUMED"

    def _cancel_challenge(self, correlation: str) -> None:
        with self._lock:
            self._challenges.pop(correlation, None)
            if self._challenge_history.get(correlation) == "ISSUED":
                self._challenge_history[correlation] = "CANCELLED"

    def _resolve_entry(self, entry_id: str) -> AuthorityEntry:
        matches = [entry for entry in self.snapshot.entries if entry.entry_id == entry_id]
        if len(matches) != 1:
            raise DaemonServiceFailure("cst_saved_field.not_authorized")
        return matches[0]

    def _budget(self) -> QpcDeadlineV1:
        frequency = self._qpc_frequency()
        admitted = self._qpc_counter()
        return QpcDeadlineV1(frequency, admitted, admitted + 60 * frequency)

    def _settle(self, correlation: str, lease: AdmissionLease, *, quarantine: bool) -> None:
        with self._lock:
            if self._active != (correlation, lease):
                return
            if quarantine:
                self._quarantined = True
                self._gate.quarantine_and_release(lease)
                disposition = "quarantined"
            else:
                self._gate.release(lease)
                disposition = "released"
            self._active = None
            self._event_sink(
                AdmissionSettlementV1(
                    correlation,
                    self.snapshot.revision,
                    lease.generation,
                    disposition,
                )
            )

    def exchange(
        self,
        request: FrontendDaemonRequestV1,
        *,
        exact_capability_eof: bool,
        response_writer: ResponseWriter,
    ) -> tuple[FrontendDaemonResultV1, DaemonResponseReceiptV1]:
        if not isinstance(request, FrontendDaemonRequestV1):
            raise DaemonServiceFailure("cst_saved_field.frontend_protocol_invalid")
        lease: AdmissionLease | None = None
        result: FrontendDaemonResultV1 | None = None
        try:
            self._consume_challenge(request)
            try:
                capability = bytes.fromhex(request.launch_capability)
            except ValueError as exc:
                raise DaemonServiceFailure("cst_saved_field.frontend_protocol_invalid") from exc
            if not self.enrollment.consume_frontend(
                request.correlation_id,
                capability,
                exact_32_and_eof=exact_capability_eof,
                frontend_challenge_consumed=True,
            ):
                raise DaemonServiceFailure("cst_saved_field.not_authorized")
            entry = self._resolve_entry(request.entry_id)
            lease = self._gate.acquire_and_seal(self.snapshot.revision, wait_seconds=1.0)
            with self._lock:
                self._active = (request.correlation_id, lease)
            budget = self._budget()
            response = self._broker.invoke_admitted(
                lease=lease,
                policy_revision=self.snapshot.revision,
                entry_id=entry.entry_id,
                manifest_sha256=entry.bundle_manifest_sha256,
                request=request.request,
                deadline=budget,
            )
            result = FrontendDaemonResultV1(
                request.correlation_id,
                entry.entry_id,
                request.request_sha256,
                AbsoluteInvocationBudget(budget.qpc_frequency, budget.admitted_tick, budget.deadline_tick),
                response.ok,
                response.text,
                response.failure_id,
            )
            receipt = response_writer(result)
            if (
                not isinstance(receipt, DaemonResponseReceiptV1)
                or receipt.correlation_id != request.correlation_id
                or not receipt.complete
            ):
                raise DaemonServiceFailure("cst_saved_field.containment_settle_failed")
            with self._lock:
                if self._quarantined or self._active != (request.correlation_id, lease):
                    raise DaemonServiceFailure("cst_saved_field.containment_settle_failed")
            self._settle(request.correlation_id, lease, quarantine=False)
            return result, receipt
        except BaseException as exc:
            self._cancel_challenge(request.correlation_id)
            if lease is not None:
                self._settle(request.correlation_id, lease, quarantine=True)
            if isinstance(exc, DaemonServiceFailure):
                raise
            if isinstance(exc, BrokerProtocolFailure):
                failure_id = str(exc)
                if failure_id not in FRONTEND_SAFE_FAILURE_IDS:
                    failure_id = "cst_saved_field.internal_error"
                raise DaemonServiceFailure(failure_id) from exc
            raise DaemonServiceFailure("cst_saved_field.internal_error") from exc

    def shutdown(self) -> None:
        with self._lock:
            active = self._active
            for correlation in tuple(self._challenges):
                self._cancel_challenge(correlation)
        if active is not None:
            self._settle(active[0], active[1], quarantine=True)
        self.enrollment.shutdown()


def run_service(service: WindowsCstDaemonService, serve: Callable[[WindowsCstDaemonService], int]) -> int:
    """Run the provisioning-owned SCM listener around one daemon owner."""

    try:
        return serve(service)
    finally:
        service.shutdown()


@dataclass(frozen=True, slots=True)
class DaemonServiceCompositionV1:
    """Provisioning-supplied dependencies for the fixed daemon service root."""

    raw_policy_path: str | None
    policy_platform: PolicyPlatform
    enrollment: HubEnrollmentServerV1
    broker_transport: BrokerTransport
    qpc_frequency: Callable[[], int]
    qpc_counter: Callable[[], int]
    broker_correlation: Callable[[], str]
    random_bytes: Callable[[int], bytes]
    event_sink: Callable[[AdmissionSettlementV1], object]
    serve: Callable[[WindowsCstDaemonService], int]


def compose_service(composition: DaemonServiceCompositionV1) -> WindowsCstDaemonService:
    if not isinstance(composition, DaemonServiceCompositionV1):
        raise DaemonServiceFailure("cst_saved_field.daemon_unavailable")
    return WindowsCstDaemonService.from_policy(
        raw_policy_path=composition.raw_policy_path,
        policy_platform=composition.policy_platform,
        enrollment=composition.enrollment,
        broker_transport=composition.broker_transport,
        qpc_frequency=composition.qpc_frequency,
        qpc_counter=composition.qpc_counter,
        broker_correlation=composition.broker_correlation,
        random_bytes=composition.random_bytes,
        event_sink=composition.event_sink,
    )


def compose_default_off_runtime() -> DaemonServiceCompositionV1 | None:
    """Return no service unless the later provision owner supplies closed runtime state."""

    return None


def _run_composed_service(composition: DaemonServiceCompositionV1) -> int:
    return run_service(compose_service(composition), composition.serve)


def main() -> int:
    composition = compose_default_off_runtime()
    if composition is None:
        raise DaemonServiceFailure("cst_saved_field.daemon_unavailable")
    return _run_composed_service(composition)
