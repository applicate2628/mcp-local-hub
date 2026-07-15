# Backlog: GUI state-dir-error handlers leak absolute paths on POSIX (phase-1 audit finding 4 residual)

Filed: 2026-07-15
Priority: P3 (security-hygiene; POSIX beta/preview only; the absolute path is the operator's OWN
`$XDG_STATE_HOME`/home path, not a secret — low sensitivity, but the finding's own fix text named these
sites in-scope)
Source: HEAD-reconciliation of `work-items/2026-06-17-phase1-audit-findings.md` finding 4 (sonnet
verify 2026-07-15). Finding 4 is otherwise FIXED (central `writeAPIErrorRedacted` helper shipped; the
cited supervisor-intent/overlay/manifest 500s migrated); THIS is the residual it missed.

## The leak

4 GUI handlers still return a raw `api.DaemonStateDir()` (state-dir resolution) error to the HTTP
response via `writeAPIError(w, err, ...)` instead of the redacting `writeAPIErrorRedacted`. On POSIX the
error embeds an absolute path through `internal/api/state_paths.go:286,294` (`errStateParentInsecure`,
e.g. `"<path>: owner UID %d != current UID %d"`), so the GUI response surfaces the operator's
`$XDG_STATE_HOME`/`~/.local/state/mcp-local-hub` path on a state-dir sanity failure.

Sites (all wrap the `api.DaemonStateDir()` failure):
- `internal/gui/daemon_env.go:163`
- `internal/gui/daemon_env.go:223`
- `internal/gui/daemon_env.go:355`
- `internal/gui/supervisor_restart.go:84`

(Windows uses `%LOCALAPPDATA%` + a different DACL failure path; the absolute-path leak is the POSIX
`errStateParentInsecure` string. Beta/preview hosts only, but the same class the finding closed elsewhere.)

## Fix

Route these 4 sites through `writeAPIErrorRedacted(w, err, status, code, route)` (the helper the rest of
finding 4 already uses — `internal/gui/scan.go:117`), OR collapse the `api.DaemonStateDir()` failure to a
stable opaque token (like finding 5's `path_validate.go` fix) before it reaches the wire. Keep the raw
error in the server log (`log.Printf`) for diagnosis. Add a test asserting the state-dir-failure response
body carries no absolute path.

## Why P3

The leaked value is the operator's own state-dir path (not a secret / not another user's data), and the
trigger is a state-dir DACL/ownership sanity failure (rare). Same-class leaks with real sensitivity
(secrets, other-manifest config) are already closed. Bundle with the next GUI-handler-hygiene pass.
