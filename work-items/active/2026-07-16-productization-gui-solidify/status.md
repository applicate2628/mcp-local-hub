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

## Now

Design/discovery consilium (fable + codex Sol) grounding Phase 0: enumerate the concrete GUI/hub
roughness + a ranked hardening plan + a bounded first milestone. Then $lead executes.
