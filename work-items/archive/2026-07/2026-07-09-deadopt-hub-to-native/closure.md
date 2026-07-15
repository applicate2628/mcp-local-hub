# Closure — de-adopt v1 (hub → native, all-clients-only, gate-OFF-only, atomic)

Closed: 2026-07-15

## Outcome — DELIVERED end-to-end

All 12 planned phases (0-11) are merged to master (HEAD `837fe95c`). `mcphub de-adopt`
now safely reverses an `mcphub adopt`: it restores every adopt-owned client entry to its
exact pre-adopt config (including the original secret-literal spellings from the pinned
snapshots) and removes the hub-managed manifest, atomically and roll-forward-recoverable.

Delivery (each phase its own PR → gate → Codex-bot → merge):

| Phase | PR | master | What |
|---|---|---|---|
| 0+1 | #539 | `82e07b46` | docs precondition + `ManifestDeleteInWithHash` fail-closed hash gate |
| 2 | #540 | — | `restoreEntryFromBackup`→`restoreEntryFromBytes` extraction (pure refactor) |
| 3 | #542 | `f4623355` | CAS mutators `CASRestoreEntryFromBytes` + `CASGuardedRemoveEntry` (allowlist-gated, fail-closed) |
| 4 | #544 | `43675926` | `ClassifyEntryUnderLock` (read-only) + `EntryRawSubtree` — the atomic classify seam |
| 5 | #543 | `9c03b3ff` | `.snapshot` read-cap (16 MiB) |
| 6 | #545 | `a2a109b0` | provenance mutators `MarkAdoptProvenanceDeAdopting` + `CloseAdoptProvenance` (+ P2 lease-fix `27a622ac`) |
| 7 | #546 | `1d9c37c2` | `BuildDeAdoptPlan` read-only planner |
| 8 | #547 | `0e5614df` | `ExecuteDeAdoptWithOpts` ATOMIC executor + `deadopt_events.go` |
| 9 | #548 | `5d362a76` | CLI `mcphub de-adopt` (alias `deadopt`) |
| 10 | #549 | `7bb45df6` | GUI backend routes + eligibility (G3) |
| 11 | #550 | `837fe95c` | frontend affordance + Playwright + `GET /api/deadopt/recoverable` |

v1 scope held: **all-clients-only, gate-OFF-only, atomic**. `BuildHubReconcilePlan` /
`install_hub_reconcile.go` NOT touched (claim-11 negative). Subset de-adopt, gate-ON
de-adopt, and `--reconstruct-legacy` were DEFERRED (backlog
`2026-07-11-deadopt-subset-and-gate-on-followup.md`).

## Architecture spine

- **Atomicity model:** per-manifest flock lease (E1, outermost, held E1→E6) → transient
  non-nested inners (adopted-entries.lock, per-file config-lock via the CAS mutators, the
  read-selection config-read-lock via `ClassifyEntryUnderLock`, vault lock). No IPC / kill /
  wait under the lease; events + caller narration buffered and flushed AFTER the lease
  release (LIFO defer).
- **CLOSE-READY** is the single terminal-state predicate: E4 (manifest+intent delete),
  E5 (routed-secret delete), E6 (`CloseAdoptProvenance`, snapshots-first) run ONLY when every
  target client is RESTORE-DONE or genuinely-accepted; a Failed client blocks the whole close
  with manifest+intent+secrets+snapshots intact (partial report, row stays `de_adopting`).
- **Roll-forward resume:** every step skip-if-done; a crash leaves a recoverable `de_adopting`
  row a re-run completes — never a rollback that re-writes hub over restored native.
- **Under-lease authority:** the executor re-reads the provenance row (identity-verified via
  immutable `CreatedAt`), re-probes the hub gate, derives routing from the leased row, and
  re-classifies every client under the lease — the pre-lease plan is advisory only.
- **`--accept-conflict`** is honored ONLY on a mutation-point `ClassifyGenuineConflict`
  (StillHub / unreadable / crash-done → rejected/short-circuited); the GUI checkbox states the
  irreversible-destruction consequence.

## Quality gates run

FULL COMMISSION (Sol + Terra + fable) on Phases 3, 4, 6, 8; security-reviewer on
1,3,4,5,6,7,8,10; Phase 8 additionally bot + fable final-gate. Codex-bot PASS on every PR.
Phase 8 (the atomic executor) alone absorbed **16 fixes** across the Sol+Terra+fable
commission + 6 bot rounds + a fable final-gate that caught a fail-open the bot missed
(shared-secret scan swallowing a manifest-dir listing error). Phase 7 took 5 bot rounds
(incl. the mimocode-layered GetEntry authority); Phase 11 took Sol UX + 5 bot rounds of
frontend edge cases (recovery-row visibility, migrate fail-closed, provenance-only recovery
surface).

## Residual risk / known limitations

- **Deferred (v2, backlog):** subset de-adopt, gate-ON de-adopt, `--reconstruct-legacy`.
- **Pre-existing test fragility (not de-adopt):** `adopt_test.go:218`
  `TestExecuteAdoptScopedConsentDoesNotAuthorizeConcurrentNonAdoptWrite` binds a hardcoded
  port 9318 which `AdGuardVpnSvc` squats on this host → env-specific failure. Separate
  test-hygiene follow-up.
- **Operator-facing residual:** a fully-invisible provenance-only `de_adopting` row is now
  surfaced in the GUI via `/api/deadopt/recoverable`; the CLI (`mcphub de-adopt <name>`) was
  always able to resume it.

## Retrospective

- **What went well:** the per-phase PR + gate + bot loop kept each change small and
  independently reviewable; the FULL-COMMISSION + fable final-gate on the atomic executor was
  worth it (fable caught a real data-loss fail-open the bot missed). Diverse review lenses
  (Sol arch, Terra concurrency, fable adversarial) each caught defects the others didn't
  (Terra+fable found the pre-lease-plan-trust TOCTOU Sol missed; Sol found the lease-hold
  blocking-I/O the others missed).
- **What didn't:** (1) a via-hub-only eligibility limit (a bot-round-1 perf fix) regressed
  recoverable-row visibility — the right fix was non-blocking async, not row-limiting; the
  lesson: for a destructive recovery-capable surface, fail-VISIBLE + fail-CLOSED beats
  fail-quiet. (2) Uncommitted status.md/index.md archival edits were wiped by a
  `git reset --hard` during a master sync — commit doc edits before any reset.
- **Host note:** the session hit persistent Windows ephemeral-port exhaustion (range
  1024-15000, ~24000 half-closed sockets, ~12000 leaked by `AdGuardVpnSvc` PID 7140) that
  intermittently blocked github ops near the end. Not a code issue.

## Archive location

`work-items/archive/2026-07/2026-07-09-deadopt-hub-to-native/`
