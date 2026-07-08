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

   **PR5 round-2 (bot #520 + codex army + workflow — ALL fixed in this PR):**
   - **T3 over-drop regression FIXED** (bot P2 #1 / codex B): `len(ps)==1 && ps[0]==name`
     wrongly emptied the REAL `mcp-language-server` manifest. Moved the bare-name demotion
     to its SOURCE — `patternsForServerNominatable` + `patternsFromManifestEx` (fallback
     flag) return nil ONLY for a true synthesized fallback; a real self-named binary
     survives. Scan process-count keeps the bare-name fallback (unchanged).
   - **Reference-side short-token FIXED** (bot P2 #2): `argIsReferenceCandidate` (relaxed,
     inclusive=true) keeps a short real token like `serena` on the config-REFERENCE side
     (over-match only SPARES); nomination side stays strict.
   - **Exists() stat-error → degraded FIXED** (bot P2 #3 / codex A P0#2): stat
     `ConfigPath()` when `Exists()==false`; a non-IsNotExist error (ACCESS_DENIED) counts
     as degraded, not absent.
   - **Dry-run KillErr consent-surface P1 FIXED** (workflow sonnet+fable CONFIRMED):
     stamping KillErr in dry-run made the GUI Preview render a reap-ELIGIBLE row (empty
     kill_err) as a false "killed". `classifyReapVerdict` extracted; KillErr stamped ONLY
     on apply; ReapVerdict is the dry-run-safe audit field.
   - **Aggressive snapshot-scanner-error fail-closed FIXED** (codex A P2 / B MH1,
     all-return-paths): `parseAggressiveCandidates` propagates the parseProcessRows error;
     `AggressiveCleanup` refuses APPLY kills on a truncated census.

   **Deferred to a coherent tracked bug** —
   `work-items/bugs/2026-07-08-cleanup-ancestor-walk-fails-open-on-uncertainty.md`
   (PRE-EXISTING walk fail-opens, NOT introduced by #520; one refactor, not two touches):
   - Case A (codex A P0): byPID-miss can't tell dead-parent orphan from dropped-live-row →
     needs a `process.IsPidAlive` parent-probe with an injectable seam (a blunt spare would
     inert the reaper — byPID-miss is ALSO the normal real-orphan signature).
   - Case B (codex A P1): depth-16 exhaustion falls through to orphan → spare-on-exhaustion.
   - CLI aggressive `ExpectPIDs==nil` validated-set-binding gap (codex A P2 secondary).
   - Still deferred per H5: per-candidate SupervisorEventLog audit, supervisor-state PID
     exclusion, ticker apply:false policy, knownClientLauncher 8→45, project-scoped-config
     enum (R1), PEB step-5.
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
