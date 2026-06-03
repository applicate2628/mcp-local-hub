---
title: cli supervise stateDirFunc honors MCPHUB_STATE_DIR_OVERRIDE in production (ungated test seam)
severity: low
found-by: roadmap-audit during PR #264 func-hook test-seam analysis
found-on: 2026-06-03
project: mcp-local-hub
context: adjacent-finding
status: open
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

## Why this matters

The api-layer env fallback (`api.DaemonStateDir`'s `MCPHUB_STATE_DIR_OVERRIDE`
path) IS gated out of release binaries by the `test_state_path_env` build tag
(CLAUDE.md "State path"; the watchdog plan §16/§50 even asserts the symbol's
absence via `go tool nm`). The CLI-layer copy here defeats that intent: the
same env var that is supposed to be test-only still redirects the *production*
supervisor's state dir.

This is the same hazard class as
`2026-05-20-tests-leak-state-into-production-logs` (closed by #264): an operator
who has a stray `MCPHUB_STATE_DIR_OVERRIDE` in their shell/profile (e.g. left
over from a test session) would silently send the production supervisor's
`supervisor-intent.json` / `supervisor-state.json` / events log to the override
path instead of `%LOCALAPPDATA%\mcp-local-hub\` — a plausible cause of
"dashboard broke after reboot"-style confusion where state appears to vanish.

Severity is **low** because on a single-user solo host the env is the user's own
and the supervisor runs as that user; there is no cross-tenant escalation. The
concern is operator-surprise and inconsistency with the gating effort, not a
privilege boundary.

## Suggested fix (not yet implemented)

Make the CLI-layer seam consistent with the api-layer one — one of:

1. Gate the `MCPHUB_STATE_DIR_OVERRIDE` branch behind the `test_state_path_env`
   build tag (split `stateDirFunc` into a tagged test variant plus an untagged
   production variant that calls `api.DaemonStateDir()` directly), mirroring the
   api-layer treatment. Release `go build` (no tag) then ignores the env.
2. OR, if a production state-dir override is genuinely wanted as an operator
   feature, rename it to a non-"test" env (e.g. `MCPHUB_STATE_DIR`), document it
   as a supported operator override, and keep `MCPHUB_STATE_DIR_OVERRIDE` for
   the gated test path only.

Option 1 matches current intent (the env is documented test-only everywhere
else). Option 2 is a deliberate scope expansion requiring an operator-docs
update.

## Related code

- `internal/cli/supervise.go:148-164` — the ungated seam plus its rationale comment
- `internal/api` `DaemonStateDir` + the `test_state_path_env` build-tag gating
- CLAUDE.md "Supervisor (v0.5.0) → State path" — documents the env as test-only
- `work-items/bugs/closed/2026-05-20-tests-leak-state-into-production-logs.md` —
  sibling leaked-env hazard (closed by #264)
