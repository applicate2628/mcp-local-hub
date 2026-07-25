# MCP front daemon — Increment 1

Template: full-delivery (reliability/architecture-critical). Orchestration weight: requiresLead.
Branch: feat/mcp-front-daemon (worktree d:/dev/mcphub-front-daemon, off origin/master 1889cff6).
Started: 2026-07-25. Operator go-ahead: yes.

## Goal
Extract the serena+LSP router data plane out of the GUI process into a
supervisor-managed `mcphub route` front daemon on a secondary port, so serena+LSP
MCP survive GUI death. Contract-neutral (no client-config change, secondary port).
Full design: work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md

## Constraint
Codex bot unavailable until Tuesday (operator). No merge until bot PASS. Build +
internal ultracode gate now; hold merge.

## Pipeline
- [x] Research/design: architect (PASS) + reliability (REVISE→resolved on the
      safety-net side) — both lenses in.
- [x] Decision record filed (proposed).
- [x] Implement Increment 1 (sonnet), SCOPED per a verified conflict (see
      below): internal/mcproute (port-bound origin guard only) + thin GUI
      adapter (all gui tests green, incl. -race) + `mcphub route` subcommand
      (reuses gui.Server directly — the full handler was NOT relocated) +
      real empirical probe (PASS — real windowsgui binary, temp state,
      GUI killed, route daemon proved to survive). Supervisor daemon
      descriptor wiring is DEFERRED (reported as a distinct remaining
      sub-step, not attempted).
      Commits: 1fa828eb (Phase 1a), a71e861e (Phase 1b), 74104237 (adjacent
      finding). Full report: see the implementer's final message in this
      session (not yet copied into a separate report.md).
      CONFLICT found + reported (not fixed unilaterally): a literal move of
      serena_router.go/lsp_router.go's HANDLER + the stateful session stores
      (serena_router_session.go/serena_router_handshake.go) is blocked by (1)
      an undeclared hard dependency on serena_router_lifecycle.go (JSON-RPC/
      idle-wake/activity logic, not in the Increment-1 file list) and (2)
      ~9+ existing gui test files directly touching the session stores'
      UNEXPORTED fields/methods — Go visibility rules make byte-identical
      preservation across a package move impossible; only a mechanical,
      compiler-verified rename would work, which is a scope decision for the
      architect, not an implementer judgment call.
- [ ] Architect/orchestrator decision needed: accept the scoped Phase-1a/1b
      delivery as Increment 1, or approve the wider rename-and-move scope for
      a follow-up Phase 1a-continued.
- [x] Verify: my empirical (build/vet/scope/no-lock-no-write in NewServer+route.go
      listener) PASS. Opus architecture-reviewer PASS-on-correctness / REVISE-on-2.
      codex Sol cross-family UNAVAILABLE (usage limit until 2026-07-28) — honest gap.
- [x] REVISE round (implementer): F1 + F2 fixed, F3 addressed. Commits:
      6f2d0d0d (F1 — SetSerenaRouterReadOnly/SetLSPRouterReadOnly nil
      AutoRegisterFn+WakeIdleFn + Config.ReadOnlyRouterMode gating
      maybePersistSerenaActivity; falsifying test mutation-proven),
      f262df68 (F2 — DefaultRouteDaemonPort 9126→9137 + guard test
      mutation-proven; NOTE the reviewer's suggested "9122" alternative was
      independently found UNSAFE — it is the serena dynamic pool's live
      "codex" member port per internal/api/serena_dynamic_pool.go — took the
      reviewer's other explicitly-offered branch, ≥9137, instead),
      b5da2a04 (F3 — persisted + strengthened probe under probe/, real
      registered workspace + real forwarded tool-call survives GUI kill).
      Two more adjacent findings filed during this round's `go build/vet/test`
      re-verification (both verified pre-existing at merge-base 1889cff6,
      unrelated to this branch's diff): 74104237 (cleanup.go panic, filed
      during the initial Increment-1 pass) and 762d9965 (leaked Broadcaster
      persist-drain goroutine races a test-hook global under `-race`,
      internal/gui hub_listener_restart_test.go family — intermittent,
      non-blocking; `go test` non-race and the touched-package suites are
      fully green).
- [ ] Re-verify PASS → Fable acceptance.
- [ ] Supervisor daemon-descriptor wiring (auto-spawn `mcphub route`) — deferred sub-step.
- [ ] HOLD merge for Tuesday's bot.

## Review verdict (Opus architecture-reviewer, 2026-07-25) — REVISE
Correct + verified: guard extraction byte-identical, scope tight, no
construction/serve write-lock-listener, read-only CALLS absent, no nil-panic in
GUI-less process, deferred-conflict real (11 test files), scoping decision SOUND.
Blocking:
- **F1 (P2): "READ-ONLY on registry + supervisor-intent" is BREACHED.**
  route.go:134/152 `SetSerenaRouterProduction`/`SetLSPRouterProduction` unconditionally
  wire `AutoRegisterFn` (AutoRegisterSerenaWorkspace / EnsureLSPRegistered → registry +
  supervisor-intent WRITE) + serena idle-sweeper `maybePersistSerenaActivity` (registry
  WRITE, reachable on happy path via restampSerenaForwardOnExit). Cutover-primitive
  omission blocks only INTRODUCE, not LIVE-ADD/persist. Dormant in Inc1 (no Q traffic),
  ACTIVATES in Inc2 → two writers → split-brain.
  **ARCHITECT DECISION (lead): the READ-ONLY constraint STANDS.** The front daemon is a
  pure forwarder for ALREADY-registered workspaces; new-workspace registration +
  activity-persist stay GUI-owned (else the split-brain the decision rejected). Fix:
  construct router deps with `AutoRegisterFn == nil` (→ immediate-503 back-compat for
  unregistered paths, which is correct) + suppress `maybePersistSerenaActivity` in the
  route process. Add the falsifying test (POST tool-call for unregistered-but-trusted
  path → assert NO registry row / NO supervisor-intent mutation).
- **F2 (P2): `DefaultRouteDaemonPort = 9126` collides with godbolt** (configs/ports.yaml:17).
  Retarget to a configs/ports.yaml-verified free port (9122 is the gap, or ≥9137);
  reconcile against ports.yaml as the single owner, not a fresh literal.
- **F3 (P3, advisory): survival probe not persisted + proved only handshake, not an
  end-to-end forwarded tool-call.** Persist the probe script+output under the work-item;
  extend to a real tool-call forward after GUI kill.

## Next action
F1/F2/F3 fixed and committed (6f2d0d0d, f262df68, b5da2a04) on the same branch,
no push/PR. Orchestrator/architect re-verifies, then routes to Fable acceptance
→ supervisor-descriptor wiring. Still HELD for Tuesday's bot.
