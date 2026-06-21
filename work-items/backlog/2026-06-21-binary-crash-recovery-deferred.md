# Binary Crash Recovery Deferred

Status: deferred

## Feature

Automatically restore a missing canonical `mcphub` binary after a crash between
the rename-aside `MoveFileEx` steps: after the current binary has been moved to
`*.old-<timestamp>`, but before the staged `*.new` binary is promoted into the
canonical path.

## Why Deferred

The value is low. The vulnerable crash window is tiny, and
`SweepOldBinaries` keeps the five newest rename-aside files, so an operator can
manually copy a kept aside back into place if the canonical binary is missing.

The trigger design is flawed. When the canonical `mcphub.exe` is missing,
autostart cannot launch it to run startup recovery; only a surviving aside
launch can trigger recovery. A useful revival needs a surviving-launcher trigger
rather than relying on the missing canonical binary to start itself.

The complexity tail is high. The recovery design drew roughly 15 bot findings
over six review rounds, so it is not worth shipping with the current low-value
trigger.

## Revival Notes

- Candidate ranking used the `.old-<timestamp>` suffix timestamp as the primary
  authority, not file mtime.
- Restore copied aside bytes into a temporary file, fsynced it, then atomically
  renamed the temp into the canonical target.
- Aside-launch recovery needed a canonical-from-aside derivation.
- Recovery ran under the supervisor singleton lock to avoid concurrent restore
  or sweep races.
- Candidate filtering accepted only regular files and ignored staged `.new`
  files.
- Stale restore temp files were retryable and disposable on the next pass.
- Cross-device restore could not depend on hard links; `EXDEV` and no-hard-link
  environments required copy-based restore.
