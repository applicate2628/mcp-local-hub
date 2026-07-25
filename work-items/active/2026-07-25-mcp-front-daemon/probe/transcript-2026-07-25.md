# Probe transcript — 2026-07-25

Real captured output from the session that implemented Increment 1 and its
F1/F2/F3 review-response fixes. Two runs: the original Phase-1c probe
(initialize-only, no registered workspace) and the F3-strengthened probe
(real registered workspace + real forwarded tool-call). Both ran with
HOME/LOCALAPPDATA/state-dir redirected to a scratch directory, detached
processes (`Start-Process -WindowStyle Hidden`), and identity-gated kills.
Neither run touched the operator's real `~/.local/bin` fleet or state dir.

## Run 1 — Phase 1c (initialize-only, before F1/F2 fixes)

Binary: `-H windowsgui -tags test_state_path_env`, confirmed via `file`:
`PE32+ executable for MS Windows 6.01 (GUI), x86-64, 17 sections`.

```
GuiPid       : 17708
GuiExePath   : R:\...\mcphub-front-daemon-probe\bin\mcphub-probe.exe
RoutePid     : 37736
RouteExePath : R:\...\mcphub-front-daemon-probe\bin\mcphub-probe.exe

--- gui.out.log ---
GUI listening on http://127.0.0.1:19125
--- route.out.log ---
mcphub route: serving /serena/mcp + /lsp/<language>/mcp on 127.0.0.1:19126 (pid 37736)

=== GUI port 19125 /serena/mcp (initialize) ===
Status: 200
Mcp-Session-Id: 1faef42bab5abfd8f474f1529f9a5a2b
Body: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25", ...}}

=== route port 19126 /serena/mcp (initialize) ===
Status: 200
Mcp-Session-Id: e3bcf29ebbef2e3fd41766b1bd71fdc3
Body: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25", ...}}  (identical shape)

=== claim 4: wrong Host header on route port -> 403 HOST_NOT_ALLOWED ===
Status: 403

=== Identity-gated kill of GUI pid 17708 ===
Identity verified: pid 17708 is our probe binary. Killing GUI ONLY.
GUI process still running after kill: False
route daemon still alive: True (pid 37736)

=== AFTER killing GUI: route port 19126 /serena/mcp ===
Status: 200
Body: {"jsonrpc":"2.0","id":2,"result":{"protocolVersion":"2025-11-25", ...}}

=== AFTER killing GUI: GUI port 19125 /serena/mcp ===
Connection result (expected refusal): No connection could be made because
the target machine actively refused it. (127.0.0.1:19125)
```

Cleanup: route pid 37736 killed (identity-verified); final sweep of
`mcphub*` showed only the operator's real fleet at
`C:\Users\dima_\.local\bin\mcphub.exe` — untouched.

**Gap this run left open (F3 advisory):** `initialize` is synthesized by the
router itself with no workspace/daemon involved — it does not prove a
FORWARDED tool-call (the actual reliability claim) survives, only that the
listener + handshake do.

## Run 2 — F3-strengthened probe (after F1/F2 fixes; real registered workspace + real forwarded tool-call)

Setup: registered a real workspace (canonical path, `.serena/project.yml`
marker present) in a temp `workspaces.yaml`, pointed at a standalone
`fake-daemon.exe` process on port 19301 reproducing the exact MCP handshake +
tool-call contract `serena_router_test.go`'s `fakeSerenaDaemon` fixture uses.

First attempt WITHOUT the canonical path / marker file hit exactly the
described gap — captured here because it is itself informative (proves the
resolver's ancestor-walk requirement is real, not assumed):

```
=== BEFORE killing GUI (first attempt, no .serena marker, raw path) ===
port 19125: ERROR Response status code does not indicate success: 503 (Service Unavailable).
port 19137: ERROR Response status code does not indicate success: 503 (Service Unavailable).

body: {"error":"register workspace first via mcphub workspace register <path>",
       "phase_e_status":"deferred","hint_command":"mcphub workspace register <path>",
       "resolved_path":"R:\\...\\workspace2\\MyProject\\src\\main.go"}
```

Fixed by adding `.serena/project.yml` and re-registering via
`api.CanonicalWorkspacePath` (see `_fixtures/register_workspace/main.go`).
Re-running:

```
registered R:\...\workspace2\MyProject -> port 19301 in R:\...\appdata2\mcp-local-hub\workspaces.yaml

FakePid  : 68108   FakePath  : R:\...\fake-daemon\fake-daemon.exe
GuiPid   : 76172   GuiExePath: R:\...\bin\mcphub-probe.exe
RoutePid : 57772   RouteExePath: R:\...\bin\mcphub-probe.exe

--- fake.err.log ---
2026/07/25 15:23:41 fake-daemon: listening on 127.0.0.1:19301/mcp
--- gui.out.log ---
GUI listening on http://127.0.0.1:19125
--- route.out.log ---
mcphub route: serving /serena/mcp + /lsp/<language>/mcp on 127.0.0.1:19137 (pid 57772)

=== BEFORE killing GUI (retry after registering with marker + canonical path) ===
port 19125: Status=200 Body={"jsonrpc":"2.0","id":1,"result":{"fake_daemon_alive":true,"tool":"probe-marker"}}
port 19137: Status=200 Body={"jsonrpc":"2.0","id":1,"result":{"fake_daemon_alive":true,"tool":"probe-marker"}}
```

Both ports forward the REAL tool-call to the REAL upstream fake daemon and
get back the genuine result — identical on both ports.

```
=== Identity-gated kill of GUI pid 76172 ===
Identity verified: pid 76172 is our probe binary. Killing GUI ONLY.
GUI process still running after kill: False
--- route + fake-daemon still alive? ---
   Id ProcessName  Path
68108 fake-daemon  R:\...\fake-daemon\fake-daemon.exe
57772 mcphub-probe R:\...\bin\mcphub-probe.exe

=== AFTER killing GUI: route port 19137 (expect 200, same forwarded result) ===
Status: 200
Body: {"jsonrpc":"2.0","id":2,"result":{"fake_daemon_alive":true,"tool":"probe-marker"}}

=== AFTER killing GUI: GUI port 19125 (expect refused) ===
Connection result (expected refusal): No connection could be made because
the target machine actively refused it. (127.0.0.1:19125)
```

**This is the decisive proof:** the SAME real, forwarded tool-call to the
SAME upstream daemon succeeds identically before and after the GUI's death,
exclusively through the route daemon's port, while the GUI's port is
refused.

Cleanup (identity-gated): route pid 57772 and fake-daemon pid 68108 both
killed after path verification; final `Get-Process -Name
'mcphub-probe','fake-daemon'` returned nothing. The throwaway
`zzz_probe_setup/` helper used for the first version of this run was deleted
from the repo working tree before commit; its logic is preserved (properly,
under the underscore-prefixed, build-excluded `probe/_fixtures/` tree) in
`_fixtures/register_workspace/main.go`.
