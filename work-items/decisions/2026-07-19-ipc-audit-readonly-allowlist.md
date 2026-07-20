---
status: proposed
date: 2026-07-19
---
# IPC audit-row emission: read-only commands skip the audit trail (fail-safe allowlist)

## Context
The supervisor IPC audit stream (`supervisor-events.log`, `event:"ipc-command"`) was flooded 88-100% by
read-only `status` polls (GUI 5s StatusPoller + restart-watcher), evicting real lifecycle events. Bug
2026-07-16 (P1).

## Decision
`api.IPCCommandIsReadOnly(cmd string) bool` (in `internal/api/supervisor_ipc.go`) is the SINGLE OWNER of
"read-only IPC command". It is a FAIL-SAFE ALLOWLIST: returns true only for the enumerated read-only set
(today exactly `{"status"}`); every other command — including unknown/future verbs — is treated as NOT
read-only and KEEPS its audit row. The per-request emit at `internal/cli/supervise.go:1746` consumes the
predicate: `if deps.events != nil && !api.IPCCommandIsReadOnly(req.Cmd) { emit }`.

## Consequence / convention future IPC commands must follow
A new IPC command is audited by default (fail-closed). Only a pure query that a handler answers in the
pre-`reconcileReady`-gate switch (`supervise.go:1804`) may be added to the allowlist, and adding it there
REQUIRES updating the taxonomy table test that pins `IPCCommandIsReadOnly` against the dispatch switch
(the C1 drift guard). Mutating verbs (post-gate switch, `:1839`) NEVER go in the allowlist.

## Alternatives rejected
- Debug-severity + drop-unless-verbose: still emits, still costs the write; less clean.
- Rate-limit/aggregate identical rows: more code (the events.go drop-reporter pattern) for the same effect.
