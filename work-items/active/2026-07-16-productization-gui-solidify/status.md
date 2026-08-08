# Productization — GUI/hub solidify → clean-install UX

Status: active

State: ADMITTED 2026-07-16 (user unlocked the productization gate: "дев-вариант... давай
productization погоняем"). User signal: the GUI is broadly rough ("сырое практически всё что
можно делать в GUI, включая сам хаб — даже сейчас partial").

Template: full-delivery (design-first). Orchestrator: main conversation ($lead).

## Framing (why design-first, phased)

Productization = turn mcphub from a dev tool into a product a non-developer installs in one
click (exe→auto-GUI+tray+onboarding, hide CLI, obvious workspace registration, clean-VM
first-run). Roadmap vision: `work-items/active/2026-07-16-productization-gui-solidify/2026-06-18-clean-install-ux-vision.md`. The user's gate
was "not until the dev variant is solid" — now unlocked, BUT the user says the GUI/hub is still
rough. So Phase 0 is NOT install-polish; it is **make the rough GUI/hub solid** (the prerequisite
the polish sits on).

## Phases (draft — the design consilium refines)

- **Phase 0 — GUI/hub roughness audit + hardening.** Concretely enumerate what's rough across
  every GUI surface (Servers/Migration/Add-Edit/Dashboard/Logs/Secrets/Settings/Groups/About +
  the hub aggregate + tray), rank by user-facing severity, and harden the top issues. Grounded
  signals so far: hub goes "partial" under port-squatter/lost-child conditions; the supervisor
  orphans its own daemons when its exe is renamed in place (found 2026-07-16 — a real
  updater/installer robustness gap, see memory feedback_kosyak_stage_binary_rename_aside_...).
- **Phase 1 — clean-install journey.** Clean-VM first-run observation; bare-exe → auto GUI + tray
  + onboarding; one-click Install; workspace registration made obvious; name-collision protection.
- **Phase 2 — onboarding slice + polish.** Smallest end-to-end onboarding milestone.

## Phase-0 progress

Discovery done (84 findings → 5 themes → 6 ranked items; see `roadmap.md`).

- **Item 1 — honest hub-aggregate health — DELIVERED 2026-07-16.** PR #555, squash `e7eb1cc5`,
  deployed (full restart, fleet 36). The watcher + restart driver already computed
  hung/dead/exhausted/instance-rebound transitions but only logged them, so the Dashboard painted
  every daemon green while ALL aggregated MCP was dead. Now: a `{healthy | recovering |
  needs-reconcile | down}` tracker published over SSE + `GET /api/hub/health`, a degraded-only
  banner with per-state plain-language guidance, and a Groups gate that hides the `/g/` URL only
  when the hub is not serving (a `needs-reconcile` hub IS serving on its new address, so Groups
  keeps advertising it for the re-copy the banner instructs).
  Seven review rounds (5-lane panel + 3 bot rounds, ~30 findings) turned up several
  operator-facing lies the first cut would have shipped — most notably `needs-reconcile` being
  UNREACHABLE in production (the driver emits `instance-id-changed` only while `recovering` and
  never follows it with `restarted`), and an ABSORBING false `recovering` after the driver
  permanently exits on `restart-abandoned` while its watcher keeps running. Deferred debt:
  `work-items/backlog/2026-07-16-hub-health-deferred-debt.md`.
- **Item 2 — GUI recovery for quarantined/lost-child daemons — DELIVERED 2026-07-17.** PR #557,
  squash `64b16e35`, deployed: binary swapped (`mcphub version` → `64b16e35`), supervisor + 36
  daemons restarted, GUI restarted MANUALLY (`install --upgrade` restarts the supervisor, not the
  GUI — per "GUI owns supervisor lifecycle" the GUI is started on logon/by hand). Live-verified
  against the real GUI on :9125 — `/` serves the HTML shell, `/api/status` returns the Running
  fleet, `POST /api/daemon/recover` is registered and rejects an unauthorized request (400). A
  shared `internal/daemonrecovery`
  authority (CLI + GUI + sweep classify through one owner), `POST /api/daemon/recover`, a
  first-class Recover affordance, and truthful colors from the one `status.ts` bucket classifier.
  Resolved open question: **no distinct `LostChild` wire state** — `isRecoveryEligibleState` admits
  `Quarantined` by exact value; the operation proves the lost-child condition at execution time.
  Reviewed over 11 panel rounds (codex adversarial + opus deep + sonnet mechanical sweep + fable
  arbiter) + a Codex-bot round; ~70 defects fixed. The recurring class was **kill-without-restart**,
  which surfaced FOUR times through four different mechanisms (early return → starved budget → dead
  supervisor → blocking audit flock) plus the bot's fifth angle (fail-closed state-read on only one
  of three respawn paths) — each found only because the next reviewer assumed the prior fix had
  created it. Deferred debt: `backlog/2026-07-16-daemon-recovery-followups.md` (now 8 items).
- **Items 3-6** — not started (`roadmap.md`).

## Side-effects found while executing Phase 0 (all filed, none blocking)

- `bugs/2026-07-16-supervisor-audit-log-flooded-by-status-polls.md` — **P1**: the supervisor audit
  log is 100% read-only poll noise (2000/2000 rows), so real lifecycle events are evicted by
  rotation. Found from an operator observation about the GUI "constantly refreshing".
- `bugs/2026-07-16-activate-window-ignores-no-browser.md` — a `--no-browser` instance opened an
  uninvited browser on every activate-window (the cli test suite spawns such children). FIXED +
  deployed via PR #556 (`1c29ff5c`), together with making real-process-spawning cli tests opt-in.
- `bugs/2026-07-16-old-binary-sweep-skips-unparseable-names.md` — P3: `.old-*` sweep skips names it
  cannot parse, so manual-deploy asides accumulate forever (182 MB reclaimed by hand).
- `backlog/2026-07-16-activate-window-protocol-followups.md` — the activate-window endpoint
  conflates caller intents because the wire carries none (per-request window consent).

## Item 3 progress (GUI self-restart / port-change without bricking gated URLs)

Split into **Unit A** (fail-closed hub-port dependency guard) + **Unit B** (parent-supervised
self-restart handoff).

- **Unit A — DELIVERED 2026-07-17.** PR #559, squash `b18ed154`, deployed (fleet 38).
  `ProbeHubPortDependencies` in `internal/api`; `--reset-port` refuses (exit 8) unless deps Clear;
  initial hub-startup preserves the port when gated. Bot found + fixed two my-chain missed
  (`AllClients()` drops failed-factory clients → `AllClientsWithErrors()`; a startup-snapshot
  TOCTOU into async reset). PASS `c5b6ff83`.
- **Unit B — DESIGN CLOSED + CONFIRMED 2026-07-17.** `item3-restart-design.md` converged v1→v3.1
  across ~8 rounds; a 3-voice consilium (Sol+opus+fable, unanimous B) named the decisive root cause
  — **a record CAS cannot serialize OS kills/binds/flocks**, so the fully-automatic recovery graph
  structurally kept re-minting a no-owner freeze (relocated 3× + a named 4th). design-B collapses
  the 13-phase/43-transition CAS graph to a 4-phase marker `{in-progress, reserved, committed,
  interrupted}` + DEGRADE-to-operator-visible for the rare crash-mid-handoff; `ensure-alive` is
  degrade-ONLY (never spawns), which eliminated the whole double-owner class. Decision:
  `decisions/2026-07-17-item3-unitb-recovery-simplify.md` (accepted). Final fable confirm PASS with
  one non-blocking P3 (a failed `committed`-marker write → a healthy child draws a repeated false
  `--force --kill` advisory) folded into the plan.
- **Unit B — PLAN READY 2026-07-17.** `item3-unitB-plan.md` (planner PASS). 10 phases A–J:
  A (typed `resolveGuiPort` + parser-aware argv, inert), B (`GUIListenerOwner` seam — largest,
  prerequisite), C (hub initial-bind-failure → existing restart driver, ungated robustness);
  D–J one atomic feature-gated (`gui.RestartV3Enabled()`, default OFF, flipped ON in J) rollout+
  rollback unit (marker store, reservation flock, child/parent coordinator, frontend, ensure-alive,
  gate flip). Two non-blocking architect confirms flagged: OPEN-1 (gate mechanism const+env vs
  gui-preferences key) for Phase D; OPEN-2 (ungate C) — accepted by $lead.
- **Unit B foundations A+B+C — DELIVERED + DEPLOYED 2026-07-17.** PR #560, squash `a7d05fd3`, deployed
  (build.sh from master → C:-staged `install --upgrade` → supervisor restart, fleet 36; GUI restarted
  manually PID 165212, `/api/status` → 200; installed binary `mcphub version` = `a7d05fd3`). codex Sol
  implemented all three (typed `resolveGuiPort`+argv · `GUIListenerOwner` seam · hub initial-bind→driver).
  my-verify green (build/vet + tagged api/cli/gui, re-run after every fix). Commission: fable PASS
  (line-by-line Phase B behaviour-preservation + Phase C item-1 class structurally excluded), Terra PASS
  (4 audits), Sol REVISE→fixed (`Shutdown(done==nil)` unserved-listener leak → explicit close + rebind
  regression; `net/http` Shutdown doesn't close an unserved listener). 3 fable P3 polish also fixed (async
  invalid-port log per bot #423 · initial-bind telemetry port seed · clearCurrent hoist); fable #3
  declined-with-reason (would break the close(ready)-before-Serve invariant). Sol-confirm hung (not
  capacity) → proceeded on the strength of exact-spec-fix + passing regression + green suite + the bot
  gate (bot PASS on `ab0810fc`: "no major issues", 0 inline, 0 unresolved threads). Full record:
  `item3-unitB-foundations-review.md`.
- **Option B (adversarial hub-InstanceID rotation) — DELIVERED + DEPLOYED 2026-07-18.** PR #562, squash
  `61890221`, deployed (build.sh from master → C:-staged `install --upgrade` → supervisor cold-restart,
  fleet 38; GUI restarted PID 130532; `/api/version` commit `61890221`, `/api/status` 200,
  `/api/hub/health` healthy). Canonical response to Codex-bot PR #561 (closed non-canon): keep Phase C
  auto-recovery but rotate the hub InstanceID once on a FOREIGN/unverifiable port-holder at initial bind,
  with a durable + race-safe needs-reconcile signal. Six bot rounds (R3 durable-marker + confirmed-bind,
  R4 hydration/bind serialization race + stale-live-latch, R5 skipped-clients FALSE-POSITIVE 3-way-refuted)
  + a fable commission (F1 restart-accept hydrate + F2 ticker TOCTOU). Accepted multi-tenant residual
  P1-1/P1-2 (async owner probe + forgeable identity), governed by `MCPHUB_REQUIRE_SINGLE_USER_HOME` +
  owner-only DACL. Decision: `decisions/2026-07-18-hub-initial-bind-adversarial-token-rotation.md`
  (Amendments 1-3). Follow-ups: `backlog/2026-07-18-hub-restart-path-adversarial-rotation-followups.md`,
  `bugs/2026-07-18-process-snapshot-scanner-token-too-long.md`,
  `bugs/2026-05-29-cli-supervise-ipc-tests-flaky-in-full-suite.md` (reopened).
  **`feat/gui-restart-unitb-gated` was merged with master `30cbd748` (Option B integrated) 2026-07-18 so
  Phase G builds on the live hub-security code; the one code conflict (server.go NewServer init:
  `hubRestartCh` → #562's `hubListenerRestartRequest` type + the gated `GUIReadHeaderTimeout` const) was
  resolved keeping both.**
- **Unit B Phase D — COMMITTED, NOT DEPLOYED.** Default-OFF `RestartV3Enabled`,
  `HandoffMarkerStore`, and the shared `RestartDeadlines` policy landed in `ebc12e78`; commission PASS
  is recorded in `item3-unitB-phaseD-review.md`.
- **Unit B Phase E — COMMITTED, NOT DEPLOYED.** Reservation-aware single-instance acquisition,
  `DesignatedChildNonce`, and tri-state `ProbeGUIOwnerLease` landed in `c2ee96eb`; commission PASS is
  recorded in `item3-unitB-phaseE-review.md`.
- **Unit B Phase F + Phase I — COMMITTED, NOT DEPLOYED 2026-07-18.** Phase F (restart CHILD half:
  one-shot hardened nonce consume, MAC-bound challenged standby readiness, Phase-E designated-child
  reservation acquire, activation through the AUTHORIZED shared post-bind `Server` continuation seam
  extracted from `Server.Start`, exact parent-death watch, bounded Commit) and Phase I (ensure-alive
  DEGRADE-ONLY predicate — never spawns/kills/binds; both-dead reconciled to auto-recovery messaging).
  Deep commission (fable + Sol per phase) returned REVISE with ~6 P1s across the two; ALL fixed:
  F — strict owner-only nonce consume (hard-fail broadened DACL) + unlink-on-every-path, Commit unified
  with runtime liveness (exhaustion → terminal `interrupted`, never a lingering `reserved`, so a live
  flock-holding child is never misclassified wedged), Windows-hardcoded test → `t.TempDir()` (Linux
  cross-builds), single 10s `GUIReadHeaderTimeout` owner (not the 2s bind budget), exact parent-death
  seam; I — both-dead message reconciliation ($lead-decided: do NOT suppress the supervisor-liveness
  relaunch, emit `gui-restart-interrupted-owner-recovering` instead of a contradictory `mcphub gui`),
  one total classifier deadline (supervisor-liveness never starved by a wedged marker) + Free-lease
  released only after the CAS returns (no post-release CAS commit), real-flock two-tick race test, shared
  `PidportFileLeaf` single owner. codex hit its usage limit mid-F-fix (resumed on refresh). fable hit its
  Fable-5 limit at the final confirm → Sol confirm ALL-CLOSED (fable re-joins at the Phase-J gate).
  my-verify green (build/vet + tagged api/cli/gui + `-race` on the new tests). Review records:
  `item3-unitB-phaseF-review.md` + this status entry for Phase I.
- **Unit B gated group D–J** — the feature-gated handoff protocol (default-OFF, flipped ON in J), shipping
  as ONE PR at Phase J. D/E/F/I on `feat/gui-restart-unitb-gated` (default-OFF, not deployed), now merged
  up to master `30cbd748` (Option B integrated). **Phase G next** (parent coordinator: spawn/confirm/reserve/
  parent-hub-close/release + pre-release rollback + the 202/2xx endpoint).

## Now

Items 1 + 2 + Unit A + **Unit B foundations A+B+C** delivered + deployed. Unit B gated group progress:
**Phases D, E, F, I all COMMITTED to `feat/gui-restart-unitb-gated`, default-OFF, NOT deployed** (the
atomic group ships at Phase J: gate flip → one PR → bot → deploy). **Now: Phase G** — the parent
coordinator (spawn/confirm/reserve/parent-hub-close/release + pre-release rollback + the 202/2xx endpoint),
the largest remaining phase; then H (frontend + progress + degrade messages) and J (gate flip + v1 delete
+ PR + bot + deploy). Parallel queued: **PR #561** — the Codex-bot's fail-closed revert of Phase C is
non-canon (fable security review: threat overstated, revert closes nothing); the canonical fix is Option B
(adversarial-gated `RotateHubInstanceID`), to be implemented off master and then #561 closed
(`decisions/2026-07-18-hub-initial-bind-adversarial-token-rotation.md`). Also standing:
a `2026-07-17` policy-ownership audit
(`backlog/2026-07-17-ticker-intervals-hardcoded-split-policy-ownership.md`, 4×P2 + 4×P3, the
form-driven-registry rule) surfaced from an operator question during item 2; not a Phase-0 item but
overlaps items 3-6 (truthful surfaces, config ownership) — the product-manager should decide whether
it folds into Phase 0 or becomes its own initiative.

## Active review agents (2026-07-18)

| Role | Scope | Model/effort | State |
|---|---|---|---|
| knowledge-archivist | Read-only recovery-state audit for `work-items/active/`; Phase-F artifacts remain untouched | internal agent, low effort — bounded artifact-presence/index consistency check | completed: Phase-F item review-ready; unrelated recovery drift reported |
| analyst | Phase-F diff, unchanged dependents, normal-start equivalence, child/nonce/exit/commit flow | internal agent, high effort — cross-file behavioral and security-contract trace | completed: REVISE, four contract risks |
| qa-engineer | AC-F1..F5, focused/full package checks, cross-platform and failure-path falsification | internal agent, high effort — executable regression gate over security/lifecycle paths | completed: REVISE, Windows green; Linux and two deterministic defect probes fail |
| architecture-reviewer | Listener-continuation ownership, child lifecycle, failure-state consistency, extension seams | internal agent, high effort — claim-verify against accepted Phase-F facts | completed: REVISE, four ownership/contract defects |
| security-reviewer | Nonce secrecy/lifetime, challenged readiness, parent identity/death, gate and exit fencing | internal agent, high effort — adversarial implementation review | completed: REVISE, owner-only read and nonce-lifetime defects |

## Phase-F correction run (2026-07-18)

| Agent | Role | Model/effort | Status | Launched |
|---|---|---|---|---|
| recovery-state completeness audit | knowledge-archivist | internal agent, low effort — bounded read-only artifact/index audit before implementation delegation | completed: PASS; Phase F ready, unrelated parked-item drift isolated | current session |
| six-finding Phase-F correction | backend-engineer | internal agent, high effort — security-sensitive cross-platform process/marker lifecycle correction with red-green tests | completed: PASS; six focused RED→GREEN fixes and required tagged suite green | current session |
| Phase-F acceptance matrix | qa-engineer | internal agent, high effort — claim-verify of lifecycle, portability, compatibility, and exact required commands | completed: REVISE; child bind deadline is not applied | current session |
| nonce/process security re-gate | security-reviewer | internal agent, high effort — adversarial review of strict consume, identity lifetime, cleanup, and marker classification | completed: REVISE; POSIX consume race, Commit liveness race, and nonce-path authorization gap | current session |
| security correction constraints | security-engineer | internal agent, high effort — define implementable exact invariants for consume identity, marker liveness, and nonce object authorization | completed: BLOCKED:dependency; exact opened-entry unlink is impossible against malicious same-UID rename through pathname transport | current session |

Resume point: the human selected Option B pathname transport and accepted malicious same-user rename as a
non-threat because that actor already owns and can read the nonce. The resumed `$backend-engineer` lane
completed the strict exact-length consume, canonical pre-consume path authorization, post-unlink fail-closed
checks, dedicated standby bind/close budgets, single 10-second header-timeout owner, exact Windows parent
death handle plus injected seam, runtime-serialized Commit settlement, stale-generation fail-fast behavior,
environment clearing, and cross-platform fixtures. Fresh evidence: `gofmt -l` on the Phase-F files emitted
nothing; `go build ./...` and `go vet ./...` exited 0; the exact tagged API/CLI/GUI suite passed in 212.1 s
(`api` 79.197 s, `cli` 208.010 s, `gui` 191.228 s). Phase F is corrected and ready for independent QA and
security re-gates. Phase I remained untouched by this lane; no commit is authorized or created.

## Phase-G implementation run (2026-07-18)

The human directly admitted Phase G and amended its production change surface to include `internal/cli/gui.go`
plus adjacent tests. Current `HEAD` `beadf474` is the requested `58340fd5` baseline plus only that accepted
documentation amendment; no production drift was introduced. The canonical `brief.md` now names Phase G,
AC-G1 through AC-G8, the exact verification gate, and the no-commit/no-real-spawn constraints.

| Agent | Role | Model/effort | Status | Launched |
|---|---|---|---|---|
| Phase-G recovery-state audit | knowledge-archivist | internal runtime model, low effort — bounded task-memory and Git reconciliation | completed: REVISE; Phase-G plan current, stale brief corrected by Lead, unrelated parked-item drift isolated | current session |
| Phase-G parent coordinator | backend-engineer | internal runtime model, high effort — ordering-sensitive multi-file lifecycle protocol with red/green tests | completed: REVISE after independent gates | current session |
| Phase-G affected-surface facts | analyst | internal runtime model, high effort — diff-wide concurrency, rollback, endpoint, and resource-lifetime trace | completed: REVISE; five concrete ordering/resource defects, concurrency and atomic ordering clean | current session |
| Phase-G acceptance matrix | qa-engineer | internal runtime model, high effort — AC-G1..G8 claim verification and must-not-break regressions | completed: REVISE; one-shot standby confirmation fails the startup-latency contract | current session |
| Phase-G ordering/seam review | architecture-reviewer | internal runtime model, high effort — adversarial lease/hub/child lifetime and dependency-boundary review | completed: REVISE; hub producers can republish after restart close and pre-accept cleanup is split | current session |
| Phase-G correction seam | architect | internal runtime model, high effort — synthesize five confirmed ownership/race findings into one no-layering correction contract | completed: PASS; accepted correction design recorded | current session |
| Phase-G seven-defect correction | backend-engineer | internal runtime model, high effort — TDD correction of producer shutdown, confirmation retry, listener rollback, response flush, nonce generations, duplicate requests, and post-Begin cleanup | completed: PASS; seven regressions plus exact tagged full gate green | current session |
| Phase-G deep correctness gate | Lead | internal runtime model, high effort — reconcile diff, HEAD, consumers, and independent specialist evidence | completed: REVISE; seven defects recorded in `item3-unitB-phaseG-deep-review.md` | current session |

Resume point: the backend correction for all seven deep-review findings is complete. The exact tagged GUI/CLI
gate, build, vet, formatting check, all `TestRestartV3_*` tests, and the workspace executable sweep passed on
2026-07-18. Next gate is independent `$qa-engineer` and `$architecture-reviewer` review of the corrected
working tree. The gate-OFF branch remains v1 and no commit is authorized or created.

## Phase-J implementation run (2026-07-18)

The human directly admitted Phase J and supplied the authoritative final atomic-group plan. Starting HEAD
`3b42b1e7` contains Phases D, E, F, I, G, and H plus the browser-test follow-up. Phase J is behavioral and
release-cutting, but remains local-only: no commit, push, deployment, real GUI spawn, process sweep/kill,
Graphify, `claude` CLI, `MCPHUB_GUI_SPAWN_TESTS`, or untagged API/CLI test invocation is authorized.

| Agent | Role | Model/effort | Status | Launched |
|---|---|---|---|---|
| Phase-J recovery-state audit | knowledge-archivist | internal runtime model, low effort — bounded work-item and branch reconciliation | completed: PASS for the Phase-J item; unrelated parked cbuild index/brief/roadmap drift left untouched | current session |
| Phase-J gate flip and closure | backend-engineer | internal runtime model, high effort — ordering-sensitive final gate, inert-matrix test, seam deletion, and canonical docs | completed: PASS; gate ON by default, rollback OFF fully inert, v1 endpoint body removed, docs reconciled, full local gate green | current session |

Resume point: Phase J is implemented and verified in the uncommitted working tree. The compiled RestartV3
default is ON; explicit gate OFF returns the honest 503/manual guidance and reaches neither coordinator,
marker, exit, nor ensure-alive classifier; explicit gate ON reaches the v3 coordinator. The unsafe v1 endpoint
spawn-and-exit body and helper are gone, while `restartV3ParentRuntime.Spawn`, `SpawnRestartV3GUI`,
`selfRestartExitFn`, and `RequestSelfRestartExit` preserve the v3 spawn/exit and manager-stop-bypass seams.
The first full suite exposed a gate-flip regression where the Phase-I marker read created a missing state
directory and weakened the existing supervisor probe-error guard; the classifier now returns before marker
locking when the state directory is absent, and its named regression plus the fresh full suite pass. Final
evidence: build/vet pass; `go test -count=1 -timeout 5m ./...` passes; tagged API/CLI passes; frontend build,
1,122 tests, and typecheck pass. The exact AC-J2 end-to-end test names are not present on this branch; existing
seam-driven discriminator tests pass and the two real recovery cases remain the D2.7 manual smoke. Next gate:
human review, manual Windows smoke, and separate commit authorization. No commit, push, deployment, process
sweep/kill, or real GUI spawn occurred; the frontend build produced no bundle asset diff.

## PR #563 Codex-bot round-1 correction run (2026-07-18)

The human directly admitted all three review corrections and supplied the authoritative constraints. Starting
HEAD `37b39ebc` is clean and RestartV3 is ON. The correction is behavioral and spans two implementation
owners but stays within the existing frontend progress-consumer, CLI composition, and deadline-policy seams.

| Agent | Role | Model/effort | Status | Launched |
|---|---|---|---|---|
| Round-1 recovery-state audit | knowledge-archivist | internal runtime model, low effort — bounded artifact/index consistency audit | completed: PASS; current PR #563 chain complete, unrelated parked cbuild drift isolated | current session |
| P1 readiness-gated navigation (first dispatch) | frontend-engineer | internal runtime model, high effort — asynchronous cross-origin activation probe with deterministic red/green tests | INTERRUPTED(no-artifact); no product edit landed | current session |
| P1 readiness-gated navigation (narrow retry) | frontend-engineer | internal runtime model, high effort — same bounded red/green artifact with reduced context | completed: PASS; RED 2 expected failures, GREEN 19/19, lead rerun 19/19 + typecheck + TypeScript diagnostics clean | current session |
| P2 bind and explicit-port corrections | backend-engineer | internal runtime model, high effort — timing-sensitive child budget plus explicit-vs-persisted port contract | completed: PASS; RED reproduced both defects, tagged GREEN slow-bind/port-80/deadline guards pass, Lead rerun + gofmt + Go diagnostics clean | current session |
| Round-1 acceptance gate | qa-engineer | internal runtime model, high effort — three finding regressions plus exact frontend/build/vet/tagged-suite evidence | completed: PASS; focused participant checks and final exact tagged suite green, two test-harness defects corrected and reverified | current session |

Resume point: all three PR #563 round-1 findings are corrected and QA PASS. Port-change navigation waits for
activated new-origin favicon readiness and stays on the old page on exhaustion; same-port child bind uses the
7-second `Quiesce + Bind` budget and commits after a real 3.2-second listener hold; explicit actual port 80
reaches spawn while persisted implicit-port policy remains unchanged. The generated bundle, frontend
build/1,123 tests/typecheck, Go build/vet/format/diff, 100-repeat named-pipe harness, and final exact tagged
GUI/CLI suite all pass. No commit, push, deployment, real GUI spawn, or live-fleet process kill occurred.
Next gate: human review and the next PR #563 review round; real-browser favicon semantics remain the explicit
manual-smoke residual.

## DELIVERED — item-3 Unit B merged + deployed (2026-07-19)

Supersedes the stale round-1 resume point above (which described the codex round-1 fix
"explicit actual port 80 reaches spawn"). That was REVISED during the review:

- **P2 privileged-port — Option A (uniform range, single owner).** fable's deep-review found the codex
  inline fix left `gui_restart_protocol.go:770` + `ping.go:117` still enforcing `[1024,65535]` — a
  single-owner violation whose Spawn-stub test proved nothing end-to-end. Resolved via the bot's Option A:
  `validateExplicitGUIPortFlag` front-door refusal of an explicit privileged `--port` (0=auto allowed) +
  the restart `targetPort` reverted to the single owner `validPersistedGUIPort`. So privileged `--port 80`
  is now REFUSED at startup, NOT "reaches spawn".
- **Bot round-1**: 3 findings fixed (7ed32f68). Bot round-2 on 4424d4a3: "Didn't find any major issues" PASS
  (0 inline, 0 open threads).
- **Deep-security commission** (Sol auth/trust + Terra concurrency + fable error/regression + $architect
  release-posture): PASS — gate-ON release acceptable, F3 (duplicate browser on restart) inline-fixed,
  F4/T2b kept as-is (retain-guard is the correct fail-safe, test :699), 10 findings backlogged in
  `work-items/backlog/2026-07-18-restart-v3-phasej-residuals.md` (top-2 hardening: S1 nonce-via-inherited-handle,
  T1a Bind<Quiesce<Proof<Reservation timing).
- **Merged** #563 → master `a078587c` (squash, 2026-07-19T11:52:59Z). **Deployed** via build.sh (a078587c)
  + `install --upgrade` (C: staging to dodge the D:→C: cross-volume MoveFileEx block) + GUI relaunch.
  Live binary a078587c on supervisor+daemons+GUI; dashboard consistently green; MCP 37 connected; RestartV3
  gate ON (flagship self-restart flow live).

Resume point: item-3 Unit B DELIVERED + DEPLOYED. Next: Phase-0 items 4-6 (fail-soft errors+redaction;
truthful install/migrate/snapshot; confirm/undo guardrails). Separate follow-up bug filed:
supervisor-IPC status-poll congestion (dashboard-red recurrence).
