"""SCM broker service boundary for saved-field authorization and IPC state."""

from __future__ import annotations

import os
import re
import threading
from collections.abc import Callable, Mapping
from dataclasses import asdict, dataclass
from io import BytesIO
from typing import Literal, Protocol

from .cst_saved_field_broker_protocol import (
    BrokerChallengeV1,
    BrokerProtocolFailure,
    BrokerRequestV1,
    BrokerResponseV1,
    BrokerSettlementV1,
    QpcDeadlineV1,
    decode_one_frame,
    encode_frame,
    validate_response,
)
from .cst_saved_field_broker_worker_protocol import (
    BROKER_WORKER_RESPONSE_MAX,
    BrokerWorkerRequestV1,
    BrokerWorkerResponseV1,
    validate_worker_response,
)
from .cst_saved_field_containment_windows import WindowsContainedInvocation
from .cst_saved_field_policy import (
    EXACT_ENDPOINTS,
    AuthorityEntry,
    AuthoritySnapshot,
    PolicyPlatform,
    load_authority_snapshot,
)
from .cst_saved_field_transfer import TrustedWorkspacePolicy
from .cst_saved_field_vendor_isolation_windows import VendorIsolationProofV1

OUTPUT_ROOT_ENV = "MCPHUB_EM_OUTPUT_ROOT"
BROKER_ENDPOINT_V1 = EXACT_ENDPOINTS[2]
FRONTEND_ENDPOINT_V1 = EXACT_ENDPOINTS[1]
_NUMERIC_SID = re.compile(r"S-\d+(?:-\d+)+\Z", re.IGNORECASE)

NonceState = Literal["ISSUED", "CONSUMED", "CANCELLED"]


class DescriptorFailure(RuntimeError):
    pass


def _numeric_sid(value: str) -> str:
    if not isinstance(value, str) or _NUMERIC_SID.fullmatch(value) is None:
        raise DescriptorFailure("cst_saved_field.broker_descriptor_invalid")
    return value.upper()


@dataclass(frozen=True, slots=True)
class LocalPipeDescriptorV1:
    endpoint: str
    owner_sid: str
    dacl_protected: bool
    first_instance: bool
    remote_clients_rejected: bool
    message_mode: bool
    overlapped: bool
    instances: int
    sacl_integrity_sid: str
    sacl_no_write_up: bool
    audit_success_failure: bool
    aces: tuple[tuple[str, str, int, int], ...]

    def as_dict(self) -> dict[str, object]:
        return asdict(self)

    def verify_readback(self, observed: LocalPipeDescriptorV1) -> None:
        if type(observed) is not type(self) or observed != self:
            raise DescriptorFailure("cst_saved_field.broker_descriptor_invalid")


def _descriptor(
    *, endpoint: str, owner_sid: str, client_sid: str, deny_untrusted: bool
) -> LocalPipeDescriptorV1:
    owner = _numeric_sid(owner_sid)
    client = _numeric_sid(client_sid)
    full = 0x001F01FF
    client_rights = 0x00100083
    denied = (("DENY", "S-1-5-7", full, 0), ("DENY", "S-1-5-2", full, 0)) if deny_untrusted else ()
    return LocalPipeDescriptorV1(
        endpoint,
        owner,
        True,
        True,
        True,
        True,
        True,
        1,
        "S-1-16-12288",
        True,
        True,
        denied
        + (
            ("ALLOW", "S-1-5-18", full, 0),
            ("ALLOW", owner, full, 0),
            ("ALLOW", client, client_rights, 0),
        ),
    )


def build_broker_descriptor(*, broker_service_sid: str, daemon_service_sid: str) -> LocalPipeDescriptorV1:
    return _descriptor(
        endpoint=BROKER_ENDPOINT_V1,
        owner_sid=broker_service_sid,
        client_sid=daemon_service_sid,
        deny_untrusted=True,
    )


def build_frontend_descriptor(*, daemon_service_sid: str, frontend_user_sid: str) -> LocalPipeDescriptorV1:
    return _descriptor(
        endpoint=FRONTEND_ENDPOINT_V1,
        owner_sid=daemon_service_sid,
        client_sid=frontend_user_sid,
        deny_untrusted=True,
    )


def validate_sampler_descriptors(descriptors: tuple[object, ...]) -> None:
    if len(descriptors) != 3:
        raise DescriptorFailure("cst_saved_field.broker_descriptor_invalid")
    endpoints: list[str] = []
    for descriptor in descriptors:
        endpoint = getattr(descriptor, "endpoint", None)
        owner = getattr(descriptor, "owner_sid", None)
        aces = getattr(descriptor, "aces", None)
        if (
            endpoint not in EXACT_ENDPOINTS
            or _NUMERIC_SID.fullmatch(owner or "") is None
            or getattr(descriptor, "dacl_protected", False) is not True
            or getattr(descriptor, "first_instance", False) is not True
            or getattr(descriptor, "remote_clients_rejected", False) is not True
            or getattr(descriptor, "message_mode", False) is not True
            or getattr(descriptor, "sacl_integrity_sid", None) != "S-1-16-12288"
            or getattr(descriptor, "sacl_no_write_up", False) is not True
            or getattr(descriptor, "audit_success_failure", False) is not True
            or not isinstance(aces, tuple)
            or any(_NUMERIC_SID.fullmatch(ace[1]) is None for ace in aces)
        ):
            raise DescriptorFailure("cst_saved_field.broker_descriptor_invalid")
        endpoints.append(endpoint)
    if tuple(endpoints) != EXACT_ENDPOINTS or len(set(endpoints)) != 3:
        raise DescriptorFailure("cst_saved_field.broker_descriptor_invalid")


@dataclass(frozen=True, slots=True)
class BrokerPeerIdentityV1:
    pid: int
    token_user_sid: str
    service_sid: str
    service_sid_enabled: bool
    scm_pid_matches: bool
    session_id: int
    high_integrity: bool
    prohibited_privileges_absent: bool
    image_path: str


def _authenticate_peer(peer: BrokerPeerIdentityV1, *, daemon_service_sid: str, daemon_image: str) -> None:
    expected_sid = _numeric_sid(daemon_service_sid)
    if (
        not isinstance(peer, BrokerPeerIdentityV1)
        or type(peer.pid) is not int
        or peer.pid <= 0
        or peer.token_user_sid.upper() != expected_sid
        or peer.service_sid.upper() != expected_sid
        or peer.service_sid_enabled is not True
        or peer.scm_pid_matches is not True
        or peer.session_id != 0
        or peer.high_integrity is not True
        or peer.prohibited_privileges_absent is not True
        or not daemon_image
        or peer.image_path.casefold() != daemon_image.casefold()
    ):
        raise BrokerProtocolFailure("cst_saved_field.broker_unauthorized")


def bcrypt_gen_random(count: int) -> bytes:
    if os.name != "nt" or type(count) is not int or count <= 0:
        raise BrokerProtocolFailure("cst_saved_field.broker_unavailable")
    import ctypes

    value = (ctypes.c_ubyte * count)()
    status = ctypes.WinDLL("bcrypt", use_last_error=True).BCryptGenRandom(None, value, count, 0x00000002)
    if status != 0:
        raise BrokerProtocolFailure("cst_saved_field.broker_unavailable")
    return bytes(value)


@dataclass(frozen=True, slots=True)
class _NonceEntry:
    deadline: QpcDeadlineV1
    issued_tick: int
    expires_tick: int


class BrokerNonceLedgerV1:
    def __init__(self, random_bytes: Callable[[int], bytes]) -> None:
        self._random_bytes = random_bytes
        self._lock = threading.Lock()
        self._outstanding: dict[str, _NonceEntry] = {}
        self._history: dict[str, NonceState] = {}

    def issue(self, deadline: QpcDeadlineV1, issued_tick: int) -> BrokerChallengeV1:
        raw = self._random_bytes(32)
        if not isinstance(raw, bytes) or len(raw) != 32:
            raise BrokerProtocolFailure("cst_saved_field.broker_unavailable")
        expires = min(
            issued_tick + 5 * deadline.qpc_frequency,
            deadline.deadline_tick,
        )
        challenge = BrokerChallengeV1(raw.hex(), issued_tick, expires, deadline.qpc_frequency)
        with self._lock:
            if self._outstanding:
                raise BrokerProtocolFailure("cst_saved_field.resource_busy")
            self._outstanding[challenge.nonce] = _NonceEntry(deadline, issued_tick, expires)
            self._history[challenge.nonce] = "ISSUED"
        return challenge

    def consume(self, request: BrokerRequestV1, *, current_tick: int, current_frequency: int) -> None:
        with self._lock:
            entry = self._outstanding.pop(request.nonce, None)
            if entry is not None:
                self._history[request.nonce] = "CONSUMED"
        if (
            entry is None
            or request.deadline != entry.deadline
            or current_frequency != entry.deadline.qpc_frequency
            or current_tick < entry.issued_tick
            or current_tick >= entry.expires_tick
            or current_tick >= entry.deadline.deadline_tick
        ):
            raise BrokerProtocolFailure("cst_saved_field.broker_protocol_invalid")

    def cancel(self, nonce: str) -> None:
        with self._lock:
            if self._outstanding.pop(nonce, None) is not None:
                self._history[nonce] = "CANCELLED"

    def cancel_all(self) -> None:
        with self._lock:
            for nonce in tuple(self._outstanding):
                self._outstanding.pop(nonce)
                self._history[nonce] = "CANCELLED"

    def state(self, nonce: str) -> NonceState | None:
        with self._lock:
            return self._history.get(nonce)

    @property
    def outstanding_count(self) -> int:
        with self._lock:
            return len(self._outstanding)


class BrokerApplication(Protocol):
    def __call__(self, request: BrokerRequestV1, workspace_policy: object) -> BrokerResponseV1: ...


class ContainedWorkerBrokerApplicationV1:
    """Broker-owned adapter from the admitted request to one contained worker."""

    def __init__(
        self,
        *,
        invocation: WindowsContainedInvocation,
        vendor_isolation: VendorIsolationProofV1,
    ) -> None:
        if not isinstance(vendor_isolation, VendorIsolationProofV1) or not vendor_isolation.complete:
            raise BrokerProtocolFailure("cst_saved_field.vendor_isolation_unavailable")
        self._invocation = invocation

    def __call__(self, request: BrokerRequestV1, workspace_policy: object) -> BrokerResponseV1:
        # The policy remains broker-owned ambient authority; it is never serialized
        # to the worker.  Its lifetime is settled by BrokerRuntimeServiceV1.
        if workspace_policy is None:
            raise BrokerProtocolFailure("cst_saved_field.workspace_invalid")
        worker_request = BrokerWorkerRequestV1(
            request.correlation_id,
            request.policy_revision,
            request.entry_id,
            request.manifest_sha256,
            request.request_sha256,
            request.request,
            request.deadline,
        )
        response_frame = self._invocation.invoke(
            encode_frame(worker_request.to_wire()),
            deadline=request.deadline,
        )
        worker_response = validate_worker_response(
            worker_request,
            BrokerWorkerResponseV1.from_wire(
                decode_one_frame(BytesIO(response_frame), maximum=BROKER_WORKER_RESPONSE_MAX)
            ),
        )
        worker_settlement = worker_response.settlement
        settlement = BrokerSettlementV1(
            True,
            True,
            True,
            True,
            True,
            True,
            True,
            worker_settlement.workspace_settled,
            worker_settlement.session_settled,
            worker_settlement.source_unchanged,
            worker_settlement.owned_remaining,
        )
        return BrokerResponseV1(
            request.correlation_id,
            request.policy_revision,
            request.request_sha256,
            request.deadline,
            worker_response.ok,
            worker_response.text,
            worker_response.failure_id,
            settlement,
        )


def load_output_workspace_policy(
    environment: Mapping[str, str],
    platform: PolicyPlatform,
    *,
    factory: Callable[[str, PolicyPlatform], object] = TrustedWorkspacePolicy.from_windows_root,
) -> object:
    raw = environment.get(OUTPUT_ROOT_ENV)
    if not isinstance(raw, str) or not raw:
        raise BrokerProtocolFailure("cst_saved_field.workspace_invalid")
    try:
        return factory(raw, platform)
    except BrokerProtocolFailure:
        raise
    except Exception as exc:
        raise BrokerProtocolFailure("cst_saved_field.workspace_invalid") from exc


class BrokerRuntimeServiceV1:
    def __init__(
        self,
        *,
        snapshot: AuthoritySnapshot,
        daemon_service_sid: str,
        daemon_image: str,
        workspace_policy: object,
        application: BrokerApplication,
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
        random_bytes: Callable[[int], bytes] = bcrypt_gen_random,
    ) -> None:
        if not isinstance(snapshot, AuthoritySnapshot) or not snapshot.entries:
            raise BrokerProtocolFailure("cst_saved_field.policy_disabled")
        self._snapshot = snapshot
        self._daemon_service_sid = _numeric_sid(daemon_service_sid)
        self._daemon_image = daemon_image
        self._workspace_policy = workspace_policy
        self._application = application
        self._qpc_frequency = qpc_frequency
        self._qpc_counter = qpc_counter
        self._nonces = BrokerNonceLedgerV1(random_bytes)

    @classmethod
    def from_policy(
        cls,
        *,
        raw_policy_path: str | None,
        policy_platform: PolicyPlatform,
        environment: Mapping[str, str],
        daemon_service_sid: str,
        daemon_image: str,
        application: BrokerApplication,
        qpc_frequency: Callable[[], int],
        qpc_counter: Callable[[], int],
        random_bytes: Callable[[int], bytes] = bcrypt_gen_random,
    ) -> BrokerRuntimeServiceV1:
        loaded = load_authority_snapshot(raw_policy_path, policy_platform)
        if not loaded.enabled or loaded.snapshot is None:
            raise BrokerProtocolFailure(loaded.failure_id or "cst_saved_field.policy_invalid")
        workspace = load_output_workspace_policy(environment, policy_platform)
        return cls(
            snapshot=loaded.snapshot,
            daemon_service_sid=daemon_service_sid,
            daemon_image=daemon_image,
            workspace_policy=workspace,
            application=application,
            qpc_frequency=qpc_frequency,
            qpc_counter=qpc_counter,
            random_bytes=random_bytes,
        )

    def issue_challenge(self, peer: BrokerPeerIdentityV1, deadline: QpcDeadlineV1) -> BrokerChallengeV1:
        _authenticate_peer(
            peer,
            daemon_service_sid=self._daemon_service_sid,
            daemon_image=self._daemon_image,
        )
        frequency = self._qpc_frequency()
        tick = self._qpc_counter()
        if (
            frequency != deadline.qpc_frequency
            or tick < deadline.admitted_tick
            or tick >= deadline.deadline_tick
        ):
            raise BrokerProtocolFailure("cst_saved_field.broker_protocol_invalid")
        return self._nonces.issue(deadline, tick)

    def _entry(self, request: BrokerRequestV1) -> AuthorityEntry:
        matches = [entry for entry in self._snapshot.entries if entry.entry_id == request.entry_id]
        if (
            request.policy_revision != self._snapshot.revision
            or len(matches) != 1
            or request.manifest_sha256 != matches[0].bundle_manifest_sha256
        ):
            raise BrokerProtocolFailure("cst_saved_field.broker_unauthorized")
        return matches[0]

    def exchange(self, peer: BrokerPeerIdentityV1, request: BrokerRequestV1) -> BrokerResponseV1:
        try:
            _authenticate_peer(
                peer,
                daemon_service_sid=self._daemon_service_sid,
                daemon_image=self._daemon_image,
            )
        except BaseException:
            self._nonces.cancel(request.nonce)
            raise
        frequency = self._qpc_frequency()
        tick = self._qpc_counter()
        self._nonces.consume(request, current_tick=tick, current_frequency=frequency)
        self._entry(request)
        response = self._application(request, self._workspace_policy)
        return validate_response(request, response)

    def cancel(self, nonce: str) -> None:
        self._nonces.cancel(nonce)

    def shutdown(self) -> None:
        self._nonces.cancel_all()
        close = getattr(self._workspace_policy, "close", None)
        if callable(close):
            close()

    def nonce_state(self, nonce: str) -> NonceState | None:
        return self._nonces.state(nonce)

    @property
    def outstanding_nonce_count(self) -> int:
        return self._nonces.outstanding_count


def run_service(
    service: BrokerRuntimeServiceV1,
    serve: Callable[[BrokerRuntimeServiceV1], int],
) -> int:
    try:
        return serve(service)
    finally:
        service.shutdown()


def main() -> int:
    raise BrokerProtocolFailure("cst_saved_field.broker_unavailable")
