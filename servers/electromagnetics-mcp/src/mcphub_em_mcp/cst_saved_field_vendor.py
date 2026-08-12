"""Concrete CST adapter primitives for the saved-field sampler.

This module contains no server composition.  It validates the complete vendor
record set before exposing candidates and owns the exact session transfer and
Result3D activation protocols.
"""

from __future__ import annotations

import math
import re
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import PurePosixPath
from typing import Any, Literal

from .cst_saved_field_port import (
    AcquisitionSettlement,
    AuthorizedVendorPathLease,
    FieldFrameCandidate,
    OwnedSessionAcquisitionError,
    VendorFailure,
    VendorSampleBatch,
    validate_vendor_relative_path,
)

_PUBLIC_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")


@dataclass(frozen=True, slots=True)
class VendorRecordBudget:
    max_candidates: int = 4096
    max_metadata_bytes: int = 8 * 1024 * 1024


DEFAULT_VENDOR_RECORD_BUDGET = VendorRecordBudget()


class VendorCreateError(RuntimeError):
    def __init__(
        self,
        message: str,
        *,
        verify_zero_created: Callable[[], bool] | None = None,
        rollback_receipt: AcquisitionSettlement | None = None,
    ) -> None:
        super().__init__(message)
        self.verify_zero_created = verify_zero_created
        self.rollback_receipt = rollback_receipt


@dataclass(frozen=True, slots=True)
class OwnedSamplerSession:
    _resource: Any

    @property
    def handle(self) -> Any:
        return self._resource.handle

    @property
    def process_identity(self) -> Any:
        return self._resource.process_identity

    def clear_geometry_data_cache(self) -> None:
        self._resource.clear_geometry_data_cache()

    def close_without_save(self) -> None:
        self._resource.close_without_save()

    def is_absent(self) -> bool:
        return bool(self._resource.is_absent())


_RECORD_KEYS = frozenset(
    {
        "field",
        "port",
        "mode",
        "frequency_hz",
        "frame_id",
        "tree_path",
        "payload_relative",
        "adaptive_pass",
        "project_unit",
        "field_unit",
        "time_dependence",
        "time_dependence_status",
        "field_sha256",
        "initial_frequency_hz",
        "post_registration_frequency_hz",
        "activation_type",
        "status_policy",
    }
)


def _metadata_atom_size(value: object) -> int:
    if isinstance(value, str):
        return len(value.encode("utf-8"))
    if isinstance(value, Mapping):
        return sum(_metadata_atom_size(key) + _metadata_atom_size(item) for key, item in value.items())
    return len(str(value).encode("ascii"))


def validated_metadata_size(record: Mapping[str, object]) -> int:
    """Return the bounded metadata accounting size for one raw record."""
    return sum(len(key.encode("utf-8")) + _metadata_atom_size(value) for key, value in record.items())


def _record_invalid(message: str) -> VendorFailure:
    return VendorFailure(
        "cst_saved_field.vendor_record_invalid",
        "candidate_inventory",
        message,
    )


def _required_text(record: Mapping[str, object], key: str, *, maximum: int = 4096) -> str:
    value = record[key]
    if not isinstance(value, str) or not value or len(value.encode("utf-8")) > maximum:
        raise _record_invalid("a required vendor text field is invalid")
    return value


def _positive_int(record: Mapping[str, object], key: str) -> int:
    value = record[key]
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        raise _record_invalid("a required vendor integer field is invalid")
    return value


def _finite_positive(record: Mapping[str, object], key: str) -> float:
    value = record[key]
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        raise _record_invalid("a required vendor numeric field is invalid")
    numeric = float(value)
    if not math.isfinite(numeric) or numeric <= 0:
        raise _record_invalid("a required vendor numeric field is invalid")
    return numeric


def _validate_record(record: object) -> FieldFrameCandidate:
    if not isinstance(record, Mapping) or set(record) != _RECORD_KEYS:
        raise _record_invalid("the vendor candidate record shape is not exact")
    field = record["field"]
    if field not in {"E", "H"}:
        raise _record_invalid("the vendor field kind is invalid")
    activation_type = record["activation_type"]
    expected_activation = "Efield3D" if field == "E" else "Hfield3D"
    if activation_type != expected_activation:
        raise _record_invalid("the field kind and activation type do not agree")
    payload_relative = _required_text(record, "payload_relative")
    payload_parts = PurePosixPath(payload_relative).parts
    if (
        PurePosixPath(payload_relative).is_absolute()
        or not payload_parts
        or any(part in {"", ".", ".."} for part in payload_parts)
        or "\\" in payload_relative
    ):
        raise _record_invalid("the vendor payload path is not a contained relative path")
    try:
        validate_vendor_relative_path(payload_relative)
    except ValueError as exc:
        raise _record_invalid("the vendor payload namespace is invalid") from exc
    sha256 = _required_text(record, "field_sha256", maximum=64)
    if len(sha256) != 64 or any(character not in "0123456789abcdefABCDEF" for character in sha256):
        raise _record_invalid("the vendor field digest is invalid")
    adaptive_pass = record["adaptive_pass"]
    if adaptive_pass is not None and (
        not isinstance(adaptive_pass, str) or _PUBLIC_ID_RE.fullmatch(adaptive_pass) is None
    ):
        raise _record_invalid("the vendor adaptive pass is invalid")
    status_policy = record["status_policy"]
    if (
        not isinstance(status_policy, Mapping)
        or not status_policy
        or any(
            not isinstance(key, int) or isinstance(key, bool) or type(value) is not bool
            for key, value in status_policy.items()
        )
    ):
        raise _record_invalid("the vendor status policy is invalid")
    time_status = _required_text(record, "time_dependence_status")
    if time_status != "verified":
        raise _record_invalid("the vendor time-dependence status is not verified")
    time_dependence = _required_text(record, "time_dependence")
    if time_dependence not in {"exp(+jwt)", "exp(-jwt)"}:
        raise _record_invalid("the vendor time-dependence convention is unsupported")
    frequency_hz = _finite_positive(record, "frequency_hz")
    initial_frequency_hz = _finite_positive(record, "initial_frequency_hz")
    post_registration_frequency_hz = _finite_positive(record, "post_registration_frequency_hz")
    if frequency_hz != initial_frequency_hz or frequency_hz != post_registration_frequency_hz:
        raise _record_invalid("the vendor frequency identities do not agree")
    frame_id = _required_text(record, "frame_id")
    if _PUBLIC_ID_RE.fullmatch(frame_id) is None:
        raise _record_invalid("the public frame identifier is invalid")
    return FieldFrameCandidate(
        field=field,  # type: ignore[arg-type]
        port=_positive_int(record, "port"),
        mode=_positive_int(record, "mode"),
        frequency_hz=frequency_hz,
        frame_id=frame_id,
        tree_path=_required_text(record, "tree_path"),
        payload_relative=payload_relative,
        adaptive_pass=adaptive_pass,
        project_unit=_required_text(record, "project_unit"),
        field_unit=_required_text(record, "field_unit"),
        time_dependence=time_dependence,
        time_dependence_status=time_status,
        field_sha256=sha256.lower(),
        initial_frequency_hz=initial_frequency_hz,
        post_registration_frequency_hz=post_registration_frequency_hz,
        activation_type=activation_type,  # type: ignore[arg-type]
        status_policy=dict(status_policy),
    )


def validate_candidate_records(
    records: Sequence[object],
    *,
    budget: VendorRecordBudget = DEFAULT_VENDOR_RECORD_BUDGET,
    on_validated: Callable[[str], None] | None = None,
) -> tuple[FieldFrameCandidate, ...]:
    if (
        not isinstance(budget.max_candidates, int)
        or isinstance(budget.max_candidates, bool)
        or budget.max_candidates < 1
        or not isinstance(budget.max_metadata_bytes, int)
        or isinstance(budget.max_metadata_bytes, bool)
        or budget.max_metadata_bytes < 1
    ):
        raise _record_invalid("the vendor record budget is invalid")
    if len(records) > budget.max_candidates:
        raise VendorFailure(
            "cst_saved_field.resource_limit_exceeded",
            "candidate_inventory",
            "the vendor candidate count exceeds the configured limit",
        )
    total_size = 0
    validated: list[FieldFrameCandidate] = []
    for raw in records:
        candidate = _validate_record(raw)
        total_size += validated_metadata_size(raw)  # type: ignore[arg-type]
        if total_size > budget.max_metadata_bytes:
            raise VendorFailure(
                "cst_saved_field.resource_limit_exceeded",
                "candidate_inventory",
                "the vendor candidate metadata exceeds the configured limit",
            )
        validated.append(candidate)
    result = tuple(validated)
    if on_validated is not None:
        for candidate in result:
            on_validated(candidate.frame_id)
    return result


def _settlement(
    *,
    stage: str,
    transfer_committed: bool,
    handles_received: int,
    close_attempts: int = 0,
    close_succeeded: bool = False,
    absence_proven: bool = False,
) -> AcquisitionSettlement:
    remaining = 0 if transfer_committed or (close_succeeded and absence_proven) else handles_received
    return AcquisitionSettlement(
        stage=stage,
        transfer_committed=transfer_committed,
        handles_received=handles_received,
        close_attempts=close_attempts,
        close_succeeded=close_succeeded,
        absence_proven=absence_proven,
        safely_attributed_remaining=remaining,
    )


def _raise_create_failure(exc: BaseException) -> None:
    receipt: AcquisitionSettlement | None = None
    if isinstance(exc, VendorCreateError):
        if exc.rollback_receipt is not None:
            candidate = exc.rollback_receipt
            if (
                not candidate.transfer_committed
                and candidate.close_attempts == 1
                and candidate.close_succeeded
                and candidate.absence_proven
                and candidate.safely_attributed_remaining == 0
            ):
                receipt = candidate
        elif exc.verify_zero_created is not None:
            try:
                zero_created = exc.verify_zero_created() is True
            except BaseException:
                zero_created = False
            if zero_created:
                receipt = _settlement(
                    stage="session_create",
                    transfer_committed=False,
                    handles_received=0,
                    absence_proven=True,
                )
    proved_safe = receipt is not None
    failure = VendorFailure(
        "cst_saved_field.cst_unavailable" if proved_safe else "cst_saved_field.session_settle_failed",
        "session_create",
        (
            "the vendor session could not be created"
            if proved_safe
            else "vendor creation did not prove zero creation or direct rollback"
        ),
    )
    raise OwnedSessionAcquisitionError(
        failure,
        receipt or _settlement(stage="session_create", transfer_committed=False, handles_received=1),
    ) from exc


def _rollback_resource(resource: Any, stage: str) -> None:
    close_succeeded = False
    absence_proven = False
    cause: BaseException | None = None
    try:
        resource.close_without_save()
        close_succeeded = True
    except Exception as exc:
        cause = exc
    try:
        absence_proven = bool(resource.is_absent())
    except Exception as exc:
        cause = cause or exc
    rollback_ok = close_succeeded and absence_proven
    failure = VendorFailure(
        "cst_saved_field.session_ownership_ambiguous"
        if rollback_ok
        else "cst_saved_field.session_settle_failed",
        stage,
        (
            "the incomplete vendor-owned session was rolled back"
            if rollback_ok
            else "the exact vendor-owned resource could not be proved absent"
        ),
    )
    raise OwnedSessionAcquisitionError(
        failure,
        _settlement(
            stage=stage,
            transfer_committed=False,
            handles_received=1,
            close_attempts=1,
            close_succeeded=close_succeeded,
            absence_proven=absence_proven,
        ),
    ) from cause


def open_owned_sampler_session(
    create_owned: Callable[[object], Any],
    copied_project: object,
    *,
    before_transfer: Callable[[OwnedSamplerSession], None] | None = None,
) -> tuple[OwnedSamplerSession, AcquisitionSettlement]:
    try:
        resource = create_owned(copied_project)
    except Exception as exc:
        _raise_create_failure(exc)
    stage = "session_handle"
    try:
        if getattr(resource, "handle", None) is None:
            raise ValueError("missing directly closable handle")
        stage = "session_identity"
        if getattr(resource, "process_identity", None) in {None, ""}:
            raise ValueError("missing exact process identity")
        stage = "session_liveness"
        if not bool(resource.is_live()):
            raise ValueError("owned resource is not live")
        stage = "session_token"
        if not getattr(resource, "ownership_token", None):
            raise ValueError("missing ownership token")
        owned = OwnedSamplerSession(resource)
        stage = "session_before_transfer"
        if before_transfer is not None:
            before_transfer(owned)
    except Exception:
        _rollback_resource(resource, stage)
    return owned, _settlement(stage="session_transfer", transfer_committed=True, handles_received=1)


def _require_frequency(actual: object, expected: float, tolerance: float, stage: str) -> float:
    if not isinstance(actual, (int, float)) or isinstance(actual, bool):
        raise VendorFailure("cst_saved_field.activation_failed", stage, "the vendor frequency is invalid")
    numeric = float(actual)
    if not math.isfinite(numeric) or abs(numeric - expected) > tolerance:
        raise VendorFailure(
            "cst_saved_field.activation_failed",
            stage,
            "the activated frame frequency does not match",
        )
    return numeric


def _require_metadata(value: object) -> Mapping[str, str]:
    if not isinstance(value, Mapping):
        raise VendorFailure(
            "cst_saved_field.metadata_unavailable",
            "result3d_metadata",
            "field metadata is unavailable",
        )
    required = ("project_unit", "field_unit", "time_dependence", "time_dependence_status")
    if any(not isinstance(value.get(key), str) or not value[key] for key in required):
        raise VendorFailure(
            "cst_saved_field.metadata_unavailable",
            "result3d_metadata",
            "required field metadata is unavailable",
        )
    if value["time_dependence_status"] != "verified":
        raise VendorFailure(
            "cst_saved_field.metadata_unavailable",
            "result3d_metadata",
            "the time-dependence convention is not verified",
        )
    return value  # type: ignore[return-value]


def activate_and_sample(
    *,
    api: Any,
    handle: Any,
    copied_payload_relative: str,
    activation_type: Literal["Efield3D", "Hfield3D"],
    expected_frequency_hz: float,
    frequency_tolerance_hz: float,
    points_project: Sequence[tuple[float, float, float]],
    status_policy: Mapping[int, bool],
    expected_field_sha256: str,
    check_deadline: Callable[[], None] = lambda: None,
    vendor_path_lease: AuthorizedVendorPathLease,
) -> VendorSampleBatch:
    def vendor_call(method, *args):
        check_deadline()
        vendor_path_lease.revalidate_all()
        value = method(*args)
        check_deadline()
        vendor_path_lease.revalidate_all()
        return value

    copied_payload = vendor_path_lease.hold_read_input(copied_payload_relative)
    result3d = vendor_call(api.open_result3d, handle, copied_payload)
    initial_frequency = _require_frequency(
        vendor_call(api.result3d_frequency, result3d),
        expected_frequency_hz,
        frequency_tolerance_hz,
        "result3d_frequency",
    )
    metadata = _require_metadata(vendor_call(api.result3d_metadata, result3d))
    check_deadline()
    vendor_path_lease.hold_ancestor("activation")
    generated_header = vendor_path_lease.prepare_output("activation/vendor_generated.rex")
    vendor_call(api.save_result3d_generated_header, result3d, generated_header)
    sealed_header = vendor_path_lease.seal_output("activation/vendor_generated.rex")
    if len(expected_field_sha256) != 64:
        raise VendorFailure(
            "cst_saved_field.source_changed",
            "copy_field",
            "the selected field identity changed before activation",
        )
    check_deadline()
    clean_payload = vendor_path_lease.create_clean_input(
        copied_payload_relative,
        "activation/field.sct",
        expected_field_sha256,
    )
    clean_header = vendor_path_lease.create_clean_input(
        "activation/vendor_generated.rex",
        "activation/field_sct.rex",
        sealed_header.sha256,
    )
    item = vendor_call(api.register_result_tree, handle, activation_type, clean_payload, clean_header)
    vendor_call(api.select_result_tree_item, handle, item)
    post_frequency = _require_frequency(
        vendor_call(api.result_tree_frequency, handle, item),
        expected_frequency_hz,
        frequency_tolerance_hz,
        "result_tree_frequency",
    )
    rows: list[tuple[tuple[float, float, float, float, float, float], int]] = []
    for xyz in points_project:
        components, raw_status = vendor_call(api.get_field_vector, handle, item, xyz)
        if (
            not isinstance(components, Sequence)
            or len(components) != 6
            or not all(
                isinstance(value, (int, float))
                and not isinstance(value, bool)
                and math.isfinite(float(value))
                for value in components
            )
        ):
            raise VendorFailure(
                "cst_saved_field.point_sample_failed",
                "get_field_vector",
                "the vendor point result must contain six finite components",
            )
        if (
            not isinstance(raw_status, int)
            or isinstance(raw_status, bool)
            or status_policy.get(raw_status) is not True
        ):
            raise VendorFailure(
                "cst_saved_field.vendor_status_unverified",
                "get_field_vector",
                "the vendor point status is not admitted",
            )
        rows.append((tuple(float(value) for value in components), raw_status))  # type: ignore[arg-type]
    return VendorSampleBatch(
        rows=tuple(rows),
        metadata=metadata,
        initial_frequency_hz=initial_frequency,
        post_registration_frequency_hz=post_frequency,
        activation_type=activation_type,
        generated_header=True,
    )
