# REVISE — the Lead's root-cause diagnosis was REFUTED

Date: 2026-07-20. Adversarial lane (fable, `$qa-engineer`) gate: **REVISE — do not
build the permanent-error classifier on this evidence.** Lead accepts the refutation.

## What I (Lead) claimed, and why it was wrong

**Claim:** daemons crash-loop and stay dead because permanent spawn failures are
retried as transient, burning the 10-failure budget → quarantine → parole → same
permanent error forever. Supporting evidence: 8215 respawns across 32 daemons with
a 3-vs-782 spread.

### R1 — my load-bearing discriminator was invalid. REFUTED.

I argued: *"a supervisor restart respawns the WHOLE fleet, so deploy-driven
generations would be roughly EQUAL; the 3-vs-782 spread therefore proves per-daemon
crashes."*

`MarkSpawned` (`supervisor_runtime_tracker.go:319-338`) increments `PIDGeneration`
on EVERY spawn, and `:573` hydrates it from `supervisor-state.json` at cold start —
it is a **lifetime cumulative spawn counter**. In the log window: 9
`supervisor-start` events and 297 `daemon-spawned`, with almost every task at
exactly 9-10 spawns = the number of supervisor starts.

**The spread is REGISTRATION AGE, not health.** `codegraph-default` at gen 3 had
exactly 3 in-window spawns — it was registered *inside* the window. And
`lsp-b133f336-go`, my headline "782 crashes" daemon, had **9 spawns, 0 exits, 0
crash respawns** in the window — it is currently the *stablest* daemon in the fleet.

My inference does not survive: equal-bump-per-restart only holds for daemons of
equal age, which I never checked.

### R2 — the 8215 number is benign. REFUTED.

`supervisor-events.log.1` spans only ~1.6 days (94% of its 55k rows are
`ipc-command` spam, which rotates the 10 MB file fast). In-window spawn rate
≈168/day → 8215 ≈ 49 days ≈ birth ~2026-06-01, matching the v0.5.0-supervisor-era
state file. Consistent with deploy-heavy normal operation. **No crash epidemic is
required to produce that number.**

### R3 — the chain never completed even once on this host. REFUTED.

- **Zero `daemon-quarantined` events** in either log.
- Max `failures_in_30m` observed anywhere: **7** (serena-4f8e3c32). Threshold is 10.
- The invalid-cwd loop ran 20:55:04→20:56:08 — 7 attempts — then the
  **already-shipped** stale-workspace guard fired (`stale-workspace-skipped`:
  *"workspace path no longer exists on disk; daemon row dropped… to avoid cmd.Dir
  spawn-loop"*), the orphan reaper suppressed the pending respawn timer and
  terminated (`orphan-reap-backoff-timer-deferred` → `orphan-reap-terminate` →
  `controller-removed-intent-state-cleared` at 20:57:08).
- Total incident: **~2.5 minutes, self-healed.** serena-4f8e3c32 is absent from
  current intent.

### R5 — the fix targets the wrong layer, and largely already exists. CONFIRMED.

The stale-registration reaper I said was missing **exists and worked**:
`stale-workspace-skipped`, shipped PR #244 (2026-05-29, v0.4.10+); emitters
`internal/api/install_parsed_manifest.go:2367` and
`internal/api/serena_intent_repair.go:450`. It drops nonexistent-workspace rows at
intent-write/repair time. The residual defect is only the ≤2-minute window where the
runtime reconcile snapshot still carries the row — bounded and self-healing. A
permanent-vs-transient classifier would duplicate an existing owner's job one layer
down.

### The decisive counter-argument I missed

**"Restart does not help" ARGUES AGAINST the invalid-cwd theory.** On any ≥v0.4.10
binary, serena intent repair runs at supervisor start and drops the stale row —
observed doing exactly that. A restart would *permanently cure* invalid-cwd. The
operator's report that restart does NOT help is therefore evidence *against* my
theory, and I read it as evidence *for* it.

## What survives

**Proven mechanics (unchanged):** spawn failures DO feed the same `failures_in_30m`
budget as crash exits; the threshold and parole constants are as cited; the
"reconciler swallows spawn errors" godoc is textually accurate
(`supervise.go:3220-3222`) but **non-load-bearing** — the controller counts them
anyway.

**Pure inference, unevidenced:** everything connecting any of this to the laptop
symptom. No quarantine ever occurred on the evidenced host. The laptop's failing
daemon, exit class, and binary version are all unknown.

## What the evidence actually points at instead

**L3's finding stands and is now the strongest live lead:** 6 supervisor deaths with
**zero forensic trace** (`supervisor-exit` emitted 0 times across the window despite
8 restarts; panic handler wired and never fired; no WER record; no stderr sink).
A fleet-wide death 6× in 43 hours, unattributable.

*Note a genuine inter-lane conflict, left open rather than papered over:* the
adversarial lane assumed the 9 supervisor starts were deploy-driven ("restarts on
every deploy"); L3's forensics found only **one** deploy artifact
(`mcphub.exe.old-20260719T115901Z`) and exactly one OS reboot (Kernel-Power Id 109),
leaving 6 unexplained. R1's refutation does not depend on *why* the restarts
happened — only that 9 fleet respawns occurred — so R1 holds either way. The
attribution question is L3's and remains open.

**Competing explanations for the laptop, all undistinguished:**
(a) vault/DACL refusal — a *documented* exit-1 class in CLAUDE.md, hits
wolfram/paper-search; (b) foreign app squatting a pool port (WSAEACCES 10013 class);
(c) missing runtime dep (uvx/npx) on that machine; (d) stale npm binary predating the
v0.4.10 guard; (e) LSP bind-timeout → manual-restart churn (2 `daemon-bind-timeout`
events present here). Exit code 1 dominates the exits (34/58), and CLAUDE.md
documents a known exit-1 class — that is a better match for
restart→re-fail→re-quarantine than invalid-cwd.

## Required next evidence — no fix is admitted before this

From the LAPTOP's `%LOCALAPPDATA%\mcp-local-hub\supervisor-events.log*`:

1. `daemon-quarantined` — the `task_name` and the events immediately preceding it.
   - preceded by `daemon-spawn-failed` "directory name is invalid" ⇒ my theory
   - preceded by `daemon-exited` code=1 ⇒ one of the exit-1 classes
2. Presence/absence of `stale-workspace-skipped`.
3. `mcphub --version`.

## Lead's disposition

- L1's classifier design: **PARKED, not cancelled.** Its two-path errno finding and
  the `%v`-flattening constraint are real and worth keeping, but the defect it fixes
  is not evidenced as the operator's problem. Do not implement on this evidence.
- L4's observability design: **still admitted.** It is independent of the refuted
  diagnosis and is precisely what would have prevented me from being wrong for this
  long — the metrics existed and were discarded.
- L3's unexplained-death forensics: **promoted to the top priority.**
