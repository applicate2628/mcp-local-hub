# internal/api tests read the real home/hub → fail on dev hosts (CI-green)

**Status:** closed (2026-07-05) — fixed by PR #501 (`dc74d77f` hermetic HOME
isolation: `hermeticHome()` in hub_gate_detect_test.go + readiness_test.go
stubbing portAvailable/preflightPortInUse), sealed by #502/#503. Verified on this
live dev host (gated-on clients present) 2026-07-05: the three named tests PASS.
The `## REMAINING` two flaky tests below are a separate class — not reopened here.
**Found:** 2026-07-04 (during PR #500 r7 full-package gate)
**Severity:** P2 — test hygiene; blocks a clean local `go test ./internal/api/`,
does NOT affect production code or CI (these pass in an isolated CI home).

## Symptom

`go test -count=1 -timeout 5m ./internal/api/` on the developer host (with a real
installed hub + gated-on clients) fails/hangs on three tests that leak the real
environment instead of isolating HOME:

- `TestGatedOnClientsIgnoresNonHubEntries` (hub_gate_detect_test.go:69) — fails
  `AnyClientGatedOn() = true, want false on a gate-OFF host`.
- `TestGatedOnClientsEmptyOnFreshHome` (hub_gate_detect_test.go:80,83) — fails
  `GatedOnClients() = [vscode] on a fresh home, want empty` — it reads the real
  `%APPDATA%` vscode config, which on this host carries the `mcphub-hub` aggregate
  entry (the host is genuinely gate-ON for vscode).
- `TestAllServerReadiness_CoversEmbeddedServers` (readiness_test.go:101) — HANGS
  to the 5m package timeout: `AllServerReadiness()` → `CheckServerReadinessByName`
  probes the real embedded servers; one probe blocks with no per-test deadline
  that fires inside 5m.

## Root cause (hypothesis, unverified)

These tests resolve the client-config / state paths through the real user home
(no `t.Setenv("LOCALAPPDATA"/"APPDATA"/"HOME", tempdir)` isolation) and, for the
readiness one, dial real server ports. On a clean CI runner the home is empty and
no hub is installed, so they pass; on a live dev host they observe real state.

Not verified which exact resolver each uses; see memory
`feedback_state_override_misses_registry_path` (MCPHUB_STATE_DIR_OVERRIDE misses
the registry path — serena tests need `t.Setenv("LOCALAPPDATA", temp)`).

## Fix direction

Isolate HOME/`%APPDATA%`/`%LOCALAPPDATA%` per-test (temp dir) so gate-detect reads
an empty client-config set; give `AllServerReadiness` a bounded per-server probe
deadline in test, or gate the embedded-server probe behind a test seam (mirror the
E2E `MCPHUB_E2E_SCHEDULER=none` pattern).

## Not caused by PR #500

PR #500 touches only `hub_mcp_aggregator.go`, `hub_mcp_session.go`, and their two
test files; it never touches `hub_gate_detect*` or `readiness*`. No goroutine in
the timeout dump is inside the changed functions. Confirmed via
`git diff master...HEAD --name-only`.

## FIX (2026-07-04, branch fix/api-test-hermetic-home-isolation)

Commission (opus + sonnet + fable, unanimous PROCEED) approved the approach:
- `hermeticHome` (`hub_gate_detect_test.go`) extended to pin the full leak set —
  `APPDATA` (the confirmed vscode leak), `XDG_STATE_HOME`, and the client-specific
  override vars (`COPILOT_HOME`, `KIMI_CODE_HOME`, `MIMOCODE_*`,
  `MIMOCODE_TEST_MANAGED_CONFIG_DIR`). Root cause: `defaultVSCodeConfigPath`
  (`clients.go:877`) reads `%APPDATA%` first; `hermeticHome` never pinned it.
- `TestAllServerReadiness_CoversEmbeddedServers` now calls `hermeticHome(t)` +
  stubs `portAvailable = true` / `preflightPortInUse = false` so the `&&` in
  `AdmissionCheck` + `fixedPortStatus` short-circuits BEFORE the
  netstat/wmic/schtasks ownership storm (the 5-min hang).

All three target tests now pass in < 0.6s (were 2 FAIL + 1 five-minute hang).

## REMAINING — additional environmental tests (separate follow-up)

Fixing the three named tests let a full `go test ./internal/api/` run reach TWO
more host-coupled tests that were previously masked by the readiness hang:
- `TestToolCatalog_GoldenAgainstUpstream` (`tool_catalog_test.go:97`) — SPAWNS the
  real `mcp-language-server -lsp gopls` + `gopls mcp` binaries (only `t.Skip`s when
  they are absent from PATH) to golden-compare their tool catalogs. Hangs when the
  live LSP backend is cold/indexing (observed right after a redeploy restarted the
  fleet). Needs a process-spawn deadline or an offline/skip gate — a DIFFERENT class
  than the home-leak; out of this fix's scope.
- `TestRestartAllFallsThroughToLegacySchedulerAndSkipsSupervisorHandledTasks` —
  touches the real Task Scheduler; failed 18.7s on a live host. Needs a scheduler
  seam.

Both passed on an earlier calm-host run (bvm0oi9tb, 205s) and are NOT regressed by
this fix (different files; do not call `hermeticHome`); they are time/state-flaky
integration tests. Tracked here as the remaining work toward a fully-clean local
`go test ./internal/api/`.
