# Closure — PR #288 cumulative v0.6-core review

Closed: 2026-06-14

## Outcome

PR #288 (REVIEW-ONLY, do-not-merge) is **CLOSED**. Its purpose — surface
findings across the v0.6-core stack — is complete. Every finding landed on
master via the follow-up PRs, all merged and deployed:

- **#300** (`99beccc`) — fleet-hazard test isolation: cli TestMain global
  state-dir redirect so cli tests can never touch the live fleet; subprocess
  IPC-pipe discriminator (closed a live-SID-pipe collision hazard) + pidport
  non-zero-bind poll + override-leaf poll.
- **#301** (`6850f2b`) — SEC-F2 strict-mode-from-intent + SEC-F3 owner-SID.
  Drove r3→r11. Key arc: the intent strict_mode read went through the WRITE
  parent-gate, which on a broadened host (this host) flipped
  `OperatorRequiresSingleUserHome` to STRICT → would have refused all
  client/state writes → **broken the live 23-daemon fleet on deploy**.
  Resolved by reading strict_mode GATE-FREE (strict only for
  present+parseable+strict_mode=true; everything else relaxes). Plus the
  owner-SID already-exited sentinel swept across all 3 kill callers + the gui
  KillRecordedHolder already-gone recovery (no unverified/reused-PID kill).
- **#302** (`d1f8caf`) — supervisor orphan-reap on descriptor removal,
  serialized entirely on the event loop, single canonical key-space
  (legacy bare-key TaskName rows handled end-to-end), transient-absence /
  replace-in-place safe, sync barrier for the reconcile-apply cache.
  Drove r1→r9; rebased onto #303-master and composed cleanly.
- **#303** (`44894a8`) — command-drift restart terminates with the spawned
  (old) descriptor.

## Process note — gate switch

Rounds r1–r7 used the Codex Cloud bot. Per operator directive
("дальше ревьювишь сам, без бота, внутренними агентами") the final rounds
(#301 r8–r11, #302 r8–r9) used **internal multi-agent (opus) review** as the
merge gate instead of the bot. Two internal review passes (7-lens + 3-lane
final) caught: the canonical-key bug class (#302), the fleet-safety
strict-flip (#301, the most important finding — would have broken the live
fleet), the executeSideEffect synthesize-guard sweep miss (#302), and stale
comments. All fixed and re-verified.

## Verification discipline

Every fix lane proved `supervisor-intent.json` byte-identical PRE==POST
(live fleet untouched) and ran narrow `-run` tests only. The main session
re-verified each lane on the command line (stale gopls diagnostics were
discounted ~9× after command-line build/vet confirmed clean). Integrated
master: `go build ./...` ×2 GOOS + `go vet ./...` + key families green.

## Deploy

`build.sh` → `install --upgrade` (cold-restart, staged same-volume to dodge
the D:→C: MoveFileEx cross-volume gotcha) swapped the binary
(7f1e33d → d1f8caf) and restarted the supervisor (34404 → 167920) with the
10 global daemons. The GUI (host of port 9125 = serena `/serena/mcp` + all
`/lsp/*/mcp` routes) did NOT auto-restart, so serena + 9 language servers
were down; relaunched `mcphub gui --no-browser` (tray on) → 9125 rebound
(PID 61620) → `claude mcp list` shows serena + all LSP + the full fleet ✓
Connected.

## Residual risk

- The bare-key (legacy/hand-written non-canonical TaskName) reap paths are
  exercised only by synthetic tests; this host's real intent is fully
  canonical, so the common path is what runs in production.
- The accumulated `mcphub.exe.old-*` aside files in the install dir are
  auto-pruned by the supervisor (>7-day RFC3339 form); a few legacy-format
  `.bak-predeploy` / `.old` names won't auto-clean (harmless).
- No GUI autostart task exists on this host; the GUI is launched manually,
  so a future supervisor-only restart will again drop 9125 until the GUI is
  relaunched. (Candidate follow-up: a GUI autostart/liveness task.)

## Archive

`work-items/archive/2026-06/2026-06-11-pr288-cumulative-review/`
