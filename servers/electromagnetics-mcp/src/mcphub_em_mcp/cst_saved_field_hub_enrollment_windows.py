"""Daemon-owned HubEnrollmentProtocolV1 state and endpoint descriptor.

The operating-system listener supplies kernel-observed peer facts and an
independent supervisor-status query.  This module owns no ambient policy,
service control, process launch, or frontend transport.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import re
import threading
import time
from collections.abc import Callable
from dataclasses import asdict, dataclass
from typing import Any, Literal

from .cst_saved_field_policy import EXACT_ENDPOINTS

HUB_ENROLLMENT_PROTOCOL_V1 = "HubEnrollmentProtocolV1"
HUB_ENROLLMENT_FRAME_MAX = 4096
HUB_ENROLLMENT_EXPIRY_SECONDS = 5.0
SUPERVISOR_QUERY_TIMEOUT_SECONDS = 5.0
ENROLLMENT_ENDPOINT_V1 = EXACT_ENDPOINTS[0]

_HEX32 = re.compile(r"[0-9a-f]{32}\Z")
_HEX64 = re.compile(r"[0-9a-f]{64}\Z")
_NUMERIC_SID = re.compile(r"S-\d+(?:-\d+)+\Z", re.IGNORECASE)

ChannelState = Literal["ISSUED", "CONSUMED", "CANCELLED"]
CapabilityState = Literal["ENROLLED", "CONSUMED", "CANCELLED"]


@dataclass(frozen=True, slots=True)
class EnrollmentFailure(RuntimeError):
    failure_id: str

    def __str__(self) -> str:
        return f"cst_saved_field.enrollment_{self.failure_id}"


@dataclass(frozen=True, slots=True)
class DescriptorFailure(RuntimeError):
    failure_id: str = "cst_saved_field.enrollment_descriptor_invalid"

    def __str__(self) -> str:
        return self.failure_id


@dataclass(frozen=True, slots=True)
class EnrollmentPeerIdentityV1:
    """Facts obtained from the connected pipe and process/token handles."""

    pid: int
    creation_time: str
    image_path: str
    package_identity: str
    parent_pid: int
    token_user_sid: str
    session_id: int


@dataclass(frozen=True, slots=True)
class SupervisorTaskIdentityV1:
    """Independent current-task row plus canonical installed identity facts."""

    task: str
    generation: int
    pid: int
    creation_time: str
    image_path: str
    package_identity: str
    parent_pid: int
    token_user_sid: str
    session_id: int


@dataclass(frozen=True, slots=True)
class EnrollmentEndpointDescriptorV1:
    endpoint: str
    owner_sid: str
    dacl_protected: bool
    first_instance: bool
    remote_clients_rejected: bool
    message_mode: bool
    sacl_integrity_sid: str
    sacl_no_write_up: bool
    audit_success_failure: bool
    aces: tuple[tuple[str, str, int, int], ...]

    def as_dict(self) -> dict[str, object]:
        return asdict(self)

    def verify_readback(self, observed: EnrollmentEndpointDescriptorV1) -> None:
        if type(observed) is not type(self) or observed != self:
            raise DescriptorFailure()


def _require_numeric_sid(value: str) -> None:
    if not isinstance(value, str) or _NUMERIC_SID.fullmatch(value) is None:
        raise DescriptorFailure()


def build_enrollment_descriptor(
    *, daemon_service_sid: str, policy_owner_sid: str
) -> EnrollmentEndpointDescriptorV1:
    """Build the exact runtime-numeric descriptor later applied/read back by Win32."""

    _require_numeric_sid(daemon_service_sid)
    _require_numeric_sid(policy_owner_sid)
    system_sid = "S-1-5-18"
    return EnrollmentEndpointDescriptorV1(
        endpoint=ENROLLMENT_ENDPOINT_V1,
        owner_sid=daemon_service_sid.upper(),
        dacl_protected=True,
        first_instance=True,
        remote_clients_rejected=True,
        message_mode=True,
        sacl_integrity_sid="S-1-16-12288",
        sacl_no_write_up=True,
        audit_success_failure=True,
        aces=(
            ("ALLOW", system_sid, 0x001F01FF, 0),
            ("ALLOW", daemon_service_sid.upper(), 0x001F01FF, 0),
            ("ALLOW", policy_owner_sid.upper(), 0x0012019F, 0),
        ),
    )


@dataclass(slots=True)
class _ChannelEntry:
    peer: EnrollmentPeerIdentityV1
    expires_at: float


@dataclass(slots=True)
class _CapabilityEntry:
    digest: str
    generation: int
    peer: EnrollmentPeerIdentityV1
    expires_at: float


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise EnrollmentFailure("frame_invalid")
        result[key] = value
    return result


def _decode_frame(raw: bytes) -> dict[str, Any]:
    if not isinstance(raw, bytes) or not raw or len(raw) > HUB_ENROLLMENT_FRAME_MAX:
        raise EnrollmentFailure("frame_invalid")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_reject_duplicate_keys)
    except (UnicodeError, json.JSONDecodeError, TypeError, EnrollmentFailure) as exc:
        raise EnrollmentFailure("frame_invalid") from exc
    if not isinstance(value, dict):
        raise EnrollmentFailure("frame_invalid")
    return value


def _exact_keys(value: dict[str, Any], expected: frozenset[str]) -> None:
    if frozenset(value) != expected:
        raise EnrollmentFailure("frame_invalid")


def _same_peer(peer: EnrollmentPeerIdentityV1, current: SupervisorTaskIdentityV1) -> bool:
    return (
        current.task == "cst"
        and current.generation > 0
        and peer.pid > 0
        and peer.parent_pid > 0
        and peer.session_id >= 0
        and peer.pid == current.pid
        and peer.creation_time == current.creation_time
        and peer.image_path.casefold() == current.image_path.casefold()
        and peer.package_identity == current.package_identity
        and peer.parent_pid == current.parent_pid
        and peer.token_user_sid.casefold() == current.token_user_sid.casefold()
        and peer.session_id == current.session_id
    )


class HubEnrollmentServerV1:
    """Pure bounded enrollment state owner composed beneath the SCM listener."""

    def __init__(
        self,
        *,
        query_supervisor: Callable[[float], SupervisorTaskIdentityV1],
        random_bytes: Callable[[int], bytes],
        monotonic: Callable[[], float] = time.monotonic,
    ) -> None:
        self._query_supervisor = query_supervisor
        self._random_bytes = random_bytes
        self._monotonic = monotonic
        self._lock = threading.RLock()
        self._channels: dict[str, _ChannelEntry] = {}
        self._channel_history: dict[str, ChannelState] = {}
        self._capabilities: dict[str, _CapabilityEntry] = {}
        self._capability_history: dict[str, CapabilityState] = {}

    def _authenticate(self, peer: EnrollmentPeerIdentityV1) -> SupervisorTaskIdentityV1:
        try:
            current = self._query_supervisor(SUPERVISOR_QUERY_TIMEOUT_SECONDS)
        except Exception as exc:
            raise EnrollmentFailure("identity_unavailable") from exc
        if not isinstance(current, SupervisorTaskIdentityV1) or not _same_peer(peer, current):
            raise EnrollmentFailure("identity_mismatch")
        return current

    def issue_challenge(self, peer: EnrollmentPeerIdentityV1) -> str:
        self.expire()
        self._authenticate(peer)
        nonce = self._random_bytes(32)
        if not isinstance(nonce, bytes) or len(nonce) != 32:
            raise EnrollmentFailure("rng_invalid")
        challenge = nonce.hex()
        with self._lock:
            if self._channels:
                raise EnrollmentFailure("channel_busy")
            self._channels[challenge] = _ChannelEntry(
                peer=peer,
                expires_at=self._monotonic() + HUB_ENROLLMENT_EXPIRY_SECONDS,
            )
            self._channel_history[challenge] = "ISSUED"
        return challenge

    def exchange(self, peer: EnrollmentPeerIdentityV1, raw: bytes) -> dict[str, object]:
        try:
            self.expire()
            current = self._authenticate(peer)
            frame = _decode_frame(raw)
            if frame.get("op") == "enroll":
                return self._enroll(peer, current, frame)
            if frame.get("op") == "cancel":
                return self._cancel_exchange(peer, frame)
            raise EnrollmentFailure("frame_invalid")
        except EnrollmentFailure:
            self._cancel_channels_for_peer(peer)
            raise

    def _enroll(
        self,
        peer: EnrollmentPeerIdentityV1,
        current: SupervisorTaskIdentityV1,
        frame: dict[str, Any],
    ) -> dict[str, object]:
        _exact_keys(
            frame,
            frozenset(
                {
                    "version",
                    "op",
                    "challenge",
                    "correlation",
                    "task",
                    "generation",
                    "capability_sha256",
                }
            ),
        )
        challenge = frame["challenge"]
        correlation = frame["correlation"]
        digest = frame["capability_sha256"]
        if (
            type(frame["version"]) is not int
            or frame["version"] != 1
            or frame["op"] != "enroll"
            or not isinstance(challenge, str)
            or _HEX64.fullmatch(challenge) is None
            or not isinstance(correlation, str)
            or _HEX32.fullmatch(correlation) is None
            or frame["task"] != "cst"
            or type(frame["generation"]) is not int
            or frame["generation"] != current.generation
            or not isinstance(digest, str)
            or _HEX64.fullmatch(digest) is None
        ):
            raise EnrollmentFailure("frame_invalid")
        with self._lock:
            channel = self._channels.get(challenge)
            if channel is None or channel.peer != peer:
                raise EnrollmentFailure("replay")
            if correlation in self._capability_history:
                self._cancel_channel(challenge)
                raise EnrollmentFailure("replay")
            del self._channels[challenge]
            self._channel_history[challenge] = "CONSUMED"
            self._capabilities[correlation] = _CapabilityEntry(
                digest=digest,
                generation=current.generation,
                peer=peer,
                expires_at=self._monotonic() + HUB_ENROLLMENT_EXPIRY_SECONDS,
            )
            self._capability_history[correlation] = "ENROLLED"
        return self._receipt(correlation, "ENROLLED")

    def _cancel_exchange(self, peer: EnrollmentPeerIdentityV1, frame: dict[str, Any]) -> dict[str, object]:
        _exact_keys(frame, frozenset({"version", "op", "correlation"}))
        correlation = frame["correlation"]
        if (
            type(frame["version"]) is not int
            or frame["version"] != 1
            or frame["op"] != "cancel"
            or not isinstance(correlation, str)
            or _HEX32.fullmatch(correlation) is None
        ):
            raise EnrollmentFailure("frame_invalid")
        with self._lock:
            pending = [challenge for challenge, entry in self._channels.items() if entry.peer == peer]
            if len(pending) != 1 or self._capability_history.get(correlation) != "ENROLLED":
                raise EnrollmentFailure("replay")
            self._cancel_channel(pending[0])
            self._cancel_capability(correlation)
        return self._receipt(correlation, "CANCELLED")

    @staticmethod
    def _receipt(correlation: str, state: CapabilityState) -> dict[str, object]:
        return {
            "version": 1,
            "correlation": correlation,
            "state": state,
            "channel_settled": True,
        }

    def consume_frontend(
        self,
        correlation: str,
        capability: bytes,
        *,
        exact_32_and_eof: bool,
        frontend_challenge_consumed: bool,
    ) -> bool:
        self.expire()
        with self._lock:
            entry = self._capabilities.get(correlation)
            if entry is None:
                return False
            supplied = hashlib.sha256(capability).hexdigest() if len(capability) == 32 else ""
            valid = (
                exact_32_and_eof
                and frontend_challenge_consumed
                and hmac.compare_digest(entry.digest, supplied)
            )
            if valid:
                del self._capabilities[correlation]
                self._capability_history[correlation] = "CONSUMED"
                return True
            self._cancel_capability(correlation)
            return False

    def expire(self) -> None:
        now = self._monotonic()
        with self._lock:
            for challenge, entry in tuple(self._channels.items()):
                if entry.expires_at <= now:
                    self._cancel_channel(challenge)
            for correlation, entry in tuple(self._capabilities.items()):
                if entry.expires_at <= now:
                    self._cancel_capability(correlation)

    def _cancel_channels_for_peer(self, peer: EnrollmentPeerIdentityV1) -> None:
        with self._lock:
            for challenge, entry in tuple(self._channels.items()):
                if entry.peer == peer:
                    self._cancel_channel(challenge)

    def _cancel_channel(self, challenge: str) -> None:
        self._channels.pop(challenge, None)
        if self._channel_history.get(challenge) == "ISSUED":
            self._channel_history[challenge] = "CANCELLED"

    def _cancel_capability(self, correlation: str) -> None:
        self._capabilities.pop(correlation, None)
        if self._capability_history.get(correlation) == "ENROLLED":
            self._capability_history[correlation] = "CANCELLED"

    def _terminalize_one(self, correlation: str) -> None:
        with self._lock:
            self._cancel_capability(correlation)
            for challenge in tuple(self._channels):
                self._cancel_channel(challenge)

    def ack_loss(self, correlation: str) -> None:
        self._terminalize_one(correlation)

    def post_ack_failure(self, correlation: str) -> None:
        self._terminalize_one(correlation)

    def child_exit(self, correlation: str) -> None:
        self._terminalize_one(correlation)

    def disconnect(self, correlation: str) -> None:
        self._terminalize_one(correlation)

    def service_stop(self, correlation: str | None = None) -> None:
        self._terminalize_all()

    def shutdown(self, correlation: str | None = None) -> None:
        self._terminalize_all()

    def restart(self, correlation: str | None = None) -> None:
        self._terminalize_all()

    def _terminalize_all(self) -> None:
        with self._lock:
            for challenge in tuple(self._channels):
                self._cancel_channel(challenge)
            for correlation in tuple(self._capabilities):
                self._cancel_capability(correlation)

    def channel_state(self, challenge: str) -> ChannelState | None:
        with self._lock:
            return self._channel_history.get(challenge)

    def capability_state(self, correlation: str) -> CapabilityState | None:
        with self._lock:
            return self._capability_history.get(correlation)

    @property
    def outstanding_channel_count(self) -> int:
        with self._lock:
            return len(self._channels)

    @property
    def outstanding_capability_count(self) -> int:
        with self._lock:
            return len(self._capabilities)
