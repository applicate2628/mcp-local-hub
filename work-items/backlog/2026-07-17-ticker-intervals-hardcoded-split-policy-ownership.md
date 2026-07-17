# Policy values have no single owner — a form-driven registry, plus ghost settings that nothing reads

Filed: 2026-07-17
Severity: **P2** (4 items) + **P3** (4 items). **No P1 survived.**
Source: a 5-lane audit (2× codex-sol + 3× codex-terra, reasoning effort `max`), raised by the project
owner after finding one hardcoded ticker interval, then gated by an independent arbiter.

> **Severity note — every alleged P1 deflated on contact with the code.** The finding lanes proposed
> four P1s (test-only env seams honored in production; the release pipeline shipping a state-path hole;
> a security env var that is a ghost; destructive auto-cleanup). The arbiter refuted or downgraded all
> four by verifying reachability and blast radius rather than the call graph alone:
> - **env seams**: anyone who can set the GUI process's environment already executes code as that user
>   and can kill the supervisor or replace the binary — the seams grant no power the attacker lacks;
>   the two dangerous ones already warn and fail loud (`errNoopSchedulerMutation`);
> - **release pipeline**: `npm-publish.yml:110-120` builds with no `-tags`, and the fallback is behind
>   `//go:build test_state_path_env` — a compile-time exclusion. Published binaries are clean **today**;
>   what is missing is the PROOF, not the fix;
> - **`MCPHUB_ALLOW_UNHARDENED_STATE_WRITE`**: the lane conflated it with `..._READ`, which has a live
>   consumer (`daemon_env_overlay/parent_check.go:60-102`). Only the WRITE var is dead;
> - **auto-cleanup**: `apply:true` reaches a heavily-gated reaper (T3 demotion of generic names,
>   fail-closed sparing on unreadable client configs, a 600 s kill-age floor, and a held-handle
>   `{executable, basename, start-time}` re-verify). Kills stay bounded to verified orphans.
>
> The lesson is worth as much as the findings: a call-graph audit generates plausible P1s; only a
> reachability question ("who can actually do this, and what could they already do?") sizes them.

## The rule that explains all of it

> **A policy value enters the settings registry when someone explicitly wires a Settings form field.**
> Values used only by a ticker, constructor, cache, retry loop, or backend component stay literals.

This is a **form-driven registry, not a feature-policy registry**. Nothing decided that the prune
*threshold* is operator policy and the prune *cadence* is not — the threshold got a key because a
screen needed to render it. The rule is predictive: it says exactly where the next gap appears — any
new policy value whose feature has no Settings field.

## Why the Phase-1 architecture audit could not see this class

`work-items/archive/2026-07/2026-06-17-phase1-audit-findings.md` swept for duplicated **logic**: four
state classifiers, two config-path derivations, two state projections, hand-rolled atomic writers. It
even reported, as its P1, the identical *shape* — "one conceptual decision has four independent owners".

But **a literal normally has exactly one code location**, so a duplication-oriented lens reports one
owner and moves on. The split only becomes visible when you group by FEATURE and ask *who owns this
policy*, not *who owns this code*. The audit asked the second question and never the first.

**A complete policy audit needs two reconciliations, not one:**
1. runtime policy value → a registry key or a documented invariant;
2. **registry key → a live runtime consumer and a reachable control surface.**

Reconciliation 2 has never been run. That is how the ghost settings below survived.

## P2 — settings the GUI shows and saves, that nothing honours (arbiter-verified)

The operator changes these, sees them persist, and believes they took effect. A hardcoded value is
*honest* by comparison — it never claims to be tunable. These are the causal offspring of the rule
above: the keys exist because a form renders them, not because a policy owner reads them.

| Key | What actually happens |
|---|---|
| `daemons.retry_policy` | Rendered + persisted (`SectionDaemons.tsx:185-191`); `PolicyFromString` (`internal/api/retry.go:21`) has **zero production callers — only its own five tests**. Its registry Help (`settings_registry.go:168-178`) still says *"the watchdog `--once` driver reads this setting at tick start … takes effect on the next watchdog tick (~5 min)"* — **the watchdog was deleted in v0.6 Phase D**. The GUI renders a fleet-behaviour dial that does nothing, and its help names a component that no longer exists. |
| `gui_server.browser_on_launch` | Rendered + persisted, and covered end-to-end (Go tests, frontend tests, a Playwright test that clicks the checkbox). Launch reads only the CLI flag: `if !noBrowser` (`internal/cli/gui.go:789-794`). **No `SettingsGet("gui_server.browser_on_launch")` exists anywhere in Go.** The operator unchecks it; the browser opens anyway. |
| `daemons.weekly_schedule` | **Not a save failure** — `weeklyScheduleHandler` transactionally swaps the real trigger (`gui/daemons.go:262-281` → `api.SwapWeeklyTrigger`). The defect is a **clobber-back by a second writer**: `EnsureWeeklyRefreshTask` (`weekly_refresh.go:76-87`) delete-recreates the task with a hardcoded `WeeklyTrigger{DayOfWeek:0, HourLocal:3}` and never reads the setting. It fires on every non-supervised `mcphub register` (`register.go:233-236`), and `--supervised` defaults to false (`cli/register.go:97`). The operator's schedule silently reverts to Sun 03:00 on the next workspace registration. Split ownership of one value between two writers. |

**The test-coverage trap** (worth its own lesson): `browser_on_launch` and `retry_policy` are *well
tested*. The tests pin the declaration, the rendering, the click, and the PUT round-trip — every link
except *"…and something reads it"*. Tests prove a function works; they do not prove anyone calls it.
**Dead code with good coverage looks healthier than live code without it.**

Also unreachable but honest (rendered-and-disabled, no false promise): `appearance.shell`,
`appearance.default_home` (`Deferred: true`), `gui_server.tray` (excluded from `EDITABLE_KEYS`,
`SectionGuiServer.tsx:28-38`).

## P2 — the release pipeline has no proof the test-only state path is excluded, and the docs claim it does

`state_paths_envfallback.go:1` is `//go:build test_state_path_env`; `npm-publish.yml:110-120` builds
every platform binary with **no `-tags`**. So the fallback is compile-time excluded and **published
binaries are clean today** — this is a missing *gate*, not a shipped hole. What is real:
- the only `go tool nm` assertion lives in `ci.yml:115-138`, which is `workflow_dispatch`-only, and
  `ci.yml:18-21` states outright that a release `v*` tag triggers **only** npm-publish.yml. Deleting the
  `//go:build` line would ship undetected;
- **`CLAUDE.md` asserts "the release-pipeline `go tool nm` assertion … covers both"** — the assertion is
  not in the release pipeline. The doc claims a gate that does not exist.

## P3 — the rest (verified, non-blocking)

- `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE` (`client_write_init.go:216-223`) + its parser
  (`:863-866`): **zero production callers** — writes go through `secureWriteStateFileWithParentRelax`
  (`state_file_helper.go:118-162`), which consults strict-mode only. The api-package copy of the READ
  parser (`:248-251`) is likewise uncalled, so the env-var doc at `:225-242` claiming
  supervisor-state-read scope is **stale**: the var's live scope is the daemon-env-overlay gate
  (`daemon_env_overlay/parent_check.go:60-102`) only. Dead code + doc rot.
- Auto-cleanup: the comment at `gui_cleanup_ticker.go:18-20,104-108` claims a per-tick env re-read lets
  an operator *"disable the ticker live … no restart needed"*. **False** — `os.Getenv` reads the
  exec-time process env block; no external `setx`/HKCU write mutates a running process. The repo
  documents this correctly 30 files away (`state_relax_setting_windows.go:11-14`). Fix the comment; the
  kill path itself is soundly gated. Optionally add `maintenance.auto_cleanup_orphans`.
- `backups.keep_n`: registry `Min: 0` (`settings_registry.go:181-182`) vs runtime rejecting `n <= 0`
  with a **stderr-only** warning invisible to a GUI operator, silently falling back to 5
  (`backup_keep.go:44-47`). The comment already admits the schema should expose `backups.enabled`.
- Silent test seams `MCPHUB_GUI_TEST_PIDPORT_DIR` (`gui.go:269-270`) and `MCPHUB_MANIFEST_DIR_OVERRIDE`
  (`manifest_source.go:25-26`) lack the warn-on-activation symmetry the supervisor/scheduler seams have.

## P2 — split ownership, by feature

**41 ticker/interval sites exist repo-wide** (the owner's initial survey found 6). The full inventory is
in the audit lane outputs; the load-bearing splits:

| Feature | Registry-owned | Still a literal |
|---|---|---|
| Workspace pruning | `auto_prune_workspaces`, `prune_idle_hours`, `prune_dead_worktrees` | sweep cadence `60s` (`gui.go:624`); per-sweep deadline `30s` (`gui.go:1069`); the `2`-tick grace |
| Serena idle lifecycle | `serena_idle_shutdown` | idle sweep `60s` + pass deadline `10s`; backend-loss reconcile `30s` + IPC `5s`; post-idle grace `2` ticks |
| Weekly maintenance | `weekly_refresh_default`, `weekly_schedule` | schedule-evaluator cadence `60s`; and the schedule itself is overwritten (see P1) |
| Daemon retry/quarantine | `retry_policy` (ghost) | failure window `30m`, quarantine threshold `10`, backoff step, parole `15m`→`2h`, dwell `2m`, scan `30s` |
| Hub listener health/restart | `hub_endpoint_enabled`, `port` | probe `15s`/timeout `2s`/`3` failures; restart `5s` base, `30s` same-port, `6m` limit, `5` consecutive, `20`/`30m` |
| Hub sessions + fan-out | hub gate only | per-client cap `16`, global `256`, idle TTL `30m`, sweep `60s`; fan-out concurrency `8`, init `5s`, list `10s`, call `60s` |
| Lazy LSP backends | **none** (hard cap + idle TTL are hidden CLI flags only) | cap `16`, TTL `30m`, reap `1m`, materialize `15s`, request ceiling, router timeout `150s` |
| Router-session retention | **none** | three separate `24h` TTLs; cleanup cadence `1h`; router capacity `4096` |
| Supervisor liveness | **none** | probe `300ms`, post-bind grace `5s`, sweep `5s`, `2`-sweep identity-mismatch strike |
| Automatic orphan cleanup | **none** (negative-polarity env gate `MCPHUB_DISABLE_AUTO_CLEANUP`) | cadence `5m` |

## Traps — why "just raise the interval" is wrong

Several graces are counted in **ticks, not elapsed time**, with the counter held **in memory**:
- prune missing-dir/dead-worktree grace = `2` same-reason ticks (`workspace_prune_sweeper.go`);
- serena post-idle grace = `serenaBackendPostIdleGraceTicks = 2`;
- liveness identity-mismatch = `2` consecutive sweeps.

So the real grace is `2 × interval`, and it **resets on every GUI/supervisor restart**. Raising the
prune interval to 10 min silently turns "prunes in ~2 min" into "may never prune on a host that
restarts often". **A tick count is standing in for elapsed time.** Any interval made settable must have
its grace migrated to a duration first, or the setting hands the operator a footgun.

Same class: durations that must stay ORDERED (the LSP router documents
`ColdStartMaxProbation > forward timeout > ColdRequestHoldCeiling > MaterializeWaitBudget` and clamps a
misordered config). Exposing one of those without enforcing the ordering is a footgun.

## What is NOT broken

The tickers are separate goroutines, each with one job — **the separation of work is correct**. Only
config ownership is broken. Do not "fix" the goroutine structure.

## Migration shape

1. Literal → registry key with the current literal as its `Default` (behaviour unchanged on upgrade).
2. Migrate tick-count graces to durations BEFORE exposing their intervals.
3. Values read once at construction need the existing pending-restart badge convention (see
   `gui_server.port`), not a pretence of live application.
4. **Fix the ghosts first** — a setting that does nothing is worse than a literal, and P1 here is a
   4-item list, not a refactor.
5. Add a structural guard for reconciliation 2: a test that every registry key has a live production
   reader, so the next deletion cannot orphan a setting silently.
