# Productization — GUI/hub solidify → clean-install UX

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
  `decisions/2026-07-17-item3-unitB-recovery-simplify.md` (accepted). Final fable confirm PASS with
  one non-blocking P3 (a failed `committed`-marker write → a healthy child draws a repeated false
  `--force --kill` advisory) folded into the plan.
- **Unit B — PLAN READY 2026-07-17.** `item3-unitB-plan.md` (planner PASS). 10 phases A–J:
  A (typed `resolveGuiPort` + parser-aware argv, inert), B (`GUIListenerOwner` seam — largest,
  prerequisite), C (hub initial-bind-failure → existing restart driver, ungated robustness);
  D–J one atomic feature-gated (`gui.RestartV3Enabled()`, default OFF, flipped ON in J) rollout+
  rollback unit (marker store, reservation flock, child/parent coordinator, frontend, ensure-alive,
  gate flip). Two non-blocking architect confirms flagged: OPEN-1 (gate mechanism const+env vs
  gui-preferences key) for Phase D; OPEN-2 (ungate C) — accepted by $lead.
- **Unit B — IMPL IN FLIGHT.** Foundations bundle A+B+C dispatched to codex Sol (behaviour-
  preserving, feature-gate OFF, shippable-alone) as one PR to cut bot+deploy overhead; the gated
  group D–J is the next separate large effort.

## Now

Items 1 + 2 + Unit A DELIVERED + deployed; Unit B design closed + confirmed + planned. **Now: Unit B
foundations A+B+C** implementing (codex Sol) → my-verify → PR → bot → deploy, then the gated group
D–J. Also standing: a `2026-07-17` policy-ownership audit
(`backlog/2026-07-17-ticker-intervals-hardcoded-split-policy-ownership.md`, 4×P2 + 4×P3, the
form-driven-registry rule) surfaced from an operator question during item 2; not a Phase-0 item but
overlaps items 3-6 (truthful surfaces, config ownership) — the product-manager should decide whether
it folds into Phase 0 or becomes its own initiative.
