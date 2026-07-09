---
severity: high
filed: 2026-07-07
context: operator-hit (Antigravity), fable-root-caused during the global-proliferation diagnosis
---

- **status:** open
- **HEAD reconciliation (2026-07-09):** Backend/CLI fixed by PR #512 (`c3fc1801`), but the original GUI Servers-tab disable path remains a design gap at master `63b6a008`.

# "Disabled" client still gets LSP-router relay entries — Antigravity spawns 18 `mcphub relay` despite being disabled in the Servers tab

## Symptom (operator-reported, live)
The operator disabled the **Antigravity** client in the GUI Servers tab (intent: "MCP
only in codex + claude"). The Servers tab SHOWED Antigravity as disabled — but Antigravity
kept spawning **18 `mcphub.exe relay --url http://127.0.0.1:9125/lsp/<lang>/mcp` processes**
(9 languages × 2 windows). Display said off; reality kept 18 relays alive. Verified live:
`~/.gemini/antigravity/mcp_config.json` held 9 `mcp-language-server-*` entries all
`disabled:false`, and the 18 relay processes were children of Antigravity's two
`language_server_windows_x64.exe` (PIDs 23076/373232).

## Root cause (fable, verified vs HEAD)
- `EnsureLSPRouterClientEntries` (`internal/api/lsp_client_router.go` ~:84, ~:99-111) writes
  the 9 suffixed `mcp-language-server-<lang>` relay entries into **every** client where
  `adapter.Exists()` is true, with **NO per-client enablement check**. Invoked by
  `mcphub setup` (`internal/cli/setup.go` ~:78).
- The Servers-tab disable runs `Demigrate` (`internal/api/demigrate.go` ~:71), which removes
  only entries named after top-matrix MANIFEST servers. The suffixed relay entry names
  (`LSPRouterEntryName`, ~:267) are NOT matched, so they survive.
- The only removal path today is the GLOBAL `mcphub setup --rollback-lsp-router`, which would
  strip the LSP entries from claude/codex too (unacceptable). So a per-client "disable" has no
  way to remove that client's relay entries, and the next `mcphub setup` re-adds them.

Net: the per-client disable contract is silently violated for the LSP-router surface —
"disabled" does not mean disabled there.

## Fix (backend, in flight on `fix/lsp-router-per-client-disable`)
(a) `EnsureLSPRouterClientEntries` consults the client-enablement set
(`internal/gui/client_install_prefs.go`) and SKIPS operator-disabled clients (no write, no
re-write on the next setup). (b) New `RollbackLSPRouterClientEntriesForClient(client)` removes
ONLY that one client's `mcp-language-server-<lang>` entries (reusing `LSPRouterEntryName` so the
removal set matches the write set), leaving every other client untouched.

**Frontend follow-up (separate):** wire the Servers-tab LspMatrix per-client checkbox
(`internal/gui/frontend/src/screens/Servers.tsx` ~:1737-1740) + `internal/gui/demigrate.go` to
call the per-client rollback, then `go generate`.

## Interim relief (already applied this session, reversible)
Hand-edited `~/.gemini/antigravity/mcp_config.json` → set `disabled:true` on the 9
`mcp-language-server-*` entries (backup written) + killed the 18 running relay processes. The
operator's own non-mcphub Antigravity servers (lldb-bridge, stgen-dxf-viewer) were left intact.

## Related
Surfaced by the global process-proliferation diagnosis (`work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md`,
fable workflow `wf_e08d5606-406`). This is the P1 (mcphub-bug) root cause; P0 was the @mui
bypass leak (separate, now onboarded).
