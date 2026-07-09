# status — adopt npx-stdio orphans into the hub (A2)

Template: full-delivery (security-sensitive). Orchestrator: main conversation.
State: PARTIALLY DELIVERED — adopt CLI/API + GUI surface + reaper (PR5) + anti-drift
"unmanaged detected" GUI signal (#523) + both reaper-hardening bugs (#521/#522) all
SHIPPED + DEPLOYED; remaining = phase-2 de-adopt (separate item
`2026-07-09-deadopt-hub-to-native`, blocked) + D P2a/P2b GUI.

## Delivered (shipped to master + live-deployed)
- **Auto-reaper hardening (A2 PR5, #520 → master c53d874a, deployed +
  live-verified 2026-07-08):** the H5 config-absence reap-eligibility gate on the
  shared `CleanupOrphans` (hardens the manual path AND the 5-min apply:true
  auto-ticker at once) — config-referenced/degraded-scan/snapshot-degraded/
  below-600s-age-floor SPARE, only config-absent aged residue reaps; snapshot
  scanner-error fail-closed (default + aggressive paths); T3 bare-name demotion at
  SOURCE (`patternsFromServerNominatable`, preserves real `mcp-language-server`);
  Exists()/ConfigPath() stat-error → degraded; unconstructable client factory →
  degraded (`AllClientsWithErrors`); config-reference case-fold + common-word
  specificity denylist; per-row `ReapVerdict` audit surfaced in CLI + GUI preview
  (no false "killed"; Clean counts only eligible); apply bound to the confirmed
  `{pid, started_at}` IDENTITY set (architect verdict — the binding key equals the
  key the kill primitive re-verifies, so a recycled PID cannot be killed
  unacknowledged). Reviewed: architect design + security commission + a 3-lane
  codex deep-security sweep + a cross-model adversarial-verify workflow + an
  architect abstraction verdict; **5 bot rounds driven to FULL PASS** ("Didn't
  find any major issues", reviewed commit == HEAD, zero inline). Deployed via the
  cross-volume staged `install --upgrade`; fleet respawned healthy (serena×3 +
  LSPs Running, 0 quarantined) + MCP routing live-verified.
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
1. **PR5 — auto-reaper hardening — DELIVERED (#520 → master c53d874a, deployed +
   live-verified 2026-07-08).** See the Delivered section above for the full scope +
   review record. **Two reaper-hardening bugs — BOTH NOW FIXED + deployed (were tracked
   as follow-ups; NOT regressions of #520 — pre-existing gaps #520 improved around):**
   - **FIXED (#521, `509afa31`, deployed + live-verified 2026-07-08):**
     `work-items/bugs/2026-07-08-cleanup-ancestor-walk-fails-open-on-uncertainty.md`
     (**P0** kill-authority): the 16-deep ancestor walk fails OPEN on classification
     uncertainty — byPID-miss can't tell a dead-parent orphan from a dropped-live-row
     (needs a `process.IsPidAlive` parent-probe with an injectable seam — a blunt spare
     would inert the reaper since byPID-miss is ALSO the normal real-orphan signature),
     and depth-16 exhaustion falls through to orphan (spare-on-exhaustion). One coherent
     3-state-verdict walk refactor; also folds the CLI aggressive `ExpectPIDs==nil` gap.
   - **FIXED (#522, `669951e3`, deployed + live-verified 2026-07-08):**
     `work-items/bugs/2026-07-08-aggressive-cleanup-token-omits-started-at.md` (low):
     AggressiveCleanup's confirm token hashes {pid, basename, match_source} not
     started_at + kill is PID-bound → shares the default reaper's (now-closed) PID-reuse
     class; converge onto `filterToExpectedIdentities` server-side.
   - Still deferred per H5: per-candidate SupervisorEventLog audit, supervisor-state PID
     exclusion, ticker apply:false policy, knownClientLauncher 8→45, project-scoped-config
     enum (R1), PEB step-5.
2. **Anti-drift "unmanaged detected" surface** (GUI) — **LANDED 2026-07-08 (PR #523,
   master `f7eaa1c8`, deployed).** Surfaces bypass servers so the operator can adopt
   them; reduces reaper need.
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
