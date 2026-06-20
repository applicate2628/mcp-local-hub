---
status: open
severity: P3
date: 2026-06-20
slug: gui-post-handlers-unbounded-body
discovered-by: codex different-model review of PR #378 (2026-06-20)
---

# GUI POST handlers decode request bodies without a size cap

## Issue

Most `internal/gui/*.go` POST/PUT handlers decode `r.Body` with a bare
`json.NewDecoder(r.Body).Decode(...)` and NO `http.MaxBytesReader` cap — a same-origin page
could send an arbitrarily large JSON/YAML body and force unbounded decode/parse work. Sites
include (non-exhaustive): readiness.go:74, secrets.go:94/178, backups_actions.go:203/231,
cleanup.go:101/209, cleanup_aggressive.go:142, client_install_prefs.go:127, daemons.go:124/208,
demigrate.go:32, dismiss.go:34, and likely others (`grep -rn "json.NewDecoder(r.Body)" internal/gui`).

## Severity: low (local-only)

The GUI listener is bound to 127.0.0.1; the "attacker" must be a same-origin local page or a
compromised local browser context. No remote exposure. This is hygiene, not a remote
vulnerability — hence backlog/P3, not a merge blocker on any single feature PR.

## Proposed fix (one sweep)

Add a shared helper, e.g. `decodeJSONBodyLimited(w, r, &v, maxBytes) error` that wraps
`r.Body = http.MaxBytesReader(w, r.Body, maxBytes)` before `json.NewDecoder(...).Decode(...)`
and returns 413 on overflow. Apply it to every gui POST/PUT handler with a sensible per-endpoint
cap (generous for manifest YAML ~1MB; tight for secrets/prefs ~64KB). One PR, repo-wide, with a
per-endpoint oversized-body test or a shared table test.

## Note

Flagged on PR #378 (which adds the readiness + exercises the secrets endpoints) but the pattern
predates and exceeds #378; fixing only the 3 #378-touched endpoints would be inconsistent, so it
is tracked here as a standalone hygiene sweep rather than bolted onto the feature PR.
