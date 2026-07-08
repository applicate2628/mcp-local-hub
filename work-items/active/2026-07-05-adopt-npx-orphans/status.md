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
1. **PR5 — auto-reaper** (kill orphaned bypass npx-stdio daemons): gate on the
   revised config-presence predicate (NOT pipe-peer); dry-run-first +
   operator-confirm. Depends-on the A1 F1/F3 kill-authority primitives.
2. **Anti-drift "unmanaged detected" surface** (GUI) — surface bypass servers
   so the operator can adopt them; reduces reaper need.
3. **Phase-2 de-adopt / revert-to-native** (hub→native): interim = `mcphub
   uninstall --server X` + restore client backup.
4. **fable Area-3 follow-up** (#516 review): an explicitly-requested client
   whose config EXTRACTION errors survives the mismatch/disabled filter —
   fail-closed in effect (AddEntry → rollback), harden to an explicit exclusion.

## Artifacts
- design.md — architect package (this dir).
- Source bug: work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md.
- Decision: work-items/decisions/2026-07-08-pipe-peer-unreliable-reaper-gate.md.
- Adjacent finding (standalone-shippable subset of P2): work-items/bugs/2026-07-05-cleanuporphans-raw-taskkill-no-identity-reverify.md.

## Depends-on
2026-07-05-a1-lost-child-fpackage (F1/F3 kill-authority precedent + primitives; A2-PR5 sequenced after).
