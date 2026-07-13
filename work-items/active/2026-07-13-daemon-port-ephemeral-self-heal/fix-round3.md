# D round-3 fix spec — consolidated from fable (code-traced, authoritative) + Sol + Terra

Round-2 verdict: fleet-killer GONE (P1-1 self-deadlock closed, no strand, no operator-stop overwrite). Remaining = BOUNDED per-daemon bricks in narrow crash/double-fault windows + bounded loop-stalls. fable: "one more small round closes everything; fixable with small, local edits." NOT a split — fix-all.

Reviewer divergence resolved: Sol+Terra flagged a stale-snapshot lost-update P1; fable code-traced it as NOT exploitable (worker PostCtx within microseconds; a later intent mutation's watcher scan queues BEHIND evReallocApplied in the FIFO — blocked channel senders are FIFO — so an older snapshot can't overwrite a newer applied one). See FIX-4.

## FIX-1 — NEW-1 (P2): on-loop terminal emit fires the 5s foreign-holder probe ON the loop
`supervise_realloc.go:545` (`emitBindAccessDeniedTerminalOnce`) passes `foreignHolderPort=port` (>0), which fires `reallocForeignHolderFn` = netstat (2s) + WMI/PowerShell identity (3s) ON the loop goroutine — contradicting BOTH its own doc (`:534-535` "Called on the loop; no foreign-holder probe") AND the fn's doc (`realloc_foreign_holder_windows.go:17` "Runs ONLY on the off-loop worker"). Fixed-global storm (N daemons at once) serializes N×≤5s of frozen child-exit/IPC processing on the loop.
**Fix:** pass `foreignHolderPort=0` from `emitBindAccessDeniedTerminalOnce` (call sites `:178,:188`), matching its comment. The foreign-holder PID/basename is a nice-to-have in the terminal L3 event, not worth a 5s loop stall; if wanted, resolve it off-loop in the worker and carry it in the outcome body.

## FIX-2 — NEW-2 (P2): first netsh probe has no deadline, unbounded on-loop wait
`setup_ephemeral_range_windows.go:41-47`: first `ephemeralRangeContainsFn` runs `netsh` via `exec.Command` with NO deadline (once per process via sync.Once). If the first L3 event of the process is an on-loop terminal emit (fixed-global storm — likely first victim), that is an unbounded subprocess wait on the loop.
**Fix:** pre-warm `osEphemeralTCPRange()` OFF the loop during `runSupervise` wiring (the range is static per boot), so the sync.Once fires at startup off-loop; AND/OR add a context deadline (e.g. 3s) to the netsh exec as a belt. Prefer BOTH.

## FIX-3 — P1-2 slivers (P2, THE root fix): LSP registry↔argv mismatch is a dead-end exit-1 brick
Three residuals share ONE root — the LSP proxy's port-mismatch startup check (`daemon_workspace.go:192-195`) exits **1** (not exit-3), so the self-heal (keys on exit-3) never re-drives → permanent brick until manual `mcphub register`:
- (a) crash window: supervisor death between step-3 commit and step-4 (or between step-4-fail and the revert Save) → registry=newPort/intent=oldPort → LSP relaunch exit-1 forever.
- (b)(i) fail-after-publish inversion: the hardened writer can fail AFTER the rename published (`secure_write_client_config.go:71-76`, transient post-rename re-open, 3×10ms, AV-plausible) → new-port intent KEPT on disk → compensation flips registry→oldPort → OPPOSITE divergence (intent=newPort/registry=oldPort) → same exit-1 brick (revert made it worse).
- revert-also-fails residue: both stores diverge, dead-end exit-1.

**Fix (closes all three at once):** make the LSP proxy's registry↔argv port-mismatch SELF-HEALING instead of a dead-end exit-1. Options (backend + Sol decide):
- **Preferred — classify the mismatch as exit-3 (bind-refused-equivalent) when intent is the authority:** at LSP spawn, if `registry.Port != argv --port`, the descriptor was moved mid-flight; re-read supervisor-intent (the authority the loop spawns from) — if intent's descriptor Port == argv --port but registry disagrees, the registry row is stale → exit-3 so the self-heal re-drives + reconciles (the worker's AllocatePort skips the taken port, re-writes both stores consistently). Bound it to the dynamic-pool (serena/LSP) proxies so a genuine fixed-daemon mis-registration is NOT swept into a self-heal loop.
- OR reconcile row↔intent at spawn (prefer intent, rewrite the registry row under the flock before binding).
Also: fix the FALSE §E doc claim at `reallocate_dynamic_pool.go:29-34` ("the relaunch tries the old port, self-heals again" — true only for serena, false for LSP) and change the revert-failure remedy pointer from `mcphub daemon recover` (force-respawns, does NOT reconcile stores) to `mcphub register` (what the daemon's own error says).

## FIX-4 — stale-snapshot (Sol/Terra P1 vs fable "not exploitable"): verify + document or minimal guard
fable traced the apply as safe via FIFO ordering. Backend: VERIFY fable's claim concretely (blocked channel senders FIFO + the IntentWatcher scan enqueues behind evReallocApplied). If confirmed → add a load-bearing comment at the apply site (`handleReallocApplied`/`applyReapSnapshot`) documenting WHY a stale snapshot can't overwrite a newer applied one. If ANY residual doubt → add a minimal generation/revision guard: stamp the worker-read intent with its `updated_at`/mtime, reject in `handleReallocApplied` if a newer intent was already applied. Defense-in-depth satisfies all three reviewers; cheap.

## FIX-5 — P3 stale comments + docs + tests
- NEW-3: `supervisor_controller.go:2768-2769` comment claims it clears the in-flight marker "here too" — it doesn't (harmless; worker defer clears). Fix the comment.
- NEW-4: `supervise_realloc.go:391,:322` comment claims "the backstop timer re-drives the retry" for a worker-read-miss — for LSP the stale-cache retry exits 1 (not 3), so the actual rescue is the ≤60s IntentWatcher refresh. Fix the comment to name the real mechanism.
- P2-2 P3 residual (fable): setup check resolves serena pool via `serenaPortPool(nil)` (built-in default) while realloc uses the loaded manifest — an operator-customized `daemon_template.port_pool` diverges. Low-pri; note it or resolve via the manifest for symmetry.

## Tests to add (fable blind spots still open)
1. **Crash-window LSP startability (drives FIX-3):** simulate registry=newPort/intent=oldPort (and the inverse) → assert the LSP proxy self-heals (exit-3 re-drive) instead of exit-1-forever. fable: "this test would FAIL today."
2. Stale-generation exit-3 (blind spot 4).
3. Full-reallocCh drop (blind spot 5).
4. Malformed outcome-body decode (blind spot 7).
Non-vacuity: for each new test, neuter the fix → prove it fails → restore.

## Not to re-touch (held under attack across both rounds)
classification boundary; cap accounting; dwell gate (+ startReallocDwell); never-kill-foreign; zero client-config churn; stale-generation guard; single-worker+flock+row-skip+OS-probe double-alloc prevention; lock order registry→intent; the CLOSED P2-1/P2-2/P2-3 + the P1-1 loop-safety.

## After round-3: re-run the finders (fable + Sol + Terra) on the round-3 delta → if PASS → commit → bot → merge → deploy.
