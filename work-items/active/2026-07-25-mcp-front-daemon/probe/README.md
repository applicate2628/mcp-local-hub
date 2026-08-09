# Increment-1 north-star survival probe

Proves the MCP front-daemon decision record's core claim: **`mcphub route`
keeps answering `/serena/mcp` (with a REAL forwarded tool-call, not just the
router's synthesized `initialize`) after the GUI process dies.**

This directory persists the probe fixtures + a run script + a captured
transcript (F3, bot/architect review finding, 2026-07-25 — advisory,
non-blocking, but the reviewer asked for it to be persisted and strengthened
beyond the original Phase-1c initialize-only probe).

## What's here

- `_fixtures/fake_daemon/` — a standalone Go program (own `go.mod`) that
  reproduces the exact MCP Streamable-HTTP handshake + tool-call contract
  `internal/gui/serena_router_test.go`'s `fakeSerenaDaemon` Go-test fixture
  implements, as a REAL OS process other real processes can dial.
- `_fixtures/register_workspace/` — a one-shot Go program that writes a
  single serena workspace entry directly into a `workspaces.yaml`, bypassing
  the trust-gate/auto-register flow (mirrors
  `TestSerenaRouter_RealResolverIntegration_RoutesPathArgToCorrectWorkspace`'s
  `reg.PutSerena` + `reg.Save` shortcut). Uses `api.CanonicalWorkspacePath` —
  a raw, non-canonicalized path silently never matches
  `ResolveByPath`'s ancestor-walk (the first draft of this probe hit exactly
  that 503 before the fix).
- `run-probe.ps1` — the consolidated, safety-annotated PowerShell script that
  runs the whole sequence end to end.
- `transcript-2026-07-25.md` — the REAL captured output from the session
  that built Increment 1 + the F1/F2/F3 review-response fixes.

Both `_fixtures/*` directories sit under an underscore-prefixed parent
(`_fixtures/`), which the go tool ignores in `./...` patterns (documented
behavior: directories starting with `_` are skipped) — they never become
part of `go build ./...` / `go vet ./...` / `go test ./...` for the main
module, and `fake_daemon` additionally has its own separate `go.mod`.

## Hard safety rules this probe follows (do not weaken)

- **Never** launch GUI/route/supervisor into the operator's terminal/session
  — every process below is launched via `Start-Process -WindowStyle Hidden`
  with redirected stdout/stderr, never inline in the driving shell.
- **Never** kill by image name. Every `Stop-Process` call in `run-probe.ps1`
  first verifies `(Get-Process -Id $pid).Path` equals the EXACT probe binary
  path it just launched, and refuses (prints a mismatch, does not kill) on
  any other match.
- **Always** redirect `USERPROFILE`, `LOCALAPPDATA`, and
  `MCPHUB_STATE_DIR_OVERRIDE` to fresh temp directories before launching
  anything — this probe must never read or write the operator's real
  `~/.local/bin` fleet or real state dir. The mcphub binary under test must
  be built with `-tags test_state_path_env` for `MCPHUB_STATE_DIR_OVERRIDE`
  to take effect at all (production builds compile it out).
- `MCPHUB_E2E_SUPERVISOR=none` / `MCPHUB_E2E_SCHEDULER=none` are the
  documented GUI test seams (see `internal/gui/e2e/fixtures/hub.ts` +
  `CLAUDE.md`'s "GUI E2E tests" section) that keep a bare `mcphub gui` from
  waiting on / spawning a real supervisor in a fresh temp HOME.

## How to run

```powershell
# From the repo root (a worktree, never the operator's checked-out fleet
# branch/binary):
powershell -ExecutionPolicy Bypass -File work-items/active/2026-07-25-mcp-front-daemon/probe/run-probe.ps1 `
    -RepoRoot (Get-Location) `
    -ScratchDir "$env:TEMP\mcphub-front-daemon-probe-rerun"
```

The script builds the real `mcphub` binary (`-H windowsgui -tags
test_state_path_env`) and the two fixtures, registers a workspace, launches
fake-daemon + GUI + route, sends a real forwarded tool-call on both ports,
kills ONLY the GUI PID (identity-gated), repeats the tool-call on the route
port, asserts it still succeeds, then cleans up every process it started
(also identity-gated) and prints a PASS/FAIL summary.

## Result (2026-07-25)

PASS — see `transcript-2026-07-25.md` for the full captured output. Summary:

| Step | Port | Result |
|---|---|---|
| Register workspace (canonical path + `.serena/project.yml` marker) | — | OK |
| Forwarded tool-call, GUI up | 19125 (GUI) | 200, `{"fake_daemon_alive":true,...}` |
| Forwarded tool-call, GUI up | 19137 (route) | 200, IDENTICAL result |
| Kill GUI PID (identity-gated) | — | GUI process confirmed dead; route + fake-daemon confirmed alive |
| Forwarded tool-call, GUI DEAD | 19137 (route) | 200, IDENTICAL result — **survives** |
| Same request | 19125 (GUI) | connection actively refused (expected) |
