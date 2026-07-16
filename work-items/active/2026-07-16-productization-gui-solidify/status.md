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
- **Item 2 — GUI recovery for quarantined/lost-child daemons — DESIGNED, not started.**
  `item2-recover-design.md` (a4f83885). Open question for the orchestrator: should `LostChild`
  be a distinct status-wire state or stay covered by `Quarantined`?
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

## Now

Item 2 (GUI daemon recovery) — resolve the `LostChild` wire-state question, then implement from
`item2-recover-design.md`.
