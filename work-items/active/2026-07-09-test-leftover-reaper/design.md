# Test-Leftover Reaper Lane Design Memo

Repo evidence was gathered from `master` at `f7eaa1c85c827ba1204b59103fdf21e9ee0b61e4` using git, grep, Serena symbol overview, and Codegraph exploration against the indexed HEAD. Local worktree status was not used as evidence for repo behavior; it showed pre-existing unrelated changes in `cmd/mcphub/resource.syso` and `.plans/2026-07/plan(main)-2026-07-08_10-10_gui-adopt-scoped-symlink-consent.md`.

## Current Taxonomy (Cited)

### Default `CleanupOrphans`

The default orphan path is intentionally config-and-identity conservative, and it currently cannot reap test-leftover `mcphub` binaries because `parseOrphans` drops all hub-owned process names before nomination or ancestry checks. `isOurOwnProcess` treats `mcphub.exe`, `mcphub`, `mcp.exe`, `mcp`, `mcp-local-hub.exe`, and `mcp-local-hub` as own binaries, because Task Scheduler ancestry often makes the daemon look parentless; this own-binary guard is defined in `internal/api/cleanup.go:32-60` and applied at candidate-row time in `internal/api/cleanup.go:1287-1298`.

The default path first builds nominatable patterns from server manifests and optional client config scans. `patternsForServerNominatable` refuses bare fallback server-name patterns, so a server without a concrete manifest pattern cannot become an unattended kill source (`internal/api/scan.go:935-960`). Manifest extraction rejects generic launchers and too-short or flag-like arguments (`internal/api/scan.go:973-1007`), while manifest and cleanup filtering omit broad launchers such as `node`, `npx`, `uv`, `uvx`, `python`, and `py` (`internal/api/cleanup.go:1176-1200`, `internal/api/cleanup.go:1202-1249`). Client-stdio nomination skips `mcphub` relay commands entirely, so a relay command line cannot nominate hub binaries or relay arguments as kill patterns (`internal/api/cleanup.go:647-672`).

The client-config scan is fail-closed. A client parse or construction degradation records the degraded client and later spares every candidate rather than killing under uncertain config coverage (`internal/api/cleanup.go:733-790`, `internal/api/cleanup.go:1117-1141`). A candidate whose command line contains any config-derived reference pattern is marked `config-referenced` and spared (`internal/api/cleanup.go:793-816`, `internal/api/cleanup.go:1073-1114`). Tests pin that behavior: config-referenced candidates never reach the kill function, config-absent old candidates are killed, degraded config scanning spares all candidates, and dry-run mode never kills (`internal/api/cleanup_config_absence_test.go:39-57`, `internal/api/cleanup_config_absence_test.go:59-80`, `internal/api/cleanup_config_absence_test.go:82-105`, `internal/api/cleanup_config_absence_test.go:108-145`).

The ancestor walk is three-state and fail-closed. `parseOrphans` only treats a missing parent row as orphan evidence when `orphanParentProvenDead` proves the parent PID is dead; alive, unknown, or probe-error states spare the candidate (`internal/api/cleanup.go:1353-1370`, `internal/api/cleanup.go:1687-1707`). A parent command line containing `mcphub.exe daemon` / `mcp.exe daemon` or a known client launcher spares the descendant (`internal/api/cleanup.go:1324-1349`, `internal/api/cleanup.go:1372-1381`). Self-loops and chains deeper than the ancestry cap are also spared (`internal/api/cleanup.go:1382-1403`). Tests cover dead-parent-only reaping, self-loop sparing, and over-depth sparing (`internal/api/cleanup_walk_failclosed_test.go:34-64`, `internal/api/cleanup_walk_failclosed_test.go:67-87`, `internal/api/cleanup_walk_failclosed_test.go:89-103`).

Only `ReapVerdictReapEligible` reaches termination. Snapshot degradation, config references, degraded client configs, and age below the kill floor all spare candidates (`internal/api/cleanup.go:1073-1114`, `internal/api/cleanup.go:1117-1141`). The kill floor for automatic cleanup is ten minutes (`cleanupKillMinAgeSec = 600`) in `internal/api/cleanup.go:141-145`. The CLI keeps cleanup dry-run by default and requires `--confirm` to mutate (`internal/cli/cleanup.go:25-38`, `internal/cli/cleanup.go:69-83`, `internal/cli/cleanup.go:133-139`).

### Aggressive Cleanup

The aggressive path is opt-in and scoped, but it still refuses to target hub-owned binaries. Its documented invariant is to kill only live-rooted MCP stdio descendants that the default sweep cannot reap; it never bypasses the hub-daemon guard and excludes dangerous process classes by default (`internal/api/cleanup.go:396-406`, `internal/api/cleanup.go:1443-1462`). It skips own binaries, skips known client launchers, skips dangerous classes such as command shells and Chrome unless explicitly included, and requires either a root PID or known-client scope match in the ancestor walk (`internal/api/cleanup.go:1423-1462`, `internal/api/cleanup.go:1473-1539`). It then performs the same age and identity-binding model before any kill (`internal/api/cleanup.go:1571-1638`).

The aggressive CLI uses a two-step confirmation token: preview resolves candidates and prints a token; apply recomputes candidates, verifies the token, binds expected identities, and only then kills (`internal/cli/cleanup_aggressive.go:61-67`, `internal/cli/cleanup_aggressive.go:83-97`, `internal/cli/cleanup_aggressive.go:116-145`). The GUI aggressive endpoint follows the same safe polarity: `Apply=false` is preview, `Apply=true` requires `Expect` and a matching confirm token (`internal/gui/cleanup_aggressive.go:152-205`, `internal/gui/cleanup_aggressive.go:209-240`). Aggressive cleanup also emits a best-effort audit event named `aggressive-cleanup-executed` with scope, candidate, killed, skipped, class, and token data (`internal/cli/cleanup_aggressive.go:165`, `internal/cli/cleanup_aggressive.go:202-236`).

### Identity And Kill Proof

The cleanup kill contract already binds a process to `{PID, ExecutablePath, StartedAt}`. `OrphanProcess` carries `ExecutablePath` and `StartedAt` as server-side proof fields, and comments state these fields are required to avoid PID reuse kills (`internal/api/cleanup.go:62-119`). `filterToExpectedIdentities` filters previewed candidates by exact `PID` and `StartedAt` before apply (`internal/api/cleanup.go:1641-1672`). `reapOneOrphan` refuses to kill without `ExecutablePath`, `StartedAt`, or raw command line, rereads the live command line, aborts on command-line mismatch, and then calls `process.TerminatePIDWithIdentity` with a PID identity proof (`internal/api/cleanup.go:1749-1772`, `internal/api/cleanup.go:1774-1794`).

`TerminatePIDWithIdentity` opens the target process, verifies identity, terminates, and waits up to five seconds (`internal/process/pid_identity_windows.go:51-86`). Identity verification checks executable path equivalence and creation time within tolerance (`internal/process/pid_identity_windows.go:88-107`, `internal/process/pid_identity_windows.go:145-162`). Empty executable path, empty start time, and unparsable start time fail closed (`internal/process/pid_identity_common.go:11-19`, `internal/process/pid_identity_common.go:41-51`). Tests cover identity capture, proof-based kill, command-line mismatch refusal, quote normalization, and missing-proof skip (`internal/api/cleanup_reap_identity_test.go:82-118`, `internal/api/cleanup_reap_identity_test.go:120-151`, `internal/api/cleanup_reap_identity_test.go:218-270`, `internal/api/cleanup_reap_identity_test.go:338-357`). Aggressive token tests also prove token drift on `StartedAt` changes (`internal/api/cleanup_aggressive_test.go:236-251`).

### Existing Process Census Limit

The current process snapshot records only command line, creation date, executable path, parent PID, PID, and memory (`internal/api/processes.go:184-235`). It does not capture environment variables. Any predicate that depends on a live target's `MCPHUB_STATE_DIR_OVERRIDE` or e2e-only environment markers therefore requires a new process-environment evidence reader, and must fail closed when that reader is unavailable or uncertain.

## Test-Leftover Signature (Discriminator Table)

At HEAD, committed tests create two verified classes of `cmd/mcphub` test binaries: reliability-test temp binaries and GUI end-to-end binaries. A grep probe for literal `f1-cli-verify` returned no matches at HEAD, so that prefix is prior runtime evidence only, not a committed discriminator. It must be treated as `ASSUMPTION (UNVERIFIED)` until a fixture, script, or code path in the tree proves it.

| Discriminator | Evidence | FP risk | FN risk | Verdict |
|---|---|---|---|---|
| Basename is `mcphub.exe` / `mcphub` | Current cleanup treats these names as own binaries: `internal/api/cleanup.go:32-60`. | High alone. | Low for this target class. | Necessary but never sufficient. |
|  | Real installed hub, repo-run hub, e2e hub, and reliability hub share the basename. |  |  |  |
| Reliability temp image path | Temp output prefix `mcphub-reliability-*`: `internal/cli/daemon_reliability_test.go:49-63`. | Low with env + exact argv. | Medium. | Strong for reliability leftovers. |
|  | Built from `./cmd/mcphub` with `-tags test_state_path_env`: `internal/cli/daemon_reliability_test.go:67-82`. | Medium alone. | Misses e2e and uncommitted prefixes. |  |
| GUI e2e image path | Global setup writes `internal/gui/e2e/bin/mcphub(.exe)`: `internal/gui/e2e/global-setup.ts:13-40`. | Low with env + exact argv. | Medium. | Strong for GUI e2e leftovers. |
|  | The e2e binary is built with `-tags=test_state_path_env`: `internal/gui/e2e/global-setup.ts:74-99`. | Medium alone. | Misses reliability and other temp harnesses. |  |
| Build tag `test_state_path_env` | Reliability and e2e build commands pass the tag: `internal/cli/daemon_reliability_test.go:67-82`; `internal/gui/e2e/global-setup.ts:90-99`. | Very low if proven from live target. | Unknown. | `ASSUMPTION (UNVERIFIED)` as runtime proof. |
|  | The tag installs env fallback code that production builds exclude: `cmd/mcphub/ipc_test_isolation_envfallback.go:1-8`; `internal/api/state_paths_envfallback.go:1-10`; `internal/api/state_paths_prod.go:1-10`. |  | Current census cannot inspect build tags. | Do not rely on it directly. |
| `MCPHUB_STATE_DIR_OVERRIDE` env | Subprocess tests set the override and temp home env: `internal/cli/subprocess_state_isolation_test.go:84-105`. | Very low when live env is verified. | Medium. | Best positive discriminator. |
|  | GUI e2e sets the same override: `internal/gui/e2e/fixtures/hub.ts:67-108`; `internal/gui/e2e/fixtures/seeded-hub.ts:79-104`. |  | Misses tagged binaries without override. | Requires new env reader. |
|  | Tagged binaries consume the override: `internal/api/state_paths_envfallback.go:36-75`; `cmd/mcphub/ipc_test_isolation_envfallback.go:58-65`. |  |  | Current census cannot read env: `internal/api/processes.go:184-235`. |
| E2E-only env markers | `MCPHUB_E2E_SCHEDULER=none`, `MCPHUB_E2E_SUPERVISOR=none`, and PID-port dir are set in e2e fixtures. | Very low for e2e. | High outside e2e. | Use only for GUI e2e branch. |
|  | Evidence: `internal/gui/e2e/fixtures/hub.ts:96-108`; `internal/gui/e2e/fixtures/seeded-hub.ts:92-100`. |  |  |  |
| Exact GUI e2e argv | Fixture starts `gui --no-browser --no-tray --port 0`: `internal/gui/e2e/fixtures/hub.ts:109-112`. | Medium alone. | Medium. | Strong only with path + env. |
|  | Seeded fixture uses same shape: `internal/gui/e2e/fixtures/seeded-hub.ts:101-104`. | Low with image path + env proof. | Misses supervise and reliability shapes. |  |
| Exact reliability argv | Reliability test starts `daemon --server definitely-no-such-server --daemon x`: `internal/cli/daemon_reliability_test.go:154-176`. | Low with image path + env proof. | High for other shapes. | Safe for committed reliability fixture. |
|  |  | Medium alone. |  |  |
| `supervise` argv | Test isolation docs state GUI can spawn `mcphub supervise` and env is inherited: `internal/cli/subprocess_state_isolation_test.go:55-70`. | High alone. | Medium. | Include only with image path + env proof. |
|  |  | Production and developer hubs also supervise. | Supervise is likely leftover class. | Otherwise spare. |
| Parent chain non-install / dead / test parent | Current cleanup can inspect parent PID and command line: `internal/api/cleanup.go:1324-1403`. | Medium. | High. | Supporting evidence only. |
|  | Interrupted tests can leave children reparented, and dead-parent proof is generic orphan evidence. | Ancestry alone is not test proof. | Reparented leftovers lose parent evidence. | Do not require except diagnostics. |
| Real installed image path | Canonical install path is `<home>/.local/bin/mcphub(.exe)`: `internal/api/install.go:39-64`; `internal/cli/setup.go:99-115`. | N/A. | N/A. | Mandatory refusal. |
|  | Scheduler tasks reference canonical path: `internal/cli/setup.go:336-342`. |  |  | Never kill install path or install-dir asides. |
| Real production state dir | Production Windows state root is `<LocalAppData>\mcp-local-hub`: `internal/api/state_paths_prod.go:28-49`. | N/A. | N/A. | Mandatory refusal. |
|  | Supporting state-dir constants: `internal/api/state_paths_windows.go:69-73`; `internal/api/state_paths.go:43-47`. |  |  | Refuse if argv/env points there. |
| Repo-run hub path | Current cleanup spares own hub binaries as valid local tools: `internal/api/cleanup.go:32-60`; `internal/api/cleanup.go:1287-1298`. | N/A. | N/A. | Mandatory refusal. |
|  |  |  |  | Only e2e bin path with all proofs may pass. |
| `f1-cli-verify` temp prefix | `rg -n "f1-cli-verify" -S` found no committed match at HEAD. | Unknown. | Unknown. | `ASSUMPTION (UNVERIFIED)`. |
|  |  |  |  | Support only via explicit `--temp-root` or after a committed fixture proves it. |

The safe distinction is therefore not "a `mcphub` process under temp." The safe distinction is: a hub-named process whose image path matches a committed test build location, whose command-line shape matches a committed test subprocess shape, whose live environment proves a test-only state override outside production state, whose identity is bound to the previewed PID/start time/executable path, and whose negative guards rule out installed and developer-run hubs.

## Proposed Predicate + Hook

### Change-Surface Contract

- Intended change surface: a new explicit test-leftover cleanup lane under the cleanup command family, plus a small process-environment evidence reader if the implementation can provide one safely.
- Approved extension seams: `internal/api` cleanup-style candidate resolution and identity-bound termination, `internal/process` OS-specific process evidence helpers, and `internal/cli` cleanup subcommands. Existing examples are `CleanupOrphans` / `AggressiveCleanup` (`internal/api/cleanup.go:958-1638`), `TerminatePIDWithIdentity` (`internal/process/pid_identity_windows.go:51-86`), and cleanup/aggressive CLI flows (`internal/cli/cleanup.go:13-139`, `internal/cli/cleanup_aggressive.go:61-145`).
- Protected surfaces: do not change existing default `CleanupOrphans`, aggressive cleanup candidate semantics, daemon recover, scheduler/ticker behavior, or the current own-binary skip. The current own-binary guard is a safety boundary (`internal/api/cleanup.go:32-60`, `internal/api/cleanup.go:1287-1298`, `internal/api/cleanup.go:1473-1475`).
- Declared blast radius: additive CLI/API lane and tests; no unattended reaping behavior change.

### Hook Recommendation

Add an operator-invoked command, not a ticker path:

```text
mcphub cleanup test-leftovers [--min-age-sec 600] [--temp-root <path>]          # preview only
mcphub cleanup test-leftovers --confirm-token <token> [--min-age-sec 600]      # apply
```

This belongs beside `cleanup` and `cleanup aggressive`, not inside `CleanupOrphans`, because the existing default and aggressive paths both intentionally skip own `mcphub` binaries (`internal/api/cleanup.go:1287-1298`, `internal/api/cleanup.go:1473-1475`). It should use the same two-step token model as aggressive cleanup, because the existing aggressive CLI and GUI already establish preview, token, recompute, identity-bind, then kill as the destructive pattern (`internal/cli/cleanup_aggressive.go:83-145`, `internal/gui/cleanup_aggressive.go:194-240`).

The initial implementation should be CLI-only. A GUI surface can be added later after the predicate has test coverage and operational soak, because GUI cleanup handlers expose destructive cleanup over local HTTP and therefore require the same `Apply=false` / `Apply=true` polarity and `Expect` binding used today (`internal/gui/cleanup.go:24-53`, `internal/gui/cleanup.go:108-149`).

### Fail-Closed Predicate

Candidate enumeration should scan process rows directly and evaluate only hub basenames; it should not reuse `parseOrphans`, because `parseOrphans` deliberately skips hub basenames before candidate construction (`internal/api/cleanup.go:1287-1298`). The predicate should return a candidate only when every positive proof for one branch passes and every negative guard passes.

Positive common gates:

1. Snapshot is not truncated/degraded, and the row has `PID`, `ExecutablePath`, `CommandLine`, and `CreationDate` / `StartedAt`. Snapshot fields are available from the current census (`internal/api/processes.go:53-62`, `internal/api/processes.go:184-235`), and missing kill proof must fail closed as in `reapOneOrphan` (`internal/api/cleanup.go:1749-1772`).
2. Image basename is exactly `mcphub.exe` or `mcphub`, and the command-line first token resolves to the same hub binary family. Existing helpers already define hub binary basenames and own-binary names (`internal/api/cleanup.go:49-60`, `internal/api/cleanup.go:856-867`).
3. Age is at least 600 seconds by default. This matches the existing automatic kill floor (`internal/api/cleanup.go:141-145`) and avoids killing active tests.
4. Live environment evidence is read successfully from the target process, and it contains `MCPHUB_STATE_DIR_OVERRIDE` pointing outside the real production state dir. This is a new capability requirement because current census does not read env (`internal/api/processes.go:184-235`). If env read is unsupported, denied, truncated, or ambiguous, the candidate is spared.
5. The live env state override must normalize under an expected temp/e2e test root and must not normalize under production `<LocalAppData>\mcp-local-hub`, whose production owner is `daemonStateDir` on Windows (`internal/api/state_paths_prod.go:28-49`, `internal/api/state_paths_windows.go:69-73`, `internal/api/state_paths.go:43-47`).

Positive branch gates:

1. Reliability branch: normalized image path is under the OS temp directory and the basename matches `mcphub-reliability-*` with the platform executable suffix, as produced by `ensureMcphubBinary` (`internal/cli/daemon_reliability_test.go:49-63`). The command line must match the committed reliability daemon shape `daemon --server definitely-no-such-server --daemon x` (`internal/cli/daemon_reliability_test.go:154-176`) or be a `supervise` process with the same temp image path and verified test env. The `supervise` allowance exists only because test-isolated GUI subprocesses can spawn `mcphub supervise` grandchildren that inherit the test env (`internal/cli/subprocess_state_isolation_test.go:55-70`); without env proof it is refused.
2. GUI e2e branch: normalized image path equals the repo fixture output `internal/gui/e2e/bin/mcphub(.exe)` produced by global setup (`internal/gui/e2e/global-setup.ts:20-40`, `internal/gui/e2e/global-setup.ts:90-99`). The command line must match `gui --no-browser --no-tray --port 0` (`internal/gui/e2e/fixtures/hub.ts:109-112`, `internal/gui/e2e/fixtures/seeded-hub.ts:101-104`) or be `supervise` with verified inherited test env. For GUI e2e `gui` candidates, also require e2e-only env markers such as `MCPHUB_E2E_SCHEDULER=none`, `MCPHUB_E2E_SUPERVISOR=none`, or `MCPHUB_GUI_TEST_PIDPORT_DIR` when present in the fixture path (`internal/gui/e2e/fixtures/hub.ts:96-108`, `internal/gui/e2e/fixtures/seeded-hub.ts:92-100`).
3. Operator-provided temp-root branch: only after preview explicitly receives `--temp-root <path>`, allow a candidate under that root when it still passes basename, exact argv, env override, production-state refusal, min-age, and identity gates. This is the only safe way to cover the uncommitted `f1-cli-verify` shape. The default predicate must not include `f1-cli-verify`, because no file at HEAD verifies the prefix.

Negative guards, evaluated before any positive branch can admit a candidate:

1. Refuse if the image path equals the canonical installed hub path `<home>/.local/bin/mcphub(.exe)` or an install-dir replacement/aside path. The canonical target is defined by API and CLI setup code (`internal/api/install.go:39-64`, `internal/cli/setup.go:99-115`, `internal/cli/setup.go:278-286`), and Windows replacement/asides are install-dir artifacts (`internal/api/binary_rename_aside_windows.go:41-55`, `internal/api/binary_rename_aside.go:42-63`).
2. Refuse if any argv path or verified env state path normalizes under production `<LocalAppData>\mcp-local-hub` (`internal/api/state_paths_prod.go:28-49`, `internal/api/state_paths_windows.go:69-73`, `internal/api/state_paths.go:43-47`).
3. Refuse if the image path is inside the developer repo but not exactly the GUI e2e bin fixture. The current own-binary skip protects developer-run hubs broadly (`internal/api/cleanup.go:32-60`), and the new lane should not weaken that rule.
4. Refuse on env-read failure, command-line reread mismatch, missing executable path, missing started-at time, token mismatch, snapshot truncation, parent/probe uncertainty if parent evidence is used, or any path normalization error. Existing cleanup already fails closed on command-line mismatch, missing proof, snapshot truncation, and PID identity mismatch (`internal/api/cleanup.go:1749-1794`, `internal/api/cleanup_aggressive_test.go:217-251`).

### Confirm Token And Identity Binding

The preview result should carry server-side proof fields analogous to `OrphanProcess`: `PID`, `ExecutablePath`, `StartedAt`, raw command line, candidate kind, predicate version, and verified state-override path. JSON/display output should redact or hash sensitive path details, following the current cleanup design where raw command line and executable proof are hidden from JSON while a redacted display value is exposed (`internal/api/cleanup.go:62-119`).

The confirm token should be a new `TestLeftoverConfirmToken`, not a reuse of `AggressiveConfirmToken`, because the test-leftover predicate depends critically on full normalized image path and env state path, while aggressive token currently hashes PID, executable basename, match source, and started-at (`internal/api/cleanup.go:467-490`). Token input should include sorted records of `{PID, StartedAt, normalized executable path hash, normalized env state path hash, exact normalized argv hash, candidate kind, predicate version}`. Including full paths or path hashes in the token material prevents a temp candidate from being swapped with an installed hub that shares basename and PID but differs path or env proof.

Apply should recompute candidates from a fresh process snapshot, recompute the token, filter by expected `{PID, StartedAt}`, reread live command line and live env, and call `process.TerminatePIDWithIdentity` only after all predicate gates still pass. This keeps the existing anti-PID-reuse model intact (`internal/api/cleanup.go:1641-1672`, `internal/api/cleanup.go:1749-1772`, `internal/process/pid_identity_windows.go:51-107`).

### Audit Events

Emit a best-effort supervisor-log event only on apply, following the aggressive cleanup precedent (`internal/cli/cleanup_aggressive.go:202-236`). Suggested event name: `test-leftover-cleanup-executed`. Include `predicateVersion`, `scope` (`default` or operator temp-root), `candidateCount`, `killedCount`, `skippedCount`, `refusalReasonCounts`, `confirmTokenPrefix`, and per-candidate redacted fields: PID, basename, started-at, candidate kind, executable path hash, state path hash, and result. Do not log full temp paths by default, because existing cleanup avoids exposing raw command lines and executable paths in JSON display fields (`internal/api/cleanup.go:62-119`).

A token mismatch or production-path refusal should return a nonzero CLI error and can emit `test-leftover-cleanup-refused` with aggregate counts only. This preserves failure transparency without turning routine preview scans into noisy audit writes.

### Architectural Claims

| Claim | Owner | Falsifying probe |
|---|---|---|
| Existing `CleanupOrphans` and `AggressiveCleanup` behavior remains unchanged. | New `CleanupTestLeftovers` API lane and CLI subcommand. | Tests showing current own-binary skip still applies in default and aggressive paths (`internal/api/cleanup.go:1287-1298`, `internal/api/cleanup.go:1473-1475`) plus no call from existing cleanup handlers. |
| No real installed hub is killed. | Test-leftover predicate negative guards. | Fixture with image path `<home>/.local/bin/mcphub.exe` and production state dir is spared. Canonical path evidence: `internal/api/install.go:39-64`, `internal/cli/setup.go:99-115`. |
| No developer-run repo hub is killed. | Test-leftover predicate negative guards. | Fixture with repo-root `mcphub.exe gui` or `mcphub supervise`, no test env, is spared. Current safety intent: `internal/api/cleanup.go:32-60`. |
| PID reuse is refused. | Existing identity proof plus new token material. | Preview candidate, then apply with same PID and changed `StartedAt`; filter/token mismatch prevents terminate. Existing token drift and identity tests show the pattern (`internal/api/cleanup_aggressive_test.go:236-251`, `internal/api/cleanup_reap_identity_test.go:120-151`). |
| Test env is required and fail-closed. | New process env reader. | Env read failure, missing `MCPHUB_STATE_DIR_OVERRIDE`, or override under production state produces spared/refused candidate. Current census lacks env and therefore cannot satisfy this proof (`internal/api/processes.go:184-235`). |

## Test Plan

1. `internal/api` candidate-unit tests for the reliability branch: a process row with image `%TEMP%\mcphub-reliability-123.exe`, argv `daemon --server definitely-no-such-server --daemon x`, age over 600 seconds, env `MCPHUB_STATE_DIR_OVERRIDE=<temp>\supervisor-state`, and matching identity is admitted and reaped. Evidence for fixture shape: `internal/cli/daemon_reliability_test.go:49-82`, `internal/cli/daemon_reliability_test.go:154-176`.
2. `internal/api` candidate-unit tests for the GUI e2e branch: image `<repo>\internal\gui\e2e\bin\mcphub.exe`, argv `gui --no-browser --no-tray --port 0`, e2e env markers, and temp `MCPHUB_STATE_DIR_OVERRIDE` is admitted and reaped. Evidence for fixture shape: `internal/gui/e2e/global-setup.ts:20-40`, `internal/gui/e2e/global-setup.ts:90-99`, `internal/gui/e2e/fixtures/hub.ts:67-112`.
3. Real installed hub spared: image `<home>\.local\bin\mcphub.exe`, any hub argv, and production state root `<LocalAppData>\mcp-local-hub` is refused. Evidence for installed path and state root: `internal/api/install.go:39-64`, `internal/cli/setup.go:99-115`, `internal/api/state_paths_prod.go:28-49`.
4. Developer-run repo hub spared: repo-root `mcphub.exe gui`, `mcphub.exe daemon`, and `mcphub.exe supervise` without test env are refused. Evidence for current safety intent: `internal/api/cleanup.go:32-60`, `internal/api/cleanup.go:1287-1298`.
5. Temp basename alone spared: `%TEMP%\mcphub.exe` with no `MCPHUB_STATE_DIR_OVERRIDE` is refused. This guards copied helper binaries such as tests that copy the current test binary to `t.TempDir()/mcphub.exe` (`internal/cli/process_identity_match_windows_test.go:105-124`, `internal/cli/supervise_reconcile_wiring_test.go:2146-2165`).
6. Env-read failure spared: fake env reader returns access denied / unsupported / truncated; no terminate function is called. Current census cannot provide env proof (`internal/api/processes.go:184-235`).
7. Production state override refused: temp image and matching argv are still refused if `MCPHUB_STATE_DIR_OVERRIDE` normalizes under `<LocalAppData>\mcp-local-hub`. Production state owner: `internal/api/state_paths_windows.go:69-73`, `internal/api/state_paths.go:43-47`.
8. Command-line reread mismatch refused: census argv matches but live lookup returns different command line; terminate is not called. Existing pattern: `internal/api/cleanup_reap_identity_test.go:218-270`.
9. Recycled PID refused: preview token uses one `StartedAt`, apply sees same PID with changed `StartedAt`; token or expected-identity filter refuses. Existing pattern: `internal/api/cleanup_aggressive_test.go:236-251`, `internal/api/cleanup.go:1641-1672`.
10. Snapshot truncation refused: apply returns an error and does not kill, matching aggressive cleanup fail-closed behavior (`internal/api/cleanup_aggressive_test.go:217-233`, `internal/api/cleanup.go:1623-1631`).
11. CLI tests: preview prints candidates and confirm token; apply without token refuses; apply with stale token refuses; apply with fresh token passes expected identities into API; audit event `test-leftover-cleanup-executed` is written on apply. Existing CLI token flow reference: `internal/cli/cleanup_aggressive.go:83-145`, `internal/cli/cleanup_aggressive.go:202-236`.
12. Falsifying uncommitted prefix test: `f1-cli-verify` is not admitted by default. It becomes eligible only with explicit `--temp-root` and all other env, argv, identity, and negative guards satisfied. This is required because no committed file at HEAD verifies the prefix.

## Scope, Risk, And Recommendation

This is not a quick-fix. It is an additive full-delivery change with security-sensitive behavior because it creates a new destructive lane that can terminate `mcphub` binaries, a process family the current default and aggressive reapers intentionally spare (`internal/api/cleanup.go:32-60`, `internal/api/cleanup.go:1287-1298`, `internal/api/cleanup.go:1473-1475`). The implementation should be routed through design, implementation, QA, and security review before publication.

Blast radius is bounded if the lane is separate: `internal/api` for candidate resolution/token/termination, `internal/process` for live process-env evidence if implemented, `internal/cli` for the explicit subcommand, and tests. The existing cleanup endpoints, daemon recover flow, scheduler, and unattended cleanup ticker should remain untouched.

The biggest false-positive risk is killing a legitimate user-run or installed `mcphub gui` / `mcphub supervise` process just because it has the same basename and a similar argv. The guard is to require all strong proofs together: verified test image path, verified live test env with non-production state override, exact committed argv branch or supervised-child branch, minimum age, fresh command-line/env reread, token match, and `{PID, ExecutablePath, StartedAt}` termination proof. If any proof is unavailable, the process is spared.

Recommendation: keep this operator-invoked only. Do not add it to the unattended five-minute ticker. Promotion to the auto-ticker would require additional evidence not present at HEAD: a mature live env reader, zero false positives across real installed/repo/manual hub fixtures, committed coverage for any `f1-cli-verify`-style path, and a soak period proving no legitimate hub process matches the predicate.

Gate decision: PASS for design as an opt-in lane. REVISE before implementation if the chosen implementation cannot verify live process environment; without env proof, the predicate should be reduced to preview-only diagnostics or refused entirely rather than killing on path and argv alone.
