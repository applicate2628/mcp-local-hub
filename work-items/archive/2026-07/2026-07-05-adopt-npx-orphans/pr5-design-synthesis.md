# PR5 reaper-hardening — design synthesis (2026-07-08 commission)

Design commission: security(fable) + feasibility(sonnet) READY-TO-IMPLEMENT; architect
lane errored (schema retry). Two lanes CONVERGED on the config-absence gate as the
must-ship core. Full commission output: session task wtct7tpa9.

## Two factual corrections to the H5 decision (register in the decision doc)
1. **The reaper is ALREADY auto-running.** `internal/cli/gui_cleanup_ticker.go` POSTs
   `{"apply":true}` to `/api/cleanup/orphans` every 5 min (opt-out
   `MCPHUB_DISABLE_AUTO_CLEANUP`) → `DryRun=false` on the SHARED `CleanupOrphans`.
   So the gate must land on the shared owner (hardens manual + ticker at once); the
   "manual-first" framing = "no NEW unattended machinery, no PEB", NOT a manual-only branch.
   Latent S2 risk: post-adopt, adopted-manifest signatures (@mui/mcp) enter the ticker's
   kill set guarded only by ancestor-walk + 60s. (No adopted manifests on THIS host yet.)
2. **T1 wording off.** Antigravity/Cursor/Windsurf GLOBAL configs ARE scannable (their
   adapters register ConfigPath()); config-absence covers them. The REAL residual (R1) =
   PROJECT-scoped configs (.cursor/mcp.json, .vscode/mcp.json, project .mcp.json) + unknown
   clients — unenumerable, silent gap (no failure to fail-closed on). Mitigated by
   parent-dead + age + manual preview + audit; NOT auto-timer. Document as accepted R1.

## Reconciliation (the crux)
- **Config-absence (step 4) does NOT replace --scan-clients.** --scan-clients = NOMINATION
  (report "this is MCP-server-shaped"); config-absence = KILL-ELIGIBILITY ("is this signature
  still wanted by any client"). ADDITIONAL AND-gate, applied AFTER pattern-match+ancestor-walk+age,
  BEFORE the kill. Consequence (deliberate, per H5): a --scan-clients candidate is referenced
  BY CONSTRUCTION (its nominating token came FROM a config) → T2 becomes kill-INERT/report-only.
  Post-adopt the direct entry LEAVES the config → absence unlocks the reap for leaked residue.
- **Parent-dead (step 2) — LANE DISAGREEMENT (defer to security-reviewer):** fable = the
  ancestor-walk fails open (snapshot truncation → dropped live-ancestor row → false candidate;
  break-at-missing-PID) → ADD an explicit OpenProcess+GetExitCodeProcess(STILL_ACTIVE=259)+
  CreationDate-recycle probe (fail-closed SPARE on unverified; precedent probe_windows.go:37-56,
  process.IsPidAlive). sonnet = enumeration-absence already handles zombies (H5 probe: zombie
  PID absent from Win32_Process) → document-only. BOTH AGREE the snapshot-truncation fail-open
  is real + must be fixed. Repo ships zombie-safe `process.IsPidAlive` if an explicit check is wanted.

## First-PR scope (converged safety core)
1. **Config-absence gate** — `scanClientConfigsFailClosed()` (not-installed=skip;
   exists-but-unparseable=degraded, keep collecting from other clients) + `candidateConfigReferenced`
   (normalized raw-content substring of the candidate's matched pattern tokens over every adapter's
   ConfigPath()). referenced → spare (verdict spared-config-referenced); any degraded client →
   the WHOLE run's config-absent candidates spared (verdict spared-config-scan-degraded);
   else reap-eligible.
2. **Snapshot-truncation fail-closed** — a `parseProcessRows` snapshot error disables kills for
   the run (dry-run listing still returned).
3. **T3/bare-name report-only** — `patternsForServer`'s manifest-load-failure `[serverName]`
   fallback (scan.go bare name) can nominate but never kill.
4. **Kill-age-floor 600s** — kill eligibility floor (design.md "age>~10min"), independent of the
   60s listing default.
5. **`OrphanProcess.ReapVerdict`** + audit via SupervisorEventLog (source="cleanup"; body =
   pid/basename/pattern/verdict, NEVER raw cmdline — wire-redaction precedent cleanup.go:69-78).
6. **Scoped to `CleanupOrphans` only** — `AggressiveCleanup` must NOT inherit the gate (operator-
   scoped live-rooted kill; test-pinned boundary S13).

## Deferred (commission ruling or fast-follow)
Explicit parent-probe (lane disagreement — security-reviewer decides); supervisor-state PID
exclusion; ticker apply:false policy flip (H5 unattended-blessing); knownClientLauncher 8→45
expansion; project-scoped-config enumeration (R1 shrink); PEB step-5.

## Test plan (deterministic — fixture CSVs + injectable seams; Windows-only)
T1-hole guard (referenced→spared, terminate-spy zero calls even DryRun=false; token removed→
eligible); config fail-closed taxonomy (present/absent/EACCES-degraded/IsNotExist-not-degraded/
normalization); T2-never-kills symmetry; snapshot-truncation → zero kills; T3 report-only never
kills; kill-age-floor (120s listed-not-killed / 700s eligible); Aggressive-boundary unaffected;
ticker integration (apply:true traverses new gates). All via orphanTerminateFn/parseOrphans-reader
seams — no live processes.

## $security-reviewer MANDATORY before merge (design.md P2).
