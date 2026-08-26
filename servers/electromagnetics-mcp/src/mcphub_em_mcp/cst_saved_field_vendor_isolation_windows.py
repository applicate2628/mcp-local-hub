"""Windows broker identity, provisioning, pipe authorization, and runtime owners.

The provisioner is data-only by default.  A later explicitly approved target phase
may apply its receipts through a platform adapter; importing this module never opens
the Service Control Manager or a named pipe.
"""

from __future__ import annotations

import hmac
from collections.abc import Callable
from dataclasses import dataclass
from typing import Literal, Protocol

from .cst_saved_field_broker_protocol import BrokerRequestV1
from .cst_saved_field_port import SealedVendorOutput, VendorPathLeaseSettlement

DAEMON_SERVICE = "McpLocalHubCstDaemon"
BROKER_SERVICE = "McpLocalHubCstVendorBroker"
DAEMON_ACCOUNT = rf"NT SERVICE\{DAEMON_SERVICE}"
BROKER_ACCOUNT = rf"NT SERVICE\{BROKER_SERVICE}"


class BrokerIsolationFailure(RuntimeError):
    def __init__(self, failure_id: str, *, quarantine: bool = False) -> None:
        super().__init__(failure_id)
        self.failure_id = failure_id
        self.quarantine = quarantine


@dataclass(frozen=True, slots=True)
class VendorIsolationProofV1:
    service_name: str
    token_user: str
    workspace_owner: str
    protected_dacl: bool
    daemon_access_denied: bool
    session_id: int

    @property
    def complete(self) -> bool:
        return (
            self.service_name == BROKER_SERVICE
            and self.token_user == BROKER_ACCOUNT
            and self.workspace_owner == BROKER_ACCOUNT
            and self.protected_dacl is True
            and self.daemon_access_denied is True
            and self.session_id == 0
        )


@dataclass(frozen=True, slots=True)
class ServiceSpec:
    name: str
    account: str
    image: str
    credential_free: bool = True
    session_id: int = 0
    service_sid_type: str = "SERVICE_SID_TYPE_UNRESTRICTED"
    protected_dacl: bool = True

    def __post_init__(self) -> None:
        if (
            self.name not in {DAEMON_SERVICE, BROKER_SERVICE}
            or self.account != rf"NT SERVICE\{self.name}"
            or not self.image
            or not self.credential_free
            or self.session_id != 0
            or self.service_sid_type != "SERVICE_SID_TYPE_UNRESTRICTED"
            or not self.protected_dacl
        ):
            raise ValueError("invalid fixed service specification")


@dataclass(frozen=True, slots=True)
class ServiceContract:
    services: tuple[ServiceSpec, ServiceSpec]


@dataclass(frozen=True, slots=True)
class ProvisioningReceipt:
    mode: Literal["provision", "rollback"]
    dry_run: bool
    actions: tuple[str, ...]
    live_scm_calls: int


def service_contract(*, daemon_image: str, broker_image: str) -> ServiceContract:
    return ServiceContract(
        (
            ServiceSpec(DAEMON_SERVICE, DAEMON_ACCOUNT, daemon_image),
            ServiceSpec(BROKER_SERVICE, BROKER_ACCOUNT, broker_image),
        )
    )


def dry_run_provisioning(contract: ServiceContract) -> ProvisioningReceipt:
    actions: list[str] = []
    for item in contract.services:
        actions.extend((f"create:{item.name}", f"configure_sid:{item.name}", f"protect_dacl:{item.name}"))
    actions.append("restart_required")
    return ProvisioningReceipt("provision", True, tuple(actions), 0)


def dry_run_rollback(contract: ServiceContract) -> ProvisioningReceipt:
    names = tuple(item.name for item in contract.services)
    if names != (DAEMON_SERVICE, BROKER_SERVICE):
        raise ValueError("unexpected service contract")
    return ProvisioningReceipt(
        "rollback",
        True,
        (
            f"stop:{DAEMON_SERVICE}",
            "request_broker_settlement",
            f"stop:{BROKER_SERVICE}",
            "prove_service_handles_signaled",
            "prove_job_worker_pipe_workspace_absent",
            "disable_policy",
            f"delete:{DAEMON_SERVICE}",
            f"delete:{BROKER_SERVICE}",
            "revoke_service_acl_state",
            "restart_required",
        ),
        0,
    )


@dataclass(frozen=True, slots=True)
class PeerTokenProofV1:
    service_name: str
    service_account: str
    scm_process_matches: bool
    token_user_matches: bool
    service_sid_enabled: bool
    logon_sid_matches: bool
    session_id: int
    high_integrity: bool
    prohibited_privileges_absent: bool
    pinned_image_matches: bool

    @property
    def complete(self) -> bool:
        return (
            self.service_name in {DAEMON_SERVICE, BROKER_SERVICE}
            and self.service_account == rf"NT SERVICE\{self.service_name}"
            and self.scm_process_matches
            and self.token_user_matches
            and self.service_sid_enabled
            and self.logon_sid_matches
            and self.session_id == 0
            and self.high_integrity
            and self.prohibited_privileges_absent
            and self.pinned_image_matches
        )


class ImpersonationPort(Protocol):
    def impersonate(self) -> bool: ...

    def revert(self) -> bool: ...


class AuthenticatedPipeSession:
    """Authenticate and revert before parsing any request bytes."""

    def __init__(
        self,
        *,
        peer: PeerTokenProofV1,
        impersonation: ImpersonationPort,
        privileged_counter: Callable[[], None],
    ) -> None:
        self._peer = peer
        self._impersonation = impersonation
        self._privileged_counter = privileged_counter

    def authenticate_then(self, parse: Callable[[], BrokerRequestV1]) -> BrokerRequestV1:
        if not self._impersonation.impersonate():
            raise BrokerIsolationFailure("cst_saved_field.broker_unauthorized")
        authenticated = False
        revert_proven = False
        try:
            authenticated = self._peer.complete and self._peer.service_name == DAEMON_SERVICE
        finally:
            revert_proven = self._impersonation.revert()
        if not revert_proven:
            raise BrokerIsolationFailure("cst_saved_field.containment_settle_failed", quarantine=True)
        if not authenticated:
            raise BrokerIsolationFailure("cst_saved_field.broker_unauthorized")
        request = parse()
        self._privileged_counter()
        return request


class VendorPathPlatform(Protocol):
    def hold_ancestor(
        self, relative: str, *, share_read: bool, share_write: bool, share_delete: bool
    ) -> tuple[int, str]: ...

    def hold_read_input(
        self, relative: str, *, share_read: bool, share_write: bool, share_delete: bool
    ) -> tuple[int, str]: ...

    def prepare_output(self, relative: str) -> tuple[int, str]: ...

    def seal_output(
        self, relative: str, *, share_read: bool, share_write: bool, share_delete: bool
    ) -> tuple[int, str, str]: ...

    def create_clean_input(
        self,
        source_relative: str,
        destination_relative: str,
        expected_sha256: str,
        *,
        share_read: bool,
        share_write: bool,
        share_delete: bool,
    ) -> tuple[int, str, str]: ...

    def revalidate(self, handle: int) -> bool: ...

    def close(self, handle: int) -> bool: ...


class IsolatedVendorPathLease:
    """Single owner for retained ancestors, read inputs, write targets, and seals."""

    def __init__(self, platform: VendorPathPlatform, *, proof: VendorIsolationProofV1) -> None:
        if not proof.complete:
            raise BrokerIsolationFailure("cst_saved_field.vendor_isolation_unavailable")
        self._platform = platform
        self._handles: list[int] = []
        self._received = 0
        self._attempts = 0
        self._closed = False

    def _own(self, value: tuple[int, str]) -> str:
        handle, locator = value
        if type(handle) is not int or handle <= 0 or not isinstance(locator, str) or not locator:
            raise BrokerIsolationFailure("cst_saved_field.path_identity_ambiguous")
        self._handles.append(handle)
        self._received += 1
        return locator

    def hold_ancestor(self, relative: str) -> str:
        return self._own(
            self._platform.hold_ancestor(relative, share_read=True, share_write=True, share_delete=False)
        )

    def hold_read_input(self, relative: str) -> str:
        return self._own(
            self._platform.hold_read_input(relative, share_read=True, share_write=False, share_delete=False)
        )

    def prepare_output(self, relative: str) -> str:
        return self._own(self._platform.prepare_output(relative))

    def seal_output(self, relative: str) -> SealedVendorOutput:
        handle, locator, digest = self._platform.seal_output(
            relative, share_read=False, share_write=False, share_delete=False
        )
        self._own((handle, locator))
        if len(digest) != 64 or any(character not in "0123456789abcdef" for character in digest):
            raise BrokerIsolationFailure("cst_saved_field.path_identity_ambiguous")
        return SealedVendorOutput(locator, digest)

    def create_clean_input(
        self, source_relative: str, destination_relative: str, expected_sha256: str
    ) -> str:
        handle, locator, digest = self._platform.create_clean_input(
            source_relative,
            destination_relative,
            expected_sha256,
            share_read=True,
            share_write=False,
            share_delete=False,
        )
        self._own((handle, locator))
        if not hmac.compare_digest(digest, expected_sha256):
            raise BrokerIsolationFailure("cst_saved_field.source_changed")
        return locator

    def revalidate_all(self) -> None:
        if self._closed or not all(self._platform.revalidate(handle) for handle in self._handles):
            raise BrokerIsolationFailure("cst_saved_field.source_changed")

    def settle(self) -> VendorPathLeaseSettlement:
        failures = False
        for handle in tuple(reversed(self._handles)):
            self._attempts += 1
            if self._platform.close(handle):
                self._handles.remove(handle)
            else:
                failures = True
        self._closed = not self._handles
        return VendorPathLeaseSettlement(
            handles_received=self._received,
            close_attempts=self._attempts,
            close_succeeded=not failures and self._closed,
            owned_remaining=len(self._handles),
        )
