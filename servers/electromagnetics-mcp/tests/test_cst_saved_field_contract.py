from __future__ import annotations

import ast
import hashlib
import importlib.util
import json
import math
from pathlib import Path

import pytest
from pydantic import ValidationError

EXISTING_CST_BODY_HASHES = {
    "_runner": "4824827d4cf1a0de12e82936701ce033f007103a510f0e684d1bb05c0164fa90",
    "cst_solve": "6f569bb6b4dcac92ed869150644f185b35320d9c3f88c1968ef1462c2d9c070c",
    "cst_export_mesh": "ff61bafc09fd2aaa3348dc1060124472a4bd4ff1dc623595530a4fe628454c09",
    "cst_export_results": "9c2a9428417941b890bd1a5c2810a5a60e6330e04e02204709f804f0e1880c05",
}
EXISTING_TOOL_SCHEMA_HASHES = {
    "hfss_export_mesh": "1b679fae82efddb4c4bc23a13397425c75cd99707a87046c987ea55028189822",
    "hfss_export_sparams": "09387c3aef2bfe742fac5b3789eb890951972c8bd3bc19153cac0dec39f46f2c",
    "hfss_solve": "ea92c1932d3617ccba7d7632bd080a06f77e176c8d4007c8436fae3f07061d97",
    "cst_export_mesh": "3bceb5355a44e3d76365bbdfa83df4e3d81259cf4cc9972698ed368606ff1eae",
    "cst_export_results": "8d096418c564189962342ccb08effe101167996f80fed0a74412ae4a6b095bee",
    "cst_solve": "f4a211e8a5a08751d1af59f5c523d73fb008f12267a6cbfc2e220163afcc293e",
}


def _core():
    module_name = "mcphub_em_mcp.cst_saved_field"
    assert importlib.util.find_spec(module_name) is not None
    return __import__(module_name, fromlist=["*"])


def _request_dict() -> dict[str, object]:
    return {
        "project_bundle": "retained/model.cst",
        "expected_project_sha256": None,
        "field": "E",
        "result": {
            "port": 1,
            "mode": 1,
            "frequency_hz": 3_000_000_000.0,
            "frequency_tolerance_hz": 0.0,
            "frame_selector": None,
            "expected_field_sha256": None,
            "expected_mesh_sha256": None,
            "adaptive_pass": None,
        },
        "points": [{"id": "p0", "xyz": [0.0, -0.0, 2.5]}],
        "coordinate_unit": "mm",
        "allow_solve": False,
        "max_points": 256,
    }


def _candidate(module, **overrides):
    values = {
        "field": "E",
        "port": 1,
        "mode": 1,
        "frequency_hz": 3_000_000_000.0,
        "frame_id": "#0003",
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
    values.update(overrides)
    return module.FieldFrameCandidate(**values)


def test_existing_six_semantic_schemas_and_cst_function_identities_are_frozen() -> None:
    from mcphub_em_mcp.cst import mcp as cst_mcp
    from mcphub_em_mcp.hfss import mcp as hfss_mcp

    cst_source = Path(__file__).parents[1] / "src" / "mcphub_em_mcp" / "cst.py"
    tree = ast.parse(cst_source.read_text(encoding="utf-8"))
    bodies = {
        node.name: hashlib.sha256(
            ast.dump(node, annotate_fields=True, include_attributes=False).encode("utf-8")
        ).hexdigest()
        for node in tree.body
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name in EXISTING_CST_BODY_HASHES
    }
    assert bodies == EXISTING_CST_BODY_HASHES

    async def schema_hashes():
        tools = [*(await hfss_mcp.list_tools()), *(await cst_mcp.list_tools())]
        return {
            tool.name: hashlib.sha256(
                json.dumps(tool.inputSchema, sort_keys=True, separators=(",", ":")).encode()
            ).hexdigest()
            for tool in tools
            if tool.name in EXISTING_TOOL_SCHEMA_HASHES
        }

    import anyio

    assert anyio.run(schema_hashes) == EXISTING_TOOL_SCHEMA_HASHES


def test_saved_field_wire_schema_v1() -> None:
    module = _core()
    request = module.SavedFieldRequestV1.model_validate(_request_dict())
    assert request.points[0].xyz == (0.0, -0.0, 2.5)
    schema = module.SavedFieldRequestV1.model_json_schema()
    assert schema["additionalProperties"] is False
    assert schema["$defs"]["SavedFieldResultRequestV1"]["additionalProperties"] is False
    assert schema["$defs"]["SavedFieldPointRequestV1"]["additionalProperties"] is False
    for bad_allow_solve in (True, None, 0, "false"):
        invalid = _request_dict()
        invalid["allow_solve"] = bad_allow_solve
        with pytest.raises(ValidationError):
            module.SavedFieldRequestV1.model_validate(invalid)


def test_saved_field_frame_resolution_table() -> None:
    module = _core()
    request = module.SavedFieldRequestV1.model_validate(_request_dict())
    exact = _candidate(module)
    other = _candidate(module, field="H", frame_id="#0001")
    assert module.resolve_frame([other, exact], request).candidate is exact
    assert module.resolve_frame([exact, other], request).candidate is exact
    tolerance = request.model_copy(
        update={
            "result": request.result.model_copy(
                update={"frequency_hz": 3_000_000_001.0, "frequency_tolerance_hz": 1.0}
            )
        }
    )
    assert module.resolve_frame([exact], tolerance).candidate is exact
    cases = [
        ([], "cst_saved_field.frame_missing"),
        ([exact, _candidate(module, frame_id="#0004")], "cst_saved_field.frame_ambiguous"),
    ]
    for candidates, failure_id in cases:
        with pytest.raises(module.SavedFieldFailure) as raised:
            module.resolve_frame(candidates, request)
        assert raised.value.failure_id == failure_id
    for update, failure_id in [
        ({"frame_selector": "#9999"}, "cst_saved_field.frame_selector_mismatch"),
        ({"adaptive_pass": "pass-5"}, "cst_saved_field.field_identity_mismatch"),
        ({"expected_field_sha256": "b" * 64}, "cst_saved_field.field_identity_mismatch"),
    ]:
        selected = request.model_copy(update={"result": request.result.model_copy(update=update)})
        with pytest.raises(module.SavedFieldFailure) as raised:
            module.resolve_frame([exact], selected)
        assert raised.value.failure_id == failure_id


def test_saved_field_component_order_and_zero_semantics() -> None:
    module = _core()
    point = module.SavedFieldPointRequestV1(id="p0", xyz=(1.0, 2.0, 3.0))
    zero = module.make_sample_vector(point, (-0.0, 0.0, -0.0, 0.0, 0.0, -0.0), -1)
    assert tuple(zero.as_wire())[2:8] == module.COMPONENT_ORDER
    assert zero.zero_ambiguous is True
    assert module.make_sample_vector(point, (0, 0, 0, 0, 0, 1e-300), -1).zero_ambiguous is False
    for bad in ((1.0,) * 5, (1.0, 2.0, 3.0, 4.0, 5.0, math.inf)):
        with pytest.raises(module.SavedFieldFailure):
            module.make_sample_vector(point, bad, -1)


@pytest.mark.parametrize(
    ("input_unit", "project_unit", "scale"),
    [("m", "mm", 1000.0), ("mm", "m", 0.001), ("m", "m", 1.0), ("mm", "mm", 1.0)],
)
def test_saved_field_unit_transform(input_unit: str, project_unit: str, scale: float) -> None:
    module = _core()
    transform = module.UnitTransform.resolve(input_unit, project_unit)
    assert transform.to_project((1.0, -2.0, 0.5)) == (
        1.0 * scale,
        -2.0 * scale,
        0.5 * scale,
    )
    with pytest.raises(module.SavedFieldFailure):
        module.UnitTransform.resolve(input_unit, "um")
