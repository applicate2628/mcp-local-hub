---
title: 8 of 19 tests in supervise_lostchild_f1_f3_test.go fail deterministically on this dev host, uniformly at ~3.0s
severity: low
found-by: backend-engineer, sub-increment 2a (MCP front-daemon port ownership) regression sweep
affected-surface: internal/cli/supervise_lostchild_f1_f3_test.go (TestF1_*/TestF3_* families)
context: adjacent-finding
status: open
---

## What happened

While running the internal/cli regression safety net for an unrelated change
(sub-increment 2a of the MCP front-daemon decision,
work-items/decisions/2026-07-25-increment2-mcp-front-port-ownership.md), the
following 8 tests in `internal/cli/supervise_lostchild_f1_f3_test.go` failed,
every one taking almost exactly 3.00-3.01s (the other 11 tests in the same
file pass in well under 0.21s):

```
--- FAIL: TestF1_ForeignRespawnHeldNoSpawnNoIncrement (3.01s)
--- FAIL: TestF1_OwnSquatterReapedThenSpawnsNoIncrement (3.00s)
--- FAIL: TestF1_UnverifiedSpawnsAsToday (3.01s)
--- FAIL: TestF1_LoopNotBlockedByWorkerClassify (3.01s)
--- FAIL: TestF1_GatesEvIntentUpdateRespawn (3.01s)
--- FAIL: TestF3_UnflaggedManualRestartUnconditional (3.01s)
--- FAIL: TestF1_GatesEvStartRespawn (3.01s)
--- FAIL: TestF1_ColdStartEvStartFreePortSpawns (3.01s)
```

Sample failure detail (`spawn count` assertions expecting 1, observing 0):

```
supervise_lostchild_f1_f3_test.go:603: spawn count (unverified fail-open via gateCleared) = 0, want 1
supervise_lostchild_f1_f3_test.go:661: daemon B spawn count while A's classify blocks the worker = 0, want 1
```

Two of the failures assert on a specific `supervisor-events.log` event
(`daemon-port-squatter-foreign`) that never appears — instead the log shows an
UNRELATED `daemon-spawn-held-missing-path` event (`the mcphub program file is
missing at C:\mcphub.exe`), suggesting the test's fake/real binary path
resolution on this host does not produce the squatter-classification path the
test expects, and/or the expected respawn simply never happens before the
test's own timeout fires.

**Confirmed pre-existing, unrelated to the change in progress** — reproduced
three separate ways:

1. `git stash -u` (every tracked AND untracked change from this session's
   sub-increment 2a work removed, back to accepted branch tip `301081a2`) +
   `go test -run 'TestF1_ForeignRespawnHeldNoSpawnNoIncrement|TestF1_OwnSquatterReapedThenSpawnsNoIncrement' ./internal/cli/` —
   same 2 failures.
2. Same stash technique with the full 8-name filter
   (`TestF1_|TestF3_`) — identical 8 failures, identical ~3.0s durations.
3. Repeated 3x on the branch WITH this session's changes present
   (`go test -run 'TestF1_|TestF3_' ./internal/cli/ -count=1` run 3
   times) — the SAME 8 tests fail every time, the other 11 pass every time.
   Deterministic (not flaky-random) on this host, in both isolation
   (`-run` filtered to just this file) and inside the full `go test ./...`
   sweep (where a DIFFERENT-looking subset of these same ~19 tests failed
   across two consecutive full-suite runs earlier in this session — the
   full-suite interleaving with other packages' resource usage appears to
   perturb which subset of the ~3s-timeout tests lands inside vs. outside
   its deadline, but the underlying defect — something with a ~3s budget
   not completing in time on this host — is the same across every
   observation).

This matches the "documented flaky/crashy on this host" characterization
already noted informally in
work-items/active/2026-07-25-mcp-front-daemon/status.md's Increment 1b
entry ("`internal/cli` full sweep intentionally NOT run... confirmed
pre-existing via the same stash test, unrelated to this diff") and in
CLAUDE.md's PR-workflow section, but as far as could be found this specific
test file/family was never filed as its own dedicated bug-registry entry
before now.

## Root cause (not yet investigated — out of scope for sub-increment 2a)

Not root-caused. The uniform ~3.0s duration across all 8 failures strongly
suggests a shared timeout/deadline constant somewhere in the test helper
scaffolding these 8 (but not the other 11) tests use, and that this host's
process/goroutine scheduling, spawn latency, or file-path resolution
(`C:\mcphub.exe` appears to be an assumed/fixture path that may not resolve
the way the test expects on this machine) does not complete the awaited
condition before that deadline. A real root-cause would need: (1) reading
the shared test helper(s) these 8 tests use vs. the 11 passing ones, (2)
identifying the exact timeout constant, (3) determining whether the fix is a
test-only timing/fixture-path adjustment or points at a genuine product
defect in the lost-child squatter-reap path these tests exercise.

## Why not fixed here

Out of the approved change surface for sub-increment 2a (MCP front-daemon
port ownership) — `supervise_lostchild_f1_f3_test.go` covers the supervisor's
lost-child/squatter-reap classification, a subsystem unrelated to the
mcp_front.port setting, the route daemon's port source, or the
`--reconcile-mcp-front` command. Filed per the adjacent-findings protocol
instead of expanding scope. Excluded via `-skip` (alongside the
already-filed `TestCleanupAggressive_IncludeClassFlagOverridesWithWarning`
crash, work-items/bugs/2026-07-25-findwindowsexeextensionend-index-out-of-range.md)
for this session's `go test ./...` gate re-verification.
