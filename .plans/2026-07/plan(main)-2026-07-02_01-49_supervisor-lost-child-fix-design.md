# Implementation-ready design — supervisor lost-child / quarantine class (P1a+P1b, P2a, P2b)

Deep design study (Fable 5, 2026-07-02). Every citation below re-verified against HEAD this session.
Source bug: `work-items/bugs/2026-07-02-supervisor-lost-child-quarantine-class.md`.
Prior review: `.reports/2026-07/report(main)-2026-07-01_23-58_hub-red-incident-review.md`.

## 0. Verification pass — corrections to the filed write-ups

Confirmed as filed (all re-read at HEAD):

- `supervisorPortBindGrace = 5s` — `internal/cli/supervise_liveness.go:20`; applied at `:443`, `:458`, `:475`, anchored on `entry.StartedAt`.
- `port_unbound` / `port_owner_mismatch` / `port_owner_self` → `supervisorLivenessReasonNeedsRestart` (`supervise_liveness.go:499-508`) → sweep posts `EvManualRestart` (`:282-311`). `ownerPID` computed `:428`, mismatch verdict `:466-467`.
- StRunning-on-start: spawn success → `PostSelf(EvHealthOK)` at `supervisor_controller.go:2895` (design comment `:2779-2786`: "health here is process-start success").
- Wait goroutine `internal/cli/supervise.go:3612-3723`; pid-blind `tracker.MarkExited(taskName)` at `:3649`; `crashEvent{Daemon, ExitCode, WaitErr}` (NO pid) at `:3704`, struct at `:3064-3068`. Bridge `runCrashEventBridge` (`supervisor_controller.go:3324-3377`) posts `EvChildExit` with Body `{exit_code, wait_err, clean_exit}` — task name only.
- `MarkExited` unconditional clear `supervisor_runtime_tracker.go:290-305`; `MarkSpawned` bumps `PIDGeneration` `:207-225`; generation IS persisted (`PersistTo` `:458`) and rehydrated (`HydrateFromState` `:416`).
- SM: `StRunning+EvChildExit→StBackoffWaiting` (`internal/api/supervisor_state_machine.go:134-135`); `StBackoffWaiting+EvTimerDue`, `Failures<10→StSpawning` else `StQuarantined` (`:188-194`); `StRunning+EvManualRestart→StExiting, queued_action=respawn` (`:140-141`); `StExiting+EvChildExit→StSpawning` when queued respawn (`:147-157`); `StQuarantined+EvManualRestart→"reset failures, …create-process"` (`:216-217`).
- Terminate targets only `tracker.CurrentPID` (`supervise.go:2389-2394`); identity proof `:2426-2431`. Defect C mechanism confirmed: mismatch → EvManualRestart → StExiting → terminate own fresh child; squatter never targeted.
- Quarantine reason string at `supervisor_controller.go:3139`; force-respawn escape exists (`supervise_respawn.go:231-248`; GUI `internal/gui/daemon_env.go:56`, `:367` → `api.DialSupervisorIPCRespawn`).

Corrections:

1. **`synthesizeForeignChildExit` is `internal/cli/supervisor_controller.go:3042-3062`, NOT `supervise.go:~3042`** (that offset in supervise.go is a spawn-fn doc comment). The bug file's Deliverable-1 citation is wrong; the report's P1 text is right.
2. The wait goroutine's `MarkExited`+persist (`supervise.go:3649-3650`) runs OFF-loop — a standing exception to the Conc-F2 single-writer discipline the sweep was fixed for. P1a keeps the mutation off-loop but makes it generation-guarded (tracker mutex + monotonic generation make it safe); full on-loop migration is explicitly out of scope.
3. `LookupProcessIdentity` (argv/CommandLine probe needed by P2a) is **Windows-only** (`//go:build windows`, `internal/process/lookup_process_identity_windows.go:1`). P2a reap is Windows-GA-only in v1; Linux/macOS keep today's behavior (documented below).
4. `pidIdentityStartTolerance = 2s` (`internal/process/pid_identity_common.go`) — second-precision `CreationDateUnix` from the identity lookup is a valid start-time proof for the kill gate (tolerance ≥ 1s).

---

## 1. PR-1 — P1a (generation-stamped exit attribution) + P1b (first-bind deadline), ONE PR

### 1.1 P1a — generation-stamped exit attribution

**Invariant being installed:** an `EvChildExit` may mutate tracking or drive an SM transition only if the exit belongs to the tracker's CURRENT `PIDGeneration` for the task. Staleness is judged by **generation, not PID equality** — PID equality breaks the deliberate-kill path (terminate clears `CurrentPID` to 0 via `MarkTerminated`→`MarkExited` at `supervise.go:2506` BEFORE the real exit arrives, and the StExiting queued respawn NEEDS that exit), and PID alone is ABA-vulnerable to OS PID reuse. Generation is bumped only by `MarkSpawned`, so the just-terminated current child's exit carries `gen == entry.PIDGeneration` (passes) while an older child's late exit carries `gen < entry.PIDGeneration` (dropped).

**Changes, exact:**

(a) `internal/cli/supervise.go:3064` — `crashEvent` gains two fields:

```go
type crashEvent struct {
    Daemon        api.SupervisorDaemon
    PID           int   // pid of the exited child (== spawnedPID)
    PIDGeneration int   // tracker generation stamped by MarkSpawned for THIS child
    ExitCode      int
    WaitErr       error
}
```

(b) `internal/cli/supervisor_runtime_tracker.go:207` — `MarkSpawned` returns the new generation:

```go
func (t *DaemonRuntimeTracker) MarkSpawned(taskName string, pid int, startedAt time.Time) int
// … existing body …; return entry.PIDGeneration (after increment)
```
Only the production spawn closure needs the value (`supervise.go:3578`: `spawnGen := tracker.MarkSpawned(d.TaskName, pid, startedAt)`); all other callers ignore the return (compile-compatible).

(c) `internal/cli/supervisor_runtime_tracker.go` — new method beside `MarkExited` (:290):

```go
// MarkExitedIfCurrent clears the runtime entry ONLY when pidGeneration is the
// tracker's current generation for the task (>= entry.PIDGeneration; the >
// case is impossible by monotonicity and treated as current defensively).
// Returns false — entry untouched — when the exit is STALE (an older
// generation's late cmd.Wait). Idempotent for the current generation
// (an already-cleared current-gen entry returns true).
func (t *DaemonRuntimeTracker) MarkExitedIfCurrent(taskName string, pidGeneration int) bool {
    // lock; entry := t.entries[canonical]
    // if pidGeneration < entry.PIDGeneration { return false }
    // …identical clears to MarkExited…; return true
}
```

(d) Wait goroutine (`supervise.go:3649-3650`) — replace `MarkExited` + persist:

```go
current := tracker.MarkExitedIfCurrent(taskName, spawnGen)
if current {
    _ = persistDaemonRuntimeTracker(events, tracker, statePath, taskName)
} else {
    _ = events.Emit(api.SupervisorEvent{Severity: "info", Source: "lifecycle",
        Event: "daemon-stale-exit-ignored", TaskName: taskName,
        Body: map[string]any{"pid": spawnedPID, "pid_generation": spawnGen,
            "exit_code": exitCode, "note": "late cmd.Wait exit of a superseded child; current tracking untouched"}})
}
```
**And the crashCh post at `:3702-3722` fires ONLY when `current == true`.** Dropping the stale exit at the source (not just in the controller) is load-bearing: a stale exit that reached the SM during StExiting would consume the queued respawn prematurely, re-creating the lost-child race one layer up. The crashEvent gains `PID: spawnedPID, PIDGeneration: spawnGen` regardless (bridge body + defense-in-depth).

(e) Bridge (`supervisor_controller.go:3355-3374`) — add to Body: `"pid": ev.PID, "pid_generation": ev.PIDGeneration` (ints; note handleBackoffWaiting already reads `ev.Body["exit_code"].(int)` so in-process `int` assertion is consistent).

(f) Controller-side processing-time guard — `handleLoopEvent`, **immediately after the canonicalization at `supervisor_controller.go:2385-2386` and BEFORE the `reaperOutstanding.Delete` at `:2398-2400` and before the clean-exit drop at `:2504`**:

```go
if ev.Kind == api.EvChildExit && c.tracker != nil && ev.Body != nil {
    if gen, ok := ev.Body["pid_generation"].(int); ok && gen > 0 {
        if entry, ok2 := c.tracker.Get(ev.TaskName); ok2 && gen < entry.PIDGeneration {
            // emit daemon-stale-exit-ignored (body: pid, pid_generation,
            // current_generation, sm_state) and RETURN — no SM transition,
            // no reaperOutstanding clear (the CURRENT child's reaper is
            // still live; clearing would re-open the Conc-F3 synthesize race).
            return
        }
    }
}
```
This is the authoritative check (evaluated at processing time on the loop, so it also covers the window where the exit was current when posted but a respawn was processed ahead of it in the queue). Events WITHOUT `pid_generation` — the pre-child synthetic self-post (`:2899`), `synthesizeForeignChildExit` (`:3059`), and the liveness sweep's `EvChildExit` for `missing_pid`/`pid_dead`/`pid_identity_*` reasons — pass through unchanged (treated as current; they are all "about the tracked child now" by construction).

**Why the guard placement matters (ordering):** the stale check must precede (1) `reaperOutstanding.Delete` — else a stale exit strands the synthesize gate open; (2) the clean-exit-at-StRunning drop (`:2504-2518`) — else a stale CLEAN exit stores `smStates=StIdle` while the current child is alive (a second live instance of the same bug class, currently latent at `:2505`).

**Edge cases, resolved:**

- *Exit of the pid being deliberately killed right now:* terminate does not bump generation → exit passes both guards → `StExiting+EvChildExit→StSpawning` queued respawn works. Covered by test 4.
- *Synthesized/foreign exits with no pid:* pass through (above). `synthesizeForeignChildExit`'s caller contract (terminate returned nil = target gone, not own-spawned, no reaper outstanding — `:2952-2987`) is unchanged.
- *StExiting→queued-respawn deadlock:* impossible — the only exits the SM waits for in StExiting are current-generation (terminate targets `CurrentPID`, generation unchanged) or synthesized (gen-less, passes).
- *Backoff/quarantine window:* stale exits today reach `StRunning+EvChildExit→StBackoffWaiting` and `RecordCrashAndCountInWindow` — phantom failures inflate the 30-min window toward quarantine. Under P1a they never reach `handleBackoffWaiting`; the window counts real crashes only.
- *Supervisor cold restart:* generation is persisted (`PersistTo :458`) and rehydrated (`HydrateFromState :416`); wait goroutines do not survive restart, so no stale exit can cross a restart. A hydrated row with `PIDGeneration<=0` is already marked stale by `loadSupervisorCurrentRunning` (`supervise.go:2273`). New spawns continue the persisted sequence — monotonicity holds across restarts.
- *Task removed mid-flight (`tracker.Remove`):* `Get` misses → guard passes → existing orphan-drop / reap-shadow path (`:2401-2429`) handles it as today.

**Events:** `daemon-stale-exit-ignored` (info, source=lifecycle; two emit sites: wait goroutine and controller guard, distinguishable by body fields). Add to the CLAUDE.md supervisor-events cross-channel routing list.

### 1.2 P1b — first-bind deadline replacing the flat 5s grace

**Rule:** `port_unbound` (and the grace arm of `port_owner_unverified`) is not a restart trigger before the FIRST successful bind of the CURRENT generation; a per-descriptor startup deadline gates that phase. After first bind, today's 5s logic stays byte-identical.

**Where the deadline lives — descriptor, fed from manifest:**

- `internal/api/supervisor_intent.go:52-81` — additive field on `SupervisorDaemon`:
  ```go
  // StartupBindDeadlineSeconds bounds how long a freshly-spawned child may
  // take to FIRST bind its port before the liveness sweep treats port_unbound
  // as a restart trigger. 0 = default (60s; serena-proxy descriptors 120s).
  // Additive + omitempty: old binaries ignore it; old intent files read as 0.
  StartupBindDeadlineSeconds int `json:"startup_bind_deadline_seconds,omitempty"`
  ```
- `internal/config/manifest.go:244-258` — same field on `DaemonSpec` (`yaml:"startup_bind_deadline_seconds,omitempty"`), `Validate()` rejects <0 and >600.
- Plumbing: `supervisorDaemonsFromPlan` (`internal/api/install_parsed_manifest.go:2445-2453`) copies it; the serena-proxy descriptor builder (`internal/api/supervisor_intent_build.go:310` row-construction vicinity / `register_supervisor.go:794`) sets 120 explicitly.
- Resolution helper (cli side, next to `daemonExpectedIdentityExe`):
  ```go
  func supervisorStartupBindDeadline(d api.SupervisorDaemon) time.Duration {
      if d.StartupBindDeadlineSeconds > 0 { return time.Duration(d.StartupBindDeadlineSeconds) * time.Second }
      if isSerenaProxyDescriptor(d) { return 120 * time.Second } // defense for pre-field serena rows
      return 60 * time.Second
  }
  ```

**Mechanics — sweep-goroutine-local bind latch (no tracker/persist changes, no Conc-F2 exposure):**

- `supervisorDaemonEntryLiveWithProbe` (`supervise_liveness.go:385`) signature change:
  `(d, entry, now, probe, bindGrace time.Duration) (live bool, reason string, portBoundByCurrentPID bool)`.
  `bindGrace` replaces the const at `:443`, `:458`, `:475`. `portBoundByCurrentPID` is true only on the verified-owner success return (`:469`) and the PortLive-fallback success (`:480`); false everywhere else (incl. `d.Port<=0`).
- `startSupervisorLivenessMonitor` (`:120`) owns `latch := map[string]int{}` (canonical task → generation at first observed bind) and passes it into `sweepSupervisorLivenessOnce` (new param; nil-safe for existing direct-call tests).
- Per daemon in the sweep loop (`:231`):
  ```go
  grace := supervisorPortBindGrace                       // 5s post-bind rule
  if g, ok := latch[taskName]; !ok || g != entry.PIDGeneration {
      grace = supervisorStartupBindDeadline(d)           // pre-first-bind rule
  }
  live, reason, bound := supervisorDaemonEntryLiveWithProbe(d, entry, now, livenessProbe, grace)
  if bound { latch[taskName] = entry.PIDGeneration }
  ```
- When `reason == port_unbound` fires with NO latched bind for the current generation (i.e. the startup deadline expired), emit **`daemon-bind-timeout`** (warn, source=liveness, body `{pid, port, deadline_seconds, waited_seconds}`) before the existing `daemon-running-state-stale` + `EvManualRestart` post. Post-bind unbound keeps only the existing event.
- Prune `latch` entries whose task is absent from the tracker snapshot (end of sweep).
- Status path: `supervisorDaemonEntryLive` wrapper (`:381-383`) and `supervisorStatusDaemons` pass `supervisorStartupBindDeadline(d)` as grace (status has no latch; tradeoff: a bound-then-lost port shows Stale in status after the deadline instead of 5s — display-only, restart decisions unaffected; document in the wrapper comment).

**Preserved behaviors (regression guards):** warm-restart stale-handoff rows have OLD `StartedAt` → already past any deadline → the immediate first sweep (`:130-140`) still terminates them at once. Crash-before-bind daemons are exit-driven (wait goroutine → EvChildExit → backoff), NOT sweep-driven — the longer deadline does not delay crash-loop detection. `port_owner_unverified` observe-only routing (`:253-268`) unchanged.

**Explicitly NOT in P1b:** changing `EvHealthOK` semantics (StRunning still = process-start success). That is decision **D-B** ("StRunning requires port-bind readiness"), registered as `proposed`/deferred — a true readiness gate touches SM rows, IPC synchronous-restart contracts (`postManualRestartAndWaitRunning`, `supervisor_controller.go:2227-2257` waits for StRunning), and GUI status meaning. P1b removes the operational damage without that surgery.

### 1.3 P1 test plan (deterministic; no natural-timing reliance)

Tracker units (`supervisor_runtime_tracker_test.go`):
1. `TestMarkExitedIfCurrent_StaleGenerationIsNoop` — MarkSpawned(pid=100)→gen1; MarkSpawned(pid=200)→gen2; `MarkExitedIfCurrent(task,1)`==false, entry still `{running,200,gen2}`; `MarkExitedIfCurrent(task,2)`==true clears.
2. `TestMarkExitedIfCurrent_IdempotentForCurrentGeneration` — clear twice at gen N → both true.

Wait-goroutine (supervise-level, `TestHelperProcess` pattern — child blocks on stdin, released explicitly; the "late exit window" is engineered by holding the child alive across a simulated respawn, NOT by sleeps):
3. `TestWaitGoroutine_StaleExitNoCrashPostNoClear` — spawn child A via production spawn closure (test crashCh); simulate supersession: `tracker.MarkSpawned(task, fakePIDB, now)`; release A; assert: crashCh stays empty (bounded receive with timeout-assert-empty), tracker still `{fakePIDB, genB}`, `daemon-stale-exit-ignored` emitted.
4. `TestWaitGoroutine_CurrentExitPostsWithGeneration` — spawn A, release A with no supersession; crashEvent carries `{PID: pidA, PIDGeneration: genA}`.

Controller units (existing harness style, `supervisor_controller_test.go:194`):
5. `TestHandleLoopEvent_StaleGenerationExitDropped` — seed tracker gen3/StRunning; post `EvChildExit{Body:{pid_generation:2}}` → smStates stays StRunning, no failure recorded, no backoff timer, stale event emitted, `reaperOutstanding` NOT cleared.
6. `TestHandleLoopEvent_CurrentGenerationExitAtStExitingDrivesQueuedRespawn` — StExiting + queuedActions=respawn + `EvChildExit{pid_generation == current}` → StSpawning (the deliberate-kill regression guard).
7. `TestHandleLoopEvent_StaleCleanExitDoesNotIdleRunningDaemon` — `EvChildExit{clean_exit:true, pid_generation: stale}` at StRunning → dropped by the stale guard BEFORE the clean-exit handler; smStates stays StRunning (guards the ordering).
8. `TestHandleLoopEvent_GenerationlessExitUnchanged` — no `pid_generation` in body → routes exactly as today (synthetic/foreign/liveness parity).
9. `TestIncidentReplay_LateExitAfterRespawnNoDuplicateSpawn` — scripted end-to-end on the controller: spawn(gen1) → liveness `EvManualRestart(port_unbound)` → StExiting → current exit(gen1) → respawn(gen2) → inject late duplicate `EvChildExit{pid_generation:1}` → assert spawn-fire count == 2 and gen2 tracking intact (the 2026-07-01 incident transcript, compressed).

Liveness units (existing `setSupervisorLivenessProbeForTest` seam; `StartedAt` set directly on the entry — the window is engineered by back-dating StartedAt, no clock injection needed):
10. `TestSweep_PortUnboundWithinStartupDeadlineNoRestart` — unbound probe, `StartedAt=now-30s`, deadline 120s, no latch → no loop event.
11. `TestSweep_PortUnboundPastStartupDeadlineRestartsAndEmitsBindTimeout` — `StartedAt=now-121s` → `daemon-bind-timeout` + `EvManualRestart`.
12. `TestSweep_PostFirstBindUnboundRestartsAt5s` — sweep1 probe owner==CurrentPID (latches); sweep2 unbound, `StartedAt=now-30s` (< deadline, > 5s) → restart fires (post-bind rule).
13. `TestSweep_BindLatchResetsOnNewGeneration` — latch at gen1; MarkSpawned→gen2 `StartedAt=now`; unbound → no restart (startup rule re-applies).
14. `TestSupervisorStartupBindDeadline_Resolution` — 0→60s; serena-proxy descriptor→120s; explicit 300→300s.
15. Manifest plumb: `DaemonSpec.StartupBindDeadlineSeconds` → `supervisorDaemonsFromPlan` output; `Validate()` bounds.
16. Sweep-test sweep: update any existing test asserting the flat-5s behavior for freshly-started daemons (`supervise_liveness_test.go:378+` cluster).

---

## 2. PR-2 — P2a: reap-or-adopt on `port_owner_mismatch`

**Chosen shape: reap (adopt REJECTED for v1).** Adoption (seeding the squatter into tracking as a warm-start foreign PID à la `hydrateControllerRunningStates`, `supervise_liveness.go:108-118`) would avoid one restart blip but: the squatter has no `cmd.Wait` reaper (permanent reliance on the synthesize path), its `StartedAt` provenance is a shelled CIM lookup rather than our own spawn record, and its generation bookkeeping fights P1a. One identity-gated kill + one clean controller-serialized respawn is strictly simpler and bounded. Record in D-A as the rejected alternative.

**Classifier — single owner, shared with P2b** (new `internal/cli/supervise_squatter.go`):

```go
type squatterVerdict int
const (
    squatterOwnTask    squatterVerdict = iota // verified: our binary + THIS task's argv
    squatterForeign                            // identity read OK; not this task's daemon
    squatterUnverified                         // lookup failed / non-Windows — fail closed
)

var squatterLookupIdentityFn = process.LookupProcessIdentity // windows; injectable
var squatterExeMatchesFn     = process.PIDExecutableMatches  // handle-truth exe gate

func classifyPortSquatter(d api.SupervisorDaemon, ownerPID, selfPID int,
    tracked map[string]DaemonRuntimeEntry) (squatterVerdict, process.ProcessIdentity)
```

Gates, in order (ALL must pass for `squatterOwnTask`):
1. `ownerPID > 0 && ownerPID != selfPID` (self is the existing `port_owner_self` reason) and `ownerPID != entry.CurrentPID` (given by the mismatch).
2. **Not a tracked sibling:** no other task's `CurrentPID` or `OrphanPID` equals `ownerPID` → else `squatterForeign` with a `tracked_sibling` note (a same-port misconfiguration between two of our daemons must never resolve by killing the sibling).
3. **Exe gate (handle truth):** `squatterExeMatchesFn(ownerPID, daemonExpectedIdentityExe(d.Command))` (`internal/process/pid_identity_windows.go:23-34` — `QueryFullProcessImageName` on a live handle; argv spoofing cannot beat it; a hardlink/copy of our binary at another path mismatches → foreign → no kill).
4. **Identity read:** `id, err := squatterLookupIdentityFn(ownerPID)` (`lookup_process_identity_windows.go:78-100`; PowerShell CIM primary, wmic fallback, 3×1s retries). Any error incl. `ErrProcessNotFound`, CLM-locked+no-wmic → `squatterUnverified`.
5. **Argv gate (this task, not merely our binary):** token-boundary match on `id.CommandLine` —
   - global daemon: contains `--server <d.Server>` AND `--daemon <d.Daemon>` as adjacent flag/value tokens;
   - runtime-spec proxy: contains `--task-name <canonicalSupervisorTaskName(d.TaskName)>` (that pair lives in `d.Args` per the descriptor contract, `supervisor_controller.go:2383` comment).
   Match on parsed whitespace tokens, exact string equality per token — no substring matching (`serena-b1` must not match `serena-b133f336`).
6. Build the kill proof from the LOOKUP's observed values: `process.PIDIdentityProof{PID: ownerPID, ExecutablePath: id.ExecutablePath, StartedAt: time.Unix(id.CreationDateUnix,0).UTC().Format(time.RFC3339Nano)}`. Second precision is inside `pidIdentityStartTolerance = 2s` (`pid_identity_common.go:22-28`).

**Reap capability — single owner in `runSupervise`,** beside the spawn/terminate closures (`supervise.go:946-953`):

```go
squatterReapFn := makeProductionSquatterReapFn(events)
// func(d api.SupervisorDaemon, proof process.PIDIdentityProof) error
// body = process.TerminatePIDWithIdentity(proof)
```
`TerminatePIDWithIdentity` (`pid_identity_windows.go:51-86`) is the right primitive: it re-verifies exe+basename+start-time **on the same held handle** it terminates (kill-time TOCTOU closed), maps a dead-PID open to `ErrProcessAlreadyExited` (benign), and on `ERROR_ACCESS_DENIED` returns an error → **fail closed, no kill** (`:68-73`). It then waits ≤5s for exit — the sweep gets synchronous confirmation the port is freeable.

**Sweep integration** (`sweepSupervisorLivenessOnce`, the `port_owner_mismatch` arm before the `:282-311` post). Probing and the kill syscall stay on the sweep goroutine (off-loop is fine: no tracker/SM mutation; the discipline "off-loop posters detect and post" is preserved — SM consequences still travel via the posted event):

```go
if reason == supervisorLivenessReasonPortOwnerMismatch {
    verdict, id := classifyPortSquatter(...)     // rate-limited, see below
    switch verdict {
    case squatterOwnTask:
        if err := squatterReapFn(d, proof); err != nil && !errors.Is(err, process.ErrProcessAlreadyExited) {
            emit "daemon-port-squatter-reap-failed" (warn) ; continue   // NO restart post — futile while port is held
        }
        emit "daemon-port-squatter-reaped" (warn, body{squatter_pid, executable_path, command_line, started_at})
        // fall through to today's EvManualRestart post → StExiting → terminate own child → clean respawn binds
    case squatterForeign:
        emit "daemon-port-squatter-foreign" (warn, body incl. identity) ; continue   // observe-only
    case squatterUnverified:
        emit "daemon-port-squatter-unverified" (warn) ; continue                     // observe-only, fail closed
    }
}
```

**Deliberate behavior change:** `port_owner_mismatch` stops being an unconditional restart. Foreign/unverified squatters get observe-only warns — restarting our own child cannot succeed while a foreign process holds the port, and that futile loop is exactly the 10-failures→quarantine churn (defect C). `supervisorLivenessReasonNeedsRestart` keeps the mismatch entry (the own-squatter path still uses it); the sweep intercepts before posting. **Existing sweep tests asserting mismatch→EvManualRestart must be updated** (`supervise_liveness_test.go`).

**Rate limiting (sweep-local, beside the bind latch):** per task, at most one identity lookup per 30s and at most 3 reap attempts per `respawnFailureWindow`; past that, downgrade to `squatterUnverified` handling with a `rate_limited: true` body note and point at the recover verb. Bounds sweep-tick inflation (lookup worst case ≈3×1s retries + shell) and prevents a kill-loop against something that keeps rebinding.

**POSIX:** `classifyPortSquatter` returns `squatterUnverified` on non-Windows in v1 (no `LookupProcessIdentity`); Linux gets `/proc/<pid>/cmdline` in a follow-up. Net Linux effect of PR-2: mismatch flips from restart-churn to observe-only — strictly less destructive.

**Security argument for D-A (what changes, why it is sound):** the supervisor gains kill authority over a PID it did not spawn, gated on the conjunction: (1) the port comes from `supervisor-intent.json` — operator-authored, owner-only-DACL state; (2) the process image IS our binary at the daemon's configured install path, proven from a live process handle, not argv; (3) argv names THIS task exactly; (4) the kill primitive re-verifies identity incl. start-time on the handle it terminates (no PID-reuse window); (5) `OpenProcess(PROCESS_TERMINATE)` succeeds only same-user (no privilege enablement anywhere) — ACCESS_DENIED fails closed; (6) every verdict and kill emits an audit event with the full observed identity.

**$security-reviewer checklist (gate for PR-2):**
- Verify the argv tokenizer cannot be confused by quoted/embedded spaces into cross-task matches; verify exact-token (not substring) semantics with a test using overlapping task names.
- Verify no code path enables `SeDebugPrivilege` or retries a kill after ACCESS_DENIED.
- Confirm the relax-lane threat delta is zero-new-capability: an attacker who can swap `supervisor-intent.json` (documented residual on relax-lane hosts) could already aim `terminate`/`spawn` at arbitrary descriptors; the reap adds targets that must ALSO be running our binary with our argv.
- Confirm `daemon-port-squatter-*` event bodies respect the 16 KB identity-cap rules (`CommandLine` may carry long workspace paths — truncate body-side, never identity fields).
- Confirm the tracked-sibling gate covers `OrphanPID` too.

**P2a tests:** `TestClassifyPortSquatter_OwnGlobalDaemon`, `_OwnSerenaProxyByTaskName`, `_RejectsForeignExe`, `_RejectsOtherTasksArgv` (incl. prefix-overlap names), `_RejectsTrackedSibling` (CurrentPID and OrphanPID variants), `_LookupErrorUnverified`, `_NonWindowsUnverified`; `TestSweepMismatch_OwnSquatterReapedThenRestart` (fake probes + recorded reap fn + fake loop: reap called with ownerPID, THEN EvManualRestart posted), `TestSweepMismatch_ForeignObserveOnlyNoRestart`, `TestSweepMismatch_ReapAccessDeniedNoRestart`, `TestSweepMismatch_RateLimitDowngrades`, `TestSquatterProof_SecondPrecisionStartTimeWithinTolerance`.

---

## 3. PR-3 — P2b: `mcphub daemon recover <task>` + quarantine-reason fix

**CLI verb:** new `internal/cli/daemon_recover.go`, registered beside the proxies (`daemon.go:392-396`): `c.AddCommand(newDaemonRecoverCmd())`. Update `daemon.go:90` Short to "Run or recover a daemon" (it currently reads "not by humans"). Cobra shape: `mcphub daemon recover <task-name> [--yes]`.

**Flow (composition; no new IPC verb — `respawn{force:true}` exists at `supervise.go:1684` → `handleRespawn`):**
1. Resolve state dir → read `supervisor-intent.json` → match descriptor via canonical/bare toggle (as `handleRespawn` does). Unknown → exit 2, print known task names.
2. Port disposition: `api.LoopbackPortOwnerPID(d.Port)`. Owner absent, or equals a live tracked PID for this task → skip reap.
3. Squatter path: `classifyPortSquatter(...)` (same single-owner classifier). `squatterOwnTask` → print identity, confirm unless `--yes`, `process.TerminatePIDWithIdentity(proof)`, then poll `LoopbackPortOwnerPID` until unbound (bounded 10s). `squatterForeign`/`squatterUnverified` → print the observed identity + honest refusal, exit 3 (no kill; operator handles the foreign process themselves).
4. Force respawn through the SUPERVISOR (never direct spawn — ownership stays with the controller): `api.DialSupervisorIPCRespawn(ctx, task, /*force=*/true, 15000)` (same client the GUI uses, `internal/gui/daemon_env.go:367`). The quarantine window reset already exists: `hydrateSMStateFromTrackerIfMissing` seeds `StQuarantined` (`supervisor_controller.go:2294-2314`) and `StQuarantined+EvManualRestart → "reset failures, …create-process"` (`supervisor_state_machine.go:216-217`).
5. Supervisor unreachable → exit 5 with pointer to `mcphub supervise` / autostart. Respawn error → exit 4 surfacing the IPC code.

**Quarantine reason-string fix.** Two emit sites plus a doc sweep:
- `supervisor_controller.go:3139` →
  `fmt.Sprintf("%d+ failures in %s sliding window; automatic respawns suspended — run 'mcphub daemon recover <task>' (or POST /api/daemon/respawn with force=true) to reap any port squatter and force a respawn; a supervisor restart also clears the window", c.quarantineThreshold, c.failureWindow)` (parametrized — the current text hardcodes "10+" and falsely implies restart is the ONLY recovery).
- `supervisor_controller.go:2922` ("EvTimerDue at or beyond quarantine threshold") — append the same recovery pointer.
- `grep -rn "until supervisor restart"` across repo (incl. `MarkQuarantined` docstring `supervisor_runtime_tracker.go:338-340`, CLAUDE.md, GUI copy if surfaced) and update every stale site (Step-8 consistency rule).

**P2b tests:** `TestDaemonRecover_UnknownTask`, `TestDaemonRecover_NoSquatterGoesStraightToForceRespawn` (fake IPC records `force=true`), `TestDaemonRecover_OwnSquatterReapThenRespawn` (injected classifier + recorded kill + fake IPC, ordering asserted), `TestDaemonRecover_ForeignSquatterRefusesExit3`, `TestDaemonRecover_SupervisorUnreachableExit5`, `TestQuarantineReasonNamesRecoverVerb` (event-body assertion; also update any test pinning the old string).

---

## 4. Adversarial self-check

**Where P1a's guard could wrongly drop a REAL current exit:**
- *Generation captured before MarkSpawned?* No — `spawnGen` is MarkSpawned's return, captured before the goroutine launches (`:3578→:3612` ordering).
- *`MarkSpawnFailed*` paths:* don't bump generation and launch no wait goroutine — no stamped exit exists to mis-drop.
- *Terminate-then-exit:* generation unchanged by `MarkTerminated` → passes. Covered by tests 4/6.
- *Tracker `Remove` mid-flight:* `Get` misses → guard passes → orphan-drop path. No drop.
- *Genuine residual:* a hand-crafted/legacy event carrying a WRONG `pid_generation` (there are none in-tree; the field is produced at exactly one site). Risk ≈ 0 with test 8 pinning generationless parity.

**Where P1b's deadline could let a genuinely-dead daemon linger:**
- Crash-before-bind: exit-driven, unaffected (wait goroutine → EvChildExit → backoff regardless of sweep).
- The real exposure is a WEDGED child (alive, never binds, never exits): detection latency rises 5s → 60s/120s, once per generation. Accepted and bounded by the 600s validation cap; `daemon-bind-timeout` makes the expiry observable. This latency is the price of not killing 46s-cold-start LSP children — the incident's trigger.
- Operator sets a huge deadline: `Validate()` caps at 600s.
- Warm-restart stale rows: old `StartedAt` → deadline already expired → immediate-first-sweep terminate preserved.

**Where P2a could reap something it shouldn't:**
- Same-user process that IS our binary with THIS task's argv but "legitimately" running: by definition of the intent contract there is exactly one legitimate holder — the tracked child; any other same-argv instance is a lost child (the class this bug files). One theoretically-legit case: an operator hand-launched `mcphub daemon --server X --daemon Y` on the same port for debugging — the reap kills it; mitigated by the audit event + this being precisely the duplicate the port contract forbids.
- Sibling daemon on a colliding port: gate 2 refuses (CurrentPID + OrphanPID).
- PID reuse between classify and kill: closed by verify-on-held-handle start-time proof (2s tolerance vs second-precision CreationDateUnix — deterministic match, test pinned).
- Attacker-shaped victims: argv is attacker-influencable, exe path (handle truth) is not without executing our binary; ACCESS_DENIED (other-user) fails closed; no privilege enablement.
- Intent-file swap on relax-lane hosts: no new capability vs existing spawn/terminate authority (documented residual, D-A).

**Race windows introduced by the fixes:**
- *Sweep reaps while loop respawns the same task:* new child binds between owner-capture and kill → start-time proof mismatches → kill aborted (`ErrProcessIdentityMismatch`) → next sweep re-evaluates. Benign.
- *Reap succeeds, own child self-recovers before EvManualRestart processes:* restart of a just-recovered child — one redundant clean cycle, serialized on the loop. Benign.
- *Bind latch vs generation:* latch keyed by generation; a respawn between sweeps invalidates the latch atomically (single sweep goroutine owns the map). No cross-goroutine access.
- *Stale-exit drop starving `reaperOutstanding`:* the drop deliberately does NOT clear the marker; the marker describes the CURRENT reaper, which posts a current-generation exit that does clear it. No strand.
- *crashCh suppression removing back-pressure signals:* stale exits were never load-bearing for anything but the bug; the abandoned-on-shutdown path is unchanged.

**Residual risk ranking:**
1. **P2a kill authority** (highest inherent risk, tightly mitigated; the reason it carries the `$security-reviewer` gate).
2. **P1b wedge-detection latency** (60–120s once per spawn for a truly-wedged child; observable via `daemon-bind-timeout`).
3. **Mismatch→observe-only behavior change** (a foreign squatter no longer triggers restart churn; the daemon sits portless-but-warned until the operator acts — strictly better than quarantine-with-masking, but it IS less "self-healing" on paper).
4. **P1a misclassification** (double-guarded, generation-monotonic; lowest).

---

## 5. Build order + review gates

| # | Item | Contents | Gate |
|---|---|---|---|
| 1 | **PR-1: P1a+P1b** | crashEvent+MarkSpawned/MarkExitedIfCurrent+wait-goroutine+bridge+controller guard; bind latch+deadline field/manifest/plumbing+sweep; events; tests 1–16 | Full commission (sonnet + opus + **fable lane**) before bot; Codex bot PASS per repo workflow; no new authority → no security gate |
| 2 | **Decisions** | Register D-A (reap authority; incl. adopt-rejected rationale) and D-B (readiness-gate, status: proposed/deferred) in `work-items/decisions/` | D-A text reviewed by `$security-engineer` before PR-2 starts |
| 3 | **PR-2: P2a** | classifier + reap closure + sweep mismatch rework + rate limit + events; sweep-test updates; tests listed §2 | **`$security-reviewer` gate mandatory** + commission + bot PASS |
| 4 | **PR-3: P2b** | `daemon recover` verb (composes §2 classifier + `DialSupervisorIPCRespawn`) + reason-string sweep | Commission + bot PASS |
| 5 | Post-merge each PR | build.sh → rename-aside deploy → full supervisor restart → `claude mcp list` verify (standing redeploy rule) | operator |

P1 ships first and alone already ends the incident class's manufacture step (no more forgotten children, no more startup kill-loop); P2a removes the last self-heal gap; P2b gives the operator the honest recovery lever the quarantine text will now name.
