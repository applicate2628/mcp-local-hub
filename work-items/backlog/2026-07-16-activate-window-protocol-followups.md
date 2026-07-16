# activate-window protocol — deferred follow-ups (from the PR #556 review panel)

Filed: 2026-07-16
Source: the Sol (architecture/arbiter) + Terra (complementary audit) panel on PR #556, across three
review rounds (12 findings fixed in the PR). These four were scoped OUT deliberately.

PR #556 fixed the operator-visible defect (a `--no-browser` incumbent popping an uninvited browser on
every activate-window, notably from the cli test suite) and the reason-classification regression it
briefly introduced. The items below are PRE-EXISTING gaps the panel surfaced while reading the path —
none is a regression introduced by that PR, and none blocks the fix the owner is waiting on.

## 1. Per-request window consent is not transmitted (the design-correct seam)

`internal/cli/gui.go` — a second invocation's own `--no-browser` value is DISCARDED: `mcphub gui
--no-browser` against an incumbent started WITHOUT `--no-browser` still makes that incumbent launch a
browser, even though THIS caller forbade browsers.

Root: `/api/activate-window` conflates caller intents because the wire carries none. The incumbent
cannot distinguish "a tray click (consent)" from "a second instance handshake (no consent)" from "a
caller that explicitly forbids a browser".

**Right shape:** carry window-intent on the POST (the second instance sends its own `!noBrowser`; the
tray sends true; an ABSENT param defaults to refuse — safe under version skew). Require BOTH caller
consent and incumbent permission before launching.

**Why deferred:** pre-existing (before #556 the incumbent ALWAYS launched, ignoring the caller
entirely) — #556 strictly improves it. The tray-closure fallback is the minimal correct repair for the
regression #556 had to fix; the full consent protocol is its own change with its own review.

## 2. A failed focus / failed browser launch still reports "activated"

`internal/cli/gui.go:~429` and the callback's non-`ErrFocusNoWindow` path — a Win32 focus jitter or a
`LaunchBrowser` error becomes nil/204, so the caller prints "activated" and exits 0 while NOTHING
opened.

**Right shape:** propagate a typed activation failure into the shared activation-result owner (#556
introduced that owner, so the seam now exists).

**Why deferred:** pre-existing behavior (the 204-on-transient-focus-failure was a deliberate PR #26
choice: "the incumbent IS reachable, just focus jitter"). Changing it flips an exit-code contract and
needs its own scope.

## 3. No capability negotiation against a legacy incumbent

`internal/gui/handshake.go` — the activation POST is unconditional. After a LIVE binary upgrade (new
CLI, old incumbent still running), the legacy incumbent has no `--no-browser` guard and still launches
a browser.

**Why deferred:** the documented deploy protocol is a FULL supervisor+GUI restart (CLAUDE.md
"Redeploy always after merge"), so the mixed-version window does not persist in practice. Probing
protocol support before every activation is real cost for a transient state.

**Mitigation already in place:** #556 classifies a header-less 503 as `headless` (a pre-header
incumbent returned 503 from exactly one branch — its headless check), so the legacy path at least
produces correct guidance.

## 4. REJECTED (not deferred) — do not gate `mcphub version` / `help daemon`

One audit lane asked to put `requireGuiSpawnTests(t)` on `daemon_reliability_test.go:~130,~201`
(`mcphub version`, `mcphub help daemon`) and to broaden the gate to "all real-mcphub subprocess tests".

**Arbiter call: rejected.** The gate exists to stop DESKTOP SPAM — GUI/supervisor/daemon processes with
windows and lifecycles. `version` and `help` spawn the binary for ~50 ms, print text, and exit: zero
desktop effect. Gating them would drop real exit-code coverage for no benefit, and it contradicts the
same lane's earlier analysis (which explicitly classified them as "harmless and correctly ungated, so
the shared binary build is not wasted"). The gate's name and CLAUDE.md wording stay scoped to
GUI/supervisor/daemon spawns.
