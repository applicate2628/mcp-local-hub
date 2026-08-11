# Closure — Windows console is explicit opt-in and the live hub is restored

Closed: 2026-08-11T15:34:49Z
Outcome: DELIVERED — published commit `b87dc8ddc30d4aba815790f6a5a8b88fb37884c1` is installed, the supervisor/GUI/configured MCP fleet recovered, and the default Windows launch contract produced no visible console during the full live observation.
Evidence: independent QA PASS in `qa-final-r2.md`; exact integration/publication PASS in `implementation-integration-final.md`; live deployment PASS in `deployment-live.md` SHA-256 `8686302E0660FFA9D49D4B16807EAEC4AD0C68FC2108984A02B328AD49FAE249`; installed binary SHA-256 `193969E1B34ED816313BB4C3EE516288E4BB0FCDA8F0CFE898B3535330CDC2E6` reports commit `b87dc8dd`.
Residual risk: native macOS execution is operator-parked because no target exists. The generic idle-server health probe may report HTTP 502 while the actual connected CodeGraph MCP remains healthy; the separate lifecycle/diagnostic bug records remain open. The canonical prior binary is retained by the upgrade owner for rollback.

## Delivered outcome

The exact leading startup token `--debug-console` is the only parent-console opt-in. Ordinary GUI, supervisor, daemon, scheduler, helper, npm, install, upgrade, and restart paths remain console-free by default. Windows product candidates are admitted as PE subsystem 2 before promotion.

The repository commit was pushed to `master`, built through `build.ps1`, admitted, and installed through the canonical `install --upgrade` owner. The replacement GUI and supervised fleet reached readiness. A single 646.906-second monitor completed 21 of 21 samples with stable critical process identities, HTTP readiness, functioning CodeGraph MCP, zero correlated visible windows, and zero global or correlated `ConsoleWindowClass` windows.

## Archive location

`work-items/archive/2026-08/2026-08-11-windows-console-opt-in-r2/`

## Retrospective

- The strict staged allowlist and remote-drift proof preserved unrelated dirty work while three PRs landed concurrently.
- Target Windows observation, immutable-baseline comparisons, and native WSL verification separated task regressions from pre-existing broad-suite defects.
- The canonical upgrade correctly refused while the old GUI held the destination; following its explicit recovery kept the supervisor and managed daemons available until handoff.

## Terms and Abbreviations

- GUI: Graphical User Interface.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- QA: Quality Assurance.
- WSL: Windows Subsystem for Linux.

Lifecycle-schema: work-items-physical-v1
