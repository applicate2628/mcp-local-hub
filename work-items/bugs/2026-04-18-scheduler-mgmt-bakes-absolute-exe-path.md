---
title: SchedulerUpgrade and WeeklyRefreshSet still bake os.Executable() into scheduler task Command
severity: medium
found-by: backend-engineer
found-in-phase: PATH-based resolution implementation (adjacent finding)
affected-surface: internal/api/scheduler_mgmt.go:71, internal/api/scheduler_mgmt.go:110
context: adjacent-finding
status: open
---

## Summary

`internal/api/install.go` was changed to reference `mcphub` by short name
(`mcphub.exe` on Windows, `mcphub` elsewhere) in scheduler task `<Command>`
fields and Antigravity `RelayExePath`. Two sibling call sites in
`internal/api/scheduler_mgmt.go` were intentionally left out of scope for
the in-flight change but still bake the absolute path returned by
`os.Executable()`:

- `SchedulerUpgrade()` (line ~71) — re-creates every `mcp-local-hub-*` task
  with `Command: exe` where `exe` is `os.Executable()`. This is the command
  the user is expected to run after moving the binary, so if it re-bakes
  an absolute path it defeats the whole point of the short-name model.
- `WeeklyRefreshSet(schedule)` (line ~110) — creates the hub-wide weekly
  refresh task with `Command: exe`. Same concern.

## Why this matters

After `install.go` changes, fresh installs are portable: moving mcphub
does not break anything as long as the binary stays on PATH. But
`scheduler upgrade` and `mcphub scheduler weekly-refresh set <schedule>`
will still bake the old absolute path and produce tasks that only work
from the specific mcphub location they were registered against.

## Suggested fix

Replace both `Command: exe` assignments with a reference to the same
`mcphubShortName` variable added to `install.go`. Consider moving that
variable to a shared location if it starts being used in more than one
file (e.g. `internal/api/mcphub_name.go`, or an existing `api.go`). Do
not duplicate the `runtime.GOOS` switch in each consumer.

Also: `SchedulerUpgrade`'s doc-comment still advertises "moving the binary
to a new location" as a supported use case — with the short-name model
that specific workflow is now handled by `mcphub setup`, not by
`SchedulerUpgrade`. The doc should be updated accordingly.
