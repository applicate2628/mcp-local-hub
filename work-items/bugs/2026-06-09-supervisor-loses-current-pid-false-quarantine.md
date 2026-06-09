# Supervisor loses current_pid for a live child → port-in-use respawn loop → false quarantine

- **Status:** Layer A root cause CONFIRMED + fixed (branch
  `fix/supervisor-liveness-identity-path`); Layer B (adopt-on-port) still open as
  a separate defensive follow-up
- **Date:** 2026-06-09
- **Severity:** P2 — operator-visible status lie + recovery risk (service currently still up via the orphan; goes truly down if the orphan dies or on the next supervisor restart without the fix)
- **Found by:** live diagnosis of `time` showing GUI State = Quarantined while the MCP service still answered
- **Related:** PR #268 (supervisor liveness/restart fixes) — same domain; the #268 deploy's supervisor restart resolves the *current instance* (see Workaround) but does not necessarily prevent recurrence. Sibling symptom class to the memory phantom-running case #268 B1 targets.

## Summary

The supervisor can end up with `state=quarantined`, `current_pid=0` for a daemon
whose ORIGINAL child process is still alive, still bound to its port, and still
serving clients. The status surface (GUI / `mcphub status`) reports the daemon
as Quarantined/down while it is functionally up — a split-brain where the status
lies. The quarantine itself is spurious: it was produced by a port-in-use
respawn loop, not by the daemon actually failing.

## Evidence (captured 2026-06-09, host state-dir `%LOCALAPPDATA%\mcp-local-hub\`)

`time` daemon (`\mcp-local-hub-time-default`, port 9128, manifest runs
`npx -y @mcpcentral/mcp-time@0.0.5` via the stdio-bridge wrapper):

- **supervisor-state.json:** `state="quarantined"`, `current_pid=0`,
  `quarantine_since=null`, `restart_history=null`, `backoff_until=null` — i.e.
  quarantined with NO quarantine metadata (inconsistent on its own).
- **Live process tree:** PID `167352` = `C:\Users\dima_\.local\bin\mcphub.exe
  daemon --server time --daemon default`, parent `109756` =
  `D:\dev\mcp-local-hub\bin\mcphub.exe supervise`, **CreationDate 2026-06-04**
  (alive 5 days). `netstat`: `127.0.0.1:9128 LISTENING 167352` + an ESTABLISHED
  client connection. So the supervisor's OWN child (167352) is alive and serving
  — but the supervisor's state for it says `current_pid=0`.
- **supervisor-events.log (2026-06-08T23:41):**
  - `daemon-spawned` pid `246576` (a NEW time child) — supervisor thought time
    was down (current_pid=0) and respawned.
  - `daemon-exited exit_code=1` for 246576, ~1s later (could not bind 9128 — held
    by the still-alive untracked 167352).
  - `daemon-quarantined reason="10+ failures in 30-min sliding window; respawn
    attempts suspended until supervisor restart"`, `failures_in_30m=10`.
- **Functional check:** `mcp__time__current_time` through the hub (→ 9128 → 167352)
  returned a correct result. Service is UP despite the Quarantined status.
- **Package check:** `@mcpcentral/mcp-time@0.0.5` exists in the npm registry — NOT
  a missing/yanked-package failure.

## Mechanism

1. Supervisor (109756) spawned time child 167352 on 2026-06-04, bound 9128. Healthy.
2. At some later point the supervisor's `current_pid` for time was reset to 0 while
   167352 stayed alive (the supervisor process itself never died, so its Job Object
   never closed → the child was never reaped). **Trigger CONFIRMED (2026-06-09):**
   the DAEMON-identity checks built `process.PIDIdentityProof` with
   `ExecutablePath = canonicalMcphubPath()` — the SUPERVISOR's OWN `os.Executable()`.
   The evidence above shows the supervisor (109756) ran from
   `D:\dev\mcp-local-hub\bin\mcphub.exe` while the time child (167352) ran from
   `C:\Users\dima_\.local\bin\mcphub.exe`. Because the two paths differ,
   `process.VerifyPIDIdentity` returned `ErrProcessIdentityMismatch`
   ("executable path mismatch") for the live, correctly-serving child. The
   liveness sweep classified it `pid_identity_mismatch` → `EvManualRestart`; the
   competitor could not bind 9128 → exit 1 → `current_pid` lost → split-brain. The
   terminate path ALSO failed identity (could not verify → could not kill),
   worsening the orphan. **Live fleet impact:** 86 `pid_identity_mismatch` events
   across the daemon fleet on 2026-06-09, all explained by this single
   supervisor-vs-daemon path mismatch.
3. Seeing `current_pid=0`, the supervisor treated time as not-running and respawned
   (246576).
4. 246576 could not bind 9128 (held by the untracked-but-alive 167352) → exit 1
   within ~1s.
5. Repeated 10× in 30 min → quarantine, respawn suspended.

## Root cause (two layers)

- **Layer A (trigger, CONFIRMED + fixed):** the daemon process-identity checks
  compared the daemon's live PID against the SUPERVISOR's own
  `os.Executable()` (`canonicalMcphubPath()`) instead of the daemon's CONFIGURED
  command (`d.Command` — the exact exe the supervisor `exec.Command(d.Command,
  d.Args...)`'d, written into `supervisor-intent.json`). When the supervisor runs
  from a different binary than the daemons (dev build vs `~/.local/bin`), every
  live daemon fails the path-match in `process.VerifyPIDIdentity` → false
  `pid_identity_mismatch` → restart churn → lost `current_pid`. Fixed at the three
  daemon-identity sites (liveness sweep, terminate proof,
  `loadSupervisorCurrentRunning` startup scan incl. its inner port re-check) by
  comparing against `d.Command` (normalized), with a `canonicalMcphubPath()`
  fallback only for legacy rows whose `Command` is empty. The GUI single-instance
  KILL gate (`process_identity_match_{linux,windows}.go`) is intentionally left on
  `canonicalMcphubPath()` — there the running GUI *is* the binary being matched,
  so its own path is correct.
- **Layer B (the amplifier, confirmed — still open):** the respawn path, on
  `current_pid=0`/not-running, does NOT check whether the assigned port is already
  bound by a process matching the daemon's OWN identity before spawning. So it
  fights its own untracked child for the port and quarantines a healthy daemon.
  Layer A removes the dominant trigger; Layer B remains a worthwhile defensive
  follow-up so any *other* future `current_pid=0` cause recovers by adoption
  instead of a port fight.

## Fix direction

- **Layer A fix (DONE — branch `fix/supervisor-liveness-identity-path`):** at the
  three DAEMON-identity sites, build the identity proof's `ExecutablePath` from the
  daemon's configured `d.Command` (normalized via `filepath.Abs` + `EvalSymlinks` +
  `Clean` to match `process.normalize{Windows,Expected}ExecutablePath`), falling
  back to `canonicalMcphubPath()` only when `Command` is empty:
  - `internal/cli/supervise_liveness.go` `supervisorDaemonEntryLive` — uses
    `daemonExpectedIdentityExe(d.Command)`.
  - `internal/cli/supervise.go` `makeProductionTerminateFnWithStatePath` terminate
    proof — uses `daemonExpectedIdentityExe(d.Command)` (threaded through verify →
    terminate → finish).
  - `internal/cli/supervise.go` `loadSupervisorCurrentRunning` — builds a per-task
    `task → Command` map from `supervisor-intent.json`
    (`supervisorIntentCommandMapForStateDir`) since `supervisor-state.json` has no
    Command; both the outer identity check AND the inner port-liveness re-check use
    the daemon's command.
- **Layer B fix (defensive, still open):** before respawning a daemon whose
  `current_pid` is 0/unknown, probe the assigned port; if it is bound by a process
  whose identity matches this daemon (image basename + cobra `daemon --server X`
  argv, per the existing identity gate used elsewhere), ADOPT it (record its PID as
  current_pid, mark running) instead of spawning a competitor. This converts the
  split-brain into recovery and prevents the spurious quarantine. Mirrors the
  listener-PID-ownership work in #268 r4-F4 but for the `current_pid==0` case.
- **Consistency:** a `state=quarantined` with `quarantine_since==null &&
  restart_history==null` should be treated as a corrupt/incomplete quarantine on
  load and reconciled, not trusted.

## Workaround (immediate)

Restart the supervisor (this is also the #268 memory-revival step):
closing supervisor 109756 lets the Job Object `KILL_ON_JOB_CLOSE` reap the orphan
167352 → 9128 frees → the new (post-#268) supervisor respawns time cleanly and the
quarantine clears. **Caveat:** a supervisor restart reaps the ENTIRE daemon fleet
(all LSP backends + every MCP daemon), so it interrupts any live multi-project work
— do it at a deliberate moment, together with the #268 deploy, not mid-session.

Do NOT kill the orphan 167352 on its own: time is quarantined (respawn suspended),
so killing the orphan takes time fully down with no auto-respawn until a supervisor
restart.

## Scope decision

Layer A is fixed as a separate focused change on
`fix/supervisor-liveness-identity-path` (post-#268), scoped to the daemon-identity
sites only (no GUI-gate / intent-file changes). The earlier workaround (run the
supervisor from `~/.local/bin` so it matches the daemons' install path) is now
unnecessary once this branch ships — the supervisor may run from any path. Layer B
(adopt-on-port) remains a separate defensive follow-up.
