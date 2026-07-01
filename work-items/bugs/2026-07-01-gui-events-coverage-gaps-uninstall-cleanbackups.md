---
status: open
severity: low
context: adjacent-finding
---

# gui-events.log operator-action coverage gaps: uninstall, install-all

## Finding (P3 — adjacent finding, partially closed)

The 2026-07-01 deep-review P3 observability fix added `gui-events.log` audit
rows (`Broadcaster.PublishOperatorAction`) for the cited operator-initiated
GUI mutations: supervisor restart, migrate, demigrate, install (single,
`POST /api/install`), secret set/rotate/delete, backup restore/delete, and
(added in the PR #476 bot-finding round) backup clean.

Two sibling mutation routes remain UNAUDITED — identified while implementing,
but not yet in an approved change surface:

- `internal/gui/install.go` — `DELETE /api/install/:server` (uninstall) and
  `POST /api/install-all` (bulk install). Both commit real fleet mutations
  (scheduler task deletion, client-config rewrite) with no gui-events.log
  row, same as the single-install path that WAS fixed.

## Already fixed (no longer open)

- `internal/gui/backups.go` — `POST /api/backups/clean` (bulk backup prune,
  global or per-client) — FIXED in the PR #476 bot-finding round (P3): a
  clean that deletes files now emits a `backup-clean` operator-action row
  (client + count + basenames). Removed from this finding's open scope.

## Why not fixed here

Per the backend-engineer adjacent-findings protocol, the approved change
surface did not include `install.go`'s uninstall/install-all handlers.
Expanding into them would widen the diff beyond the reviewed scope without a
separate verified hypothesis authorizing that expansion.

## Suggested fix (for whoever picks this up)

Same pattern already established: call
`s.events.PublishOperatorAction(action, api.CurrentOSUser(), detail)` after
each mutation commits successfully (uninstall: after `s.uninstaller.Uninstall`
succeeds, action="uninstall", detail={"server": server}; install-all: per
successful row or one summary row, action="install-all"). Both already have
the committed-mutation result in scope at the point a row should emit. No new
owner needed — `PublishOperatorAction` (internal/gui/events.go) is already the
single owner for this event class.
