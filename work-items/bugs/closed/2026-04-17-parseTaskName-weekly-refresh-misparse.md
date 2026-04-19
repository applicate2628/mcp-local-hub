---
title: parseTaskName mis-splits "<server>-weekly-refresh" tasks because it splits on the LAST '-'
severity: medium
found-by: qa-engineer
found-in-phase: Phase 3A.3 / Task 5 — scheduler upgrade review
affected-surface: internal/api/status_enrich.go:45 (consumed by internal/api/scheduler_mgmt.go:43 and internal/api/status_enrich.go:24)
context: docs/superpowers/plans/2026-04-17-phase-3a3-management-cli.md (Task 5, lines 1290-1476)
status: closed
---

## Reproduction

Concrete trace against committed parseTaskName (executed during review):

```
\mcp-local-hub-serena-weekly-refresh               -> server="serena-weekly" daemon="refresh"
\mcp-local-hub-memory-default                      -> server="memory"        daemon="default"
\mcp-local-hub-serena-claude                       -> server="serena"        daemon="claude"
\mcp-local-hub-paper-search-mcp-default            -> server="paper-search-mcp" daemon="default"
```

The first row demonstrates the defect: for any task whose daemon segment itself contains a hyphen (currently only `weekly-refresh`), `strings.LastIndex(rest, "-")` splits at the wrong place.

Note on hyphenated server names (e.g. `paper-search-mcp`): this works accidentally because the daemon for global-default servers is `default` (no hyphen) and `parseTaskName` treats the last hyphen as the daemon boundary — so `paper-search-mcp-default` happens to split correctly. The parser does not actually tolerate hyphens in a server name when the daemon segment also contains a hyphen; the two cases just happen not to collide for current manifests.

## Expected vs actual

Task name convention from `internal/api/install.go:281`:

```go
Name: "mcp-local-hub-" + m.Name + "-weekly-refresh",
```

So for server `serena` the canonical task is `mcp-local-hub-serena-weekly-refresh`.

- Expected: `parseTaskName("\mcp-local-hub-serena-weekly-refresh")` → `server="serena", daemon="weekly-refresh"`
- Actual: `server="serena-weekly", daemon="refresh"`

Downstream consequences in `SchedulerUpgrade` (`internal/api/scheduler_mgmt.go`):

1. The `if dmn == "weekly-refresh"` branches at lines 55, 71, 73 are **unreachable** for any real weekly-refresh task.
2. `loadManifestForServer(manifestDir, "serena-weekly")` fails with a file-not-found (no `servers/serena-weekly/manifest.yaml` exists).
3. The task is reported as `"manifest serena-weekly: open ...servers/serena-weekly/manifest.yaml: no such file or directory"` and is **skipped entirely** — i.e. on upgrade the old weekly-refresh task stays bound to the stale exe path and the user sees a misleading error.

Downstream consequences in `enrichStatus` (`internal/api/status_enrich.go:24`): for weekly-refresh rows in `mcphub status`, `Server` is populated as `"serena-weekly"` and `Daemon` as `"refresh"`, and the manifest-port lookup falls through both lookups (no such server, no such daemon) so `Port` stays 0. This is cosmetic for weekly-refresh (it has no port) but it is still wrong data.

Note: the rest of the codebase correctly uses `strings.Contains(t.Name, "weekly-refresh")` or `containsSuffix(t.Name, "-weekly-refresh")` (see `internal/api/install.go:525,556`, `internal/cli/restart.go:55`). Only `parseTaskName` consumers are affected.

## Files involved

- internal/api/status_enrich.go:43-58 — `parseTaskName` (the misparser)
- internal/api/scheduler_mgmt.go:43-82 — Task 5 consumer (unreachable branch + failing manifest lookup)
- internal/api/status_enrich.go:20-41 — `enrichStatus` consumer (cosmetic wrong Server/Daemon for weekly-refresh rows)
- internal/api/install.go:281 — where the hyphenated daemon name is introduced

## Suggested fix (for the owning phase / implementer)

The simplest targeted fix keeps the contract and is local to `parseTaskName`: check for the `-weekly-refresh` suffix first, strip it, and return (rest, "weekly-refresh"); otherwise fall back to the existing LastIndex split. That preserves every current caller and the per-task-name convention. The plan could also record this convention as an explicit test vector in `status_enrich_test.go` so the hyphenated-daemon case is locked in.
