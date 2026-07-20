# CLI first-run UX: `mcphub` should just work

Template: quick-fix (requiresLead: false) · Lead: main conversation · Opened: 2026-07-20

## Goal (operator, verbatim)
"нужно чтобы user установил, запустил mcphub и все работает!" — install → run `mcphub` → the hub +
GUI are up. Today a bare `mcphub` in a terminal dumps 40 commands instead (and several descriptions are
stale v0.4.x language).

## Evidence (verified on the CURRENT binary a0b2dded, build 2026-07-20T08:41)
1. `cmd/mcphub/main.go` `shouldAutoLaunchGUI()` routes to `gui` ONLY when there are no args AND **no console
   attached** (Explorer double-click). From a terminal (console attached) cobra prints the 40-command help.
2. Stale descriptions (v0.4.x survived the v0.5 supervisor + v0.6 watchdog removal):
   - `setup`: "…install **watchdog** task" — the watchdog was DELETED in v0.6 (`mcphub setup` no longer
     installs `\mcp-local-hub-watchdog`).
   - `status`: "state of all mcp-local-hub **scheduler tasks**" — replaced by the supervisor in v0.5.
   - `restart`: "re-run **scheduler tasks**"; `stop`: "(**tasks** and configs remain)"; `uninstall`:
     "(**scheduler** + client bindings)" — same stale model.
3. 52 registered commands, flat: internals (`daemon`, `relay`, `intent-collapse`, `hub-mcp`,
   `adopt-provenance`, `repair-state-dacl`) sit next to user-facing ones. `Hidden:` is used in only 4 files;
   there are NO cobra groups (`AddGroup`/`GroupID` absent).

## Operator decisions (asked + answered 2026-07-20)
- **Bare-run** → **launch hub+GUI** (same as `mcphub gui`), regardless of console. `--help`/`help` keep
  printing the command list.
- **Command list** → **hide internals + group the rest** (Setup / Servers / Runtime / Secrets / Diagnostics).
  - **Deviation from the recorded names, deliberate (2026-07-20).** Three of the five shipped under
    different titles: `Servers` → **"MCP servers:"**, `Runtime` → **"Running the hub:"**, `Diagnostics` →
    **"Maintenance:"** (`internal/cli/root.go:28-56`). The first two are the same concept spelled for an
    operator reading a help screen. The third is a correction, not a rename: the group holds `backups`,
    `rollback`, `cleanup`, `config`, `settings`, `version` and (post-review) `scheduler` + `reconcile` —
    those are maintenance actions, and only two of them diagnose anything, so "Diagnostics" would have
    mislabeled the group it names. Recorded here rather than silently left to disagree with the code.

## Scope
`cmd/mcphub/main.go` (shouldAutoLaunchGUI), `internal/cli/root.go` (groups), the internal commands' `Hidden`,
and the stale `Short` descriptions. Non-goals: changing what `gui` itself does; renaming commands; touching
the supervisor/daemon runtime.

## Stage log
| Stage | Owner | Status |
|---|---|---|
| Evidence + operator decisions | main conv ($lead) | PASS |
| Implement | $backend-engineer | delivered — commit `27f42953` |
| Review (architecture + adversarial QA) | 2 independent reviewers | REVISE — 9 findings |
| Revision (FIX-1 … FIX-9) | $backend-engineer | delivered — awaiting lead gate |

### Review round 1 (2026-07-20): REVISE, 9 findings, all addressed
Filed bugs: `2026-07-20-gui-spawned-supervisor-console-client` (high),
`2026-07-20-post-console-release-diagnostics-discarded` (medium),
`2026-07-20-internal-process-suite-flake-unidentified` (low, pre-existing/adjacent).

| # | Finding | Disposition |
|---|---|---|
| 1 | GUI-spawned supervisor re-attached to the console; fleet still died with the terminal | FIXED at the supervisor (attach suppressed), not by reordering |
| 2 | Post-release diagnostics written to a dead handle with no file sink | FIXED — switchable sink on the command's writers + the supervisor monitor |
| 3 | `--no-tray` silently controlled process lifetime | FIXED — `--foreground` added; policy resolved once from console state |
| 4 | Bare-`mcphub` contract change had no opt-out | FIXED — `MCPHUB_NO_AUTO_GUI=1`, outside the pure seam |
| 5 | Six hidden commands lost tab-completion, their only discovery surface | `scheduler` + `reconcile` un-hidden; other four accepted-with-rationale in code |
| 6 | Hidden-command/GroupID invariant had no enforcement | FIXED — `internal/cli/root_test.go` |
| 7 | `stderrIsValid()` became load-bearing by accident | FIXED — second consumer documented; handle behavior measured and pinned |
| 8 | Six stale premise comments (repo rule C6) | FIXED |
| 9 | Record inaccuracies (`45 → 36`, stale stage row, group names) | FIXED — true count is **45 → 35**, now **45 → 37** after finding 5 |

### Review round 2 (2026-07-20): lead returned the adjacent finding — FIXED in-branch
`2026-07-20-detached-supervisor-spawns-still-console-clients` was filed as adjacent and sent back
under the **all-return-paths discipline**: having enumerated the defect class, shipping FIX-1 on one
of its known instances would leave the rest of the class in place at known addresses.

- Enumeration corrected: `install_upgrade.go:424` is a **comment site**, not a spawn site (it calls
  `Deps.StartSupervisor` through an interface). Two real spawn configurations, one stale-text fix.
- Both fixed through the single owner `process.SuppressConsoleAttach`; inline `build` closures
  extracted to package-level constructors so each spawn config is assertable without spawning.
- Per-site external probes: every flag set ATTACHES without the marker and never attaches with it —
  **including `CreationFlags=0`**, the degraded retry, where the marker is the only surviving
  protection on a host that refuses the detach flags.
- `newRestartV3GUICmd` (RestartV3 replacement **GUI**) deliberately NOT suppressed — `--foreground`
  requires the console; the asymmetry is pinned by a test.
- 6 further mutations, each killed by the assertion.

### Review round 3 (2026-07-20): architecture review REVISE on the round-2 delta — all addressed
Confirmed holding and NOT touched: `resolveReleaseConsole` as sole policy owner (C2 closed, not relocated);
the sink-capture fix as structural; marker preserved on every retry incl. `CreationFlags = 0`; composition
with the sibling forensics branch load-bearing in the right direction (that branch keys on
`GetConsoleMode(STD_ERROR_HANDLE)`, so without suppression a console-attached supervisor would be
misclassified interactive and skip the redirect).

| # | Finding | Disposition |
|---|---|---|
| FIX-1 | BLOCKER: durable sink engaged AFTER `release()` — silent-loss window for the supervisor crash line | FIXED — engage both sinks BEFORE release; mutex deliberately not widened across the syscall |
| FIX-2 | Asymmetry justification cited Ctrl-C, already false via `CREATE_NEW_PROCESS_GROUP`; asserted in comment AND test message | FIXED — justification is terminal OUTPUT only, in both places |
| F2 | Marker inherited by every daemon + third-party MCP child | Lead decision: INTENDED. Pinned by `TestComposeChildEnvPropagatesConsoleAttachSuppression`; CLAUDE.md states the subtree contract |
| F4 | One `configureDetached` shared by two spawns with opposite console needs | Lead decision: SPLIT into `configureDetachedSupervisor` / `configureDetachedGUI`; asymmetry test now guards the CHOICE |
| F4b | Marker applied Windows-only in `internal/cli`, unconditionally in `internal/gui` | FIXED — applied in every platform variant of both supervisor configurators; one answer per platform |
| F5 | Visibility change bundled into a console-lifetime commit | Not routed (lead) — reviewer-requested, stays |

A **FOURTH** spawn site neither the lead nor I enumerated — `spawnStandaloneSupervisor`
(`internal/cli/supervise_ensure_alive.go`, the GUI-independent liveness relaunch) — was already covered
because `internal/cli` folds the marker into `configureSupervisorDetach`. That is the argument for the
folded shape, and F4 brings `internal/gui` to the same structural footing.

7 further mutations, each killed by the assertion.

## Requirement #4 (operator, added 2026-07-20): GUI must survive the launching terminal
Operator, verbatim: "при закрытии терминала, из которого я запускал `mcphub gui`, он тоже закрывается
(пропадает из трея)" + "какого хрена оно вообще привязывается к терминалу, сколько это может повторяться!"

### ROOT CAUSE (verified, `file:line`)
`cmd/mcphub/main.go:26` calls `attachParentConsoleIfAvailable()` (`cmd/mcphub/console_windows.go:34`) as the
FIRST statement of `main()`. mcphub.exe is a **Windows-subsystem** binary (no console by default, so a
double-click does not flash a black window); that helper **attaches it to the parent console** so CLI output
is visible from a terminal. Attaching makes the process a **client of that console** → on console close
Windows delivers CTRL_CLOSE_EVENT to every attached process → the GUI/tray dies.

This is NOT a regression — it is a standing design gap: the GUI path never RELEASES the console. That is why
the operator experiences it as recurring. The repo's `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` usages
(`internal/cli/gui_supervisor_owner_windows.go`, `internal/gui/supervisor_restart_*.go`) cover spawning the
SUPERVISOR / RestartV3 child — never the GUI-launched-from-a-terminal case.

### Fix options handed to the implementer
- **(a) `FreeConsole()` on the GUI path — LOW RISK, preferred first.** Detaches from the parent console after
  the GUI path is chosen → no CTRL_CLOSE_EVENT → survives terminal close. Adds NO new process and NO new
  flock/spawn path, so it cannot re-open the single-instance / RestartV3 reservation race (PR #568 area).
  Caveat to verify: the launching shell may still WAIT (prompt does not return until exit).
- **(b) Re-spawn detached + exit** — also returns the prompt, but adds a spawn path that must not
  double-acquire/orphan the single-instance flock, must not look like a RestartV3 structured child, and must
  not disturb the reservation window. Higher risk; if needed, it must be designed, not hacked.

Implementer instructed: if (a) is insufficient for the operator goal and (b) cannot be done safely as a
bounded edit, STOP and report that it needs a design pass rather than hacking it. Empirical proof required:
launch from a real console, CLOSE it, show the GUI still alive; state whether the prompt returns.

## Mutation ledger — three dead entries, and why

Three round-2 mutations now report `SETUP-ERROR`. They are recorded here rather
than dropped, because a future reader who finds three unexplained SETUP-ERRORs
cannot otherwise tell this case from a rotted suite.

**Cause:** their anchor was removed by the F4 configurator split. They targeted
the hand-applied `MCPHUB_NO_CONSOLE_ATTACH` marker at each call site; that marker
now lives inside `configureDetachedSupervisor`, so there is nothing at the call
site left to mutate.

**Superseded by** the round-3 constructor-choice mutations:
`TestSupervisorSpawnUsesTheSuppressingConfigurator`,
`TestRestartV3GUISpawnUsesTheNonSuppressingConfigurator`, and
`TestDetachedConfiguratorsDifferOnlyInConsoleSuppression`.

**Coverage went UP, not down.** The dead mutations tested "did someone remember
to hand-apply the marker at this call site". The replacements test "did this call
site pick the right constructor" PLUS "do the two constructors differ on exactly
one axis". That second property did not exist before the split and is strictly
stronger: the old form could not catch a *new* supervisor spawn written against
the shared helper — which was F4's entire regression risk.

These mutations are dead because the defect class they guarded was made
structurally unreachable. That is the good reason for a mutation to expire, and
categorically different from one dying because a test was deleted.
