---
title: The canonical ~/.local/bin/mcphub.exe is never auto-reconciled against a newer npm-installed binary — `mcphub` keeps running a stale version
severity: high
found-by: operator, fresh-laptop npm upgrade (0.4.27 → 0.4.28 console-subsystem fix would not take effect)
affected-surface: npm/bin/cli.js (the shim); internal/cli/setup.go Bootstrap (~/.local/bin canonicalization)
context: operator-reported, live on the laptop
status: implemented
---

## What happened

Operator upgraded the laptop with `npm install -g mcp-local-hub@0.4.28`, then ran
`mcphub` — it still reported **0.4.27**. The console-flood fix (v0.4.28) therefore did
not take effect, even though npm had the correct binary.

Cause: there are **two** mcphub binaries and nothing keeps them in sync.

1. The npm meta package's `bin` is a Node shim (`npm/bin/cli.js`) that `spawnSync`s the
   platform binary from `@applicate2628/mcp-local-hub-win32-x64/bin/mcphub.exe`. npm
   updated THAT to 0.4.28.
2. `mcphub setup` (`Bootstrap`, `setup.go:220`) copies the **currently-running** binary
   to the canonical `~/.local/bin/mcphub.exe` and puts that dir on PATH. An earlier
   setup had put **0.4.27** there.
3. On the operator's PATH, `~/.local/bin` shadows the npm shim, so `mcphub` resolved to
   the stale 0.4.27 canonical. Running `mcphub install` / `mcphub setup` then executed
   the 0.4.27 binary, which copies **itself** — so the "self-update" re-installed 0.4.27.
   A deadlock: the stale binary can never replace itself with a newer one it never runs.

Operator's words: **"у меня в .local висела версия старая и почему-то её не обновил хаб
сам."** The expectation is correct — the hub should reconcile this itself.

## Confirmed gap

`grep` for any version-mismatch / stale-canonical / running-vs-canonical check across
`internal/cli` + `internal/api` + `npm/bin/cli.js`: **none exists.** The shim does zero
reconciliation (deliberately minimal — no postinstall, a security stance). Nothing
compares the running binary's version to the canonical's, and nothing warns on drift.

## Manual break-the-deadlock (what unstuck the laptop)

Run the fresh npm binary BY ABSOLUTE PATH so it bypasses the PATH shadow, and let ITS
setup re-canonicalize:

```powershell
npm uninstall -g mcp-local-hub ; npm cache clean --force ; npm install -g mcp-local-hub@0.4.28
$exe = "$(npm root -g)\@applicate2628\mcp-local-hub-win32-x64\bin\mcphub.exe"
& $exe version   # 0.4.28
& $exe setup     # copies 0.4.28 over the stale ~/.local/bin canonical
```

(The fleet must be stopped first — a running canonical locks the file on Windows.)

## Fix direction — NOT yet decided (candidates, needs an architect call)

The hard constraint is the chicken-and-egg: if PATH prefers the stale canonical, the
user's `mcphub` never runs the fresh npm binary, so a self-heal that lives in the
BINARY can't fire. The reconcile has to happen somewhere that always sees the fresh
binary, or the drift has to be surfaced loudly.

1. **npm shim self-heal (npm/bin/cli.js).** Before spawning, the shim (which always
   resolves to the fresh npm binary) stats `~/.local/bin/mcphub.exe`; if it exists and
   its version differs from the npm binary, re-canonicalize it (lock-safe: skip if the
   fleet holds it, or rename-aside). This fixes the case where the npm shim is ever
   invoked — but NOT the case where PATH fully shadows it. Weigh against the deliberate
   "shim does nothing" security stance (it would now touch the filesystem).
2. **Version-drift detection + loud warning.** Any long-lived surface (the GUI, `mcphub
   status`, the supervisor) that can see BOTH the running version and the newest
   installed npm package version warns: "a newer mcphub (X) is installed via npm; the
   canonical binary is stale (Y) — run `& <npm-exe> setup` to update." Honest, no silent
   drift, but still requires an operator step.
3. **`mcphub install`/`setup` re-canonicalize when RUNNING newer than canonical.** The
   running binary compares its own `main.version` to the canonical's and, if strictly
   newer, offers/auto re-copies. Correct for "user ran the fresh one", useless when the
   stale one runs.
4. **Make the npm shim the single source of truth** (stop copying to ~/.local/bin at
   all, always run through the shim). Biggest change; removes the two-binary split
   entirely but reworks the whole PATH/canonical model and the scheduler-task binary
   paths that point at ~/.local/bin.

Per the operator's standing UX principle (never make the user dig / run obscure
commands — [[feedback_never_force_user_manual_config_offer_gui_toggle]]), the fix must
end with the hub doing the reconcile, or at minimum surfacing the drift in the GUI with
a one-click "update" — not leaving the user to discover the absolute-path setup dance.

## Root confirmed by the operator: `~/.local/bin` is EARLIER than npm on PATH — BY DESIGN

Operator: "он не обновляется потому что раньше чем npm лежит в path." Confirmed and it
sharpens the fix analysis:

- `~/.local/bin` being ahead of the npm global bin is **intended**. `~/.local/bin/mcphub.exe`
  is the CANONICAL binary the whole product points at — the scheduler tasks
  (`\mcp-local-hub-supervisor`, `\mcp-local-hub-liveness`), the supervisor's own-child
  spawn path, and `install --upgrade`'s rename-aside target all use `~/.local/bin`. npm is
  only the DELIVERY vehicle; the canonical path is the authority.
- So the defect is NOT "npm should win PATH". It is: **a newer binary delivered by npm is
  never propagated into the canonical `~/.local/bin`, and nothing detects the drift.**
- This also KILLS fix-candidate 1 (npm-shim self-heal) as a complete fix on its own: with
  `~/.local/bin` ahead of npm, the shim is NEVER invoked by a bare `mcphub`, so a shim-side
  reconcile can't fire. The reconcile must live where the AUTHORITATIVE (canonical) binary
  or an always-run surface can act.

### Refined recommendation (security-sensitive — needs a proper design pass)

The only place that reliably runs on the operator's `mcphub` is the CANONICAL binary
itself (it's first on PATH). So the self-heal must be there: **on run (or on
`install`/`setup`), the canonical binary compares its own `main.version` against the
npm-delivered binary's version at `$(npm root -g)/@applicate2628/mcp-local-hub-<plat>/bin/`,
and if npm's is strictly newer, re-canonicalizes itself** (rename-aside the running exe +
copy the npm one over, the same crash-safe Windows swap `install --upgrade` already uses).

This is a self-updating-binary feature, so it is a SECURITY surface and must not be rushed:
- The source must be VERIFIED as the legitimate npm-installed binary (correct path, owned by
  the user, plausibly the real package) — not an attacker-planted file at a guessable path.
- The swap must be crash-atomic and must not brick the fleet mid-update (rename-aside, verify
  the new binary runs `version` before committing, keep the `.old-<ts>` for rollback).
- It must respect the operator's "never make me dig" principle: end state is the hub having
  updated itself, or a one-click GUI "update available → update" — not the absolute-path
  setup dance.
- Consider a safer middle ground first: DETECT + LOUD WARN (GUI banner + `mcphub status`
  line: "canonical binary Y is older than the installed npm package X — updating…") even if
  the auto-swap is gated behind a confirm.

## DECISION (operator, 2026-07-22): npm must install into ~/.local too

Operator: **"npm должен ставить в local тоже."** Chosen fix direction — the npm install
itself propagates the platform binary into the canonical `~/.local/bin`, so
`npm install -g mcp-local-hub@<newer>` refreshes the authoritative binary directly. This
avoids the runtime self-updating-binary security surface entirely: the copy happens at
install time, from the just-installed local package, with no network.

Implementation constraints (must honor the ORIGINAL security intent, not discard it):
- The existing "deliberately NO postinstall" stance in `npm/bin/cli.js` is specifically
  against a postinstall that DOWNLOADS (the supply-chain vector). A postinstall that only
  COPIES the already-installed local platform binary into `~/.local/bin` — no network, no
  fetch — is a different, defensible risk profile. Reconcile the comment: "no DOWNLOAD
  postinstall" rather than "no postinstall at all".
- FAIL-SAFE: if the copy/canonicalize fails (permission, file lock because the fleet is
  running, `--ignore-scripts`), the npm install must still SUCCEED — the user can fall back
  to `mcphub setup`. Never break `npm install` on a canonicalize failure.
- FILE LOCK: on Windows a running canonical `~/.local/bin/mcphub.exe` is locked; use the
  same rename-aside crash-safe swap `install --upgrade` uses, or skip-with-warning if the
  fleet is up.
- REUSE the existing canonicalization: the postinstall should invoke the FRESH platform
  binary's own Bootstrap/canonicalize path (`setup.go:220` copies the running binary to
  `~/.local/bin`), NOT re-implement the copy in JS — single owner. Determine the minimal
  invocation (Bootstrap-only vs full `mcphub setup`; full setup may prompt / want elevation
  / install tasks, which is too heavy for every npm install — a binary-only canonicalize
  entry point is likely needed).
- `--ignore-scripts` degrades gracefully to the manual `mcphub setup` path (documented).

## Related

- The PATH collision itself is already noted in CLAUDE.md / the reliability plan (npm
  shim vs `~/.local/bin` build vs a repo-root dev binary). This bug is the AUTO-UPDATE
  half of that: not just that they collide, but that a stale canonical is never healed.

## IMPLEMENTED (2026-07-22) — copy-only postinstall canonicalize

Chosen fix direction #4-adjacent (the operator's decision): the **npm install
itself** propagates the fresh platform binary into `~/.local/bin`, at install
time, from the just-installed local package, with **no network**. This sidesteps
the runtime self-updating-binary security surface (fix candidates 1-3) entirely.

What shipped (branch `fix/npm-canonicalize-local-bin`, off `master`):

1. **New Go entry point `mcphub canonicalize`** (`internal/cli/canonicalize.go`,
   hidden, registered in `internal/cli/root.go`). Non-interactive, binary-ONLY:
   copies the currently-running binary into `~/.local/bin` and nothing else — no
   PATH edit, no scheduled task, no client-config rewrite, no ephemeral-range
   probe, no prompt/elevation, no fleet reap/restart. Reuses the existing
   `copyExe` + `setupTargetPath` owners (single owner — the copy is NOT
   reimplemented). Lock-safe on Windows: when the running fleet holds the
   canonical `.exe`, it stages the fresh binary and swaps via
   `api.RenameAsideReplace` (the SAME crash-safe rename-aside `install --upgrade`
   uses), so the new binary lands even while the fleet runs and takes effect on
   the next fleet/supervisor restart. Idempotent: byte-identical target → no-op
   (no `.old-<ts>` churn on repeated same-version `npm i`); running-is-canonical
   → no-op (self-copy guard). Test: `TestCanonicalizeBinaryToTarget`.

2. **Copy-only `postinstall`** in the META package (`npm/scripts/postinstall.js`
   + `"postinstall"` script in `npm/package.json`). It resolves the platform
   binary npm just installed (via the new shared single-owner map
   `npm/lib/platform-binary.js`, also consumed by `bin/cli.js`) and spawns it
   with `canonicalize`. **NO network I/O** — it only copies an already-installed
   local file.

3. **FAIL-SAFE (load-bearing):** the postinstall ALWAYS exits 0. Any failure —
   no platform package, optionalDependency not installed, binary won't launch,
   or `mcphub canonicalize` exits non-zero (file lock / permission / missing
   `~/.local`) — prints a one-line notice pointing at `mcphub setup` and is
   swallowed. Proven end-to-end: with `~/.local` forced to be a FILE (MkdirAll
   fails → `canonicalize` exits 1), the postinstall still exited **0** with the
   `mcphub setup` fallback notice and wrote no canonical. Regression-guarded by
   `npm/postinstall.test.js` (unresolvable-binary → exit 0 + notice).

4. **`--ignore-scripts`** degrades gracefully: the postinstall is skipped
   entirely; the operator reconciles `~/.local` manually with `mcphub setup`.

5. **Security stance reconciled, not discarded:** the `bin/cli.js` `SECURITY`
   comment now reads "no postinstall **DOWNLOAD** step" and explicitly documents
   that the shipped `scripts/postinstall.js` only copies an already-installed
   local file (a different, defensible risk profile than a downloading
   postinstall). Docs updated: `npm/README.md`, `INSTALL.md`, `CLAUDE.md`.

Residual (documented, accepted): the postinstall deliberately does NOT restart
the running fleet, so after `npm install -g mcp-local-hub@<newer>` the CANONICAL
binary is fresh immediately (a new shell / scheduler relaunch / supervisor
restart picks it up), but the ALREADY-running supervisor keeps its old image
until it is restarted — a brief mixed-version window, strictly better than the
permanent staleness this bug describes, and the same window `install --upgrade`
manages before its own restart step.
