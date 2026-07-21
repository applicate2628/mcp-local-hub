# Delivery plan — supervisor reliability

Lead-authored 2026-07-20 after six investigation lanes. Ranked by
(operator-visible reliability gain) ÷ (blast radius), with the honest note that
the original diagnosis driving this work-item was **refuted** — see
`REVISE-diagnosis-refuted.md`. Nothing here rests on it.

## Ranking rationale

The operator mandate is "must not crash AT ALL". The evidence says the dominant
multiplier is **the supervisor dying**, not daemons dying: 8 of 9 supervisor
starts in a 42.4h window had no `supervisor-exit` row, and each cost ~29 daemon
respawns (259 of 297 in-window spawns were fleet restarts, only 38 were
crash-driven). So supervisor survivability and supervisor forensics outrank
per-daemon work.

## P0 — in flight

### P0.1 Supervisor death forensics (branch `feat/supervisor-death-forensics`)
Owner: dispatched implementer. Closes L3's top finding: 6 unexplained deaths with
zero trace. stderr sink + goroutine panic capture at every long-lived entry point
+ heartbeat row (healthy-and-quiet became indistinguishable from dead after
`835ee3e4` removed 96% of log volume). **A crash you cannot attribute is a crash
you cannot eliminate** — this gates the credibility of everything downstream.

### P0.2 Serena readiness-timeout inversion (branch `fix/serena-readiness-timeout-inversion`)
Owner: dispatched implementer. Inner `HealthTimeout` 30s vs outer
`StartupBindDeadlineSeconds` 120s; the child self-kills before the supervisor's
budget is reachable. **The only live, accelerating defect found: 6 failures in
June → 157 in July.** Includes herd mitigation — six serena workspaces cold-start
in lockstep and all miss the same fixed budget within ~250ms; raising the timeout
alone would not break the lockstep.

### P0.3 Test-hygiene sweep ban (DONE, uncommitted)
`CLAUDE.md` Step 2 rewritten to an identity-gated sweep. Name-matched
`Stop-Process` was proven by probe table to be the single largest source of
supervisor death on the dev host — our own documented command was killing the
live fleet. Doc-only; rides with this work-item's PR.

## P1 — next, after P0 branches land (they touch adjacent code)

### P1.1 Workdir pre-spawn hold-gate
Replaces the **refuted** permanent-error classifier. Per fable D4: `os.Stat` the
workspace before create-process; on failure hold in backoff **without** a
crash-count increment and re-probe each tick, reusing the shipped
`preSpawnPortGateHold` shape (`supervisor_controller.go:3513-3521`, `:4017-4024`).
Auto-recovers the first tick after a slow volume mounts; a deleted directory stays
held with escalating severity instead of burning the budget.

Must NOT route to `errSpawnJobProtectionRefused` — that target is an *absorbing*
quarantine with no parole, so a not-yet-mounted network share (which yields the
identical errno 267) would be parked permanently on every reboot, strictly worse
than today's self-healing parole ladder. Fail OPEN for spawn on Stat
`ERROR_ACCESS_DENIED`: probed, an ordinary deny-ACE directory still spawns fine
because bypass-traverse-checking is granted to Everyone by default.

Deferred behind P0 because it edits `supervisor_controller.go`, which P0.2 is
already touching.

### P1.2 Crash observability (L4 N1+N2+N3) — with the corrected metric
- **N1** DACL-fallback dedup + rollup: 1656 rows/window → ~2. One function, no
  envelope change. Severity stays `warn` (demotion breaks a named regression guard
  on a security surface, `state_file_helper_test.go:279`).
- **N2** surface `crashes_in_window` + `pid_generation` on the proven
  `job_protection` seam (producer → IPC wire → `DaemonStatus` → `types.ts` →
  badge). **Metric corrected per L2's Gate-2 failure: rate over a bounded window,
  NOT the lifetime counter.** A dashboard built on lifetime `pid_generation` would
  report an already-extinct port-in-use epidemic as live.
- **N3** `testing.Testing()` fail-closed state-dir guard — closes the in-process
  test-leak channel. The prior fix for this (bug closed 2026-06-17) added more
  opt-in call sites; the recurrence proves opt-in does not hold. Invert the default.

## P2 — needs a decision or an owner

- **Ephemeral-port overlap persistent surface.** The remedy exists
  (`mcphub setup --fix-ephemeral-range`) but is discoverable only as a warning
  printed during setup. Given the condition caused 70% of all historical launch
  failures, a startup check + GUI badge is warranted. See
  `finding-ephemeral-port-overlap.md`.
- **IPC hello-write clusters.** Root cause is host-level, NOT supervisor
  congestion (the original bug-doc's diagnosis is falsified — corrected in place).
  Needs correlation against host state (EDR scan, handle pressure, disk stall).
  Log level: per-connection row → `debug`, add a rate-based warn.
- **L4 N7 / channel-B test leak.** Closing it requires compiling a state-dir
  override into production, weakening a documented compile-time guarantee →
  `$security-reviewer` decision, deliberately not adopted.
- **Crash-ledger persistence (N4/N5).** Requires the decision-inert constraint to
  be enforced by test, since CLAUDE.md's threat model depends on persisted fields
  not priming restart decisions.

## P3 — recorded, not scheduled

- 21 orphaned `.supervisor-state.json.tmp.<pid>.<hash>` files back to 2026-05-19,
  several zero-length — the atomic temp+rename path leaks its temp file when the
  writer dies mid-write. Weak corroborating evidence of past unclean deaths.
- `internal/process` suite flakes ~1-in-5, un-root-caused, unquarantined.
- `flapping` health state (L4 N6) — new enum on a shared 3-projection classifier,
  needs an exhaustive Go+TS consumer sweep. Do not bundle.

## Blocked on the operator

- **Laptop evidence.** No laptop claim is admissible without its own
  `supervisor-events.log*` and `mcphub --version`. Discriminator: `daemon-quarantined`
  preceded by a bind-refused/WSAEACCES signature ⇒ ephemeral-port overlap;
  preceded by `upstream not ready after 30s` ⇒ serena timeout inversion.
- **Elevated `mcphub setup --fix-ephemeral-range`** — agent cannot elevate. Must
  use the full path to `~/.local/bin/mcphub.exe`; the PATH-resolved `mcphub` is a
  stale npm 0.4.24 that lacks the flag.
- **PATH collision decision** — npm shim (`C:\nvm4w\nodejs`, v0.4.24, 2026-07-09)
  shadows the canonical `~/.local/bin` build, and a June-3 dev binary in the repo
  root shadows both when CWD is the repo.

## Uncovered lane (recorded honestly, not folded into a PASS)

Cross-family codex/Sol review: **FAILED after 5 attempts.** The sandbox policy
rejects piped read commands (`sed`, `grep`, `bash -lc`, and any `| Select-Object`
pipeline) and the models cache is corrupt (`missing field
supports_reasoning_summaries`, regenerates identically — codex v0.144.6 versus the
server's format). Two fable lanes were dispatched to cover the gap and both
returned substantive results, but codex-family diversity is absent from this
work-item's verification.
