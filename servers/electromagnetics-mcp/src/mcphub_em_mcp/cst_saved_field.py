from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Annotated, Literal, TypeAlias

from pydantic import AfterValidator, BaseModel, BeforeValidator, ConfigDict, Field, field_validator

from .cst_saved_field_port import (
    AcquisitionSettlement,
    FieldFrameCandidate,
    OwnedSessionAcquisitionError,
    PreparedSavedFieldSource,
    SavedFieldApplicationPort,
    VendorFailure,
    VendorSampleBatch,
)

SHA256_HEX_LENGTH = 64
COMPONENT_ORDER = ("ReX", "ReY", "ReZ", "ImX", "ImY", "ImZ")
SavedFieldKind: TypeAlias = Literal["E", "H"]
CoordinateUnit: TypeAlias = Literal["m", "mm"]
FiniteNumber = Annotated[float, Field(allow_inf_nan=False)]
PositiveFiniteNumber = Annotated[float, Field(gt=0, allow_inf_nan=False)]
NonNegativeFiniteNumber = Annotated[float, Field(ge=0, allow_inf_nan=False)]
PositiveInteger = Annotated[int, Field(gt=0)]
MaxPoints = Annotated[int, Field(ge=1, le=256)]


class _ClosedModel(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True, frozen=True)


def _normalized_optional_sha256(value: str | None) -> str | None:
    if value is None:
        return None
    if len(value) != SHA256_HEX_LENGTH or any(
        character not in "0123456789abcdefABCDEF" for character in value
    ):
        raise ValueError("expected a null value or exactly 64 hexadecimal digits")
    return value.lower()


def _literal_false(value: object) -> Literal[False]:
    if type(value) is not bool or value is not False:
        raise ValueError("allow_solve must be the literal false")
    return False


OptionalSha256 = Annotated[str | None, AfterValidator(_normalized_optional_sha256)]
AllowSolveFalse = Annotated[Literal[False], BeforeValidator(_literal_false)]


class SavedFieldPointRequestV1(_ClosedModel):
    id: Annotated[str, Field(min_length=1, max_length=128)]
    xyz: tuple[FiniteNumber, FiniteNumber, FiniteNumber]

    @field_validator("xyz", mode="before")
    @classmethod
    def _json_array_to_tuple(cls, value: object) -> object:
        return tuple(value) if isinstance(value, list) else value


class SavedFieldResultRequestV1(_ClosedModel):
    port: PositiveInteger
    mode: PositiveInteger
    frequency_hz: PositiveFiniteNumber
    frequency_tolerance_hz: NonNegativeFiniteNumber
    frame_selector: Annotated[str, Field(min_length=1, max_length=1024)] | None
    expected_field_sha256: OptionalSha256
    expected_mesh_sha256: OptionalSha256
    adaptive_pass: Annotated[str, Field(min_length=1)] | None


SavedFieldPoints = Annotated[list[SavedFieldPointRequestV1], Field(min_length=1, max_length=256)]


class SavedFieldRequestV1(_ClosedModel):
    project_bundle: str
    expected_project_sha256: OptionalSha256
    field: SavedFieldKind
    result: SavedFieldResultRequestV1
    points: SavedFieldPoints
    coordinate_unit: CoordinateUnit
    allow_solve: AllowSolveFalse = False
    max_points: MaxPoints = 256


@dataclass
class SavedFieldFailure(Exception):
    failure_id: str
    stage: str
    safe_message: str
    causal_failure_id: str | None = None

    def __str__(self) -> str:
        return f"{self.failure_id}: {self.safe_message}"


@dataclass(frozen=True, slots=True)
class ResolvedFrame:
    candidate: FieldFrameCandidate


def _failure(failure_id: str, stage: str, safe_message: str) -> SavedFieldFailure:
    return SavedFieldFailure(failure_id=failure_id, stage=stage, safe_message=safe_message)


def resolve_frame(
    candidates: list[FieldFrameCandidate] | tuple[FieldFrameCandidate, ...],
    request: SavedFieldRequestV1,
) -> ResolvedFrame:
    result = request.result
    matches = [candidate for candidate in candidates if candidate.field == request.field]
    matches = [candidate for candidate in matches if candidate.port == result.port]
    matches = [candidate for candidate in matches if candidate.mode == result.mode]
    matches = [
        candidate
        for candidate in matches
        if abs(candidate.frequency_hz - result.frequency_hz) <= result.frequency_tolerance_hz
    ]
    if result.frame_selector is not None:
        matches = [
            candidate
            for candidate in matches
            if result.frame_selector in (candidate.frame_id, candidate.tree_path, candidate.payload_relative)
        ]
        if len(matches) != 1:
            raise _failure(
                "cst_saved_field.frame_selector_mismatch",
                "resolve_frame",
                "the exact frame selector did not identify one metadata candidate",
            )
    if result.adaptive_pass is not None:
        pass_matches = [candidate for candidate in matches if candidate.adaptive_pass == result.adaptive_pass]
        if len(pass_matches) != len(matches) or not pass_matches:
            raise _failure(
                "cst_saved_field.field_identity_mismatch",
                "resolve_frame",
                "the selected frame adaptive-pass identity does not match",
            )
        matches = pass_matches
    if result.expected_field_sha256 is not None:
        identity_matches = [
            candidate
            for candidate in matches
            if candidate.field_sha256.lower() == result.expected_field_sha256
        ]
        if len(identity_matches) != len(matches) or not identity_matches:
            raise _failure(
                "cst_saved_field.field_identity_mismatch",
                "resolve_frame",
                "the selected frame payload identity does not match",
            )
        matches = identity_matches
    if not matches:
        raise _failure(
            "cst_saved_field.frame_missing",
            "resolve_frame",
            "no saved field frame matches the requested metadata",
        )
    if len(matches) > 1:
        raise _failure(
            "cst_saved_field.frame_ambiguous",
            "resolve_frame",
            "more than one saved field frame matches the requested metadata",
        )
    return ResolvedFrame(candidate=matches[0])


@dataclass(frozen=True, slots=True)
class UnitTransform:
    input_unit: CoordinateUnit
    project_unit: CoordinateUnit
    scale: float

    @classmethod
    def resolve(cls, input_unit: str, project_unit: str) -> UnitTransform:
        scales = {
            ("m", "m"): 1.0,
            ("mm", "mm"): 1.0,
            ("m", "mm"): 1000.0,
            ("mm", "m"): 0.001,
        }
        try:
            scale = scales[(input_unit, project_unit)]
        except KeyError as exc:
            raise _failure(
                "cst_saved_field.metadata_unavailable",
                "unit_transform",
                "the project coordinate unit is unavailable or unsupported",
            ) from exc
        return cls(input_unit=input_unit, project_unit=project_unit, scale=scale)  # type: ignore[arg-type]

    def to_project(self, xyz: tuple[float, float, float]) -> tuple[float, float, float]:
        transformed = tuple(value * self.scale for value in xyz)
        if not all(math.isfinite(value) for value in transformed):
            raise _failure(
                "cst_saved_field.invalid_request",
                "unit_transform",
                "the transformed coordinate is not finite",
            )
        return transformed  # type: ignore[return-value]


@dataclass(frozen=True, slots=True)
class SampleVector:
    point_id: str
    xyz: tuple[float, float, float]
    components: tuple[float, float, float, float, float, float]
    vendor_status_raw: int
    zero_ambiguous: bool

    def as_wire(self) -> dict[str, object]:
        result: dict[str, object] = {"id": self.point_id, "xyz": list(self.xyz)}
        result.update(dict(zip(COMPONENT_ORDER, self.components, strict=True)))
        result["vendor_status_raw"] = self.vendor_status_raw
        result["zero_ambiguous"] = self.zero_ambiguous
        return result


def make_sample_vector(
    point: SavedFieldPointRequestV1,
    components: tuple[float, ...],
    vendor_status_raw: int,
) -> SampleVector:
    if len(components) != 6 or not all(math.isfinite(value) for value in components):
        raise _failure(
            "cst_saved_field.point_sample_failed",
            "sample_point",
            "the vendor point result must contain six finite components",
        )
    canonical = tuple(float(value) for value in components)
    return SampleVector(
        point_id=point.id,
        xyz=point.xyz,
        components=canonical,  # type: ignore[arg-type]
        vendor_status_raw=int(vendor_status_raw),
        zero_ambiguous=all(value == 0.0 for value in canonical),
    )


def _validate_semantic_request(request: SavedFieldRequestV1) -> None:
    identifiers = [point.id for point in request.points]
    if len(set(identifiers)) != len(identifiers) or len(request.points) > request.max_points:
        raise _failure(
            "cst_saved_field.invalid_request",
            "request_semantics",
            "point identifiers must be unique and fit the declared maximum",
        )


def _safe_product_version(value: object) -> str:
    if (
        not isinstance(value, str)
        or not value
        or len(value) > 256
        or any(ord(character) < 32 or ord(character) > 126 for character in value)
    ):
        raise _failure(
            "cst_saved_field.metadata_unavailable",
            "product_version",
            "the CST product version is unavailable",
        )
    return value


def _map_external_failure(value: Exception) -> SavedFieldFailure:
    if isinstance(value, OwnedSessionAcquisitionError):
        value = value.failure
    if isinstance(value, VendorFailure):
        return SavedFieldFailure(value.failure_id, value.stage, value.safe_message, value.causal_failure_id)
    if isinstance(value, SavedFieldFailure):
        return value
    return _failure(
        "cst_saved_field.activation_failed",
        "application",
        "the saved-field request failed unexpectedly",
    )


def _validate_batch(
    batch: VendorSampleBatch,
    frame: FieldFrameCandidate,
    requested_count: int,
) -> None:
    if len(batch.rows) != requested_count:
        raise _failure(
            "cst_saved_field.point_sample_failed",
            "sample_batch",
            "the vendor returned a different point count",
        )
    required = {
        "project_unit": frame.project_unit,
        "field_unit": frame.field_unit,
        "time_dependence": frame.time_dependence,
        "time_dependence_status": "verified",
    }
    if any(batch.metadata.get(key) != value for key, value in required.items()):
        raise _failure(
            "cst_saved_field.metadata_unavailable",
            "sample_batch",
            "the activated field metadata does not match the selected frame",
        )
    if (
        batch.activation_type != frame.activation_type
        or not batch.generated_header
        or batch.initial_frequency_hz != frame.initial_frequency_hz
        or batch.post_registration_frequency_hz != frame.post_registration_frequency_hz
    ):
        raise _failure(
            "cst_saved_field.activation_failed",
            "sample_batch",
            "the activation receipt does not match the selected frame",
        )


def _success_payload(
    request: SavedFieldRequestV1,
    source: PreparedSavedFieldSource,
    frame: FieldFrameCandidate,
    batch: VendorSampleBatch,
    product_version: str,
) -> dict[str, object]:
    points = [
        make_sample_vector(point, components, status).as_wire()
        for point, (components, status) in zip(request.points, batch.rows, strict=True)
    ]
    return {
        "schema": "mcphub.cst.saved_field_sample.v1",
        "solver": {"product": "CST Studio Suite", "version": product_version},
        "frame": {
            "field": frame.field,
            "port": frame.port,
            "mode": frame.mode,
            "frequency_hz": frame.frequency_hz,
            "frame_id": frame.frame_id,
            "adaptive_pass": frame.adaptive_pass,
            "field_sha256": frame.field_sha256,
        },
        "component_order": list(COMPONENT_ORDER),
        "points": points,
        "sampling": {
            "requested_point_count": len(request.points),
            "returned_point_count": len(points),
            "caller_max_points": request.max_points,
            "server_max_points": 256,
            "input_order_preserved": True,
            "interpolation": "none_by_mcphub",
        },
        "source_integrity": {
            "files": [
                {
                    "role": "project",
                    "relative_path": source.project_relative,
                    "sha256": source.project_sha256,
                },
                {"role": "mesh", "relative_path": source.mesh_relative, "sha256": source.mesh_sha256},
                {"role": "field", "relative_path": frame.payload_relative, "sha256": frame.field_sha256},
            ]
        },
        "lifecycle": {"owned_sessions_remaining": 0, "settled": True},
    }


def sample_saved_field(
    request: SavedFieldRequestV1,
    port: SavedFieldApplicationPort,
) -> dict[str, object]:
    """Execute one pure application transaction through an abstract runtime port."""
    _validate_semantic_request(request)
    source: PreparedSavedFieldSource | None = None
    owned: object | None = None
    acquisition: AcquisitionSettlement | None = None
    result: dict[str, object] | None = None
    failure: SavedFieldFailure | None = None
    try:
        source = port.prepare_source(request)
        if request.expected_project_sha256 is not None and (
            source.project_sha256.lower() != request.expected_project_sha256
        ):
            raise _failure(
                "cst_saved_field.source_identity_mismatch",
                "source_pre_hash",
                "the expected project identity does not match",
            )
        frame = resolve_frame(source.candidates, request).candidate
        if request.result.expected_mesh_sha256 is not None and (
            source.mesh_sha256.lower() != request.result.expected_mesh_sha256
        ):
            raise _failure(
                "cst_saved_field.source_identity_mismatch",
                "source_pre_hash",
                "the expected mesh identity does not match",
            )
        transform = UnitTransform.resolve(request.coordinate_unit, frame.project_unit)
        points_project = tuple(transform.to_project(point.xyz) for point in request.points)
        owned, acquisition = port.open_owned_session(source)
        if not acquisition.transfer_committed or acquisition.handles_received != 1:
            raise _failure(
                "cst_saved_field.session_ownership_ambiguous",
                "session_adopt",
                "the vendor acquisition receipt is incomplete",
            )
        batch = port.activate_and_sample(
            owned,
            source,
            frame,
            points_project,
            request.result.frequency_tolerance_hz,
        )
        _validate_batch(batch, frame, len(request.points))
        result = _success_payload(
            request, source, frame, batch, _safe_product_version(port.product_version(owned))
        )
    except Exception as exc:
        if isinstance(exc, OwnedSessionAcquisitionError):
            acquisition = exc.settlement
        failure = _map_external_failure(exc)
    settlement = port.settle(source, owned, acquisition, failure)
    port.emit(
        "cst_saved_field.session_settled",
        {
            "workspace_settled": settlement.workspace_settled,
            "session_settled": settlement.session_settled,
            "source_unchanged": settlement.source_unchanged,
            "owned_remaining": settlement.owned_remaining,
            "cache_cleared": settlement.cache_cleared,
            "closed_without_save": settlement.closed_without_save,
            "source_changed_role": settlement.source_changed_role,
            "acquisition": (
                None
                if settlement.acquisition is None
                else {
                    field: getattr(settlement.acquisition, field)
                    for field in settlement.acquisition.__dataclass_fields__
                }
            ),
        },
    )
    ownership_settled = (
        settlement.workspace_settled and settlement.session_settled and settlement.owned_remaining == 0
    )
    if ownership_settled and not settlement.source_unchanged:
        raise SavedFieldFailure(
            "cst_saved_field.source_changed",
            f"source_post_hash:{settlement.source_changed_role or 'unknown'}",
            "a monitored source identity changed during sampling",
            failure.failure_id if failure is not None else None,
        )
    if not settlement.complete:
        raise SavedFieldFailure(
            "cst_saved_field.session_settle_failed",
            "session_settle",
            "the saved-field application did not settle every owned resource",
            failure.failure_id if failure is not None else None,
        )
    if failure is not None:
        raise failure
    if result is None:
        raise _failure("cst_saved_field.activation_failed", "application", "the sampler produced no result")
    return result
