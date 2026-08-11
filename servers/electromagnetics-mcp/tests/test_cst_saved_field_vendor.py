from __future__ import annotations

import hashlib
import importlib.util
import math
from pathlib import Path

import pytest


def _vendor():
    name = "mcphub_em_mcp.cst_saved_field_vendor"
    assert importlib.util.find_spec(name) is not None, "P07 concrete vendor adapter is missing"
    return __import__(name, fromlist=["*"])


def _raw_candidate(**changes: object) -> dict[str, object]:
    value: dict[str, object] = {
        "field": "E",
        "port": 1,
        "mode": 1,
        "frequency_hz": 3_000_000_000.0,
        "frame_id": "frame-e1",
        "tree_path": "2D/3D Results/E-Field/e1",
        "payload_relative": "Result/saved/e1.sct",
        "adaptive_pass": "pass-4",
        "project_unit": "mm",
        "field_unit": "V/m",
        "time_dependence": "exp(+jwt)",
        "time_dependence_status": "verified",
        "field_sha256": "a" * 64,
        "initial_frequency_hz": 3_000_000_000.0,
        "post_registration_frequency_hz": 3_000_000_000.0,
        "activation_type": "Efield3D",
        "status_policy": {-1: True},
    }
    value.update(changes)
    return value


@pytest.mark.parametrize(
    ("mutation", "value"),
    [
        ("missing", None),
        ("extra", None),
        ("port", True),
        ("frequency_hz", math.inf),
        ("field", "J"),
        ("payload_relative", "../source.sct"),
        ("payload_relative", "Result/saved/e1.sct:secret"),
        ("frame_id", r"C:\\CANARY\\frame"),
        ("adaptive_pass", "../CANARY"),
        ("field_sha256", "not-a-hash"),
        ("activation_type", "Hfield3D"),
        ("status_policy", {-1: "yes"}),
        ("time_dependence_status", "unknown"),
        ("time_dependence", "unknown"),
        ("post_registration_frequency_hz", 3_000_000_001.0),
        ("field_unit", "x" * 4097),
    ],
)
def test_saved_field_vendor_record_validation(mutation: str, value: object) -> None:
    vendor = _vendor()
    record = _raw_candidate()
    if mutation == "missing":
        record.pop("frame_id")
    elif mutation == "extra":
        record["unexpected"] = "value"
    else:
        record[mutation] = value
    selected: list[str] = []
    with pytest.raises(vendor.VendorFailure) as raised:
        vendor.validate_candidate_records([record], on_validated=selected.append)
    assert raised.value.failure_id == "cst_saved_field.vendor_record_invalid"
    assert selected == []


def test_vendor_candidate_count_budget_is_atomic_exact_and_one_over() -> None:
    vendor = _vendor()
    budget = vendor.VendorRecordBudget(max_candidates=4, max_metadata_bytes=100_000)
    exact = [_raw_candidate(frame_id=f"frame-{index}") for index in range(4)]
    assert len(vendor.validate_candidate_records(exact, budget=budget)) == 4
    selected: list[str] = []
    with pytest.raises(vendor.VendorFailure) as raised:
        vendor.validate_candidate_records(
            [*exact, _raw_candidate(frame_id="frame-4")],
            budget=budget,
            on_validated=selected.append,
        )
    assert raised.value.failure_id == "cst_saved_field.resource_limit_exceeded"
    assert selected == []


def test_vendor_candidate_real_4096_exact_and_one_over() -> None:
    vendor = _vendor()
    exact = [_raw_candidate(frame_id=f"frame-{index}") for index in range(4_096)]
    assert len(vendor.validate_candidate_records(exact)) == 4_096
    with pytest.raises(vendor.VendorFailure) as raised:
        vendor.validate_candidate_records([*exact, _raw_candidate(frame_id="overflow")])
    assert raised.value.failure_id == "cst_saved_field.resource_limit_exceeded"


def test_vendor_metadata_budget_is_atomic_exact_and_one_over() -> None:
    vendor = _vendor()
    record = _raw_candidate()
    measured = vendor.validated_metadata_size(record)
    assert vendor.validate_candidate_records([record], budget=vendor.VendorRecordBudget(4_096, measured))
    with pytest.raises(vendor.VendorFailure) as raised:
        vendor.validate_candidate_records([record], budget=vendor.VendorRecordBudget(4_096, measured - 1))
    assert raised.value.failure_id == "cst_saved_field.resource_limit_exceeded"


def test_vendor_real_8_mib_metadata_exact_and_one_over() -> None:
    vendor = _vendor()
    records = [_raw_candidate(frame_id=f"f{index}") for index in range(4_096)]
    target = 8 * 1024 * 1024
    remaining = target - sum(vendor.validated_metadata_size(record) for record in records)
    assert 0 < remaining < 4_096 * 4_000
    for record in records:
        if remaining == 0:
            break
        original = str(record["tree_path"])
        addition = min(remaining, 4_096 - len(original))
        record["tree_path"] = original + "t" * addition
        remaining -= addition
    assert remaining == 0
    assert sum(vendor.validated_metadata_size(record) for record in records) == target
    assert len(vendor.validate_candidate_records(records)) == 4_096
    over = [dict(record) for record in records]
    over[-1]["tree_path"] = str(over[-1]["tree_path"]) + "x"
    with pytest.raises(vendor.VendorFailure) as raised:
        vendor.validate_candidate_records(over)
    assert raised.value.failure_id == "cst_saved_field.resource_limit_exceeded"


class _CreatedResource:
    def __init__(self, trace: list[str], foreign: set[int]) -> None:
        self.handle = object()
        self.ownership_token = "owned"
        self.process_identity = 200
        self.trace = trace
        self.foreign = foreign
        self.closed = False

    def is_live(self) -> bool:
        self.trace.append("owned_is_live")
        return not self.closed

    def clear_geometry_data_cache(self) -> None:
        self.trace.append("clear_geometry_data_cache")

    def close_without_save(self) -> None:
        self.trace.append("close_without_save:200")
        self.closed = True

    def is_absent(self) -> bool:
        self.trace.append("owned_is_absent:200")
        return self.closed


def test_saved_field_owned_session_identity(tmp_path: Path) -> None:
    vendor = _vendor()
    trace: list[str] = []
    foreign = {100, 300}
    resource = _CreatedResource(trace, foreign)
    owned, receipt = vendor.open_owned_sampler_session(lambda _: resource, tmp_path / "copy.cst")
    assert owned.handle is resource.handle
    assert owned.process_identity == 200
    assert receipt.transfer_committed is True
    assert foreign == {100, 300}


@pytest.mark.parametrize("boundary", ["handle", "identity", "liveness", "token", "before_transfer"])
def test_saved_field_partial_acquisition_transaction(tmp_path: Path, boundary: str) -> None:
    vendor = _vendor()
    trace: list[str] = []
    foreign = {100, 300}
    created = _CreatedResource(trace, foreign)
    if boundary == "handle":
        created.handle = None
    elif boundary == "identity":
        created.process_identity = None
    elif boundary == "liveness":
        created.is_live = lambda: False  # type: ignore[method-assign]
    elif boundary == "token":
        created.ownership_token = ""

    def before_transfer(_owned) -> None:
        if boundary == "before_transfer":
            raise RuntimeError("injected boundary")

    with pytest.raises(vendor.OwnedSessionAcquisitionError) as raised:
        vendor.open_owned_sampler_session(
            lambda _: created,
            tmp_path / "copy.cst",
            before_transfer=before_transfer,
        )
    receipt = raised.value.settlement
    assert receipt.transfer_committed is False
    assert receipt.handles_received == 1
    assert receipt.close_attempts == 1
    assert receipt.close_succeeded is True
    assert receipt.absence_proven is True
    assert receipt.safely_attributed_remaining == 0
    assert trace.count("close_without_save:200") == 1
    assert trace.count("owned_is_absent:200") == 1
    assert foreign == {100, 300}


def test_raise_before_handle_requires_zero_creation_or_direct_rollback_proof(tmp_path: Path) -> None:
    vendor = _vendor()

    def create(_path):
        raise vendor.VendorCreateError("private create failure")

    with pytest.raises(vendor.OwnedSessionAcquisitionError) as raised:
        vendor.open_owned_sampler_session(create, tmp_path / "copy.cst")
    assert raised.value.failure.failure_id == "cst_saved_field.session_settle_failed"
    assert raised.value.settlement.handles_received == 1
    assert raised.value.settlement.safely_attributed_remaining == 1


class _ActivationAPI:
    def __init__(self, trace: list[str]) -> None:
        self.trace = trace
        self.solve = 0
        self.save_project = 0
        self.remesh = 0
        self.fallback = 0
        self.handwritten_header = 0

    def open_result3d(self, handle: object, payload: Path) -> object:
        self.trace.append("result3d_open")
        assert handle is not None and payload.read_bytes() == b"copied-field"
        return object()

    def result3d_frequency(self, _result3d: object) -> float:
        self.trace.append("result3d_frequency")
        return 3_000_000_000.0

    def result3d_metadata(self, _result3d: object) -> dict[str, str]:
        self.trace.append("result3d_metadata")
        return {
            "project_unit": "mm",
            "field_unit": "V/m",
            "time_dependence": "exp(+jwt)",
            "time_dependence_status": "verified",
        }

    def save_result3d_generated_header(self, _result3d: object, path: Path) -> None:
        self.trace.append("result3d_generated_header")
        path.write_bytes(b"vendor-generated-header")

    def register_result_tree(self, _handle: object, activation_type: str, payload: Path, header: Path) -> str:
        self.trace.append(f"result_tree_register:{activation_type}")
        assert payload.read_bytes() == b"copied-field"
        assert header.read_bytes() == b"vendor-generated-header"
        return "tree-item"

    def select_result_tree_item(self, _handle: object, item: str) -> None:
        self.trace.append("result_tree_select")
        assert item == "tree-item"

    def result_tree_frequency(self, _handle: object, item: str) -> float:
        self.trace.append("result_tree_frequency")
        assert item == "tree-item"
        return 3_000_000_000.0

    def get_field_vector(self, _handle: object, _item: str, xyz: tuple[float, float, float]):
        self.trace.append(f"get_field_vector:{xyz[0]},{xyz[1]},{xyz[2]}")
        return (1.0, 2.0, 3.0, 4.0, 5.0, 6.0), -1


class _PathLease:
    def __init__(self, root: Path, *, corrupt_clean: bool = False) -> None:
        self.root = root
        self.corrupt_clean = corrupt_clean

    def _path(self, relative: str) -> Path:
        return self.root.joinpath(*relative.split("/"))

    def hold_ancestor(self, relative: str):
        path = self._path(relative)
        path.mkdir(parents=True, exist_ok=True)
        return path

    def hold_read_input(self, relative: str):
        return self._path(relative)

    def prepare_output(self, relative: str):
        path = self._path(relative)
        path.parent.mkdir(parents=True, exist_ok=True)
        return path

    def seal_output(self, relative: str):
        from mcphub_em_mcp.cst_saved_field_port import SealedVendorOutput

        path = self._path(relative)
        return SealedVendorOutput(str(path), hashlib.sha256(path.read_bytes()).hexdigest())

    def create_clean_input(self, source_relative: str, destination_relative: str, expected: str):
        source = self._path(source_relative)
        destination = self._path(destination_relative)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(b"corrupt-field" if self.corrupt_clean else source.read_bytes())
        if hashlib.sha256(destination.read_bytes()).hexdigest() != expected:
            from mcphub_em_mcp.cst_saved_field_vendor import VendorFailure

            raise VendorFailure("cst_saved_field.source_changed", "copy_field", "clean input hash mismatch")
        return destination

    def revalidate_all(self):
        return None

    def settle(self):
        raise AssertionError("application owner settles the lease")


def test_saved_field_vendor_call_order(tmp_path: Path) -> None:
    vendor = _vendor()
    trace: list[str] = []
    api = _ActivationAPI(trace)
    payload = tmp_path / "workspace" / "model" / "Result" / "saved" / "e1.sct"
    payload.parent.mkdir(parents=True)
    payload.write_bytes(b"copied-field")
    result = vendor.activate_and_sample(
        api=api,
        handle=object(),
        copied_payload_relative="model/Result/saved/e1.sct",
        activation_type="Efield3D",
        expected_frequency_hz=3_000_000_000.0,
        frequency_tolerance_hz=0.0,
        points_project=((1.0, 2.0, 3.0),),
        status_policy={-1: True},
        expected_field_sha256=hashlib.sha256(b"copied-field").hexdigest(),
        vendor_path_lease=_PathLease(tmp_path / "workspace"),
    )
    assert result.rows == (((1.0, 2.0, 3.0, 4.0, 5.0, 6.0), -1),)
    assert trace == [
        "result3d_open",
        "result3d_frequency",
        "result3d_metadata",
        "result3d_generated_header",
        "result_tree_register:Efield3D",
        "result_tree_select",
        "result_tree_frequency",
        "get_field_vector:1.0,2.0,3.0",
    ]
    assert (
        api.solve,
        api.save_project,
        api.remesh,
        api.fallback,
        api.handwritten_header,
    ) == (0, 0, 0, 0, 0)


@pytest.mark.parametrize(
    ("case", "failure_id"),
    [
        ("wrong_arity", "cst_saved_field.point_sample_failed"),
        ("nonfinite", "cst_saved_field.point_sample_failed"),
        ("boolean", "cst_saved_field.point_sample_failed"),
        ("status", "cst_saved_field.vendor_status_unverified"),
        ("frequency", "cst_saved_field.activation_failed"),
        ("time", "cst_saved_field.metadata_unavailable"),
    ],
)
def test_activation_rejects_unverified_vendor_results(tmp_path: Path, case: str, failure_id: str) -> None:
    vendor = _vendor()

    class InvalidAPI(_ActivationAPI):
        def result3d_metadata(self, result3d: object) -> dict[str, str]:
            metadata = super().result3d_metadata(result3d)
            if case == "time":
                metadata["time_dependence_status"] = "unknown"
            return metadata

        def result_tree_frequency(self, handle: object, item: str) -> float:
            value = super().result_tree_frequency(handle, item)
            return value + 1.0 if case == "frequency" else value

        def get_field_vector(self, handle: object, item: str, xyz: tuple[float, float, float]):
            components, status = super().get_field_vector(handle, item, xyz)
            if case == "wrong_arity":
                components = components[:5]
            elif case == "nonfinite":
                components = (*components[:5], math.nan)
            elif case == "boolean":
                components = (True, *components[1:])
            elif case == "status":
                status = 7
            return components, status

    payload = tmp_path / "workspace" / "model" / "Result" / "saved" / "e1.sct"
    payload.parent.mkdir(parents=True)
    payload.write_bytes(b"copied-field")
    with pytest.raises(vendor.VendorFailure) as raised:
        vendor.activate_and_sample(
            api=InvalidAPI([]),
            handle=object(),
            copied_payload_relative="model/Result/saved/e1.sct",
            activation_type="Efield3D",
            expected_frequency_hz=3_000_000_000.0,
            frequency_tolerance_hz=0.0,
            points_project=((1.0, 2.0, 3.0),),
            status_policy={-1: True},
            expected_field_sha256=hashlib.sha256(b"copied-field").hexdigest(),
            vendor_path_lease=_PathLease(tmp_path / "workspace"),
        )
    assert raised.value.failure_id == failure_id


def test_selected_field_copy_corruption_fails_at_copy_field_before_registration(
    tmp_path: Path,
) -> None:
    vendor = _vendor()
    trace: list[str] = []
    api = _ActivationAPI(trace)
    payload = tmp_path / "workspace" / "model" / "Result" / "saved" / "e1.sct"
    payload.parent.mkdir(parents=True)
    payload.write_bytes(b"copied-field")

    with pytest.raises(vendor.VendorFailure) as raised:
        vendor.activate_and_sample(
            api=api,
            handle=object(),
            copied_payload_relative="model/Result/saved/e1.sct",
            activation_type="Efield3D",
            expected_frequency_hz=3_000_000_000.0,
            frequency_tolerance_hz=0.0,
            points_project=((1.0, 2.0, 3.0),),
            status_policy={-1: True},
            expected_field_sha256=hashlib.sha256(b"copied-field").hexdigest(),
            vendor_path_lease=_PathLease(tmp_path / "workspace", corrupt_clean=True),
        )
    assert raised.value.failure_id == "cst_saved_field.source_changed"
    assert raised.value.stage == "copy_field"
    assert not any(item.startswith("result_tree_register") for item in trace)
