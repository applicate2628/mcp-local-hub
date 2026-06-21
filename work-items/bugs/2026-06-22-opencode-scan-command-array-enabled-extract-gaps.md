---
title: OpenCode (and JSON-family) scan ignores command-ARRAY shape + `enabled` flag; OpenCode has no extract case
severity: low
found-by: backend-engineer
found-in-phase: PR #420 mimocode adapter — bot findings 3, 5, 6 (mimo-specific fixes landed; opencode side flagged not fixed)
affected-surface: >
  internal/api/scan.go shapeURLOrCommandEntry (shared by ~30 clients incl. opencode, kiro, cline,
  kilocode, hermes, openclaw, and the entire jsonMCPClient family); internal/api/scan.go
  scanOpenCode (no `enabled` filter); internal/api/scan.go ExtractManifestFromClient (no `opencode`
  case → hits the `default` "extract not yet supported" branch); internal/gui/extract_manifest.go
  realExtractor (does not populate OpenCodeConfigPath)
context: adjacent-finding
status: open
---

## Summary

While fixing the mimocode adapter for PR #420 (bot findings 3 + 5 + 6) I confirmed the SAME three
gaps exist for OpenCode and, for two of them, for the whole shared JSON client family. They were left
unfixed deliberately: OpenCode is PROTECTED scope for PR #420, the affected helper
`shapeURLOrCommandEntry` is consumed by ~30 clients, and changing it is a behavioral change for all
of them that needs its own review. The mimocode fixes are self-contained (a mimo-specific shaper +
a mimo extract case), so they do not touch the shared path.

## The three gaps (verified against code at HEAD a3e8bf3c)

1. **Local `command` ARRAY → empty "Unknown stdio" row (mirror of mimo Finding 3).**
   `shapeURLOrCommandEntry` (scan.go) reads `cmd, _ := raw["command"].(string)`. OpenCode (and
   MiMoCode) store a local entry's command as an ARRAY (`["npx","-y","pkg"]`, no separate `args`).
   So `scanOpenCode` surfaces every operator-authored local OpenCode MCP server as a stdio row with
   an EMPTY endpoint. The `command`-string assumption is also wrong for any other client in the
   shared-helper set whose real format is an array; each should be audited.
   - Verified: https://opencode.ai/docs/mcp-servers/ — `"command": ["npx", "-y", "my-mcp-command"]`,
     no separate `args`, `"type":"local"`.

2. **`enabled:false` not honored (mirror of mimo Finding 6).**
   scan.go has NO `enabled`/`disabled` filtering anywhere. OpenCode's `enabled:false` (a documented
   disable flag) is ignored, so a disabled OpenCode hub entry still classifies via-hub/connected. The
   JSON family's `disabled:true` is likewise ignored (separate but related: the standard
   `mcpServers` shape uses `disabled`, not `enabled`).
   - Verified: https://opencode.ai/docs/mcp-servers/ — "disable a server by setting enabled to
     false".

3. **No `opencode` case in `ExtractManifestFromClient` (mirror of mimo Finding 5).**
   The extract switch has cases for claude-code, codex-cli, cursor, vscode, gemini-cli, qwen-cli,
   antigravity, then `default` → "extract not yet supported". OpenCode (like mimocode before this PR)
   hits the default and cannot extract. The GUI `realExtractor` also does not populate
   `OpenCodeConfigPath`. Fixing this needs the array→command/args split (gap 1) too.

## Suggested fix (separate PR, OpenCode-in-scope)

- Add an array-aware, `enabled`-aware OpenCode shaper (mirror `shapeMimoCodeEntry` /
  `mimoCodeCommandEndpoint` / the `scanMimoCode` enabled-skip added in this PR), OR — better —
  promote those mimo helpers to a SHARED `mcp`-family shaper and switch opencode (and the other
  OpenCode-format clients) onto it, retiring the per-client divergence. Decide whether the
  string-`command` clients in the shared set are genuinely string-format or also array (audit each).
- Add an `opencode` extract case + populate `OpenCodeConfigPath` in `internal/gui/extract_manifest.go`
  and `internal/cli/manifest.go`.
- Consider a `disabled`-aware filter for the JSON `mcpServers` family in the same pass.

## Why not fixed here

PR #420 scope is the mimocode adapter; opencode behavior is explicitly PROTECTED and the helper is
shared across ~30 clients. The orchestrator decides priority and whether to widen scope.
