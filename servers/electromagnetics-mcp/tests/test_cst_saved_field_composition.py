from __future__ import annotations

import json

import pytest
from mcp.server.fastmcp.exceptions import ToolError

from mcphub_em_mcp import cst
from mcphub_em_mcp.cst_saved_field_policy import PolicyFailure
from mcphub_em_mcp.strict_fastmcp import strict_fastmcp


def _arguments(**changes):
    value = {
        "project_bundle": r"C:\approved\model.cst",
        "expected_project_sha256": None,
        "field": "E",
        "result": {
            "port": 1,
            "mode": 1,
            "frequency_hz": 3e9,
            "frequency_tolerance_hz": 0.0,
            "frame_selector": "frame-e1",
            "expected_field_sha256": None,
            "expected_mesh_sha256": None,
            "adaptive_pass": None,
        },
        "points": [{"id": "p1", "xyz": [1.0, 2.0, 3.0]}],
        "coordinate_unit": "mm",
        "allow_solve": False,
        "max_points": 1,
    }
    value.update(changes)
    return value


class _Snapshot:
    revision = "a" * 64

    def __init__(self, *, authorized: bool = True) -> None:
        self.authorized = authorized
        self.calls = 0

    def authorize(self, project_bundle: str):
        self.calls += 1
        if not self.authorized:
            raise PolicyFailure("cst_saved_field.not_authorized", "lexical_authority")
        return {"entry_id": "fixture", "project_bundle": project_bundle}


def _fresh_cst_server():
    server = strict_fastmcp("fresh-cst")
    for name in ("cst_solve", "cst_export_mesh", "cst_export_results"):
        tool = cst.mcp._tool_manager.get_tool(name)
        assert tool is not None
        server._tool_manager.add_tool(tool.fn, name=name, description=tool.description)
    return server


class _BrokerClient:
    def __init__(self, ready: bool) -> None:
        self.ready = ready

    def startup_ready(self) -> bool:
        return self.ready

    def bind_revision(self, revision: str) -> None:
        assert revision == "a" * 64


@pytest.mark.parametrize(
    ("snapshot", "broker"),
    [(None, _BrokerClient(True)), (object(), None), (object(), _BrokerClient(False))],
)
def test_fresh_composition_default_off_has_exact_baseline_catalogue(snapshot, broker) -> None:
    server = _fresh_cst_server()
    assert cst._compose_saved_field_tool(server, snapshot, broker) is False
    assert set(server._tool_manager._tools) == {
        "cst_solve",
        "cst_export_mesh",
        "cst_export_results",
    }


def test_fresh_enabled_composition_has_four_cst_and_seven_total_tools() -> None:
    from mcphub_em_mcp import hfss

    server = _fresh_cst_server()
    snapshot = _Snapshot()
    assert cst._compose_saved_field_tool(server, snapshot, _BrokerClient(True)) is True
    assert set(server._tool_manager._tools) == {
        "cst_solve",
        "cst_export_mesh",
        "cst_export_results",
        "cst_sample_saved_field",
    }
    assert len(server._tool_manager._tools) == 4
    assert len(hfss.mcp._tool_manager._tools) == 3
    assert len(server._tool_manager._tools) + len(hfss.mcp._tool_manager._tools) == 7


@pytest.mark.asyncio
async def test_policy_on_registration_is_one_additive_closed_tool() -> None:
    server = strict_fastmcp("fixture")
    snapshot = _Snapshot()
    entered = []
    cst._register_saved_field_tool(
        server,
        snapshot,
        lambda request, descriptor: (
            entered.append((request, descriptor)) or {"schema": "fixture", "ok": True}
        ),
    )
    assert set(server._tool_manager._tools) == {"cst_sample_saved_field"}
    schema = server._tool_manager._tools["cst_sample_saved_field"].parameters
    assert schema["additionalProperties"] is False
    assert schema["properties"]["result"]["additionalProperties"] is False
    assert schema["properties"]["points"]["items"]["additionalProperties"] is False
    result = await server.call_tool("cst_sample_saved_field", _arguments())
    assert result.isError is False
    assert result.structuredContent is None
    assert len(result.content) == 1
    assert json.loads(result.content[0].text) == {"ok": True, "schema": "fixture"}
    assert snapshot.calls == 1 and len(entered) == 1


@pytest.mark.asyncio
@pytest.mark.parametrize("allow_solve", [True, None, 0, "false"])
async def test_saved_field_framework_validation_boundary(allow_solve) -> None:
    server = strict_fastmcp("fixture")
    snapshot = _Snapshot()
    entered = []
    cst._register_saved_field_tool(server, snapshot, lambda *_: entered.append(True))
    with pytest.raises(ToolError) as raised:
        await server.call_tool("cst_sample_saved_field", _arguments(allow_solve=allow_solve))
    assert str(raised.value) == "cst_saved_field.invalid_request"
    assert snapshot.calls == 0 and entered == []
    from mcphub_em_mcp import hfss

    baselines = {
        "cst_solve": ("Error executing tool cst_solve:", "required for action=start"),
        "cst_export_mesh": ("Error executing tool cst_export_mesh:", "1 validation error"),
        "cst_export_results": (
            "Error executing tool cst_export_results:",
            "1 validation error",
        ),
        "hfss_solve": ("Error executing tool hfss_solve:", "required for action=start"),
        "hfss_export_sparams": (
            "Error executing tool hfss_export_sparams:",
            "1 validation error",
        ),
        "hfss_export_mesh": ("Error executing tool hfss_export_mesh:", "1 validation error"),
    }
    for name, required_parts in baselines.items():
        owner = cst.mcp if name.startswith("cst_") else hfss.mcp
        with pytest.raises(ToolError) as existing_error:
            await owner.call_tool(name, {})
        assert all(part in str(existing_error.value) for part in required_parts)
        assert "CANARY" not in str(existing_error.value)


@pytest.mark.asyncio
async def test_unknown_nested_field_is_fixed_and_pre_entry() -> None:
    server = strict_fastmcp("fixture")
    snapshot = _Snapshot()
    entered = []
    cst._register_saved_field_tool(server, snapshot, lambda *_: entered.append(True))
    arguments = _arguments()
    arguments["result"]["unknown"] = 1
    with pytest.raises(ToolError) as raised:
        await server.call_tool("cst_sample_saved_field", arguments)
    assert str(raised.value) == "cst_saved_field.invalid_request"
    assert snapshot.calls == 0 and entered == []


@pytest.mark.asyncio
async def test_unauthorized_call_has_zero_invoker_work_and_one_safe_text() -> None:
    server = strict_fastmcp("fixture")
    snapshot = _Snapshot(authorized=False)
    entered = []
    cst._register_saved_field_tool(server, snapshot, lambda *_: entered.append(True))
    result = await server.call_tool("cst_sample_saved_field", _arguments())
    assert result.isError is True
    assert result.structuredContent is None
    assert len(result.content) == 1
    assert result.content[0].text == "cst_saved_field.not_authorized"
    assert entered == []


def test_saved_field_mcp_result_boundary() -> None:
    exact = cst.publish_saved_field_text("x" * 1_048_576)
    assert len(exact.content) == 1
    assert len(exact.content[0].text.encode("utf-8")) == 1_048_576
    over = cst.publish_saved_field_text("x" * 1_048_577)
    assert over.isError is True
    assert len(over.content) == 1
    assert over.content[0].text == "cst_saved_field.response_too_large"
