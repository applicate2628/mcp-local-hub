from __future__ import annotations

from copy import deepcopy
from typing import Any

from mcp.server.fastmcp import FastMCP
from mcp.server.fastmcp.exceptions import ToolError
from mcp.server.fastmcp.tools.base import Tool
from mcp.server.fastmcp.utilities.func_metadata import ArgModelBase
from pydantic import ConfigDict, ValidationError

_FIXED_SAMPLER_VALIDATION_FAILURE = "cst_saved_field.invalid_request"


class _FixedValidationTool(Tool):
    async def run(self, arguments, context=None, convert_result=False):
        try:
            return await super().run(arguments, context, convert_result)
        except ToolError as exc:
            if isinstance(exc.__cause__, ValidationError):
                raise ToolError(_FIXED_SAMPLER_VALIDATION_FAILURE) from exc.__cause__
            raise


def strict_fastmcp(name: str, **kwargs: Any) -> FastMCP:
    """Create FastMCP after making its flat function-argument models closed."""
    config = dict(ArgModelBase.model_config)
    existing = config.get("extra")
    if existing not in {None, "forbid"}:
        raise RuntimeError(f"unsupported FastMCP argument extra policy: {existing!r}")
    config["extra"] = "forbid"
    ArgModelBase.model_config = ConfigDict(**config)
    return FastMCP(name, **kwargs)


def fix_tool_validation_error(server: FastMCP, tool_name: str) -> None:
    """Map only one tool's Pydantic pre-entry failures to its fixed public ID."""
    tool = server._tool_manager.get_tool(tool_name)  # noqa: SLF001
    if tool is None:
        raise RuntimeError(f"cannot configure unknown tool: {tool_name}")
    tool.__class__ = _FixedValidationTool


def publish_action_requirements(
    server: FastMCP,
    tool_name: str,
    *,
    routed_actions: tuple[str, ...],
    routed_required: tuple[str, ...],
    execution_required: tuple[str, ...],
    non_nullable_execution_fields: tuple[str, ...] = (),
) -> None:
    """Publish action-dependent required fields without changing the Python call model."""
    tool = server._tool_manager.get_tool(tool_name)  # noqa: SLF001
    if tool is None:
        raise RuntimeError(f"cannot configure unknown tool: {tool_name}")

    schema = deepcopy(tool.parameters)
    properties = schema.get("properties")
    if not isinstance(properties, dict):
        raise RuntimeError(f"{tool_name} has no object properties")
    if "allOf" in schema:
        raise RuntimeError(f"{tool_name} already declares conditional requirements")

    required_fields = {*routed_required, *execution_required}
    missing_fields = sorted(required_fields.difference(properties))
    if missing_fields:
        raise RuntimeError(f"{tool_name} requirement names unknown fields: {missing_fields}")

    action_schema = properties.get("action")
    if not isinstance(action_schema, dict) or not set(routed_actions).issubset(action_schema.get("enum", [])):
        raise RuntimeError(f"{tool_name} action schema does not contain every routed action")

    def non_null_property(field_name: str) -> dict[str, Any]:
        property_schema = properties.get(field_name)
        if not isinstance(property_schema, dict):
            raise RuntimeError(f"{tool_name}.{field_name} has no property schema")
        branches = property_schema.get("anyOf")
        if not isinstance(branches, list):
            raise RuntimeError(f"{tool_name}.{field_name} is not an optional schema")
        if not all(isinstance(branch, dict) for branch in branches):
            raise RuntimeError(f"{tool_name}.{field_name} has a non-object schema branch")
        non_null = [branch for branch in branches if branch.get("type") != "null"]
        nulls = [branch for branch in branches if branch.get("type") == "null"]
        if len(non_null) != 1 or len(nulls) != 1 or property_schema.get("default") is not None:
            raise RuntimeError(f"{tool_name}.{field_name} has an unsupported optional schema shape")
        replacement = deepcopy(non_null[0])
        for metadata_key in ("description", "title"):
            if metadata_key in property_schema:
                replacement[metadata_key] = property_schema[metadata_key]
        return replacement

    for field_name in non_nullable_execution_fields:
        replacement = non_null_property(field_name)
        properties[field_name] = replacement

    routed_properties = {field_name: non_null_property(field_name) for field_name in routed_required}
    execution_properties = {
        field_name: non_null_property(field_name)
        for field_name in execution_required
        if field_name not in non_nullable_execution_fields
    }
    schema["allOf"] = [
        {
            "if": {
                "properties": {"action": {"enum": list(routed_actions)}},
                "required": ["action"],
            },
            "then": {
                "properties": routed_properties,
                "required": list(routed_required),
            },
            "else": {
                "properties": execution_properties,
                "required": list(execution_required),
            },
        }
    ]
    tool.parameters = schema
