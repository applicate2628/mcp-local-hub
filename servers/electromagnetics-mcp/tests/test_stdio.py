from __future__ import annotations

import sys

import pytest
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("module", "expected"),
    [
        ("mcphub_em_mcp.hfss", {"hfss_solve", "hfss_export_mesh", "hfss_export_sparams"}),
        ("mcphub_em_mcp.cst", {"cst_solve", "cst_export_mesh", "cst_export_results"}),
    ],
)
async def test_real_stdio_handshake(module: str, expected: set[str]) -> None:
    server = StdioServerParameters(command=sys.executable, args=["-m", module])
    async with stdio_client(server) as (read, write), ClientSession(read, write) as session:
        await session.initialize()
        tools = await session.list_tools()
        assert {tool.name for tool in tools.tools} == expected
