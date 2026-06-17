---
title: TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation collides with installed time daemon on port 9128
severity: low
found-by: g4-phase3 r6 verification on PR #157
found-on: 2026-05-12
project: mcp-local-hub
context: pre-existing master flake
status: closed
related-pr: pending Phase 3 (feat/g4-phase3-resolver-sessions-aggregator)
---

# Pre-existing test flake: install_intent_test.go assumes a clean port 9128

## Reproduction

On any developer box that has run `mcphub install time` (the canonical
demo daemon), the daemon binds 127.0.0.1:9128 via Task Scheduler.
While that daemon is running:

```bash
go test -count=1 -run '^TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation$' ./internal/api/
```

returns:

```
--- FAIL: TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation (0.01s)
    install_intent_test.go:823: error chain: want ErrIdentityOversize, got port 9128 already in use (needed for daemon time/default)
```

The audit IS rigged to fail with `ErrIdentityOversize`, but `Install`
short-circuits with the port-collision check FIRST (preflight order:
binary check → port check → audit). The test expects audit failure
ordering but the production code does ports-then-audit.

## Why this is pre-existing

`install_intent_test.go` has NOT been touched on
`feat/g4-phase3-resolver-sessions-aggregator` vs `origin/master`:

```bash
$ git diff origin/master..HEAD -- internal/api/install_intent_test.go
# (empty)
```

Confirmed by stashing Phase 3 r6 changes and re-running the test on
plain `bdc211f` — identical failure. The flake exists on master.

## Why filing as adjacent finding

- Phase 3 r6 deliverable touches only `hub_mcp_aggregator.go`,
  `hub_mcp_request_id.go`, and their tests. `install_intent_test.go`
  + `install.go` are out of scope.
- Killing the running time daemon would (a) defeat the watchdog (it
  restarts within 5 min) and (b) violate the user's memory rule about
  not killing their installed processes.
- A correct fix requires either reordering preflight (audit-before-
  port-check) or scoping the port check to ports specified by the
  test fixture (not embed-FS `time` manifest's hardcoded 9128).

## Suggested fix

Option A (least invasive): the test substitutes an alternative
preflight path via a test-only hook that skips port-check when the
audit is rigged to fail. Mirrors the pattern of
`testCanonicalMcphubPathOverride` for the binary check.

Option B (better but riskier): reorder preflight so the audit-first
guarantee documented in plan §62 actually holds — audit failures must
short-circuit BEFORE any port probe. The current order risks the
audit-write side-effect being elided whenever the ports happen to be
in use, masking publication-safety failures.

## Related code

- `internal/api/install.go:1156-1170` — port preflight check that
  fires before the audit recordingAuditWriter is consulted.
- `internal/api/install_intent_test.go:804-837` — the test that
  expects audit-first ordering.
- `internal/api/install.go:1214-1221` — `portInUse` (DialTimeout
  probe).

## Workaround for the Phase 3 PR

Skip this test on local pre-push verification when a `mcphub time`
daemon is detected on port 9128. CI (manual-only on `workflow_dispatch`)
runs on a clean Windows runner with no installed daemons, so it
passes there.

## Resolution (closed 2026-06-17)

Fixed-in: already-fixed (pickFreeLocalPort + AST guard TestNoLiveBandLiteralReachesKillOrListenSink); verified 2026-06-17
