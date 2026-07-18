---
title: process-snapshot bufio.Scanner overflows on long process lines → incomplete snapshot → VerifyProxyReady full-suite flake
severity: low
found-by: lead (side-effect while gating PR #562)
affected-surface: internal/api/cleanup.go:1267 (process snapshot scan) + internal/api VerifyProxyReady tests
status: open
---

## Symptom

Under heavy machine load (many concurrent `go test` / `codex` / `mcphub` processes,
i.e. a large process table with long command lines), `go test
-tags=test_state_path_env ./internal/api/` intermittently FAILS on:

- `TestVerifyProxyReadyForServerNames_AcceptsJSONFiniteAndHeldOpenSSE` (finite-sse, held-open-sse)
- `TestVerifyProxyReady_GenericAllowlistDoesNotReadHeldOpenBody`

accompanied by a stderr warning:

```
mcphub: warning: process snapshot scan ended early: bufio.Scanner: token too long
```

All pass in isolation (`-run TestVerifyProxyReady... ./internal/api/`) → full-suite /
heavy-load only. Observed 2026-07-18 while gating PR #562 (unrelated hub-reconcile change;
the gui half of the same run passed 213s).

## Root cause (latent, pre-existing)

`internal/api/cleanup.go:1267` warns "process snapshot scan ended early: <err>". The
underlying scan uses `bufio.NewScanner` with the DEFAULT 64 KB max token size. On a
heavily-loaded host a single process's command line (or a `tasklist`/`wmic`/`netstat`
row) can exceed 64 KB, so `Scanner.Scan()` returns `bufio.ErrTooLong` and the snapshot
ends EARLY — INCOMPLETE. A proxy-readiness / port-owner check reading that truncated
snapshot can then miss a process it should have seen, so `VerifyProxyReady` misjudges and
the test fails. Light load → short lines → no overflow → passes.

This is a real latent PRODUCTION bug, not test-only: a truncated process snapshot on a
busy host could make cleanup / port-owner / proxy-readiness logic act on incomplete data.

## Fix direction

- Replace the default-buffer `bufio.NewScanner` in the process-snapshot path with either
  `Scanner.Buffer(make([]byte, 0, 64*1024), maxTokenBytes)` sized to a realistic ceiling
  (Windows command lines can be up to 32 KB chars; allow generous headroom, e.g. 1 MB),
  OR a `bufio.Reader.ReadString('\n')` loop with no per-line cap.
- On `ErrTooLong`, do NOT silently end early with a partial snapshot: either grow + retry
  the line, or fail loud so callers know the snapshot is incomplete rather than trusting a
  truncated result.
- Add a regression test feeding a synthetic >64 KB process line and asserting the snapshot
  is complete (not truncated).

## Scope

Unrelated to PR #562 (hub adversarial-rotation); found as a gating side-effect. Low
severity (surfaces under heavy load; the flake is in the test harness's full-suite run,
but the underlying truncation is a real production data-integrity gap). Separate from the
supervise-IPC TempDir-cleanup flake (`2026-05-29-cli-supervise-ipc-tests-flaky-in-full-suite.md`).
