# Backlog: broad GUI writeAPIError path-leak audit (beyond the state-dir/IPC class)

Filed: 2026-07-15
Priority: P3 (security-hygiene; per-handler judgment needed; POSIX abs paths + Windows config paths)
Source: phase-1 audit finding 4 follow-through. The DaemonStateDir + supervisor-IPC-state
leak class is CLOSED (PR #552: STATE_DIR_FAILED / STATE_READ_FAILED / RESPAWN_SETUP_FAILED /
IPC_FAILED / STATUS_FAILED all redacted + `state_dir_error_redaction_test.go` guard). This
tracks the REMAINING surface the same finding named ("no central error-redaction; raw path
leaks") that is NOT state-dir-derived.

## The surface

`internal/gui/*.go` has ~40 `writeAPIError(w, err, ...)` sites passing a raw error variable.
Many wrap file/vault/registry reads under the state or config tree, so on a read/permission
failure the response body can embed an absolute path. Candidate handlers (not yet audited):

- `backups.go` / `backups_actions.go` — BACKUPS_LIST/CLEAN/PREVIEW/RESTORE/DELETE_FAILED
- `secrets.go` — SECRETS_LIST/SET/DELETE_FAILED (vault path)
- `workspaces.go` — REGISTRY_LOAD_FAILED (workspaces.yaml path)
- `settings.go` — SETTINGS_LIST/SET_FAILED (gui-preferences path)
- `dismiss.go` / `groups.go` / `logs.go` / `install.go` / `scan.go` / `lsp_*` — assorted

## Why it needs per-handler judgment (NOT a blanket redact)

The phase-1 audit EXPLICITLY warned against over-correcting: finding 6 EXEMPTED the backups
restore/snapshot path echoes because they are paths the operator already chose and owns, and
the endpoint is same-origin ("Noting it so the redaction sweep does not over-correct and strip
these legitimate operator-facing path echoes"). So each site must be classified:

1. **Redact** — the path is incidental to an internal failure (state/config file the operator
   never named); route through `writeAPIErrorRedacted`.
2. **Keep** — the path is an operator-facing value they chose (backups target, a root they
   typed); the audit's exemption applies. Leave it, document why.

## Fix

Per-handler pass: for each raw-`err` `writeAPIError` site, decide redact-vs-keep, route the
redact ones through `writeAPIErrorRedacted`, and extend the `statePathDerivedErrorCodes` guard
(or a sibling) with the codes that are redacted. Add a per-handler test where a forced read
failure would embed a path.

## Why P3

The high-reachability, non-operator-chosen leaks (the DaemonStateDir/IPC class, esp. the
dashboard-polled `/api/status`) are already closed. The remainder are lower-frequency failure
paths and several are legitimately operator-facing. Bundle with the next GUI-handler-hygiene pass.
