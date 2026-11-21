"""Pure checks by default; real borrowed-Hub integration requires explicit opt-in."""

from __future__ import annotations

import ctypes
from ctypes import wintypes
import hashlib
import importlib.metadata
import inspect
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from hub_discovery import (ConfigIsolationError, Inputs, config_field_diagnostics, discover,
                           isolated_environment, private_config, verify_effective_config)


def example_inputs(root: Path) -> Inputs:
    return Inputs(Path(sys.executable), "http://127.0.0.1:43123/lsp/go/mcp", "hub-test",
                  ("go_file_context",), root / "fresh", 30, 10)


def example_config(inputs: Inputs) -> dict:
    return {"features": {"apps": False, "plugins": False},
            "apps": {"_default": {"enabled": False}},
            "mcp_servers": {inputs.server_name: {"url": inputs.hub_url, "enabled": True}}}


class ConfigurationTests(unittest.TestCase):
    def test_missing_opt_in_or_input_cannot_start_anything(self):
        with self.assertRaisesRegex(ValueError, "INTEGRATION=1"):
            Inputs.from_env({})
        with self.assertRaisesRegex(ValueError, "CODEX_SDK_EXPECTED_TOOLS"):
            Inputs.from_env({"CODEX_SDK_HUB_INTEGRATION": "1"})
        for value in ('"tool-name"', '{"tool-name": true}'):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "JSON array"):
                Inputs.from_env({"CODEX_SDK_HUB_INTEGRATION": "1", "CODEX_SDK_EXPECTED_TOOLS": value})

    def test_budgets_routes_tools_and_project_chain_fail_closed(self):
        from dataclasses import replace
        with tempfile.TemporaryDirectory() as directory:
            inputs = example_inputs(Path(directory))
            inputs.validate()
            for changes in ({"operation_seconds": 0}, {"cleanup_seconds": float("nan")},
                            {"operation_seconds": float("inf")}, {"expected_tools": ()},
                            {"expected_tools": ("go_file_context", "go_file_context")},
                            {"hub_url": "https://token@example.test/mcp"},
                            {"hub_url": "http://localhost/mcp?secret=x"},
                            {"hub_url": "file:///tmp/mcp"}, {"codex_bin": Path("codex")}):
                with self.subTest(changes=changes), self.assertRaises(ValueError):
                    replace(inputs, **changes).validate()
            (Path(directory) / ".codex").mkdir()
            with self.assertRaisesRegex(ValueError, "config chains"):
                inputs.validate()

    def test_environment_drops_credentials_and_sdk_parent_merge_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            env = isolated_environment({"SystemRoot": str(root), "PATH": "foreign",
                                        "OPENAI_API_KEY": "synthetic-secret",
                                        "CODEX_HOME": "foreign", "MCP_AUTH_TOKEN": "secret"},
                                       root / "home", root / "temp")
            self.assertNotIn("OPENAI_API_KEY", env)
            self.assertNotIn("MCP_AUTH_TOKEN", env)
            self.assertEqual(env["CODEX_HOME"], str(root / "home"))
            self.assertEqual(env["PATH"], str(root / "System32"))
            self.assertNotIn("foreign", env.values())

    def test_one_config_writer_and_missing_equal_defaults_do_not_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            inputs = example_inputs(Path(directory))
            text = private_config(inputs)
            self.assertEqual(text.count("[mcp_servers."), 1)
            self.assertNotIn("token", text)
            verify_effective_config(example_config(inputs), inputs)
            import copy
            for mutate in (
                lambda c: c.pop("features"),
                lambda c: c["features"].pop("plugins"),
                lambda c: c["features"].update(apps=True),
                lambda c: c.pop("apps"),
                lambda c: c["apps"].update(extra={"enabled": True}),
                lambda c: c["mcp_servers"].update(extra={"url": inputs.hub_url}),
                lambda c: c["mcp_servers"][inputs.server_name].pop("enabled"),
                lambda c: c["mcp_servers"][inputs.server_name].update(http_headers={"X": "secret"}),
                lambda c: c["mcp_servers"][inputs.server_name].update(url="http://localhost/other"),
            ):
                config = copy.deepcopy(example_config(inputs))
                mutate(config)
                with self.subTest(config=config), self.assertRaises(ValueError):
                    verify_effective_config(config, inputs)


class DiscoveryTests(unittest.TestCase):
    def setUp(self):
        from openai_codex.generated.v2_all import ConfigReadResponse, ListMcpServerStatusResponse
        self.config_type = ConfigReadResponse
        self.page_type = ListMcpServerStatusResponse
        self.inputs = example_inputs(Path(tempfile.gettempdir()))

    def client(self, pages, config=None):
        owner = self

        class Client:
            calls = []

            def initialize(self):
                self.calls.append(("initialize", None))
                return object()

            def request(self, method, params, *, response_model):
                self.calls.append((method, params))
                if method == "config/read":
                    owner.assertIs(response_model, owner.config_type)
                    return response_model.model_validate({"config": config or example_config(owner.inputs),
                                                          "origins": {}})
                owner.assertEqual(method, "mcpServerStatus/list")
                owner.assertIs(response_model, owner.page_type)
                return response_model.model_validate(next(pages))

        return Client()

    def status(self, **overrides):
        value = {"name": self.inputs.server_name, "authStatus": "unsupported",
                 "tools": {"go_file_context": {"name": "go_file_context", "inputSchema": {}}},
                 "resources": [], "resourceTemplates": [], "serverCapabilities": {"tools": {}},
                 "serverInfo": {"name": "synthetic-unit-only", "version": "0"}}
        value.update(overrides)
        return value

    def test_public_sdk_surface_and_typed_wire_aliases(self):
        from openai_codex.client import CodexClient, CodexConfig
        from openai_codex.generated.v2_all import ListMcpServerStatusParams
        self.assertIn("launch_args_override", inspect.signature(CodexConfig).parameters)
        self.assertIn("response_model", inspect.signature(CodexClient.request).parameters)
        for method in ("start", "close", "initialize"):
            self.assertTrue(callable(getattr(CodexClient, method)))
        self.assertEqual(ListMcpServerStatusParams(server_name="hub").model_dump(
            by_alias=True, exclude_none=True), {"serverName": "hub"})

    def test_discovery_paginates_without_turns_and_returns_only_allowed_evidence(self):
        client = self.client(iter([{"data": [], "nextCursor": "next"},
                                   {"data": [self.status()], "nextCursor": None}]))
        result = discover(client, self.inputs)
        self.assertEqual([method for method, _ in client.calls],
                         ["initialize", "config/read", "mcpServerStatus/list", "mcpServerStatus/list"])
        self.assertEqual(client.calls[-1][1], {"serverName": "hub-test", "cursor": "next"})
        self.assertEqual(result["model_calls"], 0)
        self.assertEqual(result["status_pages"], 2)
        self.assertNotIn(self.inputs.hub_url, json.dumps(result))

    def test_pagination_repetition_and_missing_catalog_fail(self):
        client = self.client(iter([{"data": [], "nextCursor": "same"}] * 2))
        with self.assertRaisesRegex(ValueError, "did not progress"):
            discover(client, self.inputs)
        for status in (self.status(toolsError="synthetic failure"), self.status(serverInfo=None),
                       self.status(serverCapabilities=None), self.status(tools={}),
                       self.status(name="other"), self.status(pluginId="foreign")):
            with self.subTest(status=status), self.assertRaises(ValueError):
                discover(self.client(iter([{"data": [status]}])), self.inputs)

    def test_effective_config_refusal_precedes_mcp_discovery(self):
        client = self.client(iter([]), config={"features": {"apps": True}})
        with self.assertRaisesRegex(ValueError, "apps feature"):
            discover(client, self.inputs)
        self.assertEqual([method for method, _ in client.calls], ["initialize", "config/read"])

    def test_field_diagnostic_keeps_values_and_unknown_names_private(self):
        config = example_config(self.inputs)
        config["mcp_servers"][self.inputs.server_name].update(
            environment_id="local", http_headers={"Authorization": "synthetic-secret"},
            **{"synthetic-secret-in-key": "another-secret"})
        with self.assertRaises(ConfigIsolationError) as failure:
            verify_effective_config(config, self.inputs)
        diagnostic = failure.exception.diagnostics
        self.assertEqual(diagnostic["environment_id_state"], "default_local")
        self.assertEqual(diagnostic["http_headers_state"], "configured")
        self.assertEqual(diagnostic["unknown_field_count"], 1)
        self.assertNotIn("synthetic-secret", str(failure.exception))
        self.assertNotIn("Authorization", str(failure.exception))
        self.assertEqual(config_field_diagnostics(config, self.inputs), diagnostic)

    def test_local_environment_metadata_and_omission_preserve_all_required_controls(self):
        import copy
        config = example_config(self.inputs)
        verify_effective_config(config, self.inputs)
        config["mcp_servers"][self.inputs.server_name].update(environment_id="local", tool_timeout_sec=None)
        result = discover(self.client(iter([{"data": [self.status()]}]), config=config), self.inputs)
        self.assertTrue(result["config_verified"])
        for value in ("remote", "", None, False, 0, {}, [], ["local"], "LOCAL"):
            candidate = copy.deepcopy(config)
            candidate["mcp_servers"][self.inputs.server_name]["environment_id"] = value
            with self.subTest(environment=value), self.assertRaisesRegex(ConfigIsolationError, "local default"):
                verify_effective_config(candidate, self.inputs)
        for mutate in (
            lambda c: c["mcp_servers"][self.inputs.server_name].pop("enabled"),
            lambda c: c["mcp_servers"][self.inputs.server_name].pop("url"),
            lambda c: c["features"].pop("apps"),
            lambda c: c["features"].pop("plugins"),
            lambda c: c.pop("apps"),
            lambda c: c["mcp_servers"].update(other={"enabled": True, "url": self.inputs.hub_url}),
            lambda c: c["mcp_servers"][self.inputs.server_name].update(auth="oauth"),
            lambda c: c["mcp_servers"][self.inputs.server_name].update(http_headers={}),
            lambda c: c["mcp_servers"][self.inputs.server_name].update(unknown_control=False),
        ):
            candidate = copy.deepcopy(config)
            mutate(candidate)
            with self.subTest(candidate=candidate), self.assertRaises(ValueError):
                verify_effective_config(candidate, self.inputs)


class WindowsExitObservation:
    """Read-only process-incarnation observation; never requests termination access.

    Existing Go process observations cannot be imported into this Python test.
    Use the same Windows query/synchronize/get-times/wait/close facilities here.
    The SDK remains the creator and sole shutdown owner.
    """

    def __init__(self):
        self.handle = None
        self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        self.kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
        self.kernel.OpenProcess.restype = wintypes.HANDLE
        self.kernel.GetProcessTimes.argtypes = [wintypes.HANDLE] + [ctypes.POINTER(wintypes.FILETIME)] * 4
        self.kernel.GetProcessTimes.restype = wintypes.BOOL
        self.kernel.QueryFullProcessImageNameW.argtypes = [wintypes.HANDLE, wintypes.DWORD,
                                                         wintypes.LPWSTR, ctypes.POINTER(wintypes.DWORD)]
        self.kernel.QueryFullProcessImageNameW.restype = wintypes.BOOL
        self.kernel.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
        self.kernel.WaitForSingleObject.restype = wintypes.DWORD
        self.kernel.GetExitCodeProcess.argtypes = [wintypes.HANDLE, ctypes.POINTER(wintypes.DWORD)]
        self.kernel.GetExitCodeProcess.restype = wintypes.BOOL
        self.kernel.CloseHandle.argtypes = [wintypes.HANDLE]
        self.kernel.CloseHandle.restype = wintypes.BOOL
        self.kernel.GetSystemTimeAsFileTime.argtypes = [ctypes.POINTER(wintypes.FILETIME)]
        self.kernel.GetSystemTimeAsFileTime.restype = None

    def observe(self, codex_bin: Path, marker: Path, earliest: int, budget: float, capture_file: Path):
        system_root = Path(os.environ["SystemRoot"])
        powershell = system_root / "System32/WindowsPowerShell/v1.0/powershell.exe"
        env = dict(os.environ, CODEX_SDK_OBSERVE_EXE=str(codex_bin),
                   CODEX_SDK_OBSERVE_MARKER=str(marker), CODEX_SDK_OBSERVE_PARENT=str(os.getpid()))
        script = (
            "$ErrorActionPreference='Stop'; "
            "$rows=@(Get-CimInstance Win32_Process | Where-Object { "
            "$_.ExecutablePath -eq $env:CODEX_SDK_OBSERVE_EXE -and "
            "$_.ParentProcessId -eq [int]$env:CODEX_SDK_OBSERVE_PARENT -and "
            "$_.CommandLine.Contains($env:CODEX_SDK_OBSERVE_MARKER) "
            "} | ForEach-Object { @{ pid=$_.ProcessId; "
            "created=$_.CreationDate.ToUniversalTime().ToFileTimeUtc().ToString() } }); "
            "ConvertTo-Json -Compress -InputObject $rows"
        )
        try:
            captured = subprocess.run([str(powershell), "-NoProfile", "-NonInteractive", "-Command", script],
                                      env=env, text=True, capture_output=True,
                                      timeout=budget, creationflags=subprocess.CREATE_NO_WINDOW)
        except subprocess.TimeoutExpired as error:
            capture_file.write_text(json.dumps({"error_type": type(error).__name__,
                "stdout": (error.stdout or b"").decode("utf-8", errors="replace"),
                "stderr": (error.stderr or b"").decode("utf-8", errors="replace")}, indent=2), encoding="utf-8")
            raise
        capture_file.write_text(json.dumps({"returncode": captured.returncode,
                                           "stdout": captured.stdout, "stderr": captured.stderr}, indent=2),
                                encoding="utf-8")
        captured.check_returncode()
        rows = json.loads(captured.stdout)
        if len(rows) != 1:
            raise RuntimeError("exact CLI/fixture/parent observation did not identify one app-server")
        self.pid = int(rows[0]["pid"])
        # PROCESS_QUERY_LIMITED_INFORMATION | SYNCHRONIZE. No kill rights.
        self.handle = self.kernel.OpenProcess(0x1000 | 0x100000, False, self.pid)
        if not self.handle:
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            created, exited, kernel, user = (wintypes.FILETIME() for _ in range(4))
            if not self.kernel.GetProcessTimes(self.handle, ctypes.byref(created), ctypes.byref(exited),
                                               ctypes.byref(kernel), ctypes.byref(user)):
                raise ctypes.WinError(ctypes.get_last_error())
            self.created = created.dwLowDateTime | (created.dwHighDateTime << 32)
            # CIM datetime preserves microseconds; the retained native identity
            # preserves all 100-ns FILETIME ticks. Compare at CIM's resolution.
            if self.created < earliest or self.created // 10 != int(rows[0]["created"]) // 10:
                raise RuntimeError("app-server birth identity changed before native receiving")
            size = wintypes.DWORD(32768)  # Windows maximum extended path, UTF-16 units.
            image = ctypes.create_unicode_buffer(size.value)
            if not self.kernel.QueryFullProcessImageNameW(self.handle, 0, image, ctypes.byref(size)):
                raise ctypes.WinError(ctypes.get_last_error())
            if os.path.normcase(os.path.realpath(image.value)) != os.path.normcase(str(codex_bin.resolve())):
                raise RuntimeError("retained app-server image differs from selected CLI")
        except BaseException:
            self.close()
            raise

    @staticmethod
    def before_start() -> int:
        value = wintypes.FILETIME()
        ctypes.windll.kernel32.GetSystemTimeAsFileTime(ctypes.byref(value))
        return value.dwLowDateTime | (value.dwHighDateTime << 32)

    def receive_exit(self, seconds: float) -> dict:
        # DWORD waits cannot encode the INFINITE sentinel as a finite budget.
        remaining = seconds
        import time
        deadline = time.monotonic() + remaining
        while remaining > 0:
            result = self.kernel.WaitForSingleObject(self.handle, min(math_ceil_ms(remaining), 0xFFFFFFFE))
            if result == 0:
                code = wintypes.DWORD()
                if not self.kernel.GetExitCodeProcess(self.handle, ctypes.byref(code)):
                    raise ctypes.WinError(ctypes.get_last_error())
                return {"pid": self.pid, "creation_filetime": self.created,
                        "native_exit_received": True, "exit_code": code.value}
            if result != 258:
                raise ctypes.WinError(ctypes.get_last_error())
            remaining = deadline - time.monotonic()
        raise TimeoutError(f"SDK app-server native exit unreceived: pid={self.pid}, birth={self.created}")

    def close(self):
        if self.handle:
            if not self.kernel.CloseHandle(self.handle):
                raise ctypes.WinError(ctypes.get_last_error())
            self.handle = None


def math_ceil_ms(seconds: float) -> int:
    import math
    return math.ceil(seconds * 1000)


def file_sha256(path: Path) -> str:
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


@unittest.skipUnless(os.environ.get("CODEX_SDK_HUB_INTEGRATION") == "1", "real Hub integration is opt-in")
class RealHubIntegration(unittest.TestCase):
    def test_two_sequential_sdk_lifecycles(self):
        if os.name != "nt":
            self.fail("native exit receiving is currently implemented only for Windows")
        inputs = Inputs.from_env(dict(os.environ))
        inputs.run_root.mkdir()
        fixture = inputs.run_root / "fixture"
        home = inputs.run_root / "codex-home"
        temp = inputs.run_root / "temp"
        for directory in (fixture, home, temp, home / "AppData/Roaming", home / "AppData/Local"):
            directory.mkdir(parents=True, exist_ok=True)
        config_file = home / "config.toml"
        config_file.write_text(private_config(inputs), encoding="utf-8")
        config_hash = hashlib.sha256(config_file.read_bytes()).hexdigest()
        original_env = dict(os.environ)
        evidence = {"sdk": importlib.metadata.version("openai-codex"),
                    "sdk_cli_bundle": importlib.metadata.version("openai-codex-cli-bin"),
                    "python": sys.version.split()[0],
                    "cli_sha256": file_sha256(inputs.codex_bin),
                    "lifecycles": []}
        evidence_file = inputs.run_root / "result.json"

        def persist():
            evidence_file.write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")

        try:
            os.environ.clear()
            os.environ.update(isolated_environment(original_env, home, temp))
            try:
                cli_version = subprocess.run([str(inputs.codex_bin), "--version"], capture_output=True,
                                             text=True, timeout=inputs.operation_seconds,
                                             creationflags=subprocess.CREATE_NO_WINDOW)
            except subprocess.TimeoutExpired as error:
                (inputs.run_root / "cli-version.json").write_text(json.dumps({"error_type": type(error).__name__,
                    "stdout": (error.stdout or b"").decode("utf-8", errors="replace"),
                    "stderr": (error.stderr or b"").decode("utf-8", errors="replace")}, indent=2), encoding="utf-8")
                raise
            (inputs.run_root / "cli-version.json").write_text(json.dumps({"returncode": cli_version.returncode,
                "stdout": cli_version.stdout, "stderr": cli_version.stderr}, indent=2), encoding="utf-8")
            cli_version.check_returncode()
            evidence["cli_version"] = cli_version.stdout.strip()
            from openai_codex.client import CodexClient, CodexConfig
            for iteration in range(2):  # Acceptance requires cold start and settled sequential reuse.
                outcome = {"iteration": iteration + 1, "sdk_close_returned": False,
                           "observer_closed": False, "deadline_expired": False}
                evidence["lifecycles"].append(outcome)
                persist()
                client = CodexClient(CodexConfig(
                    codex_bin=str(inputs.codex_bin), cwd=str(fixture),
                    launch_args_override=(str(inputs.codex_bin), "-C", str(fixture),
                                          "app-server", "--listen", "stdio://")))
                observer = None
                timer = None
                sdk_threads = []
                close_lock = threading.Lock()
                close_errors = []
                closed = False

                def close_once():
                    nonlocal closed
                    with close_lock:
                        if not closed:
                            closed = True
                            try:
                                client.close()
                                outcome["sdk_close_returned"] = True
                            except BaseException as error:
                                close_errors.append(error)

                def expire():
                    outcome["deadline_expired"] = True
                    close_once()

                before = WindowsExitObservation.before_start()
                baseline_threads = set(threading.enumerate())
                operation_error = None
                cleanup_errors = []
                try:
                    client.start()
                    sdk_threads = [thread for thread in threading.enumerate() if thread not in baseline_threads]
                    observer = WindowsExitObservation()
                    observer.observe(inputs.codex_bin, fixture, before, inputs.operation_seconds,
                                     inputs.run_root / f"observer-{iteration + 1}.json")
                    outcome.update(pid=observer.pid, creation_filetime=observer.created)
                    persist()
                    timer = threading.Timer(inputs.operation_seconds, expire)
                    timer.start()
                    outcome["discovery"] = discover(client, inputs)
                    persist()
                except BaseException as error:
                    operation_error = error
                    outcome["operation_error_type"] = type(error).__name__
                    if isinstance(error, ConfigIsolationError):
                        outcome["config_field_diagnostics"] = error.diagnostics
                    persist()
                finally:
                    if timer:
                        timer.cancel()
                    close_once()
                    if timer:
                        timer.join(inputs.cleanup_seconds)
                        outcome["deadline_thread_joined"] = not timer.is_alive()
                    try:
                        if observer and observer.handle:
                            outcome["native_exit"] = observer.receive_exit(inputs.cleanup_seconds)
                    except BaseException as error:
                        cleanup_errors.append(error)
                    finally:
                        if observer:
                            try:
                                observer.close()
                                outcome["observer_closed"] = True
                            except BaseException as error:
                                cleanup_errors.append(error)
                        if not sdk_threads:
                            sdk_threads = [thread for thread in threading.enumerate()
                                           if thread not in baseline_threads and thread is not timer]
                        for thread in sdk_threads:
                            thread.join(inputs.cleanup_seconds)
                        outcome["sdk_reader_thread_count"] = len(sdk_threads)
                        outcome["sdk_reader_threads_joined"] = all(not thread.is_alive() for thread in sdk_threads)
                        outcome["close_error_types"] = [type(error).__name__ for error in close_errors]
                        outcome["cleanup_error_types"] = [type(error).__name__ for error in cleanup_errors]
                        persist()
                if operation_error:
                    raise RuntimeError("SDK discovery failed; partial/cleanup outcomes retained in result.json") from operation_error
                if cleanup_errors:
                    raise RuntimeError("SDK teardown unreceived; pending identity retained in result.json") from cleanup_errors[0]
                self.assertTrue(outcome.get("native_exit", {}).get("native_exit_received"))
                self.assertTrue(outcome["sdk_close_returned"])
                self.assertTrue(outcome["observer_closed"])
                self.assertTrue(outcome["sdk_reader_threads_joined"])
                self.assertTrue(outcome.get("deadline_thread_joined"))
                self.assertFalse(outcome["deadline_expired"])
                self.assertFalse(close_errors)
                self.assertEqual(hashlib.sha256(config_file.read_bytes()).hexdigest(), config_hash)
            evidence["two_lifecycles_settled"] = True
            persist()
        finally:
            os.environ.clear()
            os.environ.update(original_env)
            persist()


if __name__ == "__main__":
    unittest.main()
