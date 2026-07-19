# RestartV3 Child Coordinator Implementation Plan

> **For agentic workers:** Execute inline in the current `$backend-engineer` session. Subagent dispatch, Graphify/code-graph tools, and the `claude` CLI are not authorized.

**Goal:** Ensure an activated RestartV3 child GUI owns a restart coordinator and accepts the next restart request with HTTP 202, while preserving gate-OFF inertness.

**Architecture:** Keep restart composition at the CLI composition root. Select either the initial server or the injected child server first, then run the same gate-ON `buildRestartV3ParentDependencies` and `ConfigureRestartCoordinator` path for both. The child reuses its server-owned `GUIListenerOwner` so the coordinator and activated runtime share the same listener lifecycle owner.

**Tech Stack:** Go, `net/http/httptest`, existing RestartV3 coordinator and child-startup seams.

## Global Constraints

- Do not use Graphify/code-graph tools or the `claude` CLI.
- Do not set `MCPHUB_GUI_SPAWN_TESTS`.
- Do not sweep or kill `mcphub.exe`.
- Every CLI/GUI test command uses `-tags=test_state_path_env`.
- Do not commit.
- Do not change the reservation-aware CLI acquire or same-port bind-budget residuals.

---

### Task 1: Activated-child regression

**Files:**
- Modify: `internal/cli/gui_self_restart_handoff_test.go`

**Interfaces:**
- Consumes: `runRestartV3ChildStartup`, `gui.Server.ServeHTTP`, `gui.Server.GUIListenerOwner`.
- Produces: `TestRestartV3_ActivatedChildAcceptsSecondRestart`.

- [ ] Add a child-startup test that completes the first handoff, asserts the child runtime receives the server-owned listener, and POSTs `/api/gui/restart` through the activated child.
- [ ] Run `go test -tags=test_state_path_env -count=1 -timeout 12m -run '^TestRestartV3_ActivatedChildAcceptsSecondRestart$' ./internal/cli/`.
- [ ] Confirm RED is caused by the absent shared child composition path, not fixture failure.

### Task 2: Shared restart composition

**Files:**
- Modify: `internal/cli/gui.go`

**Interfaces:**
- Consumes: `buildRestartV3ParentDependencies`, `gui.Server.ConfigureRestartCoordinator`, `gui.RestartV3Enabled`.
- Produces: one shared initial-or-child server composition helper used by `startGuiServerWithStartup`.

- [ ] Select/create the server first, outside the gate-ON coordinator block.
- [ ] Configure the coordinator after selection for both `startup == nil` and `startup != nil`; skip it entirely when the gate is OFF.
- [ ] Reuse `server.GUIListenerOwner()` for child standby/activation ownership.
- [ ] Re-run the focused test and confirm HTTP 202 with no `spawn_error`.

### Task 3: Narrative cleanup

**Files:**
- Modify: `internal/cli/gui.go`
- Modify: `docs/phase-3b-ii-verification.md`

**Interfaces:**
- Produces: three current-v3/upgrade-bridge comments and one USER/MACHINE rollback-environment sentence.

- [ ] Rewrite the handoff environment comment, acquire helper comment, and normal-startup comment to describe structured v3 and the literal `"1"` old-v1-binary upgrade bridge.
- [ ] Add the D2.7 rollback sentence requiring USER/MACHINE or scheduled-task environment configuration for a shipped rollback.

### Task 4: Verification and handoff

**Files:**
- Verify touched Go and Markdown files; write only the mandatory `.reports/2026-07/` session summary afterward.

**Interfaces:**
- Produces: fresh command evidence and the final backend implementation package.

- [ ] Run `gofmt` on touched Go files, then prove `gofmt -l` emits nothing for them.
- [ ] Run `go build ./...` and `go vet ./...`.
- [ ] Run `go test -tags=test_state_path_env -count=1 -timeout 12m ./internal/gui/ ./internal/cli/`.
- [ ] Inspect the final diff for scope, gate-OFF inertness, exact comments, rollback sentence, and unrelated-worktree preservation.
- [ ] Do not commit.

## Terms and Abbreviations

- CLI — command-line interface.
- GUI — graphical user interface.
- HTTP — Hypertext Transfer Protocol.
- P1/P3 — release-review priority levels one and three.
- RestartV3 — the current coordinated GUI restart handoff protocol.
