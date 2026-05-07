---
title: Task Scheduler RestartOnFailure does not auto-recover daemon after Task Manager End Task (or `Stop-Process -Force`) despite correct XML config
severity: medium (operator self-recovery promise from verification doc D2.4 doesn't hold; daemon stays Stopped indefinitely after kill until manual `mcphub restart`)
found-by: D2.4 manual smoke during preview-tag verification on master HEAD d99af40
found-on: 2026-05-07
project: mcp-local-hub
related-pr: pre-existing — present before PR #131/#132/#133 (G2 didn't touch internal/scheduler or daemon wrap)
---

# Task Scheduler RestartOnFailure not firing on daemon kill

## What happens

The verification doc `docs/phase-3b-ii-verification.md` D2.4 step 3 promises:

> "End the process from Task Manager → Daemon exits non-zero; Task Scheduler RestartOnFailure (3 retries × 1 min) kicks in"

In practice (verified 2026-05-07):

1. `mcphub restart --server memory` → memory daemon Running, PID=X.
2. Task Manager → Details → find PID X (`mcphub.exe daemon --server memory --daemon default`) → End Task.
3. Task Scheduler records `LastTaskResult = 0x1` (non-zero, real failure indicator).
4. Wait 75 seconds (past the `Interval=PT1M` configured restart interval).
5. Task is in State=Ready, not Running. NO `mcphub.exe daemon` process is running. RestartOnFailure did NOT trigger.

After waiting an additional 2 minutes (>3× the configured interval): same result, task still Stopped.

## Configuration check (confirms config is correct)

```xml
<Settings>
  <RestartOnFailure>
    <Count>3</Count>
    <Interval>PT1M</Interval>
  </RestartOnFailure>
  ...
</Settings>
```

Config matches what the verification doc expects. Yet behavior diverges.

## Two kill methods produce different LastTaskResult

| Kill method | LastTaskResult | RestartOnFailure |
|---|---|---|
| `Stop-Process -Force -Id X` (PowerShell, equivalent to `taskkill /F`) | `0xFFFFFFFF` (-1, "no result" placeholder) | NO |
| Task Manager → Details → End Task | `0x1` (exit code 1) | NO |
| `mcphub stop --server memory` (graceful) | `0x1` | n/a (intentional stop) |

So the kill-via-Task-Manager produces a non-zero exit (correct failure signal), but Task Scheduler still doesn't honor RestartOnFailure.

## Root cause hypotheses (not yet diagnosed)

### H1 — Conditions filter blocks restart

Possible Settings.Conditions block (e.g. battery state, network availability, idle state) prevents the task from starting via RestartOnFailure even though the kill triggered the failure path.

Verify by reading the full XML:

```bash
schtasks //Query //TN "\\mcp-local-hub-memory-default" //XML
```

Look for `<Conditions>` section — `<DisallowStartIfOnBatteries>true</DisallowStartIfOnBatteries>` or similar gates.

### H2 — `<MultipleInstancesPolicy>` set to `IgnoreNew` or `Queue`

If the policy is `IgnoreNew`, Task Scheduler refuses to launch a second instance. After kill, the prior instance might still be in some "completing" state. Check XML for `<MultipleInstancesPolicy>`.

### H3 — Task Scheduler "Hidden" / per-user / not-running-on-user-session-flag

Tasks configured for non-interactive sessions might not restart from a user-killed scenario the way the doc assumed.

### H4 — Doc was written aspirationally and never verified

The verification doc may have been written expecting RestartOnFailure to work without it ever having been tested end-to-end. The XML config is correct in intent but doesn't actually produce restart behavior on this Windows version (Win11 24H2+).

### H5 — `mcphub.exe daemon` exit-code propagation broken

Even though `LastTaskResult=1` looks correct from the API side, internally Task Scheduler might be storing some other state that prevents restart.

## Diagnostic data captured

```text
$ Get-ScheduledTask -TaskName 'mcp-local-hub-memory-default' | Get-ScheduledTaskInfo
State              : Ready          (idle, ready to run on demand — NOT running)
LastRunTime        : 2026-05-07 17:22:28
LastTaskResult     : 0x1 (1)        (after Task Manager kill)
NumberOfMissedRuns : 0
NextRunTime        : (blank)        (no scheduled next run)

$ Get-ScheduledTask -TaskName 'mcp-local-hub-memory-default' | % Settings
RestartCount         : 3
RestartInterval      : PT1M
AllowDemandStart     : True
ExecutionTimeLimit   : PT0S          (no limit)
StartWhenAvailable   : False
DisallowStartIfOnBatteries : False
```

Task Scheduler Operational event log: empty (history not enabled on this host).

## Investigation next steps

1. Enable Task Scheduler History via `Get-ScheduledTask | Enable-ScheduledTask`-equivalent (actually via Task Scheduler Library → "Enable All Tasks History" in the right pane).
2. Reproduce kill, capture event log.
3. Look for "Task Scheduler did not launch task because the launch conditions specified in the task definition were not met" or similar event entries.
4. Cross-reference with the full task XML.
5. Cross-reference with a SECOND scheduled task on the host that DOES restart cleanly to identify config delta.
6. Test on a different Windows version (10 vs 11 vs Server 2022) to see if behavior is host-specific.

## Workaround (operator-side, until fix)

Manually run `mcphub restart --server <name>` after a kill. The CLI restart explicitly invokes `schtasks /Run` which works regardless of why the task stopped.

## Plan

- Defer to a dedicated daemon-recovery PR post-preview-tag
- Update `docs/phase-3b-ii-verification.md` D2.4 to reflect actual behavior (either fix the underlying issue OR change the doc to match observed reality + document the workaround)
- Add an integration test that kills a daemon and verifies the recovery path (likely will need to use `mcphub restart` rather than relying on TS auto-restart)

## Owner

TBD — needs Windows / Task Scheduler expertise; could be `reliability-engineer` or `platform-engineer` lane.

## Resolution

Resolved by the watchdog feature in branch `fix/v0.3.0-blockers` (Tasks
0–11 of `docs/superpowers/plans/2026-05-07-mcphub-watchdog.md` v13).
Rather than chase the underlying Task Scheduler `RestartOnFailure`
behavior on Win11 24H2+ (root cause not isolated; see hypotheses
H1–H5 above), v0.3.0 ships a separate per-user scheduled task
(`\mcp-local-hub-watchdog`) running `mcphub watchdog --once` every 5
minutes. Each tick walks the daemon registry, classifies failures via
the exported `IsRealFailure` predicate, and restarts eligible daemons
under a strictly-pure recovery state machine. Force-killed daemons
recover within ~5 min regardless of whether `<RestartOnFailure>`
fires.

**Status:** `closed` (mitigation shipped). Underlying root cause is
re-classified as an adjacent finding — see
`work-items/bugs/2026-05-07-task-scheduler-restartonfailure-not-firing.md`
itself for hypotheses to revisit if v0.4.x decides to re-investigate
the native restart path.

**Implementation commits (branch `fix/v0.3.0-blockers`):**

| Commit | Scope |
|---|---|
| `d149f3d` | `feat(api): foundational ctx-aware API surfaces + ownership snapshot + sealed audit constructors` |
| `abaec6d` | `feat(api): cross-platform state dir + KnownFolder + 0600 perms + sanity check` |
| `dc98b6d` | `feat(api): daemon intent — 3-state, TTL, clock-skew, UTC, post-rename quarantine + non-fatal prune + identity-oversize rejection` |
| `a7563c8` | `feat(api): intent audit log — sealed SystemEntry, identity-preserving 16KB, Priority, idempotent rotation` |
| `eb133ed` | `feat(api): watchdog state — fail-CLOSED, sliding-30min strikes, restart-pending(now-injected), stale-clear events` |
| `6f1e609` | `fix(scheduler): MultipleInstancesPolicy=StopExisting unblocks manual restart edge cases (bug #2 partial)` (Task 5) |
| `8afc238` | `feat(api): hardened owned-task XML validator + structural ownership` |
| `e59c93a` | `feat(api): watchdog recovery state machine (strictly pure) + IsRealFailure exported` |
| `9a67b68` | `feat(scheduler): watchdog scheduled task install/uninstall` |
| `94e3919` | `feat(cli): mcphub watchdog command — full v8 driver flow` |
| `a903c94` | `feat(api): intent + audit writes — fail-closed semantics` |
| `4e2740b` | `feat(cli): auto-install watchdog during setup; uninstall ordering` |

**Verification:** the `D2.4` row in `docs/phase-3b-ii-verification.md`
has been re-scoped to call out the recovery-via-watchdog cadence; the
new `D2.6` block carries a 16-sub-case manual smoke for the watchdog
flow (force-kill recovery, suspicious-XML rejection, chronic-failure
auto-disable, wall-clock jump, corrupt-strike self-quarantine,
singleton-lock contention, 16 KB cap, status redaction, etc.). The
operator-facing reference documentation lives in `CLAUDE.md` →
"Watchdog (Phase 3B-II onward)".

**Workaround paragraph above is now obsolete** — operators no longer
need to run `mcphub restart --server <name>` after a kill on a host
where the watchdog is installed (default after `mcphub setup`).
