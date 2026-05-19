---
title: state-file helper tests write into the operator's REAL %LOCALAPPDATA%\mcp-local-hub state-dir
severity: medium
found-by: diagnostic dig during "после перезагрузки сломался дэшборд" investigation
found-on: 2026-05-20
project: mcp-local-hub
context: pre-existing test-hygiene gap, not caused by feat/v0.5.x-servers-matrix-revamp
status: open
related-pr: (none — separate from #222)
---

# Pre-existing test pollution: `go test ./internal/api/` writes into the production state-dir

## Symptom

Running `go test ./internal/api/...` (or any subset that triggers
state-file-helper tests, including the
`TestWriteStateFileAtomic_StrictModeWith*` and
`TestRealClientInitializer_HappyPath` families) writes audit events
into the operator's REAL hub-mcp.log and supervisor-events.log at
`%LOCALAPPDATA%\mcp-local-hub\`, not into a per-test temp dir.

Examples captured during 2026-05-20 dig:

`supervisor-events.log` (production):
```
{"schema_version":"1","ts":"2026-05-19T20:27:01.2793669Z","severity":"warn",
 "source":"state-file-helper","event":"state-file-write-unhardened-fallback",
 "body":{"err":"secure write: parent directory not single-user safe
  (path r:\\Temp\\TestWriteStateFileAtomic_StrictModeWithWriteCapableParent3613181051\\001\\writable-parent): ...",
  "parent":"r:\\Temp\\TestWriteStateFileAtomic_StrictModeWithWriteCapableParent3613181051\\001\\writable-parent", ...}}

{"schema_version":"1","ts":"2026-05-19T21:09:58.1303287Z","severity":"warn",
 "source":"state-file-helper","event":"state-file-write-unhardened-fallback",
 "body":{"err":"secure write: parent directory not single-user safe
  (path r:\\Temp\\TestWriteStateFileAtomic_StrictModeWithWriteCapableParent1758516891\\001\\writable-parent): ...", ...}}
```

`hub-mcp.log` (production):
```
{"err":"secure write: parent directory not single-user safe
 (path r:\\Temp\\TestRealClientInitializer_HappyPath3956462697\\001\\AppData\\Roaming\\Code\\User): ...",
 "event":"client-write-unhardened-fallback","level":"warn",
 "origin":"SecureCreateClientConfigIfMissing",
 "path":"r:\\Temp\\TestRealClientInitializer_HappyPath3956462697\\001\\AppData\\Roaming\\Code\\User\\mcp.json",
 "reason":"default-relax-on-solo-host (init-stub)",
 "ts":"2026-05-19T21:20:52.4764988Z"}
```

The `parent` and `path` fields prove the WRITE was directed at the test's
`t.TempDir()` location (`r:\Temp\TestRealClientInitializer_HappyPath...`),
but the EVENT (the audit row reporting the write) ended up in the
operator's real `%LOCALAPPDATA%\mcp-local-hub\` log.

## Impact

- **Log noise** — production audit logs now interleave real operator
  events with test events that have no meaning outside the test process.
- **Capacity** — `hub-mcp.log` and `supervisor-events.log` rotate at
  10 MB. Every `go test ./internal/api/...` run inflates these logs and
  shortens the operator's effective retention window.
- **Forensic confusion** — when investigating a real incident (as in
  this very session), the test paths in audit rows are a misleading
  signal until the operator notices the `r:\Temp\Test*` prefix.
- **NOT a security boundary breach** — the file writes themselves are
  hermetic (they go to the test's TempDir). Only the AUDIT EVENT stream
  leaks. So this is hygiene, not a confidentiality issue.

## Root cause hypothesis (UNVERIFIED — needs grep + Read)

The state-file-helper logging path (`api.LogHubMcpEvent` + the
supervisor-events appender) resolves the log file's location via
`api.DaemonStateDir()` / equivalent — which uses the OS user's real
`%LOCALAPPDATA%`. Tests that set up a `t.TempDir()`-based parent do
NOT redirect `DaemonStateDir()` to that tempdir; they only point the
SUBJECT of the write (`path`) at the tempdir, not the logging output.

There is likely a `MCPHUB_STATE_DIR_OVERRIDE` (or similar) test seam
already in the codebase for production redirection, but the state-file
helper tests don't set it.

## Reproduction

```bash
# Note current size of production log.
wc -c "%LOCALAPPDATA%\mcp-local-hub\hub-mcp.log"

# Run a known-leaking test.
go test -count=1 -run TestRealClientInitializer_HappyPath ./internal/api/

# Re-check size — should be larger.
wc -c "%LOCALAPPDATA%\mcp-local-hub\hub-mcp.log"
# Grep for the test name in the log — should appear.
grep "TestRealClientInitializer_HappyPath" "%LOCALAPPDATA%\mcp-local-hub\hub-mcp.log"
```

## Fix shape (proposed)

Either:

1. **Redirect log target per test.** Make `api.LogHubMcpEvent` resolve
   the log path through a test seam (function-pointer variable + env
   var override) that tests using `t.TempDir()` can override. Add a
   helper `apitest.IsolateStateDir(t)` that sets the seam to the
   tempdir and `t.Cleanup`s it back. Touch the state-file helper tests
   to call the helper as their first line.

2. **Refuse to write events when state-dir is a system path AND the
   binary was built with the test build-tag.** Sidestep by failing the
   `LogHubMcpEvent` write under `test_state_path_env` builds. Simpler
   to land, harder to retrofit clean isolation across the whole test
   suite.

Option 1 is the cleaner architectural change. Option 2 is the smaller
patch. Pick based on how many call sites are affected.

## Out of scope

- Cleaning the already-polluted production logs. They'll rotate out on
  their own once new events push past the 10 MB cap.
- Other tests outside `internal/api/` that may also leak (no audit
  done — grep needed before scope-expansion).

## Related

- Hub-mcp event log + rotation pattern lives at
  `internal/api/hub_mcp_log.go` (event-log writer) and
  `internal/api/watchdog_log.go` (16 KB per-entry cap + 10 MB rotation
  + `audit-rotated` self-event after rotation).
- The state-file helper tests live at `internal/api/state_file_helper_test.go`
  and `internal/api/client_write_init_test.go`.
- Memory rule that's adjacent but DIFFERENT: `feedback_kosyak_full_test_sweep_affects_real_scheduler.md`
  documents the related-but-not-identical issue where `go test ./...`
  STOPS the operator's real installed daemons via the scheduler API. That
  one is about side-effects on the real Task Scheduler; this one is
  about side-effects on the real audit log.
