# Item 3 DESIGN v3.1 (design-B) — GUI self-restart and port change

Design package by `$architect`. This revision implements the accepted decision
`work-items/decisions/2026-07-17-item3-unitb-recovery-simplify.md`: keep the restartable listener,
authenticated confirm-then-release standby, parent pre-release rollback, and reservation/Held mapping; delete
the fully automatic post-release recovery graph. V3.1 also incorporates the accepted two-lane design-gate
findings: extend the one existing hub restart owner to cover an initial bind failure from a nil component, close
the parent's own hub listener at flock release, make failed rollback free the flock and exit, distinguish
free-flock from live-holder recovery, and replace the unsafe ensure-alive relaunch with one degrade-only
predicate. The accepted factual source remains `item3-restart-recon.md`; the live design baseline is
`master @ b18ed154`, plus the accepted decision at `47a45076` and the cited current-tree checks in §1.

The simplification is structural. A durable record cannot serialize OS process termination, socket bind, or
file-lock side effects. V3.1 therefore uses the record only where it can make a truthful decision: reserving the
healthy release-to-acquire gap and recording a proved-free interruption. Once the parent releases the GUI
flock, it never arbitrates, terminates, reclaims, or advances the child. The child activates immediately after
acquiring the flock. Ambiguous crash-during-handoff outcomes fail loudly to one of two exact operator commands,
selected only by the flock discriminator.

No implementation code is specified below. Code-like text is limited to signatures and protocol pseudocode.

---

## 0. Change-Surface Contract

**Architect-owned field; the planner and implementers consume it, may return `REVISE` on conflict, and may
not redefine it.**

- **Intended change surface:**
  - `internal/gui/server.go` — REQUIRED. Split the currently coupled `Server.Start` lifetime so the GUI HTTP
    listener can close/rebind without ending the hub, event bus, or process runtime. The current `Server` still
    owns the GUI and hub lifecycles together (`server.go:509-625,980-1205`). Route a gate-on initial hub-start
    error at `server.go:1064-1076` into the existing hub restart driver instead of terminal `HubHealthDown`, and
    close the parent's own hub listener through that existing owner immediately before flock release.
  - `internal/gui/hub_listener.go` — BOUNDED ADDITIVE AMENDMENT. At the existing
    `restartHubListenerWithOutcome` nil-component guard (`hub_listener.go:265-269`), admit only a typed
    `initial-bind-failed` request from `Server.Start`; skip old-component swap/shutdown and enter the existing
    start/backoff loop. The same driver, rolling window, consecutive-attempt cap, same-port backoff, exhaustion,
    abandonment, events, and `hubHealthTracker` remain unchanged. Every other nil-component entry still
    stop-drives.
  - New `internal/gui/gui_listener_lifecycle.go` — `GUIListenerOwner`, standby/full/grace request modes,
    listener close, and owned rebind.
  - `internal/gui/gui_self_restart.go` plus new `internal/gui/gui_restart_protocol.go` — parent coordinator,
    child standby, retained child process handle, authenticated readiness, bounded pre-release rollback, coarse
    progress, and the post-release no-op boundary.
  - New `internal/gui/gui_restart_record.go` — one small marker owner for
    `{in-progress,reserved,committed,interrupted}`, reservation identity, freshness, and an owned-free-probe
    interruption compare-and-swap. It does not implement a general recovery graph or relaunch state.
  - `internal/gui/single_instance.go` — reservation-aware flock acquisition and typed
    `Held | Free(owned_probe_lease) | Unknown` probing.
  - `internal/gui/ping.go` — additive challenged standby readiness fields; existing public ping fields remain
    unchanged.
  - `internal/cli/gui.go` — inject the existing GUI lease, build standby before full activation, gate mutable
    child work until flock acquisition, reconstruct restart argv through the parser-aware owner, and keep normal
    manual launch precedence.
  - New `internal/api/hub_port_dependencies.go`, `internal/cli/gui.go`, and `internal/gui/server.go` — Unit A's
    independent typed dependency probe and its two fail-closed consumers. Unit A remains unchanged and ships
    first.
  - `internal/cli/supervise_ensure_alive.go` — additive tri-state GUI-owner probe, the single degrade-only
    predicate, and distinct free-flock/live-holder operator discriminators. It never spawns a GUI. Existing
    supervisor recovery remains separate.
  - `internal/gui/frontend/src/api.ts`,
    `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx`, and one restart-progress consumer —
    preserve the 202/2xx spawn contract, show coarse progress, attempt best-effort navigation, and show the
    exact discriminator-selected manual recovery guidance.
  - `internal/gui/events.go` only if the coarse progress event needs a registered identifier; reuse the existing
    broadcaster and `/api/events` stream.
  - Focused tests adjacent to these owners. No 43-edge record matrix, post-release-kill matrix, or watchdog
    timing matrix remains.
- **Approved extension seams:**
  - `GUIListenerOwner` is the only component allowed to bind, close, swap, or rebind the GUI HTTP listener.
  - `HandoffMarkerStore` is the only component allowed to create/replace the coarse marker or evaluate its
    reservation.
  - `SingleInstanceLease` is the only component allowed to release, acquire, or transfer the GUI flock.
  - `SpawnedGUIChild` retains the exact OS process handle and is the only termination/wait seam, and only before
    the parent releases the flock.
  - `resolveGuiPort` plus its typed persisted-value helper is the only port-validity and precedence seam.
  - `ProbeHubPortDependencies` is the only Unit A reset-safety predicate.
  - The existing `runHubListenerRestartDriver` plus `hubHealthTracker` is the only hub recovery owner. The
    initial-bind request is a new entry cause, not a second owner, protocol phase, or child-sequencing gate.
  - The existing SSE broadcaster is the only browser progress channel.
- **Protected / must-not-touch surfaces:**
  - `internal/gui/hub_listener.go` outside the exact initial-bind/nil-component driver amendment above;
    `api.BindHubMcpListener`; endpoint/token/instance-id formats; hub-session semantics; and the hub-health state
    machine. Initial failure now enters that state machine's existing bounded recovery path; no retry policy,
    bind transaction, or second hub recovery owner changes.
  - `internal/api/hub_mcp_groups.go` parsing and persistence semantics. `LoadGroups` remains the group probe;
    its errors remain errors.
  - Daemon/supervisor protocols, manifest, secrets, client reconcile, scheduler task format, and force-kill
    identity gates.
  - `internal/autostart/windows.go`; it remains evidence only and launches `gui` without `--port`.
  - Existing `/api/ping` fields `{ok,pid,version}` and existing non-restart callers.
  - All post-release recovery machinery is prohibited: no `ClaimRecovery`, claimant record, activation signal,
    child self-advance, parent reclaim, post-release child termination, or fallback recovery listener.
- **Declared blast radius:**
  - Unit B changes GUI-listener lifetime, single-instance handoff, restart response/progress, desired-port argv,
    the narrowly gated ensure-alive degrade branch, parent-owned hub teardown ordering at flock release, and the
    existing hub restart driver's nil-component entry for one initial-bind cause.
  - Unit A changes only the two named hub-port reset decisions.
  - No daemon lifecycle, hub routing, token, manifest, group schema, scheduler task, or client-reconcile change.
  - The listener seam is the only cross-cutting internal extraction. The recovery simplification reduces the
    protocol surface from 13 phases and 43 legal edges to four coarse marker phases and one degrade predicate;
    ensure-alive has no GUI spawn path.
- **Shared mutable-state ownership and committed events:**
  - Listener mode and request admission: exactly one writer-owner, `GUIListenerOwner`; downstream settled event,
    `gui-restart-progress{phase:"listener-settled"}`.
  - Coarse marker and reservation: exactly one writer-owner, `HandoffMarkerStore`; downstream committed event,
    durable marker `committed` or `interrupted`.
  - Single-instance ownership: exactly one writer-owner, `SingleInstanceLease`; downstream observable event,
    `gui-restart-lock-acquired` emitted by the child immediately before activation.
  - Browser-visible pre-release progress: exactly one writer-owner, parent `RestartCoordinator`; downstream
    event, `gui-restart-progress`. After flock release the parent publishes no child verdict.
  - Hub listener recovery: exactly one writer-owner, the existing `runHubListenerRestartDriver` over
    `Server.hubMcpComp`; downstream observable events remain the registered `hub-listener-restart-*` events and
    `hubHealthTracker` state changes.

## 1. Re-verified constraints that drive v3.1

1. The GUI listener is not independently restartable today. `Server.Start` binds and serves it, starts the hub
   lifecycle, and drains hub/events when the GUI listener returns (`server.go:980-1205`). The current CLI
   composition owns the flock outside that monolith.
2. The current self-restart reports spawn success, schedules `os.Exit`, and reduces the spawned child to a PID
   while a background goroutine waits (`gui_self_restart.go:98-201`). V3.1 retains the exact OS process handle
   until the only safe termination boundary ends.
3. The child must acquire the GUI flock before it can become the full owner. V1's “confirm full child, then
   release flock” therefore circularly waited. V3.1 confirms a bounded standby listener before release.
4. Current `/api/ping` proves only a caller-provided PID. A retained process handle plus a one-use nonce is
   required to confirm and, before release only, terminate the exact spawned child.
5. A record compare-and-swap cannot serialize OS kills, binds, and flocks. The accepted decision records three
   relocations of the same no-owner freeze and a fourth named race. V3.1 does not attempt to repair that mismatch
   with another record edge.
6. The current initial hub-start path does **not** recover a never-bound listener: a gate-on error becomes
   terminal `HubHealthDown` (`server.go:1064-1076`), and the existing restart driver stop-drives when
   `hubMcpComp` is nil (`hub_listener.go:265-269`). The bind transaction uses an exclusive listener
   (`internal/api/hub_mcp_bind.go:149-160`, `internal/api/hub_mcp_listener_windows.go:75-93`), so a child start
   can contend with a parent's live socket. V3.1 routes only that initial failure into the existing driver and
   closes the parent's own hub listener at flock release; neither amendment sequences child GUI activation.
7. A raw `reserved` marker is necessary because the healthy parent-release to designated-child-acquire gap can
   expose a momentarily free OS flock. Treating that moment as dead would launch a third GUI.
8. `runEnsureAlive` currently conflates some path/read failures with “not alive” and returns early while the
   supervisor is live. The Unit B branch must be independent, tri-state, record-gated, and fail closed.
9. The frontend depends on `restarting`; spawn failure is deliberately a 2xx body. Accepted work remains HTTP
   202 and spawn failure remains 200/2xx so the friendly UI branch is reachable.
10. Only long `port` is registered. Autostart launches `gui` without an explicit port.
11. A live GUI process can still wedge while holding the single-instance flock. That is the pre-existing
    single-instance baseline, not the deleted no-owner relocation class: a proved live holder requires the
    existing identity-gated `mcphub gui --force --kill`, while only a proved-free flock can advertise plain
    `mcphub gui`.

## 2. Chosen architecture

### Parent-supervised confirm-then-release, with no parent recovery after release

The running GUI coordinates only the portion it can prove safely. It spawns a minimal **STANDBY** child through
a retained OS process handle and transfers a 256-bit nonce through an owner-only inherited handle/file, never
through argv or environment. The child binds the target GUI port in standby, serves only authenticated readiness,
and performs no mutable-runtime side effect. The parent confirms the exact child, quiesces or closes its own GUI
listener as required, publishes `reserved` while still holding the flock, closes its own hub listener through
the existing bounded hub owner, and releases the flock immediately afterward. This is own-resource ordering:
the child receives no hub signal and waits only for normal flock acquisition.

That release is the architectural boundary. After it, the parent never writes a child phase, waits for child
activation, claims recovery, terminates the child, reacquires the flock, or decides a winner. It may finish a
fixed old-port grace interval and ordinary process cleanup, but those actions are not protocol gates and never
condition child progress.

The designated child acquires the reserved flock, writes pidport, and **activates immediately**. It does not
wait for hub release or a parent signal. Full GUI activation and the existing hub lifecycle remain separate:
the GUI becomes reachable, while an initial hub bind failure is enqueued into the one existing restart driver,
which retries from nil with its unchanged backoff/window/exhaustion policy after the old socket is closed.

### Deliberate recovery floor

There is no automatic GUI relaunch. After a nonterminal phase deadline, ensure-alive only classifies the flock:
an actually acquired free probe lease permits one compare-and-swap to `interrupted`, after which plain
`mcphub gui` is truthful; a proved held flock permits no marker write and surfaces the identity-gated
`mcphub gui --force --kill` baseline. `Unknown` remains fail-closed and is not mislabeled as either case. The
two exact messages and discriminators are defined in §4, §9, and §12.

### Single feature gate

`gui.RestartV3Enabled()` is the one internal capability owner, resolved at the CLI composition root and consumed
by the endpoint and ensure-alive branch. Disabled means fully inert: endpoint 503, no child, no marker, no
reservation, no recovery branch, and frontend manual guidance. The unsafe v1 spawn-and-exit path is not a
fallback.

## 3. Port authority and parser-aware child argv

The desired/actual model remains:

| Layer | Authority | Owner |
| --- | --- | --- |
| Persisted `gui_server.port` | Desired operator intent when valid | Settings registry |
| Bound listener port | Actual runtime fact | `GUIListenerOwner` |
| `gui.pidport` | Derived rendezvous cache | Current flock holder |

Manual launch remains `explicit --port -> valid persisted -> 0`. Self-restart changes only inherited argv: a
**valid** persisted value wins and every recognized pre-terminator `--port` occurrence is removed. Unset or
invalid persisted state is “no valid intent”: inherited explicit `--port`, including `0`, is preserved. Invalid
persisted state emits a visible warning; with no explicit flag it degrades to ephemeral `0` rather than
pretending the invalid value was honored.

One typed helper under `resolveGuiPort` owns the sole `[1024,65535]` predicate and returns
`Unset | Valid(port) | Invalid(raw,reason)`. The argv builder consumes that classification; it does not retype
the range.

`RebuildSelfRestartArgv` uses the GUI `pflag.FlagSet` metadata to identify effective token spans, respects `--`,
and recognizes only the registered long name `port`. It is not a raw string replacement.

| Parent argv shape | Valid persisted intent | Unset/invalid persisted intent |
| --- | --- | --- |
| `--port N` | Remove flag and value | Preserve both tokens |
| `--port=N` | Remove token | Preserve token |
| `--port 0` | Remove flag and value | Preserve explicit `0` |
| Repeated `--port` forms | Remove every recognized occurrence | Preserve all; parser last-wins remains |
| `-port` | Reject unregistered shorthand before spawn | Same rejection |
| Tokens after `--` | Preserve as positional | Preserve as positional |
| No `--port` | No argv change | No change; invalid persisted warns and resolves to `0` |

Decision details remain in `work-items/decisions/2026-07-17-gui-server-port-authority-model.md`.

## 4. Confirm-then-release handoff

### Port-change path (`N != P`)

```text
parent holds flock + full GUI(P) + hub
  -> marker in-progress; spawn retained child handle with owner-only nonce
child binds N; serves STANDBY authenticated ping; parent confirms exact child
  -> parent enters GRACE(P); new mutators 503; admitted mutators drain
parent writes reserved while still holding flock
  -> bounded/non-blocking target-port progress flush
  -> close PARENT'S OWN hub listener through the existing hub owner (bounded 5 s, force-close on expiry)
  -> RELEASE flock immediately; the child waits on no hub signal
  -> protocol duty ends; no parent child-phase write, wait, claim, kill, or reclaim
child acquires matching reservation; writes pidport
  -> ACTIVATE IMMEDIATELY: full GUI(N) becomes reachable
  -> existing hub owner attempts desired bind independently
  -> initial failure enters that same owner's retry-from-nil path; healthy/recovering/down is honest
  -> child writes committed
parent finishes fixed GRACE(P), releases only its remaining GUI/process-local resources,
  and exits through the self-restart path without stopping the adopted supervisor fleet
  and without observing child outcome
```

The old-port grace bridge is delivery help, not recovery authority. It may serve SSE and the fixed redirect
route for `G`, but it neither waits for `committed` nor changes the marker after release.

### Same-port path (`N == P`)

```text
parent holds flock + full GUI(P) + hub
  -> marker in-progress; spawn retained child with owner-only nonce
child prepares minimal standby and supplies authenticated poised proof
parent closes new mutable admissions; admitted mutators drain
parent closes ONLY GUI listener P; flock, hub, events, and process remain owned
child binds P in STANDBY; parent confirms exact child before the bind deadline
parent writes reserved while still holding flock
  -> close PARENT'S OWN hub listener through the existing bounded hub owner
  -> RELEASE flock immediately; protocol duty ends; no child hub signal exists
child acquires matching reservation; writes pidport; ACTIVATE IMMEDIATELY
  -> full GUI(P), then independent existing hub lifecycle with retry-from-nil on initial failure; write committed
parent releases only its remaining GUI/process-local resources and exits through the
self-restart path without stopping the adopted supervisor fleet; browser reconnects
```

### Parent pre-release rollback — the only automatic rollback

The rollback gate is the concrete fact `parentLeaseReleased == false`, never an inferred marker phase. On child
bind/auth failure, confirmation timeout, quiesce failure, or marker-write failure:

1. Retain the already-owned `SingleInstanceLease`; never reacquire it.
2. Terminate only the exact authenticated child through the retained OS process handle. A child that never
   authenticated remains flockless and closes its own bounded standby on timeout.
3. For same-port, call `GUIListenerOwner.BindForRecovery(P)` while reaping the exact child; for port-change,
   swap the still-owned grace listener back to full.
4. Restore full admission within the rollback budget. Once restoration and exact exit are proved, clear the
   non-reserved marker and report restart failure while the original GUI remains healthy.
5. If safe restoration cannot be proved, write `interrupted` with the §12 reason, surface the exact operator
   instruction, and enter the terminal rollback-failure branch below. No second automatic mechanism is entered.

**Normative rollback-failure terminal branch:** after the durable `interrupted` write, the parent spends only
the remaining rollback budget on exact-child cleanup and bounded shutdown of its own hub listener, then releases
the retained `SingleInstanceLease` and exits unconditionally. An authenticated child may be terminated through
the retained handle; an unauthenticated child remains flockless and is left only to its already-bounded standby
timeout. Cleanup failure is logged but cannot retain the flock or keep the crippled parent alive. If the
`interrupted` write itself fails, emit `gui-restart-interrupted-marker-write-failed` and still perform the same
bounded cleanup, release, and exit. This branch fails loud and leaves the flock free, so plain `mcphub gui` is a
genuine recovery rather than a bounce to the crippled parent.

### Post-release rule

After the release call returns, the parent:

- does not call `Terminate`, `Wait` as a protocol gate, `ClaimRecovery`, lease reacquire, or
  `BindForRecovery`;
- does not publish `hub-released`, signal activation, advance a suffix, or decide between commit and abort;
- does not condition ordinary cleanup or the fixed grace deadline on child state;
- has already closed its own hub listener at the release boundary and may only close its retained process handle
  without terminating the child, finish bounded GUI grace, release its remaining resources, and exit;
- must not run the normal GUI-return `manager.Stop` path that tears down the adopted supervisor/daemon fleet;
  successful handoff uses the existing self-restart-specific process-exit boundary after its owned cleanup.

The child has no activation-wait state. Its successful flock acquisition directly calls the single activation
barrier. This removes the parent-death, live-parent-wedge, claimant-death, claim-expiry, and in-flight-terminate
arbitration surfaces instead of guarding them.

### Minimal deadlines

All values belong to one injected `RestartDeadlines` policy; tests use a fake clock.

| Deadline | Production value | Expiry action |
| --- | ---: | --- |
| Authenticated poised proof / flockless standby | 10 s | Parent stays full; unauthenticated child closes and exits |
| Port-change bind/ping confirmation | 2 s | Parent stays full; terminate exact authenticated child; clear marker |
| Same-port bind/ping after P closes | 2 s from close | Retain flock; run pre-release rebind rollback |
| Mutable-request quiesce | 5 s | Restore full admission; terminate exact authenticated child |
| Reservation protection window | 10 s from durable `Reserve` | Covers the 5 s parent hub-shutdown cap plus 5 s post-release acquisition; before expiry raw `reserved` maps Held, after expiry the predicate only classifies Free/Held and never spawns |
| Parent hub-listener teardown at release | Existing 5 s shutdown cap | Force-close on expiry, then release flock; never wait for child hub state |
| Pre-release exact-child reap plus P rebind | 5 s | Write/log `interrupted`, finish bounded cleanup, release flock, and exit |
| Old-port grace `G` | 5 s after release | Close old GUI listener and exit regardless of child outcome; hub is already closed |
| Marker freshness | 3 min from generation start | Never auto-relaunch; classify expired nonterminal Free/Held for exact guidance |

There is no 10-second parent decision cutoff, 30-second child signal wait, 65-second admission arithmetic,
post-release reacquire deadline, absolute-expiry transition, or recovery-surface lifetime.

### Held-flock nonterminal work is bounded

There are only three held-flock nonterminal sections in v3.1:

1. **Parent preparation before `reserved`:** authenticated proof, target bind confirmation, mutator quiesce,
   and same-port rebind rollback use the 10 s, 2 s, 5 s, and 5 s deadlines above. Record writes are single
   durable owner operations. Browser navigation, old-port grace, child activation, and hub-health convergence
   are not awaited here.
2. **Parent `reserved`-to-release boundary:** the progress publication is bounded/non-blocking, the parent's own
   hub teardown uses the existing 5 s cap and force-close fallback, and flock release follows immediately. No
   parent signal, child status, hub bind result, or grace completion gates release.
3. **Ensure-alive owned-Free probe:** no `reserved -> in-progress` spawn executor exists. The probe lease permits
   only one generation/sequence-checked `interrupted` write and is then released on success, error,
   cancellation, or timeout; it is never transferred to a child.

The remaining way a nonterminal handoff can stay `Held` past its deadline is that the GUI process itself or an
OS/filesystem call inside it has wedged while retaining the flock. That is the irreducible pre-existing
single-instance-holder baseline. No claimant, parent signal, child self-advance, competing writer, or
post-release termination is added to mask it.

### What the operator sees in a degraded case

The supervisor/ensure-alive log, the next CLI invocation, and any surviving grace UI select exactly one message
from the flock discriminator:

- **Free flock:** after ensure-alive actually acquires the probe lease and writes `interrupted`, or after
  rollback failure releases and exits, show exactly:

  > GUI restart interrupted; run `mcphub gui`.

  The command acquires the now-free normal flock, starts a new generation, and restores the full GUI.
- **Live-wedged holder:** when a nonterminal phase is past its applicable deadline and the probe returns
  `Held`, show exactly:

  > GUI restart interrupted: a GUI process still holds the single-instance lock; run `mcphub gui --force --kill`.

  This is the existing identity-gated single-instance recovery: it verifies and terminates the live holder,
  then performs the normal launch path. Plain `mcphub gui` is deliberately not advertised because it would only
  report/bounce to that holder.

`Unknown` is neither message: it emits `gui-restart-owner-unknown`, mutates no marker, and remains fail-closed
until the lock/path/DACL uncertainty is resolved. Neither degraded case requires record surgery, a fallback
port, or an automatic takeover.

## 5. Coarse marker and reservation/Held rule

### One small record owner

`HandoffMarkerStore` owns `<state-dir>/gui-restart.json` and its private record lock. It exposes intent-specific
operations, not a generic legal-edge engine:

```text
Begin(generation, route, old_port, new_port, freshness_deadline) -> in-progress
Reserve(generation, expected_sequence, reservation_deadline, designated_child_hash) -> reserved
Commit(generation, owner_lease, bound_port) -> committed
Interrupt(generation, reason_code, operator_action) -> interrupted
InterruptFromOwnedFreeProbe(generation, expected_sequence, owned_probe_lease,
                            reason_code, operator_action) -> interrupted
ClearAfterProvedPreReleaseRollback(generation, owner_lease) -> absent
```

`Reserve` and `InterruptFromOwnedFreeProbe` require generation+sequence comparison because they guard the
release gap without overwriting a changed generation. `Begin` is parent-only while it holds the flock. `Commit`
is child-only while it holds the flock. `Interrupt` is written by the current pre-release parent;
`InterruptFromOwnedFreeProbe` is written only by ensure-alive while it holds the acquired free probe lease. A
non-owner observing `Held` cannot write the marker. This ownership removes the need for a general transition
graph and gives ensure-alive no spawn/lease-transfer operation.

Record v3.1 fields are deliberately small:

| Field | Purpose |
| --- | --- |
| `version`, `generation`, `sequence` | Schema, generation identity, and compare-and-swap interruption |
| `phase` | One of the four coarse decisions below |
| `route`, `old_port`, `new_port` | Same-port/port-change operation and operator diagnostics |
| `old_pid`, `child_pid` | Diagnostics only; never confirmation or kill authority |
| `designated_child_hash` | Reservation match without persisting the nonce |
| `created_at`, `updated_at`, `fresh_until` | Injected-clock validity; no expiry phase |
| `reservation_expires_at` | Parent hub-shutdown cap plus healthy post-release acquisition protection window |
| `reason_code`, `operator_action` | §12 discriminator and exact manual guidance |

There are no claim IDs, claim deadlines, fallback ports, activation-signal fields, hub-release fields, or
phase-suffix cursors.

**"State-dir-matching" (§9 line ~681, §12 "state-dir-mismatched") means LOCATION-binding, not an embedded
origin field.** There is deliberately no `state_dir`/origin field in the v3.1 schema: the store binds to
one absolute `<state-dir>/gui-restart.json` and never searches or falls back to another root, so the
record any operation consults IS, by construction, the one at the caller's own resolved state dir; and
provenance/wrong-owner planting is owned uniformly by the hardened owner-only DACL pipeline (the same
posture as `supervisor-intent.json`), NOT by a per-marker field. An embedded origin field was rejected —
it would be forgeable by anyone who can plant the file (so it does not defend the plant threat), would
add Windows path-normalization fail-closed hazards, and would violate the closed field list above. The
residual (a valid marker byte-copied/backup-restored/planted at the exact path) is benign: the marker
carries no kill/spawn authority (`old_pid`/`child_pid` are diagnostics only), ensure-alive's relaunch/
degrade predicate is AND-gated on the REAL OS flock (a planted JSON cannot free it; spawn count stays 0),
and `operator_action` is enum-mapped to fixed literals, never an arbitrary persisted command — worst case
is a ≤`reservation` reject or a benign "run `mcphub gui`" message. (Recorded from the 2026-07-17
fable/Sol split + opus tie-break; `RESOLUTION: PATH-OWNED-CORRECT`.)

### Minimal phase set

| Phase | The one distinct decision it gates |
| --- | --- |
| `in-progress` | The parent is preparing a handoff; automatic relaunch is forbidden |
| `reserved` | Parent has committed to flock release; the designated child is protected during the window, after which only Free/Held degradation is permitted |
| `committed` | A replacement owns the flock and full GUI is reachable; suppress all recovery |
| `interrupted` | The current owner failed pre-release or ensure-alive proved the flock free; stop automation and require plain `mcphub gui` |

Any proposed fifth phase must gate a decision not expressible by the owner lease, process handle, listener
ownership, reservation deadline, or §12 reason. `poised`, `parent-quiesced`, `parent-listener-released`,
`child-lock-owned`, `hub-released`, `activating`, `aborted`, `recovery-failed`, `shutdown-intent`, and `expired`
do not meet that test and are removed.

The only normal marker shapes are:

```text
absent -> in-progress -> reserved -> committed
                 |          |
                 +----------+-> interrupted   // only on proved failure/ambiguity
reserved -------------------> interrupted      // ensure-alive owns Free; no spawn/relaunch
```

Successful pre-release rollback clears `in-progress` after restoring the original full GUI; it does not mint
an `aborted` phase. An explicit operator launch after `interrupted` starts a new generation rather than
advancing the interrupted generation.

### Reservation-aware acquisition

Every entrant that tentatively acquires the flock reads the marker while still holding that tentative lease:

1. A fresh unexpired `reserved` marker rejects every ordinary/third entrant and returns
   `ErrHandoffReserved` after releasing the tentative lease.
2. The designated child may retain the lease only when its owner-only nonce hashes to the reservation. It then
   writes pidport and activates immediately; it does not need a record phase advance first.
3. Ensure-alive maps raw `reserved` to `Held` throughout the reservation window even if the OS flock is
   momentarily free.
4. After the reservation window, ensure-alive may retain a proved-free probe lease only as part of the exact §9
   predicate. It may compare-and-swap `reserved -> interrupted`, release the lease, and emit the free-flock
   message; it never spawns or transfers the lease.
5. Unknown path, marker, DACL, or flock state releases any tentative lease and returns `Unknown`; it never means
   dead.

If a designated child is alive but stalled in standby when ensure-alive owns the free lease and changes its
matching `reserved` marker to `interrupted`, the child cannot acquire during the write. After release it finds no
matching reservation, releases any tentative lease, closes standby, and exits. Thus the degrade action fences a
late surviving child without adding a takeover or closure protocol.

An explicit operator launch is not ensure-alive recovery. After the healthy reservation window it may acquire
the normal flock; when it encounters a stale/`interrupted` marker it starts a new generation and reports the
prior reason. It never kills a holder based on the marker.

### Crash analysis — reservation/Held only

| Crash point | Durable/OS observation | Required verdict |
| --- | --- | --- |
| Parent dies before `reserved` | `in-progress`; flock becomes Free | Ambiguous pre-release state: write `interrupted`, show manual command, no auto relaunch |
| Parent writes `reserved`, dies before release | `reserved`; flock Held until OS cleanup | Suppress inside the window; after the deadline, a still-Held live process gets the force-kill message, while OS cleanup to Free permits `interrupted` + plain launch |
| Parent releases; child has not acquired | `reserved`; flock briefly Free inside reservation window | Map to Held; reject third entrant and suppress ensure-alive |
| Child acquires and remains alive | `reserved` or `committed`; flock Held | Suppress while within deadline or committed; stale nonterminal Held emits the force-kill message |
| Child acquires then dies before `committed` | `reserved`; flock becomes Free | After reservation expiry, §9 writes `interrupted`, releases the probe, and advertises plain launch; no relaunch |
| Marker reserve write fails | parent still holds flock | Pre-release rollback; never expose an unreserved free gap |

This is the complete crash analysis for the record. There is no whole-protocol crash matrix because the marker
does not claim to order OS process, bind, hub, or termination side effects.

### Why the relocation class is unreachable by construction

| V2.2 relocation surface | V3 construction that removes it |
| --- | --- |
| Recovery-claimant death | No `ClaimRecovery`, claim credential, claimant lease, or claimant timeout exists |
| Live-parent post-lock wedge | The child waits on no parent signal and activates immediately after flock acquisition |
| Parent-death suffix repair | The child has no phase suffix to self-advance; flock acquisition directly enters the one activation path |
| Parent/child CAS winner race | The parent makes no marker decision after release; there are no competing post-release writers |
| Claim-expiry versus in-flight `Terminate` | No post-release termination API or claim expiry exists; the retained handle is detached at release |
| Hub-release sequencing race | Parent hub close is own-resource ordering immediately before release; the child waits only on flock acquisition, and any residual initial bind failure enters the existing retry-from-nil owner |

These are unreachable because the required actor, wait, edge, or operation is absent—not because another
timeout or guard attempts to win the same race.

## 6. Component contracts and dependency direction

The stable dependency direction is CLI composition → GUI lifecycle contracts → existing hub/API primitives.
No reusable GUI component imports CLI-private code.

### `GUIListenerOwner`

```text
BindStandby(port, readiness) -> BoundListener
ServeFull(bound, fullHandler) -> ModeSettled
EnterGrace(graceHandler) -> ModeSettled
CloseListener(deadline) -> Closed
AdoptAndServe(boundListener, handlerMode) -> ModeSettled
BindForRecovery(port, deadline) -> BoundListener
Shutdown(deadline) -> result
```

- Owns the `net.Listener`, `http.Server`, and one per-request handler-mode gate.
- Mode swap rejects new non-allowed requests immediately and then drains already-admitted mutators within the
  quiesce deadline.
- Existing SSE may survive full→grace. Listener close is independent of hub/event/runtime shutdown.
- `BindForRecovery` returns an already-bound exclusive listener; it is callable only while the parent still
  owns the GUI flock.

### `GUIOwnerLifecycle`

- Owns `GUIListenerOwner`, injected `SingleInstanceLease`, existing hub components, and orderly cleanup.
- Normal launch starts full immediately.
- Handoff child starts standby and gates hub, tray, browser, pollers, supervisor adoption/spawn, and mutable
  background work behind **flock acquisition**, not a parent activation signal.
- After acquiring the flock, the child opens full GUI service immediately and starts the desired hub lifecycle
  through its existing owner. An initial hub bind failure is recovering health owned by the existing restart
  driver, not failed GUI activation or a reason to wait on the parent.
- During healthy handoff the parent closes its own hub component with the existing bounded shutdown operation
  immediately before `SingleInstanceLease.Release`; after release the old parent has no hub component left to
  close during grace.

### Existing hub restart owner — bounded v3.1 amendment

```text
RequestHubRestart(cause: unresponsive | initial-bind-failed)
```

- `runHubListenerRestartDriver` and `hubHealthTracker` remain the single recovery/state owner.
- `Server.Start` changes only the gate-on initial-error branch: replace the stale “gate-OFF for this process”
  terminal log with a retry-scheduled diagnostic, retain `HubHealthRecovering`, and enqueue
  `initial-bind-failed` on the existing buffered channel. The once-started driver consumes that request; the
  branch no longer terminates the never-bound case as `HubHealthDown` before the retry policy runs.
- For `initial-bind-failed` with `hubMcpComp == nil`, `restartHubListenerWithOutcome` skips old-listener swap and
  shutdown, then enters its existing start loop. A nil component for every other cause still stop-drives.
- Base/max backoff, rolling-window attempt cap, consecutive restart cap, same-port wait, exhaustion/abandon
  outcomes, event identifiers, and health transitions are unchanged. Exhaustion may still end in honest
  `HubHealthDown`; the amendment only makes the existing recovery owner run first.
- This owner receives no child readiness signal and exposes no handoff phase. Parent hub close at flock release
  is a caller-owned resource-lifetime action, not a restart-driver decision.

### `SpawnedGUIChild`

```text
PID() -> int                  // diagnostic only
WaitBeforeRelease(deadline) -> ExitResult
TerminateBeforeRelease(auth, deadline) -> result
DetachAtRelease() -> result
Close() -> result
```

- The coordinator retains the exact OS process handle until release or proved pre-release rollback.
- No termination is addressed by a bare PID.
- Termination is legal only after valid nonce proof and only while the parent still owns the flock.
- At flock release, the parent irrevocably detaches/close-releases its local handle without waiting for child
  completion. No post-release termination API exists.
- Before flock acquisition, the child monitors the inherited parent process handle and its standby deadline. A
  parent death without a matching `reserved` marker, or deadline expiry, closes standby and exits; the child
  never improvises ownership from `in-progress`.

### `AuthenticatedReadinessSession`

- Parent generates a 256-bit nonce and transfers it through a one-shot owner-only inherited OS handle/file.
  The nonce is never placed in argv, environment, logs, or durable raw state.
- Child consumes and closes the channel before serving standby.
- Readiness binds `{handoff_id,generation,sequence,pid,port}` to a message authentication code.
- Parent accepts one valid proof and binds it to the retained process handle. Challenged standby ping must match
  PID, generation, target port, and proof; ordinary ping remains byte-compatible.
- Port-change child binds standby before proof. Same-port child supplies a pre-bind poised proof, then supplies
  bound proof after the parent closes P. Neither route performs full-runtime side effects before the flock.
- The child requests the flock only against its matching fresh `reserved` marker. Parent-handle death before
  reservation is an exit signal, not permission to acquire.
- A changed/nonmatching phase, including ensure-alive's `interrupted`, makes a late standby child release any
  tentative lease, close standby, and exit; it never turns that mismatch into ownership.

## 7. Fail-closed hub-port dependency guard (Unit A)

Unit A is unchanged, independent, and ships first. One typed probe replaces boolean/list-only decisions:

```text
ProbeHubPortDependencies() -> {
  gated_clients,
  groups,
  state: clear | dependent | unknown,
  errors[]
}
```

- `dependent` when at least one enabled aggregate client or group exists.
- `clear` only when every applicable client is proved gate-OFF and `groups.yaml` is missing or valid-empty.
- `unknown` on any client config read/parse/DACL error, group read/parse/DACL/validation error, or probe path
  failure.

Both destructive reset callers fail closed:

- `mcphub gui --reset-port` refuses with exit 8 on `dependent` and `unknown`, naming proved dependencies and
  separately listing unreadable sources.
- Initial hub startup in `internal/gui/server.go` sets
  `preservePortOnReloadHandlerFailure=true` unless the typed result is exactly `clear`. The reset mechanism in
  `hub_listener.go` remains unchanged; the composition caller supplies the safe policy.

This preserves live client/group URLs or blocks when safety cannot be proved. No automatic rewrite of
hand-pasted group URLs is introduced.

## 8. External API, grace handler, and frontend behavior

### Restart response — unchanged expand-contract

Accepted restart returns HTTP 202 while retaining current fields:

```text
{handoff_id, generation, phase:"in-progress", spawned:true,
 spawned_pid, restarting:true, old_port, target_port?}
```

Spawn failure returns **HTTP 200 (2xx) with a non-restarting body**
`{spawned:false,spawned_pid:0,restarting:false,spawn_error}`. It must not return non-2xx: the current client
throws on non-2xx before it can render the friendly `Restart incomplete` body. `202` means accepted, not
completed. Keeping `restarting` preserves existing frontend behavior.

### Coarse progress event

`gui-restart-progress` contains
`{handoff_id,generation,phase,old_port,new_port?,same_port,reason_code?,operator_action?}`. Before flock release,
the parent is the only writer to the old-port broadcaster. It may emit `in-progress`, `listener-settled`,
`reserved`, or `interrupted`. It never emits a post-release child `committed` verdict. The child may publish
`committed` only on its own new/full broadcaster after full GUI reachability.

### Grace-mode allowlist

On a port-change route, after authenticated standby confirmation the parent enters grace. Grace permits only:

- existing/new `GET /api/events` SSE streams;
- `GET /api/gui/restart/redirect?handoff_id=...`.

All other paths/methods return 503 `GUI_RESTART_IN_PROGRESS`. Before release the redirect returns 202. Once the
parent has published/flushed `reserved`, closed its own hub listener, and released the flock, it may return 200
with the confirmed loopback target URL for the remainder of fixed `G`; this reports the handoff target, not
child commit. It never accepts a host or URL from the child.

### Navigation guarantee

Port-change navigation remains explicitly **best-effort**. The frontend records the confirmed target URL before
handoff, navigates on matching `reserved`, and may poll the grace redirect during `G`. Because design-B deletes
post-release child acknowledgement, neither SSE nor redirect claims that the child committed. If navigation
lands early, normal browser retry/manual refresh applies. If it misses, the UI already displayed the target URL
and the recovery command selected by the trusted `operator_action` enum.

Same-port restart does not navigate. Native EventSource/browser reconnect targets the same origin.

If a durable free-flock `interrupted` is observed, the frontend stops retrying and shows exactly
**“GUI restart interrupted; run `mcphub gui`.”** A live-wedged-holder discriminator, when observable through a
surviving surface, shows exactly **“GUI restart interrupted: a GUI process still holds the single-instance lock;
run `mcphub gui --force --kill`.”** The frontend maps registered reason/action values to these literals; it never
renders an arbitrary persisted command.

## 9. Interrupted-handoff recovery: one degrade predicate, never relaunch

### Tri-state owner probe

```text
ProbeGUIOwnerLease(record)
  -> Held(reason) | Free(owned_probe_lease) | Unknown(error)
```

`Free` means the caller actually acquired and still owns the flock. The caller releases that lease on every
success, error, cancellation, and timeout path; ensure-alive never transfers it. There is no
probe-release-reacquire gap. A raw `reserved` marker inside `reservation_expires_at` maps to
`Held(ErrHandoffReserved)` even when the OS flock is momentarily free. Path, marker, DACL, or lock uncertainty
is `Unknown`, never dead and never a holder proof.

### The exact ensure-alive predicate

```text
feature gate enabled
AND record is schema-valid and state-dir-matching
AND record.phase IN {in-progress, reserved}
AND now >= phase_deadline(record)
    where in-progress -> fresh_until
      and reserved    -> reservation_expires_at
AND reservation-aware probe returns one of:

  Free(owned_probe_lease)
    AND InterruptFromOwnedFreeProbe(generation, sequence, owned_probe_lease,
                                    reason_code, "mcphub gui")
        atomically changes the exact nonterminal record -> interrupted
    => emit the free-flock message; release the lease; GUI spawn count = 0

  Held(reason)
    => mutate no marker; emit gui-restart-live-holder-wedged with
       operator_action "mcphub gui --force --kill"; GUI spawn count = 0

  Unknown(error)
    => mutate no marker; emit gui-restart-owner-unknown; GUI spawn count = 0
```

Every conjunct is mandatory. Inside the healthy reservation window, raw `reserved` remains Held/suppressed and
does not reach the deadline branch. After the window, `reserved + Free` can only become `interrupted`; it never
becomes `in-progress` and never launches a process. `InterruptFromOwnedFreeProbe` is the idempotency point:
concurrent ticks cannot hold the same flock and a generation/sequence mismatch loses the compare-and-swap.

This predicate makes no claim that both processes are dead. A parent may still be completing grace and a
designated child may still be alive or stalled in standby. It is nevertheless safe because ensure-alive never
spawns: while it owns the flock it changes the exact reservation to `interrupted`, and after release any late
standby child finds no matching `reserved`, releases, and exits. Therefore it cannot relaunch into or create a
second owner beside a surviving child.

### Predicate crash analysis — and no broader matrix

| Predicate point | Observation on next ensure-alive tick | Verdict |
| --- | --- | --- |
| Before the phase deadline | `reserved` inside its window, or active `in-progress` | Suppress; do not mutate or message |
| Deadline passed; probe is Held | Current process still owns the flock | Emit the force-kill message; do not mutate or spawn |
| Deadline passed; probe is Unknown | Ownership cannot be proved | Emit unknown discriminator; do not mutate, command, or spawn |
| Free lease acquired; before compare-and-swap | Flock is Held by this ensure-alive tick | Competing ticks suppress; cancellation releases lease |
| Compare-and-swap loses | Marker/generation/sequence changed | Release lease; follow the current coarse phase; no spawn |
| `interrupted` write wins | Exact nonterminal record is terminal; flock remains held only by this tick | Emit plain-launch message, release lease; late standby child rejects the marker |
| Ensure-alive dies while owning the probe | OS releases flock; record is old nonterminal or `interrupted` | Next tick repeats classification or leaves terminal state; no hidden process exists |

No other phase is eligible:

- `committed` suppresses handoff recovery even if a later intentional shutdown frees the flock; normal
  launch/autostart policy remains separate.
- `interrupted` is the manual floor and never auto-advances.
- absent, unknown-schema, or state-dir-mismatched data cannot authorize a marker write or command choice.
- `Held` before the applicable deadline is healthy/protected; `Held` after it is the live-wedged-holder
  discriminator, not permission for ensure-alive to kill.

### Two degrade outcomes

| Proved observation | Durable action | Exact message and recovery |
| --- | --- | --- |
| Nonterminal deadline passed + `Free(owned_probe_lease)` | Compare-and-swap the exact record to `interrupted`; release probe | **“GUI restart interrupted; run `mcphub gui`.”** |
|  |  | Plain launch acquires the proved/released free flock and starts a new generation. |
| Nonterminal deadline passed + `Held` | No marker mutation; emit `gui-restart-live-holder-wedged` | **“GUI restart interrupted: a GUI process still holds the single-instance lock; run `mcphub gui --force --kill`.”** |
|  |  | The existing identity gate verifies/kills the holder, then normal launch recovers. |

If a free-probe marker write fails, the owner emits `gui-restart-interrupted-marker-write-failed`, releases the
proved-free lease, shows the same plain-launch instruction, and does not claim a durable `interrupted` state.
Plain launch is still truthful because the discriminator was the owned free flock, not the marker write.

The GUI handoff classification branch is evaluated independently of the supervisor-live early return but does
not redefine supervisor recovery or autostart policy. Autostart intent alone never triggers it. There is no
ensure-alive kill, bind, spawn, retry, claim, fallback listener, or takeover path.

## 10. Shippable units and rollback groups

| Unit | Included scope | Reversibility / rollback group |
| --- | --- | --- |
| **A — independent fail-closed guard** | Typed hub-port dependency probe and two fail-closed callers | Ships first; independently reversible |
|  | Unreadable-client and group-error tests | Does not activate restart or port precedence |
| **B — atomic restart v3.1** | Listener seam, retained pre-release child handle, owner-only nonce standby | One feature-gated rollout and rollback group |
|  | Four-phase marker, reservation/Held, one degrade predicate, two exact manual commands | No post-release recovery or ensure-alive spawn component exists |
|  | Existing hub-driver initial-bind entry plus parent hub close at flock release | Same hub owner and retry/exhaustion policy |
|  | Parser-aware port precedence, 202/2xx API, coarse progress/navigation | Rollback disables endpoint and recovery together |

Unit B may be built internally as listener seam → standby/auth → marker/reservation → UI/recovery → gate, but no
intermediate state is a supported release. A disabled v3.1 leaves its marker inert and the UI presents
discriminator-selected manual guidance.

## 11. Contract and persisted-state migration

- **HTTP expand-contract:** retain `spawned`, `spawned_pid`, `spawn_error`, and `restarting`; accepted work is
  202, spawn failure remains 200/2xx-with-body. Add coarse fields, one SSE event, and one GET redirect route.
  Existing ping fields remain unchanged for ordinary callers.
- **Compatibility window:** one full Unit B release retains every legacy response field. Backend and frontend
  activate together; older tabs continue to receive fields they understand.
- **Persisted operational state:** introduce versioned v3.1 `gui-restart.json` with only the §5 fields and four
  phases. Missing means no handoff. Unknown version/schema/path fails closed and surfaces
  `gui-restart-owner-unknown`; it is never migrated into a recovery-qualifying phase or command choice.
- **Expand/contract:** code first reads absent/v3.1 and treats any prototype v2.x/v3.0 record as unknown/manual.
  After one release with no v2.x/v3.0 reader or writer, documentation and fixtures may delete those shapes. There is no live
  production v2.2 phase graph to migrate because v2.2 was a design target, not a shipped protocol.
- **Rollback:** disabling Unit B makes v3.1 markers inert. Plain `mcphub gui` may replace a stale/interrupted
  marker only after acquiring a free flock; a proved live holder still requires the pre-existing
  `mcphub gui --force --kill` identity gate. No settings, hub endpoint, groups, token, or client-config migration
  occurs.

## 12. Failure modes and observable discriminators

| Failure | Required behavior | Observable discriminator |
| --- | --- | --- |
| Spawn fails | Parent remains full; no marker reservation/release; 2xx non-restarting body | `gui-restart-spawn-failed` |
| Poised/bound proof absent or mismatched | Parent retains flock; exact authenticated child only may be terminated | `gui-restart-proof-timeout` or `gui-restart-proof-mismatch` |
| Target N busy | Parent remains full | `gui-restart-child-bind-failed`, reason `address-in-use` |
| Same-port bind/auth fails after P closes | Retain flock; exact-child reap; owned P rebind | `gui-restart-pre-release-rollback`, reason `bind-failed` or `confirm-timeout` |
| Parent P-rebind/restoration fails | Durably interrupt when possible; bounded child/hub cleanup; release flock; exit | `gui-restart-pre-release-rollback-failed`; `operator_action:"mcphub gui"` |
| Mutable requests do not drain | Restore parent full; no release | `gui-restart-quiesce-timeout` |
| Reservation write fails | Parent retains flock and rolls back | `gui-restart-reservation-write-failed` |
| Third entrant or ensure-alive enters healthy gap | Release tentative lease; map raw reservation to Held | `ErrHandoffReserved`; `gui-restart-reservation-held` |
| Expired nonterminal + proved Free flock | Compare-and-swap to `interrupted`; release lease; never spawn | `gui-restart-interrupted-free-flock`; `operator_action:"mcphub gui"` |
| Expired nonterminal + proved Held flock | Mutate no marker; require identity-gated baseline recovery | `gui-restart-live-holder-wedged`; `operator_action:"mcphub gui --force --kill"` |
| Marker/path/flock state unknown | Fail closed; do not select either recovery message | `gui-restart-owner-unknown` |
| Proved-Free `interrupted` write fails | Release lease and show plain command without claiming durability | `gui-restart-interrupted-marker-write-failed`; `operator_action:"mcphub gui"` |
| Child hub bind initially contends/fails | Full GUI stays reachable; enqueue `initial-bind-failed` into the existing driver from nil | Existing `hub-listener-restart-failed`, `-exhausted`, `-abandoned`, success, and hub-health events |
| Parent hub teardown reaches deadline | Force-close owned hub component, then release flock; do not wait for child | Existing `hub-shutdown-incomplete` plus handoff reason metadata |
| SSE/redirect missed | No false committed claim; retain target URL and manual command | Frontend navigation timeout/manual guidance |

Every new event carries `handoff_id`, generation, coarse phase, reason code, and non-sensitive port/process
metadata. Nonce, argv, environment, and secrets are never logged.

## 13. Security and resource lifetime

- Listeners remain loopback-only; existing Host/origin protections remain on full handlers.
- Standby exposes only challenged ping. Grace exposes only SSE and restart redirect. Neither exposes mutators,
  secrets, tokens, force-kill, or arbitrary proxying.
- The nonce is cryptographically random, owner-only, one-use, absent from argv/environment/logs/durable raw
  state, and represented in the marker only by a one-way hash.
- PID is diagnostic only. Confirmation requires retained handle plus nonce proof; termination uses that exact
  handle and exists only before flock release.
- Reservation uncertainty fails closed. The marker never authorizes a kill, bind takeover, or holder eviction.
- Every listener, process handle, marker lock, flock lease, SSE subscription, timer, and hub component has
  explicit success/failure/cancel/timeout cleanup in its owner.
- The old parent atomically takes and closes its own hub component through the existing hub owner before flock
  release; the child never closes or signals a parent resource.
- Ensure-alive's `Free` result is an owned resource. It is used only to guard one exact `interrupted`
  compare-and-swap and is released on every path; it is never transferred to a process.
- `mcphub gui --force --kill` retains the existing holder-identity proof. Neither the marker nor a bare PID
  authorizes termination.
- Redirect URLs are constructed from authenticated numeric loopback ports, never untrusted host input.

## 14. Automated test strategy and required seams

### Required seams

| Seam | Deterministic harness control |
| --- | --- |
| GUI listener owner | Exact standby/full/grace mode, listener close, exclusive rebind |
| Flock lease | Release/acquire, raw-reservation Held mapping, owned probe interruption and release |
| Spawned child handle | Nonce proof, pre-release exact terminate/wait, detach-at-release |
| Clock | Proof, bind, quiesce, reservation, rollback, grace, freshness deadlines |
| Marker store | Four phases, reserve/interrupt CAS, interrupted reason/action, write/read failure |
| Handler gate | In-flight mutator drain, new-request 503, grace allowlist |
| Hub owner | Parent close before flock release; initial-bind request from nil; unchanged retry/exhaustion policy |
| SSE/redirect | Pre-release event flush, fixed grace, best-effort navigation |
| Ensure-alive | Held/Free-owned/Unknown, zero GUI spawns, exact free/held command discriminator |

### Required runnable contract tests — 20 total, down from v2.2's 35

1. `TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive`
2. `TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately`
3. `TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire`
4. `TestRestartV3_PreReleaseRollbackFailureInterruptsReleasesLeaseAndExits`
5. `TestRestartV3_NonceRetainedHandleDefeatsPIDReuseAndNeverUsesEnvironment`
6. `TestRestartV3_ReservationRejectsThirdEntrantAndDesignatedChildWins`
7. `TestRestartV3_RawReservedFreeFlockMapsHeldDuringWindow`
8. `TestEnsureAliveGUIRecovery_ExpiredReservedFreeInterruptsAndNeverSpawns`
9. `TestEnsureAliveGUIRecovery_FreeVsHeldSelectsExactOperatorCommand`
10. `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease`
11. `TestRestartV3_ChildActivatesImmediatelyAndInitialHubBindRetriesFromNilThroughExistingDriver`
12. `TestRestartV3_API202RetainsRestartingField`
13. `TestRestartV3_SpawnFailureReturns2xxNonRestartingBody`
14. `TestRestartV3_PortArgvMatrix`
15. `TestRestartV3_GraceNavigationIsBestEffortAndNeverClaimsCommit`
16. `TestRestartV3_FeatureGateInertMatrix`
17. `TestRestartV3_FreeFlockInterruptedPlainLaunchRecoversEndToEnd`
18. `TestRestartV3_LiveHeldInterruptedForceKillRecoversEndToEnd`
19. `TestHubPortDependencies_FailsClosedOnUnreadableClient`
20. `TestHubPortDependencies_FailsClosedOnGroupsLoadError`

The protocol suite deliberately has no 43-edge/crash-verdict matrix, recovery-claim expiry race, 10s/30s
arbiter race, child suffix self-advance test, post-release terminate/reacquire test, or fallback recovery-listener
test. Windows process smokes cover both discriminator outcomes: a dead/free parent-child handoff restored by
plain `mcphub gui`, and a live wedged holder restored only through the existing identity-gated
`mcphub gui --force --kill`. Neither smoke permits an ensure-alive GUI spawn.

## 15. Diff-invisible invariants

| Invariant | Named regression guard and expected result |
| --- | --- |
| Listener close does not end hub/events/process | `TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive`: hub and SSE remain through same-port proof, until the explicit flock-release boundary |
| Parent hub socket is not held through old-port grace | `TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately`: parent hub close completes/force-closes before release; old GUI grace may continue afterward |
| Pre-release rollback never reacquires an owned lease | `TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire`: reacquire count is zero; P is full again |
| Failed rollback cannot leave a crippled flock holder | `TestRestartV3_PreReleaseRollbackFailureInterruptsReleasesLeaseAndExits`: cleanup deadline expires, but release count is one and process exit is requested |
| Healthy release-to-acquire gap never launches a third GUI | `TestRestartV3_RawReservedFreeFlockMapsHeldDuringWindow`: raw reserved plus free OS flock returns Held; GUI spawn count is zero |
| Child standby has no mutable side effects | Port-change test: hub/tray/browser/poller/mutator counters remain zero until flock acquisition |
| Child never waits for parent after flock acquisition | Immediate-activation test: full handler opens without hub-release or activation signal |
|  | Initial hub bind conflict enqueues the existing driver from nil; unchanged retry events lead to healthy or honest exhausted/down |
| Parent cannot recreate post-release arbitration | `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease`: all forbidden seam counters remain zero after release |
| Ensure-alive never creates an owner | `TestEnsureAliveGUIRecovery_ExpiredReservedFreeInterruptsAndNeverSpawns`: concurrent/later ticks produce one terminal compare-and-swap and zero GUI spawns/transfers |
| Supervisor fleet survives self-restart | Existing manager-stop regression guard: parent handoff exit does not stop adopted supervisor/daemons |
| Old parent never admits mutators after grace | Grace test: every non-allowlisted route returns 503 and admitted mutators drain before release |
| Hub endpoint port is never cleared when dependencies are unknown | Both Unit A tests assert reset owner is never invoked |
| Spawn failure remains friendly 2xx | `TestRestartV3_SpawnFailureReturns2xxNonRestartingBody`: frontend reaches incomplete banner, parent stays full |
| Free-flock guidance cannot bounce to an owner | `TestRestartV3_FreeFlockInterruptedPlainLaunchRecoversEndToEnd`: message appears only after owned Free; plain command restores full GUI |
| Live-holder guidance cannot falsely advertise plain launch | `TestRestartV3_LiveHeldInterruptedForceKillRecoversEndToEnd`: Held emits only the force-kill command; identity-gated recovery restores full GUI |

## 16. Alternatives and rejection drivers

The superseded v2.2 fully automatic recovery graph is not an available alternative: accepted decision
`2026-07-17-item3-unitb-recovery-simplify` rejects it because a record CAS cannot serialize OS kills, binds, or
flocks, and its no-owner freeze relocated across repeated revisions. The three remaining alternatives are:

### A. Keep `Server.Start` monolithic and close the listener indirectly — rejected

The current return path drains hub/events and ends process lifecycle. It cannot close/rebind only the GUI
listener or retain old-port grace. Decisive driver: re-verified lifecycle coupling. The `GUIListenerOwner` seam
remains required and `internal/gui/server.go` remains in the change surface.

### B. Dual-bind same port with `SO_REUSEPORT` — rejected

The exclusive listener and single-instance design intentionally forbid two full owners. Reuse-port would change
cross-platform socket/security semantics and still would not solve hub, mutator, or flock transfer.

### C. Add a surviving-child fence and automatic ensure-alive takeover — rejected

A correct takeover would need a provable child closure/fence protocol before spawning, because expired
`reserved + Free` does not prove the parent or standby child dead. That protocol would reintroduce another
long-lived actor/edge solely to retain automation. V3.1 instead uses the owned free lease to terminally interrupt
the exact record and fence a late child through the existing reservation check; it never spawns. Relaunching on
mere flock absence is even less safe because the flock is briefly absent during healthy handoff and may be
absent after intentional shutdown, while path/read errors can look dead.

## 17. Claims

Each claim is a falsifiable `{guarantee, single-owner, enforcement-probe}` contract for design-gate re-review.

1. `{ guarantee: the GUI listener can close and rebind without returning the full Server lifecycle or draining
   hub/events; single-owner: GUIListenerOwner; enforcement-probe:
   TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive }`.
2. `{ guarantee: authenticated standby is reachable before flock release, eliminating the v1 circular wait,
   and PID reuse cannot authorize the wrong child; single-owner: AuthenticatedReadinessSession;
   enforcement-probe:
   TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately and
   TestRestartV3_NonceRetainedHandleDefeatsPIDReuseAndNeverUsesEnvironment }`.
3. `{ guarantee: raw reserved maps to Held throughout the healthy gap so neither a third entrant nor
   ensure-alive can preempt the designated child; single-owner: SingleInstanceLease reservation-aware acquire;
   enforcement-probe: TestRestartV3_ReservationRejectsThirdEntrantAndDesignatedChildWins and
   TestRestartV3_RawReservedFreeFlockMapsHeldDuringWindow }`
   (decision `2026-07-17-item3-unitb-recovery-simplify`).
4. `{ guarantee: a successful automatic parent rollback retains the owned lease and rebinds P without
   reacquire, while a failed restoration durably interrupts when possible, performs bounded cleanup, releases
   the lease, and exits; single-owner: RestartCoordinator pre-release rollback;
   enforcement-probe: TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire and
   TestRestartV3_PreReleaseRollbackFailureInterruptsReleasesLeaseAndExits }`
   (decision `2026-07-17-item3-unitb-recovery-simplify`).
5. `{ guarantee: the parent closes its own hub listener before flock release, then performs no child-phase
   write, wait gate, termination, claim, reclaim, activation signal, or delayed hub teardown, so
   claimant-death/wedge/self-advance races have no actor or edge;
   single-owner: RestartCoordinator post-release no-op boundary; enforcement-probe:
   TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately and
   TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease }`
   (decision `2026-07-17-item3-unitb-recovery-simplify`).
6. `{ guarantee: child flock acquisition immediately activates full GUI without a parent signal, parent-status
   wait, or hub-health gate; single-owner: GUIOwnerLifecycle activation barrier; enforcement-probe:
   TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately }`
   (decision `2026-07-17-item3-unitb-recovery-simplify`).
7. `{ guarantee: a never-bound initial hub failure enters the one existing restart driver from nil with
   unchanged backoff, rolling-window, exhaustion, abandonment, event, and health semantics; single-owner:
   runHubListenerRestartDriver; enforcement-probe:
   TestRestartV3_ChildActivatesImmediatelyAndInitialHubBindRetriesFromNilThroughExistingDriver }`
   (decision `2026-07-17-item3-unitb-recovery-simplify`).
8. `{ guarantee: ensure-alive never spawns, kills, binds, retries, or transfers its probe lease; after the
   phase deadline an exact nonterminal + Free record can only become interrupted, and a late surviving standby
   child rejects that changed marker;
   single-owner: GUI ensure-alive predicate; enforcement-probe:
   TestEnsureAliveGUIRecovery_ExpiredReservedFreeInterruptsAndNeverSpawns }`
   (decision `2026-07-17-item3-unitb-recovery-simplify`).
9. `{ guarantee: only an owned Free probe advertises plain mcphub gui, while an expired nonterminal Held probe
   advertises only the existing identity-gated mcphub gui --force --kill; Unknown advertises neither;
   single-owner: ensure-alive flock discriminator/manual recovery boundary;
   enforcement-probe: TestEnsureAliveGUIRecovery_FreeVsHeldSelectsExactOperatorCommand,
   TestRestartV3_FreeFlockInterruptedPlainLaunchRecoversEndToEnd, and
   TestRestartV3_LiveHeldInterruptedForceKillRecoversEndToEnd }`
   (decision `2026-07-17-item3-unitb-recovery-simplify`).
10. `{ guarantee: valid persisted port intent wins only for self-restart while unset/invalid intent preserves
   explicit inherited port forms including 0; single-owner: resolveGuiPort typed helper;
   enforcement-probe: TestRestartV3_PortArgvMatrix }`
   (decision `2026-07-17-gui-server-port-authority-model`).
11. `{ guarantee: no destructive hub-port reset occurs unless clients and groups are proved dependency-free;
    single-owner: ProbeHubPortDependencies; enforcement-probe:
    TestHubPortDependencies_FailsClosedOnUnreadableClient and
    TestHubPortDependencies_FailsClosedOnGroupsLoadError }`
    (decision `2026-07-17-gui-server-port-authority-model`).
12. `{ guarantee: accepted restart remains HTTP 202 with restarting=true and spawn failure remains 2xx with a
    friendly non-restarting body; single-owner: restart HTTP response contract;
    enforcement-probe: TestRestartV3_API202RetainsRestartingField and
    TestRestartV3_SpawnFailureReturns2xxNonRestartingBody }`.
13. `{ guarantee: Unit B endpoint, marker, child mode, frontend path, and ensure-alive branch cannot be partially
    active; single-owner: gui.RestartV3Enabled; enforcement-probe: TestRestartV3_FeatureGateInertMatrix }`.

The claims surface is 13, with the recovery core expressed as reservation/Held, bounded pre-release rollback,
parent hub-close plus post-release no-op, immediate child activation, the one existing hub retry owner, and a
degrade-only free/held manual floor.

## 18. Non-goals and adjacent findings

- No zero-downtime same-port handover.
- No automatic rewrite of hand-pasted group URLs.
- No permanent rendezvous port or long-lived GUI health watcher.
- No change to supervisor ownership, daemon restart policy, or hub bind transaction.
- No bare-PID termination.
- No post-release parent/child arbiter, recovery claimant, self-advance, activation signal, hub-release phase,
  post-release kill/reacquire, fallback listener, ensure-alive GUI spawn, or automatic takeover/relaunch.
- No adjacent finding is folded into this revision.
- **Planner note:** update stale `CLAUDE.md` B1 “Hub listener hang” text in the Unit B docs pass; `runHubListenerRestartDriver` plus `hubHealthTracker` already implements bounded restart and exhaustion/abandon states.

## Terms and Abbreviations

- **CAS:** Compare-and-swap; in v3.1 used only to reserve the handoff and interrupt an exact nonterminal record
  by generation and sequence.
- **Flock:** The existing file lock granting one full GUI owner per user.
- **GRACE:** Old-port, read-only parent mode serving only SSE and restart redirect status for a fixed deadline.
- **GUI:** Graphical User Interface.
- **Handoff nonce:** One-use random value proving readiness came from the exact spawned child.
- **Held:** Tri-state probe result meaning an owner or protected reservation exists; not proof of which process.
- **SSE:** Server-Sent Events, the browser progress stream at `/api/events`.
- **STANDBY:** Child mode serving authenticated readiness with no mutable-runtime side effects.

## Gate decision: PASS — design-B v3.1 ready for final confirm
