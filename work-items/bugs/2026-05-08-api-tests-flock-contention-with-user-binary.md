---
title: api package tests hang on flock contention when user's installed mcphub holds daemon-intent.json.lock
severity: low
found-by: backend-engineer
found-in-phase: PR #142 round-1 codex deep-sec follow-up
affected-surface: internal/api/legacy_migrate_test.go, internal/api/register_test.go
context: adjacent-finding
status: open
---

## Reproduction

1. Have the production binary `~/.local/bin/mcphub.exe` running (the user's normal background daemon/watchdog/gui session).
2. Run `go test -count=1 -timeout 8m ./...` from a clean dev tree.
3. Observe: `internal/api` package hangs at the 8-minute timeout. Stack trace points to `WriteDaemonIntent → flock.Lock()` blocked at `LockFileEx` on
   `C:\Users\<user>\AppData\Local\mcp-local-hub\daemon-intent.json.lock`.

Affected test names observed across runs:
- `TestMigrateLegacy_DedupByWorkspace`
- `TestMigrateLegacy_PartialEOFConfirmationYes`
- `TestMigrateLegacy_PreservesInPlaceReplacedEntry`
- `TestUnregister_KillsStaleProxyByPort`
- `TestRegister_RegistryPersistedBeforeRun`

(any test that drives `Register → recordRegisterIntentForTask` is at risk).

## Expected vs actual

**Expected:** api package tests run hermetically against test-temp directories without contending with the user's installed mcphub on the real LocalAppData path.

**Actual:** `newRegisterHarness` (internal/api/register_test.go:32) does NOT install a `daemonStateRootOverride`. `WriteDaemonIntent` therefore resolves `DaemonStateDir()` to the production `%LOCALAPPDATA%\mcp-local-hub\` path. The user's running mcphub holds `daemon-intent.json.lock` periodically (watchdog cycle / intent writes), and the test's `flock.Lock()` blocks until the test timeout fires.

Additionally, when the test eventually proceeds, it MUTATES the user's real intent file — the file currently shows ~921 KB of stale test data accumulated from prior runs.

## Risk

Local dev only. CI runners are clean of the user's installed binary, so this manifests as a developer footgun rather than a CI failure. Already adjacent to `2026-05-08-test-portinuse-flake-install-audit-failclosed.md` which documents a similar test-vs-real-binary leak via port 9128.

The two findings together reveal the same root cause: api tests bypass the `daemonStateRootOverride` seam (and similar I/O isolation seams) when they call into `Register` / `Install` / `MigrateLegacy`. The seam exists (used by `daemon_intent_test.go::daemonIntentTestHelper`), it is just not threaded through `newRegisterHarness`.

## Files involved

- internal/api/register_test.go:32-92 — `newRegisterHarness` does not seed `daemonStateRootOverride`.
- internal/api/legacy_migrate_test.go — every `TestMigrateLegacy_*` calls `MigrateLegacy → Register → WriteDaemonIntent`.
- internal/api/state_paths.go:86 — `daemonStateRootOverride` test seam (un-used by Register-side tests).
- internal/api/testhooks.go:124-127 — existing helper that sets/restores the override (used by intent / watchdog state tests, not by Register tests).

## Suggested fix

Extend `newRegisterHarness` to set a per-test `daemonStateRootOverride` (pointed at `t.TempDir()`) and restore it in `restore()`. Any test that exercises `Register / Install / MigrateLegacy` paths should inherit hermetic state-dir behavior automatically.

## Severity rationale

Low: dev-environment-only flake; no CI impact; no production-runtime impact. Tests that pass in CI keep passing — the bug only surfaces when a developer runs `go test ./...` on a host where mcphub is installed and running. Fix is mechanical (add one line to `newRegisterHarness`) but out of scope for the current PR (codex deep-sec round-1 tray-state finding).

## Discovery context

Found while running `go test -count=1 -timeout 8m ./...` to verify the round-1 deep-sec fix on `internal/tray/state.go`. The tray fix itself is hermetic; the api package hang reproduces on master without the fix and is unrelated to the tray-state change.
