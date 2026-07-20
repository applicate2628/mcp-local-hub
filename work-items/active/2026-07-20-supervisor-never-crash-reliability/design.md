# Design — accepted artifacts (L1 + L4)

Accepted by Lead 2026-07-20. Lanes L2, L3 and two fable lanes still in flight.

## L1 — permanent vs transient spawn-failure classification (gate: PASS)

### Corrections L1 made to the Lead's captured evidence — both accepted

**C1. The "~15 min" attribution in `status.md` was wrong about mechanism.**
The backoff ladder is `1s,2s,4s,8s,16s,32s` capped at 60s
(`supervisor_controller.go:4394,:4398`), so 10 failures burn in **≈4 minutes**,
not 15. The operator's 15 minutes is `quarantineParoleBaseDelay`
(`supervisor_controller.go:424`) — the FIRST parole retry, which re-hits the same
permanent error and re-quarantines on the doubling ladder. **The fix must stop
both the burn and the parole cycle**, not just the burn.

**C2. The Lead's hypothesised latent defect does not exist.**
Parole does NOT tick against absorbing quarantines. `runQuarantineParoleTick`
ranges only over `c.quarantineParole` (`:534`); absorbing sites call
`clearQuarantineParole` (`:3398`, `:3544`); `recordQuarantineParoleEligible`
fires only on `EvTimerDue` (`:3607`). Regression-locked by
`TestSupervisorController_QuarantineParole_AbsorbingStrictJob_NoParole`
(`supervise_lostchild_f6_f2_test.go:749`) and `..._AbsorbingLegacySerena_NoParole`
(`:714`). **Withdrawn — no finding.** L1 verified rather than inheriting the claim.

### The blocking discovery — two spawn paths, two errno for one root cause

`supervise.go:3663-3669`:

```go
if daemonJob != nil { pid, startErr = process.StartWithJob(daemonJob, cmd) }
else               { startErr = cmd.Start() }
```

| Path | Taken when | Invalid cwd yields |
|---|---|---|
| `StartWithJob` → raw `windows.CreateProcess` (`start_with_job_windows.go:167`) | normal, Job allocated | `CreateProcess: The directory name is invalid.` = **ERROR_DIRECTORY (267)** |
| `cmd.Start()` fallback | per-spawn Job-create failed, strict mode OFF (`supervise.go:3640`) | `chdir …: The system cannot find the path specified.` = **ERROR_PATH_NOT_FOUND (3)** |

The production incident came through `StartWithJob` (errno 267). A classifier
written against only the incident log would **silently fail on every host where
Job allocation degraded** — precisely the AppLocker/WDAC/handle-exhaustion hosts
in the CLAUDE.md "Job Protection field operator runbook". Both codes must match.

### Error-chain constraint — classify at the spawn site

- `errors.Is(startErr, windows.ERROR_DIRECTORY)` — works (`StartWithJob` wraps `%w`).
- `errors.Is(startErr, windows.ERROR_PATH_NOT_FOUND)` — works on the `cmd.Start` path.
- **`errors.Is(returnedErr, <errno>)` FAILS** because `supervise.go:3768` does
  `fmt.Errorf("%w: %v", errSpawnPreChild, startErr)` — the `%v` **flattens the
  cause out of the chain**. Same defect class CLAUDE.md already records (a `%v`
  wrap flattened `ErrWrongOwner`, bug 2026-07-08 F1).

⇒ **Classification MUST happen at `supervise.go:3671` where `startErr` is intact**,
not in the controller. Changing `:3768` to `%w` is rejected — it widens what every
downstream `errors.Is` can match.

Never use `syscall.ENOENT`/`ENOTDIR` on Windows; match `windows.Errno` constants.

### Classification table

**PERMANENT** (retry cannot help — precondition absent, not busy):
`ERROR_DIRECTORY` (267); `ERROR_PATH_NOT_FOUND` (3) + probe; `ERROR_FILE_NOT_FOUND`
(2) + probe; `exec.ErrNotFound`; `ERROR_BAD_EXE_FORMAT` (193); `ERROR_ACCESS_DENIED`
(5) only with probe confirming the binary exists and is readable; malformed
descriptor (structural, no errno); POSIX `ENOENT`/`ENOTDIR`/`ENOEXEC` + probe.
`errSpawnJobProtectionRefused` is already handled (`supervise.go:3627`).

**TRANSIENT** (default class): `api.IsPortBindRefusedErr` (WSAEADDRINUSE 10048 /
WSAEACCES 10013 — **never** `syscall.EADDRINUSE`); `ERROR_NOT_ENOUGH_MEMORY` (8);
`ERROR_NO_SYSTEM_RESOURCES`; `ERROR_SHARING_VIOLATION` (32); `ERROR_LOCK_VIOLATION`
(33); `ERROR_BAD_NETPATH` (53); `ERROR_NETNAME_DELETED` (64); POSIX `ENOMEM`,
`EAGAIN`; **and everything not on the permanent allowlist**.

**AMBIGUOUS, and how each resolves:**
1. `ERROR_PATH_NOT_FOUND` (3) — bad cwd / missing binary / momentarily-unreachable
   network path → resolved by the confirmation probe; unresolvable ⇒ transient.
2. `ERROR_ACCESS_DENIED` (5) — policy refusal vs AV holding the image → probe;
   ties go transient.
3. **Any workdir or binary on a UNC / network / removable path ⇒ presumed
   TRANSIENT regardless of errno.** Explicit override on top of the table.
4. `ERROR_DIRECTORY` (267) — the only unambiguous permanent signal (still subject
   to the UNC override).

**Standing rule — fail toward transient.** The cost is asymmetric: a permanent
error mis-read as transient costs ~4 min of backoff plus a parole cycle (today's
behavior — bad but self-limiting and visible). A transient error mis-read as
permanent **parks a recoverable daemon indefinitely** — worse than the defect
being fixed.

### State-machine shape — quarantine class attribute, NOT a new SMState

L1 **declines the mandate's literal "new terminal state"**, with evidence:
`supervise_respawn.go:87,227,231` allowlist `{StIdle, StBackoffWaiting,
StQuarantined}`. A new `StMisconfigured` is in none of them, so `mcphub daemon
recover` and `POST /api/daemon/respawn {force:true}` — **the documented clearing
levers** — would silently stop working for exactly the daemons that need them.
That is a self-inflicted "terminal state that can never clear". Persistence also
collapses everything-but-running to `idle`
(`supervisor_runtime_tracker.go:689-696`), needing a forward-compat story.

Recommended shape:
1. New sentinel `errSpawnPermanent` (alongside `supervise.go:92`/`:109`) wrapping
   the cause with `%w` + a stable `reason_id` (`invalid-workdir`, `missing-binary`,
   `not-executable`, `policy-refused`, `malformed-descriptor`).
2. Classify at `supervise.go:3671`; add `class` + `reason_id` to the existing
   `daemon-spawn-failed` body (`:3677`); return `errSpawnPermanent` instead of
   falling through to `:3768`.
3. Generalize `supervisor_controller.go:3538` from a hardcoded single
   `errors.Is(err, errSpawnJobProtectionRefused)` to an ordered sentinel set →
   one shared `quarantineAbsorbing(taskName, reasonID, message)` helper. Both
   existing absorbing sites (`:3394`, `:3538`) fold into it — **removes a live
   copy-paste pair rather than adding a third**.
4. Surface `Misconfigured` in `mcphub status --json` + GUI badge, derived from
   `state == quarantined && reason_id != ""`. Message shape follows the `"action"`
   precedent at `supervise.go:3622`.

**Clearing paths (terminal but not permanent):** cold restart (quarantine is
in-memory; a restarted supervisor re-probes and re-parks after exactly ONE attempt,
not 10); operator force (`mcphub daemon recover` — works *because* the state stays
`StQuarantined`); descriptor change (`EvIntentUpdate` is a legal
`StQuarantined → StSpawning` transition). No new clearing mechanism needed.

### Pre-flight validation — probe on the failure path, not the reconcile tick

Post-failure probe, not a reconcile-time gate:
- It resolves the ambiguity that actually matters (unreachable from a pre-flight check).
- **TOCTOU is irrelevant by construction** — the spawn already failed; we explain a
  fact, not predict one. Directory recreated between failure and probe ⇒ probe says
  present ⇒ transient ⇒ retry ⇒ correct outcome.
- Zero cost on the healthy path (a reconcile pre-flight pays one `stat` per daemon
  per tick forever — watcher 60s, liveness 5s, ×32 daemons).

**Probe contract:** `lstat` (a) `d.Workspace` if non-empty, (b) `d.Command`;
bounded ~2s timeout; returns `present | absent | indeterminate`. `indeterminate`
⇒ transient. Permanent requires errno-says-permanent **AND** probe-says-absent
(or, for `ACCESS_DENIED`, probe-says-present-and-readable).

### Mitigation stack for the dangerous direction (transient → permanent)

1. Default-transient; only the explicit allowlist is permanent.
2. Two-signal rule: errno **AND** agreeing probe.
3. UNC/network override — never permanent on a non-local path.
4. **Two-strike confirmation** — two consecutive identical permanent verdicts
   (same `reason_id`) before parking. Costs one extra spawn (~1s).
5. In-memory only — a false park cannot outlive the supervisor process.
6. Always visible (`error` severity + `reason_id`), always escapable
   (`mcphub daemon recover` still works).
7. Env kill-switch `MCPHUB_SPAWN_FAILURE_CLASSIFICATION=off`, mirroring
   `strictJobProtection`'s resolution (`supervise.go:3282`) — field regression is
   one env var from today's behavior, no rollback build.

**Accepted residual risk:** a genuinely permanent condition reported with an errno
outside the allowlist still burns the budget and parks in ordinary quarantine
(today's behavior). Widening the allowlist speculatively is how the dangerous mode
gets triggered. The `reason_id`-less quarantine rate is the metric that reveals a
missing code — owned by L4's surface.

### Acceptance tests (each RED at `d8ab4777`, GREEN after)

T1 invalid-workdir on the Job path is permanent · **T2 same verdict on the
`cmd.Start` fallback path — the two-path test a log-only classifier fails** ·
T3 permanent does not consume the retry budget · T4 permanent is never paroled ·
**T5 port-in-use (10048 AND 10013) stays transient — guards the dangerous mode** ·
T6 UNC workdir never permanent · T7 ambiguous errno without probe confirmation ⇒
transient · **T8 error chain survives the wrap (goes RED on a future `%w`→`%v`
regression)** · T9 cleared by cold restart · **T10 force-respawn still accepted —
the guard that catches the enum-value mistake if anyone later adds
`StMisconfigured`** · T11 two-strike confirmation.

### Open item carried forward

`ASSUMPTION (UNVERIFIED)`: no other consumer (GUI badge map, TS union, `mcphub
status` renderer) mishandles an added `reason_id`. Three decisive call sites
verified; exhaustive sweep in flight. **Settling probe:** the sweep plus
`go build ./... && go vet ./...` and `npm run typecheck`. Risk is bounded by
design — no enum value is added and an unknown `reason_id` degrades to the
existing `quarantined` label.

---

## L4 — crash-churn observability (gate: PASS)

### Governing finding

**Every metric the mandate needs already exists in the running supervisor and is
discarded before it reaches a human.** `RecordCrashAndCountInWindow` computes a
30-minute crash count, uses it for the quarantine life-or-death decision
(`supervisor_controller.go:3793`, threshold `:4366`), then throws it away.
`RestartCount` increments on every spawn
(`supervisor_runtime_tracker.go:328,349,392`) and is documented verbatim as
*"never persisted and never surfaced"* (`:574-578`).
`/api/health.restart_count` **already exists** and is hardcoded to 0 with
*"Future scheduler integration fills them"* (`health.go:486-488`).
This is a **projection gap, not a measurement gap** — which is why the top-ranked
items have near-zero blast radius.

### Metric (three quantities)

| Quantity | Window | Persisted | Answers |
|---|---|---|---|
| `crashes_in_window` | 30 min (`respawnFailureWindow`) | no | "crashing right now" |
| `crashes_24h` | 24 h, from a bounded ring | **yes** | "keeps crashing across restarts" |
| `pid_generation` | lifetime | already persisted | "has this ever been bad" |

**Thresholds derive from the existing ladder, never a literal:** healthy
`crashes_in_window == 0` (under a must-not-crash mandate, healthy is zero);
warn `1 ≤ n < respawnQuarantineThreshold/2`; critical
`n >= respawnQuarantineThreshold/2` — half the quarantine budget spent. Expressed
in code as `respawnQuarantineThreshold / 2` so retuning the ladder retunes the
badge. One owner, no drift.

**Restart survival is required and does not violate the in-memory rule.** The
in-memory justification (`supervisor_state.go:27-41`) is scoped to *decisions*.
The evidence proves the reset destroys the signal: 8 `supervisor-start` events in
one rotation window zeroed the 30-minute window 8 times, so recurrence could never
have been proven. Constraint: **the crash ledger is decision-inert** — no restart,
backoff, quarantine, or spawn path may read it. This preserves CLAUDE.md's threat
model (persisted fields must not be attacker-primable): worst case is a wrong
dashboard number, not a control-flow change.

Storage: bounded ring of the 16 most recent `{ts, exit_code}` per daemon in
`supervisor-state.json`, written on the existing `persistDaemonRuntimeTracker`
path. No new file, no new write path.

Surfaces follow the 4×-proven `job_protection` seam (producer → IPC wire struct →
`DaemonStatus` → `types.ts` → `DaemonMetrics` badge). The IPC wire struct
(`supervisor_ipc_status_client.go:144`) is **required** — without the field
`json.Unmarshal` silently drops it, a trap documented in that struct's own
comments (`:157-163`).

### Test-leak isolation

Two channels. **Channel A (in-process)** — `daemonStateRootOverride`
(`state_paths.go:102`) is opt-in per test and **fails open**; a test that forgets
it writes to the operator's real log. Already filed and closed as
`work-items/bugs/closed/2026-05-20-tests-leak-state-into-production-logs.md`,
"fixed" by adding *more opt-in call sites* — the recurrence proves opt-in does not
hold. **Fix: invert to fail-closed** in the production resolver — if
`testing.Testing()` and no explicit override, return a sentinel naming the
isolation helper. Inert in the shipped binary; works from any package's test
binary (an `init()` in package `api` would only cover that package's own test
binary, missing `internal/cli` and `internal/gui` — the two that actually leaked).

**Channel B (subprocess) cannot be closed by that guard** and must not be claimed
closed: `MCPHUB_STATE_DIR_OVERRIDE` is honored only under `-tags=test_state_path_env`
(`state_paths_envfallback.go:73`), deliberately excluded from production
(`state_paths.go:16-24`). Any harness spawning a production-built binary leaks by
construction. Closing it requires compiling a loud, auditable override into
production, weakening a documented compile-time guarantee → **security decision,
routes to `$security-reviewer`**, not adopted here.

### Log-noise budget

1656 `state-file-write-unhardened-fallback` rows in one window. The event fires
per write, but the posture it documents is a property of the
`(parent_dir, reason)` pair — constant across thousands of writes.

**Severity demotion REJECTED** — `state_file_helper_test.go:279` asserts
`severity == warn` with the rationale "security-boundary downgrade must be
dashboard-visible". Breaking a named regression guard on a security surface.

**Adopted: dedup with count rollup** at the single emit owner
(`state_file_helper.go:227-270`). First observation per key emits the full row
**byte-identical to today**; subsequent increment an in-memory counter; a new
`…-rollup` event at `warn`, at most once per 15 min per key and once at process
exit, carries `suppressed_count`, `first_ts`, `last_ts`. Information preserved is
**strictly more** than today. Envelope untouched (new event *name*, not a new
identity field) — the 16 KB cap and `ErrIdentityOversize` fail-closed semantics in
`intent_audit.go` are unmodified. 1656 → ~2 rows.

### Anti-masking rule

> Every automatic recovery action increments a **persisted** counter, and the
> health projection treats recent recovery activity as **not-healthy**. A recovery
> that succeeded is still a defect that must be visible.

Recovery already emits at `info` (`liveness-relaunched-owner`,
`liveness-relaunched-supervisor-under-gui`, `supervise_ensure_alive.go:715-742`) —
nothing counts it, nothing shows it, and at `info` it is buried under the 1656
`warn` rows above.

Named surface: an **always-rendered** Dashboard "Stability" card
(`supervisor_starts_24h`, `liveness_relaunches_24h`, `daemon_respawns_24h`,
`quarantines_24h`). Always-rendered is load-bearing — a card that appears only when
things are bad is indistinguishable from a broken card; "0 recoveries in 24 h" must
be a positive assertion.

**Tray is the strongest lever:** currently green/red on failure rows
(`gui_tray_state.go:150-181`), so a constantly-recovering fleet reads **green** —
precisely the mandate violation. Add an amber "degraded" state driven by non-zero
recent recovery counts.

**Deferred:** projecting a churning daemon as `flapping` instead of `running`
(`daemon_state.go:150-235`) — a new enum on a shared 3-projection classifier
requiring an exhaustive Go+TS consumer sweep plus `go generate`. Ranked last
deliberately; do not bundle.

### Ranking — first cut N1 + N2 + N3

| # | Change | Blast radius |
|---|---|---|
| **N1** | DACL fallback dedup + rollup | 1 function; no envelope, no wire change |
| **N2** | Surface `crashes_in_window` + `pid_generation` | additive optional fields on a 4×-proven seam |
| **N3** | `testing.Testing()` fail-closed state-dir guard | 1 function, inert in production |
| N4 | Persisted crash ledger (`crashes_24h`) | new key in existing file, existing write path |
| N5 | Recovery ledger + Stability card + amber tray | new API field group + GUI component + tray state |
| N6 | `flapping` health state | shared classifier + exhaustive Go/TS sweep — separate phase |
| N7 | Production state-dir override (channel B) | weakens a documented guarantee — `$security-reviewer` gate, not adopted |

### Decision records to file (`status: proposed`)

- `2026-07-20-crash-observability-ledger-decision-inert.md`
- `2026-07-20-production-state-dir-override.md` → `$security-reviewer`

### Adjacent finding

`work-items/bugs/closed/2026-05-20-tests-leak-state-into-production-logs.md` is
**closed but the defect recurs**; its own "Out of scope: other tests that may also
leak (no audit done)" caveat is exactly what fired. Reopen or file a successor —
do not silently re-close.

---

## Lead notes

- L1's §1a two-path errno divergence is the finding most likely to be lost in
  implementation. It must survive into the plan verbatim.
- L1 declines the literal "new terminal state" from `status.md`'s non-negotiables
  in favor of a class attribute, with blast-radius evidence. **Lead accepts the
  substitution** — the non-negotiable was "an honest terminal state with a defined
  clearing condition", and the class-attribute shape satisfies it while keeping the
  clearing levers working. If the literal enum is ever wanted, T10 must be written
  BEFORE, not after.
- Cross-family (codex/Sol) lane: **FAILED, 5 attempts** — codex sandbox policy
  rejects piped read commands and the models cache is corrupt
  (`missing field supports_reasoning_summaries`, regenerates identically; codex
  v0.144.6 vs server format). Recorded as an UNCOVERED lane, not a PASS. Two fable
  lanes were dispatched to cover the gap (Windows error taxonomy + boot-order
  hazard; adversarial refutation of the root cause).
