# Productization GUI solidify — current delivery brief

Admission source: direct human PR #563 Codex-bot round-1 correction decision on 2026-07-18, grounded in
Phase-0 item 3 in `roadmap.md`, the accepted `item3-unitB-plan.md`, and the three supplied review findings.

Primary task: fix all three PR #563 round-1 findings against the RestartV3 gate-ON implementation, add one
red/green regression per finding, regenerate the embedded frontend bundle, and run the exact human-required
frontend and Go gates.

Scope: `internal/gui/frontend/src/api.ts`,
`internal/gui/frontend/src/components/settings/SectionGuiServer.tsx` and its adjacent test;
`internal/cli/gui.go`, `internal/cli/gui_port.go`, and adjacent CLI restart tests;
`internal/gui/gui_restart_record.go` and its adjacent deadline-policy test; generated
`internal/gui/assets/*` only through `go generate ./internal/gui/...`.

Approved seams: the existing RestartV3 progress consumer and best-effort navigation interface; the
CLI-composed child standby bind budget; the single `RestartDeadlines` policy owner; the distinction between
explicit runtime TCP-port validity and persisted GUI-setting validity. No endpoint wire shape changes.

Out of scope: deployment, commit, push, real GUI spawn, Graphify, the `claude` CLI,
`MCPHUB_GUI_SPAWN_TESTS`, any `mcphub.exe` sweep/kill, startup rejection of privileged explicit ports,
RestartV3 protocol redesign, or changes outside the named correction surface.

Acceptance:

- Port-change `reserved`/`new_port` waits for the new origin's full-GUI readiness probe before navigation;
  same-port never navigates; probe exhaustion leaves the old page in place and preserves guidance.
- Same-port child bind retry spans at least parent quiesce plus the post-close bind budget, so a 3-4 second
  drain no longer expires the child; the injected timeout ordering remains within the reservation policy.
- A restart callback whose actual bound port is any valid TCP port `[1,65535]`, including port 80, reaches
  spawn; persisted GUI settings keep their existing `[1024,65535]` policy.
- Existing 202/2xx progress/degrade behavior, no-commit constraints, manager-stop bypass, marker protocol,
  same-port native reconnect, and gate-ON behavior remain unchanged.

Required verification: `cd internal/gui/frontend && npm run build && npm run test && npm run typecheck`;
`go generate ./internal/gui/...`; `go build ./...`; `go vet ./...`; gofmt clean on touched Go files; and
`go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/gui/ ./internal/cli/`.

Current stage: corrective implementation. Branch `feat/gui-restart-unitb-gated` at starting HEAD `37b39ebc`
is clean and has RestartV3 ON. Integration owner: the main conversation holding `$lead`; implementation
owners: `$frontend-engineer` for P1 and `$backend-engineer` for both P2 findings; verification owner:
`$qa-engineer`, with final exact-command reconciliation by `$lead`.

Critical risks and owners: cross-origin/full-GUI readiness discrimination and bounded polling
(`$frontend-engineer`); deadline ordering and same-port retry lifetime (`$backend-engineer`); explicit-vs-
persisted port contract isolation (`$backend-engineer`); regression and exact-command evidence
(`$qa-engineer` / `$lead`).

Next action: run the three focused regressions red, implement each minimal owner-level correction, prove each
green, regenerate the bundle, run frontend checks, then the exact tagged Go/build/vet/format gates. Do not
commit.

## Terms and Abbreviations

- CLI: Command-Line Interface.
- FLOCK: The operating-system-backed GUI single-instance file lock.
- GUI: Graphical User Interface.
- P1/P2: Review finding priorities, with P1 higher than P2.
- TCP: Transmission Control Protocol.
