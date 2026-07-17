# Item 3 DESIGN v2.2 — GUI self-restart and port change

Design package by `$architect`, tightened by the bounded v2.2 consistency pass after the confirm-gate accepted
the architecture and its compare-and-swap (CAS), rollback, and reservation mechanics. Branch baseline:
`master @ 9f39c2a2`. Accepted factual source: `item3-restart-recon.md`, supplemented by the accepted confirm-gate
trace and the live-parent hub-listener wedge documented in
`work-items/backlog/closed/2026-06-16-hub-listener-hang-no-recovery.md:11-24`. V2.2 changes only the
post-`child-lock-owned` wait owner: activation now follows a parent signal, proved parent death, or a bounded
no-signal timeout. The phase graph, rollback split, and reservation semantics are unchanged.

Cross-cutting port decision: `work-items/decisions/2026-07-17-gui-server-port-authority-model.md`
(`status: proposed`, revised with this package). Listener lifecycle, reservation, and recovery decisions
are local to item 3 and remain in this package.

No implementation code is specified below. Go-like text is limited to signatures and protocol pseudocode.

---

## 0. Change-Surface Contract

**Architect-owned field; the planner and implementers consume it, may return `REVISE` on conflict, and may
not redefine it.**

- **Intended change surface:**
  - `internal/gui/server.go` — REQUIRED. Split the currently coupled `Server.Start` lifetime into a
    restartable GUI-listener owner, full-runtime activation, hub-listener release, and final process
    shutdown. Today one GUI-listener close returns from `Start` and drains hub, events, and process state
    (`server.go:980-1009,1126-1205`), so a CLI-only handoff cannot be correct.
  - New `internal/gui/gui_listener_lifecycle.go` — `GUIListenerOwner`, request-mode gate, standby/full/grace
    handler swap, listener close, and rebind-from-owned-listener operations.
  - `internal/gui/gui_self_restart.go` plus new `internal/gui/gui_restart_protocol.go` — parent coordinator,
    child standby protocol, retained spawned-process handle, authenticated readiness, deadlines, progress
    events, and read-only recovery surface.
  - New `internal/gui/gui_restart_record.go` — the single writer-owner API for the generation-checked
    handoff record and bounded lock reservation. Parent and child request transitions only through this API.
  - `internal/gui/single_instance.go` — REQUIRED. The single-instance lease and reservation-aware acquire
    are the lock-transfer seam. The current lock is CLI-local state (`single_instance.go:25-31`); v2.2 moves
    its lifecycle ownership into the injected GUI-owner lifecycle.
  - `internal/gui/ping.go` — additive standby readiness proof on `/api/ping`; the existing public fields
    remain unchanged.
  - `internal/cli/gui.go` — inject the acquired lease into the GUI-owner lifecycle; construct child standby
    before full runtime; gate hub, supervisor, tray, browser, pollers, and mutable background work behind
    child activation; apply parser-aware restart argv reconstruction; retain the current manual-launch
    precedence. The current caller owns `defer lock.Release()` and one monolithic `Server.Start`
    (`gui.go:499-505,638-640`).
  - New `internal/api/hub_port_dependencies.go` — the one typed dependency probe covering
    `{gated, grouped, unknown/error}`.
  - `internal/cli/gui.go` and `internal/gui/server.go` — the two fail-closed consumers of that typed probe:
    `--reset-port` and initial hub-listener rollback policy respectively. The initial-start policy is set at
    the `Server.Start` composition call (`server.go:1043-1063`), not by changing the reset branch in
    `hub_listener.go:657-663`.
  - `internal/cli/supervise_ensure_alive.go` — additive tri-state GUI-owner probe and interrupted-handoff
    recovery decision. Existing supervisor recovery semantics remain separate.
  - `internal/gui/frontend/src/api.ts`,
    `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx`, and one restart-event consumer —
    preserve `restarting`, consume v2.2 progress, best-effort port-change navigation, and manual recovery.
  - `internal/gui/events.go` only if a registered event identifier is required; the existing
    `Broadcaster.Publish` and `/api/events` streaming contract are otherwise reused unchanged
    (`events.go:350-397,567-607`).
  - Focused unit, contract, and browser tests adjacent to every changed owner.
- **Approved extension seams:**
  - `GUIListenerOwner` is the only seam allowed to bind, close, swap, or rebind the GUI HTTP listener.
  - `HandoffRecordStore` is the only seam allowed to create or advance `gui-restart.json` or evaluate its
    reservation.
  - `SingleInstanceLease` is the only seam allowed to release/reacquire the GUI flock during handoff.
  - `SpawnedGUIChild` retains the exact OS process handle and is the only termination/wait seam.
  - `resolveGuiPort` plus its typed persisted-value helper is the only port-validity and precedence seam.
  - `ProbeHubPortDependencies` is the only reset-safety predicate.
  - The existing SSE broadcaster is the browser progress channel; no second event bus is introduced.
- **Protected / must-not-touch surfaces:**
  - `internal/gui/hub_listener.go` bind transaction, `api.BindHubMcpListener`, endpoint/token/instance-id
    formats, hub-session semantics, and hub-health state machine. Existing start/shutdown functions are
    orchestrated through the new lifecycle; their owned logic is not duplicated.
  - `internal/api/hub_mcp_groups.go` parsing and persistence semantics. `LoadGroups` remains the source probe;
    its errors remain errors (`hub_mcp_groups.go:328-356`).
  - Daemon/supervisor protocols, manifest, secrets, adopt/de-adopt, client reconcile, scheduler task format,
    and force-kill identity gates.
  - `internal/autostart/windows.go`; it is evidence only and already launches `gui` without `--port`
    (`windows.go:63-68`).
  - Existing `/api/ping` fields `{ok,pid,version}` and existing non-restart callers. V2.2 adds proof fields
    only for an authenticated standby challenge.
- **Declared blast radius:**
  - Full GUI-process lifecycle on self-restart: GUI listener, single-instance lease, hub-listener transfer,
    tray/browser/poller activation barrier, restart progress API, and interrupted-handoff recovery.
  - Hub-port reset safety only at the two named callers.
  - No daemon lifecycle, hub routing, token, manifest, group schema, scheduler task, or client-reconcile change.
  - This is a large internal seam because the current `Server.Start` conflates listener and process lifetime.
    It is developed in internal stages but ships only in the atomic v2.2 rollout group in §10.
- **Shared mutable-state ownership and committed events:**
  - Listener mode and request admission: exactly one writer-owner, `GUIListenerOwner`; downstream settled
    event, `gui-restart-progress{phase:"parent-quiesced"}`.
  - Handoff record and reservation: exactly one writer-owner, `HandoffRecordStore`; downstream committed
    events are its monotonic terminal transitions `committed`, `aborted`, `recovery-failed`,
    `shutdown-intent`, or `expired`.
  - Single-instance ownership: exactly one writer-owner, `SingleInstanceLease`; downstream settled event,
    record phase `child-lock-owned`.
  - Browser-visible restart state: exactly one writer-owner, the parent `RestartCoordinator`; downstream
    event, `gui-restart-progress`, emitted on the old-port broadcaster.

## 1. Re-verified constraints that drive v2.2

1. The GUI listener is not independently restartable today. `Server.Start` binds and serves it, starts the
   hub lifecycle, and drains hub/events when the GUI server returns (`server.go:980-1009,1043-1124,
   1126-1205`). `startGuiServer` merely owns the flock and invokes that monolith (`gui.go:499-505,638-640`).
2. The current restart returns success after spawn and schedules `os.Exit`, while the process handle is
   immediately handed to a background `Wait` and only a bare PID is returned
   (`gui_self_restart.go:98-145,156-201`).
3. The current child must acquire the flock before binding (`gui.go:285-305`). V1's “confirm child ping,
   then release flock” therefore deadlocked.
4. Current `/api/ping` proves only a caller-provided JSON PID (`ping.go:12-25`); the existing handshake
   checks that PID against pidport (`handshake.go:83-125`) but has no per-handoff proof.
5. `GatedOnClients` deliberately omits unreadable clients, although `ProbeHubGate` retains them
   (`hub_gate_detect.go:35-51,110-123`). `LoadGroups` distinguishes missing from every other read/parse error
   (`hub_mcp_groups.go:328-356`). A reset predicate that discards either error class fails open.
6. The current frontend contract requires `restarting` (`api.ts:920-948`) and branches on it
   (`SectionGuiServer.tsx:65-89`). A backend-only `202 {handoff_id}` is a regression.
7. `runEnsureAlive` currently returns early when the supervisor is live (`supervise_ensure_alive.go:346-360`),
   and its GUI-owner helper maps pidport path/read uncertainty to “not alive”
   (`supervise_ensure_alive.go:176-218,266-277`). V2.2 recovery must be independent, tri-state, and record-gated.
8. Only long `port` is registered (`gui.go:358`). Autostart supplies `gui` with no `--port`
   (`autostart/windows.go:63-68`).

## 2. Chosen architecture

### Parent-supervised, confirm-then-release handoff with a restartable GUI-listener owner

The running GUI remains the coordinator and rollback agent. The replacement child starts as a minimal
**STANDBY** process: it may bind and serve the GUI listener and answer an authenticated readiness probe, but it
does not load mutable GUI state, bind the hub, start tray/browser/pollers, expose mutators, or write ordinary
runtime state. The parent confirms this exact child, quiesces its own full handler, installs a bounded
reservation, releases the flock, and waits until the designated child owns it. Only then does the parent
release the hub listener and authorize child activation.

For a port change, the parent retains the old GUI listener in **GRACE** mode after giving up full-service
authority. GRACE serves only the existing SSE stream and `GET /api/gui/restart/redirect`; every other request
returns `503 GUI_RESTART_IN_PROGRESS`. This supplies best-effort navigation without permitting a second tab to
mutate settings, secrets, groups, or daemon state after the child begins loading state.

For a same-port restart, the parent must close only its GUI listener before the child can bind. The new
`GUIListenerOwner` makes that granular close and later rebind possible without returning from the full Server
lifecycle or implicitly draining the hub/events/process.

### Single feature gate

`gui.RestartV2Enabled()` is the single internal capability owner. It is resolved once at the CLI composition
root and injected into `Server`; `supervise --ensure-alive` calls the same owner. It is not a user setting and
adds no persisted configuration.

Disabled means fully inert: the v2.2 endpoint returns `503 GUI_RESTART_UNAVAILABLE`, the frontend gives manual
restart guidance, no child is spawned, no handoff record/reservation is created, and ensure-alive ignores v2.2
records. The unsafe v1 spawn-and-exit path is not used as fallback.

## 3. Component contracts and dependency direction

The stable dependency direction is CLI composition → GUI lifecycle contracts → existing hub/API primitives.
No reusable GUI component imports CLI-private code.

### `GUIListenerOwner`

Signature-level contract:

```text
BindStandby(port, readiness) -> BoundListener
ServeFull(bound, fullHandler) -> ModeSettled
EnterGrace(graceHandler) -> ModeSettled
CloseListener(deadline) -> Closed
AdoptAndServe(boundListener, handlerMode) -> ModeSettled
BindForRecovery(port, deadline) -> BoundListener
Shutdown(deadline) -> result
```

- Owns the `net.Listener`, `http.Server`, and one atomic per-request handler-mode gate.
- A mode swap rejects new non-allowed requests immediately; `EnterGrace` then waits, within a declared
  quiesce deadline, for already-admitted mutable requests to drain before reporting `ModeSettled`.
- Existing SSE connections survive a full→grace swap. A listener close is independent of hub/event/runtime
  shutdown.
- `BindForRecovery` returns the already-bound exclusive listener to the owner. The proof that the port is free
  and the rebind are one operation, avoiding a probe-close-rebind race.

### `GUIOwnerLifecycle`

- Owns `GUIListenerOwner`, injected `SingleInstanceLease`, hub components, activation barrier, and orderly
  cleanup.
- Normal launch starts full immediately.
- Handoff child starts standby and blocks every full-runtime side effect behind `Activate`.
- Parent `ReleaseHub` drains the existing hub through the existing owner; child `Activate` starts the desired
  hub state only after it owns the GUI lease. `ReleaseHub` is governed by the §4 post-confirmation deadline:
  the existing five-second graceful shutdown must fall through to its owned forced `http.Server.Close` action
  (`hub_listener.go:840-881`), never leave the coordinator blocked on drain completion.
- CLI tray, browser, pollers, supervisor adoption/spawn, session sweepers, and mutable background work wait on
  `Activated()`. This prevents the child's automatic hub bind from colliding with the parent and preserves
  same-port hub-toggle restart behavior.

### `SpawnedGUIChild`

```text
PID() -> int                 // diagnostic only
Wait(deadline) -> ExitResult // exactly once, on the retained OS process handle
Terminate(auth, deadline) -> result
DetachAfterCommit() -> result
Close() -> result
```

- The coordinator, not a background reaper, owns the retained spawn handle until commit/abort.
- No termination is addressed by a bare integer PID.
- `Terminate` is enabled after the child has supplied the valid one-time authenticated `poised` proof. This
  proof arrives before either route lets the parent quiesce or close anything, so same-port pre-release
  rollback is authorized to terminate the exact retained child. If no proof ever arrives, the child is still
  flockless: its ten-second pre-ownership standby expiry closes that listener and exits while the parent keeps
  serving. That exit rule never applies after `child-lock-owned`; the normative post-ownership conduct is below.
- Abort is recorded only after `Wait` proves the exact child exited. On commit, `DetachAfterCommit` releases
  the parent's local handle without waiting for the long-lived child.

### Authenticated readiness

- Parent generates a 256-bit handoff nonce and transfers it through a one-shot inherited OS pipe/handle whose
  endpoints are owned only by the parent and exact child; the nonce is never placed in argv, an environment
  variable, logs, or durable raw state. An owner-only temporary file is the only permitted platform fallback.
- Child reads once from the inherited channel and closes it before starting standby.
- The child readiness file contains `{handoff_id,generation,sequence,phase,pid,port,proof}` where `proof` is a
  message authentication code over the preceding fields.
- Parent accepts the first valid proof once and binds that authenticated session to the retained process
  handle. A challenge-bearing standby `GET /api/ping` must return matching `pid`, `handoff_id`, `generation`,
  and proof. The existing public ping fields remain byte-compatible for ordinary callers.
- The child explicitly serves standby ping **before** asking for the flock. This is a sanctioned, bounded,
  parent-supervised flockless listener, not full GUI ownership.

## 4. Confirm-then-release state machine

### Port-change path (`N != P`)

```text
parent holds flock + full GUI(P) + hub
  -> begin generation; spawn retained child handle
child binds N; serves STANDBY ping; parent confirms the nonce-bound retained child
  -> record poised
  -> parent EnterGrace(P); new mutators 503; admitted mutators drain
  -> record parent-quiesced
parent advances parent-quiesced -> reserved while still holding flock
  -> RELEASE flock
child acquires flock with matching claim; consumes reservation; writes pidport
  -> record/signal child-lock-owned; start child activation wait + parent post-confirmation deadlines
parent releases hub listener within the bounded post-confirmation duty
  -> record hub-released; signals activate
child loads mutable state, applies desired hub state, opens full handler,
  -> record activating; then starts tray/browser/pollers/supervisor work
  -> record committed; signal child-active
parent publishes committed(new_port=N) on old-port SSE
  -> keep GRACE(P) for G; close old listener; detach child handle; exit parent
```

The child answering ping no longer waits on a flock that the parent is withholding. Full child service still
waits for exclusive ownership.

### Same-port path (`N == P`)

```text
parent holds flock + full GUI(P) + hub
  -> begin generation; spawn retained child
child prepares minimal standby and supplies authenticated poised proof
  -> record poised
parent rejects new mutators and drains admitted mutators
  -> record parent-quiesced
parent closes ONLY GUI listener P; hub/events/process and flock remain owned
  -> record parent-listener-released; start the 2 s same-port bind-and-confirm deadline
child binds P; serves STANDBY ping; parent confirms the nonce-bound retained child
parent advances parent-listener-released -> reserved while still holding flock
  -> RELEASE flock
child acquires reserved flock; writes pidport; signals child-lock-owned
  -> start child activation wait + parent post-confirmation deadlines
parent releases hub within the bounded post-confirmation duty
  -> record hub-released; signals activate
child advances activating; loads state, starts desired hub state and full runtime; commits
parent detaches child handle and exits
browser EventSource reconnects to the same origin
```

The zero-listener interval is bounded by child bind/confirmation and the pre-flock-release rollback below.
The parent remains the rollback agent and retains the flock, hub, events, full handler state, and listener
ownership seam until the child is both confirmed and allowed to acquire the flock.

### Same-port pre-flock-release rollback

This path is keyed by `parentLeaseReleased == false`; it is not the post-release reclaim path. After
`parent-listener-released`, either an authenticated child bind failure or expiry of the **2 s deadline measured
from the successful close of P** triggers this ordered rollback. The same steps apply if `reserved` was CASed
but the parent detects failure before the actual lease-release call completes:

1. Keep the already-owned `SingleInstanceLease`; never call reacquire.
2. Cancel the handoff session and call `SpawnedGUIChild.Terminate` through the retained handle. The earlier
   authenticated `poised` proof authorizes this exact-child termination even if bound confirmation never came.
3. Concurrently retry `GUIListenerOwner.BindForRecovery(P)` and reap the exact child. A free P is rebound
   immediately; if the child briefly owns P, its standby listener prevents a zero-listener state until exact
   exit releases P, after which the same owner rebinds it.
4. Within the 5 s pre-release rollback budget, restore the full handler. Publish `aborted` only after exact
   `Wait` also proves child exit; if `Wait` outlives the rebind budget, keep serving full on P and continue the
   exact-handle wait without a PID kill. If P cannot be restored, enter the bounded read-only
   `recovery-failed` surface from §5; never wait indefinitely with P closed.

Only `parentLeaseReleased == true` permits the post-flock-release recovery in §5. At that point the child was
already authenticated and serving standby before it became `child-lock-owned`, so reacquisition is recovery of
a served child, not a race to repair an unconfirmed bind.

### Child activation-wait and parent post-confirmation arbiter

Let `t_lock` be the instant the child successfully CASes `reserved -> child-lock-owned` and atomically retains
the flock. Two monotonic-clock deadlines start at `t_lock`; neither adds a phase or legal edge.

The parent owns the earlier **ten-second post-confirmation decision window**. Its first five seconds are the
graceful `ReleaseHub` budget. If that sub-deadline expires, the hub owner cancels the drain and force-closes the
owned hub HTTP server; this forced close is independent of the blocked drain completion. During the remaining
five seconds the parent must land exactly one existing CAS decision: either
`child-lock-owned -> hub-released` followed by the activation signal, or `ClaimRecovery` followed by the §5
post-release rollback. At `t_lock + 10s` the parent may no longer initiate a post-confirmation phase write or
recovery claim; a late wake-up only re-reads and follows the winner.

The child waits at most **30 seconds** for the activation signal while also monitoring the inherited parent
process handle. A signal runs normal activation from the current live phase. Proved parent death triggers the
same fallback immediately. If no signal has arrived at `t_lock + 30s` and the parent handle is still alive, the
child emits `gui-restart-parent-wedged-self-activate` and runs that same fallback. The fallback re-reads the
record and, within five seconds, advances whichever suffix remains of
`child-lock-owned -> hub-released -> activating -> committed`; it never duplicates a phase already landed by
the parent. On the live-parent timeout branch, `hub-released` records expiry of the parent's exclusive release
authority, not proof that the old hub socket is already free.

**Normative child exit/hold conduct:** before authenticated `poised`, the flockless child exits on its
ten-second pre-ownership standby expiry; after `poised` but before `child-lock-owned`, it exits only on an
authenticated parent abort/termination or its own reported bind failure. After `child-lock-owned`, the child
never treats standby or absolute record expiry as an exit instruction and never voluntarily exits while
retaining the flock. It activates on signal, self-advances on proved parent death, or self-advances on the
30-second no-signal deadline. If a live parent recovery claim wins first, the child holds the flock and obeys
the existing exact-child rollback/termination protocol. These are the complete post-ownership outcomes.

The parent cutoff is strictly earlier than the child fallback. A parent phase CAS or recovery-claim CAS that
lands by `t_lock + 10s` increments the sequence and determines the path; the child's later CAS must observe that
phase/claim. If neither parent decision lands by the cutoff, the parent has forfeited new writes and the child
is the sole fallback owner at `t_lock + 30s`. Any CAS boundary race is serialized by the unchanged
generation+sequence+legal-edge guard: one write wins, the loser re-reads, and an unexpired recovery claim blocks
child advance. The child already owns the only flock, so no outcome creates two full GUI owners.

The normal path, parent-death path, and live-parent no-signal path share the same
`GUIOwnerLifecycle.Activate` implementation and existing CAS edges in §5. A hub bind failure against a still-
live wedged parent leaves the committed full GUI reachable with existing degraded hub-health reporting; it does
not strand a standby owner. The old parent is quiesced/grace-only and lacks the flock, so it cannot resume as a
second full owner. While the child remains alive its flock makes §9 suppress relaunch; if it dies, the
qualifying record plus a successful recovery claim allows one relaunch.

### Declared deadlines

All values are fields of one injected `RestartDeadlines` policy; tests use a fake clock.

| Deadline | Production value | Expiry action |
| --- | ---: | --- |
| Authenticated pre-close `poised` proof / flockless child standby expiry | 10 s | Retain parent full service; no release; unauthenticated child closes standby and exits |
| Port-change bind/ping confirmation | 2 s | Parent remains full; no release; terminate authenticated child and abort |
| Same-port bind/ping confirmation after P closes | 2 s from successful close | Run §4 pre-flock-release rollback; retain lease, rebind P, never reacquire |
| Mutable-request quiesce | 5 s | Return parent to full mode; abort |
| Handoff reservation | 5 s | Non-designated entrants remain rejected; stale reservation becomes recoverable |
| Same-port pre-release terminate/rebind total | 5 s | Restore P full or enter bounded read-only `recovery-failed`; no unbounded closed-P wait |
| Child lock-owned confirmation | 5 s | Enter post-flock-release rollback; never assume ownership transferred |
| Parent post-confirmation hub handoff | 10 s total from `child-lock-owned` | At 5 s force-close the owned hub server |
|  | First 5 s: graceful drain; last 5 s: CAS decision | By 10 s land `hub-released` + activate signal or the existing recovery claim; after 10 s parent is read-only for this decision |
| Exact child exit wait after post-release termination | 5 s | Do not write `aborted` or kill by PID; CAS `recovery-failed` and expose bounded recovery status |
| Post-release lock reacquire + owned-listener bind | 5 s | Terminal `recovery-failed`; start read-only recovery surface |
| Child activation-signal wait | 30 s from `child-lock-owned` | If no signal, emit the death or live-wedge discriminator |
|  | Admission also reserves 5 s conduct + 30 s expiry headroom | Self-advance the remaining activation suffix within 5 s |
| Child parent-death reaction | Immediate trigger; 5 s conduct budget | Run the same self-advance without waiting for the 30 s no-signal deadline |
| Old-port grace `G` after commit event flush | 5 s | Close old listener and exit parent |
| Absolute handoff-record expiry | 3 min | Recovery probe marks/observes expired; never relaunches from it |
| Ephemeral recovery-surface lifetime | 2 min | Close read-only surface; leave durable terminal guidance |

Deadline arithmetic is normative. With `E = expires_at`, admission to `child-lock-owned` requires
`t_lock + 65s < E`: 30 s activation wait + 5 s self-advance + 30 s recovery headroom. Therefore the no-signal
trigger satisfies `t_lock + 30s < E - 35s < E - 30s`, and even the complete five-second self-advance satisfies
`t_lock + 35s < E - 30s`. The absolute-expiry probe cannot land first. Parent arbitration also finishes first:
its decision cutoff is `t_lock + 10s < t_lock + 30s`, leaving a strict 20-second separation before child
fallback.

## 5. Reservation, crash-consistent record, and rollback

### One record owner and CAS protocol

`HandoffRecordStore` owns `<state-dir>/gui-restart.json` and its private record lock. Every transition is an
atomic compare-and-swap (CAS) under that lock:

```text
Advance(handoff_id, generation, expected_sequence, allowed_from, next_phase, patch)
  -> {new_sequence | stale_generation | illegal_transition | io_error}
```

The store rejects a lower generation, a stale sequence, an illegal live edge after absolute expiry, or a
terminal regression; `Expire` is the sole write whose time guard requires absolute expiry. Parent and child may
request advances, but neither writes the file directly.

Record v2.2 fields:

| Field | Purpose |
| --- | --- |
| `version`, `generation`, `sequence` | Schema and monotonic CAS identity |
| `handoff_id`, `state_dir_fingerprint` | Match this restart to this owner domain |
| `phase`, `route` | Current state and same-port/port-change path |
| `old_pid`, `old_port`, `child_pid`, `new_port` | Diagnostics; never sufficient for kill/confirmation |
| `nonce_hash` | Reservation claim check without persisting the nonce |
| `created_at`, `updated_at`, `expires_at` | Injected-clock freshness; `expires_at` is immutable per generation |
| `reservation_expires_at` | Bounded designated-child acquisition window |
| `recovery_claim_id`, `recovery_claim_expires_at` | CAS-owned single recovery claimant; absent until `ClaimRecovery` succeeds |
| `reason_code`, `fallback_port` | Typed terminal diagnosis and recovery surface |

### Complete monotonic phase graph

The complete durable `phase` enum is:

```text
accepted, poised, parent-quiesced, parent-listener-released,
reserved, child-lock-owned, hub-released, activating,
committed, aborted, recovery-failed, shutdown-intent, expired
```

`committed`, `aborted`, `recovery-failed`, `shutdown-intent`, and `expired` are absorbing for recovery: none can
be relaunched or advanced back into a live handoff. The sole terminal refinement is
`committed -> shutdown-intent`, which records later intentional shutdown without making the generation
resurrectable. `recovery-claim` is deliberately **not** another phase value: it is an in-place,
sequence-advancing CAS transition over one qualifying phase, preserving `phase` and storing the claimant fields.

Every mutation checks the same identity triple and graph guard:

```text
Advance(handoff_id, generation, expected_sequence, allowed_from, next_phase, patch)
ClaimRecovery(handoff_id, generation, expected_sequence, allowed_from,
              claim_id, claim_deadline)
Expire(handoff_id, generation, expected_sequence, allowed_from, now)
```

A write is legal only when `handoff_id` and `generation` equal the current record, `expected_sequence` equals
the current sequence, and the requested edge is in the following set with its route/owner predicate true. A
successful write increments `sequence` exactly once. Stale generation, stale sequence, wrong route, wrong
claim, or an edge absent from this table returns `illegal_transition` without modifying the record.

Repeated guards below are normative: `PRE_ABORT` = no child was created or exact child exit is proved and
parent full service is restored; `POST_ABORT` = matching recovery claim, exact child exit, owned lease, and full
service restored; `POST_FAIL` = matching claim plus an exhausted recovery deadline; `INTENT` = current
authorized owner records intentional shutdown before releasing its last capability; `EXPIRE` = absolute expiry
reached, generation+sequence match, no live claim, and no terminal landed first.

| Legal edge or CAS transition | Additional legality guard |
| --- | --- |
| `accepted -> poised` | Exact retained child supplies the authenticated `poised` proof |
| `poised -> parent-quiesced` | Parent still owns the flock; mutable admissions are closed and admitted mutators drained |
| `parent-quiesced -> parent-listener-released` | Same-port route only; parent still owns flock, hub, events, and listener owner |
| `parent-quiesced -> reserved` | Port-change route only; child N is authenticated and serving standby |
| `parent-listener-released -> reserved` | Same-port route only; child P bind and authenticated ping confirmed before the 2 s deadline |
| `reserved -> child-lock-owned` | Designated child presents the matching nonce, or recovery child presents the installed claim |
|  | Caller proves `now + 65s < expires_at` and atomically retains the tentatively acquired flock |
| `child-lock-owned -> hub-released` | Parent signals release by its 10 s cutoff, or the lock-owning child fallback observes proved parent death or the 30 s activation-wait expiry |
|  | Child fallback requires no unexpired recovery claim |
| `hub-released -> activating` | Current child owns the flock and invokes the single activation barrier |
| `activating -> committed` | Full GUI handler is reachable; desired hub activation has resolved healthy or explicitly degraded |
| `accepted -> aborted` | `PRE_ABORT` |
| `poised -> aborted` | `PRE_ABORT` |
| `parent-quiesced -> aborted` | `PRE_ABORT` |
| `parent-listener-released -> aborted` | `PRE_ABORT` |
| `reserved -> aborted` | `PRE_ABORT` when parent still holds the lease; otherwise `POST_ABORT` |
| `child-lock-owned -> aborted` | `POST_ABORT` |
| `hub-released -> aborted` | `POST_ABORT` |
| `activating -> aborted` | `POST_ABORT` |
| `parent-listener-released -> recovery-failed` | Pre-release owner retained the flock but failed the bounded P rebind/recovery-surface contract |
| `reserved -> recovery-failed` | Retained-lease pre-release recovery failure, or `POST_FAIL` after release |
| `child-lock-owned -> recovery-failed` | `POST_FAIL` |
| `hub-released -> recovery-failed` | `POST_FAIL` |
| `activating -> recovery-failed` | `POST_FAIL` |
| `accepted -> shutdown-intent` | `INTENT` |
| `poised -> shutdown-intent` | `INTENT` |
| `parent-quiesced -> shutdown-intent` | `INTENT` |
| `parent-listener-released -> shutdown-intent` | `INTENT` |
| `reserved -> shutdown-intent` | `INTENT` |
| `child-lock-owned -> shutdown-intent` | `INTENT` |
| `hub-released -> shutdown-intent` | `INTENT` |
| `activating -> shutdown-intent` | `INTENT` |
| `committed -> shutdown-intent` | `INTENT`; no other edge leaves `committed` |
| `ClaimRecovery` on `reserved` | Record is fresh, reservation deadline elapsed, no unexpired claim exists, claim deadline preserves 30 s expiry headroom, and CAS preserves `phase=reserved` while incrementing sequence |
| `ClaimRecovery` on `child-lock-owned` | Live authenticated parent rollback requested by the 10 s §4 cutoff, or ensure-alive already holding Free probe lease |
|  | Fresh record, no live claim, 30 s headroom; preserve phase and increment sequence |
| `ClaimRecovery` on `hub-released` | Same guard as the `child-lock-owned` claim transition |
| `ClaimRecovery` on `activating` | Same guard as the `child-lock-owned` claim transition |
| `accepted -> expired` | `EXPIRE` |
| `poised -> expired` | `EXPIRE` |
| `parent-quiesced -> expired` | `EXPIRE` |
| `parent-listener-released -> expired` | `EXPIRE` |
| `reserved -> expired` | `EXPIRE` |
| `child-lock-owned -> expired` | `EXPIRE` |
| `hub-released -> expired` | `EXPIRE` |
| `activating -> expired` | `EXPIRE` |

The recovery-qualifying phase set is exactly `reserved` **with a matching installed recovery claim**,
`child-lock-owned`, `hub-released`, and `activating`; the latter three still require `ClaimRecovery` before
automatic relaunch. The suppress set is exactly `accepted`, `poised`, `parent-quiesced`,
`parent-listener-released`, raw `reserved` without a matching claim, and all absorbing phases
`committed|aborted|recovery-failed|shutdown-intent|expired`.

A prior child `activating -> committed` CAS changes both sequence and phase, so any parent CAS whose
`allowed_from` excludes `committed` fails by construction. In particular, parent `aborted` after a prior child
`committed` is illegal regardless of timing; exact-child exit is necessary for abort but cannot override the
generation/sequence/legal-edge checks.

### Per-edge crash probes

Each row states the durable record left when parent or child crashes after X but before the named CAS to X+1.
“Relaunch” always additionally means a successful matching recovery claim and a provably free reservation-
aware flock; otherwise the verdict is suppress.

| Edge crossed next | Record left at X | Recovery verdict and reason |
| --- | --- | --- |
| `accepted -> poised` | `accepted` | **Suppress**: no authenticated child and ownership release is not proved |
| `poised -> parent-quiesced` | `poised` | **Suppress**: parent is still the lease/full-service rollback owner |
| `parent-quiesced -> parent-listener-released` | `parent-quiesced` | **Suppress**: same-port parent still owns P and the flock |
| `parent-quiesced -> reserved` | `parent-quiesced` | **Suppress**: port-change parent still owns the flock and grace/full rollback path |
| `parent-listener-released -> reserved` | `parent-listener-released` | **Suppress**: parent still owns the flock; §4 pre-release rollback must rebind P without reacquire |
| `reserved -> child-lock-owned` | raw `reserved` | **Suppress initially**: this healthy release-to-acquire gap maps to `Held/ErrHandoffReserved` |
|  |  | After reservation expiry only `reserved` with a successful claim may relaunch |
| `child-lock-owned -> hub-released` | `child-lock-owned` | **Suppress while Held**: the live child owns both parent-death and live-parent no-signal fallback |
|  |  | **Relaunch** only if the child also died, the flock is Free, and claim CAS succeeds |
| `hub-released -> activating` | `hub-released` | **Suppress while Held**; **relaunch** only on Free plus successful claim because takeover is already authorized and activation owns the healthy/degraded hub result |
| `activating -> committed` | `activating` | **Suppress while Held**; **relaunch** only on Free plus successful claim because activation was interrupted |
| Parent crashes during the 10 s post-confirmation decision window before its CAS | `child-lock-owned` | **Suppress while Held**: the child handle monitor triggers the common fallback immediately |
|  |  | The child commits within 5 s |
| Parent lands `hub-released`, then crashes before the activate signal | `hub-released` | **Suppress while Held**: the child re-reads, skips the already-landed edge, and advances `activating -> committed` |
| Child crashes during the parent decision or 30 s activation-wait window | `child-lock-owned` or `hub-released` | Flock becomes Free; only the existing owned-lease + matching-claim path may **relaunch** |
| `accepted -> aborted` | `accepted` | **Suppress**: no ownership release is proved |
| `poised -> aborted` | `poised` | **Suppress**: parent still owns pre-release cleanup |
| `parent-quiesced -> aborted` | `parent-quiesced` | **Suppress**: parent still owns pre-release cleanup |
| `parent-listener-released -> aborted` | `parent-listener-released` | **Suppress**: parent retains the lease and §4 rollback owns P |
| `reserved -> aborted` | raw or claimed `reserved` | Raw suppresses; claimed may **relaunch only** with Free because abort did not land |
| `child-lock-owned -> aborted` | `child-lock-owned` | Held suppresses for the child activation-wait arbiter; Free plus claim may **relaunch** |
| `hub-released -> aborted` | `hub-released` | Held suppresses; Free plus claim may **relaunch** |
| `activating -> aborted` | `activating` | Held suppresses; Free plus claim may **relaunch** |
| `parent-listener-released -> recovery-failed` | `parent-listener-released` | **Suppress** automatic relaunch: parent still owns the flock and must finish the bounded recovery surface |
| `reserved -> recovery-failed` | raw or claimed `reserved` | Raw suppresses; claimed may **relaunch only** with Free because failure did not land |
| `child-lock-owned -> recovery-failed` | `child-lock-owned` | Held suppresses; Free plus claim may **relaunch** |
| `hub-released -> recovery-failed` | `hub-released` | Held suppresses; Free plus claim may **relaunch** |
| `activating -> recovery-failed` | `activating` | Held suppresses; Free plus claim may **relaunch** |
| `accepted -> shutdown-intent` | `accepted` | **Suppress**: intentional shutdown did not land, but release is still unproved |
| `poised -> shutdown-intent` | `poised` | **Suppress**: intentional shutdown did not land, but parent still owns cleanup |
| `parent-quiesced -> shutdown-intent` | `parent-quiesced` | **Suppress**: intentional shutdown did not land, but parent still owns cleanup |
| `parent-listener-released -> shutdown-intent` | `parent-listener-released` | **Suppress**: parent retains the lease and P rollback duty |
| `reserved -> shutdown-intent` | raw or claimed `reserved` | Raw suppresses; claimed may **relaunch only** with Free because shutdown did not land |
| `child-lock-owned -> shutdown-intent` | `child-lock-owned` | Held suppresses; Free plus claim may **relaunch** because shutdown did not land |
| `hub-released -> shutdown-intent` | `hub-released` | Held suppresses; Free plus claim may **relaunch** because shutdown did not land |
| `activating -> shutdown-intent` | `activating` | Held suppresses; Free plus claim may **relaunch** because shutdown did not land |
| `committed -> shutdown-intent` | `committed` | **Suppress**: committed is already non-resurrectable |
| `ClaimRecovery` on `reserved` | `reserved` without claim if CAS did not land | Raw `reserved` suppresses |
|  | `reserved` with incremented sequence and claim if it did | After claim, only that claimant may acquire the reservation-aware lease and relaunch |
| `ClaimRecovery` on `child-lock-owned` | `child-lock-owned`, without or with incremented-sequence claim | Ensure-alive claim requires its owned Free lease; live parent claim freezes child commit for rollback |
| `ClaimRecovery` on `hub-released` | `hub-released`, without or with incremented-sequence claim | Same claimant exclusivity; Held suppresses and Free claimant may relaunch |
| `ClaimRecovery` on `activating` | `activating`, without or with incremented-sequence claim | Same claimant exclusivity; Held suppresses and Free claimant may relaunch |
| `accepted -> expired` | `accepted` | **Suppress** before or after expiry; no ownership release is proved |
| `poised -> expired` | `poised` | **Suppress** before or after expiry; parent ownership was not released |
| `parent-quiesced -> expired` | `parent-quiesced` | **Suppress** before or after expiry; parent ownership was not released |
| `parent-listener-released -> expired` | `parent-listener-released` | **Suppress** before or after expiry; retained-lease rollback owns P |
| `reserved -> expired` | raw or claimed `reserved` | Before expiry use reserved claim rule; after `Expire` lands, **suppress** as absorbing |
| `child-lock-owned -> expired` | `child-lock-owned` | Before expiry use Held/Free claim rule; after `Expire` lands, **suppress** |
| `hub-released -> expired` | `hub-released` | Before expiry use Held/Free claim rule; after `Expire` lands, **suppress** |
| `activating -> expired` | `activating` | Before expiry use Held/Free claim rule; after `Expire` lands, **suppress** |

### Bounded flock reservation every entrant honors

The reservation is the `reserved` record phase, written while the parent still holds the flock. Every
`AcquireSingleInstanceAt` entrant follows one owner rule after it tentatively acquires the flock:

1. Read the handoff record through `HandoffRecordStore` while holding the flock.
2. If a fresh reservation exists and the entrant has neither the matching designated-child
   `{handoff_id,generation,nonce}` nor the installed recovery claim, release immediately and return typed
   `ErrHandoffReserved`.
3. If the designated nonce or recovery claim matches, CAS `reserved -> child-lock-owned`, consume the
   reservation, retain the flock, and write pidport.
4. If record/path/read validation is unknown, release and fail closed.
5. A fresh raw `reserved` record without a matching recovery claim returns `ErrHandoffReserved` even when the
   OS flock is momentarily free. After `reservation_expires_at`, only a successful `ClaimRecovery` may install
   credentials that let the claimant retain the flock. After absolute `expires_at`, `Expire` makes the record
   terminal for automatic recovery; an explicit later user launch is not a v2.2 recovery relaunch.

A third `mcphub gui` from double-click, autostart, or a user command therefore cannot wedge the designated
handoff. It receives restart-in-progress guidance rather than entering the normal busy/force path.

An older/foreign binary may ignore the new reservation. If it wins anyway, the coordinator never kills it.
It waits for the authenticated child, retries lease reclaim within the deadline, and distinguishes a healthy
foreign GUI from an unknown holder. Persistent foreign ownership ends in `recovery-failed` with the read-only
recovery surface.

### Two rollback paths and durable recovery failure

The coordinator selects rollback only from the authoritative lease state, never from an inferred phase name:

| Lease state | Applicable failure window | Required rollback |
| --- | --- | --- |
| `parentLeaseReleased == false` | Any failure through bind/auth confirmation or the tiny reserved-before-release interval | Retain the already-owned flock and **never reacquire** |
|  | Includes the same-port 2 s deadline | Same-port runs §4 terminate/reap plus `BindForRecovery(P)` |
|  |  | Port-change swaps its still-owned Grace(P) listener back to full |
| `parentLeaseReleased == true` | Failure after the child was authenticated/serving and the flock was released | Install the matching recovery claim and terminate through the retained handle |
|  |  | Prove exact child exit, reacquire through the reservation-aware lease, and restore the old/full listener |

Post-release rollback is ordered: `ClaimRecovery`; exact-handle `Terminate`; exact `Wait`; lease reacquire;
exclusive `BindForRecovery(old_port)` or adoption of the still-owned port-change grace listener; full-handler
and desired-hub restore; then `aborted`. A prior `committed` makes the abort edge illegal. If exact exit,
reacquire, or bind cannot complete within the declared deadlines, the matching claim owner CASes
`recovery-failed` rather than waiting indefinitely.

If the old listener still exists on the port-change route, it remains the read-only recovery endpoint.
Otherwise the owner starts a read-only ephemeral recovery listener/agent containing only restart status and
manual recovery guidance, records its port, logs it, and best-effort opens it in the existing browser. The
agent owns no flock, hub, tray, or mutator and expires after 2 minutes. Failure to bind even that surface has
its own durable event and does not turn into a false success.

## 6. Port authority and parser-aware child argv

The desired/actual model remains:

| Layer | Authority | Owner |
| --- | --- | --- |
| Persisted `gui_server.port` | Desired operator intent when valid | Settings registry |
| Bound listener port | Actual runtime fact | `GUIListenerOwner` |
| `gui.pidport` | Derived rendezvous cache | Current flock holder |

Manual launch remains `explicit --port -> valid persisted -> 0`. Self-restart changes only inherited argv:
a **valid** persisted value wins and every recognized pre-terminator `--port` occurrence is removed. Unset or
invalid persisted state is “no valid intent”: inherited explicit `--port`, including `0`, is preserved.
Invalid persisted state emits a visible warning; with no explicit flag it degrades to ephemeral `0` rather
than silently pretending the invalid value was honored.

One typed helper under `resolveGuiPort` owns the sole `[1024,65535]` predicate and returns
`Unset | Valid(port) | Invalid(raw,reason)`. The argv builder consumes that classification; it does not parse
or retype the range.

`RebuildSelfRestartArgv` uses the actual GUI `pflag.FlagSet` metadata to identify effective token spans,
respects `--`, and recognizes only the registered long name `port`. It is not a raw string replacement.

| Parent argv shape | Valid persisted intent | Unset/invalid persisted intent |
| --- | --- | --- |
| `--port N` | Remove the flag/value pair | Preserve both tokens |
| `--port=N` | Remove the token | Preserve the token |
| `--port 0` | Remove the flag/value pair | Preserve explicit `0` |
| Repeated `--port` forms | Remove every recognized occurrence; persisted wins | Preserve all; normal parser last-wins behavior remains |
| `-port` | Reject as an unregistered shorthand before spawn; never normalize | Same rejection |
| Tokens after `--` | Preserve; they are positional, not effective flags | Preserve |
| No `--port` | No argv change | No argv change; invalid persisted warns and resolves to `0` |

Decision details and enforcement matrix live in
`work-items/decisions/2026-07-17-gui-server-port-authority-model.md`.

## 7. Fail-closed hub-port dependency guard

One typed probe replaces boolean/list-only decisions:

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
- `unknown` on any client config read/parse/DACL error, any groups read/parse/DACL/validation error, or probe
  path failure.

Both destructive reset callers fail closed:

- `mcphub gui --reset-port` refuses with exit 8 on `dependent` and on `unknown`, naming proved dependencies
  and separately listing unreadable sources.
- Initial hub startup in `internal/gui/server.go` sets
  `preservePortOnReloadHandlerFailure=true` unless the typed result is exactly `clear`. The reset mechanism in
  `hub_listener.go:657-663` remains unchanged; the composition caller supplies the safe policy.

This preserves every live `/clients/<client>/mcp` and `/g/<group>/mcp` URL or blocks when safety cannot be
proved. No automatic group URL reconcile is introduced because there is no authoritative inventory of where
operators pasted those URLs.

## 8. External API, grace handler, and frontend behavior

### Restart response

Accepted restart returns HTTP 202 while preserving current fields:

```text
{handoff_id, generation, phase:"accepted", spawned:true,
 spawned_pid, restarting:true}
```

Spawn failure returns **HTTP 200 (2xx) with a non-restarting body**
`{spawned:false,spawned_pid:0,restarting:false,spawn_error}`. It must not return non-2xx: today's GUI route
deliberately reports spawn failure as 200-with-body, mirroring `/api/supervisor/restart`
(`gui_self_restart.go:115-120`), while the current client throws on any non-2xx before it can enter the friendly
`Restart incomplete` body branch (`api.ts:937-948`). `202` means accepted, not completed; only a terminal
progress event/record means completed or aborted. Keeping `restarting` prevents the current frontend contract
regression (`api.ts:920-948`, `SectionGuiServer.tsx:70-79`).

### Progress event

`gui-restart-progress` body contains `{handoff_id,generation,phase,old_port,new_port?,same_port,reason_code?}`.
The parent is the only writer to the old-port broadcaster. The child communicates with the parent through the
authenticated readiness channel, not by publishing directly to the parent's bus.

### Grace-mode allowlist

On the port-change route, after authenticated `poised`, the parent swaps to grace and quiesces. The grace
handler permits only:

- existing/new `GET /api/events` SSE streams;
- `GET /api/gui/restart/redirect?handoff_id=...`.

All other paths and methods return `503` with stable code `GUI_RESTART_IN_PROGRESS`. The redirect route returns
202 while transfer is incomplete, 200 with a loopback-only URL after commit, and 409 with typed manual recovery
on abort/recovery failure. It never accepts a host or URL from the child; the parent formats
`http://127.0.0.1:<confirmed-port>/` from the authenticated bound port.

### Navigation guarantee

Port-change navigation is explicitly **best-effort**, not guaranteed delivery. After the child commits, the
parent publishes and flushes the `committed` event, keeps the old listener alive for `G=5s`, and keeps the
redirect route available for that same interval. The frontend navigates only on matching
`committed{handoff_id,generation}`. If SSE is missed, it polls the redirect route while the old origin remains
available. If both are missed, the UI has already shown the confirmed new port and manual URL before handoff;
the terminal record and logs retain it.

Same-port restart does not navigate. Native EventSource reconnects to the same origin
(`useEventSource.ts:31-65`).

## 9. Interrupted-handoff recovery without false resurrection

The v2.2 GUI recovery branch uses the record owner plus a reservation-aware lease probe:

```text
ProbeGUIOwnerLease(record, optional_matching_claim)
  -> Held | Free(owned_probe_lease) | Unknown(error)
ProbeInterruptedHandoff(now) -> ValidNonTerminal(record) |
                                AbsentOrTerminal | Expired | Unknown(error)
```

`Free` is proved by actually acquiring the flock and returning that still-owned probe lease; the caller either
passes it into the one relaunch or explicitly releases it on CAS/error/cancellation. There is no probe-release-
reacquire race. Path resolution, lock I/O, pidport read, malformed state, or permission failures are `Unknown`,
never “dead.” This corrects the current boolean helper's fail-open mapping
(`supervise_ensure_alive.go:176-218,266-277`).

The lease probe is reservation-aware: a fresh raw `reserved` record without a matching recovery claim maps to
`Held` with typed cause `ErrHandoffReserved` even during the healthy instant after parent release and before
child acquisition. It may return `Free` for `reserved` only after `reservation_expires_at` and only when the
caller presents the exact claim installed by a successful generation+sequence+legal-edge `ClaimRecovery` CAS.
For `child-lock-owned|hub-released|activating`, the order reverses: first acquire and retain the free probe lease,
then CAS the claim while holding it. Therefore ensure-alive never increments the record sequence underneath a
healthy lock-owning child.

The exact recovery-qualifying list is:

- `reserved` **with a matching installed recovery claim** after the reservation deadline;
- `child-lock-owned`;
- `hub-released`;
- `activating`.

The exact suppress list is `accepted`, `poised`, `parent-quiesced`, `parent-listener-released`, raw `reserved`
without a matching claim, and every absorbing phase: `committed`, `aborted`, `recovery-failed`,
`shutdown-intent`, and `expired`. This list is normative planner input; no implementation may infer a broader
set from prose such as “ownership was released.”

The additive GUI-relaunch rule is a phase-specific ordered AND gate:

```text
feature gate enabled
AND record == fresh + schema-valid + state-dir-matching + recovery-qualifying phase
AND (
  phase == reserved:
    reservation deadline elapsed
    -> generation+sequence+legal-edge ClaimRecovery CAS succeeds
    -> reservation-aware owner lease with that matching claim == Free(owned lease)
  OR phase in {child-lock-owned, hub-released, activating}:
    owner lease == Free(owned lease)
    -> ClaimRecovery CAS succeeds while that lease remains held
)
=> re-fire the GUI owner task once with the owned lease
```

Every other combination is no action with a typed event. In particular:

- `Held`: a healthy handoff gap, raw reservation, or owner exists; do not relaunch.
- `Unknown`: fail closed and retry next tick.
- every phase in the explicit suppress list, or an absent, malformed, mismatched, or unknown record: do not
  relaunch.
- autostart intent by itself never triggers this GUI-recovery branch.

When `child-lock-owned` is Held, ensure-alive intentionally takes no action: the §4 child activation-wait
arbiter is the single owner. It self-activates within 5 s of proved parent death, or by 35 s after
`child-lock-owned` when a live parent supplies no activation signal. If the child also dies, the flock becomes
Free; then and only then may one claimant recover from `child-lock-owned|hub-released|activating`.

The existing supervisor-down recovery remains its own path. The new GUI branch is evaluated independently of
the current early supervisor-live no-op (`supervise_ensure_alive.go:346-360`) but does not redefine supervisor
ownership or standalone-supervisor recovery.

## 10. Shippable units and rollback groups

Internal development stages are not independent releases. There are exactly two shippable units:

| Unit | Included scope | Reversibility / rollback group |
| --- | --- | --- |
| **A — independent fail-closed guard** | Typed hub-port dependency probe; CLI reset refusal; initial-start preserve policy | Independently shippable and reversible; roll back its caller + probe together |
|  | Unreadable/group error tests | It does not activate restart or port precedence |
| **B — atomic restart v2.2** | Restartable listener/full-runtime seam; retained child handle; owner-only nonce readiness | One feature-gated rollout and one rollback group |
|  | Unchanged reservation/CAS graph; two rollback paths; child activation-wait arbiter | Ship disabled until every member is present, then enable once |
|  | Bounded parent hub handoff; standby/hub transfer; parser-aware port precedence | Rollback disables the single gate for backend and recovery together |
|  | 202+SSE+redirect frontend; tri-state recovery; all protocol tests | The passive frontend receives 503 and presents manual guidance |

Unit B internal build order may be lifecycle seam → protocol/record → UI/recovery → activation, but no
intermediate order is a supported runtime. Specifically:

- Port precedence activation and port-change navigation are coupled.
- The accepted 202 response, spawn-failure 200/2xx body, and frontend consumer are coupled; `restarting`
  remains present throughout.
- Recovery never activates before the record protocol.
- Protocol backend cannot be rolled back while its UI or recovery consumer remains enabled.
- A disabled/rolled-back v2.2 leaves versioned records inert; old binaries ignore them, and the v2.2 recovery
  gate ignores them while disabled.

## 11. Contract and persisted-state migration

- **HTTP expand-contract:** add v2.2 response fields while retaining existing `spawned`, `spawned_pid`,
  `spawn_error`, and `restarting`; accepted work is 202, while spawn failure remains 200/2xx-with-body. Add one
  SSE event and one GET redirect route. Existing ping fields stay; authenticated readiness fields appear only
  on the challenged standby path.
- **Compatibility window:** one full Unit-B release keeps all legacy response fields. Frontend and backend
  activate together, so no supported mixed v2.2 state exists; older tabs still receive fields they understand.
- **Persisted state:** introduce versioned `gui-restart.json` for restart v2.2. It is operational state, not
  user config. The complete phase enum, recovery-claim fields, and legal-edge set in §5 are one schema unit.
  Missing means no handoff. Unknown version, invalid schema, mismatch, or expiry fails closed.
- **Rollback of migrated state:** disabling Unit B makes the file inert. Re-enabling may consume only a fresh
  valid v2.2 generation; otherwise the store writes/observes an expired terminal and starts a new generation.
  No settings, hub endpoint, groups, tokens, or client config migration occurs.

## 12. Failure modes and observable discriminators

| Failure | Required behavior | Observable discriminator |
| --- | --- | --- |
| Spawn fails | Parent remains full; no release; return 200/2xx non-restarting body | HTTP body `spawned:false,restarting:false`; `gui-restart-spawn-failed` |
| Child never supplies `poised` proof | Parent remains full; no kill, no release; child standby expiry | `gui-restart-readiness-timeout` |
| Nonce/ping proof mismatch | Reject confirmation; retain parent; never kill by PID | `gui-restart-proof-mismatch` |
| Target new port busy | Child reports bind failure; parent remains full | `gui-restart-child-bind-failed`, reason `address-in-use` |
| Same-port bind/auth fails or exceeds 2 s after P closes | Retain the flock; terminate authenticated poised child | `gui-restart-pre-release-rollback` |
|  | Rebind P without reacquire within pre-release budget | Reason `bind-failed` or `confirm-timeout` |
| Mutable requests do not drain | Reopen parent full admission; abort after child exit | `gui-restart-quiesce-timeout` |
| Third new-binary entrant | Release tentative flock; typed busy guidance | `ErrHandoffReserved`; `gui-restart-reservation-rejected` |
| Ensure-alive sees fresh raw `reserved` while flock is free | Map lease to Held and suppress | `ErrHandoffReserved`; `gui-restart-recovery-suppressed` |
|  | Recovery claim is required after reservation expiry | Reason `reservation-unclaimed` |
| Foreign/old entrant wins | Never kill it; retry exact child/lease recovery | `gui-restart-foreign-owner-detected` |
| Child fails after flock release | Claim recovery, terminate authenticated handle, prove exit, reacquire, then restore listener | phase plus `recovery_claim_id`; `gui-restart-post-release-rollback` |
| Exact child will not exit post-release | Do not write `aborted`; CAS `recovery-failed` after 5 s and expose bounded recovery status | `gui-restart-child-exit-timeout` |
| Old port cannot be rebound within the applicable pre/post-release budget | Terminal failure; start read-only recovery surface | `recovery-failed`; `gui-restart-fallback-listener-up` |
|  |  | or `gui-restart-fallback-listener-failed` |
| Parent graceful hub drain exceeds 5 s after `child-lock-owned` | Force-close the owned hub server | `gui-restart-parent-hub-force-closed` |
|  | Land normal handoff or the existing rollback claim by the 10 s parent cutoff | Followed by the winning phase/rollback event |
| Parent dies after `child-lock-owned` before activate | Held child self-advances within 5 s through `hub-released -> activating -> committed` | `gui-restart-parent-death-self-activate` then `gui-restart-child-active` |
| Parent remains alive but supplies no activation signal for 30 s after `child-lock-owned` | Held child self-advances the remaining CAS suffix within 5 s | `gui-restart-parent-wedged-self-activate` |
|  |  | Fields `parent_handle:"alive"` and `elapsed_ms:30000` |
|  | A conflicting old-hub bind is an honest degraded result, never a standby freeze | Same event with `record_remaining_ms`, then `gui-restart-child-active` |
| Hub activation fails | Child full GUI remains reachable; hub health reports existing degraded state | existing `hub-listener-*` events plus `gui-restart-child-active` |
| Record CAS conflict | Re-read; never overwrite newer/terminal state | `gui-restart-record-cas-conflict` |
| Record read/path/DACL error | No release/relaunch; fail closed | `gui-restart-record-unknown` |
| Recovery sees lock free but terminal/expired record | No relaunch | `gui-restart-recovery-suppressed`, typed reason |
| Parent or child crashes between phase edges | Apply the exact §5 crash-probe row; no inferred edge | `gui-restart-recovery-claimed` or phase-specific typed suppression |
| SSE/redirect missed | No false success claim; manual confirmed-port guidance | frontend timeout message plus terminal record/log |

All events carry `handoff_id`, `generation`, phase, and only non-sensitive process/port metadata. The nonce,
argv, environment, and secrets are never logged.

## 13. Security and resource lifetime

- Listeners remain loopback-only; the existing Host/origin protections remain on full handlers.
- Standby exposes only challenged ping; grace exposes only SSE and redirect status. Neither mode exposes
  mutators, secrets, group tokens, force-kill, or arbitrary proxying.
- The handoff nonce is cryptographically random, transferred only through the owner-only inherited
  pipe/handle (owner-only file fallback), absent from argv/environment/logs/durable raw state, and consumed once.
- PID is diagnostic only. Confirmation requires retained process handle + nonce proof; termination uses the
  retained handle only after authentication.
- The designated-child reservation compares a nonce hash; recovery claims compare their CAS-installed claim
  identifier under the same record lock. Unknown record state fails closed.
- Every listener, process handle, record lock, single-instance lease, SSE subscription, timer, hub component,
  and recovery agent has explicit success/failure/cancel/timeout cleanup in its owning component.
- The fallback recovery agent is read-only, time-bounded, and owns no single-instance or hub capability.
- Redirect URLs are constructed from authenticated numeric loopback ports, never untrusted host input.

## 14. Automated test strategy and required seams

### Required seams

| Seam | What the deterministic harness controls |
| --- | --- |
| Old-listener close/rebind | Exact close, retained handler mode, owned bound-listener transfer |
| Flock release/reacquire | Reservation order, third entrant, foreign winner, reclaim deadline |
| Spawned child handle | Authenticate, terminate, exact `Wait`, detach, handle cleanup |
| Bind probe | Port busy/free, exclusive listener ownership, persistent rebind failure |
| Clock/deadlines | Every timeout, parent 5 s force-close / 10 s decision cutoff, child 30 s activation wait / 5 s self-advance, strict 65 s admission guard, record expiry, grace `G`, retry cadence |
| Readiness channel | Poised, route-specific bind confirmation, lock-owned/active, nonce proof, sequence corruption |
| Record store | Complete legal-edge matrix, recovery claim/expiry CAS, generation regression, committed-to-aborted rejection, DACL/read error |
| Handler-mode gate | In-flight mutator drain, new-request 503, SSE continuity |
| Hub transfer | Parent graceful drain, forced close, and activation signal only after child-lock-owned; child live-wedge degraded bind; gate ON/OFF toggle outcomes |
| SSE/redirect | Event flush, five-second grace, poll fallback, abort/failure payloads |
| Recovery relaunch | Owner tri-state, record tri-state, CAS claim, autostart invocation count |

### Runnable contract tests

- `TestRestartV2_PortChange_StandbyBoundBeforeReservedRelease`
- `TestRestartV2_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive`
- `TestRestartV2_SamePort_PreReleaseConfirmDeadlineIsTwoSeconds`
- `TestRestartV2_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire`
- `TestRestartV2_PostReleaseRollbackReacquiresOnlyAfterLeaseRelease`
- `TestRestartV2_ChildStandbyHasNoHubTrayMutatorsOrBackgroundWrites`
- `TestRestartV2_ChildParentDeathAfterLockOwnedSelfActivatesWithinFiveSeconds`
- `TestRestartV2_ParentPostConfirmationForcesHubCloseAtFiveSecondsAndDecidesByTen`
- `TestRestartV2_ChildActivationWaitLiveWedgedParentSelfActivatesAtThirtySeconds`
- `TestRestartV2_ActivationWaitAdmissionRequiresStrictSixtyFiveSeconds`
- `TestRestartV2_ParentDecisionAndChildFallbackCASHaveExactlyOneWinner`
- `TestRestartV2_ParentCrashesAfterHubReleasedBeforeSignalChildResumes`
- `TestRestartV2_ReservationRejectsThirdEntrantAndDesignatedChildWins`
- `TestRestartV2_RawReservedFreeFlockMapsHeldUntilRecoveryClaim`
- `TestEnsureAliveGUIRecovery_HeldChildCannotBeClaimedOrSequenceStarved`
- `TestRestartV2_ForeignWinnerNeverKilled`
- `TestRestartV2_NonceAndRetainedHandleDefeatPIDReuse`
- `TestRestartV2_NonceUsesInheritedOwnerOnlyChannelNotEnvironment`
- `TestRestartV2_AbortWaitsForExactChildExitBeforeRecordTerminal`
- `TestRestartV2_RebindUsesOwnedExclusiveListener`
- `TestRestartV2_RebindFailureStartsReadOnlyFallbackAndEndsRecoveryFailed`
- `TestRestartV2_RecordCASCannotRegressCommittedToAborted`
- `TestRestartV2_RecordCompleteLegalEdgeAndCrashVerdictMatrix`
- `TestRestartV2_RecordRecoveryClaimAndExpiryRequireGenerationSequenceAndLegalEdge`
- `TestRestartV2_IntentionalShutdownCannotRelaunch`
- `TestRestartV2_GraceAllowsOnlySSEAndRedirectForFiveSeconds`
- `TestRestartV2_HubToggleSamePortTransfersAfterLockOwnership`
- `TestRestartV2_API202RetainsRestartingField`
- `TestRestartV2_SpawnFailureReturns2xxNonRestartingBody`
- `TestRestartV2_PortArgvMatrix` covering every §6 row plus invalid persisted warning.
- `TestHubPortDependencies_FailsClosedOnUnreadableClient`
- `TestHubPortDependencies_FailsClosedOnGroupsLoadError`
- `TestEnsureAliveGUIRecovery_RequiresFreeLockClaimAndExplicitQualifyingPhase`
- `TestEnsureAliveGUIRecovery_UnknownOrAutostartIntentAloneNeverRelaunches`
- Browser test: `gui-restart-port-change.spec.ts` proves committed SSE navigation and redirect-poll fallback
  against the deterministic two-process harness.

The only manual smoke retained is killing a real live GUI process during each handoff checkpoint on Windows
to validate OS handle/flock behavior and operator recovery. All protocol ordering, failure, navigation, and
record semantics are automated.

## 15. Diff-invisible invariants

| Invariant | Named regression guard and expected result |
| --- | --- |
| Same-port restart with hub toggle still works | `TestRestartV2_HubToggleSamePortTransfersAfterLockOwnership`: no child hub bind before parent release; final hub state equals persisted intent |
| Closing only the GUI listener does not end Server/hub/events | `TestRestartV2_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive`: hub probe and existing SSE stay live until explicit transfer |
| Same-port pre-release rollback never reacquires an already-owned lease | `TestRestartV2_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire` |
|  | Reacquire counter remains zero and P returns to full within the budget |
| A healthy release-to-child-acquire gap never triggers a second GUI | `TestRestartV2_RawReservedFreeFlockMapsHeldUntilRecoveryClaim` |
|  | Raw `reserved` plus provably free OS flock returns Held/`ErrHandoffReserved`; relaunch count remains zero |
| Parent death after child lock ownership cannot strand standby | `TestRestartV2_ChildParentDeathAfterLockOwnedSelfActivatesWithinFiveSeconds` |
|  | Inherited parent handle signals death; child commits within 5 s with expiry headroom intact |
| A live wedged parent after child lock ownership cannot strand standby | `TestRestartV2_ChildActivationWaitLiveWedgedParentSelfActivatesAtThirtySeconds` |
|  | No signal with an unsignaled live parent handle triggers at 30 s and commits by 35 s |
| Parent completion/abort and child fallback cannot both or neither win | `TestRestartV2_ParentDecisionAndChildFallbackCASHaveExactlyOneWinner` |
|  | Every parent CAS/claim versus child-timeout schedule produces one winning sequence path |
|  | Parent cutoff is 10 s and child trigger is 30 s |
| Absolute expiry cannot preempt the lock-owning child's fallback | `TestRestartV2_ActivationWaitAdmissionRequiresStrictSixtyFiveSeconds` |
|  | `reserved -> child-lock-owned` rejects equality or less |
|  | When admitted, commit lands with more than 30 s remaining |
| No mutable request reaches the old parent after `parent-quiesced` | `TestRestartV2_GraceRejectsAllMutators`: every non-allowlisted route returns 503; in-flight mutator drains before child activation |
| Child standby has no externally mutable side effects | `TestRestartV2_ChildStandbyHasNoHubTrayMutatorsOrBackgroundWrites`: all injected counters remain zero until activation |
| Supervisor fleet survives self-restart | `TestRestartV2_ParentSuccessDoesNotRunSupervisorStop`: adopted/spawned supervisor handle is not stopped by parent handoff exit |
| Existing second-instance activation remains unchanged outside reservation | Existing `TryActivateIncumbent` and force-path suites pass; new reservation test applies only during fresh `reserved` phase |
| EventSource same-origin reconnect remains native | Existing `useEventSource` test plus same-port browser contract reaches `open` without manual recreation |
| Hub endpoint port is never cleared when dependencies are unknown | Two fail-closed dependency tests assert `ResetHubPortContext` is not invoked |
| No false resurrection after intentional close | `TestRestartV2_IntentionalShutdownCannotRelaunch` records terminal shutdown, frees lock, runs ensure-alive, and observes zero relaunches |

## 16. Alternatives and rejection drivers

### A. Keep `Server.Start` monolithic and close the listener indirectly — rejected

The current return path drains hub and events and ends the process lifecycle (`server.go:1126-1205`). It cannot
hold/release/rebind only the GUI listener or retain an old-port grace handler. Decisive driver: panel finding A
and re-verified lifecycle coupling.

### B. Dual-bind same port with `SO_REUSEPORT` — rejected

The current exclusive listener and single-instance design intentionally forbid two full owners. This would
change cross-platform socket/security semantics and still would not solve hub, mutator, or lock transfer.
Decisive driver: accepted recon known limit 1.

### C. Relaunch whenever the GUI flock is absent — rejected

Absence occurs during healthy handoff and after intentional shutdown; current path/read errors can also look
dead. Decisive driver: panel finding D plus `supervise_ensure_alive.go:176-218,266-277`. The v2.2 AND gate
requires an explicit qualifying phase, a matching recovery claim, and a provably free reservation-aware lock.

## 17. Claims

Each claim is a falsifiable `{guarantee, single-owner, enforcement-probe}` contract for the next architecture
review.

1. `{ guarantee: the GUI listener can close, remain closed, or rebind without returning the full Server
   lifecycle or draining hub/events; single-owner: GUIListenerOwner; enforcement-probe:
   TestRestartV2_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive fails unless hub and SSE remain live
   across listener close and the owner restores an exclusively rebound P }`.
2. `{ guarantee: authenticated child standby serves before the parent releases the flock, eliminating the v1
   circular wait while keeping full service exclusive; single-owner: RestartCoordinator; enforcement-probe:
   TestRestartV2_PortChange_StandbyBoundBeforeReservedRelease asserts the exact ordered trace
   bind -> serve-ping -> poised -> parent-quiesced -> reserved -> release }`.
3. `{ guarantee: port-change navigation is best-effort with a measured five-second old-port delivery window
   and a manual recovery path, not an unprovable delivery promise; single-owner: parent GraceBridge;
   enforcement-probe: TestRestartV2_GraceAllowsOnlySSEAndRedirectForFiveSeconds plus
   gui-restart-port-change.spec.ts fail if the old listener closes before event flush plus G or if both SSE
   and redirect fallback cannot navigate }`.
4. `{ guarantee: valid persisted intent wins only for self-restart, while unset or invalid persisted intent
   preserves every explicit inherited port form including 0 and emits an invalid-value warning;
   single-owner: resolveGuiPort typed helper; enforcement-probe: TestRestartV2_PortArgvMatrix table-tests
   --port N, --port=N, --port 0, repeats, -port rejection, post-- tokens, no flag, unset, valid, and invalid }`
   (decision `2026-07-17-gui-server-port-authority-model`).
5. `{ guarantee: no destructive hub-port reset occurs unless clients and groups are both proved dependency-
   free; single-owner: ProbeHubPortDependencies; enforcement-probe:
   TestHubPortDependencies_FailsClosedOnUnreadableClient and
   TestHubPortDependencies_FailsClosedOnGroupsLoadError inject read, parse, and DACL errors and assert both
   reset callers never invoke ResetHubPortContext }`.
6. `{ guarantee: during a fresh reservation only the designated child can retain the GUI flock, and raw
   reserved without a recovery claim maps to Held even when the OS flock is free;
   single-owner: SingleInstanceLease reservation-aware acquire; enforcement-probe:
   TestRestartV2_ReservationRejectsThirdEntrantAndDesignatedChildWins races designated child, autostart tick,
   and double-click entrant and asserts one matching winner, while
   TestRestartV2_RawReservedFreeFlockMapsHeldUntilRecoveryClaim asserts zero recovery launch }`.
7. `{ guarantee: PID reuse or a foreign listener cannot cause confirmation or termination of the wrong
   process; single-owner: SpawnedGUIChild plus authenticated readiness session; enforcement-probe:
   TestRestartV2_NonceAndRetainedHandleDefeatPIDReuse supplies a matching integer PID with wrong handle/proof
   and asserts no confirm/terminate, then supplies the retained handle plus valid proof and observes the exact
   handle only }`.
8. `{ guarantee: every phase, legal edge, recovery-claim mutation, expiry mutation, and per-edge crash verdict
   is closed under generation+sequence CAS, and a committed child can never be overwritten by parent abort;
   single-owner: HandoffRecordStore; enforcement-probe:
   TestRestartV2_RecordCompleteLegalEdgeAndCrashVerdictMatrix and
   TestRestartV2_RecordRecoveryClaimAndExpiryRequireGenerationSequenceAndLegalEdge exhaust the graph, while
   TestRestartV2_RecordCASCannotRegressCommittedToAborted races the terminal writes }`.
9. `{ guarantee: ensure-alive relaunches only reserved-with-claim, child-lock-owned, hub-released, or
   activating after the phase-specific claim/owned-lease order succeeds; it cannot claim or sequence-starve a
   Held child; accepted through
   parent-listener-released, raw reserved, and every terminal suppress; single-owner: GUI interrupted-handoff recovery
   decision in supervise_ensure_alive; enforcement-probe:
   TestEnsureAliveGUIRecovery_RequiresFreeLockClaimAndExplicitQualifyingPhase exhaustively table-tests the
   owner x record state matrix, TestEnsureAliveGUIRecovery_HeldChildCannotBeClaimedOrSequenceStarved preserves
   the healthy child sequence, and TestEnsureAliveGUIRecovery_UnknownOrAutostartIntentAloneNeverRelaunches
   asserts zero relaunches for path/read errors, healthy handoff, terminal shutdown, expiry, and bare
   autostart intent }`.
10. `{ guarantee: rollback is selected only by actual lease release: pre-release retains the owned lease and
    rebinds P without reacquire, while post-release claims recovery, proves exact child exit, and reacquires;
    either path restores full service or ends durably in recovery-failed with a bounded read-only surface;
    single-owner:
    RestartCoordinator rollback state machine; enforcement-probe:
    TestRestartV2_SamePort_PreReleaseConfirmDeadlineIsTwoSeconds,
    TestRestartV2_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire,
    TestRestartV2_PostReleaseRollbackReacquiresOnlyAfterLeaseRelease,
    TestRestartV2_AbortWaitsForExactChildExitBeforeRecordTerminal,
    TestRestartV2_RebindUsesOwnedExclusiveListener, and
    TestRestartV2_RebindFailureStartsReadOnlyFallbackAndEndsRecoveryFailed cover both lease branches and
    terminal outcomes }`.
11. `{ guarantee: the child binds no hub listener, starts no tray/browser/poller, and exposes no mutator until
    it owns the reserved flock and activation is authorized by parent signal, proved parent death, or the
    bounded no-signal fallback after the parent decision cutoff;
    single-owner: GUIOwnerLifecycle activation
    barrier; enforcement-probe: TestRestartV2_ChildStandbyHasNoHubTrayMutatorsOrBackgroundWrites and
    TestRestartV2_HubToggleSamePortTransfersAfterLockOwnership assert zero pre-activation side effects and
    correct final hub state }`.
12. `{ guarantee: restart v2.2 backend, port precedence, navigation, and recovery cannot be partially active;
    single-owner: gui.RestartV2Enabled capability; enforcement-probe: TestRestartV2_FeatureGateInertMatrix
    runs every reachable endpoint, child mode, frontend response path, record path, and ensure-alive path with
    the gate disabled and observes no v2.2 side effect }`.
13. `{ guarantee: after child-lock-owned, neither confirmed parent death nor a live parent that supplies no
    activation signal can leave a lock-holding standby child waiting indefinitely; the child triggers
    immediately on death or at 30 s without signal, commits within the following 5 s, and retains more than
    30 s before absolute expiry; single-owner: child activation-wait arbiter; enforcement-probe:
    TestRestartV2_ChildParentDeathAfterLockOwnedSelfActivatesWithinFiveSeconds,
    TestRestartV2_ChildActivationWaitLiveWedgedParentSelfActivatesAtThirtySeconds, and
    TestRestartV2_ActivationWaitAdmissionRequiresStrictSixtyFiveSeconds cover both triggers and reject an
    admission guard that is not strict }`.
14. `{ guarantee: spawn failure remains a 2xx non-restarting body so stale tabs reach friendly incomplete
    handling instead of throwing on status; single-owner: restart HTTP response contract; enforcement-probe:
    TestRestartV2_SpawnFailureReturns2xxNonRestartingBody asserts status, body, no release, and no exit }`.
15. `{ guarantee: the handoff nonce is never transferred through argv or environment and is readable only by
    the exact parent/child pair; single-owner: authenticated readiness transport; enforcement-probe:
    TestRestartV2_NonceUsesInheritedOwnerOnlyChannelNotEnvironment inspects child argv/environment and owner
    permissions, then proves the inherited one-shot channel is consumed and closed }`.
16. `{ guarantee: after child-lock-owned, the parent lands completion or the existing rollback claim by its
    10 s cutoff, otherwise the child owns fallback at 30 s; exactly one CAS path wins and no schedule permits
    both owners or no owner; single-owner: HandoffRecordStore generation+sequence+claim arbiter;
    enforcement-probe: TestRestartV2_ParentPostConfirmationForcesHubCloseAtFiveSecondsAndDecidesByTen and
    TestRestartV2_ParentDecisionAndChildFallbackCASHaveExactlyOneWinner exhaust the parent-edge,
    parent-claim, no-parent-write, stale-sequence, and signal-loss schedules }`.

## 18. Non-goals and adjacent findings

- No zero-downtime same-port handover.
- No automatic rewrite of hand-pasted group URLs.
- No permanent rendezvous port and no long-lived GUI health watcher.
- No change to supervisor ownership, daemon restart policy, or hub bind transaction.
- No use of bare PID termination for this protocol.
- The admitted live-parent post-lock wedge is closed by the bounded v2.2 patch; no additional adjacent finding
  is folded into this revision.

## Terms and Abbreviations

- **CAS:** Compare-and-swap; a write accepted only when generation, sequence, and prior phase match.
- **Flock:** The existing file lock that grants one full GUI owner per user.
- **GRACE:** Old-port, read-only parent mode serving only SSE and restart redirect status.
- **GUI:** Graphical User Interface.
- **Handoff nonce:** One-use random value proving that readiness came from the spawned child.
- **Recovery claim:** Sequence-advancing CAS credential that grants one recovery actor permission to test and
  retain the reservation-aware flock; it does not change the durable phase.
- **SSE:** Server-Sent Events, the browser progress stream at `/api/events`.
- **STANDBY:** Child mode that may serve authenticated readiness but has no full-runtime side effects.

## Gate decision: PASS — v2.2 ready for final confirm
