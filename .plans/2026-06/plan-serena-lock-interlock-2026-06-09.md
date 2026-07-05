# Plan: serena migrate supervisor-lock interlock (rev 2 — 3-reviewer fold-in)

Date: 2026-06-09. Status: AWAITING APPROVAL. Windows-only (migrate is Windows-GA).
Source: design subagent (read-only investigation, all file:line verified). Revised
after a 3-reviewer loop (PRECISE confirmed all 3 cruxes; STRATEGIC confirmed the
fix shape; 5 revisions folded in below).

## Loop verdict (carried forward — the APPROACH stands, do NOT re-litigate)
- **Crux 1 — gate bypass is NECESSARY.** Windows byte-range locks are per-HANDLE
  (gofrs/flock + MS LockFileEx): the §7.1 gate's `SupervisorRunningUnderStateDir`
  opens a SECOND flock handle and genuinely misreads the migrate's OWN held lock
  as a foreign supervisor (ERROR_LOCK_VIOLATION → "running"). So the gate must be
  told "the caller holds the lock; skip the probe." CONFIRMED correct.
- **Crux 2 — the hand-off window is benign, INCLUDING the old-binary case.** When
  the migrate releases `supervisor.lock` immediately before spawning the successor,
  any racing acquirer is the NEW binary (reads runtime_spec fine). Even an OLD
  supervisor that won the release→acquire race fails LOUD at cold start (its
  `DisallowUnknownFields` decoder rejects runtime_spec → process exits → releases
  the lock → the migrate's new-binary start then acquires cleanly), and NOTHING
  re-spawns on that crash (the GUI exit-monitor only logs — gui_supervisor_owner.go
  startExitMonitor). No split-brain. CONFIRMED.
- **Crux 3 — path-consistency mitigation is sufficient.** Hard-code
  `api.DaemonStateDir()` (the gate's exact resolver) for the interlock leaf so the
  held-lock path == the gate-probed path. CONFIRMED.

## Problem
`mcphub migrate serena legacy-to-dynamic-pool` reaps the supervisor (step 7,
migrate_serena.go:697), then writes the new spec-bearing intent (step 8, :846). In
the reap→write window a foreign supervisor can start (GUI `ensureSupervisorRunning`
gui_supervisor_owner.go:88, schedulerless `registerEnsureSupervisorRunning`
register.go:113, or `POST /api/supervisor/restart`). InstallParsedManifest's §7.1
gate (install_parsed_manifest.go:315-329) refuses the spec-bearing write while a
supervisor runs (its `SupervisorRunningUnderStateDir` probe, supervisor_lock.go:205,
reports "running") → migrate fails mid-flight + recovery jams on the IPC pipe
(DialPipe not-found, 30s timeout, because the fresh supervisor holds the lock but
hasn't bound its pipe yet).

### Terminology — "old binary" means OLD DECODER-VINTAGE, not binary identity
The reap-path is a SAME-binary cutover (migrate_serena.go:38 — RunInstallUpgrade's
rename-aside is skipped precisely because the binary is unchanged). The hazard the
§7.1 gate guards is NOT binary identity; it is the **decoder vintage of the
currently-RUNNING supervisor process** — a supervisor that BOOTED before
`runtime_spec` existed in its compiled `ReadSupervisorIntent` decoder
(supervisor_intent.go:107 `DisallowUnknownFields`) will reject the new field and
keep its stale snapshot. After a binary upgrade the on-disk binary is new, but a
supervisor process launched from the PRIOR image is still running the old decoder
until it is restarted. The gate's necessity is "no process running an old decoder
may observe the new intent" — restart, not re-link, is what clears it.

## Fix: supervisor-lock interlock (lock-as-mutex over reap→write→start)
The migrate HOLDS `supervisor.lock` across reap→write→start. While held, no other
actor can ACQUIRE it, so no other actor can START a supervisor (every
supervisor-starter calls `api.AcquireSupervisorLock`, supervise.go:373, and fails
fast on a held lock → its `runSupervise` returns error → the spawned child exits).
STRATEGIC review: this is cohesive with the EXISTING singleton authority — the lock
already IS the single-supervisor mutex; the migrate borrows it as the critical-
section mutex it already is, rather than inventing a parallel coordination channel.

### Two crux mechanisms
1. **Gate "held by me" (Crux 1) — via a TYPED token, not a bool (Revision 1).**
   The §7.1 gate must skip its `SupervisorRunningUnderStateDir` probe when the
   caller already holds `supervisor.lock` (the per-handle quirk above). The signal
   is a TYPED bypass token mintable ONLY by `api.AcquireSupervisorLock` — see
   "Revision 1" below for the type design. The held lock is a STRONGER guarantee
   than the point-in-time probe (it provably excludes every foreign starter for the
   whole critical section, where the probe is one instant) → §7.1 is satisfied by
   construction. NOT a self-PID check (racy; the sidecar is best-effort,
   supervisor_lock.go:29-32).
2. **Hand-off (Crux 2):** the migrate releases the lock immediately before spawning
   the successor (the child must `AcquireSupervisorLock` itself; an inherited locked
   region is NOT granted to a detached child on Windows). The residual
   release→child-acquire window is benign per Crux 2 above: the intent is already
   committed (the §7.1 WRITE-blocking property is past), the singleton makes a
   duplicate spawn exit (supervise.go:373), an OLD-decoder winner self-crashes and
   nothing re-spawns it, and `waitReconcileReadyViaIPC` retries 200ms×30s
   (migrate_serena_restart_windows.go:181) so the pre-bind pipe race is tolerated.

Crash safety: the OS drops the flock on crash; the on-disk intent is atomic
temp+rename (legacy if crash-before-write) → the next supervisor restores legacy. No
split-brain. The new failure mode (a foreign supervisor already running at acquire)
is the CORRECT loud-and-retryable behavior, not a bug.

---

## Revision 1 (HIGHEST VALUE) — typed bypass token, not a bool
A `bool SupervisorLockHeldByCaller` is a zero-cost escape hatch: a future caller can
set it `true` WITHOUT holding the lock, silently re-arming the split-brain the
fail-CLOSED §7.1 gate exists to prevent. Convert the comment-enforced invariant into
a **constructor-enforced** one — make the bypass key impossible to manufacture
without having acquired the lock.

**Design — the lock handle IS the token (identity check, not a flag).**
- Add an UNEXPORTED field to `InstallParsedManifestOpts`:
  `supervisorLockBypass *SupervisorLock`. Because it is unexported, NO code outside
  the `api` package can set it; cross-package callers (the cli migrate, auto-register)
  must go through a constructor.
- Add an exported constructor method on the live lock:
  `func (l *SupervisorLock) AllowSpecBearingWriteBypass() InstallParsedManifestBypass`
  returning a tiny opaque exported wrapper type
  `type InstallParsedManifestBypass struct{ lk *SupervisorLock }` whose only
  consumer is `InstallParsedManifest`. The wrapper cannot be constructed with a nil
  or forged `lk` outside `api` (its field is unexported). A caller holding a real
  `*SupervisorLock` from `AcquireSupervisorLock` is the ONLY way to obtain one.
- `InstallParsedManifestOpts` gains an EXPORTED field
  `SupervisorLockBypass InstallParsedManifestBypass` (zero value = no bypass).
- The §7.1 gate verifies IDENTITY, not truthiness: it skips the probe ONLY when
  `opts.SupervisorLockBypass.lk != nil` AND that lock's `path` resolves to the gate's
  own `filepath.Join(stateDir, "supervisor.lock")` AND the lock is still held
  (`opts.SupervisorLockBypass.lk.fl != nil` — Release() nils `fl`,
  supervisor_lock.go:130). A bypass whose lock leaf does NOT match the gate's
  stateDir is REJECTED (treated as no-bypass → the probe runs → fail-closed), which
  also catches the path-mismatch class (Crux 3 / Key risk) at the gate itself rather
  than only at the call site.
- Emit the info event `spec-bearing-write-allowed-under-caller-lock` (carry the
  matched lock path) only on a verified bypass.

Net: a caller that does not hold the matching lock CANNOT obtain a non-nil
`SupervisorLockBypass`, so it cannot bypass the gate; the invariant is enforced by
the type system + an identity check, not by a code comment.

(Implementation note: keep the field unexported-pointer-wrapped so `go vet`'s
copylocks does not fire — `SupervisorLock` holds a `*flock.Flock`, not a value lock,
so an opts copy is safe, but the wrapper keeps the intent legible.)

---

## Revision 2 (BIGGEST GAP) — the two uncovered supervisor-starters: DECISION

The original "every supervisor-starter contends on `supervisor.lock`, so the
interlock covers them all" claim is TRUE for the three starters in the Problem
section (they each START a supervisor via `AcquireSupervisorLock`). But it is
INCOMPLETE for two flows that REAP (kill) a supervisor before starting, because
**the held lock prevents ACQUIRE, not KILL** — a concurrent reaper can still
force-kill a PID.

### DECISION SUMMARY (plain language — the crux of this revision)
- **Starter A — `AutoRegisterSerenaWorkspace` (serena_auto_register.go:84): EXTEND
  the interlock to it. This is a REAL cross-process kill hazard, not a benign
  fail-clean.** Its INTRODUCE branch reaps the supervisor by PID
  (`autoRegisterReapFn` → `ReapSupervisorForRestart` → `ForceKillSupervisor`,
  install_migration_wiring_windows.go:958-980) WITHOUT first acquiring
  `supervisor.lock`. The PID it force-kills comes from
  `supervisor.lock.owner.json`. But once the MIGRATE acquires the interlock, it
  OVERWRITES that sidecar with its OWN CLI-process PID (supervisor_lock.go:80-88,
  `os.Getpid()`). So a concurrent auto-register INTRODUCE that fires inside the
  migrate's held-lock window reads the sidecar, sees the migrate's PID, and
  `taskkill /F /T`s the MIGRATE PROCESS mid-cutover. FIX: make auto-register's
  INTRODUCE cutover take `supervisor.lock` via the same interlock seam the migrate
  uses, so the two reaping flows are mutually exclusive and neither can kill the
  other's lock-holding process. (SUPERSEDED ORDER — Revision 7: the acquire happens
  IMMEDIATELY AFTER the reap, not before it. The running supervisor holds the lock, so
  a pre-reap acquire could never succeed on the introduce-while-running case; the reap
  frees the lock, the post-reap acquire takes it. The quiet acquire keeps the kill-race
  closed under either order.)
- **Starter B — `v5UpgradeDeps.StartSupervisor` from `RunInstallUpgrade` /
  `mcphub install --upgrade` (install_upgrade.go:415, wired
  install_migration_wiring_windows.go:333): DOCUMENT-AND-ACCEPT — it fails CLEANLY,
  with one improvement.** On a v0.5.x host (supervisor-intent.json present — exactly
  the migrate-target host), `install --upgrade` routes to `runV5UpgradeWindows`
  (install.go:1357 → :1390 → wiring :280) which does **NOT** take `migration.lock`
  (verified: the function body :280-345 calls `RunInstallUpgrade` directly with no
  lock). So `migration.lock` is NOT a usable broader-exclusion here. But the upgrade
  ALSO reaps-then-starts, and its reap (`RunInstallUpgrade` step 4a force-kill,
  install_upgrade.go:362) reads the SAME owner sidecar — so in principle it has the
  same "kill the migrate PID" shape as Starter A. The DECISION differs because the
  upgrade is an OPERATOR-INITIATED foreground command, not an automatic inbound-
  tool-call trigger: running `mcphub install --upgrade` AT THE SAME TIME as
  `mcphub migrate serena legacy-to-dynamic-pool` is an operator self-collision the
  product does not currently defend (the two share no lock today). Folding the
  upgrade into the serena interlock seam is OUT OF SCOPE (it would require threading
  the interlock through the whole `RunInstallUpgrade` contract, which serves every
  v0.5.x upgrade, not just serena). Instead: (a) document the collision in CLAUDE.md
  + the spec as a known operator-sequencing constraint, and (b) confirm the migrate
  side fails LOUD (not corrupt) if the upgrade wins: if the upgrade force-kills the
  migrate, the migrate process dies — its intent write is atomic temp+rename so a
  death before the rename leaves legacy intact (recoverable); a death after the
  rename leaves the committed dynamic-pool intent which the upgrade's freshly-started
  new-binary supervisor reconciles correctly. Either way: no split-brain, no silent
  corruption. The residual is a failed/odd-looking migrate run the operator re-runs —
  acceptable for a self-inflicted concurrent-command collision, and made visible by
  the Revision-4 hand-off event + a documented constraint.

### Precise timing trace (Starter A — why EXTEND is required), all file:line verified
1. Migrate reaps the REAL old supervisor (step 7, migrate_serena.go:699 →
   ReapSupervisorForRestart): reads `supervisor.lock.owner.json` → OLD supervisor
   PID → force-kills it. Correct (sidecar still names the old supervisor here).
2. Migrate ACQUIRES the interlock (Phase-2 seam, after the reap) →
   `AcquireSupervisorLock` overwrites `supervisor.lock.owner.json` with the MIGRATE
   CLI PID (supervisor_lock.go:82).
3. Concurrently, an inbound `/serena/mcp` tool-call for an UNREGISTERED workspace
   hits the GUI router → `AutoRegisterSerenaWorkspace` (serena_auto_register.go:84).
   On a pre-cutover host `priorHasSpec=false`; its liveness probe
   (`autoRegisterSupervisorRunningFn`, line 263 = `SupervisorRunningUnderStateDir`)
   sees the lock HELD → `supRunning=true` → `needReap=true` (line 268).
4. Auto-register calls `autoRegisterReapFn(commitCtx)` (line 341) →
   `ReapSupervisorForRestart` → IPC quiesce/exit fail (no supervisor pipe; the
   migrate holds the lock but is a CLI process, not a supervisor) → force-kill
   fallback reads `supervisor.lock.owner.json` → **migrate's PID** → `taskkill /F /T
   /PID <migrate-pid>` KILLS THE MIGRATE mid-cutover.
5. Without the fix: the migrate dies between reap and write (legacy intact but no
   supervisor running; the migrate's own recovery never runs because the process is
   gone) OR between write and start (committed intent, no supervisor) — and the
   auto-register, having "reaped" nothing real, proceeds to commit ITS intent and
   start a supervisor over a half-migrated registry. This is the split-brain the
   whole effort prevents, re-entering through the back door.

### The fix for Starter A — same interlock seam, acquired after the auto-register reap

> SUPERSEDED ORDER (Revision 7): this section originally specified acquire-BEFORE-reap.
> Bot PR #276 r2 P1 corrected it to acquire-IMMEDIATELY-AFTER-reap — the running
> supervisor holds `supervisor.lock`, so a pre-reap acquire could never succeed on the
> introduce-while-running case (it would defer every such cutover with a 503). The reap
> frees the lock; the post-reap acquire takes it. Read this section's acquire-FAIL
> handling as the POST-reap defer (the reap ran; `failPreCommit` drives the recovery
> restart for our own reap) — the kill-race + mutual-exclusion guarantees below all
> still hold, just on the FREED post-reap lock. The quiet acquire keeps the sidecar
> kill-race closed under either order. See Revision 7 for the corrected design.
- Auto-register's INTRODUCE branch acquires `supervisor.lock` BEFORE its reap
  (serena_auto_register.go:340), via the SAME production seam the migrate uses
  (`defaultAcquireSupervisorInterlock`, Phase 2), and releases it immediately before
  its start (line 410-411) — identical lifetime to the migrate's. Then:
  - If the MIGRATE holds the lock: auto-register's acquire FAILS → it does NOT reap
    (so it cannot kill the migrate). It fails PRE-COMMIT (before its own reap, so the
    old supervisor — already reaped by the migrate — is not double-killed; nothing of
    auto-register's is committed), rolls back its registry row (the existing
    `rollback()` path), and returns an error the router maps to 503 → the client
    retries, by which time the migrate has finished and the workspace is either a
    live-add or a fresh register. Crucially the error must be the HONEST
    "another migrate/cutover holds supervisor.lock; retry shortly" — NOT a misleading
    "supervisor running" (there is none; a CLI holds the lock). This addresses the
    reviewer's "misleading 'supervisor running' error + false-positive audit event"
    concern: the audit event for this branch is a distinct
    `serena-auto-register-deferred-on-interlock` info event, NOT
    `spec-bearing-install-refused`.
  - Symmetrically, the MIGRATE's acquire fails if AUTO-REGISTER holds the lock →
    the migrate's existing acquire-FAIL fail-loud branch fires (legacy untouched,
    intent not written; see §7.1.1) → operator re-runs.
- The §7.1 gate inside auto-register's own `InstallParsedManifest` call
  (serena_auto_register.go:352) MUST then receive the bypass token (Revision 1) so
  it does not misread auto-register's OWN held lock. Today auto-register relies on
  the reap clearing the gate (no supervisor running at the write); with the interlock
  held the probe would see auto-register's own lock → must bypass via the token.
- LIVE-ADD branch (priorHasSpec=true, no reap): auto-register does NOT reap and does
  NOT need the interlock for kill-safety, but its `InstallParsedManifest` write runs
  while a NEW supervisor is up. That path is already §7.1-safe today
  (`priorIntent.HasRuntimeSpecRow()` ⟹ gate allows, install_parsed_manifest.go:315).
  It does NOT acquire the interlock and is unchanged — BUT note a live-add running
  concurrently with a migrate is itself bounded by the existing
  `serenaAutoRegisterInstallMu` only WITHIN the GUI process; cross-process it relies
  on the registry flock (held continuously through commit, serena_auto_register.go:217)
  which the migrate also takes (migrate_serena.go:486 / re-read :731). The registry
  flock already serializes the two writers' intent fan-out. No interlock change needed
  for live-add.

### Why NOT a broader `migration.lock` exclusion (rejected, with evidence)
`migration.lock` is a DIFFERENT leaf (`<stateDir>/migration.lock`, journal_lock.go:53)
and the migrate driver never takes it (verified: runMigrateSerenaDynamicPool acquires
the registry lock + the new supervisor.lock interlock, never migration.lock). The
v5-upgrade path that is the actual concurrency partner ALSO never takes it
(runV5UpgradeWindows :280-345). Routing both onto migration.lock would mean adding it
to (a) the serena migrate, (b) the auto-register cutover, AND (c) the generic
`RunInstallUpgrade` — a contract that serves every v0.5.x upgrade, far beyond serena.
`supervisor.lock` is the correct serializer because it is the ONE lock every
supervisor-START already contends on; extending the few REAP flows to take it too is
the minimal change that closes the kill-race without inventing a new lock or widening
an unrelated contract.

---

## Revision 3 — deferred release must arm ON ACQUIRE, plus a recovery-branch test
The migrate has a recovery-start at migrate_serena.go:446 (the
`alreadyMigrated && !drift && !healthy` branch) that fires BEFORE the main reap/
acquire. If the interlock release closure were armed at function entry (or as a bare
`defer` over a nil handle), that early recovery-start path would attempt to release a
NEVER-ACQUIRED lock (nil deref or a spurious unlock).
- DESIGN: the release closure is armed ONLY at the moment of a successful acquire.
  Mirror `releaseRegistryLock` (migrate_serena.go:497-503): a captured
  `interlockHeld bool` + a closure that no-ops when the handle is nil/released. The
  acquire site sets the handle and arms; every release site (including the early
  recovery-start at :446, which never acquired → no-op) calls the SAME idempotent
  closure. The early recovery branch at :446 returns through the normal path, so it
  must call the release no-op too (harmless) to keep one exit discipline.
- TEST (Phase 2): `Interlock_ReleasedOnRecoveryStartBranch` — drive the
  `alreadyMigrated && !drift && !healthy` recovery path (seam
  `migrateSerenaSupervisorHealthyFn` → false; `migrateSerenaStartFn` stub observes
  the lock is ACQUIRABLE during its call AND remains released after the function
  returns). A leaked interlock would deadlock the NEXT migrate — invisible to a
  single-run test, so the assertion is "a SECOND `AcquireSupervisorLock` in-test
  after the function returns SUCCEEDS." (Note: the early recovery branch at :446 does
  NOT itself acquire the interlock in the current design — the acquire is post-step-7.
  The test's value is the regression guard that NO path releases-without-acquire or
  leaks-without-release as the seam wiring evolves.)

---

## Revision 4 — observe the benign hand-off window
After the prior production trust-burn (the original bug looked like a bare 30s IPC
timeout), an operator must be able to tell a KNOWN-BENIGN hand-off window apart from
a recurrence. Emit a named `supervisor-events.log` event whenever the
release→child-acquire window actually exercises its tolerance:
- `EVENT supervisor-interlock-handoff-window` (severity info, source `migration`),
  body `{phase: "duplicate-spawn-exit" | "reconcile-ready-retry", task_name, note:
  "known-benign hand-off window: the migrate released supervisor.lock before
  starting the successor; a racing duplicate exits via the singleton, an old-decoder
  winner self-crashes, and reconcile-ready retries cover the pre-bind pipe race"}`.
- Fired from: (a) `defaultMigrateSerenaStart` when `waitReconcileReadyViaIPC` takes
  >1 retry to succeed (the pre-bind race materialized but resolved —
  migrate_serena_restart_windows.go:181); and (b) — best-effort — when the start
  primitive observes a duplicate-spawn singleton exit. The event is observability,
  never a gate; emit-failure is silently non-fatal (mirror
  emitWorkspaceAutoRegisteredEvent, serena_auto_register.go:609).

---

## Revision 5 — terminology + citation fixes (folded into the plan text above)
- "old binary" → "old DECODER-VINTAGE of the running process" — see the
  "Terminology" subsection under Problem (one paragraph, keeps the gate's necessity
  legible against the same-binary reap at migrate_serena.go:38).
- §7.1.1 scope (below): the interlock covers the reap→WRITE window; the tiny
  reap-complete→acquire gap is covered by the acquire-FAIL fail-loud branch (intent
  not yet committed, legacy untouched).
- CITATION FIX: the supervisor singleton-exit point is **supervise.go:373**
  (`AcquireSupervisorLock` failing on a held lock → `runSupervise` returns error →
  process exits → flock released), NOT migrate_serena_restart_windows.go:123 (that
  line is `defaultMigrateSerenaSupervisorHealthy`, an unrelated health probe). Every
  "duplicate spawn exits" reference in this plan now cites supervise.go:373.
- TEST-HARNESS FIX: the Phase-2 concurrent-acquirer test MUST call
  `api.AcquireSupervisorLock(filepath.Join(api.DaemonStateDir(), "supervisor.lock"))`
  DIRECTLY to simulate the contending supervisor — NOT spawn a real `mcphub
  supervise` child. A child under `MCPHUB_STATE_DIR_OVERRIDE` resolves a DIFFERENT
  lock leaf (its own `DaemonStateDir`) → the test would assert against a lock the
  migrate never contends → vacuous. Calling `AcquireSupervisorLock` on the gate's
  exact path in-process is the only faithful contender.

---

## Revision 6 (bot PR #276 fold-in) — QUIET acquire + acquire-immediately-after-reap

The first implementation (commits b72e4b3/51b2968/31c7369) used the FULL
`api.AcquireSupervisorLock` for the interlock and deferred the migrate's acquire to
the final write boundary. The Codex bot found two P1 defects in that shape:

- **Finding 1 — sidecar corruption before the reap.** `AcquireSupervisorLock` writes
  `supervisor.lock.owner.json` with `os.Getpid()` (supervisor_lock.go:80-88). The
  auto-register INTRODUCE acquire fires BEFORE its reap (serena_auto_register.go:311,
  reap at :388); the reap primitive reads that sidecar to choose the PID it
  taskkills / IPC-handshakes (`ForceKillSupervisor` / `QuiesceTimers` /
  `ExitGraceful`, install_migration_wiring_windows.go:949/954/968). So the acquire
  overwrote the sidecar with the GUI/router PID and the reap then targeted the
  CALLER, not the old supervisor — the first cutover could fail OR kill itself.
- **Finding 2 — acquire too late, gap stays open.** The migrate reaped at step 7
  (migrate_serena.go:735) but did not acquire until step 7e
  (migrate_serena.go:915), AFTER the registry re-read + start-supported re-check +
  late-reap decision. A foreign supervisor could take `supervisor.lock` in that
  unlocked post-reap gap → the original reap→write race re-opened.

**Fix (verified against the actual reap + gate code):**

- **Quiet acquire.** Add `api.AcquireSupervisorLockQuiet(path)` (supervisor_lock.go)
  — flock only, NO owner-sidecar write; a `quiet` field makes `Release()` skip the
  sidecar removal. The interlock's job is the FLOCK (mutual exclusion); the SIDECAR
  is what the reap reads, so the interlock must take the flock without touching the
  sidecar. The quiet handle still has `.fl` + `.path` set, so it mints a valid §7.1
  bypass token (the gate identity check at install_parsed_manifest.go:343 verifies
  only `lk.fl != nil` + `lk.path == gateLockPath`, never the sidecar). Both
  `defaultAcquireSupervisorInterlock` (migrate_serena_restart_windows.go) and the
  auto-register seam now use the quiet acquire.
- **Migrate acquires immediately after each reap.** A single `acquireInterlockOnce`
  helper takes the lock the instant the step-7 reap and the step-7d late reap
  complete (and at the no-reap spec-bearing-write boundary for the
  no-supervisor-running case), closing the post-reap gap. Acquire-FAIL stays
  fail-loud + willReap recovery-start; release before every start.
- **Auto-register (finding 1)** is fixed by the quiet wiring on the sidecar axis —
  but its acquire ORDER (acquire-before-reap) was still wrong; see Revision 7.

**Tests added:**
- `TestAcquireSupervisorLockQuiet_PreservesOwnerSidecar` (api) — the primitive must
  not write/remove the sidecar; flock still excludes; token still mints.
- `TestAutoRegisterSerena_Introduce_ReapTargetsOldSupervisorNotCaller` (api) — seed
  an OLD-supervisor PID; assert the reap reads it (NOT `os.Getpid()`) while the
  interlock is held. Falsified: FAILS with the full acquire.
- `TestMigrateSerena_Interlock_AcquiredImmediatelyAfterReap_ClosesPostReapGap` (cli)
  — a concurrent `AcquireSupervisorLock` injected in the post-reap work window now
  FAILS. Falsified: FAILS when the immediate acquire is disabled.
The existing 8 interlock tests still pass (their `realInterlockAcquire` /
`realAutoRegisterInterlockAcquire` stubs switched to the quiet acquire to match
production).

---

## Revision 7 (bot PR #276 r2 P1 fold-in) — auto-register must REAP THEN ACQUIRE

The Revision-6 quiet-acquire fix closed the sidecar-overwrite axis but left the
auto-register acquire ORDER wrong. The Codex bot's round-2 P1 (serena_auto_register.go,
the pre-reap acquire site) found that the INTRODUCE-while-running auto-register can
NEVER make progress:

- **Root.** The RUNNING supervisor holds `supervisor.lock` — it took the flock in
  `runSupervise` on its startup (`api.AcquireSupervisorLock`, supervise.go:373). The
  quiet acquire `AcquireSupervisorLockQuiet` (supervisor_lock.go:132-143) does a
  `lk.TryLock()` on that SAME `supervisor.lock.lock` byte-range. So a PRE-reap acquire
  on the exact introduce-while-running case (`needReap` — a supervisor is running)
  ALWAYS fails: the live supervisor holds the lock. The cutover then treats that as
  "another cutover holds the lock" → defers → rolls back → 503 → it NEVER reaps or
  installs. The quiet acquire fixed the sidecar but cannot bypass the running
  supervisor's flock contention.
- **Why the migrate was already correct.** Only the REAP frees the lock (it kills the
  holder → the OS drops the byte-range lock). The migrate already does
  reap-THEN-acquire-immediately (`acquireInterlockOnce` right after each reap,
  migrate_serena.go:781 / :937 / :994). The auto-register did acquire-THEN-reap — the
  inverted order the bot flagged.

**Fix (serena_auto_register.go):** move the INTRODUCE interlock acquire to IMMEDIATELY
AFTER `autoRegisterReapFn` (reap → quiet-acquire-the-FREED-lock → install → start),
mirroring the migrate. KEEP the quiet acquire (so during the held-lock window the
sidecar still names the dead old supervisor → a concurrent reaper reads a dead PID →
no-op → never force-kills THIS live process — finding-1 property holds, now doubly: the
reap also read the intact sidecar BEFORE any lock touch). On a POST-reap acquire FAIL
(another cutover reaped the same supervisor and won the freed lock first): emit the
distinct `serena-auto-register-deferred-on-interlock` event, drive `failPreCommit`
(recovery-restart for our OWN reap + registry rollback), return the honest 503 error.

**Concurrency re-verification (with both migrate and auto-register on
reap-then-quiet-acquire-immediately), file:line verified:**
1. **Double-reap is a benign no-op.** Both may reap the same supervisor concurrently.
   `ReapSupervisorForRestart` (install_upgrade.go:515) on an already-dead supervisor:
   QuiesceTimers/ExitGraceful IPC fail (no pipe) → force-kill fallback →
   `ForceKillSupervisor` (install_migration_wiring_windows.go:958) →
   `killPIDViaTaskkill` `taskkill /F /T /PID <dead>` exit 128 → `isAlreadyExitedError`
   (install_upgrade.go:595, exit-code 128 on Windows) returns true → benign → port
   verify passes. The loser's 2nd reap does NOT abort the cutover.
2. **Exclusion is on the FREED post-reap lock.** After the reap both race for the now-
   free `supervisor.lock`; one wins the quiet `TryLock`, the other's fails → defers.
   Migrate fail-loud (migrate_serena.go:781 acquire-FAIL branch); auto-register rolls
   back + 503 (serena_auto_register.go post-reap acquire-FAIL branch).
3. **Kill-race prevented.** The quiet acquire never writes
   `supervisor.lock.owner.json` (supervisor_lock.go:181-188 `Release` skips it for a
   quiet handle; the acquire at :132-143 never writes it). So during the winner's
   held-lock window the sidecar still names the DEAD old supervisor; a concurrent
   reaper reads that dead PID and `taskkill` no-ops (exit 128). The winner's live CLI
   PID is NEVER in the sidecar.
4. **No orphaned-down supervisor.** If auto-register reaps then LOSES + defers, the
   race WINNER (migrate or the other auto-register) holds the freed lock and restarts a
   supervisor from its committed intent. The loser's `failPreCommit` recovery start
   fails fast while the winner holds the lock (correct). The ONLY hole — surfaced, not
   papered over — is the shared "BOTH reaping flows fail AFTER a reap" case (the winner
   ALSO fails post-reap): that is a fail-loud 503 + `mcphub supervise` operator
   guidance, identical to the migrate's own post-reap-write-fail recovery semantics,
   never a silent no-supervisor half-state.

**Tests added/changed (api):**
- `TestAutoRegisterSerena_Introduce_AcquiresInterlockAfterReap` (was
  `...AcquiresInterlockBeforeReap`) — asserts order reap→acquire→install→start.
  Falsified: FAILS on the pre-reap order.
- `TestAutoRegisterSerena_Introduce_WhileSupervisorHoldsLock_ReapsThenAcquires_Succeeds`
  — NEW. A REAL `supervisor.lock` is held before the call (the live supervisor); the
  reap stub releases it; the introduce SUCCEEDS (reaps → acquires the freed lock →
  installs). This is the exact case the bot says currently 503s. Falsified: FAILS on
  the unfixed pre-reap order (held lock blocks the pre-reap acquire → defer → 503).
- `TestAutoRegisterSerena_Introduce_DefersWhenInterlockHeldAfterReap_RecoversAndRollsBack`
  (was `...DefersWhenInterlockHeld_NoReap_NoKill`) — a concurrent holder of the
  post-reap lock makes the cutover defer AFTER its reap: reap ran, recovery-restart
  ran, distinct event emitted, row rolled back. Falsified: FAILS on the pre-reap order
  (its stub asserts the acquire fires after the reap).

---

## Revision 8 (bot PR #276 r3 P2 fold-in) — the reaper VALIDATES its kill-target identity

Revisions 6 + 7 closed the interlock's ACQUIRE-axis kill-race (the quiet acquire leaves
the owner sidecar naming the OLD supervisor; reap-then-acquire keeps the two reaping
flows mutually exclusive on the freed lock). They relied on a stated safety property:
during a held-lock window the sidecar names the now-DEAD old supervisor, so a concurrent
reaper that reads it `taskkill`s a dead PID → exit 128 → benign no-op. The Codex bot's
round-3 P2 (`internal/cli/migrate_serena.go:993`, the migrate's `willReap == false`
no-reap branch) found the GAP in that property: it holds ONLY while the recorded PID stays
DEAD.

- **Root.** `supervisor.lock.owner.json` is best-effort and SURVIVES a supervisor crash
  (a quiet holder's `Release()` deliberately leaves it; an OS-killed supervisor never
  tidies it). If that crashed supervisor's PID is later REUSED by an unrelated OS process,
  a concurrent reaper firing in the migrate's held-lock window reads the stale sidecar and
  `taskkill /F /T`s the REUSED, unrelated process. Both reap functions —
  `forceKillSupervisor` (rollback closure) and `v5UpgradeDeps.ForceKillSupervisor` (the
  production reap primitive `ReapSupervisorForRestart` calls for the migrate AND the
  auto-register INTRODUCE, install_migration_wiring_windows.go) — read `owner.PID` and
  called `killPIDViaTaskkill(owner.PID)` validating ONLY `PID > 0`, never that the PID is
  actually a supervisor. Blind-kill confirmed (file:line verified at both sites).
- **The `willReap == false` no-reap branch is the specific exposure.** That branch
  quiet-acquires `supervisor.lock` (flock) and leaves any pre-existing stale sidecar
  UNTOUCHED. A concurrent auto-register/upgrade that fires during this held-lock window
  hits the reuse hazard above. (The auto-register reap routes through the SAME
  `v5UpgradeDeps.ForceKillSupervisor`, so it had the identical exposure.)

**Fix (Option B, root — validate the kill TARGET, not just clear data):**
`supervisorPIDIsLiveMcphubSupervisor(pid, sidecarStartedAt)` (install_migration_wiring_windows.go)
is the kill-target identity gate. It reuses the existing tested `process.LookupProcessIdentity`
primitive (via the `processLookupIdentityFn` seam).

> **Revision 8a (fable-5 #276 fold-in) — hardened to fourGateOwnershipCheck PARITY.** The
> first implementation applied only TWO checks (basename + `strings.Contains(CommandLine,
> "supervise")`). The fable-5 #276 xhigh security review found three gaps vs the established
> sibling `migration.fourGateOwnershipCheck` (internal/migration/journal.go). The gate now
> applies the SAME four gates, keyed on `supervise` instead of `daemon`:
>
> 1. **Image basename** = `mcphub`/`mcphub.exe` (case-insensitive).
> 2. **argv-token** (Finding 3) — argv[1] parsed from the command-line must equal `supervise`
>    EXACTLY, a token check NOT a substring, so a path/flag value merely containing `supervise`
>    (e.g. `--log-dir C:\supervise\logs`) cannot satisfy it. Mirrors the `firstArgIsGUI`
>    argv-token discipline of `mcphub gui --force --kill` (install.go).
> 3. **Creation-time precedence** (Finding 1) — the process `CreationDateUnix` must PRECEDE the
>    sidecar `SupervisorLockOwner.StartedAt` (RFC3339Nano string parsed to Unix seconds to match
>    `CreationDateUnix`). A PID created at/after the sidecar write is a post-crash REUSE — even a
>    reused `mcphub supervise` (a doomed duplicate mid-exit) — and is refused. Empty/unparseable
>    `StartedAt` → fail closed, mirroring gate-3's "createdUnix is zero" refusal. Both reap
>    functions now thread `owner.StartedAt` (previously read-then-discarded).
> 4. **Install-dir path** (Finding 1) — `ExecutablePath` under `filepath.Dir(os.Executable)` (the
>    same anchor the forward migration uses for `migration.State.InstallDir`), via a
>    `supervisorReapInstallDirFn` test seam; skipped when unresolvable, exactly as gate-4 skips on
>    `installDir == ""`.
>
> **Transient probe errors fail the reap LOUD (Finding 2).** The gate returns a tri-state
> `(bool, error)`: `(true, nil)` kill; `(false, nil)` proven-not-a-supervisor (any gate fails OR
> `process.ErrProcessNotFound` — PID proven gone) → benign no-op; `(false, err)` for any OTHER
> lookup error (transient WMI stall that survived `LookupProcessIdentity`'s 3 retries) → the reaper
> cannot PROVE the PID dead, so it propagates a reap FAILURE rather than reporting "nothing to
> kill". This closes the gap where a transient probe failure on a genuinely-live old supervisor
> would silently satisfy the reap-before-spec-bearing-write §7.1 guarantee — load-bearing on the
> generic `RunInstallUpgrade` path which has no interlock backstop.

Both reap functions consult the gate BEFORE `taskkill`: a stale / reused / unrelated /
already-gone PID fails → treated as "no supervisor to reap" (benign no-op), never force-killed;
a transient-probe-error PID propagates a reap failure. It fixes the root for ALL THREE reaping
flows (migrate, auto-register INTRODUCE, rollback) because they share these two reaper functions.
`forceKillSupervisor` swallows a reused/non-supervisor PID as a no-op and now wraps a
transient-probe error; `v5UpgradeDeps.ForceKillSupervisor` returns nil (benign) for the
live-but-not-a-supervisor case, propagates the transient-probe error, AND STILL propagates the
corrupt-sidecar `PID <= 0` error from codex r4.

**Tests added (api wiring, Windows-tagged, deterministic — no real process is ever killed;
the kill is mediated through a new `killPIDViaTaskkillFn` seam):**

- `TestSupervisorPIDIsLiveMcphubSupervisor_Gate` — table: live supervisor (exe/no-exe) →
  true; reused unrelated process (`notepad.exe`/`python.exe`/`node.exe`) → false; mcphub
  but not a supervisor (`gui`/`daemon`) → false; `ErrProcessNotFound` → false; transient
  lookup error → false; PID ≤ 0 → false.
- `TestForceKillSupervisor_Rollback_SkipsReusedNonSupervisorPID` /
  `TestV5UpgradeDeps_ForceKillSupervisor_SkipsReusedNonSupervisorPID` — seed a sidecar
  naming a PID the lookup stub reports as an unrelated process; assert the reaper does NOT
  kill it (the recorder `killed` slice stays empty) and returns benign. FALSIFIED on the
  unfixed code: both FAIL with `killed=[31337]` (the reaper kills the reused PID) — verified
  by temporarily disabling the gate.
- `TestForceKillSupervisor_Rollback_KillsLiveSupervisor` /
  `TestV5UpgradeDeps_ForceKillSupervisor_KillsLiveSupervisor` — positive path: a sidecar
  naming a live `mcphub.exe supervise` PID IS reaped (gate does not over-block).

---

## Revision 9 (bot PR #276 r4 P2 fold-in) — auto-register holds the interlock for its NO-supervisor introduce too

Revisions 6 + 7 closed the auto-register INTRODUCE-while-running (`needReap`) case:
reap → quiet-acquire-the-FREED-lock → install-with-bypass → start. But the auto-register
has a SECOND spec-bearing write path the interlock did NOT cover. The Codex bot's round-4
P2 (`internal/api/serena_auto_register.go`, the `needStart && !needReap` no-supervisor
branch) found it:

- **Root.** When the FIRST serena workspace is introduced while NO supervisor is running
  (`!priorHasSpec && !supRunning` ⟹ `needReap=false`, `needStart=true`), the cutover
  reaps NOTHING but STILL writes the spec-bearing `runtime_spec` intent (the §7.1 gate's
  `HasRuntimeSpecRow() && !priorIntent.HasRuntimeSpecRow()` fires) and THEN starts a
  supervisor. The original code acquired the interlock ONLY inside `if needReap` and
  passed a zero-value bypass token on this path. The §7.1 gate passed naturally (nothing
  running at the probe), but the write→start window was UNPROTECTED: an old-decoder
  supervisor could take the `supervisor.lock` singleton between the liveness probe and
  the auto-register's own start, read the just-written `runtime_spec` with
  `DisallowUnknownFields`, and split-brain — the exact original migrate hazard, re-entering
  through the no-reap door.
- **Why the migrate was already correct.** The migrate's `acquireInterlockOnce` is
  idempotent and is ALSO called unconditionally at the no-reap spec-bearing boundary
  (step 7e, `migrate_serena.go` — `if len(installWorkspaces) > 0`), so the migrate's
  no-reap spec-bearing write already held the interlock. The auto-register did not mirror
  that second acquire site.

**Fix (`serena_auto_register.go`):** rename the reap-path acquire closure to the generic
idempotent `acquireInterlock` (a `interlockHeld bool` guard, mirroring the migrate's
`acquireInterlockOnce`) and add a SECOND call site immediately BEFORE the install, gated on
`!needReap && !priorHasSpec` (the no-supervisor spec-bearing introduce). No supervisor holds
the lock there, so the quiet `TryLock` normally succeeds; the minted bypass token is passed
into the install (else the §7.1 gate refuses the cutover's OWN held lock). On acquire-FAIL
(a concurrent migrate/cutover holds the lock): emit the distinct
`serena-auto-register-deferred-on-interlock` event + return the honest 503 via
`failPreCommit` — which, because `needReap=false`, owes NO recovery restart (nothing was
reaped) and just releases the interlock defensively + rolls back the registry row. The
LIVE-ADD-to-a-stopped-pool path (`needStart && !needReap` but `priorHasSpec=true`) is NOT
spec-bearing (prior intent already carries `runtime_spec` → the introduce gate never fires)
and is left untouched — no interlock.

**Concurrency re-verification (file:line verified):**
1. The no-reap introduce now HOLDS the interlock across write→start → no foreign supervisor
   can start in that window (every supervisor-starter calls `api.AcquireSupervisorLock` and
   fails fast on the held flock, `supervise.go:373`). CONFIRMED.
2. The §7.1 gate receives the bypass token whose `lk.path == filepath.Join(stateDir,
   "supervisor.lock")` (the gate's own resolver, `install_parsed_manifest.go:342-343`) and
   `lk.fl != nil` (still held) → the auto-register's OWN spec-bearing write is ALLOWED, not
   refused. CONFIRMED (the install-seam test asserts a non-nil, path-matching, still-held
   token).
3. Release on ALL paths: the explicit `releaseInterlock()` before the post-commit start
   (so the child can `AcquireSupervisorLock`), `failPreCommit`'s defensive release on the
   defer/install-fail paths, and the late-bound deferred `releaseInterlock()` safety-net
   backstop for any early return → no leak (idempotent closure; `Release()` nils `.fl`).
   CONFIRMED.

**Tests added (api, deterministic via the existing seams; FALSIFIED on unfixed code first):**
- `TestAutoRegisterSerena_Introduce_NoSupervisor_AcquiresInterlockBeforeSpecBearingWrite` —
  the no-supervisor introduce ACQUIRES the interlock (seam fires) WITHOUT reaping, the
  install receives a non-nil matching STILL-HELD bypass token, and the step order is
  `[acquire install start]`. FALSIFIED: FAILS on the unfixed code (the acquire seam never
  fires on the `!needReap` path; the install gets a zero-value token).
- `TestAutoRegisterSerena_Introduce_NoSupervisor_DefersWhenInterlockHeld_NoReap` — a held
  lock makes the no-reap introduce DEFER: install NEVER runs, NO reap, NO recovery restart,
  registry row rolled back, distinct event emitted, honest error names `supervisor.lock`.
  FALSIFIED: FAILS on the unfixed code (no acquire → the install runs → the call succeeds).

---

## Plan (3 phases, each commit-sized + tested)

### Phase 1 — gate bypass plumbing via typed token (api package; changes NO production behavior)
- `internal/api/supervisor_lock.go`: add the exported wrapper type
  `InstallParsedManifestBypass struct{ lk *SupervisorLock }` and the method
  `(*SupervisorLock).AllowSpecBearingWriteBypass() InstallParsedManifestBypass`.
- `internal/api/install_parsed_manifest.go`:
  - add `SupervisorLockBypass InstallParsedManifestBypass` to
    `InstallParsedManifestOpts` (:48-68).
  - guard the §7.1 probe (:315-329) with a verified-identity check: bypass applies
    ONLY when `opts.SupervisorLockBypass.lk != nil` AND `opts.SupervisorLockBypass.lk`
    is still held (`.fl != nil`) AND `opts.SupervisorLockBypass.lk.path ==
    filepath.Join(stateDir, "supervisor.lock")` (the gate's own `stateDir` from
    :224). On a non-matching path → NO bypass (probe runs; fail-closed) — this folds
    the Crux-3 path-mismatch check INTO the gate.
  - emit info event `spec-bearing-write-allowed-under-caller-lock` (body: matched
    lock path) on a verified bypass.
- Tests (deterministic — engineered held lock, no timing dependence):
  - `..._SpecBearingWrite_BypassedWhenCallerHoldsMatchingLock`: acquire the real
    `supervisor.lock` in-test on the gate's exact path → mint the token via
    `AllowSpecBearingWriteBypass()` → assert `SupervisorRunningUnderStateDir` reports
    running (proves the per-handle quirk) → `InstallParsedManifest` with the token
    SUCCEEDS + HasRuntimeSpecRow.
  - `..._StillRefusesWithoutToken`: zero-value bypass → refuses + emits
    `spec-bearing-install-refused`.
  - `..._RefusesWhenBypassLockPathMismatches`: mint a token from a lock on a
    DIFFERENT leaf → gate REJECTS the bypass → probe runs → refuses (the in-gate
    Crux-3 guard).
  - `..._RefusesWhenBypassLockAlreadyReleased`: mint, then `Release()` the lock
    (`.fl == nil`) → gate REJECTS the stale token.
- Acceptance: `go build ./... && go vet ./...` (copylocks clean) +
  `go test -tags=test_state_path_env ./internal/api/` green; the 14 existing call
  sites unchanged (zero-value `SupervisorLockBypass` preserves behavior).

### Phase 2 — migrate + auto-register interlock acquire/release (cli + api)
**2a — shared interlock seam (cli):**
- `internal/cli/migrate_serena.go`: add seam `acquireSupervisorInterlockFn` →
  `(*api.SupervisorLock, func(), error)` (return the handle so the caller can mint
  the bypass token, plus an idempotent release closure).
- `internal/cli/migrate_serena_restart_windows.go`: production binding
  `defaultAcquireSupervisorInterlock` = `api.AcquireSupervisorLock(
  filepath.Join(api.DaemonStateDir(), "supervisor.lock"))` returning the handle +
  an idempotent release. **PATH MUST be `api.DaemonStateDir()` (the gate's exact
  resolver, install_parsed_manifest.go:224), NOT `stateDirFunc()`** — a mismatch
  silently re-opens the bug (the in-gate Crux-3 guard from Phase 1 is the backstop,
  but the call site must still hard-code the right resolver). HIGHEST-PRIORITY review
  item.
- `internal/cli/migrate_serena_restart_other.go`: POSIX no-op (returns a nil handle +
  a no-op release + nil error; the spec-bearing path is unreachable on POSIX — the
  reap stub fails loud first, migrate_serena_restart_other.go:39).

**2b — migrate driver wiring (cli):**
- Acquire the interlock AFTER the reap (post step 7 :706 / the late-reap step 7d
  :835), arm the release ON ACQUIRE (Revision 3, mirror releaseRegistryLock
  :497-503).
- Pass `opts.SupervisorLockBypass = handle.AllowSpecBearingWriteBypass()` at the
  step-8 write (:846).
- Release before EVERY start: recovery :736, :859, :903 + normal :915, AND the early
  recovery-start at :446 (no-op there — never acquired). One idempotent closure.
- On acquire-FAIL (a foreign supervisor OR a concurrent auto-register/upgrade won the
  window): fail loud — "a supervisor (or another serena cutover) started during the
  migrate window and now holds supervisor.lock; the new dynamic-pool intent was NOT
  written and legacy serena is untouched — wait for it to settle (or `mcphub status`)
  and re-run the migrate." Do NOT block. (§7.1.1 covers why this is safe: the acquire
  is post-reap-PRE-write, so the intent is not yet committed and the legacy state is
  the prior reaped-but-restartable supervisor; the deferred outer stack restores the
  reconcile + registry; the recovery-start the migrate would owe for its OWN reap is
  handled by the existing willReap recovery branch since willReap is already true at
  this point if a reap ran.)

**2c — auto-register extension (api) [Revision 2 / Starter A; acquire ORDER corrected by Revision 7]:**
- `internal/api/serena_auto_register.go`: on the INTRODUCE branch (`needReap`,
  line 268), acquire `supervisor.lock` IMMEDIATELY AFTER the reap (Revision 7 — the
  running supervisor holds the lock, so a pre-reap acquire never succeeds) via a NEW seam
  `autoRegisterAcquireInterlockFn` (default nil; the CLI wires it to the SAME
  `defaultAcquireSupervisorInterlock` via a new param on
  `SetSerenaAutoRegisterCutoverPrimitives`, keeping api↛cli direction intact).
  Release before the start (line 410-411) and on every INTRODUCE pre-commit error.
- Pass the minted bypass token into the auto-register `InstallParsedManifest` call
  (line 352) so its §7.1 gate does not misread auto-register's OWN held lock.
- On acquire-FAIL: do NOT reap; roll back the registry row via the existing
  `rollback()` (line 230) — runs BEFORE any reap, old supervisor untouched; emit a
  DISTINCT info event `serena-auto-register-deferred-on-interlock` (NOT
  `spec-bearing-install-refused`); return an HONEST error
  ("another serena cutover/migrate holds supervisor.lock; retry shortly") the router
  maps to 503 → client retries.
- LIVE-ADD branch unchanged (no reap, already §7.1-safe, registry-flock-serialized
  vs the migrate).
- When the interlock seam is nil (unwired / non-Windows): the INTRODUCE branch's
  existing cutover-support gate (serena_auto_register.go:290-295) already fails loud
  pre-commit on a platform that cannot reap/start, so a nil interlock seam there is
  consistent with the existing "introduce needs the Windows primitives" refusal.
- Tests (api, deterministic):
  `AutoRegisterIntroduce_AcquiresInterlockBeforeReap` (seam observes the lock held
  before `autoRegisterReapFn` is called);
  `AutoRegisterIntroduce_DefersWhenInterlockHeld_NoReap_NoKill` (pre-held lock →
  reap seam NEVER invoked, registry row rolled back, distinct event emitted);
  `AutoRegisterIntroduce_PassesBypassTokenToInstall` (the install seam receives a
  non-nil matching `SupervisorLockBypass`).

**2d — migrate tests (cli, deterministic via the reconcile-stub injection point = the window):**
- `Interlock_BlocksConcurrentSupervisorStartInWindow` (a concurrent
  `AcquireSupervisorLock` on the gate's exact path — NOT a child, Revision 5 — fails
  mid-flight while the migrate holds; migrate succeeds + commits spec-bearing).
- `Interlock_ReleasedBeforeStart` (the start stub observes the lock acquirable).
- `Interlock_ReleasedOnRecoveryStartBranch` (Revision 3 — recovery path; SECOND
  in-test acquire after return SUCCEEDS → no leak).
- `Interlock_AcquireFailsLoud_WhenForeignHolderWonTheWindow` (pre-held lock → fail
  loud, no spec-bearing write).
- `HandoffWindowEvent_EmittedOnReconcileRetry` (Revision 4 — start stub forces
  >1 reconcile-ready retry → assert the `supervisor-interlock-handoff-window` event
  lands in supervisor-events.log).
- Acceptance: `go test -tags=test_state_path_env ./internal/api/ ./internal/cli/`
  green; existing ReapClearsTheGate tests (migrate_serena_test.go:1178, :2455) still
  pass.

### Phase 3 — e2e + canonical-doc updates
- `docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md`
  §7.1: add §7.1.1 "Reap→write→start window interlock" (lock-as-mutex; typed-token
  gate bypass; the reap-complete→acquire gap covered by acquire-FAIL fail-loud);
  extend acceptance criterion 2 ("...AND no foreign supervisor can start in the
  reap→write window, AND no concurrent serena auto-register reap can force-kill the
  migrate's lock-holding process"); cross-ref the "old supervisor reading new files
  is NOT auto-safe" note (:533-536) with the decoder-vintage terminology.
- CLAUDE.md "Supervisor" + the serena migrate area: document the Starter-B operator-
  sequencing constraint ("do NOT run `mcphub install --upgrade` concurrently with
  `mcphub migrate serena legacy-to-dynamic-pool`; they are not co-serialized; if they
  collide the migrate fails loud and is re-runnable") and the new
  `supervisor-interlock-handoff-window` / `serena-auto-register-deferred-on-interlock`
  events.
- Full `go build ./... && go vet ./... && go test ./...` + tagged api/cli per
  CLAUDE.md Step 1; CLAUDE.md Step-8 consistency grep
  (`SupervisorLockBypass` / `supervisor.lock` interlock / §7.1.1 / decoder-vintage —
  no stale "bool flag" or "refuse-while-running is the only mechanism" text).

## Key risk
Path-resolution mismatch (the interlock acquires a different lock leaf than the gate
probes) → silent re-open. DUAL mitigation now: (1) the call sites hard-code
`api.DaemonStateDir()`; (2) the §7.1 gate itself REJECTS a bypass token whose lock
leaf ≠ the gate's `stateDir` (Phase 1), so a mismatch fails CLOSED (probe runs)
instead of silently bypassing. A Phase-1 test
(`..._RefusesWhenBypassLockPathMismatches`) and a Phase-2 test assert held-lock path
== gate-probed path.

## Why 3 phases
Phase 1 reviewable in isolation (pure api: token type + gate identity-guard, fully
tested without migrate, changes NO production behavior until a caller mints a token).
Phase 2 depends on Phase 1's token and adds BOTH the migrate interlock AND the
auto-register extension (the two reaping flows must land together — fixing only the
migrate side leaves the auto-register kill-race open). Phase 3 is docs + integration.
Each phase independently revertable; Phase 2's two sub-parts (migrate / auto-register)
share the seam so they are one logical commit.
