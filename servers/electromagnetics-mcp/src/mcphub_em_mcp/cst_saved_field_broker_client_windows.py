"""Credential-free daemon-side client for the fixed saved-field broker."""

from __future__ import annotations

import os
import threading
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Protocol

from .cst_saved_field_broker_protocol import (
    BrokerChallengeV1,
    BrokerProtocolFailure,
    BrokerRequestV1,
    BrokerResponseV1,
    QpcDeadlineV1,
    canonical_sha256,
    validate_response,
)


@dataclass(frozen=True, slots=True)
class BrokerStartupProofV1:
    service_running: bool
    scm_pid_token_bound: bool
    service_sid_bound: bool
    pinned_image_bound: bool
    pipe_descriptor_bound: bool

    @property
    def complete(self) -> bool:
        return all(
            (
                self.service_running,
                self.scm_pid_token_bound,
                self.service_sid_bound,
                self.pinned_image_bound,
                self.pipe_descriptor_bound,
            )
        )


@dataclass(frozen=True, slots=True)
class BrokerCancelReceiptV1:
    worker_settled: bool
    job_active_zero: bool
    pipe_closed: bool
    owned_remaining: int

    @property
    def complete(self) -> bool:
        return (
            type(self.worker_settled) is bool
            and type(self.job_active_zero) is bool
            and type(self.pipe_closed) is bool
            and type(self.owned_remaining) is int
            and self.worker_settled
            and self.job_active_zero
            and self.pipe_closed
            and self.owned_remaining == 0
        )


class AdmissionFailure(BrokerProtocolFailure):
    pass


@dataclass(frozen=True, slots=True)
class AdmissionLease:
    _gate: SamplerAdmissionGate
    revision: str
    generation: int
    token: object

    def authorize_start(self) -> None:
        self._gate._authorize_start(self)


class SamplerAdmissionGate:
    """One-active/one-waiter daemon gate with restart-only quarantine recovery."""

    def __init__(self, revision: str) -> None:
        self._revision = revision
        self._condition = threading.Condition()
        self._active: object | None = None
        self._waiters = 0
        self._generation = 0
        self._quarantined = False

    @property
    def active_count(self) -> int:
        with self._condition:
            return int(self._active is not None)

    @property
    def waiter_count(self) -> int:
        with self._condition:
            return self._waiters

    def acquire_and_seal(self, revision: str, *, wait_seconds: float) -> AdmissionLease:
        with self._condition:
            self._check_admission(revision)
            if self._active is None:
                return self._grant(revision)
            if self._waiters != 0 or wait_seconds <= 0.0:
                raise AdmissionFailure("cst_saved_field.resource_busy")
            self._waiters = 1
            deadline = time.monotonic() + min(wait_seconds, 1.0)
            try:
                while self._active is not None and not self._quarantined:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0.0:
                        raise AdmissionFailure("cst_saved_field.resource_busy")
                    self._condition.wait(remaining)
                self._check_admission(revision)
                return self._grant(revision)
            finally:
                self._waiters = 0

    def _check_admission(self, revision: str) -> None:
        if self._quarantined:
            raise AdmissionFailure("cst_saved_field.containment_quarantined")
        if revision != self._revision:
            raise AdmissionFailure("cst_saved_field.policy_revision_changed")

    def _grant(self, revision: str) -> AdmissionLease:
        token = object()
        self._active = token
        return AdmissionLease(self, revision, self._generation, token)

    def _authorize_start(self, lease: AdmissionLease) -> None:
        with self._condition:
            self._check_admission(lease.revision)
            if (
                lease._gate is not self
                or lease.generation != self._generation
                or self._active is not lease.token
            ):
                raise AdmissionFailure("cst_saved_field.policy_revision_changed")

    def release(self, lease: AdmissionLease) -> None:
        with self._condition:
            if self._active is lease.token:
                self._active = None
                self._condition.notify_all()

    def quarantine_and_release(self, lease: AdmissionLease) -> None:
        with self._condition:
            self._quarantined = True
            self._generation += 1
            if self._active is lease.token:
                self._active = None
            self._condition.notify_all()

    def _test_only_advance_generation(self) -> None:
        with self._condition:
            self._generation += 1


class BrokerTransport(Protocol):
    def startup_proof(self) -> BrokerStartupProofV1: ...

    def challenge(self, deadline: QpcDeadlineV1) -> BrokerChallengeV1: ...

    def exchange(self, request: BrokerRequestV1) -> BrokerResponseV1: ...

    def cancel_and_settle(self, correlation_id: str, deadline: QpcDeadlineV1) -> BrokerCancelReceiptV1: ...


class UnavailableBrokerTransport:
    """Fail-closed composition default until the fixed SCM broker proves ready."""

    def startup_proof(self) -> BrokerStartupProofV1:
        return BrokerStartupProofV1(False, False, False, False, False)

    def challenge(self, deadline: QpcDeadlineV1) -> BrokerChallengeV1:
        del deadline
        raise BrokerProtocolFailure("cst_saved_field.broker_unavailable")

    def exchange(self, request: BrokerRequestV1) -> BrokerResponseV1:
        del request
        raise BrokerProtocolFailure("cst_saved_field.broker_unavailable")

    def cancel_and_settle(self, correlation_id: str, deadline: QpcDeadlineV1) -> BrokerCancelReceiptV1:
        del correlation_id, deadline
        return BrokerCancelReceiptV1(True, True, True, 0)


class WindowsBrokerClient:
    def __init__(
        self,
        *,
        transport: BrokerTransport,
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
        correlation: Callable[[], str],
        gate: SamplerAdmissionGate | None = None,
    ) -> None:
        self._transport = transport
        self._qpc_frequency = qpc_frequency
        self._qpc_counter = qpc_counter
        self._correlation = correlation
        self._gate = gate

    def bind_revision(self, revision: str) -> None:
        if self._gate is None:
            self._gate = SamplerAdmissionGate(revision)
        elif self._gate._revision != revision:
            raise AdmissionFailure("cst_saved_field.policy_revision_changed")

    def startup_ready(self) -> bool:
        return self._transport.startup_proof().complete

    def invoke(
        self,
        *,
        policy_revision: str,
        entry_id: str,
        manifest_sha256: str,
        request: Mapping[str, object],
    ) -> BrokerResponseV1:
        self.bind_revision(policy_revision)
        assert self._gate is not None
        lease = self._gate.acquire_and_seal(policy_revision, wait_seconds=1.0)
        frequency = self._qpc_frequency()
        admitted = self._qpc_counter()
        deadline = QpcDeadlineV1(frequency, admitted, admitted + 60 * frequency)
        try:
            return self.invoke_admitted(
                lease=lease,
                policy_revision=policy_revision,
                entry_id=entry_id,
                manifest_sha256=manifest_sha256,
                request=request,
                deadline=deadline,
            )
        finally:
            self._gate.release(lease)

    def invoke_admitted(
        self,
        *,
        lease: AdmissionLease,
        policy_revision: str,
        entry_id: str,
        manifest_sha256: str,
        request: Mapping[str, object],
        deadline: QpcDeadlineV1,
    ) -> BrokerResponseV1:
        """Exchange under the daemon-owned lease without releasing it."""

        self.bind_revision(policy_revision)
        assert self._gate is not None
        if lease._gate is not self._gate or lease.revision != policy_revision:
            raise AdmissionFailure("cst_saved_field.policy_revision_changed")
        lease.authorize_start()
        if not self.startup_ready():
            raise BrokerProtocolFailure("cst_saved_field.broker_unavailable")
        challenge = self._transport.challenge(deadline)
        if (
            challenge.qpc_frequency != deadline.qpc_frequency
            or challenge.issued_tick < deadline.admitted_tick
            or challenge.issued_tick >= deadline.deadline_tick
            or challenge.expires_tick > deadline.deadline_tick
        ):
            raise BrokerProtocolFailure("cst_saved_field.broker_protocol_invalid")
        value = dict(request)
        serialized = repr(value).casefold()
        if any(token in serialized for token in ("project_bundle", "root", "path", "handle", "bytes")):
            raise BrokerProtocolFailure("cst_saved_field.broker_protocol_invalid")
        broker_request = BrokerRequestV1(
            self._correlation(),
            challenge.nonce,
            policy_revision,
            entry_id,
            manifest_sha256,
            canonical_sha256(value),
            value,
            deadline,
        )
        try:
            response = self._transport.exchange(broker_request)
        except BaseException as exc:
            receipt = self._transport.cancel_and_settle(broker_request.correlation_id, deadline)
            if not receipt.complete:
                self._gate.quarantine_and_release(lease)
                raise BrokerProtocolFailure("cst_saved_field.containment_settle_failed") from exc
            raise
        try:
            validated = validate_response(broker_request, response)
        except BaseException:
            self._gate.quarantine_and_release(lease)
            raise
        if self._qpc_frequency() != deadline.qpc_frequency or self._qpc_counter() >= deadline.deadline_tick:
            self._gate.quarantine_and_release(lease)
            raise BrokerProtocolFailure("cst_saved_field.deadline_exceeded")
        return validated


def windows_qpc_frequency() -> int:
    if os.name != "nt":
        raise OSError("QueryPerformanceFrequency is unavailable")
    import ctypes

    value = ctypes.c_int64()
    if not ctypes.WinDLL("kernel32", use_last_error=True).QueryPerformanceFrequency(ctypes.byref(value)):
        raise ctypes.WinError(ctypes.get_last_error())
    return int(value.value)


def windows_qpc_counter() -> int:
    if os.name != "nt":
        raise OSError("QueryPerformanceCounter is unavailable")
    import ctypes

    value = ctypes.c_int64()
    if not ctypes.WinDLL("kernel32", use_last_error=True).QueryPerformanceCounter(ctypes.byref(value)):
        raise ctypes.WinError(ctypes.get_last_error())
    return int(value.value)
