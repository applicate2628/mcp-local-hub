# Bug: Audit-lock helper acquisition is flaky under the broad GUI suite

- id: 2026-08-10-audit-lock-helper-broad-flake
- context: 2026-08-10-windows-console-opt-in
- status: open
- severity: medium
- area: internal/gui/audit_lock_terminal_worker_test.go
- found-by: qa-engineer

The first candidate broad run failed because the contained helper did not
acquire the occurrence flock; an identical broad candidate rerun and the
immutable `HEAD` broad run did not reproduce it. The exact test passes on both
candidate and `HEAD`. Preserve the failing run as a flaky finding; engineer a
deterministic acquisition window before using this test as a must-not-break
gate. A green rerun does not close the finding.
