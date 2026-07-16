# BUG: `--no-browser` instance opens an uninvited browser on activate-window (and `go test ./internal/cli/` spams the desktop)

Status: open
Filed: 2026-07-16
Severity: P2 (developer-hostile + a real product-behavior defect)

## Symptom (operator-visible)

Running `go test ./internal/cli/` pops **~10 real browser windows** onto the developer's desktop, one
per gui-spawning test. Reported by the user mid-session: "нахера запускается по 10 подряд gui?".

## Root cause

`internal/cli/gui.go:538-568` — the activate-window callback. A `if noBrowser { return
gui.ErrActivationNoTarget }` guard **existed until 2026-05-20 and was deliberately removed** (see the
comment block at 538-552: the removal reasoning was "the `--no-browser` startup flag suppresses the
AUTO-launch at GUI boot, while /api/activate-window is only reachable via the CSRF + same-origin gate
… Honoring tray clicks even under `--no-browser` matches the user's expectation").

That reasoning holds when a window EXISTS (the tray click focuses it). It does NOT hold for the
no-window case:

1. A `mcphub gui --no-browser --no-tray` instance has **no window**.
2. Something POSTs `/api/activate-window` (in tests: a second `mcphub gui` instance detecting the
   incumbent; in the field: any same-origin tab or the tray child).
3. `FocusWindow` fails with `gui.ErrFocusNoWindow`.
4. The early return at `gui.go:530` only fires for **non**-`ErrFocusNoWindow` errors, so control falls
   through.
5. `gui.HeadlessSession()` is false on a desktop, so `gui.LaunchBrowser(url)` (`gui.go:562`) runs and
   **opens a real Chrome/Edge window** — despite `--no-browser`.

The headless branch (`gui.go:556-559`) already does the right thing for a display-less session
(`ErrActivationNoTarget` + SSH-tunnel guidance). The `--no-browser`-with-no-window case needs the same
treatment.

## Stale comment (secondary defect)

`internal/cli/gui_integration_test.go:180-181` asserts the opposite of reality:

> "Both instances spawn with --no-browser, so the SECURITY guard (--no-browser refuses LaunchBrowser
> fallback in the callback)"

That guard no longer exists. The comment is stale since 2026-05-20 and hides the browser spam the test
actually causes. (Stale-relation residue — architecture law C6.)

## Blast radius

- **Dev**: every `go test ./internal/cli/` run (≈10 gui-spawning tests: `gui_integration_test`,
  `gui_force_test`, `gui_resetport_test`, `gui_self_restart_handoff_test`, `gui_supervisor_owner_test`,
  `gui_tray_state_test`, `daemon_test`, `hubmcp_test`, …) opens uninvited browser windows.
- **Product**: an orphan/headless-intent `mcphub gui --no-browser` (e.g. a service-style launch) can be
  made to spawn a Chrome window by any same-origin caller — the exact scenario the removed guard was
  protecting against, for the no-window case.

## Proposed fix

In the activate-window callback (`internal/cli/gui.go`), when focus fails with `ErrFocusNoWindow`:
refuse the browser fallback when the instance was started with `--no-browser` — return
`gui.ErrActivationNoTarget` (mirroring the headless branch) so the caller prints the "no target"
guidance instead of launching a browser. Keep honoring the fallback for instances started WITHOUT
`--no-browser` (the original tray-consent intent is unaffected: with a window present, focus succeeds
and no fallback runs).

Then fix the stale comment in `gui_integration_test.go:180-181` and add a regression test asserting a
`--no-browser` instance returns `ErrActivationNoTarget` (and does NOT call the browser seam — inject
`spawnProcess` like `internal/gui/browser_test.go` does).

## Notes

- `go test ./internal/gui/` is NOT a culprit: `internal/gui/browser_test.go` stubs the `spawnProcess`
  seam, so it never launches a real browser.
- Found while running the mandated pre-push suite for PR #555 (hub-health). Unrelated to that change —
  filed separately.
