---
title: mimocode gopls direct-LSP cleanup logs false success on a non-write-target-layer entry
severity: low
found-by: backend-engineer
found-in-phase: PR #420 r11 findings (mimocode multi-layer adapter — findings 3 & 4)
affected-surface: >
  internal/api/register.go:323 (gopls direct-LSP cleanup) →
  matchingDirectGoplsMCPEntries (register.go:368) →
  client.RemoveEntry (register.go:341), consuming
  internal/clients/mimocode.go AllStdioEntries (merged-view, all layers)
context: adjacent-finding
status: open
---

## Summary

The post-register direct-LSP cleanup has a `gopls` branch
(`internal/api/register.go:322-329`) that, when the cleaned languages
include `go`/`gopls`, calls `client.AllStdioEntries()`, runs
`matchingDirectGoplsMCPEntries` over the result, and then
`client.RemoveEntry(entry.Name)` for each match (`register.go:341`).

For the mimocode adapter, `AllStdioEntries` reads the **merged view across
all config layers** (config.json < mimocode.json < mimocode.jsonc <
MIMOCODE_CONFIG < overlay < inline). `RemoveEntry` → `deleteMember`
deletes from the **write target (`o.path` = mimocode.json) ONLY**. So a
direct `gopls`-MCP entry defined in a NON-write-target layer (a lower
`config.json` or a higher `mimocode.jsonc`/MIMOCODE_CONFIG/overlay/inline)
is matched, `RemoveEntry` is called, cleanup logs success, but the entry
STAYS ACTIVE and re-emerges via the merge.

This is the SAME defect class as PR #420 finding 4 (which was fixed for
`FindStdioLanguageServerEntries` — the `mcp-language-server` cleanup — by
restricting matches to write-target-defined names via
`mimoCodeFileDefines(o.path, name)`). The gopls branch consumes the
SHARED, non-destructive-also `AllStdioEntries` instead, which must stay
full-merged for its OTHER consumer — the orphan-process kill-pattern
derivation `patternsFromClientStdio` (`internal/api/cleanup.go:566`),
where a lower-layer entry IS a real orphan source to detect.

## Why it was NOT folded into PR #420 r11

Fixing it requires write-target scoping at the `register.go` gopls path,
which is a SHARED api-layer cleanup surface used by EVERY adapter — a
mimo-specific filter there would need either:

- a `clients.Client` interface concept of "writable layer" (rejected —
  no shared-interface change in this PR; the other 15+ adapters are
  single-file and have no such concept), or
- a mimo-type-assertion inside shared `register.go` (rejected — protected
  shared surface, and an anti-pattern: ownership leaks out of the adapter).

`AllStdioEntries` itself cannot carry the write-target filter because its
non-destructive kill-pattern consumer requires the full merged view.

## Realistic blast radius

Low. A `gopls` MCP entry is normally defined in exactly one layer, and the
hub writes the relay/HTTP shape (no `command`), so a direct `gopls` stdio
entry is operator-authored. The user-visible symptom is a cosmetic
false "removed" log line plus a gopls entry that survives the cleanup —
no data loss, no crash.

## Possible fix directions (for the orchestrator to prioritize)

1. Add a narrow `clients.Client` capability (e.g. an optional
   `WriteTargetDefines(name) (bool, error)` probed via a type assertion in
   the gopls branch) so the destructive gopls cleanup can scope to the
   write target the same way finding 4 did — without changing every
   adapter (default: not implemented → treat all as deletable, preserving
   today's single-file-adapter behavior).
2. Route the gopls destructive cleanup through a write-target-scoped
   variant of `AllStdioEntries` (a separate method) rather than the
   shared merged-view one.

Either is a separate, reviewed change; this entry records the gap so it is
not silently lost.
