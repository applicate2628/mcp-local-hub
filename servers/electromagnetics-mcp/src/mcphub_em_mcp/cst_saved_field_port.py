"""Neutral records shared by the saved-field application and vendor adapter."""

from __future__ import annotations

import unicodedata
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any, Literal, Protocol

_VENDOR_RELATIVE_SCALAR_MAX = 1_024
_VENDOR_RELATIVE_UTF8_MAX = 4_096
_VENDOR_RELATIVE_DEPTH_MAX = 32
_VENDOR_RESERVED = {
    "CON",
    "PRN",
    "AUX",
    "NUL",
    "CLOCK$",
    "CONIN$",
    "CONOUT$",
    *(f"COM{digit}" for digit in range(1, 10)),
    *(f"LPT{digit}" for digit in range(1, 10)),
}
_VENDOR_SUPERSCRIPT_DIGITS = str.maketrans({"¹": "1", "²": "2", "³": "3"})
_VENDOR_FORBIDDEN_COMPONENT = frozenset('<>:"/\\|?*')


def validate_vendor_relative_path(raw: str) -> tuple[str, ...]:
    """Validate the neutral relative namespace accepted by the vendor port."""
    if (
        not isinstance(raw, str)
        or not raw
        or len(raw) > _VENDOR_RELATIVE_SCALAR_MAX
        or len(raw.encode("utf-8")) > _VENDOR_RELATIVE_UTF8_MAX
        or raw.startswith(("\\", "/"))
        or ":" in raw
        or "\\" in raw
    ):
        raise ValueError("invalid vendor relative path")
    components = tuple(raw.split("/"))
    if not components or len(components) > _VENDOR_RELATIVE_DEPTH_MAX:
        raise ValueError("invalid vendor relative path")
    for component in components:
        if (
            not component
            or component in {".", ".."}
            or component.endswith((".", " "))
            or component != unicodedata.normalize("NFC", component)
            or "~" in component
            or any(character in _VENDOR_FORBIDDEN_COMPONENT or ord(character) < 32 for character in component)
            or len(component.encode("utf-16-le")) // 2 > 255
        ):
            raise ValueError("invalid vendor relative path")
        comparison_key = (
            component.casefold().split(".", 1)[0].rstrip(". ").translate(_VENDOR_SUPERSCRIPT_DIGITS)
        )
        if comparison_key.upper() in _VENDOR_RESERVED:
            raise ValueError("invalid vendor relative path")
    return components


@dataclass(frozen=True, slots=True)
class AbsoluteInvocationBudget:
    """One immutable 60-second QueryPerformanceCounter budget."""

    qpc_frequency: int
    admitted_tick: int
    deadline_tick: int

    def __post_init__(self) -> None:
        if (
            type(self.qpc_frequency) is not int
            or type(self.admitted_tick) is not int
            or type(self.deadline_tick) is not int
            or self.qpc_frequency <= 0
            or self.admitted_tick < 0
            or self.deadline_tick != self.admitted_tick + 60 * self.qpc_frequency
        ):
            raise ValueError("invalid absolute invocation budget")

    def to_wire(self) -> dict[str, int]:
        return {
            "qpc_frequency": self.qpc_frequency,
            "admitted_tick": self.admitted_tick,
            "deadline_tick": self.deadline_tick,
        }

    @classmethod
    def from_wire(cls, value: object) -> AbsoluteInvocationBudget:
        if not isinstance(value, dict) or set(value) != {
            "qpc_frequency",
            "admitted_tick",
            "deadline_tick",
        }:
            raise ValueError("invalid absolute invocation budget")
        return cls(value["qpc_frequency"], value["admitted_tick"], value["deadline_tick"])

    def remaining(self, *, current_frequency: int, current_tick: int) -> float:
        if current_frequency != self.qpc_frequency or type(current_tick) is not int:
            raise ValueError("invalid QueryPerformanceCounter observation")
        return max(0.0, (self.deadline_tick - current_tick) / self.qpc_frequency)

    def cleanup_deadline(self, *, termination_tick: int) -> int:
        if type(termination_tick) is not int or termination_tick < 0:
            raise ValueError("invalid termination tick")
        return termination_tick + 10 * self.qpc_frequency


@dataclass
class VendorFailure(Exception):
    failure_id: str
    stage: str
    safe_message: str
    causal_failure_id: str | None = None

    def __str__(self) -> str:
        return f"{self.failure_id}: {self.safe_message}"


@dataclass(frozen=True, slots=True)
class AcquisitionSettlement:
    stage: str
    transfer_committed: bool
    handles_received: int
    close_attempts: int
    close_succeeded: bool
    absence_proven: bool
    safely_attributed_remaining: int


@dataclass(frozen=True, slots=True)
class SealedVendorOutput:
    locator: str
    sha256: str


@dataclass(frozen=True, slots=True)
class VendorPathLeaseSettlement:
    handles_received: int
    close_attempts: int
    close_succeeded: bool
    owned_remaining: int

    @property
    def complete(self) -> bool:
        return (
            self.close_attempts >= self.handles_received
            and self.close_succeeded
            and self.owned_remaining == 0
        )


class AuthorizedVendorPathLease(Protocol):
    def hold_ancestor(self, relative: str) -> str: ...

    def hold_read_input(self, relative: str) -> str: ...

    def prepare_output(self, relative: str) -> str: ...

    def seal_output(self, relative: str) -> SealedVendorOutput: ...

    def create_clean_input(
        self, source_relative: str, destination_relative: str, expected_sha256: str
    ) -> str: ...

    def revalidate_all(self) -> None: ...

    def settle(self) -> VendorPathLeaseSettlement: ...


class OwnedSessionAcquisitionError(RuntimeError):
    def __init__(self, failure: VendorFailure, settlement: AcquisitionSettlement) -> None:
        super().__init__(str(failure))
        self.failure = failure
        self.settlement = settlement


@dataclass(frozen=True, slots=True)
class VendorSampleBatch:
    rows: tuple[tuple[tuple[float, float, float, float, float, float], int], ...]
    metadata: Mapping[str, str]
    initial_frequency_hz: float
    post_registration_frequency_hz: float
    activation_type: str
    generated_header: bool


@dataclass(frozen=True, slots=True)
class FieldFrameCandidate:
    field: Literal["E", "H"]
    port: int
    mode: int
    frequency_hz: float
    frame_id: str
    tree_path: str
    payload_relative: str
    adaptive_pass: str | None
    project_unit: str
    field_unit: str
    time_dependence: str
    time_dependence_status: str
    field_sha256: str
    initial_frequency_hz: float
    post_registration_frequency_hz: float
    activation_type: Literal["Efield3D", "Hfield3D"]
    status_policy: dict[int, bool]


@dataclass(frozen=True, slots=True)
class PreparedSavedFieldSource:
    """Opaque authorized-workspace capability plus verified source identities."""

    capability: Any
    candidates: tuple[FieldFrameCandidate, ...]
    project_relative: str
    project_sha256: str
    mesh_relative: str
    mesh_sha256: str


@dataclass(frozen=True, slots=True)
class ApplicationSettlement:
    workspace_settled: bool
    session_settled: bool
    source_unchanged: bool
    owned_remaining: int
    cache_cleared: bool
    closed_without_save: bool
    acquisition: AcquisitionSettlement | None
    source_changed_role: Literal["project", "mesh", "field"] | None = None

    @property
    def complete(self) -> bool:
        return (
            self.workspace_settled
            and self.session_settled
            and self.source_unchanged
            and self.owned_remaining == 0
        )


class SavedFieldApplicationPort(Protocol):
    """Filesystem/vendor abstraction consumed by the pure application core."""

    def prepare_source(self, request: Any) -> PreparedSavedFieldSource: ...

    def open_owned_session(self, source: PreparedSavedFieldSource) -> tuple[Any, AcquisitionSettlement]: ...

    def activate_and_sample(
        self,
        owned: Any,
        source: PreparedSavedFieldSource,
        frame: FieldFrameCandidate,
        points_project: tuple[tuple[float, float, float], ...],
        frequency_tolerance_hz: float,
    ) -> VendorSampleBatch: ...

    def product_version(self, owned: Any) -> str: ...

    def settle(
        self,
        source: PreparedSavedFieldSource | None,
        owned: Any | None,
        acquisition: AcquisitionSettlement | None,
        causal_failure: Exception | None,
    ) -> ApplicationSettlement: ...

    def emit(self, event: str, fields: Mapping[str, object]) -> None: ...
