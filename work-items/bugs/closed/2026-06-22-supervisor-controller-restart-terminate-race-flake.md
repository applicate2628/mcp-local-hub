---
status: open
severity: P3
date: 2026-06-22
slug: cli-suite-preexisting-failures-force-kill-dacl-and-controller-race
context: adjacent-finding
discovered-by: mimocode PR #420 multi-layer fix (full internal/cli suite run, 2026-06-22)
---

# Pre-existing internal/cli full-suite failures: TestForce_Kill* DACL-env + a supervisor-controller restart race

Two unrelated failures surface in a FULL `go test -tags=test_state_path_env
./internal/cli/` run on this workstation. Both are PRE-EXISTING and independent
of the mimocode PR #420 change — proven by stashing the mimocode change and
reproducing the dominant failure on the clean HEAD (see "Not the mimocode
change").

## Failure 1 (dominant, environmental) — TestForce_Kill* DACL allowlist refusal

```
--- FAIL: TestForce_KillNonGUIMcphubSubcommand_RefusedBeforePrompt
    gui_force_test.go:994: exit code = 4, want 7 (kill refused at argv gate before prompt)
```

The `mcphub gui --force --kill` recovery tests (`gui_force_test.go`) build a
`gui.pidport` under the test's `R:\Temp\...` temp dir. On this host that temp
dir carries an `Authenticated Users` ACE, so the hub-mcp state-file owner-only
DACL allowlist refuses it:

```
file ...\gui.pidport not single-user safe: hub-mcp state file DACL grants read
to a SID outside {current-user, LocalSystem, BuiltinAdministrators}:
SID S-1-5-11 grants access (mask=0x00010116)
```

`S-1-5-11` = `Authenticated Users`. The DACL gate fires FIRST (exit 4,
"Malformed") before the argv/identity gate the test expects (exit 7). This is
the "sandbox-broadened %LOCALAPPDATA% / broadened-temp-DACL" scenario the
CLAUDE.md "Hardened state-file writes" runbook documents — a HOST/temp-dir DACL
condition, not a code defect. Affected (8): TestForce_HealthyIncumbent_*,
TestForce_KillHappyPath_SeamMocked, TestForce_KillRewritesPidportWithCurrentPID,
TestForce_KillRefusesNonMcphubImage, TestForce_KillNonInteractiveWithoutYesExits6,
TestForce_KillDeadPID_NoPromptExits3, TestForce_KillRaceLost,
TestForce_KillNonGUIMcphubSubcommand_RefusedBeforePrompt.

Likely remediation: tighten the `R:\Temp` (or the Go test TMP) DACL to
owner-only, OR the tests should create their pidport temp dir with an explicit
owner-only DACL rather than inheriting the broadened temp-root ACE.

## Failure 2 (intermittent, test-harness race) — supervisor controller restart

```
--- FAIL: TestSupervisorController_CleanExitDuringRestart_OwnDaemon_CompletesRespawn
    supervisor_controller_test.go:1606: manual restart fired 0 terminates; want 1
```

`supervisor_controller_test.go:1603-1607` posts `EvManualRestart`, waits for the
SM to reach `StExiting` via `waitForSMStateHelper`, then immediately asserts
`terminateCalls.Load() == 1`. The `ctrl.terminate` side-effect runs in the FIFO
loop goroutine; the test observes the SM STATE before the side-effect's atomic
increment is guaranteed visible. Passes in isolation; only flakes under
full-suite parallel scheduling pressure ("assert on a transient window without
engineering the window deterministically large" — governance Race-window
assertion discipline). Suggested fix: wait for `terminateCalls.Load() == 1`
with a bounded `Eventually`/poll instead of the bare check at line 1605.

## Not the mimocode change

- `gui_force_test.go`, `supervisor_controller_test.go` have ZERO references to
  `mimocode`, `clients.MCPEntry`, or the additive `MCPEntry.Raw` field PR #420
  adds. The mimocode adapter cannot affect the GUI force-kill DACL gate or the
  supervisor controller.
- Stashing the entire PR #420 diff and running on the clean HEAD reproduces
  Failure 1 IDENTICALLY (same `SID S-1-5-11` exit-4 refusal). Definitive: it is
  a host/temp-DACL condition, not a regression.
- The `internal/api` suite passes; the mimocode + opencode + targeted
  Scan/Extract/Backup/Rollback/Layer/LanguageServer suites all pass with the
  PR #420 change applied. Live supervisor-intent.json / workspaces.yaml were
  byte-identical before/after (state-path-env isolation held).

## Closure (2026-06-30)

Fixed in fix/open-bug-batch: polled terminateCalls instead of a bare assert after the SM-state wait (race-assertion discipline). build/vet/tests green.
