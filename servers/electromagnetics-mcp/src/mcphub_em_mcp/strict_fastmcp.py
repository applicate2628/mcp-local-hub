from __future__ import annotations

from typing import Any

from mcp.server.fastmcp import FastMCP
from mcp.server.fastmcp.utilities.func_metadata import ArgModelBase
from pydantic import ConfigDict


def strict_fastmcp(name: str, **kwargs: Any) -> FastMCP:
    """Create FastMCP after making its flat function-argument models closed."""
    config = dict(ArgModelBase.model_config)
    existing = config.get("extra")
    if existing not in {None, "forbid"}:
        raise RuntimeError(f"unsupported FastMCP argument extra policy: {existing!r}")
    config["extra"] = "forbid"
    ArgModelBase.model_config = ConfigDict(**config)
    return FastMCP(name, **kwargs)
