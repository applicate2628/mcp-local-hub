import pytest
from mcp.server.fastmcp.exceptions import ToolError

from mcphub_em_mcp.cst import mcp as cst_mcp
from mcphub_em_mcp.hfss import mcp as hfss_mcp


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("server", "expected"),
    [
        (hfss_mcp, {"hfss_solve", "hfss_export_mesh", "hfss_export_sparams"}),
        (cst_mcp, {"cst_solve", "cst_export_mesh", "cst_export_results"}),
    ],
)
async def test_tool_inventory_is_exact_mvp(server, expected: set[str]) -> None:
    tools = await server.list_tools()
    assert {tool.name for tool in tools} == expected


@pytest.mark.asyncio
async def test_solve_start_requires_positive_confirmation_after_validation(tmp_path) -> None:
    hfss_project = tmp_path / "model.aedt"
    hfss_project.write_bytes(b"fixture")
    cst_project = tmp_path / "model.cst"
    cst_project.write_bytes(b"fixture")
    with pytest.raises(ToolError, match="confirm=true"):
        await hfss_mcp.call_tool(
            "hfss_solve",
            {
                "action": "start",
                "project_path": str(hfss_project),
                "output_root": str(tmp_path),
                "confirm": False,
            },
        )
    with pytest.raises(ToolError, match="confirm=true"):
        await cst_mcp.call_tool(
            "cst_solve",
            {
                "action": "start",
                "project_path": str(cst_project),
                "output_root": str(tmp_path),
                "frequency_range_ghz": [1.0, 10.0],
                "frequency_samples_ghz": [1.0, 5.0, 10.0],
                "confirm": False,
            },
        )


@pytest.mark.asyncio
async def test_hfss_rejects_zero_adaptation_frequency(tmp_path) -> None:
    project = tmp_path / "model.aedt"
    project.write_bytes(b"fixture")
    with pytest.raises(ToolError, match="positive number"):
        await hfss_mcp.call_tool(
            "hfss_solve",
            {
                "project_path": str(project),
                "output_root": str(tmp_path),
                "adaptation_frequency": "0GHz",
                "confirm": True,
            },
        )
