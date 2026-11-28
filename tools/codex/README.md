# Codex SDK development checks

This private virtual Python project uses the official `openai-codex` SDK to
check an existing Hub route. It is development/test tooling only. Product Go
code, server manifests, npm runtime dependencies and Product versions do not
depend on it. The SDK also installs `openai-codex-cli-bin`; all of those bytes
stay in the development environment even when selecting a separate installed CLI.

Use an explicitly selected Python compatible with the SDK (minimum Python 3.10)
and the existing `uv` tool. The committed `uv.lock` records the exact released
distributions and hashes. Resolution failure is a setup failure: do not silently
select another Python, CLI, registry, or global environment.

From the repository root, with a task-owned cache/environment location:

```powershell
$python = (Get-Command python -ErrorAction Stop).Source
$env:UV_CACHE_DIR = '<task-owned-cache>'
$env:UV_PROJECT_ENVIRONMENT = '<task-owned-venv>'
uv sync --project tools/codex --group integration --locked --python $python
uv run --project tools/codex --group integration --locked --python $python python -B -m unittest discover -s tools/codex/tests -v
```

Default discovery runs pure/negative checks against actual SDK response types.
It never starts Codex, connects to MCP, or makes a model request. Missing SDK
dependencies fail setup rather than silently removing tests. To deliberately
update dependency resolution, use `uv lock --project tools/codex --python
<explicit-python>` and review the resulting lock before syncing.

## Real borrowed-Hub test

Only run after the Hub lifecycle owner supplies a ready existing route and an
operation window. The fixture does not start, wake, configure or stop a Hub.
No fake server substitutes for this test. Windows is the only currently
implemented native exit receiving path; other platforms fail the opted-in test
explicitly and need their own accepted native observation before live use.

Supply every input explicitly; there are no implicit CLI, endpoint, tool,
working directory or budget defaults:

```powershell
$env:CODEX_SDK_HUB_INTEGRATION = '1'
$env:CODEX_SDK_CODEX_BIN = (Get-Command codex -ErrorAction Stop).Source
$env:CODEX_SDK_HUB_URL = '<owner-supplied-existing-http-route>'
$env:CODEX_SDK_SERVER_NAME = '<one-fixture-server-name>'
$env:CODEX_SDK_EXPECTED_TOOLS = '["<expected-real-tool-name>"]'
$env:CODEX_SDK_RUN_ROOT = '<absolute-vacant-directory-under-task-owned-parent>'
$env:CODEX_SDK_OPERATION_SECONDS = '<caller-operation-budget-in-seconds>'
$env:CODEX_SDK_CLEANUP_SECONDS = '<caller-cleanup-budget-in-seconds>'
uv run --project tools/codex --group integration --locked --python $python python -B -m unittest discover -s tools/codex/tests -v
```

The selected executable must be the installed native CLI, not a shell shim.
The caller retains/receives the original outer test process, its complete
stdout/stderr/exit and a finite overall deadline covering setup, the two
sequential lifecycles and cleanup. Operation and cleanup inputs are positive
finite seconds chosen for that window; the fixture adds no arbitrary page,
tool-count, duration or host-version limit. An operation deadline invokes only
the SDK's public close. The caller's outer deadline remains necessary if setup
or SDK shutdown itself cannot return. Pending native identity/cleanup failure
stops further iterations; never retry or kill by process name/PID/ancestry.

The test creates a new private fixture and `CODEX_HOME` outside any Git/project
Codex configuration chain. It temporarily replaces the serial test worker's
environment with ordinary Windows OS entries and private home/temp locations
before SDK construction, because the SDK merges overrides with inherited
environment. It copies no auth/user config, API keys, sessions or credentials.
The borrowed route must need no copied credentials. Existing machine-managed
requirements still apply; extra routes or required enabled Apps fail receiving.

One public `CodexClient` uses the exact CLI argv with the unique fixture cwd
marker and `app-server --listen stdio://`. It initializes, reads effective
configuration, requires only the supplied Hub server and disabled Apps/plugins,
then receives typed `mcpServerStatus/list` pages for that server. Pagination must
advance to its actual end. Expected real tools and successful initialization
metadata/capabilities are required. There are no thread, turn, model, tool-call,
account/login, App-discovery or model-list calls.

The CLI's effective server configuration may include its default
`environment_id="local"`. Only that present value is accepted; omitted metadata
retains the producer's documented local default. Empty/null/nonlocal values
fail, and omission never relaxes required route/enabled/server-set/disabled-App
controls or permits authentication/unknown fields. Isolation failures retain
only known schema field names and safe classifications; no configured values
or unknown field spellings are printed.

After each public SDK close, a test-local read-only Windows observer receives
exit of the exact retained app-server incarnation. It correlates native image,
birth time, fixture argv and parent with one held query/synchronize handle;
the observer has no termination rights. It closes that handle and joins the
actual SDK-created reader threads before the next lifecycle. This observes
SDK teardown without accessing SDK private fields or replacing transport.

`result.json` preserves allowlisted incremental operation/cleanup outcomes,
versions, CLI hash and native identity; it omits raw effective config, route,
tool schemas, environment and RPC streams. The private run directory is retained
for owner receiving even on failure. Preserve needed evidence under the task
owner before trash-first disposal; do not treat fixture existence as a leak or
delete unreceived state. The caller separately verifies unchanged original
settings/OFF intent and borrowed-Hub state; the fixture owns neither.

## Packaging guard

`cd npm; npm test` exercises the existing package owner with synthetic SDK,
bundled-CLI and virtual-environment sentinels plus exact package inventories.
For a real checkout, inspect both `npm pack --dry-run --json --ignore-scripts`
inventories from `npm/` and `npm/packages/win32-x64/`. Missing native Product
payloads do not prove a complete Product package; zero SDK entries proves only
development dependency exclusion. No SDK install or model call belongs in
normal Go tests or runtime/package manifests.

## Terms and Abbreviations

SDK: Software Development Kit. CLI: command-line interface. MCP: Model Context
Protocol. RPC: remote procedure call. uv: the existing Python resolver and
environment tool. OFF: saved stopped/user-disabled intent. Native receiving:
observing the exact Windows process incarnation's kernel exit and handle close.
