# Phase G Deep Correctness Review

## Verdict

REVISE.

## Findings

1. **P1 — hub close is not a producer barrier.** `Server.closeOwnHubListenerForRestart` swaps and shuts only the currently published component (`internal/gui/server.go:933-934`), while the initializer and restart driver remain live (`internal/gui/server.go:1121-1197`) and can publish a new component (`internal/gui/server.go:1172-1183`, `internal/gui/hub_listener.go:474-544`) after the parent releases the GUI lease (`internal/gui/gui_restart_protocol.go:301-305`). The restart boundary needs the normal shutdown path's cancel/join/take semantics under one Server owner.
2. **P1 — authenticated standby confirmation is one-shot.** The coordinator supplies a two-second context (`internal/gui/gui_restart_protocol.go:244-255`), but `ConfirmAuthenticatedStandby` performs one request and returns the first connection failure (`internal/gui/gui_restart_protocol.go:529-531`). A child that binds after the first dial but before the deadline is rolled back. Retry transient not-ready results within the supplied deadline; fail fast on authenticated mismatch.
3. **P1 — same-port rollback assumes the listener was closed.** `EnterGrace` installs grace before it can time out (`internal/gui/gui_listener_lifecycle.go:152-162`), and `CloseListener` can return before clearing `current` (`internal/gui/gui_listener_lifecycle.go:214-240`). Both errors route to rollback (`internal/gui/gui_restart_protocol.go:258-281`), which unconditionally calls `BindForRecovery` (`internal/gui/gui_restart_protocol.go:338-343`); binding rejects an existing `current` (`internal/gui/gui_listener_lifecycle.go:89-93`), converting a recoverable parent into terminal release and exit. Track whether close completed: restore the existing listener when retained; rebind only after confirmed close.
4. **P1 — the 202 body is not flushed before the exit boundary.** `Start` launches the continuation before returning (`internal/gui/gui_restart_protocol.go:219-225`); after grace drains the handler, the continuation can release and call `Exit` (`internal/gui/gui_restart_protocol.go:299-317`). The v3 handler only encodes the 202 body (`internal/gui/gui_self_restart.go:198-200`), so process exit can win against `net/http`'s post-handler socket flush and `fetch().json()` fails. Flush the accepted response while the request is still admitted, or add an explicit response-ack barrier before handoff continuation may exit.
5. **P2 — the fixed nonce leaf permits cross-generation theft after rollback.** Every generation writes the same leaf (`internal/gui/gui_restart_protocol.go:183-184`); prepare/reserve rollback zeroes only memory (`internal/gui/gui_restart_protocol.go:278-295`) and successful rollback resets the guard without removing the file (`internal/gui/gui_restart_protocol.go:353-357`). A delayed unproven child from generation A can consume/unlink generation B's replacement nonce. Use a generation-bound canonical nonce leaf and remove that generation's leaf on every terminal path.
6. **P2 — duplicate restart is reported as spawn failure, not in-progress.** The mutex correctly rejects a second `Start` (`internal/gui/gui_restart_protocol.go:131-138`), but the endpoint maps that state to HTTP 200 with `spawn_error` (`internal/gui/gui_self_restart.go:171-180`); the frontend therefore reports restart failure while the first handoff is still running (`internal/gui/frontend/src/components/settings/SectionGuiServer.tsx:70-78`). Return the active handoff as HTTP 202 with `restarting:true`.
7. **P2 — post-Begin pre-accept cleanup can leave a false live-handoff marker.** Several `Start` branches discard `ClearAfterProvedPreReleaseRollback` or nonce-removal errors (`internal/gui/gui_restart_protocol.go:171-216`). A cleanup I/O failure returns a non-restarting 200 while the healthy parent retains the flock and a stale nonterminal marker later drives false wedged-owner guidance. Consolidate post-Begin cleanup, close every acquired resource, and do not report ordinary spawn failure until marker and nonce cleanup are proved or terminal failure policy runs.

## Clean Areas

The `c.run` mutex prevents generation interleaving; `grace.released` uses ordered atomic Store/Load; the production `releaseOnceLease` composes coordinator release with the CLI defer; authenticated-versus-unproven child termination authority is correct; reviewed contexts cancel; gate-OFF retains v1; 2xx spawn-failure shape and the existing frontend fields remain compatible.

## Terms and Abbreviations

- CLI: Command-Line Interface.
- HTTP: Hypertext Transfer Protocol.
- P1/P2: Finding priority levels.

