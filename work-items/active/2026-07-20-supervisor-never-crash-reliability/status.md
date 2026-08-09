# Supervisor reliability — "must not crash AT ALL"

Status: active

Template: full-delivery (requiresLead: true)
Owner: main conversation (Lead)
Opened: 2026-07-20
Current step: investigation complete → design lanes dispatched

## Operator mandate (verbatim)

> "причина в том, что он хуевый, и падает у нас тоже (просто тут запущено
> автовосстановление видимо) а работать должен так, чтобы не падал ВООБЩЕ"

> "да так и есть, рестарт не помогает даже"

> "демон в карантине после повторенных фейлов"

Laptop incident: hub ran ~15 min, a daemon quarantined, restart does not recover.
Operator asserts the same failure class occurs on the main host but is masked by
auto-recovery (`\mcp-local-hub-liveness` + supervisor respawn).

**The mandate is not "recover faster" — it is "do not fail".** Auto-recovery
masking a recurring crash is treated as a defect, not a mitigation.

## Evidence captured (2026-07-20, main host, state dir `%LOCALAPPDATA%\mcp-local-hub`)

### E1 — fleet-wide crash churn is real and massive

`supervisor-state.json`, `pid_generation` per daemon (respawn counter):

```
   782  lsp-b133f336-go                  running
   612  serena-b133f336                  running
   577  serena-6935d24c                  running
   557  memory-default                   running
   554  time-default                     running
   447  workspace-weekly-refresh         idle
   366  paper-search-mcp-default         running
   ...
     33  excalidraw-default              running
      3  codegraph-default               running
```

TOTAL 8215 respawns across 32 daemons; 29 daemons >10 generations; 24 >50.

> ## ⛔ E1 IS FALSIFIED — DO NOT BUILD ON IT
>
> **The discriminator I originally wrote here was wrong**, and two independent
> lanes (fable adversarial + L2) killed it separately. Original claim: *"a
> supervisor restart respawns the WHOLE fleet, so deploy-driven generations would
> be roughly EQUAL; the observed 3-vs-782 spread therefore proves per-daemon
> crash respawns."*
>
> **Why it fails.** `MarkSpawned` (`supervisor_runtime_tracker.go:319-338`)
> increments `PIDGeneration` on EVERY spawn, and `:573` hydrates it from
> `supervisor-state.json` at cold start. It is a **lifetime cumulative spawn
> counter**. The equal-bump-per-restart argument only holds for daemons of equal
> age — which I never checked.
>
> **The spread is REGISTRATION AGE.** In the 42.4 h window: 9 `supervisor-start`
> events, 297 `daemon-spawned`, and the always-on global daemons cluster tightly
> at 347-557 — exactly the "equal across daemons" signature the discriminator
> predicted for fleet restarts. The low outliers are recent registrations:
> `codegraph-default` = 3 (first appears 2026-07-19), `serena-a7fe3d70` = 5
> (written into intent at 11:18:02 by `serena-intent-repair-applied`),
> `excalidraw` = 33. The two extremes (`lsp-b133f336-go` 782, `serena-b133f336`
> 612) are simply the oldest and most-used — the dev workspace itself.
>
> **The headline daemon was healthy.** `lsp-b133f336-go`, my "782 crashes"
> example, had **9 spawns, 0 exits, 0 crash respawns** in the window.
>
> **8215 reconciles as benign.** In-window spawn rate ≈168/day → 8215 ≈ 49 days
> ≈ birth ~2026-06-01, matching the v0.5.0-supervisor-era state file. Most of the
> historical total is an **extinct** port-in-use epidemic (2072 lifetime failures,
> 70% of all historical failures, self-amplifying respawn→port-still-held→fail;
> collapsed after 2026-07-10) plus fleet-restart accumulation across months of
> differing registration ages.
>
> **Correct metric (L2 Gate-2 failure, binding on L4):** crash respawns per daemon
> per 24 h over a **bounded window** (`daemon-respawn-fired`), NOT lifetime
> `pid_generation`. Current fleet reads **38 crash respawns / 42.4 h with 17 of 32
> daemons at zero**. A dashboard built on the raw lifetime counter would report an
> already-fixed problem as live.
>
> **Actual in-window attribution:** 297 spawns = 259 fleet-restart (87%) + 38
> crash-driven (13%). **The dominant multiplier is the supervisor dying, not
> daemons dying** — 8 of 9 supervisor starts had no `supervisor-exit` row, each
> costing ~29 daemon respawns.
>
> Full refutation: `REVISE-diagnosis-refuted.md` in this folder.

### E2 — permanent spawn errors are retried as if transient (⛔ NOT the root cause — see below)

> **E2's causal chain is REFUTED; only its mechanics survive.** The chain never
> completed even once on this host: **zero** `daemon-quarantined` events in either
> log, and the highest `failures_in_30m` ever observed was **7** against a
> threshold of 10. The invalid-cwd loop ran 20:55:04→20:56:08 (7 attempts), then
> the **already-shipped** stale-workspace guard fired (`stale-workspace-skipped`,
> PR #244, v0.4.10+) and the orphan reaper cleaned up — total incident **~2.5
> minutes, self-healed**.
>
> Worse for the theory: *"restart does not help"* **argues against** it. Serena
> intent repair runs at supervisor start and drops the stale row (observed doing
> exactly that), so a restart would permanently CURE invalid-cwd.
>
> And the proposed classifier is **empirically impossible**: probes on this host
> show CreateProcessW collapses deleted-dir, missing-parent, file-as-dir,
> unreachable-UNC (`\\nonexistent-host\share` → 267, not 53) and unmounted-drive
> (`W:\` → 267, not 21/3) into the SINGLE code 267. Codes 2/3/53/21 appear only on
> the non-production path (bare `exec.Cmd` without `SysProcAttr`, where Go's
> `os.Stat` pre-check runs); production always sets `SysProcAttr`
> (`supervise.go:3566` → `noconsole_windows.go:38-42`) so only 267 is ever seen.
> **There is no discriminating signal to classify on.**
>
> Routing to `errSpawnJobProtectionRefused` would also make things WORSE: that
> target is an *absorbing* quarantine with no parole
> (`supervisor_controller.go:3538-3544`, `:3607-3608`), so a not-yet-mounted
> network share or locked volume — which yields the same 267 — would be parked
> permanently on every reboot, whereas today's parole ladder auto-recovers it
> within 15 min.
>
> **Replacement (fable D4): a pre-spawn workdir hold-gate**, reusing the shipped
> `preSpawnPortGateHold` shape (`supervisor_controller.go:3513-3521`, `:4017-4024`)
> that holds in backoff WITHOUT a crash-count increment. `os.Stat(d.Workspace)`
> before create-process; on failure hold + re-probe each tick, never spawn, never
> touch the budget; auto-proceeds the first tick after the volume appears. Fail
> OPEN for spawn on Stat `ERROR_ACCESS_DENIED` (probed: an ordinary deny-ACE dir
> still spawns fine — bypass-traverse-checking is granted to Everyone by default),
> fail closed only on not-found classes.
>
> What survives of E2: spawn failures DO feed the same `failures_in_30m` budget as
> crash exits, and the constants are as cited. The `supervise.go:3217` "reconciler
> swallows that error" godoc is textually accurate but **non-load-bearing** — the
> controller counts them anyway.

`supervisor-events.log.1`, `daemon-spawn-failed`:

```json
2026-07-18T20:55:04 serena-4f8e3c32 {"command":"%USERPROFILE%\\.local\\bin\\mcphub.exe",
                                     "err":"CreateProcess: The directory name is invalid.",
                                     "orphan": false}
2026-07-18T20:55:05 serena-4f8e3c32 {... same ...}
2026-07-18T20:55:07 serena-4f8e3c32 {... same ...}
```

Once per second, same permanent error. `CreateProcess: The directory name is
invalid.` can never become transient — the working directory does not exist.

Code confirms there is no classification (`internal/cli/supervise.go:3217` godoc):

> "Errors from cmd.Start propagate up via the SpawnFunc return value;
> Reconciler **swallows that error** (per `supervise_reconcile.go:118`) because
> the audit row is the canonical operator-visible signal."

Consequence chain:
1. permanent spawn error → counted as an ordinary crash
2. `respawnQuarantineThreshold = 10` failures inside `respawnFailureWindow` (30 min)
   → **Quarantined** (`internal/cli/supervisor_controller.go:4366`)
3. parole retries at 15 min → ×2 → cap 2 h
   (`supervisor_controller.go:424-438`) hit the SAME permanent error → re-quarantine

This reproduces the laptop report exactly: ~15 min to burn the threshold, then a
daemon parked in quarantine, and restart does not help because the invalid
directory survives the restart.

### E3 — other live failure lanes in the same rotated window

| count | event | note |
|---|---|---|
| 1656 | `state-file-write-unhardened-fallback` | DACL relax firing constantly; log noise + posture question |
| 134 | `ipc-hello-write-error` | body: `write hello: The pipe is being closed.` — IPC handshake race |
| 82 | `daemon-port-owner-unverified` | supervisor lost-child / squatter class |
| 58 | `daemon-exited` | exit 1 ×34, exit 0 ×13, `0xFFFFFFFF` ×10, `0x40010004` ×1 (DBG_TERMINATE) |
| 40/38 | `daemon-respawn-scheduled` / `-fired` | |
| 8 | `supervisor-start` | supervisor itself restarted 8× in the window |
| 8 | `daemon-running-state-stale` | |
| 7 | `daemon-spawn-failed` | E2 above |

serena is the top exiting daemon (29 exits across workspaces) — i.e. the failure
concentrates on the product's primary reason to exist.

### E4 — observability gap

- Current `supervisor-events.log` (since 09:12) is silent apart from test noise —
  crashes are not being surfaced anywhere an operator would look.
- Test runs leaked rows into the PRODUCTION event log
  (`r:\Temp\...\scratchpad\iso-baseline\gui.pidport`) — the log is not isolated
  from test executions.
- There is no crash-rate surface at all: nothing counts respawns, so an
  8215-respawn fleet reads as "healthy" on the dashboard.

## Design lanes (dispatched in parallel)

| Lane | Scope |
|---|---|
| L1 | Permanent-vs-transient spawn/exit failure classification + terminal `Misconfigured` state with actionable operator message (owns E2) |
| L2 | serena exit `0xFFFFFFFF` / exit-1 churn root cause (owns E3 serena rows) |
| L3 | IPC handshake `pipe is being closed` ×134 + `daemon-port-owner-unverified` ×82 |
| L4 | Crash-rate observability: respawn counters, health surface, event-log isolation from tests, DACL-fallback noise (owns E4 + the 1656 rows) |

Cross-family verification: Sol (codex gpt-5.6-sol, xhigh) on the L1 classification
design, per the standing balance rule. Fable acceptance mandatory before PR.

## Non-negotiables for any proposed fix

- A permanent error must NOT consume the transient-retry budget.
- A daemon that cannot possibly start must reach an honest terminal state with a
  concrete operator instruction — never silent, never an infinite loop.
- Auto-recovery must not be the thing that makes a recurring crash invisible.
