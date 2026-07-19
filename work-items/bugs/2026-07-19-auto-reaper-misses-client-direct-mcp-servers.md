# BUG: the automatic orphan-reaper does not cover client-direct (unadopted) MCP servers — `ScanClientConfigs` off in the automatic path

Status: open
Filed: 2026-07-19
Severity: P1 (recurrence of a "fixed" leak class; operator-felt lag from ~300 accumulated node.exe)
Cross-ref: RECURRENCE of `work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md`
(marked fixed by #513/#516/#520 — the *machinery* shipped, the *automatic coverage* gap did not close).
Operator directive (verbatim, 2026-07-04): "хаб ОБЯЗАН ВБИРАТЬ ТАКИЕ СЛУЧАИ В СЕБЯ".

## Incident (measured 2026-07-19, operator host)
~334 `node.exe` accumulated, dominated by DUPLICATE client-direct MCP servers far exceeding the operator's
few real sessions: 71× `@colbymchenry/codegraph`, 30× `chrome-devtools-mcp` (+7 variants), 20×
`next-devtools-mcp`. 56 were >6h old (survived this session's GUI crash ~08:05Z + supervisor restart +
deploy). Operator: real sessions "much fewer than 13". Caused operator-felt lag. `mcphub` fleet itself was
clean (0 daemon orphans, supervisor+GUI healthy). Immediate relief applied: killed the 56 >6h node (334→240).

## Root cause ($analyst, static-read)
codegraph / chrome-devtools / next-devtools are **client-direct stdio entries** (Cursor/VS Code/codex/claude
configs) with NO mcphub manifest and NO adoption record, so they bypass every mcphub protection:
1. **Hub aggregate** (`internal/api/hub_mcp_handler.go:79-243`) dedups ONLY registered/adopted servers to one
   shared Job-owned backend. Unadopted servers never enter this path → each client spawns its own per reconnect.
2. **Squatter sweep** (`internal/cli/supervise_squatter.go:491-561`) reaps only mcphub's OWN forgotten daemon
   children (SupervisorDaemon ports + mcphub-binary identity). A client-direct node classifies Foreign → never killed.
3. **Automatic orphan reaper** (5-min ticker `internal/cli/gui_cleanup_ticker.go:88` → `internal/gui/cleanup.go:143`
   `CleanupOpts{...}`) runs `CleanupOrphans` with **`ScanClientConfigs=false` and `Aggressive=false`**. Nomination
   patterns come ONLY from mcphub's registered-server manifests (`internal/api/scan.go:950`) → these 3 servers match
   ZERO patterns → never nominated.
4. The two mechanisms that WOULD reach client-direct servers — A6 `--scan-clients` (client-config reverse-lookup,
   `internal/cli/cleanup.go:137`) and `cleanup aggressive` (live-rooted dup reap, `internal/cli/cleanup_aggressive.go`) —
   exist ONLY as MANUAL CLI, never wired into any automatic path.

So the ONLY hub protection for unadopted client-direct servers is voluntary `mcphub adopt`, which the operator
had not done for these three. The IN-FLIGHT hub-stability fix (2026-07-19-supervisor-ipc-poll-flood) does
NEAR-ZERO for this class — it's a control-plane fix; these servers never reconnect through the hub.

## Fix (ranked, $analyst)
1. **[operator-relief, available TODAY, not a work-item]** `mcphub adopt codegraph` / `chrome-devtools` /
   `next-devtools` (or GUI "Adopt into hub") → one shared Job-owned backend behind the hub URL; the
   per-client-per-reconnect spawn disappears at the source. (chrome/playwright also spawn a browser — adoption
   dedups the node MCP process, not the browser.)
2. **[THIS work-item's core, P1/HIGH, LOW risk]** Wire the EXISTING A6 `ScanClientConfigs=true` into the
   automatic reaper path (`internal/gui/cleanup.go:143` CleanupOpts). Reuses the shipped config-absence gate +
   identity-verified kill (`TerminatePIDWithIdentity`) + 600s age floor — this is SCOPE, not new authority. Makes
   the 5-min sweep reap TRUE orphans (dead-parent) of ANY client-direct server without adoption — closing the
   coverage hole the 2026-07-04 "fix" left open. + a regression test pinning the auto-ticker's ScanClientConfigs.
3. **[follow-up, MEDIUM, higher risk, needs $security-reviewer]** Automatic duplicate-accumulation reaper for
   LIVE-rooted servers (the H-live-dup subset — client alive, N stale children). Automating the `aggressive` path
   (kills children of a live client) needs a safe duplicate-detector (same signature, count>N, keep newest) +
   mandatory security gate + the dangerous-class deny-list (chrome/etc.). Do NOT auto-enable aggressive without that.

## Constraints / risks
- Reaper is Windows-only (`cleanup.go:978`; POSIX 501). Fix-over-fix history on `internal/api/cleanup.go`
  (#511/#520/#521/#522 — four hardening rounds/month) → any change re-runs the security commission.
- Two mechanism sub-hypotheses unresolved without a live process-table probe: H-orphan (dead-parent → Rank-2
  reaches) vs H-live-dup (live-client → needs Rank-3). Resolve via `mcphub cleanup --scan-clients` (dry-run) +
  `cleanup aggressive --client <X>` (dry-run) counts.
