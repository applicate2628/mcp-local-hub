# Item-3 Unit B foundations (A+B+C) — implementation + commission record

Date: 2026-07-17. Author: $lead (main conversation).
Scope: the three shippable-alone, behaviour-preserving foundation phases of Unit B (GUI self-restart),
implemented by codex Sol against `item3-unitB-plan.md` (as amended by the fable pre-impl review).

## What shipped in the diff (UNCOMMITTED as of this record)
- **Phase A** — `internal/cli/gui_port.go` (+test): typed `resolveGuiPort` (`Unset|Valid|Invalid`, sole
  `[1024,65535]` predicate) + parser-aware `RebuildSelfRestartArgv` (INERT — no production call site;
  live self-restart still copies `os.Args[1:]` at `gui_self_restart.go:179`).
- **Phase B** — `internal/gui/gui_listener_lifecycle.go` (+test) `GUIListenerOwner`: extracted the GUI
  HTTP listener (bind/serve/close/rebind + handler-mode gate) out of the monolithic `Server.Start`
  (`server.go` split). Behaviour-preserving.
- **Phase C** — `internal/gui/hub_listener.go` (+tests): gate-on initial hub-bind failure → keep
  `HubHealthRecovering` + enqueue a typed `initial-bind-failed` cause on the **retyped
  `chan hubListenerRestartCause`**; nil-component guard admits only that cause and sets `oldTaken=true`
  so it retries to the rolling-window cap (20) then honest `HubHealthDown`; every other nil-cause still
  stop-drives. Ungated standalone robustness.

## Verification (by $lead, not trusting the subagent)
- `go build ./...` + `go vet ./...` exit 0; `go test -tags=test_state_path_env ./internal/api/
  ./internal/cli/ ./internal/gui/` all `ok` (api 76.7s / cli 203.4s / gui 162.7s) — re-run independently
  after codex's own green claim.
- Phase C verified against the fable P2-C plan concern: the dies-after-one-retry class is structurally
  impossible — `TestHubListenerRestartDriverInitialBindFailureExhaustionEndsDown` asserts
  `starts == hubListenerRestartMaxAttemptsPerWindow` (=20), which a one-retry death would fail.

## Commission (diverse-family, neutral prompts, verdict-only)
- **fable — PASS.** Deepest lane. Traced Phase B `Server.Start` old-vs-new LINE BY LINE (the exact
  behaviour-preservation check assigned to Sol) → HOLDS (bind format, `s.port.Store`-after-bind,
  `close(ready)`-before-Serve, 10s header timeout, per-phase shutdown budgets, `events.Close` ordering,
  `ErrServerClosed` translation, SSE-survives-listener-close, `Activated()` reorder all safe, `s.srv`
  fully removed). Phase C item-1 absorbing-`Recovering` hazard STRUCTURALLY excluded (buffered cause
  channel + unique startup-scoped call site + driver-launched-before-signal). Phase A inertness + edge
  classification + info-leak cap verified. anti-layering CLEAN-SINGLE-OWNER. Four P3 polish findings.
- **Terra — PASS.** 4/4 narrow audits: argv inert (only test caller), single-owner range predicate +
  precedence intact, Phase C Clear-path + non-initial nil-causes stop-drive + caps/events unchanged,
  exhaustion → honest `HubHealthDown` via `hub-listener-restart-abandoned` → `hub_health.go:196-197`.
- **Sol — BLOCKED on `ERROR: Selected model is at capacity`** (181k tokens then died, zero output).
  Sol is the dedicated behaviour-preservation reviewer and a MANDATORY acceptance member; Terra
  complements, never replaces. fable (diverse family) independently performed Sol's exact line-by-line
  Phase B trace and PASSed, substantially de-risking the gap — but per the operator's standing rule the
  Sol lane is re-run on a fresh codex before the PR/bot push; NOT substituted.

## Findings + dispositions (fable's four P3s)
- **#1 (`gui_port.go` `emitInvalidGUIPortWarning` synchronous flock-taking `LogHubMcpEvent` on the
  readiness-critical startup / activation-ping path — same class as bot #423 P2) — FIXED:**
  fire-and-forget goroutine; synchronous stderr stays the reliable pre-bind surface.
- **#2 (`hub_listener.go` initial-bind restart telemetry reports `port: 0`, losing the operator's port
  anchor in the exact outage class) — FIXED:** seed `restartPort` from `api.LoadHubEndpoint().Port` on
  entering the initial-bind branch (test state-dir has no endpoint → stays 0, tests unaffected).
- **#4 (`gui_listener_lifecycle.go` `Shutdown` skips `clearCurrent` when `generation.done == nil`,
  leaving `o.current` set — a latent trap for the D-J coordinator) — FIXED:** hoisted `clearCurrent`
  out of the `done != nil` guard.
- **#3 (`server.go` "GUI listening" printed before a `ServeFull` error returns) — NO CHANGE (reasoned):**
  the error path is structurally unreachable from `Start` (fable's own analysis: nil handler /
  listener-not-owned / unknown mode cannot occur from `Start`), and fable's suggested fix (move
  `close(ready)` after `ServeFull`) would BREAK the `close(ready)`-before-Serve readiness invariant that
  the same review verified holds (`ServeFull` blocks; readiness would never fire). Declined — do not apply
  a reviewer fix that breaks a verified invariant.

## Gate state
Diff re-verified green after the three fixes (see `my-verify-ABC-fixed.log`). **PR is HELD** pending the
Sol lane on the fixed diff (mandatory acceptance member; blocked on external codex capacity, awaiting
operator refresh). No commit / push / bot trigger until Sol PASS or explicit operator go-ahead.
