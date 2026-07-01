---
status: open
severity: low
context: adjacent-finding
---

# `mcphub trust`/`untrust` CLI verbs mutate the trusted-roots boundary with no audit trail

## Finding (P4 — adjacent finding, out of scope for this fix)

Discovered while implementing the deep-review round-2 P3 fix for
`internal/gui/lsp_trusted_roots_handler.go` (GUI POST/DELETE
`/api/lsp/trusted-roots` now emit a `gui-events.log` `operator-action` row via
`Broadcaster.PublishOperatorAction` on both add and remove).

`codegraph_callers` on `api.BlessDefaultTrustedRoot` / `api.RemoveDefaultTrustedRoot`
shows a second, independent caller pair besides the fixed GUI handler:

- `internal/cli/trust.go:133` `runTrustAdd` → `api.BlessDefaultTrustedRoot`
  (`mcphub trust <path>`)
- `internal/cli/trust.go:148` `runUntrust` → `api.RemoveDefaultTrustedRoot`
  (`mcphub untrust <path>`)

Per `trust.go`'s own doc comment, this CLI verb group mutates the SAME shared
authorization boundary the GUI handler does — the store gates first-touch
auto-register for both the GUI LSP router and the serena router
(`WorkspaceRootTrusted`). A trust/untrust issued from the CLI leaves no audit
row anywhere: `Broadcaster.PublishOperatorAction` is a GUI-process-only
mechanism (it writes to the in-memory SSE broadcaster the GUI's
`gui-events.log` persist drain reads from), and the CLI `mcphub trust`/
`untrust` commands run as a separate short-lived process with no `*Server` /
`*Broadcaster` in scope.

## Why not fixed here

The approved change surface for this fix was the two GUI HTTP handlers
(`lsp_trusted_roots_handler.go:90` and `:114`) plus the `strict_mode.go`
success-path finding. `internal/cli/trust.go` is a different process
entry-point with no `Broadcaster` available, so mirroring the exact
`PublishOperatorAction` pattern is not a drop-in — extending audit coverage
to it needs its own design decision (e.g. does it get a
`supervisor-events.log` row via `api.OpenSupervisorEventLog` instead, since
that mechanism — unlike `gui-events.log` — is a process-agnostic on-disk
JSONL file any short-lived CLI can open+emit+close, per the precedent in
`internal/cli/supervise_ensure_alive.go`'s `emitLivenessEvent`). That is a
scope decision for whoever picks this up, not an implementer judgment call.

## Suggested fix (for whoever picks this up)

Mirror the `emitLivenessEvent` / `emitStrictModeChangedEvent` idiom (both in
`internal/cli`): open `api.OpenSupervisorEventLog(filepath.Join(stateDir,
api.SupervisorEventLogFileLeaf))`, emit a best-effort `info`-severity event
(e.g. `event: "trust-root-add"` / `"trust-root-remove"`, `source:
api.SupervisorEventSourceLifecycle` or a new dedicated source), close. Body:
the root path + resulting trusted-root count (same detail shape as the GUI
fix). A missing/failed emit must stay non-fatal — these are CLI commands
whose success must not depend on the audit log's availability.
