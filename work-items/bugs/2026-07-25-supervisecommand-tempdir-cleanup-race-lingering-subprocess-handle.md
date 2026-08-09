---
title: TestSuperviseCommand_* tests intermittently fail t.TempDir() auto-cleanup with "unlinkat ...: The directory is not empty" on Windows
severity: low
found-by: backend-engineer, sub-increment 2a (MCP front-daemon port ownership) regression sweep
affected-surface: internal/cli/supervise*_test.go (any TestSuperviseCommand_* test that spawns a real subprocess into a t.TempDir()-rooted state dir)
context: adjacent-finding
status: open
---

## What happened

Across several `go test ./internal/cli/...` (and `-tags=test_state_path_env`)
runs during sub-increment 2a's regression sweep, a DIFFERENT
`TestSuperviseCommand_*` test failed each time with the identical cleanup
error, never a logic/assertion failure:

```
testing.go:1464: TempDir RemoveAll cleanup: unlinkat R:\Temp\<TestName><rand>\001\hardened-parent: The directory is not empty.
```

Observed test names across independent runs this session (not exhaustive —
this is a sample, not the full set of tests that can exhibit it):

- `TestSuperviseCommand_AcquiresLockAndExitsOnSignal`
- `TestSuperviseCommand_SweepsOldBinariesOnStartup`
- `TestSuperviseCommand_SkipsOldBinarySweepWhenExecutableUnavailable`
- `TestSuperviseCommand_ReaperFailureDoesNotBlockStartup`

**Confirmed pre-existing, unrelated to the change in progress.** This is the
same class the accepted Increment 1b session on this branch already
informally identified in
`work-items/active/2026-07-25-mcp-front-daemon/status.md` ("a previously-
undocumented ... nondeterministic Windows TempDir-cleanup race
(`TestF1_GateClearedClearedOnSettle`, `TestSuperviseCommand_
AcquiresLockAndExitsOnSignal`), confirmed pre-existing (reproduces on the
untouched tip under a DIFFERENT test name each run) and confirmed NOT
route-caused (both pass 5/5 in isolation)") — that note named 2 examples;
this session observed 2 additional distinct test names exhibiting the exact
same symptom, all traced to the same identical error string. Every specific
test name this session named this way was individually re-run in isolation
(`-run <name> -count=3`) against BOTH the working tree and, via `git stash
-u`, the unmodified accepted branch tip (`301081a2`) — the failure is either
absent in isolation (low reproduction rate outside a large concurrent run)
or, when present, identical on both trees.

## Root cause (not yet investigated — out of scope for sub-increment 2a)

Not root-caused. The shared symptom (a `t.TempDir()`-managed directory is
non-empty at Go's own deferred cleanup time, specifically the fixed leaf
`hardened-parent` under it) across multiple, otherwise-unrelated
`TestSuperviseCommand_*` tests strongly suggests a shared test HELPER (not
each test's own logic) spawns a real subprocess (`mcphub supervise` or a
stand-in) that writes into `hardened-parent/` and is not always guaranteed
to have released every file handle / exited before the test function
returns and Go's `t.Cleanup`-registered `RemoveAll` runs. On Windows,
`RemoveAll`/`unlinkat` fails outright (rather than retrying) when a
directory entry is still open by any process — POSIX-style `rm -rf`
semantics (unlink-while-open succeeds) do not apply. A real root-cause pass
would need: (1) identify the shared spawn/wait helper these tests use, (2)
confirm whether it waits for full subprocess exit (not just a signal it
sent) before returning control to the test, (3) decide whether the fix is a
harness-level explicit wait/retry-with-backoff around the `RemoveAll`, or a
change to the spawn helper's own shutdown wait.

## Why not fixed here

Out of the approved change surface for sub-increment 2a (MCP front-daemon
port ownership) — this is a shared TEST HARNESS timing issue across the
`TestSuperviseCommand_*` family, unrelated to the mcp_front.port setting,
the route daemon's port source, or the `--reconcile-mcp-front` command.
Filed per the adjacent-findings protocol instead of expanding scope.
