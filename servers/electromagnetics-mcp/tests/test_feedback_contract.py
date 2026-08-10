from __future__ import annotations

import pytest
from mcp.server.fastmcp.exceptions import ToolError

import mcphub_em_mcp.cst as cst_module
import mcphub_em_mcp.hfss as hfss_module


async def _tool(server, name: str):
    return next(tool for tool in await server.list_tools() if tool.name == name)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("server", "name"),
    [
        (hfss_module.mcp, "hfss_solve"),
        (cst_module.mcp, "cst_solve"),
    ],
)
async def test_r1_solve_schema_is_closed_and_advertises_exact_actions(server, name: str) -> None:
    tool = await _tool(server, name)
    schema = tool.inputSchema
    assert schema["additionalProperties"] is False
    assert schema["properties"]["action"]["enum"] == [
        "start",
        "status",
        "result",
        "cancel",
        "preflight",
    ]
    assert "start, status, result, cancel, or preflight" in tool.description
    maximum_passes = schema["properties"]["maximum_passes"]
    assert maximum_passes["minimum"] == 1
    assert maximum_passes["maximum"] == 100


@pytest.mark.asyncio
async def test_r1_cst_schema_declares_two_positive_frequency_bounds() -> None:
    tool = await _tool(cst_module.mcp, "cst_solve")
    branches = tool.inputSchema["properties"]["frequency_range_ghz"]["anyOf"]
    array_schema = next(branch for branch in branches if branch.get("type") == "array")
    assert array_schema["minItems"] == 2
    assert array_schema["maxItems"] == 2
    assert array_schema["items"]["exclusiveMinimum"] == 0


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("server", "name"),
    [
        (hfss_module.mcp, "hfss_solve"),
        (cst_module.mcp, "cst_solve"),
    ],
)
async def test_r1_unknown_solve_argument_is_rejected_before_job_lookup(server, name: str) -> None:
    with pytest.raises(ToolError, match="Extra inputs are not permitted"):
        await server.call_tool(
            name,
            {"action": "status", "job_id": "qa-nonexistent", "qa_unrecognized_parameter": 1},
        )


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("server", "name"),
    [
        (hfss_module.mcp, "hfss_solve"),
        (cst_module.mcp, "cst_solve"),
    ],
)
async def test_r1_invalid_pass_bound_precedes_start_confirmation(server, name: str) -> None:
    with pytest.raises(ToolError, match="greater than or equal to 1"):
        await server.call_tool(
            name,
            {"action": "start", "maximum_passes": 0, "confirm": False},
        )


def test_r1_hfss_preflight_validates_without_submitting_job(tmp_path, monkeypatch) -> None:
    project = tmp_path / "model.aedt"
    project.write_bytes(b"fixture")

    def forbidden_start(**_kwargs):
        raise AssertionError("preflight submitted a job")

    monkeypatch.setattr(hfss_module._jobs, "start", forbidden_start)
    result = hfss_module.hfss_solve(
        action="preflight",
        project_path=str(project),
        output_root=str(tmp_path),
        confirm=False,
    )
    assert result["valid"] is True
    assert result["action"] == "preflight"


def test_r1_cst_preflight_validates_cross_fields_without_submitting_job(tmp_path, monkeypatch) -> None:
    project = tmp_path / "model.cst"
    project.write_bytes(b"fixture")

    def forbidden_start(**_kwargs):
        raise AssertionError("preflight submitted a job")

    monkeypatch.setattr(cst_module._jobs, "start", forbidden_start)
    result = cst_module.cst_solve(
        action="preflight",
        project_path=str(project),
        output_root=str(tmp_path),
        frequency_range_ghz=[1.0, 10.0],
        frequency_samples_ghz=[1.0, 5.0, 10.0],
        confirm=False,
    )
    assert result["valid"] is True
    assert result["action"] == "preflight"


def test_r1_cst_preflight_rejects_cross_field_mismatch_before_confirmation(tmp_path) -> None:
    project = tmp_path / "model.cst"
    project.write_bytes(b"fixture")
    with pytest.raises(ValueError, match="inside frequency_range_ghz"):
        cst_module.cst_solve(
            action="preflight",
            project_path=str(project),
            output_root=str(tmp_path),
            adaptation_frequency_ghz=5.0,
            frequency_range_ghz=[1.0, 10.0],
            frequency_samples_ghz=[5.0, 20.0],
            confirm=False,
        )
