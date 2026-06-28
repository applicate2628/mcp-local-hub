# Decision: serena router-native catalog flip — DROPPED (reverted)

status: dropped
date: 2026-06-28
owners: architect (a5920370 REVISE → a80c69eb wiring-gap → a971f383 REVERT) + lead (accepted REVERT)
supersedes-clause-in: work-items/decisions/2026-06-21-serena-router-client-url-single-owner.md (its strategic "delete the legacy/dynamic split / make the serena manifest router-native" clause is now DROPPED, not just deferred)
pr: #440 (closed without merge); branch feat/serena-router-native-catalog dropped

## What was attempted
Flip the SHIPPED `servers/serena/manifest.yaml` from `kind: global` (single static `unified` daemon @9121 + 7 `client_bindings`) to `kind: workspace-scoped` + `daemon_template` (dynamic-pool shape), so NEW installs are router-native from the start and skip the manual `mcphub migrate serena legacy-to-dynamic-pool` step. Two commits: the catalog flip (3c2bfc2a) + a fresh-`workspace register` client-wiring hook through `ReconcileSerenaClientsToRouter` (93709fc2).

## Why DROPPED (reverted, not merged)
The codex bot found **6 user-facing regressions** (1 P1 + 5 P2) the full internal commission (security + qa, twice each) missed — because they verified the change in isolation, not against every CONSUMER of the catalog shape:
1. **P1 — fresh host: clients wired to a router whose daemon never spawns.** The register hook saves the registry row + wires `/serena/mcp` but does NOT run the §7.1-gated dynamic-pool first-introduce/cutover; `AutoRegisterSerenaWorkspace` returns early for the existing row and `serena_intent_repair.go` defers first-introduce when no prior `runtime_spec` → clients point at a dead router until the user finds `migrate`. (A lifecycle gap, not a wiring gap.)
2. Register-retry recovery broken (duplicate-registration check returns before the reconcile block).
3. Antigravity relay breaks (`relay --server serena --daemon unified` → no static `unified` daemon post-flip).
4. Uninstall leaves orphans (`api.Uninstall`'s cleanup loops `m.ClientBindings`, now empty).
5. `mcphub install/setup serena` + GUI Catalog Install hard-fail (`refuseWorkspaceScopedInstall`).
6. Servers-matrix can't toggle serena (`primaryDaemonName` no-daemon → "no daemons declared"; the `/serena/mcp` override is unreachable).

## Root cause / the law this records
**The shipped serena catalog shape (`kind: global` + static daemons + `client_bindings`) is a CROSS-CUTTING CONTRACT**, consumed by install, uninstall, relay, the Servers matrix, demigrate, and intent-repair. Flipping that one shape is a CONTRACT CHANGE masquerading as a one-line catalog edit — every consumer that reads the shape breaks on the flipped value. The `install.go` client-wiring override (`SerenaRouterClientURL`) is structurally inside the `for _, b := range m.ClientBindings` loop, so removing bindings makes it dead code.

**Disproportionate:** 6 confirmed regressions across 6 owners (no clean single seam — fix-all-6 = 4-6 PRs, finding #1 needs the §7.1 supervisor-lock cutover re-plumbed into install, the most safety-critical code in the repo) traded for ONE medium-priority convenience (remove one manual migrate). The repeated scope-growth (architect gate-revise → wiring-gap-fix → 6-surface-breakage) is the classic wrong-abstraction signature ([[feedback_mimocode_multilayer_reresolve_edge_mine]]).

## What stands (master, unchanged)
The happy path works today, two intact steps:
1. `mcphub install serena` (kind:global) wires all clients to `/serena/mcp` (via the binding-loop + `SerenaRouterClientURL` override) + spawns the 9121 daemon.
2. `mcphub migrate serena legacy-to-dynamic-pool` cuts over to the per-workspace dynamic pool.

The manual `migrate` after install is the supported fresh-onboarding step (already documented in CLAUDE.md + the install-and-it-works-ux epic, which records area-4 router-native as deferred).

## If ever revisited
Do NOT re-attempt as a catalog edit. Router-native-from-install must be designed as a **contract migration**: make ALL six consumers (install, uninstall, relay, matrix/demigrate, register-recovery, the first-introduce daemon-spawn lifecycle) dynamic-pool-aware FIRST (single owner per concern, not scattered `IsSerenaServer` special-cases), with the §7.1 interlock + security-reviewer gate for the lifecycle piece — a multi-PR initiative, user-prioritized, not an autonomous medium-lane pick.
