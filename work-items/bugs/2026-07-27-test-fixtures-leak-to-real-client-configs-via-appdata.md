# Bug: test fixtures write the developer's REAL client configs (%APPDATA% and friends not neutralized)

- id: 2026-07-27-test-fixtures-leak-to-real-client-configs-via-appdata
- context: adjacent-finding
- status: open (internal/cli FIXED — mcp-front fixtures AND language_server_test.go's withHermeticHome, which was found still leaking by the PR #588 re-gate; internal/api, internal/gui, internal/clients NOT audited)
- severity: high
- area: any test fixture that redirects only HOME / USERPROFILE / LOCALAPPDATA and then drives `clients.AllClients()`
- found-by: backend-engineer (PR #588 branch repair lane, 2026-07-27)

## Summary

A test that drives a real client-config reconcile enumerates
`clients.AllClients()`, and each adapter resolves its config path from the
environment. Redirecting HOME / USERPROFILE / LOCALAPPDATA covers only SOME of
them. A fixture that misses one does not produce a flake — it produces a WRITE
to the developer's live config.

Proven live on this host, not hypothetical. `TestMCPFront*` and
`TestRunReconcileMCPFront_*` admitted the real vscode adapter at
`%APPDATA%\Code\User\mcp.json` and the forward reconcile rewrote all ten of the
operator's MCP server URLs to the test's ephemeral port (observed values 24976,
27531, 30426, 51654 across runs), leaving a `.bak-mcp-local-hub-<ts>` set behind
each run. The operator's VS Code MCP integration was pointing at dead ports.
Restored by hand to the live hub port (9125, cross-checked against the
untouched `~/.claude.json`), and the stale backups were moved out of the
directory because `LatestBackupPath` would otherwise select one.

## The full env set an adapter can resolve a path from

Derived from `os.Getenv` over `internal/clients` plus the APPDATA call sites:

| Variable | Adapters |
|---|---|
| `%APPDATA%` | vscode, cline, roo, kilocode, devin, amp, zed, qoder, goose |
| `$XDG_CONFIG_HOME` | opencode, devin, roo, mimocode (global dir) |
| `$MIMOCODE_HOME` | mimocode global dir (wins over XDG_CONFIG_HOME) |
| `$MIMOCODE_CONFIG` / `_CONFIG_DIR` / `_CONFIG_CONTENT` | mimocode extra read layers |
| `$MIMOCODE_TEST_MANAGED_CONFIG_DIR` / `%ProgramData%` | mimocode managed layer |
| `$COPILOT_HOME`, `$KIMI_CODE_HOME` | copilot-cli, kimi-code |

## Fixed so far

**CORRECTION (2026-07-27, PR #588 re-gate).** An earlier revision of this file said
`internal/cli` was closed. It was not, and the false line was the dangerous part: it
would have steered the follow-up sweep away from the one package still carrying a
**write-capable** leak, while the unaudited packages may only have read leaks.

`internal/cli/language_server_test.go`'s `withHermeticHome` redirected only
`USERPROFILE`/`HOME`, `LOCALAPPDATA` and `XDG_STATE_HOME` — not `%APPDATA%`,
`$XDG_CONFIG_HOME`, `%ProgramData%`, `$COPILOT_HOME`, `$KIMI_CODE_HOME` or the
`$MIMOCODE_*` set. `mcphub language-server cleanup` takes an UNFILTERED
`clients.AllClients()` (`internal/cli/language_server.go:141`) and for each stdio
`mcp-language-server` entry calls `adapter.BackupKeep` (`:292` — which also PRUNES the
operator's existing backups) then `adapter.RemoveEntry` (`:302`). Three of the four
`TestLanguageServerCleanup_*` tests run with no client filter, so a plain
`go test ./internal/cli/` could delete a real entry and rotate away the backup that
would have restored it.

It survived the original fix's own verification because that sweep was
`-run 'Router|Snapshot|Reconcile|Install|Setup|TestMCPFront|TestRouteSession'`, which
never selects `TestLanguageServerCleanup_*`.

**Measured on the dev host: no damage occurred, and the reason is luck, not isolation.**
Every real `mcp-language-server` entry there is url/http-shaped — `mcphub` itself had
already migrated them to hub URLs — and the cleanup predicate matches only stdio
entries carrying `--lsp`. A host that has not yet migrated holds exactly the shape that
would have been deleted. Verified with a SHA-256 baseline over the five live client
configs before and after running the four cleanup tests: four byte-identical, the fifth
(`~/.claude.json`) accounted for line by line as session telemetry plus one intended
edit, and the hub-managed backup count unchanged at 4.

Fixed by calling the single owner `neutralizeClientConfigPathEnv` from
`withHermeticHome`.

`internal/cli` only. `neutralizeClientConfigPathEnv`
(`internal/cli/client_config_env_isolation_test.go`) is the one owner of the
list; `mcpFrontPR588Env`, `redirectMCPFrontTestEnv` and `resetPortHermeticHome`
all call it. Verified: a full
`-run 'Router|Snapshot|Reconcile|Install|Setup|TestMCPFront|TestRouteSession'
./internal/cli/` sweep against the REAL `%APPDATA%` now leaves
`%APPDATA%\Code\User\mcp.json` byte-identical (same sha256) with no new backups.

## Not fixed — the sweep this bug is filed for

`internal/api`, `internal/gui` and `internal/clients` fixtures were NOT audited.
Several already set `APPDATA` by hand (e.g. `internal/api/deadopt_test.go:43`,
`internal/gui/adopt_test.go:31`), which is evidence the hazard is known but
handled ad hoc, per-fixture, with no single owner — exactly the shape that lets
the next fixture miss one. Bisecting those packages on this host found no leak
today, but that is a point-in-time result, not a guarantee.

Suggested shape: promote `neutralizeClientConfigPathEnv` to a shared test-only
helper (an `internal/clients/clientstest` or existing `apitest` package) so
every package's fixtures resolve one owner, and add a fixture-level assertion
in the spirit of `assertRedirectedStateDir` — fail the test when any admitted
adapter's `ConfigPath()` does not live under the sandbox root. The assertion is
the durable fix: it catches a missed env var by construction instead of relying
on the next author remembering the list.

## Why the assertion matters more than the list

Two `internal/cli` tests were passing ONLY because of this leak.
`TestRunReconcileMCPFront_ForwardThenRollback_RoundTrip` and
`TestRunReconcileMCPFront_Rollback_UsesPersistedPortNotLiveSetting` had no
sandbox client of their own; the developer's real vscode config was the only
client they reconciled. Once isolated they failed with "carries no version-3 row
map" — meaning they were already red on any host without VS Code MCP configured
(any CI runner), and green locally for the wrong reason. Both now seed a sandbox
claude-code config. A "no adapter outside the sandbox" assertion would have
surfaced both the leak and the hollow test the day they were written.
