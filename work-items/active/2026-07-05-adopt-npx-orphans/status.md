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

## Remaining (parked)
1. **PR5 — auto-reaper** (kill orphaned bypass npx-stdio daemons). **READY TO
   START** (deps verified: `process.PIDIdentityProof` + `TerminatePIDWithIdentity`
   ship; #511 already replaced the raw-taskkill with the identity-gated kill in
   `CleanupOrphans`). Remaining delta vs current `CleanupOrphans` (which has
   signature-match + age + identity-kill) = the H5-revised gate additions, per
   `work-items/decisions/2026-07-08-pipe-peer-unreliable-reaper-gate.md` §Decision.3:
   (2) parent verified dead via `GetExitCodeProcess == STILL_ACTIVE`-false (NOT
   bare OpenProcess); (4) candidate signature ABSENT from EVERY parseable on-disk
   client config (content-keyed; unparseable/unknown/unreadable → fail-closed = do
   NOT reap — this closes the documented T1 foreign-client hole where a post-adopt
   hub signature matches Antigravity/Cursor/Windsurf instances mcphub can't scan);
   (5 optional) PEB-isolated stdio NamedPipeState (amd64-only, hang-risk, fail-
   closed); (6) exclusions. Ship the MANUAL/operator-confirmed path first
   (unattended-timer is a SEPARATE gated decision). **$security-reviewer MANDATORY
   (highest-risk surface: kill authority over never-spawned PIDs; data-destruction
   hazard H1).** NOT rushed at 2026-07-08 session-end (a fatigued kill-authority
   push is poor engineering); warrants its own focused security-gated effort with
   the same multi-model rigor #519 needed.
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
