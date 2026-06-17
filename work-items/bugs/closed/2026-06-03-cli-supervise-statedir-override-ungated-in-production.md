---
title: cli supervise stateDirFunc honors MCPHUB_STATE_DIR_OVERRIDE in production (ungated test seam)
severity: low
found-by: roadmap-audit during PR #264 func-hook test-seam analysis
found-on: 2026-06-03
project: mcp-local-hub
context: adjacent-finding
status: closed
related-pr: (none — surfaced while closing the #264 test-infra batch)
---

# cli supervise stateDirFunc reads MCPHUB_STATE_DIR_OVERRIDE in shipped binaries

## Symptom

`internal/cli/supervise.go:159-164` resolves the supervisor state dir via:

```go
var stateDirFunc = func() (string, error) {
	if override := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"); override != "" {
		return override, nil
	}
	return api.DaemonStateDir()
}
```

This env-read is NOT build-tag-gated. The adjacent comment
(supervise.go:148-158) frames it as a "supervise-side test seam" deliberately
placed at the CLI layer so the production `api` surface stays free of test-only
env resolution — but the CLI layer ships in release binaries, so a production
`mcphub supervise` honors `MCPHUB_STATE_DIR_OVERRIDE` at runtime.

`stateDirFunc` is not called only by `mcphub supervise`. Every production
caller inherits the ungated override, so the blast radius spans several
commands:

- `internal/cli/supervise.go:362` — `mcphub supervise`
- `internal/cli/migrate_serena.go:287,976,1059,1108` — `mcphub migrate serena`
- `internal/cli/overlay_prune_orphans.go:48` — overlay orphan-prune
- `internal/cli/overlay_quarantine.go:66` — overlay quarantine

Fixing the seam at the `stateDirFunc` var definition covers all of them at
once, but the fix's validation must exercise each caller (not just supervise)
to confirm none separately re-reads the env.

## Why this matters

`MCPHUB_STATE_DIR_OVERRIDE` is purely a CLI/test seam — it is read by the cli
`stateDirFunc` (supervise.go) and by the supervisor IPC test pipe-discriminator,
and nowhere in the `api` package's own state-path resolution. (The api-layer's
`test_state_path_env`-gated fallback in `internal/api/state_paths_envfallback.go`
is a DIFFERENT mechanism: a `LOCALAPPDATA` → `USERPROFILE` chain consulted only
when the Known-Folder stub fails — it never reads `MCPHUB_STATE_DIR_OVERRIDE`.)
The hazard is that this test-seam env is read UNGATED at the CLI layer, so a
release `mcphub supervise` honors a test-only env at runtime even though the
binary was built without `test_state_path_env`.

This is the same hazard class as
`2026-05-20-tests-leak-state-into-production-logs` (api-side fixed by #264,
gui-side leak still open): an operator who has a stray `MCPHUB_STATE_DIR_OVERRIDE`
in their shell/profile (e.g. left
over from a test session) would silently send the production supervisor's
`supervisor-intent.json` / `supervisor-state.json` / events log to the override
path instead of `%LOCALAPPDATA%\mcp-local-hub\` — a plausible cause of
"dashboard broke after reboot"-style confusion where state appears to vanish.

Severity is **low** because on a single-user solo host the env is the user's own
and the supervisor runs as that user; there is no cross-tenant escalation. The
concern is operator-surprise and inconsistency with the gating effort, not a
privilege boundary.

## Suggested fix (not yet implemented)

Make the seam production-inert while keeping it active for the DEFAULT test
build — one of:

1. Drop the env read from production and install it as a test-only hook. The
   build-tag route is a TRAP: the cli IPC + supervise tests set
   `MCPHUB_STATE_DIR_OVERRIDE` under the default untagged `go test ./...` that CI
   runs, so gating it behind `test_state_path_env` would send them back to the
   real state dir — exactly the regression #264 fixed for the IPC pipe.
   Concretely: the production `stateDirFunc` calls `api.DaemonStateDir()`
   directly (no env read), and test setup reassigns the package `var
   stateDirFunc` to the env-reading variant from a TestMain, mirroring #264's
   `EnableSupervisorIPCTestPipeIsolation`. The env stays honored under `go test`
   (tagged or not); a release binary never reads it.
2. OR, if a production state-dir override is genuinely wanted as an operator
   feature, rename it to a non-"test" env (e.g. `MCPHUB_STATE_DIR`), document it
   as a supported operator override, and keep `MCPHUB_STATE_DIR_OVERRIDE` for
   the test-only seam.

Option 1 matches current intent (the supervise.go comment already frames the
env as a test seam, not an operator feature — there is no user-facing doc
contract for it). Option 2 is a deliberate scope expansion requiring an
operator-docs update.

## Related code

- `internal/cli/supervise.go:148-164` — the ungated seam plus its rationale comment
- `internal/api` `DaemonStateDir` + `state_paths_envfallback.go` — the
  `test_state_path_env`-gated api-layer fallback (LOCALAPPDATA → USERPROFILE),
  which never reads `MCPHUB_STATE_DIR_OVERRIDE`
- CLAUDE.md "Supervisor (v0.5.0) → State path" — documents the
  `test_state_path_env` build-tag gating of the api-layer state path, but does
  NOT mention `MCPHUB_STATE_DIR_OVERRIDE`; that env has no doc-level contract
  (its only in-tree definition is the supervise.go comment in the first
  bullet) — the documentation gap is itself part of this bug
- `work-items/bugs/2026-05-20-tests-leak-state-into-production-logs.md` —
  sibling leaked-env hazard (api-side fixed by #264; gui-side leak still open)

## Resolution (closed 2026-06-17)

Fixed-in: #318 prod gate (env override behind test_state_path_env build tag) + Wave-3 gui TestMain fence; gui-side residual closed 2026-06-17
