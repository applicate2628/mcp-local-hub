# Item 3 Unit B — delivery plan (GUI self-restart handoff, design-B v3.1)

Planner: `$planner`. Consumes the accepted design
`item3-restart-design.md` (v3.1, arbiter-PASS) and the two accepted decisions
`work-items/decisions/2026-07-17-item3-unitB-recovery-simplify.md` and
`.../2026-07-17-gui-server-port-authority-model.md`. Baseline: `master @ 0e22d6c6`.
No implementation code below — phases, file:line seams, invariants, acceptance tests, gates.

**Unit A is SHIPPED** (PR #559 / `b18ed154`; `internal/api/hub_port_dependencies.go` + its two
fail-closed consumers verified present in-tree). §7 is NOT re-planned and its files are
must-not-touch here.

---

## 0. Change-Surface Contract (consumed verbatim from design §0 — planner does not redefine it)

Allocation is WITHIN the architect's surface. Owning files and the six approved seams:

- `internal/gui/gui_listener_lifecycle.go` (NEW) — `GUIListenerOwner` (only binder/closer/rebinder of the GUI HTTP listener).
- `internal/gui/gui_restart_record.go` (NEW) — `HandoffMarkerStore` (only writer/evaluator of `<state-dir>/gui-restart.json`).
- `internal/gui/gui_restart_protocol.go` (NEW) — parent coordinator, child standby, `SpawnedGUIChild`, `AuthenticatedReadinessSession`.
- `internal/gui/gui_self_restart.go` — endpoint rewrite (v3 behind the gate; v1 retained as the gate-off branch until the final flip).
- `internal/gui/server.go` — split `Start` (verified monolith: bind+serve `:980-1009`, hub init `:1050-1076`, drain `:1159`/`:1199`); route the gate-on initial hub error at `:1064-1076` into the existing driver; close the parent's own hub listener before flock release.
- `internal/gui/hub_listener.go` — bounded additive amendment at the nil-component guard (`:265-269`): admit a typed `initial-bind-failed` entry only. Every other nil-component entry still stop-drives.
- `internal/gui/single_instance.go` — reservation-aware acquisition + typed `Held | Free(owned_probe_lease) | Unknown` probe (extends `AcquireSingleInstanceAt`, `:121`).
- `internal/gui/ping.go` — additive challenged standby readiness (existing `/api/ping` `{ok,pid,version}` at `:20-24` unchanged).
- `internal/cli/gui.go` — inject the CLI-owned lease (acquire `:90-105`, `defer lock.Release()` `:513`), build standby before full activation, gate mutable child work behind flock acquisition, parser-aware restart argv, typed `resolveGuiPort` helper (`:47-55`).
- `internal/cli/supervise_ensure_alive.go` — additive tri-state GUI-owner probe + the one degrade-only predicate (independent of the supervisor-live early return in `runEnsureAlive`, `:346-415`). Never spawns a GUI.
- `internal/gui/frontend/src/api.ts`, `.../components/settings/SectionGuiServer.tsx`, + one restart-progress consumer — 202/2xx contract, coarse progress, best-effort navigation, discriminator-selected recovery guidance.
- `internal/gui/events.go` — register new event identifiers in `classifyEvent` (`:496`) if a source/severity row is wanted; reuse the existing broadcaster (`:141` / `:350`) + `/api/events`.

**Protected / must-not-touch:** `internal/gui/hub_listener.go` outside the exact initial-bind amendment; `api.BindHubMcpListener`; endpoint/token/instance-id formats; the hub-health state machine (item-1 surface); `internal/api/hub_mcp_groups.go` (`LoadGroups`); daemon/supervisor protocols; `internal/autostart/windows.go` (`superviseArgs` `:63-68`, launches `gui` with no `--port`); Unit A files.

---

## 1. Feature gate (whole restart-v2/v3 gated so a half-landed protocol never ships)

- **Identifier:** `RestartV3`.
- **Owner (single reader):** `gui.RestartV3Enabled()` — a package-level resolver in `internal/gui`, resolved ONCE at the CLI composition root (`internal/cli/gui.go`), threaded into `Server` (endpoint) and passed to `runEnsureAlive` (predicate). Design §2 / §13 pin the owner and the disabled behavior.
- **Default state:** **OFF** through Phases D–I; flipped to **ON** in Phase J (the enabling commit). Recommended mechanism = a package const `restartV3DefaultEnabled` (false→true at J) plus an env override `MCPHUB_GUI_RESTART_V3` (truthy `1`/`true` force-on, `0`/`false` force-off) for pre-flip smoke + post-ship rollback without a rebuild — mirrors the `MCPHUB_STRICT_JOB_PROTECTION` env-knob posture already in the repo. It is NOT an operator-facing gui-preferences setting (it is a rollout/rollback gate, not a user preference), consistent with "internal capability owner." **OPEN-1 (architect confirm):** const-default+env vs a gui-preferences registry key. The rest of the plan is agnostic to this choice.
- **Where each phase reads it:** endpoint (Phase G) → `if RestartV3Enabled() { v3 } else { v1 retained }`; child startup (Phase F) → standby-handoff vs normal single-shot acquire; ensure-alive (Phase I) → run predicate else skip; marker store (Phase D) is only ever written by the coordinator/ensure-alive, so gate-off ⇒ never written.
- **Disabled path (design §2/§10/§13) — verified inert:** endpoint 503 + frontend manual guidance in the SHIPPED product; no child, no marker, no reservation, no recovery branch. During development the gate-off endpoint branch stays the **retained v1 path** (see §3 atomic-release rule) so no intermediate commit regresses the working button; Phase J swaps that branch to the honest 503 and deletes v1.
- **Both paths verified:** `TestRestartV3_FeatureGateInertMatrix` (disabled = zero marker writes, zero child spawns, endpoint 503 after the J swap, ensure-alive predicate skipped) + the enabled contract suite.

---

## 2. Phase list (one line each — shippable-alone vs atomic-group)

| Phase | One-line scope | Ship class |
| --- | --- | --- |
| **A** | Typed `resolveGuiPort` classification (`Unset\|Valid\|Invalid`) + parser-aware `RebuildSelfRestartArgv` (inert until wired) | **Shippable-alone** (behaviour-preserving refactor + additive) |
| **B** | Extract `GUIListenerOwner` from the monolithic `Server.Start` (listener close/rebind independent of hub/events/process) | **Shippable-alone** (behaviour-preserving; largest seam; prerequisite) |
| **C** | Hub `initial-bind-failed` driver entry: `server.go:1064-1076` → recovering+enqueue; `hub_listener.go:265-269` typed nil-component admission | **Shippable-alone / ungated** (standalone hub robustness; OPEN-2) |
| **D** | Feature-gate resolver (default OFF) + `HandoffMarkerStore` (`gui-restart.json`, 4 phases, reserve/interrupt CAS) | Atomic-group (inert while gate OFF) |
| **E** | Reservation-aware flock acquisition + tri-state `Held\|Free(owned_probe_lease)\|Unknown` probe in `single_instance.go` | Atomic-group |
| **F** | Child half: standby + authenticated readiness (`ping.go`) + activate-on-flock + `Commit` (bounded retry + `gui-restart-commit-write-failed`) | Atomic-group |
| **G** | Parent coordinator: spawn/confirm/reserve/parent-hub-close/release + pre-release rollback + 202/2xx endpoint (gate-off = retained v1) | Atomic-group |
| **H** | Frontend + `gui-restart-progress` event + best-effort navigation + two degrade messages (`go generate`) | Atomic-group (rollback sub-group with G) |
| **I** | Ensure-alive degrade-only predicate (tri-state probe consumer; never spawns) | Atomic-group (parallelizable after D+E) |
| **J** | Gate flip OFF→ON + swap gate-off branch v1→503 + delete v1 spawn + inert-matrix test + docs pass | Atomic-group final |

**Ready to implement — first phase: A** (lowest-risk, behaviour-preserving; unblocks the child argv). B and C are also independent and MAY proceed in parallel with A (disjoint files: `cli/gui.go`+new port file vs `gui/server.go`+`gui_listener_lifecycle.go` vs `gui/server.go`+`hub_listener.go` — note B and C both touch `server.go` in different regions, so sequence B before C to avoid a merge collision, or land C's `:1064-1076` edit inside B's Start-split branch).

There is no separate admitted-bug Phase-A: the admitted defect (roadmap `:28` "self-restart can brick itself to zero listeners while reporting success") is fixed by Unit B as a whole (the confirm-then-release protocol), not by a smaller isolable patch. The fail-closed prerequisite (Unit A guard) already shipped.

---

## 3. Atomic-group release rule (never regress the working button mid-rollout)

The atomic group is **D–J**, ONE feature-gated rollout + rollback unit. Two hard rules:

1. **Do not cut a deploy/version-bump between the start (D) and end (J) of the group.** The design deletes v1 as an unsafe fallback (§2), so a shipped gate-off state is `503 + manual guidance`. To guarantee no INTERMEDIATE commit regresses the working "Restart GUI" button, Phase G's gate-off branch stays the **retained v1 endpoint**; only Phase J replaces it with 503 and deletes the v1 spawn. Even an accidental mid-group master deploy then keeps the working v1 button.
2. **Backend + frontend activate together** (§11): the 202/2xx contract (G) and its only consumer (H) land/revert as one sub-group.

Rollback of the shipped feature = flip `RestartV3Enabled()` OFF (→ 503 + honest manual guidance). Rollback of the code = revert D–J together. A, B, C are each independently reversible.

---

## 4. Phases in detail

### Phase A — port authority typed helper + parser-aware restart argv

- **Scope / seams:** `internal/cli/gui.go` (refactor `resolveGuiPort` `:47-55` to consume a new typed classifier `Unset | Valid(port) | Invalid(raw,reason)` owning the sole `[1024,65535]` predicate); new `RebuildSelfRestartArgv` using the GUI `pflag.FlagSet` metadata (respects `--`, recognizes only the registered long `port`, per-shape table in decision doc + design §3). Argv builder is ADDITIVE and NOT invoked in production (the v1 spawn at `gui_self_restart.go:179` still uses `os.Args[1:]` until Phase F/G wires the builder behind the gate).
- **Invariant:** the `[1024,65535]` validity predicate exists exactly once; manual launch precedence `explicit --port → valid persisted → 0` is unchanged; the argv builder consumes the typed classification and contains no independent range check.
- **Allowed change surface:** `cli/gui.go` + one new port-authority helper file; no server/gui-listener/marker touch.
- **Must-not-break:** existing `resolveGuiPort` tests; manual foreground `--port` behavior; autostart `gui` (no `--port`) argv.
- **Dependencies:** none.
- **Acceptance criteria:**
  - **AC-A1** — `TestRestartV3_PortArgvMatrix` passes every argv shape (`--port N`, `--port=N`, `--port 0`, repeated forms, `-port` rejected, tokens after `--` preserved, no-flag) × persisted `{unset, valid, invalid-parse, invalid-low, invalid-high}` per the design §3 / decision-doc tables.
  - **AC-A2** — valid persisted removes EVERY recognized pre-`--` occurrence; unset/invalid preserves inherited explicit `--port` incl. `--port 0`.
  - **AC-A3** — invalid persisted emits `gui-port-persisted-invalid` (redacted-safe raw value + fallback source) and degrades to ephemeral `0` only when no explicit flag; never claims the invalid value took effect.
  - **AC-A4** — manual launch precedence unchanged (regression assertion in the same table test).
  - **AC-A5** — the argv builder body contains no `[1024,65535]` literal (grep-assert in test or review).
- **Required tests:** `TestRestartV3_PortArgvMatrix` (table). **Naming note:** the decision-doc calls it `TestRestartV2_PortArgvMatrix`; use the design v3.1 name `TestRestartV3_PortArgvMatrix` (V2 name is stale).
- **Checks:** `go build ./... && go vet ./... && go test -count=1 ./internal/cli/`.
- **Rollback:** revert the one commit; `resolveGuiPort` returns to inline form; builder was unused.

### Phase B — `GUIListenerOwner` seam extraction (largest seam; prerequisite)

- **Scope / seams:** new `internal/gui/gui_listener_lifecycle.go` owning the `net.Listener` + `http.Server` + one per-request handler-mode gate; split `Server.Start` (`server.go:980-1009` bind/serve, drain `:1159`/`:1199`) so the GUI HTTP listener close/rebind is independent of hub/event/runtime shutdown. Contract surface from design §6: `BindStandby / ServeFull / EnterGrace / CloseListener / AdoptAndServe / BindForRecovery / Shutdown`.
- **Invariant (behaviour-preserving):** normal launch starts `ServeFull` immediately with byte-identical behavior; closing the GUI listener does NOT end the hub, event bus, or process runtime; existing SSE may survive full→grace. Only `ServeFull` is exercised in production until the gate flips; `BindStandby/EnterGrace/BindForRecovery/AdoptAndServe` are inert code until Phase F/G.
- **Allowed change surface:** `gui/server.go` (`Start` split only) + the new lifecycle file.
- **Must-not-break:** `Server.Shutdown`/drain nil-`hubMcpComp` handling (`server.go:1159`,`:1199`); the `close(ready)`-before-Serve ordering (`server.go:1007-1009`, the r8 P1 non-fatal-hub contract); `s.port.Store` after bind (`:985`) feeding `s.Port()` → pidport rewrite (`cli/gui.go:692-709`); all existing `internal/gui/server_test.go` + e2e.
- **Dependencies:** none (B before C to avoid a `server.go` merge collision).
- **Acceptance criteria:**
  - **AC-B1** — `TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive`: `CloseListener` leaves the hub component + an open SSE subscription alive; hub/events survive independent of the GUI listener.
  - **AC-B2** — normal-launch parity: existing gui server tests + the Go-side embed smoke (`go test ./internal/gui/`) green; `/api/status`, `/api/events`, pidport rewrite unchanged.
  - **AC-B3** — the handler-mode gate rejects new non-allowed requests immediately and drains already-admitted mutators within an injected deadline (unit test with a fake handler + clock).
  - **AC-B4** — `BindForRecovery` returns an already-bound exclusive listener and is only callable while the flock is still owned (asserted by the caller-guard in Phase G; here: the method exists + binds exclusively).
- **Required tests:** `TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive` (partial: seam-only portion — listener close ⊥ hub); handler-gate unit test.
- **Checks:** `go build/vet/test ./internal/gui/`; run the CLI GUI-spawn tests locally (`go test -tags=test_state_path_env ./internal/cli/`); sweep `mcphub.exe` after (CLAUDE.md Step 2).
- **Rollback:** revert; `Start` re-inlines the listener. Because it is behaviour-preserving, B may ship+deploy on its own before the atomic group.

### Phase C — hub `initial-bind-failed` driver entry (standalone hub robustness)

- **Scope / seams:** `server.go:1064-1076` — replace the terminal "gate-OFF for this process" `HubHealthDown` on a gate-on initial hub-start error with a retry-scheduled diagnostic: retain `HubHealthRecovering` and enqueue a typed `initial-bind-failed` request on the restart channel. `hub_listener.go:265-269` — at the nil-component guard, admit ONLY a typed `initial-bind-failed` cause: skip old-component swap/shutdown, **establish the loop's taken-state equivalent (the `oldTaken=true` posture the swap path would have set) so iteration ≥2 does NOT re-hit the nil-component `hubListenerRestartStopDriver`**, and enter the existing start/backoff loop; every other nil-component cause still returns `hubListenerRestartStopDriver`.
- **Cause-transport plumbing (fable P2, added):** the existing restart channel is a bare buffered-1 `hubRestartCh chan struct{}` (`server.go:610-615`, made `:786`, signaled `signalHubListenerRestart` `hub_listener.go:130`, consumed `:166-167`) — it carries NO cause. Admitting a *typed* `initial-bind-failed` entry therefore requires a cause-transport: either retype the channel or add a dedicated side-band cause flag read at the driver receive. This plumbing (channel/flag decl + constructor + signal fn + driver receive) is IN-SCOPE for Phase C even though it sits outside the two `:1064-1076` / `:265-269` regions; implementer's choice of typed-channel vs flag.
- **Invariant:** `runHubListenerRestartDriver` + `hubHealthTracker` stay the single hub recovery/state owner; base/max backoff, rolling-window attempt cap, consecutive-restart cap, same-port wait, exhaustion/abandon outcomes, event identifiers, and health transitions are unchanged. Exhaustion still ends in honest `HubHealthDown` — the amendment only makes the existing recovery owner run first for the never-bound case.
- **Allowed change surface:** the two regions above PLUS the cause-transport plumbing (restart-channel type or side-band cause flag: decl + constructor + `signalHubListenerRestart` + driver receive). No other hub-listener behavior touched.
- **Must-not-break:** the same-port hub-toggle rebind mechanism (`hub_listener.go` reset) — untouched; `hub_listener_restart_test.go` + `hub_listener_restart_windows_test.go`; the item-1 honest-health surface (recovering→down on exhaustion must stay reachable); `api.BindHubMcpListener`; the exclusive-listener bind transaction.
- **Dependencies:** none functionally; sequence AFTER B (shared `server.go`).
- **Acceptance criteria:**
  - **AC-C1** — standalone: a normal-startup initial hub bind failure (gate-on) now sets `HubHealthRecovering` and enqueues `initial-bind-failed`; the driver retries from nil rather than going terminal `HubHealthDown` on the first error.
  - **AC-C1b (fable P2, added — the dies-after-one-retry guard):** a persistently-failing initial bind produces **≥2 consecutive failed retry attempts with backoff** before exhaustion (proves the taken-state was established; a driver that dies after a SINGLE nil-component retry must FAIL this test — AC-C1 alone passes on one retry and would miss the bug).
  - **AC-C2** — every other nil-component driver entry still stop-drives (regression guard on the existing tests).
  - **AC-C3** — exhaustion path still terminates in honest `HubHealthDown` (item-1 degraded banner still fires on true down).
  - **AC-C4** — backoff/window/consecutive-cap/same-port-wait constants and the emitted event *identifiers* (`hub-listener-restart-failed/-exhausted/-abandoned` + success) are unchanged. **Qualified (fable P2):** for the `initial-bind-failed` cause the `hub-listener-restart-failed` event BODY carries `port: 0` (there is no old component to read a port from, `hub_listener.go:270-272,304`); this is the one intended body difference and the test asserts it, not byte-equality.
- **Required tests:** a new standalone driver-entry test (initial-bind failure at plain startup → recovering + retry) + the unchanged existing hub-listener restart suite green.
- **Checks:** `go build/vet/test ./internal/gui/`.
- **Rollback:** revert; initial hub bind failure returns to terminal `HubHealthDown`. **OPEN-2 (architect confirm):** design §10 bundles this in Unit B's atomic group, but the §13 gate-claim inert matrix does NOT list it; planner proposes shipping the driver-entry UNGATED as a standalone robustness fix (beneficial regardless of the handoff, needed before the child's hub bind can recover). The parent-hub-close-at-flock-release ordering (handoff-only) stays inside the gated coordinator (Phase G).

### Phase D — feature-gate resolver + `HandoffMarkerStore`

- **Scope / seams:** introduce `gui.RestartV3Enabled()` (default OFF; §1) resolved at composition and threaded into `Server`; new `internal/gui/gui_restart_record.go` owning `<state-dir>/gui-restart.json` + its private record lock through the hardened owner-only state-file pipeline (same posture as `supervisor-intent.json`). Operations (design §5): `Begin / Reserve / Commit / Interrupt / InterruptFromOwnedFreeProbe / ClearAfterProvedPreReleaseRollback`, generation+sequence CAS, `designated_child_hash`, injected-clock freshness, `reservation_expires_at`, `reason_code`/`operator_action`. Four phases `{in-progress, reserved, committed, interrupted}` only. **Owns the injected-clock `RestartDeadlines` policy type (fable P3-6a):** the single struct carrying the freshness / reservation / proof / bind / quiesce / rollback / grace deadlines consumed by D (freshness), E (reservation), F (standby), G (proof/quiesce/rollback/grace), I (freshness) — introduced here so no later phase re-invents it.
- **Invariant:** the record is a single-owner intent-specific store, NOT a general legal-edge engine; `Reserve`/`InterruptFromOwnedFreeProbe` require generation+sequence match; a non-owner observing `Held` cannot write; gate-off ⇒ store is never written.
- **Allowed change surface:** new record file + the gate resolver + its composition wiring (`cli/gui.go` resolve + pass to `Server`).
- **Must-not-break:** the hardened state-file DACL/atomic-write posture (`MCPHUB_REQUIRE_SINGLE_USER_HOME` gate); no other state file schema.
- **Dependencies:** none beyond baseline; first atomic-group phase.
- **Acceptance criteria:**
  - **AC-D1** — marker CAS: `Reserve` and `InterruptFromOwnedFreeProbe` succeed only on matching generation+sequence; a changed generation loses the CAS.
  - **AC-D2** — all four phases round-trip through the file (v3.1 fields only, no claim IDs / fallback ports / activation-signal / hub-release / phase-suffix fields — design §5).
  - **AC-D3** — read of absent/unknown-version/state-dir-mismatched data fails closed to "no handoff / unknown" (never a recovery-qualifying phase).
  - **AC-D4** — write/read failure surfaces a typed error the caller can fail-closed on (feeds Phase G reservation-write-failed + Phase I marker-write-failed).
  - **AC-D5** — `gui.RestartV3Enabled()` default resolves OFF; env override flips it; resolved value is stable per-process.
- **Required tests:** marker-store unit tests (CAS, four phases, freshness with fake clock, corrupt/absent read); gate-resolver default+override test.
- **Checks:** `go build/vet/test ./internal/gui/`.
- **Rollback:** revert with E–J (atomic group).

### Phase E — reservation-aware flock acquisition + tri-state probe

- **Scope / seams:** extend `internal/gui/single_instance.go` (`AcquireSingleInstanceAt` `:121`): a reservation-aware acquire that reads the marker while holding the tentative lease — reject every ordinary/third entrant on a fresh unexpired `reserved` (`ErrHandoffReserved`, release tentative lease); the designated child retains the lease only when its owner-only nonce hashes to `designated_child_hash`; `Unknown` path/marker/DACL releases the tentative lease and returns `Unknown` (never "dead"). Add the typed `ProbeGUIOwnerLease(record) -> Held(reason) | Free(owned_probe_lease) | Unknown(error)` where `Free` means the caller actually holds the flock and releases it on every path; raw `reserved` inside `reservation_expires_at` maps `Held(ErrHandoffReserved)`.
- **Invariant:** the marker never authorizes a kill/bind-takeover/eviction; every tentative lease is released on the reject/unknown paths; gate-off ⇒ acquire is the current single-shot behavior (no marker read).
- **Allowed change surface:** `single_instance.go` + the `SingleInstanceLease` seam; NOT `cli/gui.go`'s acquire callers yet (F/G/I wire them).
- **Must-not-break:** current `AcquireSingleInstanceAt` behavior when no marker/gate-off; `TryActivateIncumbent`/`--force --kill` identity gates; the pidport rendezvous write.
- **Dependencies:** Phase D (marker read).
- **Acceptance criteria:**
  - **AC-E1** — `TestRestartV3_ReservationRejectsThirdEntrantAndDesignatedChildWins`: a fresh `reserved` rejects a third entrant with `ErrHandoffReserved`; the nonce-matching designated child retains and proceeds.
  - **AC-E2** — `TestRestartV3_RawReservedFreeFlockMapsHeldDuringWindow`: raw `reserved` + a momentarily-free OS flock returns `Held`; GUI spawn count is zero.
  - **AC-E3** — probe returns `Unknown` (releasing any tentative lease) on path/marker/DACL/lock uncertainty; never "dead," never a holder proof.
  - **AC-E4** — `Free` is an owned lease released on success/error/cancel/timeout (no probe-release-reacquire gap).
  - **AC-E5** — gate-off / no-marker acquire is byte-identical to today (regression).
- **Required tests:** the two named tests above + probe tri-state unit tests (fake marker + fake flock).
- **Checks:** `go build/vet/test ./internal/gui/`.
- **Rollback:** atomic group.

### Phase F — child half: standby + authenticated readiness + activate-on-flock + Commit

- **Scope / seams:**
  - `internal/gui/ping.go` — ADDITIVE challenged standby readiness (new challenged surface binding `{handoff_id,generation,sequence,pid,port}` to a MAC; existing `/api/ping` `{ok,pid,version}` `:20-24` byte-unchanged).
  - `internal/gui/gui_restart_protocol.go` (child half) — `SpawnedGUIChild` child view, `AuthenticatedReadinessSession` consumer, the single activation barrier. **Nonce transport pinned (fable P3):** the nonce is a one-shot **owner-only DACL-hardened FILE**, whose PATH (not the secret) is conveyed via the existing handoff env — NOT raw Windows handle inheritance. Rationale: `startDetachedSupervisorTolerant`'s corp-host `ERROR_ACCESS_DENIED` fallback re-spawns with stripped flags and could drop an inheritance list, silently breaking a handle-inheritance scheme; a path-in-env + owner-only file is compatible with BOTH spawn paths. The nonce VALUE is still NEVER in argv/env/logs/durable-raw.
  - **Child-side committed publication (fable P3-6b, assigned to F):** on activation the child publishes design §0's `gui-restart-lock-acquired` (immediately before activation) and its own `committed` on its broadcaster (design §8) — these events are owned by Phase F.
  - `internal/cli/gui.go` — child startup (behind gate + handoff env `selfRestartHandoffEnv`/`SelfRestartHandoffEnv`, `gui.go:65`/`gui_self_restart.go:69`): build STANDBY on the target port and serve only challenged readiness, gate hub/tray/browser/pollers/supervisor-adoption/mutable background work behind FLOCK ACQUISITION (not a parent signal), then activate full immediately and write `Commit`. Fold the P3 fix: bounded child-side `Commit` retry while alive + a `gui-restart-commit-write-failed` event (mirror the existing `gui-restart-interrupted-marker-write-failed` posture) so a failed `committed` write does not leave a healthy activated child classified as a wedged holder.
- **Invariant:** the child performs NO mutable-runtime side effect before the flock; on flock acquisition it activates immediately with no hub-release/parent-signal wait; a parent-handle death without a matching `reserved`, or standby-deadline expiry, closes standby and exits (never improvises ownership from `in-progress`); a late standby child that finds a changed/`interrupted` marker releases any tentative lease, closes standby, and exits.
- **Allowed change surface:** `ping.go` (additive), `gui_restart_protocol.go` (child half), `cli/gui.go` (child startup path, behind gate). Gate-off child = current bounded-retry acquire, unchanged.
- **Must-not-break:** existing `/api/ping` callers + `ping_test.go`; the current handoff-env bounded-acquire path when gate-off; the os.Exit / adopt-live-supervisor discipline (`gui_self_restart.go:39-44`) so the fleet survives.
- **Dependencies:** A (argv/port), B (`GUIListenerOwner.BindStandby/ServeFull`), D (marker `Commit`), E (reservation-aware acquire + nonce-hash retention).
- **Acceptance criteria:**
  - **AC-F1** — child standby has zero mutable side effects until flock acquisition (hub/tray/browser/poller/mutator counters remain zero).
  - **AC-F2** — child activates immediately on flock acquisition (full handler opens without a hub-release or activation signal).
  - **AC-F3** — challenged standby ping matches PID+generation+target-port+proof; ordinary `/api/ping` remains byte-compatible; a PID-reuse spoof cannot authorize the wrong child (`TestRestartV3_NonceRetainedHandleDefeatsPIDReuseAndNeverUsesEnvironment` — child side: nonce never in argv/env).
  - **AC-F4** — `Commit` writes `committed`; on write failure, bounded retry while alive then emit `gui-restart-commit-write-failed` (P3 fix) — a healthy activated child is never left classified as a wedged holder.
  - **AC-F5** — a late standby child rejecting a changed/`interrupted` marker releases, closes standby, exits (fences without a takeover).
- **Required tests:** child-side unit tests via seams (fake lease, marker store, clock, listener owner); `TestRestartV3_ChildActivatesImmediatelyAndInitialHubBindRetriesFromNilThroughExistingDriver` (child activates; initial hub bind enters Phase C's driver); a `Commit`-write-failure test asserting the bounded retry + event.
- **Checks:** `go build/vet/test ./internal/gui/ ./internal/cli/`; frontend untouched.
- **Rollback:** atomic group.

### Phase G — parent coordinator + pre-release rollback + parent hub close + 202/2xx endpoint

- **Scope / seams:**
  - `internal/gui/gui_restart_protocol.go` (parent half) — `RestartCoordinator`: marker `Begin(in-progress)` → spawn a retained-OS-handle child (`SpawnedGUIChild`) with an owner-only nonce → confirm the exact authenticated standby → for port-change enter `GRACE(P)` (new mutators 503, admitted mutators drain via the `GUIListenerOwner` handler-mode gate) / for same-port close only GUI listener P and confirm the child binds P → `Reserve` while still holding the flock → bounded/non-blocking target-port progress flush → close the parent's OWN hub listener through the existing hub owner (5 s cap, force-close on expiry) → RELEASE the flock immediately → post-release no-op boundary. Pre-release rollback (design §4): retain the owned lease, terminate only the exact authenticated child via the retained handle, `BindForRecovery(P)`/swap grace→full, restore admission; **on PROVABLE restoration (the common case — target port busy, confirm timeout: parent healthy and full again) call `ClearAfterProvedPreReleaseRollback` to erase the `in-progress` marker (design §4 step 4, fable P2) so no stale nonterminal marker survives a healthy parent**; on UNprovable restoration write `interrupted` + surface the §12 reason + the terminal rollback-failure branch (bounded cleanup, release the retained lease, exit; if the `interrupted` write itself fails emit `gui-restart-interrupted-marker-write-failed`, still clean up/release/exit).
  - `internal/gui/gui_self_restart.go` — rewrite the endpoint: `if RestartV3Enabled() { v3 coordinator: 202 in-progress body `{handoff_id,generation,phase:"in-progress",spawned:true,spawned_pid,restarting:true,old_port,target_port?}`; spawn failure → HTTP 200/2xx `{spawned:false,spawned_pid:0,restarting:false,spawn_error}` } else { retained v1 path }`.
  - `internal/gui/server.go` — wire the parent hub-close-at-flock-release ordering through the existing bounded hub-shutdown (`server.go:1159`/`:1199` `ShutdownHubListener`), owner-side, immediately before `SingleInstanceLease.Release`.
- **Invariant:** the rollback gate is the concrete fact `parentLeaseReleased == false`, never an inferred marker phase; a successful rollback retains the lease and rebinds P with zero reacquire; a failed rollback releases the lease and exits (never a crippled flock holder); after release the parent performs NO child-phase write/wait-gate/terminate/claim/reclaim/`BindForRecovery`/activation-signal and closes its retained handle without terminating the child; the successful-handoff exit uses the self-restart-specific process-exit boundary (skips `manager.Stop`) so the adopted supervisor/daemon fleet survives.
- **Allowed change surface:** the three files above PLUS `internal/cli/gui.go` (+ adjacent `_test.go`) — clarified 2026-07-18 ($lead, after codex `BLOCKED:prerequisite`; NOT a new expansion: Phase E's surface already defers the `cli/gui.go` acquire-caller wiring to "F/G/I wire them" and Phase F already lists `cli/gui.go`). The parent `SingleInstanceLease` lifecycle (`startGuiServerWithStartup` `defer lock.Release()` + `releaseOnceLease` dedup), the child spawn + parser-aware argv reconstruction (Phase A is cli-side), and the same-port close-then-bind composition (`runRestartV3Child`/`StartRuntime`/`deps` seam) all live in `internal/cli/gui.go`; the gui-package `RestartCoordinator` cannot import cli, so it is driven from the cli-layer parent composition (minimal — extend the existing composition, do NOT relocate lease/argv into the gui package); the gate-off branch stays v1 (retained) per §3.
- **Must-not-break:** the frontend restart consumer contract (`api.ts:932-949` throws on non-2xx BEFORE reading the body — so spawn failure MUST stay 2xx; `SectionGuiServer.tsx:70-89` reads `res.restarting` vs `res.spawn_error`); the supervisor-fleet-survives-self-restart invariant; the same-port hub-toggle rebind; Unit A guard.
- **Dependencies:** B, C, D, E, F.
- **Acceptance criteria:**
  - **AC-G1** — `TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately`: parent hub close completes/force-closes BEFORE release; child activates on acquisition; old-port grace may continue afterward without holding the hub socket.
  - **AC-G2** — `TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire`: reacquire count zero; P is full again.
  - **AC-G3** — `TestRestartV3_PreReleaseRollbackFailureInterruptsReleasesLeaseAndExits`: cleanup deadline may expire, but release count is one and process-exit is requested; `interrupted` written (or `gui-restart-interrupted-marker-write-failed` on write failure).
  - **AC-G4** — `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease`: all forbidden seam counters (Terminate/Wait-gate/ClaimRecovery/reacquire/BindForRecovery/marker-write) remain zero after release.
  - **AC-G5** — `TestRestartV3_API202RetainsRestartingField` (202 + `restarting:true`) and `TestRestartV3_SpawnFailureReturns2xxNonRestartingBody` (200/2xx friendly body reachable by the current frontend).
  - **AC-G6** — grace allowlist: only `GET /api/events` + `GET /api/gui/restart/redirect` served; every other route 503 `GUI_RESTART_IN_PROGRESS`; admitted mutators drain before release; redirect returns 202 pre-release and 200 with the confirmed loopback target only after reserve+hub-close+release (never accepts a host/URL from the child).
  - **AC-G7** — supervisor fleet survives: existing manager-stop regression guard confirms the handoff exit does not stop the adopted supervisor/daemons.
  - **AC-G8 (fable P2, added — no false destructive degrade after a healthy rollback):** after a PROVABLE pre-release rollback the marker is ABSENT (`ClearAfterProvedPreReleaseRollback` ran); a subsequent ensure-alive tick (Phase I) over the healthy full parent classifies "no handoff" and emits NOTHING — specifically it must NOT reach `Held → gui-restart-live-holder-wedged` and tell the operator to `mcphub gui --force --kill` a healthy GUI. Test: proved-rollback → assert marker absent → drive one ensure-alive tick → assert zero emit + zero mutation.
- **Required tests:** the six `TestRestartV3_*` above (design §14 items 1-6,10,12,13) driven by seams (listener owner, flock lease, child handle, clock, marker store, handler gate, hub owner).
- **Checks:** `go build/vet/test ./internal/gui/ ./internal/cli/`; CLI GUI-spawn tests (`-tags=test_state_path_env`); sweep `mcphub.exe`.
- **Rollback:** atomic group; sub-group with H (the 202 contract + consumer).

### Phase H — frontend + progress event + navigation + degrade messages

- **Scope / seams:** `internal/gui/events.go` — register `gui-restart-progress` (+ the discriminator events) `classifyEvent` rows (`:496`) for source/severity (default already maps unknown→`gui`/info, so this is polish). `internal/gui/frontend/src/api.ts` — keep the 202/2xx `restartGui` contract (`:932-949`); add an SSE `gui-restart-progress` consumer + auto-navigate on a genuine port change (`new_port` on the still-alive old-port stream → `window.location.assign`) + map registered `reason_code`/`operator_action` enums to the two exact literals. `SectionGuiServer.tsx` — coarse progress, best-effort navigation, and the two degrade messages ("GUI restart interrupted; run `mcphub gui`." / "…a GUI process still holds the single-instance lock; run `mcphub gui --force --kill`."). One restart-progress consumer component. `go generate ./internal/gui/...` to rebuild the embedded bundle.
- **Invariant:** navigation is explicitly best-effort; neither SSE nor redirect claims the child committed (post-release child ack is deleted by design-B); the frontend renders only the registered enum literals, never an arbitrary persisted command; same-port restart does not navigate (native reconnect).
- **Allowed change surface:** `events.go` (classify rows), the two frontend files + one consumer, the regenerated `internal/gui/assets/*`.
- **Must-not-break:** the existing `restartGui` throw-on-non-2xx behavior (spawn failure must be 2xx from G); the `Restart incomplete` banner path; existing `/api/events` SSE consumers (Dashboard `poller-error`, etc.).
- **Dependencies:** G (202/2xx + progress events).
- **Acceptance criteria:**
  - **AC-H1** — `TestRestartV3_GraceNavigationIsBestEffortAndNeverClaimsCommit`: navigation fires on matching `reserved`/`new_port`; no surface asserts child commit.
  - **AC-H2** — frontend keeps 202 `restarting:true` → reconnect copy and 2xx `spawn_error` → "Restart incomplete" (existing consumer behavior preserved).
  - **AC-H3** — a durable free-flock `interrupted` shows exactly the plain-`mcphub gui` literal; a live-wedged-holder discriminator (when observable) shows exactly the `--force --kill` literal; mapping is enum-driven.
  - **AC-H4** — embedded bundle regenerated (`internal/gui/assets/{index.html,app.js,style.css}` match source); frontend unit tests + typecheck green.
- **Required tests:** `TestRestartV3_GraceNavigationIsBestEffortAndNeverClaimsCommit`; frontend `npm run test` + `npm run typecheck`; Go-side embed smoke `go test ./internal/gui/`.
- **Checks:** `cd internal/gui/frontend && npm run build && npm run test && npm run typecheck`; `go generate ./internal/gui/...`; commit the regenerated assets.
- **Rollback:** atomic group; MUST revert together with G (API shape + its only consumer).

### Phase I — ensure-alive degrade-only predicate (never spawns)

- **Scope / seams:** `internal/cli/supervise_ensure_alive.go` — an ADDITIVE GUI-handoff classification branch evaluated INDEPENDENTLY of the supervisor-live early return (`runEnsureAlive` `:346-415` unchanged for its supervisor job). The exact §9 predicate: gate enabled AND schema-valid state-dir-matching record AND `phase ∈ {in-progress, reserved}` AND `now ≥ phase_deadline` (`in-progress→fresh_until`, `reserved→reservation_expires_at`) AND the reservation-aware probe returns `Free(owned_probe_lease)` → `InterruptFromOwnedFreeProbe(...)` CAS to `interrupted` + emit the free-flock message + release the lease (GUI spawn count 0); `Held(reason)` → mutate nothing + emit `gui-restart-live-holder-wedged` (`mcphub gui --force --kill`); `Unknown(error)` → mutate nothing + emit `gui-restart-owner-unknown`. On a free-probe marker-write failure emit `gui-restart-interrupted-marker-write-failed`, release, show the plain-launch instruction without claiming durability.
- **Invariant:** ensure-alive NEVER spawns/kills/binds/retries/transfers its probe lease; `committed`/`interrupted`/absent/unknown-schema/state-dir-mismatch never authorize a write or command choice; autostart intent alone never triggers it; the supervisor-recovery/autostart policy is unchanged. **Bounded probe-hold deadline (fable P3):** the probe-hold + `InterruptFromOwnedFreeProbe` CAS window is wall-clock bounded (a marker write on a DACL-broken file can hang) so a wedged tick cannot itself become a lease-holding stall — note this holder class is UNRECOVERABLE by `mcphub gui --force --kill` (its identity gate requires `argv[1]=="gui"` but the liveness task runs `supervise --ensure-alive`, so the gate refuses exit 7); the bounded deadline is the only defense and its runbook row is a Phase-J doc.
- **Allowed change surface:** the additive branch in `supervise_ensure_alive.go` (+ any tiny helper file); Phase D marker + Phase E probe consumed read-only.
- **Must-not-break:** the existing supervisor-liveness recovery (both topologies, `:346-415`); `SupervisorRunningUnderStateDir`; the fail-closed early returns; the `emitLivenessEvent` durable-log posture.
- **Dependencies:** D (marker), E (probe). Parallelizable with F/G/H once D+E land.
- **Acceptance criteria:**
  - **AC-I1** — `TestEnsureAliveGUIRecovery_ExpiredReservedFreeInterruptsAndNeverSpawns`: expired `reserved` + owned `Free` → one terminal `interrupted` CAS, zero GUI spawns/transfers; concurrent ticks cannot both hold the flock.
  - **AC-I2** — `TestEnsureAliveGUIRecovery_FreeVsHeldSelectsExactOperatorCommand`: `Free`→plain `mcphub gui`; `Held`→`mcphub gui --force --kill`; `Unknown`→neither (fail-closed `gui-restart-owner-unknown`).
  - **AC-I3** — inside the reservation window raw `reserved` stays Held/suppressed (does not reach the deadline branch); the supervisor-live early return still works unchanged.
  - **AC-I4** — a late surviving standby child rejects the ensure-alive-written `interrupted` marker (via Phase E reservation check) — no takeover/closure protocol added.
- **Required tests:** the two `TestEnsureAliveGUIRecovery_*` above + a supervisor-branch regression (existing ensure-alive tests green).
- **Checks:** `go build/vet/test ./internal/cli/`.
- **Rollback:** atomic group.

### Phase J — gate flip + v1 removal + inert matrix + docs pass (final atomic-group commit)

- **Scope / seams:** flip `restartV3DefaultEnabled` OFF→ON; replace Phase G's retained-v1 gate-off endpoint branch with the honest 503 + delete the v1 `spawnSelfRestartGUI` unsafe spawn-and-exit **body** (`gui_self_restart.go:148-202`). **Seam preservation (fable P3-4):** delete the v1 spawn *body* only — the `selfRestartSpawnFn`/`selfRestartExitFn` seam variables (or their v3 successors) SURVIVE, because the v3 coordinator needs equivalent spawn/exit seams and the `os.Exit`-skips-`manager.Stop` discipline (`gui_self_restart.go:39-44`) must carry over (AC-G7). Add the inert-matrix test; docs pass — `docs/phase-3b-ii-verification.md` (add the two manual-smoke discriminator rows + the real self-restart-and-reconnect smoke + **a runbook row for the wedged-ensure-alive-holder class that `--force --kill` cannot reap, fable P3**), update the stale `CLAUDE.md` "Hub listener hang — observability (B1, partial)" text (design §18 planner note: `runHubListenerRestartDriver`+`hubHealthTracker` already implement bounded restart + exhaustion/abandon), and **add a C6 supersession note to `decisions/2026-07-17-item3-unitB-recovery-simplify.md` (fable P3): its KEEP #5 "relaunch once" + 3-phase marker are superseded by design v3.1's degrade-only + 4-phase `{in-progress,reserved,committed,interrupted}` — two accepted artifacts must not contradict on whether ensure-alive may spawn.**
- **Invariant:** with the gate OFF the whole Unit B is fully inert (endpoint 503, zero marker writes, zero child spawns, ensure-alive predicate skipped, frontend manual guidance); with it ON the full contract suite passes.
- **Allowed change surface:** the gate default const, the endpoint gate-off branch swap + v1 deletion, the inert-matrix test, the two docs.
- **Must-not-break:** everything D–I; the atomic-release rule §3 (this is the release-cutting commit — deploy only after J).
- **Dependencies:** D, E, F, G, H, I.
- **Acceptance criteria:**
  - **AC-J1** — `TestRestartV3_FeatureGateInertMatrix`: gate OFF ⇒ endpoint 503, no marker file created, no child spawn, ensure-alive GUI branch skipped, frontend shows manual guidance; gate ON ⇒ the contract suite is active.
  - **AC-J2** — `TestRestartV3_FreeFlockInterruptedPlainLaunchRecoversEndToEnd` and `TestRestartV3_LiveHeldInterruptedForceKillRecoversEndToEnd` pass (end-to-end degrade → recovery, seam-driven).
  - **AC-J3** — `docs/phase-3b-ii-verification.md` carries the two manual-smoke discriminator rows + the self-restart-reconnect smoke + the wedged-ensure-alive-holder runbook row; the stale CLAUDE.md B1 text is corrected; the decision-doc C6 supersession note is added. **Env-split smoke note (fable P3):** the manual-smoke rows must either set `MCPHUB_GUI_RESTART_V3` at the `\mcp-local-hub-liveness` TASK level (not just the operator shell) so the ensure-alive predicate sees the gate during pre-flip smoke, OR explicitly record the GUI-process-vs-task env skew (gate resolves OFF in the task → ensure-alive won't classify markers a smoke handoff writes; bounded, gate-off acquire ignores stale markers per AC-E5).
  - **AC-J4** — full local gate green: `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...` + `go test -tags=test_state_path_env ./internal/api/ ./internal/cli/` (CLAUDE.md Step 1); frontend build/test/typecheck; `mcphub.exe` swept.
- **Required tests:** `TestRestartV3_FeatureGateInertMatrix`, the two end-to-end degrade tests, and a full-suite pass.
- **Checks:** the full CLAUDE.md Step-1 pre-push gate + Step-2 sweep; `go generate` if any frontend touched.
- **Rollback:** flip `restartV3DefaultEnabled` back OFF (→ 503 + manual guidance) for a shipped rollback; revert D–J for a code rollback.

---

## 5. Test strategy — honest about the known limit

**All handoff LOGIC is unit-testable WITHOUT a live GUI kill**, via the design §14 seams driven by fakes + a fake clock:

| Seam | Existing / new | Deterministic control |
| --- | --- | --- |
| `GUIListenerOwner` | new (Phase B) | standby/full/grace mode, listener close, exclusive rebind |
| `SingleInstanceLease` flock | extends `single_instance.go` | release/acquire, raw-reservation Held mapping, owned-probe interrupt + release |
| `SpawnedGUIChild` handle | new (Phase F/G) | nonce proof, pre-release exact terminate/wait, detach-at-release |
| Clock | new injected `RestartDeadlines` | proof/bind/quiesce/reservation/rollback/grace/freshness deadlines |
| `HandoffMarkerStore` | new (Phase D) | four phases, reserve/interrupt CAS, reason/action, write/read failure |
| Handler-mode gate | new (Phase B) | in-flight mutator drain, new-request 503, grace allowlist |
| Hub owner | existing (Phase C entry) | parent close before release; initial-bind request from nil; unchanged retry/exhaustion |
| SSE/redirect | existing broadcaster (`events.go:141`/`:350`) | pre-release flush, fixed grace, best-effort navigation |
| Ensure-alive | additive (Phase I) | Held/Free-owned/Unknown, zero spawns, exact free/held discriminator |
| `selfRestartSpawnFn`/`selfRestartExitFn` | EXISTING (`gui_self_restart.go:151-154`) | swapped so the handler test never spawns a real process nor exits the test binary |

20 runnable contract tests total (design §14, down from v2.2's 35) — the `TestRestartV3_*` and `TestEnsureAliveGUIRecovery_*` names in the per-phase ACs. The frontend logic is unit-tested (`npm run test`) + typechecked; navigation/degrade mapping is enum-driven and testable without a browser.

**What stays MANUAL-SMOKE (Playwright cannot kill its own GUI), recorded in `docs/phase-3b-ii-verification.md` at Phase J:**
- The real two-process handoff end-to-end (spawn standby child, confirm, reserve, release, child activates on the same/new port, browser reconnects/navigates).
- Both degrade discriminator outcomes on a real host: a dead/free parent-child handoff restored by plain `mcphub gui`, and a live wedged holder restored ONLY through the existing identity-gated `mcphub gui --force --kill`. Neither smoke permits an ensure-alive GUI spawn.
- The port-change auto-navigation against a live browser.

The Windows-only E2E job (per CLAUDE.md) covers the non-kill surfaces; the kill-and-reconnect handoff is manual because the E2E fixture spawns its own binary and cannot self-terminate mid-test.

---

## 6. Rollback groups

- **Group R1 (the whole feature) = Phases D–J** — ONE feature-gated rollout+rollback unit. Shipped rollback = flip `RestartV3Enabled()` OFF (→ 503 + manual guidance). Code rollback = revert D–J together. Deploy/version-bump only AFTER J (§3).
- **Sub-group R1a = G + H** — the 202/2xx API contract and its single frontend consumer land/revert together.
- **Independent = A, B, C** — each behaviour-preserving/ungated and independently reversible; each MAY ship+deploy on its own before the atomic group.

---

## 7. Blast-radius guards (currently-working behavior that must not regress)

1. **Same-port hub-toggle restart (`hub_listener.go` reset/rebind).** Phase C only ADDS an `initial-bind-failed` entry cause at the nil-component guard (`:265-269`); every other nil-component entry still stop-drives; backoff/window/consecutive-cap/same-port-wait/exhaustion + event ids unchanged. Guard: `hub_listener_restart_test.go` + `hub_listener_restart_windows_test.go` stay green (AC-C2/C4).
2. **Unit A guard (§7, shipped PR #559 / `b18ed154`).** Not re-planned, not touched. `internal/api/hub_port_dependencies.go` + its two fail-closed callers stay as-is; the `--reset-port` exit-8 gate and `preservePortOnReloadHandlerFailure` policy unchanged. Guard: do not edit those files; Unit A tests stay green.
3. **`hub_listener.go` reset mechanism.** The Phase C amendment is bounded to the nil-component initial-bind path; the reset/rebind mechanism is unchanged (§7: "the reset mechanism in hub_listener.go remains unchanged; the composition caller supplies the safe policy").
4. **Item-1 honest hub-aggregate health (just shipped, PR #555).** Phase C changes initial-bind from terminal `HubHealthDown` to `HubHealthRecovering`+retry, but exhaustion still ends in honest `HubHealthDown` (AC-C3) so item-1's degraded banner still fires on true down; recovering→down must stay reachable.
5. **Supervisor/daemon fleet survives self-restart.** The v3 successful-handoff exit uses the self-restart-specific process-exit boundary (skips `manager.Stop`, `gui_self_restart.go:39-44`) so the adopted supervisor survives. Guard: AC-G7 + the existing manager-stop regression guard + §15 invariant.
6. **Manual launch precedence + the working "Restart GUI" button.** Phase A preserves `explicit → valid persisted → 0` (AC-A4). The button never regresses mid-rollout because Phase G's gate-off branch stays v1 until Phase J (§3), and the atomic group is not deployed until J.
7. **The frontend restart consumer contract.** `api.ts:937-947` throws on non-2xx BEFORE reading the body, so spawn failure MUST stay 2xx (AC-G5); `SectionGuiServer.tsx:70-89` (`res.restarting` vs `res.spawn_error`) is preserved (AC-H2).

---

## 8. Non-goals / deferred (design §18)

- No zero-downtime same-port handover; the irreducible same-port window is accepted.
- No `SO_REUSEPORT` dual-bind (rejected §16-B — two full owners forbidden).
- No automatic rewrite of hand-pasted group `/g/` URLs (group-URL reconcile stays operator manual).
- No permanent rendezvous port or long-lived GUI health watcher (the no-GUI-watcher baseline stays, outside the handoff).
- No change to supervisor ownership, daemon restart policy, or the hub bind transaction.
- No bare-PID termination; no post-release parent/child arbiter, recovery claimant, self-advance, activation signal, hub-release phase, post-release kill/reacquire, fallback listener, ensure-alive GUI spawn, or automatic takeover/relaunch.

---

## 9. Open items for the architect (do not block implementation; confirm before Phase D/J)

- **OPEN-1 (Phase D/J) — RESOLVED by $lead 2026-07-17:** `gui.RestartV3Enabled()` = package const default (OFF→ON at J) + `MCPHUB_GUI_RESTART_V3` env override, NOT a gui-preferences key — mirrors the shipped `MCPHUB_STRICT_JOB_PROTECTION` precedent (rollout/rollback gate, not a user preference). fable confirmed this shape (subject to the P3 env-split smoke note now folded into AC-J3).
- **OPEN-2 (Phase C) — RESOLVED by $lead 2026-07-17:** ship the hub `initial-bind-failed` driver-entry UNGATED as a standalone robustness fix; the handoff-only parent-hub-close ordering stays inside the gated coordinator (Phase G). fable independently supported the ungated split (strictly-better robustness, honest exhaustion preserved).
- **Naming:** use the design v3.1 test name `TestRestartV3_PortArgvMatrix`; the port decision-doc's `TestRestartV2_PortArgvMatrix` is stale.

### fable pre-implementation review (2026-07-17) — applied
A read-only architecture-review pass over this plan (before code) verified every cited seam anchor at baseline `0e22d6c6` and confirmed the D–J atomic-group / gating / rollback logic sound, Phase B behaviour-preserving, Phase C's consumer premise, Phase I cannot-spawn-by-construction, and full KEEP/CUT faithfulness. Two P2 plan-text holes were fixed IN THIS DOC: **Phase C** (cause-transport plumbing for the bare `hubRestartCh` + the taken-state that stops the driver dying after one retry → AC-C1b; AC-C4 port:0 qualification) and **Phase G** (marker-clear on provable rollback → AC-G8, else a false `--force --kill` on a healthy GUI). Six P3s folded into F (nonce-file transport + committed publication), D (`RestartDeadlines` owner), I (bounded probe-hold deadline + the unreapable-holder note), and J (v1-body-only seam preservation, decision-doc C6 supersession note, env-split smoke). One process note for $lead: the working tree already carries codex Sol's live A/B/C drafts (verified: tree was code-clean at 18:54, so these are the in-flight foundations implementation, NOT stale pre-existing drafts) — verify codex's output against the A/B/C ACs, do not grandfather.

---

## Gate decision: PASS

Each phase is small enough to implement and review independently; file scope, allowed change surface, nearby smoke coverage, tests, checks, and AC-IDs are explicit per phase; parallelism (A/B/C independent; I after D+E) is used only where write boundaries are fixed; the atomic-release rule protects the working button; the plan contains no implementation code. Two non-blocking architect confirmations (OPEN-1, OPEN-2) are flagged, neither gating the start.

**Recommended next role sequence:** `$backend-engineer` implements Phase A → B → C (each independently reviewable, ungated) with `$qa-engineer` gating each against its ACs; then the atomic group D → E → (F ∥ I) → G → H → J behind `gui.RestartV3Enabled()`, with `$qa-engineer` verifying the contract suite and `$architecture-reviewer` gating the `GUIListenerOwner` seam (B) and the coordinator (G) before the Phase-J flip. Route the Phase-J docs pass through `$knowledge-archivist`. Confirm OPEN-1/OPEN-2 with `$architect` before Phase D.
