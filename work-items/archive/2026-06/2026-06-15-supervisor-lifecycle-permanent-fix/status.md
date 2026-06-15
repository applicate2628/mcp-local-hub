# Supervisor lifecycle — permanent fix (GUI-spawned death + no-respawn deadlock)

Template: full-delivery (self-managed, requiresLead:false)
Orchestrator: main conversation
Opened: 2026-06-15

## Goal
Make the supervisor NEVER end up dead-and-unrecovered again (§5 of ROADMAP). Live repro
this session: a GUI-owned supervisor died (no `supervisor-exit` event), liveness deadlock
suppressed relaunch, fleet went unmanaged until manual standalone-supervisor recovery.

## Verified root cause (read-only investigation workflow wf_8f24f250-805, 9 agents + adversarial)
Full design: `tasks/wgvfaj51o.output` (this session) — result.root_cause_and_fix.
- DEATH-TRIGGER (LIKELY, not confirmed): 3 automatic supervisor-spawn sites omit
  `CREATE_BREAKAWAY_FROM_JOB` (configureSupervisorDetach gui_supervisor_owner_windows.go;
  spawnSupervisorDetached install_migration_wiring_windows.go). The MANUAL /api/supervisor/restart
  path ALREADY has it (supervisor_restart_windows.go, with a cascade-kill comment) → asymmetry.
  Premise "GUI is in a KILL_ON_JOB_CLOSE job" UNPROVEN — live IsProcessInJob probe: everything
  IN-JOB (non-discriminating; standalone supervisor also in-job yet stable). Trigger could also be
  an unrecovered event-loop panic, my own redeploy kill, or a singleton race — all leave no event.
- RECOVERY-GAP A (likely): GUI adopts an existing supervisor (spawned:false) → armSupervisorManager
  returns nil → no respawn loop → adopted-then-died is unrecovered (gui_supervisor_owner.go:273).
- RECOVERY-GAP B (CONFIRMED): liveness defers to a live GUI → deadlock (supervise_ensure_alive.go:300-315).
- RECOVERY-GAP C (CONFIRMED): autostart task runs `mcphub gui` not `supervise` → ErrSingleInstanceBusy
  no-op, Last Result -1 (autostart/windows.go:63-68).
- CHURN/panic (speculative): event-loop dispatch has no recover() (supervisor_event_loop.go:173-195).

## Fix (defense-first; recovery works regardless of the uncertain trigger). Land order is load-bearing.
- Increment 1 (independently safe): PART 1 breakaway-tolerant spawn helper (DETACHED|NEW_GROUP|
  BREAKAWAY_FROM_JOB + tolerant retry on ERROR_ACCESS_DENIED → flagless + durable warn) wired into
  ALL THREE spawn sites; PART 2d event-loop panic observability (recover→emit supervisor-handler-panic→
  RE-RAISE, mirror supervise_maintenance.go:476-487).
- Increment 2 (recovery, order 2a→2b→2c): PART 2a GUI-independent liveness relaunch verb (detached
  supervise via the PART 1 helper, acquires singleton flock, defers on upgrade/migrate breadcrumb);
  PART 2b remove liveness defer-to-GUI suppression; PART 2c liveness = sole supervisor-liveness owner
  (GUI loop stays fast-path for Spawned() only).

## Verification (mandatory)
Go regression test: self-CreateJobObject(KILL_ON_JOB_CLOSE)+spawn-via-helper+close → survive-with-
breakaway / die-without. Breakaway-rejected retry test (both GUI + upgrade lanes). Adopted-death
recovery test. Deadlock-gone test. Interlock-safety (upgrade reap-gap + migrate reap-acquire).
Panic-observability test. LIVE 10-min no-churn window + `claude mcp list`. Full repo gate +
test_state_path_env api/cli. BACK UP supervisor-intent.json before any test (subagent-wiped-intent lesson).

## State
- [completed] Root-cause workflow + live IN-JOB probe.
- [in_progress] Increment 1 (PART 1 + 2d).
- [pending] Increment 2 (PART 2a/2b/2c); review; redeploy + 10-min window.

## Next action
Read the 5 code sites (codegraph) and implement PART 1 shared helper + wire 3 sites; PART 2d recover.
