"""Development-only, no-turn discovery through the public Codex Python SDK."""

from __future__ import annotations

from dataclasses import dataclass
import json
import math
import os
from pathlib import Path
import stat
from urllib.parse import urlsplit


@dataclass(frozen=True)
class Inputs:
    codex_bin: Path
    hub_url: str
    server_name: str
    expected_tools: tuple[str, ...]
    run_root: Path
    operation_seconds: float
    cleanup_seconds: float

    @classmethod
    def from_env(cls, env: dict[str, str]) -> "Inputs":
        def required(name: str) -> str:
            value = env.get(name, "").strip()
            if not value:
                raise ValueError(f"{name} is required for an opted-in integration")
            return value

        if env.get("CODEX_SDK_HUB_INTEGRATION") != "1":
            raise ValueError("real integration requires CODEX_SDK_HUB_INTEGRATION=1")
        tool_names = json.loads(required("CODEX_SDK_EXPECTED_TOOLS"))
        if not isinstance(tool_names, list):
            raise ValueError("expected tools must be a JSON array")
        result = cls(
            Path(required("CODEX_SDK_CODEX_BIN")),
            required("CODEX_SDK_HUB_URL"),
            required("CODEX_SDK_SERVER_NAME"),
            tuple(tool_names),
            Path(required("CODEX_SDK_RUN_ROOT")),
            float(required("CODEX_SDK_OPERATION_SECONDS")),
            float(required("CODEX_SDK_CLEANUP_SECONDS")),
        )
        result.validate()
        return result

    def validate(self) -> None:
        if not self.codex_bin.is_absolute() or not self.codex_bin.is_file():
            raise ValueError("an existing absolute installed Codex executable is required")
        if not self.run_root.is_absolute() or self.run_root.exists():
            raise ValueError("run root must be an absolute, vacant task-owned directory")
        if not self.run_root.parent.is_dir():
            raise ValueError("run root requires an existing task-owned parent")
        for ancestor in (self.run_root.parent, *self.run_root.parent.parents):
            attributes = getattr(ancestor.lstat(), "st_file_attributes", 0)
            if ancestor.is_symlink() or attributes & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0):
                raise ValueError("fixture parent chain must contain only ordinary directories")
            if (ancestor / ".git").exists() or (ancestor / ".codex").exists():
                raise ValueError("fixture must be outside Git and project Codex config chains")
        route = urlsplit(self.hub_url)
        if (route.scheme not in ("http", "https") or not route.hostname
                or route.username is not None or route.password is not None
                or route.query or route.fragment):
            raise ValueError("borrowed Hub route must be HTTP(S) without credentials/query/fragment")
        # JSON quoting handles TOML basic string keys as well as route strings.
        if not self.server_name or not self.server_name.strip():
            raise ValueError("one explicit MCP server name is required")
        if (not self.expected_tools or any(not isinstance(t, str) or not t.strip()
                                          for t in self.expected_tools)):
            raise ValueError("expected tools must be a nonempty JSON array of tool names")
        if len(set(self.expected_tools)) != len(self.expected_tools):
            raise ValueError("expected tools must be distinct")
        for budget in (self.operation_seconds, self.cleanup_seconds):
            if not math.isfinite(budget) or budget <= 0:
                raise ValueError("caller operation and cleanup budgets must be finite positive seconds")


def private_config(inputs: Inputs) -> str:
    """Exactly one borrowed HTTP route; no copied auth or inherited user config."""
    return (
        "[features]\napps = false\nplugins = false\n"
        "[apps._default]\nenabled = false\n"
        f"[mcp_servers.{json.dumps(inputs.server_name, ensure_ascii=False)}]\n"
        f"url = {json.dumps(inputs.hub_url, ensure_ascii=False)}\nenabled = true\n"
    )


def isolated_environment(source: dict[str, str], home: Path, temp: Path) -> dict[str, str]:
    # The SDK merges env into os.environ. The integration composition root must
    # replace its environment with this allowlist before constructing the client.
    ordinary = {"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SYSTEMDRIVE"}
    env = {key: value for key, value in source.items() if key.upper() in ordinary}
    env.update({"CODEX_HOME": str(home), "HOME": str(home), "USERPROFILE": str(home),
                "APPDATA": str(home / "AppData" / "Roaming"),
                "LOCALAPPDATA": str(home / "AppData" / "Local"),
                "TEMP": str(temp), "TMP": str(temp)})
    system_root = next((v for k, v in env.items() if k.upper() == "SYSTEMROOT"), None)
    if os.name == "nt" and not system_root:
        raise ValueError("Windows SystemRoot is required for the isolated worker")
    if system_root:
        env["PATH"] = str(Path(system_root) / "System32")
    return env


class ConfigIsolationError(ValueError):
    """A failed isolation check with a deliberately value-free field inventory."""

    def __init__(self, message: str, diagnostics: dict):
        self.diagnostics = diagnostics
        super().__init__(f"{message}; field diagnostics={json.dumps(diagnostics, sort_keys=True)}")


def config_field_diagnostics(config: dict, inputs: Inputs) -> dict:
    # Schema field names from the selected official CLI's McpServerConfig and
    # HTTP transport. Unknown keys may themselves contain sensitive values;
    # report only their count, never their spelling or any configured values.
    schema_fields = {
        "url", "enabled", "environment_id", "auth", "required", "startup_readiness",
        "supports_parallel_tool_calls", "startup_timeout_sec", "tool_timeout_sec",
        "tool_input_schema_max_bytes", "omit_tools_from", "default_tools_approval_mode",
        "enabled_tools", "disabled_tools", "scopes", "oauth", "oauth_resource", "tools",
        "bearer_token_env_var", "http_headers", "env_http_headers", "http_headers_helper",
        "command", "args", "env", "env_vars", "cwd",
    }
    servers = config.get("mcp_servers")
    server = servers.get(inputs.server_name) if isinstance(servers, dict) else None
    if not isinstance(server, dict):
        return {"expected_server_mapping": False}
    environment = server.get("environment_id")
    auth = server.get("auth")
    summary = {"expected_server_mapping": True,
               "server_fields": sorted(set(server) & schema_fields),
               "unknown_field_count": len(set(server) - schema_fields),
               "environment_id_state": ("absent" if "environment_id" not in server else
                                        "default_local" if environment == "local" else "nondefault"),
               "auth_state": ("absent" if "auth" not in server else
                              "default_oauth" if auth == "oauth" else "nondefault")}
    for name in ("bearer_token_env_var", "http_headers", "env_http_headers", "http_headers_helper",
                 "oauth", "oauth_resource", "scopes", "tools"):
        value = server.get(name)
        summary[name + "_state"] = ("absent" if name not in server else "null" if value is None else
                                    "empty" if value in ({}, []) else "configured")
    return summary


def verify_effective_config(config: dict, inputs: Inputs) -> None:
    if config.get("features", {}).get("apps") is not False:
        raise ValueError("effective apps feature is missing or enabled")
    if config.get("features", {}).get("plugins") is not False:
        raise ValueError("effective plugins feature is missing or enabled")
    if config.get("apps", {}).get("_default", {}).get("enabled") is not False:
        raise ValueError("effective default App access is missing or enabled")
    apps = config.get("apps", {})
    if any(name != "_default" and app.get("enabled") is not False
           for name, app in apps.items()):
        raise ValueError("effective per-App access is enabled or unknown")
    servers = config.get("mcp_servers")
    if not isinstance(servers, dict) or set(servers) != {inputs.server_name}:
        raise ValueError("effective MCP server set differs from the one borrowed Hub route")
    server = servers[inputs.server_name]
    if server.get("url") != inputs.hub_url or server.get("enabled") is not True:
        raise ValueError("effective Hub route changed or is not explicitly enabled")
    # The CLI supplies local when this optional metadata is omitted. A present
    # value must agree with that default; absence never weakens route/enabled,
    # server-set or authentication checks.
    if "environment_id" in server and server["environment_id"] != "local":
        raise ConfigIsolationError("effective Hub environment is not the local default",
                                   config_field_diagnostics(config, inputs))
    if set(server) - {"url", "enabled", "environment_id", "startup_timeout_sec", "tool_timeout_sec"}:
        raise ConfigIsolationError("effective Hub route contains unexpected connection/auth controls",
                                   config_field_diagnostics(config, inputs))


def discover(client, inputs: Inputs) -> dict:
    """Only initialize/initialized, config/read, and typed MCP status pagination."""
    from openai_codex.generated.v2_all import ConfigReadResponse, ListMcpServerStatusResponse

    client.initialize()
    config = client.request("config/read", {"includeLayers": False}, response_model=ConfigReadResponse)
    verify_effective_config(config.config.model_dump(by_alias=True, exclude_none=True), inputs)
    seen_cursors: set[str] = set()
    cursor = None
    found = []
    pages = 0
    while True:
        params = {"serverName": inputs.server_name}
        if cursor is not None:
            params["cursor"] = cursor
        page = client.request("mcpServerStatus/list", params,
                              response_model=ListMcpServerStatusResponse)
        pages += 1
        for status in page.data:
            if status.name != inputs.server_name or status.plugin_id is not None:
                raise ValueError("MCP discovery returned an unexpected server or plugin")
            found.append(status)
        cursor = page.next_cursor
        if cursor is None:
            break
        if not cursor or cursor in seen_cursors:
            raise ValueError("MCP status pagination did not progress")
        seen_cursors.add(cursor)
    if len(found) != 1:
        raise ValueError("MCP discovery must return exactly the borrowed Hub server")
    status = found[0]
    if status.tools_error is not None:
        raise ValueError("borrowed Hub tool discovery reported an error")
    if status.server_info is None or status.server_capabilities is None:
        raise ValueError("borrowed Hub initialization/capabilities were not received")
    if "tools" not in status.server_capabilities:
        raise ValueError("borrowed Hub did not advertise tool discovery")
    if not set(inputs.expected_tools).issubset(status.tools):
        raise ValueError("borrowed Hub did not expose the expected tools")
    # Deliberately omit raw config, endpoint, tool schemas, resource contents,
    # userAgent/home paths, auth, and RPC streams from portable evidence.
    return {"initialized": True, "config_verified": True, "status_pages": pages,
            "server_name": status.name, "expected_tools": list(inputs.expected_tools),
            "tool_count": len(status.tools), "thread_calls": 0, "turn_calls": 0,
            "model_calls": 0}
