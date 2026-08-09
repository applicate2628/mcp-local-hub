---
title: "internal/gui hub_listener_restart_test.go races against a later test's daemonStateRootOverride cleanup via a leaked Broadcaster persist-drain goroutine"
severity: medium
found-by: backend-engineer, MCP front-daemon Increment-1 F1/F2/F3 review-response session (2026-07-25) — go test -race verification pass
affected-surface: internal/gui/events.go (Broadcaster.ensurePersistDrain/drainPersist/persistOne); internal/gui/hub_listener_restart_test.go; internal/gui/hub_listener.go (signalHubListenerRestart); internal/api/testhooks.go (SetDaemonStateRootForTest); internal/api/state_paths_prod.go
context: adjacent-finding
status: open
---

## What happened

While re-verifying `go test -race ./internal/gui/...` after the Increment-1
F1/F2/F3 review-response fixes (a71e861e + working-tree changes on
`feat/mcp-front-daemon`), the race detector intermittently flagged a data
race, sometimes on `TestHubListenerRestartDriverInitialBindOwnMcphubGUIOwnerDoesNotRotateOrReconcile`,
sometimes on `TestHubListenerRestartDriverRotationPersistsThenCancelStillNeedsReconcile`
(re-running the full package twice picked a different pair each time — the
signature of a genuinely nondeterministic scheduling race, not a fixed bug in
one test):

```
WARNING: DATA RACE
Read at 0x... by goroutine 854:
  mcp-local-hub/internal/api.daemonStateDir()
      internal/api/state_paths_prod.go:39
  mcp-local-hub/internal/api.DaemonStateDir()
  mcp-local-hub/internal/api.(*API).AppendGUIEventLog()
      internal/api/gui_event_log.go:153
  mcp-local-hub/internal/gui.(*Broadcaster).persistOne()
      internal/gui/events.go:258
  mcp-local-hub/internal/gui.(*Broadcaster).drainPersist()
      internal/gui/events.go:246
  ...ensurePersistDrain.func1.gowrap1()
      internal/gui/events.go:158

Previous write at 0x... by goroutine 852:
  mcp-local-hub/internal/api.SetDaemonStateRootForTest.func1()
      internal/api/testhooks.go:149
  testing.(*common).Cleanup.func1()
  ...

Goroutine 854 (running) created at:
  ...ensurePersistDrain.func1()
      internal/gui/events.go:158
  sync.(*Once).doSlow()
  mcp-local-hub/internal/gui.NewServer.func1()
      internal/gui/server.go:954
  mcp-local-hub/internal/gui.(*hubHealthTracker).set()
      internal/gui/hub_listener.go:80/182 (signalHubListenerRestart)
  mcp-local-hub/internal/gui.TestHubListenerRestartDriverHydratesReconcilePendingFromAcceptedComponent()
      internal/gui/hub_listener_restart_test.go:211
```

## Root cause (mechanism, not yet a fix)

`Broadcaster.ensurePersistDrain` (`internal/gui/events.go:151-158`) lazily
spawns a background `drainPersist` goroutine via `sync.Once` the FIRST time
`Publish` is called on a given `Broadcaster` instance. That goroutine calls
`api.(*API).AppendGUIEventLog` → `api.DaemonStateDir()` →
`daemonStateDir()` (`internal/api/state_paths_prod.go:39`), which reads the
package-level `daemonStateRootOverride` variable — the SAME global
`api.SetDaemonStateRootForTest` (`internal/api/testhooks.go:136-149`)
mutates via `t.Cleanup` at the end of EVERY test that calls it.

The persist-drain goroutine has no bounded lifetime tied to the owning
test — nothing in `hub_listener_restart_test.go`'s test bodies appears to
wait for it to drain/exit before the test returns. So a goroutine spawned by
test A (via `signalHubListenerRestart` → `hubHealthTracker.set` →
`NewServer`'s lazily-constructed Broadcaster → `Publish` →
`ensurePersistDrain`) can still be reading `daemonStateRootOverride` at the
moment test B's `t.Cleanup(func(){ daemonStateRootOverride = orig })` fires
— a genuine, unsynchronized read/write race on a shared package-level
variable across two different tests' goroutines.

## Verified pre-existing, unrelated to Increment-1

The exact mechanism (unbounded persist-drain goroutine + the
`daemonStateRootOverride` test-hook global) was confirmed byte-identical at
the merge-base commit `1889cff6` (before ANY Increment-1 work), via a
temporary git worktree diff of `internal/gui/events.go`,
`internal/api/testhooks.go`, and `internal/api/state_paths_prod.go`. None of
the Increment-1 F1/F2/F3 diff (`internal/gui/server.go`'s
`Config.ReadOnlyRouterMode`, `internal/gui/serena_idle_sweeper.go`'s
`maybePersistSerenaActivity` gate, `internal/gui/serena_router.go`'s
`SetSerenaRouterReadOnly`, `internal/gui/lsp_router.go`'s
`SetLSPRouterReadOnly`, or anything in `internal/cli/route.go`) touches
`events.go`, `hub_listener.go`, `hub_listener_restart_test.go`, or
`testhooks.go`.

Isolating the originally-flagged test alone
(`go test -race -run TestHubListenerRestartDriverInitialBindOwnMcphubGUIOwnerDoesNotRotateOrReconcile`,
3 consecutive runs) never reproduced the race — consistent with a
cross-test scheduling race that only manifests when enough OTHER tests run
around it to shift goroutine timing. Adding a new test file
(`internal/gui/route_readonly_test.go`, this session's F1 falsifying test)
plausibly perturbed that scheduling enough to newly expose a
previously-latent flake; it did not introduce the race itself.

## Why this is filed as adjacent, not fixed here

Outside the approved Increment-1/F1/F2/F3 change surface (the MCP
front-daemon's read-only wiring and port collision). Non-blocking for this
work-item: `go build ./...`, `go vet ./...`, and `go test` (non-race) on
`internal/gui`, `internal/mcproute`, and the touched `internal/cli` tests
all pass clean; only `-race` on the FULL `internal/gui` package is
intermittently affected by this pre-existing, unrelated flake.

## Suggested next step

Either (a) give the persist-drain goroutine a bounded, test-observable
shutdown (a `Close`/`Stop` a test can call before its own cleanup, or a
`t.Cleanup`-registered drain-and-wait in the test helpers that construct a
`Server`/`Broadcaster` in `hub_listener_restart_test.go`), or (b) make
`daemonStateRootOverride` access properly synchronized (atomic/mutex-guarded)
so a benign cross-test race stops being a race even if the goroutine
lifetime issue isn't fixed. (b) is a narrower, lower-risk patch; (a) is the
more correct fix (an unbounded background goroutine outliving its test is
itself a hygiene problem independent of this specific race).
