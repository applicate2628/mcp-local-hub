# status — adopt npx-stdio orphans into the hub (A2)

Template: full-delivery (security-sensitive). Orchestrator: main conversation.
State: PARTIALLY DELIVERED — adopt CLI/API + GUI surface SHIPPED + DEPLOYED;
reaper (PR5) remains parked on the revised gate.

## Delivered (shipped to master + live-deployed)
- **`mcphub adopt` CLI + `api.BuildAdoptPlan`/`ExecuteAdopt`** (earlier PR chain):
  extract → ManifestCreate → Install(merge), namespaced sanitized vault keys,
  9300-9399 port allocator, signature-matched multi-client repoint,
  backup-collision-safe rollback, symlink consent.
- **GUI "Adopt into hub" surface (A2 PR4b, #516 → master 487482cf, deployed
  2026-07-08):** Discovery "Adopt into hub" button gated on the backend
  `adopt_supported` capability, preview modal, scoped symlink-consent threaded
  as reviewed (client,path) DATA (no process-global hook), execute-error
  redaction (fail-closed), gui-events audit row, explicit-clients
  mismatch/disabled re-exclusion. Bot FULL PASS + fable independent security
  PASS; fail-closed redaction hardening (fable P3-A/P3-B) applied.

## Blockers resolved
- **H2** (symlinked codex config.toml write) — resolved; scoped-consent write
  path (SecureWriteClientConfigWithConsent) shipped in #516.
- **H5 / pipe-peer spike** — resolved: `work-items/decisions/2026-07-08-
  pipe-peer-unreliable-reaper-gate.md` (accepted v2). Pipe-peer is a DEAD END
  for the reaper safety gate; adopt is the primary fix; if a reaper ships its
  gate = config-file-absence AND parent-dead(STILL_ACTIVE) AND age AND
  identity-reverify (PEB-stdio-state optional fail-closed supplement).

## Remaining
1. **PR5 — auto-reaper hardening — IN PROGRESS (config-absence gate + must-haves
   implemented 2026-07-08; PR pending).** Design + security commissions run
   (architect+fable+sonnet design; fable[$security-reviewer]+sonnet+codex review;
   full synthesis in `pr5-design-synthesis.md` + session tasks). **Two factual
   corrections to the H5 decision registered:** (a) the reaper is ALREADY
   auto-running — `internal/cli/gui_cleanup_ticker.go` POSTs `{"apply":true}` to
   `/api/cleanup/orphans` every 5 min (opt-out `MCPHUB_DISABLE_AUTO_CLEANUP`), so the
   gate lands on the shared `CleanupOrphans` (hardens manual + ticker at once);
   (b) T1 wording off — Antigravity/Cursor/Windsurf GLOBAL configs ARE scannable, so
   config-absence covers them; the REAL residual (R1) is PROJECT-scoped configs +
   unknown clients (unenumerable, silent gap). **Implemented (tested, all reviewers
   confirm the gate is a STRICT SPARE-ADDER — kills a subset of the old set):**
   config-absence gate (`scanClientConfigsFailClosed` + `candidateConfigReferenced` +
   `applyReapEligibilityGate`; referenced→spare, exists-but-unparseable→spare-all
   fail-closed, not-installed→skip); snapshot-truncation fail-closed (parseProcessRows
   error → spare all, a dropped live-ancestor row can no longer mis-classify a live
   child); T3 bare-name-fallback demotion (`manifestNominationPatterns` drops a
   `[]string{serverName}` corrupt-manifest pattern so a bare word never nominates/kills
   a bystander); 600s kill-age floor; `OrphanProcess.ReapVerdict` audit field;
   AggressiveCleanup boundary preserved (still un-gated). **$security-reviewer (fable)
   verdict = SHIP_WITH_FIXES; the two kill-hole must-haves (snapshot-fail-closed +
   T3-demotion) are IN this PR.**

   **PR5 fast-follows (tracked, NOT in this PR — must land before any ticker-policy
   expansion / broad npx-adopt fleet rollout):**
   - **parent-verified-dead explicit probe** (fable/sonnet P2, lane-disagreement per H5
     doc): the 16-deep ancestor-walk relies on snapshot presence; a SILENTLY-dropped
     live-ancestor row (a per-process WMI property race, not caught by the P1 snapshot
     error) orphanizes a live subtree whose hub signature is NOT config-referenced →
     ticker-kill. Fix: at the byPID-miss walk break, probe with
     `process.IsPidAlive`/`GetExitCodeProcess==STILL_ACTIVE`; alive-but-unenumerated →
     spare (fail closed). Cite the silent-row-drop scenario, not just ErrTooLong.
   - **Exists() stat-error fail-closed gap** (fable P2): adapter `Exists()` collapses
     ANY `os.Stat` error (incl ACCESS_DENIED) into "not installed" → a DACL-refused
     config (documented sandbox-broadened %LOCALAPPDATA% host class) contributes
     neither patterns nor a degraded entry → a signature referenced ONLY there reads
     as config-absent → killed. Fix: stat `ConfigPath()` directly; non-IsNotExist error
     → degraded.
   - **candidateConfigReferenced false-negatives** (fable P2): the reference side uses
     `argIsDiscriminatingPattern` (≥8-char) so a short config arg like `serena` (6 ch)
     that the manifest side nominates can never count as a reference → a live serena's
     un-migrated direct entry is not spared. Fix: relax the reference-side arg filter
     (over-broad only spares) + normalize the Contains (lowercase, fold `\`→`/`).
   - **per-candidate SupervisorEventLog audit** (fable/sonnet P3): emit source="cleanup"
     orphan-reaped/-spared events (pid/basename/verdict, never raw cmdline) so the
     false-positive rate is measurable before unattended-policy expansion.
   - Also still deferred per H5: supervisor-state PID exclusion, ticker apply:false
     policy decision, knownClientLauncher 8→45 expansion, project-scoped-config
     enumeration (R1 shrink), PEB step-5.
2. **Anti-drift "unmanaged detected" surface** (GUI) — surface bypass servers
   so the operator can adopt them; reduces reaper need.
3. **Phase-2 de-adopt / revert-to-native** (hub→native): interim = `mcphub
   uninstall --server X` + restore client backup.
4. **fable Area-3 follow-up** (#516 review) — **DONE 2026-07-08 (PR #519, master
   f52d3ccb, deployed).** An explicitly-requested client whose config extraction
   errors now FAILS LOUD at plan-build time (path-free error) instead of silently
   surviving the filter to an AddEntry rollback; classification is by typed
   sentinel (errors.Is), never substring on path-bearing err.Error().

## Artifacts
- design.md — architect package (this dir).
- Source bug: work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md.
- Decision: work-items/decisions/2026-07-08-pipe-peer-unreliable-reaper-gate.md.
- Adjacent finding (standalone-shippable subset of P2): work-items/bugs/2026-07-05-cleanuporphans-raw-taskkill-no-identity-reverify.md.

## Depends-on
2026-07-05-a1-lost-child-fpackage (F1/F3 kill-authority precedent + primitives; A2-PR5 sequenced after).
