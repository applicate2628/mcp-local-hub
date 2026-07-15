# Delivery Plan — de-adopt v1 (all-clients-only, gate-OFF-only, atomic)

Source: `design.md` (round-5, ACCEPTED — arch delta-recheck PASS 2026-07-13). Decisions: `2026-07-11-deadopt-v1-all-clients-only-scope`, `2026-07-10-deadopt-manifest-delete-hash-gate` (both accepted). Planner: $planner (2026-07-14).

## Planning-accuracy corrections verified at HEAD (master c7e2534b)
1. **shared-owner #3 collapses to ONE line** — the `.snapshot` secret-bearing clause is ALREADY at HEAD (`state_read_inode_anchor.go:59-61`, added by #532). Only the read-cap clause in `state_read_caps.go` remains.
2. **Both Depends-on GC bugs satisfied at HEAD** — `reapAdoptProvenanceRow(name,state,updatedAt)` identity gate `adopted_entries.go:946-978`; Signal 2b `classifyDeadAdoptingRow:521-530`. Design citations of `:860-882`/"no filter" are stale-false → Phase 0 refresh.
3. **Adopt-reachable adapter set precise** (EntryBytesChecker compile-proof, `entry_bytes.go:31-39`): jsonMCPClient(+cursor/gemini-cli/qwen-cli/antigravity/relay), claudeCode, vscodeClient, openCodeClient, codexCLI, mimoCodeClient, forwarded by lockingClient. openHands is NOT in the set → out of scope.

## Sequencing / DAG
```
Phase 0 (precondition+docs) gates everything
 ├ Phase 1 (manifest delete hash)         ┐
 ├ Phase 2→3→4 (clients: extract→CAS→classify) ├ capability track (disjoint files → parallelizable)
 ├ Phase 5 (state_read cap)                ┘
 ├ Phase 6 (provenance mutators D1)        ─ independent
 └ Phase 7 (BuildDeAdoptPlan) ← 4,5
Phase 8 (ExecuteDeAdoptWithOpts, ATOMIC, integration) ← 1,3,4,5,6,7
 ├ Phase 9 (CLI) ← 7,8
 ├ Phase 10 (GUI backend) ← 7,8
 └ Phase 11 (frontend+e2e) ← 10
```
The ONE atomic all-clients-or-fail op (Phase 8) is a single UNSPLIT phase (CLOSE-READY gate + accept-conflict + roll-forward resume = one correctness unit).

## Change-Surface Contract (exactly 3 additive shared-owner changes; delta-review-confirmed)
1. `ManifestDeleteInWithHash` (manifest.go) — Phase 1.
2. CAS+read capability on adopt-reachable clients adapters (CASRestoreEntryFromBytes + CASGuardedRemoveEntry + EntryRawSubtree + read-only ClassifyEntryUnderLock + restoreEntryFromBackup→restoreEntryFromBytes extraction) — Phases 2-4.
3. `.snapshot` read-cap line in state_read_caps.go — Phase 5 (secret-bearing half already at HEAD).
Plus D1 provenance mutator BODIES in adopted_entries.go (Phase 6, design-authorized, tightly bounded). Everything else = NEW files (deadopt.go, deadopt_events.go, cli/deadopt.go, gui/deadopt.go, frontend, e2e). **BuildHubReconcilePlan/install_hub_reconcile.go NOT touched (gate-ON deferred; claim-11 negative check).**

---
## Phases (goal · files · deps · acceptance · claims · tests · gate · intensity)

**Phase 0 — Precondition + C6 stale-text/citation refresh (docs, NO code).** design.md+status.md. Deps: none. AC1: re-verify reapAdoptProvenanceRow identity gate + Signal 2b at HEAD (else BLOCK→$lead). AC2: refresh the "Adopt-GC dependency" section + `:655-656/:896-897/:942-943/:926-927` (assert deps satisfied, reap HAS identity gate, transition-reap CLOSED); re-point ALL adopted_entries.go citations vs HEAD (reap :946-978, classify :495, adoptSnapshotDir :254, removeAdoptSnapshots :308); mark `:1307-1308` withConfigLock→withConfigReadLock SUPERSEDED. AC3: record #3 collapsed (secret clause already at HEAD). Gate: $knowledge-archivist writes, $architecture-reviewer confirms citations. Intensity LOW.

**Phase 1 — ManifestDeleteInWithHash (#1).** manifest.go(+test). Deps: 0. AC: re-read on-disk manifest, hash-gate, refuse on mismatch; **fail-closed-on-empty-hash** (do NOT inherit ManifestEditInWithHash's `!=""` skip — wrong polarity for destructive delete); retain path-escape guard `:792-796`; shipped callers byte-unchanged. Claim 6. Test T5. Gate: $security-reviewer MANDATORY (destructive+polarity) + $qa. Intensity MEDIUM.

**Phase 2 — restoreEntryFromBackup→restoreEntryFromBytes extraction (#2 pt1, PURE refactor).** adopt-reachable adapters + config_lock.go. Deps: 0. AC: one extraction owner per adapter; byte-unchanged callers (install rollback/demigrate/serena-migrate green, no assertion changes); allowHubEntry polarity preserved. Do NOT refactor non-adopt adapters (openhands). Claim partial-14. Tests: existing restore/demigrate/rollback suites are the gate. Gate: $architecture-reviewer + $qa (full regression), $security light. Intensity MEDIUM.

**Phase 3 — CAS mutators CASRestoreEntryFromBytes + CASGuardedRemoveEntry (#2 pt2).** new clients capability file + adapter impls + lockingClient mutating forwarders. Deps: 2. AC: capability NOT Client-method (only adopt-reachable + forwarder); forwarder holds withConfigLock, concrete body lock-free (non-reentrant mutex); restore gate (live==nil→refuse-no-resurrection, !match→refuse, EntryPresentInBytes absent→refuse fail-closed B5, present→restore allowHubEntry=false); remove gate (nil→idempotent, !match→refuse); injected recognizer func(*MCPEntry)bool (dependency inversion, nil-guard). Claims 4,10,14. Tests T3,T4,T2-partial. Gate: **FULL COMMISSION** (security MANDATORY + arch + qa). Intensity HIGH.

**Phase 4 — ClassifyEntryUnderLock (read-only) + EntryRawSubtree (#2 pt3).** same capability file grow. Deps: 3. AC: WRITE-TARGET-PHYSICAL bytes (EntryPresentInBytes owner), NEVER merged GetEntry (P1-b, mimocode :3868-3951); withConfigReadLock forwarder (P3-a: same flock when dir exists, in-proc mutex when absent → no dir/lock side effect); verdicts StillHub/RestoreDone/GenuineConflict/Unreadable (cleanly-absent→empty-config not Unreadable, P2); no 2nd equality/extraction owner; NEVER mutates. Claims 18,19 (enables 2,8,15). Tests T15,T16,T17. Gate: **FULL COMMISSION** (subtlest seam, P1-a/P1-b). Intensity HIGH.

**Phase 5 — state_read_caps.go .snapshot read-cap (#3).** state_read_caps.go(+test). Deps: 0. AC: `HasSuffix(base,".snapshot")` → 16MiB bounded cap (reuse maxIntentFileBytes); verify secret-bearing clause already at HEAD (add NOTHING there); >16MiB residual assigned to adjacent bug (not fixed). Claim 17. Test: cap unit + T2 e2e. Gate: $security-reviewer (OOM bound) + $qa. Intensity LOW-MED.

**Phase 6 — Provenance mutators (D1).** adopted_entries.go (author MarkAdoptProvenanceDeAdopting + CloseAdoptProvenance BODIES + C6 comment repoint ONLY; MUST NOT touch capture/promote/abort/GC/classify/lease/snapshot/schema). Deps: 0. AC: Mark flips adopted→de_adopting, for committed-adopting RE-RUNS classify==CommittedKeep under lease before flip (B4, never wedge a reapable orphan), idempotent; Close DELETES row snapshots-first (no tombstone, crash→row-missing-snapshot never leak); UpdateAdoptExpectedManifestHash stays declared-unused (comment→subset follow-up); protected boundary held (git diff = only 2 bodies + C6 comments). Claims 5,16. Tests T6,T12-B4. Gate: **FULL COMMISSION** (protected provenance store). Intensity HIGH.

**Phase 7 — BuildDeAdoptPlan (read-only planner).** NEW deadopt.go. Deps: 4,5,6. AC: gate-ON refuse (P0); state routing (found=false/adopting-no-binding→refuse, adopted/adopting-with-binding→FRESH, de_adopting→RESUME); hash readiness; per-client done-ness via ClassifyEntryUnderLock (NO parallel unlocked read); path recomputed not trusted; eligibility surface G3. Claims 3,9,13,plan-side-8/18. Tests T11,T12-partial,T2-partial. Gate: $architecture-reviewer + $security-reviewer + $qa. Intensity MED-HIGH.

**Phase 8 — ExecuteDeAdoptWithOpts — ATOMIC all-clients (DO NOT SPLIT, integration).** deadopt.go + NEW deadopt_events.go. Deps: 1,3,4,5,6,7 + explicit integration owner. AC: E1 lease + E2 flip(B4); E3 per-client CAS restore/remove BEFORE topology removal ({Restored,Failed,Accepted}); **CLOSE-READY** = every target RESTORE-DONE-or-accepted, else STOP (manifest+intent+secrets+snapshots ALL intact, E4/5/6 skip, partial report); --accept-conflict honored ONLY on E3 re-read GenuineConflict (StillHub/unreadable→REJECT, P1-a); E4/5/6 gated on CLOSE-READY (P1-b: manifest delete + intent removal; routed-secret pre-filtered delete under vault lock; Close snapshots-first); roll-forward resume skip-if-done (crash→recoverable de_adopting a retry completes, never rollback-rewrite-hub); lock total order lease-outermost no-reverse-edge no-IPC/kill/wait-under-lock no-hub-mcp.lock; redaction P2-c (names/counts/hashes only); G4 report; claim-11 negative (git diff no install_hub_reconcile change). Claims 1,7,8,11,12,15 + completes 4,16,18. Tests T1,T7,T8,T9,T10,T13,T14. Gate: **FULL COMMISSION + integration owner + Codex-bot PASS + deep-security** ($security MANDATORY, $arch lock-graph/resume/cohesion, $qa +`-race`). Intensity HIGHEST.

**Phase 9 — CLI mcphub de-adopt (alias deadopt).** NEW cli/deadopt.go(+test). Deps: 7,8. AC: dry-run default/`--yes` execute; `--accept-conflict <client>` repeatable pass-through; exit semantics (0 all-restored/accepted, non-zero any Failed/no-provenance/gate-ON); no secret leak. Gate: $qa + $arch light + $security light. Intensity MEDIUM.

**Phase 10 — GUI backend routes + eligibility (G3).** NEW gui/deadopt.go(+test). Deps: 7,8 (∥ Phase 9). AC: POST /api/deadopt/plan + /api/deadopt + GET /api/deadopt/eligible, Same-Origin, audit row; eligible iff provenance-set AND !gate_on; both paths verified. Gate: $security-reviewer (Same-Origin/eligibility/audit) + $qa. Intensity MEDIUM.

**Phase 11 — Frontend affordance + Playwright.** frontend/ + e2e/ (+ `go generate ./internal/gui/...`). Deps: 10. AC: affordance from backend eligibility only (no shape heuristic), disabled gate-ON; Playwright round-trip adopt→gate-OFF de-adopt→scan native; bundle regenerated. Gate: $ux-reviewer + $frontend self-check + $qa (Playwright, Windows runner). Intensity MEDIUM.

## Mandatory-gate map
Full commission: **3, 4, 6, 8**. Security-reviewer MANDATORY: 1,3,4,5,6,7,8,10. Phase 8 additionally: Codex-bot PASS + deep-security (CLAUDE.md PR workflow).

## Tests T1-T17 → phase
T1→8 T2→5+7+3 T3→3 T4→3 T5→1 T6→6 T7→8 T8→8 T9→8 T10→8 T11→7 T12→6+7 T13→8 T14→8 T15→4 T16→4 T17→4. GUI/CLI eligibility+report+exit+Playwright→9/10/11.

## Residuals (bounded, deferred): G7 abandoned de_adopting recovery (subset+gate-ON follow-up), G8 concurrent-no-lease E3→E4 clobber (operator-driven), snapshot>16MiB (adjacent bug capture==restore), adjacent adopt-side bugs out of v1.

## Delivery approach: each phase is its own PR (small, independently-reviewable) → its gate → Codex-bot → merge. Full-commission phases (3,4,6,8) get the multi-model commission (Sol+Terra+fable) + security-reviewer before the bot. Phase 8 is D-scale (bot + deep-security).
